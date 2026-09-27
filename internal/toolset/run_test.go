package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

func delegateSet(t testing.TB) *Set {
	t.Helper()
	s, err := snapshot(t).Build(delegatePerms, config.Tools{}, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runner(f *fakeCore) Runner {
	return Runner{Client: core.NewClient(f)}
}

func TestRunCallsCore(t *testing.T) {
	f := &fakeCore{respond: func(_ context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
		return executed(`{"tool":"` + tool + `","n":12345678901234567890.5}`), nil
	}}
	parts, err := delegateSet(t).Run(context.Background(), runner(f), courseID, []llm.Part{
		// The model names another course, and sends the nulls a strict
		// model sends for what it leaves out.
		call("c1", "document_get", `{"course_id":"0192f3c1-0000-7000-8000-000000000000","document_id":"`+docID+`","version_id":null}`),
		call("c2", "grade_list", `{"assignment_id":null,"student_member_id":null,"after":null,"limit":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("%d parts, want 2", len(parts))
	}
	for i, id := range []string{"c1", "c2"} {
		p := parts[i]
		if p.Type != llm.PartToolResult || p.CallID != id || p.IsError {
			t.Errorf("part %d: %+v", i, p)
		}
	}
	if parts[0].Name != "document_get" || parts[1].Name != "grade_list" {
		t.Errorf("names %s, %s", parts[0].Name, parts[1].Name)
	}
	// Core's envelope, status first, numbers exact.
	if want := `{"status":"executed","result":{"n":12345678901234567890.5,"tool":"grade_list"}}`; parts[1].Content != want {
		t.Errorf("content %s\nwant    %s", parts[1].Content, want)
	}
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("%d calls to Core, want 2", len(calls))
	}
	for _, c := range calls {
		if c.args["course_id"] != courseID {
			t.Errorf("%s went to course %v, want the conversation's", c.tool, c.args["course_id"])
		}
		if c.priority != core.PriorityAnswer {
			t.Errorf("%s went at priority %v, want PriorityAnswer", c.tool, c.priority)
		}
	}
	byTool := map[string]map[string]any{}
	for _, c := range calls {
		byTool[c.tool] = c.args
	}
	if _, has := byTool["grade_list"]["limit"]; has {
		t.Errorf("grade_list got limit: null, which Core refuses: %v", byTool["grade_list"])
	}
	if v, has := byTool["grade_list"]["after"]; !has || v != nil {
		t.Errorf("grade_list lost after: null, which Core takes: %v", byTool["grade_list"])
	}
}

func TestRunRefusesBeforeCore(t *testing.T) {
	s := delegateSet(t)
	tests := []struct {
		name     string
		call     llm.Part
		code     string
		messages []string
	}{
		{
			name: "a tool not offered", call: call("c", "member_add", `{}`), code: core.CodeNotFound,
			messages: []string{`no such tool here: "member_add"`, "the tools offered are: assignment_get, assignment_list,"},
		},
		{
			name: "arguments that did not parse",
			call: llm.Part{Type: llm.PartToolCall, ID: "c", Name: "course_get", Args: json.RawMessage(`{}`), ArgsError: `{"course_id":`},
			code: core.CodeInvalidArgument, messages: []string{"not a JSON object"},
		},
		{
			name: "arguments that are not an object", call: call("c", "course_get", `["x"]`),
			code: core.CodeInvalidArgument, messages: []string{"not a JSON object"},
		},
		{
			name: "arguments Core's schema refuses", call: call("c", "grade_list", `{"limit":"ten"}`),
			code: core.CodeInvalidArgument, messages: []string{"limit: ", "want"},
		},
		{
			name: "a required argument missing", call: call("c", "document_get", ``),
			code: core.CodeInvalidArgument, messages: []string{"required", "document_id"},
		},
		{
			name: "not a UUID", call: call("c", "document_get", `{"document_id":"the syllabus"}`),
			code: core.CodeInvalidArgument, messages: []string{`document_id: "the syllabus" is not a UUID`},
		},
		{
			name: "a property Core does not know", call: call("c", "course_get", `{"verbose":true}`),
			code: core.CodeInvalidArgument, messages: []string{"unexpected additional properties", "verbose"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCore{}
			parts, err := s.Run(context.Background(), runner(f), courseID, []llm.Part{tc.call})
			if err != nil {
				t.Fatal(err)
			}
			if len(f.recorded()) != 0 {
				t.Fatalf("Core was called: %v", f.recorded())
			}
			p := parts[0]
			if !p.IsError || p.CallID != "c" || p.Name != tc.call.Name {
				t.Errorf("part %+v", p)
			}
			if contentOf(t, p)["status"] != "error" {
				t.Errorf("status %v, want error", contentOf(t, p)["status"])
			}
			code, msg := errorOf(t, p)
			if code != tc.code {
				t.Errorf("code %q, want %q", code, tc.code)
			}
			for _, m := range tc.messages {
				if !strings.Contains(msg, m) {
					t.Errorf("message %q does not say %q", msg, m)
				}
			}
		})
	}
}

func TestRunEmptySet(t *testing.T) {
	s, err := snapshot(t).Build(delegatePerms, config.Tools{Mode: "none"}, toolschema.OpenAI, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCore{}
	parts, err := s.Run(context.Background(), runner(f), courseID, []llm.Part{call("c", "course_get", `{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, msg := errorOf(t, parts[0]); !strings.Contains(msg, "no tools are offered here") {
		t.Errorf("message %q", msg)
	}
}

// TestRunDeniedAgain checks the built-in deny list and reads only in Run
// itself, for a set that holds a denied tool or a write however it came to:
// each is refused before its arguments are looked at, and Core never sees
// it.
func TestRunDeniedAgain(t *testing.T) {
	cat := snapshot(t)
	held := func(name, kind string) *offered {
		return &offered{input: cat.Tools[name].InputSchema, kind: kind}
	}
	s := &Set{tools: map[string]*offered{
		"member_list":  held("member_list", KindRead),
		"grade_submit": held("grade_submit", KindWrite),
		"grade_list":   held("grade_list", ""),
		"course_get":   held("course_get", KindRead),
	}, names: []string{"course_get", "grade_list", "grade_submit", "member_list"}}
	f := &fakeCore{}
	parts, err := s.Run(context.Background(), runner(f), courseID, []llm.Part{
		call("a", "member_list", `{}`),
		call("b", "member_list", `["not an object"]`),
		call("c", "grade_submit", `{"submission_id":"`+docID+`","score":"87.5"}`),
		call("d", "grade_list", `{}`),
		call("e", "course_get", `{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts[:4] {
		if code, msg := errorOf(t, p); code != core.CodeForbidden || !p.IsError || !strings.Contains(msg, "not offered") {
			t.Errorf("%s: code %q (%s), want forbidden", p.CallID, code, msg)
		}
	}
	if parts[4].IsError {
		t.Errorf("a read in the same set was refused: %s", parts[4].Content)
	}
	if calls := f.recorded(); len(calls) != 1 || calls[0].tool != "course_get" {
		t.Errorf("Core was called with %v, want course_get alone", calls)
	}
}

// TestRunUnknownNameClipped checks that a name the model made up is quoted
// back short: a long one must not make the result long.
func TestRunUnknownNameClipped(t *testing.T) {
	name := strings.Repeat("x", 100000)
	parts, err := delegateSet(t).Run(context.Background(), runner(&fakeCore{}), courseID, []llm.Part{call("c", name, `{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(parts[0].Content); n > 1024 {
		t.Errorf("%d bytes for an unknown name", n)
	}
	if _, msg := errorOf(t, parts[0]); !strings.Contains(msg, "no such tool here") {
		t.Errorf("message %q", msg)
	}
}

func TestRunEnvelopes(t *testing.T) {
	tests := []struct {
		name    string
		env     *core.Envelope
		isError bool
		want    string
	}{
		{
			name: "executed", env: executed(`{"id":"x"}`),
			want: `{"status":"executed","result":{"id":"x"}}`,
		},
		{
			name: "denied", isError: true,
			env: &core.Envelope{Status: core.StatusDenied, Error: &core.Error{Code: core.CodeForbidden,
				Message: "grade_read is denied", Details: map[string]any{"permission": "grade_read"}}},
			want: `{"status":"denied","error":{"code":"forbidden","message":"grade_read is denied","details":{"permission":"grade_read"}}}`,
		},
		{
			name: "failed", isError: true,
			env:  &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: core.CodeFailedPrecondition, Message: "not yet"}},
			want: `{"status":"failed","error":{"code":"failed_precondition","message":"not yet"}}`,
		},
		{
			name: "error", isError: true,
			env:  &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeNotFound, Message: "no such grade"}},
			want: `{"status":"error","error":{"code":"not_found","message":"no such grade"}}`,
		},
		{
			name: "a note and <html> kept as written", env: &core.Envelope{Status: core.StatusExecuted,
				Result: json.RawMessage(`{"body_md":"a <b>bold</b> & more"}`), Note: "n"},
			want: `{"status":"executed","result":{"body_md":"a <b>bold</b> & more"},"note":"n"}`,
		},
		{
			name: "a result that is not JSON is left out", env: &core.Envelope{Status: core.StatusExecuted,
				Result: json.RawMessage(`{"download_url":"https://files.example/x?sig=1"`)},
			want: `{"status":"executed"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) { return tc.env, nil }}
			parts, err := delegateSet(t).Run(context.Background(), runner(f), courseID, []llm.Part{call("c", "grade_get", `{"grade_id":"`+docID+`"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if parts[0].IsError != tc.isError {
				t.Errorf("is_error %v, want %v", parts[0].IsError, tc.isError)
			}
			if parts[0].Content != tc.want {
				t.Errorf("content %s\nwant    %s", parts[0].Content, tc.want)
			}
		})
	}
}

func TestRunStripsDownloadURLs(t *testing.T) {
	const url = "https://store.example/blob/abc?X-Amz-Signature=deadbeef"
	f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		return executed(`{"id":"d","title":"Notes","version":{"id":"v","body_md":null,"download_url":"` + url +
			`","content_type":"application/zip","byte_size":10},"elsewhere":[{"download_url":"` + url + `"}]}`), nil
	}}
	for _, tool := range []string{"document_get", "submission_get"} {
		args := `{"document_id":"` + docID + `"}`
		if tool == "submission_get" {
			args = `{"submission_id":"` + docID + `"}`
		}
		parts, err := delegateSet(t).Run(context.Background(), runner(f), courseID, []llm.Part{call("c", tool, args)})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(parts[0].Content, "download_url") || strings.Contains(parts[0].Content, "Signature") {
			t.Errorf("%s: the URL reached the model: %s", tool, parts[0].Content)
		}
	}
}

// TestRunParallel checks that calls run at most MaxParallel at once (four
// by default), and that results come back in call order whatever order
// they finish in.
func TestRunParallel(t *testing.T) {
	for _, tc := range []struct{ maxParallel, want int }{{3, 3}, {0, DefaultMaxParallel}, {1, 1}} {
		t.Run(fmt.Sprint(tc.maxParallel), func(t *testing.T) {
			const n = 9
			var inflight, peak atomic.Int32
			started := make(chan struct{}, n)
			release := make(chan struct{})
			f := &fakeCore{respond: func(_ context.Context, _ string, args json.RawMessage) (*core.Envelope, error) {
				now := inflight.Add(1)
				for {
					p := peak.Load()
					if now <= p || peak.CompareAndSwap(p, now) {
						break
					}
				}
				started <- struct{}{}
				<-release
				inflight.Add(-1)
				var m map[string]any
				_ = json.Unmarshal(args, &m)
				return executed(fmt.Sprintf(`{"limit":%v}`, m["limit"])), nil
			}}
			calls := make([]llm.Part, n)
			for i := range calls {
				calls[i] = call(fmt.Sprintf("c%d", i), "grade_list", fmt.Sprintf(`{"limit":%d}`, i+1))
			}
			done := make(chan []llm.Part)
			go func() {
				r := runner(f)
				r.MaxParallel = tc.maxParallel
				parts, err := delegateSet(t).Run(context.Background(), r, courseID, calls)
				if err != nil {
					t.Error(err)
				}
				done <- parts
			}()
			for i := range tc.want {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatalf("only %d calls started, want %d at once", i, tc.want)
				}
			}
			select {
			case <-started:
				t.Fatalf("a call started past the limit of %d", tc.want)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			parts := <-done
			if int(peak.Load()) != tc.want {
				t.Errorf("peak of %d calls at once, want %d", peak.Load(), tc.want)
			}
			if len(parts) != n {
				t.Fatalf("%d parts, want %d", len(parts), n)
			}
			for i, p := range parts {
				if p.CallID != fmt.Sprintf("c%d", i) || !strings.Contains(p.Content, fmt.Sprintf(`"limit":%d`, i+1)) {
					t.Errorf("part %d is %s %s: out of order", i, p.CallID, p.Content)
				}
			}
		})
	}
}

func TestRunTransportErrors(t *testing.T) {
	for name, err := range map[string]error{
		"5xx after retries": &core.TransientError{Status: 503, Err: errors.New("unavailable")},
		"429 after retries": &core.RateLimitedError{RetryAfter: time.Second},
		"a protocol error":  &core.ProtocolError{Code: -32603, Message: "boom"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) { return nil, err }}
			parts, runErr := delegateSet(t).Run(context.Background(), runner(f), courseID, []llm.Part{call("c", "course_get", `{}`)})
			if runErr != nil {
				t.Fatalf("Run failed: %v", runErr)
			}
			code, msg := errorOf(t, parts[0])
			if !parts[0].IsError || code != codeUnavailable || !strings.Contains(msg, "Core could not be reached") {
				t.Errorf("part %+v", parts[0])
			}
		})
	}
}

// TestRunUnauthenticated checks that a 401 stops the whole batch: the
// other calls are cancelled and Run returns the error, for the worker to
// stop the agent.
func TestRunUnauthenticated(t *testing.T) {
	f := &fakeCore{respond: func(ctx context.Context, tool string, _ json.RawMessage) (*core.Envelope, error) {
		if tool == "course_get" {
			return nil, core.ErrUnauthenticated
		}
		<-ctx.Done()
		return nil, &core.TransientError{Err: ctx.Err()}
	}}
	parts, err := delegateSet(t).Run(context.Background(), runner(f), courseID, []llm.Part{
		call("a", "grade_list", `{}`), call("b", "course_get", `{}`), call("c", "assignment_list", `{}`),
	})
	if !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("err %v, want ErrUnauthenticated", err)
	}
	if parts != nil {
		t.Errorf("parts %v, want none", parts)
	}
}

func TestRunContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeCore{respond: func(ctx context.Context, _ string, _ json.RawMessage) (*core.Envelope, error) {
		cancel()
		<-ctx.Done()
		return nil, &core.TransientError{Err: ctx.Err()}
	}}
	_, err := delegateSet(t).Run(ctx, runner(f), courseID, []llm.Part{call("a", "course_get", `{}`), call("b", "course_get", `{}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the context's", err)
	}
}

func TestRunNothingToRun(t *testing.T) {
	parts, err := delegateSet(t).Run(context.Background(), Runner{}, courseID, []llm.Part{llm.Text("thinking aloud")})
	if err != nil || parts != nil {
		t.Fatalf("parts %v, err %v", parts, err)
	}
	if _, err := delegateSet(t).Run(context.Background(), Runner{}, courseID, []llm.Part{call("c", "course_get", `{}`)}); err == nil {
		t.Fatal("a runner with no client ran a call")
	}
}

func TestRunTruncates(t *testing.T) {
	big := strings.Repeat(`é"\`, 20000)
	bigJSON, _ := json.Marshal(big)
	result := `{"body_md":` + string(bigJSON) + `}`
	tests := []struct {
		name  string
		limit int
		env   *core.Envelope
	}{
		{"executed, at the default limit", 0, executed(result)},
		{"executed, at a small limit", 2000, executed(result)},
		{"failed, with its error kept whole", 2000, &core.Envelope{Status: core.StatusFailed, ActionID: "act-1",
			Result: json.RawMessage(result), Error: &core.Error{Code: core.CodeConflict, Message: strings.Repeat("m", 400),
				Details: map[string]any{"reason": "moved_on"}}}},
		{"under the floor of 1 KiB", 10, executed(result)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) { return tc.env, nil }}
			r := runner(f)
			r.MaxResultBytes = tc.limit
			parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("c", "course_get", `{}`)})
			if err != nil {
				t.Fatal(err)
			}
			limit := max(r.withDefaults().MaxResultBytes, minResultBytes)
			got := parts[0].Content
			if len(got) > limit {
				t.Fatalf("%d bytes, over the limit of %d", len(got), limit)
			}
			if len(got) < limit-40 {
				t.Errorf("%d bytes: the limit of %d is not used", len(got), limit)
			}
			m := contentOf(t, parts[0])
			if m["status"] != string(tc.env.Status) {
				t.Errorf("status %v", m["status"])
			}
			rt, _ := m["result_truncated"].(string)
			mark := fmt.Sprintf("…[truncated, %d bytes]", len(result))
			if !strings.HasSuffix(rt, mark) || !strings.HasPrefix(result, strings.TrimSuffix(rt, mark)) {
				t.Errorf("result_truncated %q…%q is not a prefix and the mark %q", rt[:20], rt[len(rt)-30:], mark)
			}
			if tc.env.Error != nil {
				e := m["error"].(map[string]any)
				if e["message"] != tc.env.Error.Message || e["code"] != tc.env.Error.Code || e["details"] == nil {
					t.Errorf("error not kept whole: %v", e)
				}
				if m["action_id"] != tc.env.ActionID {
					t.Errorf("action_id %v", m["action_id"])
				}
			}
		})
	}
}

func TestFitString(t *testing.T) {
	for _, s := range []string{"", "short", strings.Repeat("a", 100), strings.Repeat("日本", 50), strings.Repeat("\"\n\\", 40), strings.Repeat("\x01", 30)} {
		for room := 0; room < 140; room++ {
			got, ok := fitString(s, room)
			if !ok {
				continue
			}
			if n := len(encodeJSON(got)); n > room {
				t.Fatalf("fitString(%q, %d) is %d bytes as JSON", s, room, n)
			}
			if !strings.HasPrefix(s, strings.TrimSuffix(got, fmt.Sprintf("…[truncated, %d bytes]", len(s)))) {
				t.Fatalf("fitString(%q, %d) = %q is not a prefix", s, room, got)
			}
		}
	}
}

// TestRunTruncatesOversizedErrors checks the last resorts of rule 5: an
// error whose details alone pass the limit loses them, and one whose message
// does is cut, so that the result still fits and still says what happened.
func TestRunTruncatesOversizedErrors(t *testing.T) {
	result := json.RawMessage(`{"body_md":"` + strings.Repeat("r", 5000) + `"}`)
	tests := []struct {
		name        string
		err         *core.Error
		wantDetails bool
		wantMessage string
	}{
		{
			name:        "details that fit are kept",
			err:         &core.Error{Code: core.CodeConflict, Message: "moved on", Details: map[string]any{"reason": "moved_on"}},
			wantDetails: true, wantMessage: "moved on",
		},
		{
			name:        "details too large for the limit are dropped, the message kept",
			err:         &core.Error{Code: core.CodeInvalidArgument, Message: "too long", Details: map[string]any{"echo": strings.Repeat("d", 4000)}},
			wantMessage: "too long",
		},
		{
			name:        "a message too large for the limit is cut",
			err:         &core.Error{Code: core.CodeInternal, Message: strings.Repeat("m", 3000)},
			wantMessage: strings.Repeat("m", 197) + "…",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := &core.Envelope{Status: core.StatusFailed, ActionID: "act-1", Result: result, Error: tc.err}
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) { return env, nil }}
			r := runner(f)
			r.MaxResultBytes = minResultBytes
			parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("c", "course_get", `{}`)})
			if err != nil {
				t.Fatal(err)
			}
			got := parts[0].Content
			if len(got) > minResultBytes {
				t.Fatalf("%d bytes, over the limit of %d", len(got), minResultBytes)
			}
			m := contentOf(t, parts[0])
			e, _ := m["error"].(map[string]any)
			if m["status"] != "failed" || e["code"] != tc.err.Code || e["message"] != tc.wantMessage {
				t.Errorf("status %v, error %v", m["status"], e)
			}
			if _, has := e["details"]; has != tc.wantDetails {
				t.Errorf("details kept %v, want %v", has, tc.wantDetails)
			}
			if rt, _ := m["result_truncated"].(string); !strings.HasSuffix(rt, fmt.Sprintf("…[truncated, %d bytes]", len(result))) {
				t.Errorf("result_truncated %q", rt)
			}
			if !parts[0].IsError {
				t.Error("a failed envelope is not an error")
			}
		})
	}
}
