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
	// MaxFileBytes bounds a file fetched for the model (rule 6);
	// DefaultMaxFileBytes when 0 or less.
	MaxFileBytes int64
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

// Run runs the model's tool calls (the tool_call parts of calls; any other
// part is passed over) and returns one tool_result part per call, in call
// order, with Name and CallID set, followed by the file parts of any files
// given to the model (rule 6). At most MaxParallel calls run at once, each
// with ctx marked core.PriorityAnswer.
//
// A call is checked before it reaches Core, and any failure is an is_error
// result the model can correct itself from, with no call to Core: a tool
// not offered here ("no such tool", naming those that are); arguments that
// are not a JSON object (rule 1); arguments Core's schema refuses once
// course_id is set to courseID, the conversation's course, whatever the
// model wrote (toolschema.Reverse, Validate); a tool on the built-in deny
// list, checked again whatever built the set. Core's answer is Core's
// envelope as JSON, is_error unless executed, with every download_url
// taken out and cut to MaxResultBytes keeping status and error whole.
//
// A call Core did not answer is one of two things. Fatal: a 401
// (core.ErrUnauthenticated: the agent must stop) or ctx done (the answer's
// wall clock is spent). Run then stops the other calls and returns the
// error and no parts: the answer cannot go on. Anything else (a 5xx or a
// 429 the retrying caller underneath gave up on, a protocol error) is an
// is_error result saying Core could not be reached, and the model answers
// without it.
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
	parent := ctx
	ctx, cancel := context.WithCancel(core.WithPriority(parent, core.PriorityAnswer))
	defer cancel()

	type outcome struct {
		part llm.Part
		file *llm.File
		err  error
	}
	outs := make([]outcome, len(toolCalls))
	sem := make(chan struct{}, r.MaxParallel)
	var wg sync.WaitGroup
	for i, call := range toolCalls {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outs[i].err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			part, file, err := s.runOne(ctx, r, courseID, call)
			if err != nil {
				cancel()
			}
			outs[i] = outcome{part, file, err}
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
	for _, o := range outs {
		parts = append(parts, o.part)
	}
	for _, o := range outs {
		if o.file != nil {
			parts = append(parts, llm.Part{Type: llm.PartFile, File: o.file})
		}
	}
	return parts, nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// runOne runs one call. Its error is fatal to the answer; everything else
// is in the result.
func (s *Set) runOne(ctx context.Context, r Runner, courseID string, call llm.Part) (llm.Part, *llm.File, error) {
	res := llm.Part{Type: llm.PartToolResult, CallID: call.ID, Name: call.Name}
	t, ok := s.lookup(call.Name)
	if !ok {
		return refuse(res, core.CodeNotFound, s.noSuchTool(call.Name)), nil, nil
	}
	if call.ArgsError != "" || !isObject(call.Args) {
		return refuse(res, core.CodeInvalidArgument, fmt.Sprintf(
			"the arguments to %s are not a JSON object; call it again with its parameters as one JSON object", call.Name)), nil, nil
	}
	args, err := toolschema.Reverse(t.input, call.Args, map[string]any{"course_id": courseID})
	if err == nil {
		err = toolschema.Validate(t.input, args)
	}
	if err != nil {
		return refuse(res, core.CodeInvalidArgument, argumentMessage(call.Name, err)), nil, nil
	}
	// The deny list holds at every stage (§6.1), whatever built this set.
	if BuiltinDenied(call.Name) {
		return refuse(res, core.CodeForbidden, call.Name+" is not offered to the model"), nil, nil
	}
	env, err := r.Client.Call(ctx, call.Name, args)
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return res, nil, err
	case ctx.Err() != nil:
		return res, nil, ctx.Err()
	case err != nil, env == nil:
		return refuse(res, codeUnavailable,
			"Core could not be reached for this call; answer without it, or try it once more"), nil, nil
	}
	content, file := r.render(ctx, call.Name, env)
	res.Content = content
	res.IsError = env.Status != core.StatusExecuted
	return res, file, nil
}

func (s *Set) lookup(name string) (*offered, bool) {
	if s == nil {
		return nil, false
	}
	t, ok := s.tools[name]
	return t, ok
}

func (s *Set) noSuchTool(name string) string {
	msg := fmt.Sprintf("there is no tool named %q here", name)
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
// the model beside it, if any.
func (r Runner) render(ctx context.Context, tool string, env *core.Envelope) (string, *llm.File) {
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
	var text string
	var file *llm.File
	if doc != nil {
		c.File, text, file = r.giveFile(ctx, doc)
	}
	return r.fit(c, text), file
}

// fit makes c at most MaxResultBytes: the envelope, then a text file's
// text in the room left, cut to fit; an envelope too large on its own is
// truncated.
func (r Runner) fit(c content, text string) string {
	limit := r.MaxResultBytes
	if text != "" {
		base := encodeJSON(c)
		room := limit - len(base) - len(`,"file_text":`)
		if t, ok := fitString(text, room); ok && room >= minTextRoom {
			c.FileText = t
			return encodeJSON(c)
		}
		c.File.GivenAs = givenNot
		c.File.Note = "the file's text could not be given to the model: the result left no room for it"
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
