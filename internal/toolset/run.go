package toolset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// Runner is what one answer's tool calls are run with.
type Runner struct {
	// Client is the agent's connection to Core, with its retries and rate
	// limit underneath.
	Client *core.Client
	// Files fetches a document's file; nil gives no file to the model.
	Files FileFetcher
	// MaxParallel is how many calls run at once (max_parallel_tools);
	// DefaultMaxParallel when 0 or less.
	MaxParallel int
	// MaxResultBytes bounds one result's content (§3.1 rule 5);
	// DefaultMaxResultBytes when 0 or less, and never under 1 KiB.
	MaxResultBytes int
	// FileInput: the model takes file parts (llm.Capabilities.FileInput).
	FileInput bool
	// PDFLimits are the largest PDF the model's provider takes as a file
	// (llm.FileLimiter): a PDF past them is given as its text.
	PDFLimits llm.FileLimits
	// MaxFileBytes bounds a file fetched for the model (rule 6);
	// DefaultMaxFileBytes when 0 or less.
	MaxFileBytes int64
	// DocLimits bound the reading of a file's text (doctext); its zero
	// fields are doctext's defaults.
	DocLimits doctext.Limits
	// Texts keeps what was read of files for the models given their text,
	// so that a file read in parts is fetched and read once; nil keeps
	// nothing.
	Texts *TextCache
	// OCR recognizes the text of a scanned PDF or an image for a model
	// that cannot take the file; nil recognizes nothing, and the model is
	// told there is no OCR here.
	OCR OCR
	// Writes is the answer's account of its writes: their keys and their
	// budget. Nil refuses every write, whatever the set offers.
	Writes *Writes
	// Guard is the seats the model's member writes never change; its zero
	// value refuses every member write.
	Guard SeatGuard
}

// Defaults of Runner.
const (
	DefaultMaxParallel    = 4
	DefaultMaxResultBytes = 32 << 10
	DefaultMaxFileBytes   = 10 << 20
	// minResultBytes leaves room for the status, the error and the
	// truncation mark whatever the configuration says.
	minResultBytes = 1 << 10
)

func (r Runner) withDefaults() Runner {
	if r.MaxParallel <= 0 {
		r.MaxParallel = DefaultMaxParallel
	}
	if r.MaxResultBytes <= 0 {
		r.MaxResultBytes = DefaultMaxResultBytes
	}
	r.MaxResultBytes = max(r.MaxResultBytes, minResultBytes)
	if r.MaxFileBytes <= 0 {
		r.MaxFileBytes = DefaultMaxFileBytes
	}
	return r
}

// codeUnavailable is the code of a result the runtime gives when Core could
// not be reached; the other refusals the runtime makes itself use Core's
// codes, so that the model reads every result the same way.
const codeUnavailable = "unavailable"

// Writes is one answer's account of the writes its model makes (design
// §4). The loop keeps one for the whole answer, across its turns, and Run
// uses it between them, never two Runs at once.
//
// Run numbers the writes it sends to Core in the order the model made
// them, from 1, before any is sent, and binds each to the idempotency key
// Key gives its number: the same answer tried again (the same attempt,
// after its model or the worker failed) numbers its writes the same, and
// Core replays what it did the first time instead of doing it twice; a new
// attempt's keys are new. A write the answer has already sent, the same
// tool with the same arguments, goes under the key it went under then and
// takes no number, so that a model that makes it again, after a proposal or
// an answer Core did not give, meets Core's replay. At most Max writes are
// sent: past it, a write is an is_error result that reaches nobody.
type Writes struct {
	// Max is how many writes the answer may send (per_answer.max_writes).
	Max int
	// Key is the idempotency key of the answer's nth write, from 1.
	Key func(n int) string

	sent int
	keys map[string]sent
	// Records are the writes sent to Core, in the order the model made
	// them, and what came of each.
	Records []WriteRecord
	// Refused are the tools of the writes refused because the budget was
	// spent, in the order the model made them.
	Refused []string
	// Guarded are the tools of the writes refused before Core for what
	// they would change or how they would be decided: member writes
	// SeatGuard refused, and decisions the seat would make alone
	// (Decides); in the order the model made them.
	Guarded []string
}

// sent is a write already sent, by its tool and arguments.
type sent struct {
	n   int
	key string
}

// WriteRecord is one write sent to Core and what came of it, in ids and
// codes: never its arguments.
type WriteRecord struct {
	// N is the write's number, and Key its idempotency key: a write sent
	// again has the number and key it had the first time.
	N    int
	Key  string
	Tool string
	// Status is Core's envelope status (executed, proposed, denied,
	// failed, error), or StatusUnreachable when Core did not answer.
	Status   string
	Code     string
	Reason   string
	ActionID string
	Replayed bool
	// IDs are the result's own ids, its top-level members named *_id.
	IDs map[string]string
}

// StatusUnreachable is a WriteRecord's status when Core did not answer:
// the write may or may not have been done.
const StatusUnreachable = "unreachable"

// Sent is how many writes the answer has numbered and sent.
func (w *Writes) Sent() int {
	if w == nil {
		return 0
	}
	return w.sent
}

// claim numbers a write of tool with args, or finds the number and key it
// had when the answer sent it before; ok is false when the budget is spent.
func (w *Writes) claim(tool string, args json.RawMessage) (sent, bool) {
	id := tool + "\x00" + string(args)
	if prev, ok := w.keys[id]; ok {
		return prev, true
	}
	if w.sent >= w.Max {
		w.Refused = append(w.Refused, tool)
		return sent{}, false
	}
	w.sent++
	if w.keys == nil {
		w.keys = map[string]sent{}
	}
	sn := sent{n: w.sent, key: w.Key(w.sent)}
	w.keys[id] = sn
	return sn, true
}

// Run runs the model's tool calls (the tool_call parts of calls; any other
// part is passed over) and returns one tool_result part per call, in call
// order, with Name and CallID set, followed by the file parts of any files
// given to the model (rule 6). At most MaxParallel calls run at once, each
// with ctx marked core.PriorityAnswer.
//
// A call is checked before it reaches Core, and any failure is an is_error
// result the model can correct itself from, with no call to Core: a tool
// not offered here ("no such tool here", naming those that are); a tool on
// the built-in deny list, or a write without r.Writes, checked again
// whatever built the set; arguments that are not a JSON object (rule 1);
// arguments Core's schema refuses once course_id is set to courseID, the
// conversation's course, whatever the model wrote (toolschema.Reverse,
// Validate); a member write that would change a seat r.Guard keeps, which
// for a change to every seat of a role the runtime reads with member_get
// (SeatGuard); a decision or review from a seat whose action_decide is not
// confirm_required (Decides). A write is then numbered and bound to its
// idempotency key (Writes), whatever key the model wrote; one past the
// answer's budget, or one that repeats another call of the same turn
// exactly, is refused.
// Core's answer is Core's envelope as JSON, is_error unless executed or
// proposed (a proposal is Core's normal answer at confirm_required, not a
// failure), with every download_url taken out and cut to MaxResultBytes
// keeping status and error whole. Every write sent is recorded in
// r.Writes.Records, in call order.
//
// A call Core did not answer is one of two things. Fatal: a 401
// (core.ErrUnauthenticated: the agent must stop) or ctx done (the answer's
// wall clock is spent). Run then stops the other calls and returns the
// error and no parts: the answer cannot go on. Anything else (a 5xx or a
// 429 the retrying caller underneath gave up on, a protocol error) is an
// is_error result saying Core could not be reached, and the model answers
// without it; for a write, that it may or may not have been done, and that
// the same call again is never done twice.
func (s *Set) Run(ctx context.Context, r Runner, courseID string, calls []llm.Part) ([]llm.Part, error) {
	r = r.withDefaults()
	var toolCalls []llm.Part
	for _, c := range calls {
		if c.Type == llm.PartToolCall {
			toolCalls = append(toolCalls, c)
		}
	}
	if len(toolCalls) == 0 {
		return nil, nil
	}
	if r.Client == nil {
		return nil, errors.New("toolset: the runner has no Core client")
	}
	// Every call is checked, and every write numbered, in call order and
	// before any is sent: a write's number must not hang on which call
	// Core answers first.
	preps := make([]prepared, len(toolCalls))
	thisTurn := map[string]string{}
	for i, call := range toolCalls {
		p := s.prepare(r, courseID, call)
		if p.done || !p.write {
			preps[i] = p
			continue
		}
		if Decides(call.Name) && s.decide != core.LevelConfirmRequired {
			r.Writes.Guarded = append(r.Writes.Guarded, call.Name)
			preps[i] = refusedCall(p.res, core.CodeForbidden, fmt.Sprintf(
				"the runtime did not send this: a model's decision or review must wait for a person to confirm it, and this seat's action_decide is %s, "+
					"which Core should never give an agent; tell the person to decide it themselves", cut(levelOf(s.decide), maxNameInMessage)))
			continue
		}
		if guarded(call.Name) {
			why, err := r.Guard.check(ctx, r.Client, courseID, call.Name, p.args)
			if err != nil {
				return nil, err
			}
			if why != "" {
				r.Writes.Guarded = append(r.Writes.Guarded, call.Name)
				preps[i] = refusedCall(p.res, core.CodeForbidden, why)
				continue
			}
		}
		id := call.Name + "\x00" + string(p.args)
		if first, ok := thisTurn[id]; ok {
			preps[i] = refusedCall(p.res, core.CodeInvalidArgument, fmt.Sprintf(
				"this call repeats call %s of this turn exactly: it is made once, and its result is that call's", cut(first, maxNameInMessage)))
			continue
		}
		thisTurn[id] = call.ID
		sn, ok := r.Writes.claim(call.Name, p.args)
		if !ok {
			preps[i] = refusedCall(p.res, core.CodeFailedPrecondition, fmt.Sprintf(
				"the changes this answer may make are spent (%d): %s was not made; tell the person what is left undone", r.Writes.Max, call.Name))
			continue
		}
		args, err := bindKey(p.args, sn.key)
		if err != nil {
			preps[i] = refusedCall(p.res, core.CodeInvalidArgument, argumentMessage(call.Name, err))
			continue
		}
		p.args, p.n, p.key = args, sn.n, sn.key
		preps[i] = p
	}

	parent := ctx
	ctx, cancel := context.WithCancel(core.WithPriority(parent, core.PriorityAnswer))
	defer cancel()

	type outcome struct {
		part llm.Part
		file *llm.File
		env  *core.Envelope
		err  error
	}
	outs := make([]outcome, len(preps))
	sem := make(chan struct{}, r.MaxParallel)
	var wg sync.WaitGroup
	for i, p := range preps {
		if p.done {
			outs[i] = outcome{part: p.res}
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outs[i].err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			part, file, env, err := s.send(ctx, r, p)
			if err != nil {
				cancel()
			}
			outs[i] = outcome{part, file, env, err}
		})
	}
	wg.Wait()

	// The first error that is not the context's (a 401) wins over one that
	// is, which may be only the cancellation it caused.
	var ctxErr error
	for _, o := range outs {
		switch {
		case o.err == nil:
		case !isContextError(o.err):
			return nil, o.err
		case ctxErr == nil:
			ctxErr = o.err
		}
	}
	if ctxErr != nil {
		if err := parent.Err(); err != nil {
			return nil, err
		}
		return nil, ctxErr
	}
	parts := make([]llm.Part, 0, len(outs))
	for i, o := range outs {
		parts = append(parts, o.part)
		if p := preps[i]; !p.done && p.write {
			r.Writes.Records = append(r.Writes.Records, record(p, o.env))
		}
	}
	for _, o := range outs {
		if o.file != nil {
			parts = append(parts, llm.Part{Type: llm.PartFile, File: o.file})
		}
	}
	return parts, nil
}

// Decides reports whether a write of tool decides or reviews a proposal:
// Run sends one only from a seat whose action_decide is confirm_required,
// where Core makes the decision itself a proposal that a person confirms.
// At pending_review a decision would take effect before anyone looked, and
// at autonomous without anyone looking; Core holds an agent's to
// confirm_required, and the runtime holds it there again.
func Decides(tool string) bool { return tool == "action_decide" || tool == "action_review" }

// levelOf is a seat's level as a refusal names it.
func levelOf(level string) string {
	if level == "" {
		return "not set"
	}
	return level
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// prepared is one call checked and ready to send, or done: refused before
// Core, with its result.
type prepared struct {
	res  llm.Part
	done bool
	t    *offered
	args json.RawMessage
	// write, n and key: a write, its number and its idempotency key.
	write bool
	n     int
	key   string
	// part is the part of a document's file text the model asked for
	// (FilePartArg), 0 when it asked for none.
	part int
}

func refusedCall(res llm.Part, code, msg string) prepared {
	return prepared{res: refuse(res, code, msg), done: true}
}

// prepare checks one call before it may reach Core (Run).
func (s *Set) prepare(r Runner, courseID string, call llm.Part) prepared {
	res := llm.Part{Type: llm.PartToolResult, CallID: call.ID, Name: call.Name}
	t, ok := s.lookup(call.Name)
	if !ok {
		return refusedCall(res, core.CodeNotFound, s.noSuchTool(call.Name))
	}
	// The deny list holds at every stage (§6.1), whatever built this set,
	// and before anything of the call is looked at; so does a write's
	// need of the answer's keys and budget.
	write := t.kind == KindWrite
	if BuiltinDenied(call.Name) || (t.kind != KindRead && !write) || (write && (r.Writes == nil || r.Writes.Key == nil)) {
		return refusedCall(res, core.CodeForbidden, call.Name+" is not offered to the model")
	}
	if call.ArgsError != "" || !isObject(call.Args) {
		return refusedCall(res, core.CodeInvalidArgument, fmt.Sprintf(
			"the arguments to %s are not a JSON object; call it again with its parameters as one JSON object", call.Name))
	}
	// The runtime's own argument is taken out before Core's schema sees
	// the call: Core takes no other.
	callArgs, part := call.Args, 0
	if call.Name == FilePartTool {
		var err error
		if part, callArgs, err = takeFilePart(call.Args); err != nil {
			return refusedCall(res, core.CodeInvalidArgument, fmt.Sprintf(
				"%s: %s is the part of the file's text to read, a whole number from 1 (file.parts says how many there are); call it again", call.Name, FilePartArg))
		}
	}
	bound := map[string]any{"course_id": courseID}
	if write {
		// The model's key, if it wrote one, goes: bindKey gives the
		// runtime's once the write is numbered.
		bound["idempotency_key"] = ""
	}
	args, err := toolschema.Reverse(t.input, callArgs, bound)
	if err == nil {
		err = toolschema.Validate(t.input, args)
	}
	if err != nil {
		return refusedCall(res, core.CodeInvalidArgument, argumentMessage(call.Name, err))
	}
	return prepared{res: res, t: t, args: args, write: write, part: part}
}

// bindKey is a write's arguments with its idempotency key, which Core's
// schema describes beside the tool's own (and REST sends as a header):
// always the runtime's, whatever the model wrote (Reverse took the model's
// out).
func bindKey(args json.RawMessage, key string) (json.RawMessage, error) {
	v, err := decodeJSON(args)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("toolset: the arguments are not an object")
	}
	m["idempotency_key"] = key
	return json.RawMessage(encodeJSON(m)), nil
}

// send sends one prepared call. Its error is fatal to the answer;
// everything else is in the result.
func (s *Set) send(ctx context.Context, r Runner, p prepared) (llm.Part, *llm.File, *core.Envelope, error) {
	res := p.res
	env, err := r.Client.Call(ctx, res.Name, p.args)
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return res, nil, nil, err
	case ctx.Err() != nil:
		return res, nil, nil, ctx.Err()
	case (err != nil || env == nil) && p.write:
		return refuse(res, codeUnavailable, "Core could not be reached for this change, and it may or may not have been made. "+
			"Call it again with exactly the same arguments to find out: it goes under the same key, and is never made twice"), nil, nil, nil
	case err != nil, env == nil:
		return refuse(res, codeUnavailable,
			"Core could not be reached for this call; answer without it, or try it once more"), nil, nil, nil
	}
	content, file := r.render(ctx, res.Name, env, p.part)
	res.Content = content
	res.IsError = env.Status != core.StatusExecuted && env.Status != core.StatusProposed
	return res, file, env, nil
}

// record is what came of a write sent: env nil when Core did not answer.
func record(p prepared, env *core.Envelope) WriteRecord {
	w := WriteRecord{N: p.n, Key: p.key, Tool: p.res.Name, Status: StatusUnreachable}
	if env == nil {
		return w
	}
	w.Status, w.Code, w.Reason, w.ActionID, w.Replayed = string(env.Status), env.Code(), env.Reason(), env.ActionID, env.Replayed
	if env.Status == core.StatusExecuted {
		w.IDs = resultIDs(env.Result)
	}
	return w
}

// maxIDs bounds the ids a WriteRecord keeps of a result.
const maxIDs = 8

// resultIDs are a result's top-level members named *_id whose values are
// short strings: what a write made, never what anyone wrote.
func resultIDs(raw json.RawMessage) map[string]string {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := map[string]string{}
	for _, k := range sortedKeys(m) {
		var v string
		if !strings.HasSuffix(k, "_id") || len(k) > 64 || json.Unmarshal(m[k], &v) != nil || v == "" || len(v) > 64 || !isID(v) {
			continue
		}
		out[k] = v
		if len(out) == maxIDs {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isID reports whether v looks like an id: letters, digits, '-' and '_'.
func isID(v string) bool {
	return strings.IndexFunc(v, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_'
	}) < 0
}

func (s *Set) lookup(name string) (*offered, bool) {
	if s == nil {
		return nil, false
	}
	t, ok := s.tools[name]
	return t, ok
}

// maxNameInMessage bounds how much of a name the model made up is quoted
// back to it: every API's names are at most 128 characters, and a longer one
// must not push the result past its size.
const maxNameInMessage = 128

func (s *Set) noSuchTool(name string) string {
	msg := fmt.Sprintf("no such tool here: %q", cut(name, maxNameInMessage))
	if s.Len() == 0 {
		return msg + "; no tools are offered here"
	}
	return msg + "; the tools offered are: " + strings.Join(s.names, ", ")
}

func argumentMessage(tool string, err error) string {
	var ae *toolschema.ArgumentError
	if errors.As(err, &ae) {
		return ae.Msg
	}
	// Check keeps this from happening: the schema is Core's, not the
	// model's to fix.
	return "the runtime could not check the arguments to " + tool + "; answer without it"
}

// isObject reports whether raw is one JSON object; no arguments at all are
// the empty object, as Core takes them.
func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return true
	}
	if raw[0] != '{' {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// refused is a result the runtime makes itself, in the shape of Core's
// envelope, so that it begins with its status and speaks for itself.
type refused struct {
	Status core.Status `json:"status"`
	Error  core.Error  `json:"error"`
}

func refuse(res llm.Part, code, msg string) llm.Part {
	res.Content = encodeJSON(refused{Status: core.StatusError, Error: core.Error{Code: code, Message: msg}})
	res.IsError = true
	return res
}

// content is a result as the model gets it: Core's envelope, field for
// field in Core's order (status first), then what became of a document's
// file, and its text when that is given as text.
type content struct {
	Status      core.Status     `json:"status"`
	ActionID    string          `json:"action_id,omitempty"`
	ReviewState string          `json:"review_state,omitempty"`
	Replayed    bool            `json:"replayed,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Note        string          `json:"note,omitempty"`
	Error       *core.Error     `json:"error,omitempty"`
	File        *fileRecord     `json:"file,omitempty"`
	FileText    string          `json:"file_text,omitempty"`
}

// truncated is a result cut to size (rule 5): status and error whole, and
// as much of the result's JSON as fits, as a string.
type truncated struct {
	Status          core.Status `json:"status"`
	Error           *core.Error `json:"error,omitempty"`
	ActionID        string      `json:"action_id,omitempty"`
	File            *fileRecord `json:"file,omitempty"`
	ResultTruncated string      `json:"result_truncated"`
}

// render is the content of Core's answer to a call, and the file to give
// the model beside it, if any; part is the part of a document's file text
// the model asked for, 0 for none.
func (r Runner) render(ctx context.Context, tool string, env *core.Envelope, part int) (string, *llm.File) {
	c := content{Status: env.Status, ActionID: env.ActionID, ReviewState: env.ReviewState,
		Replayed: env.Replayed, Note: env.Note, Error: env.Error}
	var doc *docFile
	if len(env.Result) > 0 {
		// A result that does not read cannot be searched for URLs, so none
		// of it goes to the model.
		if v, err := decodeJSON(env.Result); err == nil {
			if tool == "document_get" && env.Status == core.StatusExecuted {
				doc = documentFile(v)
			}
			stripDownloadURLs(v)
			c.Result = json.RawMessage(encodeJSON(v))
		}
	}
	if doc == nil {
		return r.fit(c, nil, given{}, 0), nil
	}
	g := r.giveFile(ctx, doc)
	c.File = g.rec
	if part > 1 && g.text == "" && g.rec.GivenAs != givenNot {
		g.rec.Note = strings.TrimPrefix(g.rec.Note+"; ", "; ") + FilePartArg + " does not apply: the file itself is given, whole"
	}
	return r.fit(c, doc, g, part), g.file
}

// fit makes c at most MaxResultBytes: the envelope, then a file's text, whole
// when it fits a part (partBudget) and the part of it the model asked for
// otherwise (pageText), in the room left, cut to fit; an envelope too large
// on its own is truncated.
func (r Runner) fit(c content, d *docFile, g given, part int) string {
	limit := r.MaxResultBytes
	if text := g.text; text != "" {
		if escapedLenOf(text) > r.partBudget() || part > 1 {
			text = r.pageText(c.File, d, g.text, g.sections, part)
		}
		if text != "" {
			room := limit - len(encodeJSON(c)) - len(`,"file_text":`)
			if escapedLenOf(text)+len(`""`) > room && c.File.Part > 0 {
				// Only an envelope far larger than a document's leaves a
				// part too little room: it is cut, and says so.
				c.File.Note += "; this part is cut short, as the rest of the result leaves it too little room"
				room = limit - len(encodeJSON(c)) - len(`,"file_text":`)
			}
			if t, ok := fitString(text, room); ok && room >= minTextRoom {
				c.FileText = t
				return encodeJSON(c)
			}
			c.File.GivenAs, c.File.ExtractedFrom = givenNot, ""
			c.File.Part, c.File.PartHolds, c.File.NextPart = 0, "", nil
			c.File.Note = "the file's text could not be given to the model: the result left no room for it"
		}
	}
	if out := encodeJSON(c); len(out) <= limit {
		return out
	}
	tr := truncated{Status: c.Status, Error: c.Error, ActionID: c.ActionID, File: c.File}
	// Status and error are kept whole. Only an error whose details alone
	// pass the limit loses them, then the file's record, then the message's
	// tail: the result must fit, and the code still says what happened.
	for range 4 {
		room := limit - len(encodeJSON(tr)) + len(`""`)
		if rt, ok := fitString(string(c.Result), room); ok {
			tr.ResultTruncated = rt
			return encodeJSON(tr)
		}
		switch {
		case tr.Error != nil && tr.Error.Details != nil:
			e := *tr.Error
			e.Details = nil
			tr.Error = &e
		case tr.File != nil:
			tr.File = nil
		case tr.Error != nil:
			e := *tr.Error
			e.Message = cut(e.Message, 200)
			tr.Error = &e
		}
	}
	// Not reached for any limit of at least minResultBytes.
	last := truncated{Status: c.Status}
	if c.Error != nil {
		last.Error = &core.Error{Code: c.Error.Code}
	}
	return encodeJSON(last)
}

// minTextRoom is the least room worth giving a file's text; less, and the
// model is told there was no room instead.
const minTextRoom = 256

// fitString is s as a JSON string of at most room bytes, quotes included:
// whole if it fits, else its longest prefix, cut on a rune boundary,
// followed by "…[truncated, N bytes]", N being s's length. ok is false when
// not even the mark fits.
func fitString(s string, room int) (string, bool) {
	// Escaping only lengthens a string, so one longer than the room is not
	// encoded whole to find that out: a file's text may be megabytes.
	if len(s)+len(`""`) <= room && len(encodeJSON(s)) <= room {
		return s, true
	}
	mark := fmt.Sprintf("…[truncated, %d bytes]", len(s))
	best := -1
	lo, hi := 0, min(len(s), room)
	for lo <= hi {
		mid := (lo + hi) / 2
		n := runeFloor(s, mid)
		if len(encodeJSON(s[:n]+mark)) <= room {
			best = n
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		return "", false
	}
	return s[:best] + mark, true
}

// runeFloor is the largest rune boundary of s at or before n.
func runeFloor(s string, n int) int {
	n = min(n, len(s))
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// cut shortens s to at most n bytes, on a rune boundary, marking the cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:runeFloor(s, n-len("…"))] + "…"
}

// decodeJSON parses JSON keeping numbers exact.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// encodeJSON writes v compactly, with <, > and & as they are: a model reads
// them. It cannot fail for the values here (maps, slices, strings,
// json.Number, the envelope's types).
func encodeJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return `{"status":"error","error":{"code":"internal","message":"the runtime could not write this result"}}`
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// stripDownloadURLs removes every download_url from a result: the URL is a
// bearer credential for the file, and the model never gets it (rule 6).
func stripDownloadURLs(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "download_url")
		for _, e := range x {
			stripDownloadURLs(e)
		}
	case []any:
		for _, e := range x {
			stripDownloadURLs(e)
		}
	}
}
