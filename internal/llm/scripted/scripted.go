// Package scripted is an llm.Adapter that plays a script, for tests: each
// call takes the next step, and every request is kept to be looked at
// afterwards. Nothing leaves the process.
//
//	model := scripted.New(
//		scripted.CallTool("assignment_list", `{}`),
//		scripted.Then(func(*llm.Request) { core.Post(followUp) }), // while the model thinks
//		scripted.Reply("HW3 is due Friday."),
//	)
//	… run the worker with model …
//	if err := model.Err(); err != nil { t.Fatal(err) }
//	reqs := model.Requests()
package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// Step answers one call, but for Then, which hands its call on to the step
// after it. A step may inspect req, block until ctx is done, or fail; its
// response is given ids, a maker and usage where it has none, and is
// normalised (llm.Response.Normalize).
type Step func(ctx context.Context, req *llm.Request) (*llm.Response, error)

// ErrOutOfSteps is what a call past the end of the script fails with: the
// code under test called the model more often than the test expected.
var ErrOutOfSteps = errors.New("scripted: the script has no step left for this call")

// Defaults of an Adapter, before its With… settings.
const (
	DefaultName     = "scripted"
	DefaultProvider = "scripted"
	DefaultModel    = "scripted-model"
)

// Usage a step's response is given when it reports none: small numbers, so
// that the ledger and the budgets see every call.
const (
	DefaultInputTokens  = 100
	DefaultOutputTokens = 20
)

// Adapter plays its steps in order. It is safe for concurrent use:
// concurrent calls take successive steps.
type Adapter struct {
	name, provider, model, maker string
	dialect                      toolschema.Dialect
	caps                         llm.Capabilities
	files                        llm.FileLimits

	mu       sync.Mutex
	steps    []Step
	next     int
	requests []*llm.Request
	err      error
}

var (
	_ llm.Adapter     = (*Adapter)(nil)
	_ llm.Streamer    = (*Adapter)(nil)
	_ llm.FileLimiter = (*Adapter)(nil)
)

// New plays steps. Its capabilities are those of openai_chat against OpenAI
// (parallel calls, tool_choice none, files) and its dialect OpenAI's, until
// set otherwise.
func New(steps ...Step) *Adapter {
	return &Adapter{
		name: DefaultName, provider: DefaultProvider, model: DefaultModel,
		dialect: toolschema.OpenAI,
		caps:    llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
		steps:   append([]Step(nil), steps...),
	}
}

// WithName sets Name. The With… settings are for building the adapter,
// before its first call.
func (a *Adapter) WithName(name string) *Adapter { a.name = name; return a }

// WithProvider sets Provider.
func (a *Adapter) WithProvider(provider string) *Adapter { a.provider = provider; return a }

// WithModel sets Model.
func (a *Adapter) WithModel(model string) *Adapter { a.model = model; return a }

// WithMaker sets Maker; by default it is llm.MakerOf(name, "", model).
func (a *Adapter) WithMaker(maker string) *Adapter { a.maker = maker; return a }

// WithDialect sets Dialect.
func (a *Adapter) WithDialect(d toolschema.Dialect) *Adapter { a.dialect = d; return a }

// WithCapabilities sets Capabilities.
func (a *Adapter) WithCapabilities(c llm.Capabilities) *Adapter { a.caps = c; return a }

// WithFileLimits sets FileLimits: by default none.
func (a *Adapter) WithFileLimits(l llm.FileLimits) *Adapter { a.files = l; return a }

// FileLimits are the largest PDF the adapter's model takes as a file.
func (a *Adapter) FileLimits() llm.FileLimits { return a.files }

// Name is the adapter's name, scripted by default.
func (a *Adapter) Name() string { return a.name }

// Provider is the provider's name, scripted by default.
func (a *Adapter) Provider() string { return a.provider }

// Model is the model's name, scripted-model by default.
func (a *Adapter) Model() string { return a.model }

// Maker is what the adapter's reasoning parts are stamped with.
func (a *Adapter) Maker() string {
	if a.maker != "" {
		return a.maker
	}
	return llm.MakerOf(a.name, "", a.model)
}

// Dialect is the schema dialect the tools are expected in.
func (a *Adapter) Dialect() toolschema.Dialect { return a.dialect }

// Capabilities are what the code under test may rely on.
func (a *Adapter) Capabilities() llm.Capabilities { return a.caps }

// Append adds steps to the end of the script.
func (a *Adapter) Append(steps ...Step) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steps = append(a.steps, steps...)
}

// Remaining is the number of steps not yet taken.
func (a *Adapter) Remaining() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.steps) - a.next
}

// Requests are copies of every request seen, in the order the calls came.
func (a *Adapter) Requests() []*llm.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*llm.Request, len(a.requests))
	for i, r := range a.requests {
		out[i] = cloneRequest(r)
	}
	return out
}

// Err is the script's first failure, or nil: a call made past its end
// (ErrOutOfSteps), or a step that answered nothing. The call failed too,
// but the code under test may have handled that quietly; a test checks Err
// to be sure it did not happen.
func (a *Adapter) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// Call takes the next step. A call with no step left fails with an
// *llm.Error that no loop retries, and is kept for Err.
func (a *Adapter) Call(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if req == nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "scripted: no request"}
	}
	a.mu.Lock()
	a.requests = append(a.requests, cloneRequest(req))
	n := len(a.requests)
	a.mu.Unlock()

	var resp *llm.Response
	for {
		step, ok := a.take()
		if !ok {
			return nil, a.fail(fmt.Errorf("%w (call %d)", ErrOutOfSteps, n))
		}
		var err error
		resp, err = step(ctx, req)
		if errors.Is(err, errNext) {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if resp == nil {
		return nil, a.fail(fmt.Errorf("scripted: step %d returned neither a response nor an error", n))
	}
	k := 0
	for i := range resp.Parts {
		p := &resp.Parts[i]
		if p.Type == llm.PartToolCall {
			k++
			if p.ID == "" {
				// Ids unique across the whole script, as a provider's
				// are, so that no two calls in one history share one.
				p.ID = fmt.Sprintf("call_%d_%d", n, k)
			}
		}
		if p.Type == llm.PartReasoning && p.Maker == "" {
			p.Maker = a.Maker()
		}
	}
	if resp.Usage.Input == 0 && resp.Usage.Output == 0 {
		resp.Usage.Input, resp.Usage.Output = DefaultInputTokens, DefaultOutputTokens
		resp.Usage.Raw = json.RawMessage(fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d}`, DefaultInputTokens, DefaultOutputTokens))
	}
	if resp.Model == "" {
		resp.Model = a.model
	}
	if resp.RequestID == "" {
		resp.RequestID = fmt.Sprintf("scripted-%d", n)
	}
	resp.Normalize()
	return resp, nil
}

// textKey carries a streamed call's llm.TextFunc to the step that answers
// it.
type textKey struct{}

// Stream is Call, streamed: the step answering it is given onText
// (TextOf), which a streaming step (Streamed) tells its text in pieces,
// and any other step tells nothing, as an adapter that does not stream.
func (a *Adapter) Stream(ctx context.Context, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	return a.Call(context.WithValue(ctx, textKey{}, onText), req)
}

// TextOf is what the call a step answers tells its text to: the caller's
// llm.TextFunc for a streamed call, and one that keeps nothing otherwise.
func TextOf(ctx context.Context) llm.TextFunc {
	if f, ok := ctx.Value(textKey{}).(llm.TextFunc); ok && f != nil {
		return f
	}
	return func(string) {}
}

// Streamed answers with the pieces' text, told one piece at a time, every
// apart, as a provider streams it, and stops at the end. A call that is
// not streamed gets the same answer whole.
func Streamed(every time.Duration, pieces ...string) Step {
	return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		if err := tell(ctx, every, pieces); err != nil {
			return nil, err
		}
		return Reply(strings.Join(pieces, ""))(ctx, req)
	}
}

// StreamedThenFail tells the pieces as Streamed does, then fails with err:
// a stream cut off part way, whose text is no answer.
func StreamedThenFail(every time.Duration, err error, pieces ...string) Step {
	return func(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
		if terr := tell(ctx, every, pieces); terr != nil {
			return nil, terr
		}
		return nil, err
	}
}

// tell tells the pieces to the call's TextFunc, every apart; a timeout if
// the call's context ends first.
func tell(ctx context.Context, every time.Duration, pieces []string) error {
	onText := TextOf(ctx)
	for i, p := range pieces {
		if i > 0 && every > 0 {
			t := time.NewTimer(every)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return &llm.Error{Kind: llm.ErrTimeout, Message: "scripted: the stream ran out of time"}
			}
		}
		onText(p)
	}
	return nil
}

// take is the next step, if there is one.
func (a *Adapter) take() (Step, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.next >= len(a.steps) {
		return nil, false
	}
	a.next++
	return a.steps[a.next-1], true
}

// errNext is how a Then step hands its call on to the step after it.
var errNext = errors.New("scripted: the next step answers this call")

// fail keeps err for Err, the first one only, and returns it as an
// *llm.Error that no loop retries: a script gone wrong is the test's bug,
// and retrying it would only slow the test down.
func (a *Adapter) fail(err error) error {
	a.mu.Lock()
	if a.err == nil {
		a.err = err
	}
	a.mu.Unlock()
	return &llm.Error{Kind: llm.ErrBadRequest, Code: "scripted", Message: err.Error()}
}

// Then is a side effect while the model is generating, for the step after
// it to answer: a follow-up written, a level changed, a seat removed. It
// takes no call of its own:
//
//	New(CallTool("grade_list", `{}`), Then(writeFollowUp), Reply("…"))
//
// answers two calls, and writeFollowUp runs during the second. f sees the
// request as the model did. A Then and the step after it are taken one
// after the other, not at once, so a call made concurrently may take the
// step between them; a script with Then is meant for calls made one at a
// time, as one answer's loop makes them. Step.Then is the form that cannot
// be split.
func Then(f func(req *llm.Request)) Step {
	return func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		if f != nil {
			f(req)
		}
		return nil, errNext
	}
}

// Then is s followed by f, a side effect run once s has answered and
// before the call returns: Reply("…").Then(writeFollowUp) is the same call
// as Then(writeFollowUp) followed by Reply("…"), with f run last.
func (s Step) Then(f func(req *llm.Request)) Step {
	return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		resp, err := s(ctx, req)
		if f != nil {
			f(req)
		}
		return resp, err
	}
}

// Reply answers with text and stops at the end.
func Reply(text string) Step {
	return Stop(llm.StopEnd, text)
}

// Stop answers with text, which may be empty, and stops for stop: a
// max_tokens with partial text, a content_filter, a refusal, …
func Stop(stop llm.Stop, text string) Step {
	return func(context.Context, *llm.Request) (*llm.Response, error) {
		resp := &llm.Response{Stop: stop, RawStop: string(stop)}
		if text != "" {
			resp.Parts = []llm.Part{llm.Text(text)}
		}
		return resp, nil
	}
}

// ToolCall is one call CallTools makes. Args is the JSON the model writes;
// text that is not a JSON object is kept as the call's ArgsError, as an
// adapter keeps a model's malformed arguments. An empty ID is made unique.
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// CallTool calls one tool with args.
func CallTool(name, args string) Step {
	return CallTools(ToolCall{Name: name, Args: args})
}

// CallTools calls several tools at once, in order.
func CallTools(calls ...ToolCall) Step {
	return func(context.Context, *llm.Request) (*llm.Response, error) {
		resp := &llm.Response{Stop: llm.StopToolCalls, RawStop: "tool_calls"}
		for _, c := range calls {
			p := llm.Part{Type: llm.PartToolCall, ID: c.ID, Name: c.Name}
			p.Args, p.ArgsError = args(c.Args)
			resp.Parts = append(resp.Parts, p)
		}
		return resp, nil
	}
}

func args(s string) (json.RawMessage, string) {
	t := strings.TrimSpace(s)
	if t == "" {
		return json.RawMessage("{}"), ""
	}
	if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return json.RawMessage(t), ""
	}
	return json.RawMessage("{}"), s
}

// Fail fails the call with err: an *llm.Error to act as a provider's
// refusal (rate limited, overloaded, context overflow, …).
func Fail(err error) Step {
	return func(context.Context, *llm.Request) (*llm.Response, error) {
		return nil, err
	}
}

// Respond answers with a copy of resp, for anything the other steps do not
// make: reasoning parts, a given usage, several parts.
func Respond(resp llm.Response) Step {
	return func(context.Context, *llm.Request) (*llm.Response, error) {
		out := resp
		out.Parts = cloneParts(resp.Parts)
		out.Usage.Raw = cloneBytes(resp.Usage.Raw)
		return &out, nil
	}
}

// WithUsage is s with its response's usage set to u.
func WithUsage(s Step, u llm.Usage) Step {
	return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		resp, err := s(ctx, req)
		if resp != nil {
			resp.Usage = u
			resp.Usage.Raw = cloneBytes(u.Raw)
		}
		return resp, err
	}
}

// Hang waits for the call's context to end and fails as a timeout: a
// provider that never answers, for wall-clock budgets.
func Hang() Step {
	return func(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
		<-ctx.Done()
		return nil, &llm.Error{Kind: llm.ErrTimeout, Message: "scripted: the provider did not answer in time"}
	}
}

// cloneRequest copies r deeply, so that nothing the caller does to r later
// changes what was recorded.
func cloneRequest(r *llm.Request) *llm.Request {
	out := *r
	if r.Messages != nil {
		out.Messages = make([]llm.Message, len(r.Messages))
		for i, m := range r.Messages {
			out.Messages[i] = llm.Message{Role: m.Role, Parts: cloneParts(m.Parts)}
		}
	}
	if r.Tools != nil {
		out.Tools = make([]llm.Tool, len(r.Tools))
		for i, t := range r.Tools {
			t.Schema = cloneBytes(t.Schema)
			out.Tools[i] = t
		}
	}
	return &out
}

func cloneParts(parts []llm.Part) []llm.Part {
	if parts == nil {
		return nil
	}
	out := make([]llm.Part, len(parts))
	for i, p := range parts {
		p.Args = cloneBytes(p.Args)
		p.Opaque = cloneBytes(p.Opaque)
		if p.File != nil {
			f := *p.File
			f.Data = cloneBytes(f.Data)
			p.File = &f
		}
		out[i] = p
	}
	return out
}

func cloneBytes[T ~[]byte](b T) T {
	if b == nil {
		return nil
	}
	return append(T(nil), b...)
}
