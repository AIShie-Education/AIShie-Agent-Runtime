package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

func ask(q string) *llm.Request {
	return &llm.Request{System: "sys", Messages: []llm.Message{llm.UserText(q)}, ToolMode: llm.ToolAuto}
}

func TestStepsPlayInOrder(t *testing.T) {
	a := New(
		CallTool("assignment_list", `{}`),
		CallTools(ToolCall{Name: "grade_list", Args: `{"assignment_id":"hw3"}`}, ToolCall{ID: "mine", Name: "assignment_get", Args: `{"assignment_id":"hw3"}`}),
		Stop(llm.StopMaxTokens, "partial"),
		Reply("HW3 is due Friday."),
	)
	ctx := context.Background()

	r1, err := a.Call(ctx, ask("When is HW3 due?"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Stop != llm.StopToolCalls || len(r1.ToolCalls()) != 1 || r1.ToolCalls()[0].ID != "call_1_1" || string(r1.ToolCalls()[0].Args) != "{}" {
		t.Errorf("first = %+v", r1)
	}

	r2, err := a.Call(ctx, ask("again"))
	if err != nil {
		t.Fatal(err)
	}
	calls := r2.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "call_2_1" || calls[1].ID != "mine" || calls[0].Name != "grade_list" {
		t.Errorf("second = %+v", calls)
	}

	r3, _ := a.Call(ctx, ask("more"))
	if r3.Stop != llm.StopMaxTokens || r3.Text() != "partial" {
		t.Errorf("third = %+v", r3)
	}

	r4, _ := a.Call(ctx, ask("last"))
	if r4.Stop != llm.StopEnd || r4.Text() != "HW3 is due Friday." || r4.RawStop != "end" {
		t.Errorf("fourth = %+v", r4)
	}
	if r4.Usage.Input != DefaultInputTokens || r4.Usage.Output != DefaultOutputTokens || r4.Usage.Estimated || len(r4.Usage.Raw) == 0 {
		t.Errorf("usage = %+v", r4.Usage)
	}
	if r4.Model != DefaultModel || r4.RequestID != "scripted-4" {
		t.Errorf("model %q, request id %q", r4.Model, r4.RequestID)
	}
	if a.Remaining() != 0 || a.Err() != nil {
		t.Errorf("remaining %d, err %v", a.Remaining(), a.Err())
	}
}

func TestRunningOutOfStepsIsAnError(t *testing.T) {
	a := New(Reply("one"))
	if _, err := a.Call(context.Background(), ask("q")); err != nil {
		t.Fatal(err)
	}
	_, err := a.Call(context.Background(), ask("q2"))
	var e *llm.Error
	if !errors.As(err, &e) || e.Retryable() {
		t.Fatalf("err = %v, want a non-retryable *llm.Error", err)
	}
	if !errors.Is(a.Err(), ErrOutOfSteps) {
		t.Errorf("Err() = %v", a.Err())
	}
	if _, err := a.Call(context.Background(), ask("q3")); err == nil {
		t.Error("a third call succeeded")
	}
	if len(a.Requests()) != 3 {
		t.Errorf("%d requests kept", len(a.Requests()))
	}

	a.Append(Reply("more"))
	if r, err := a.Call(context.Background(), ask("q4")); err != nil || r.Text() != "more" {
		t.Errorf("after Append: %v, %v", r, err)
	}
}

func TestAStepThatAnswersNothingIsAnError(t *testing.T) {
	a := New(func(context.Context, *llm.Request) (*llm.Response, error) { return nil, nil })
	if _, err := a.Call(context.Background(), ask("q")); err == nil || a.Err() == nil {
		t.Errorf("err %v, Err() %v", err, a.Err())
	}
}

func TestUnparseableArgumentsAreKept(t *testing.T) {
	a := New(CallTool("grade_list", `{"assignment_id": "hw3`), CallTool("course_get", ""))
	r, _ := a.Call(context.Background(), ask("q"))
	c := r.ToolCalls()[0]
	if string(c.Args) != "{}" || c.ArgsError != `{"assignment_id": "hw3` {
		t.Errorf("call = %+v", c)
	}
	r, _ = a.Call(context.Background(), ask("q"))
	if c := r.ToolCalls()[0]; string(c.Args) != "{}" || c.ArgsError != "" {
		t.Errorf("no arguments = %+v", c)
	}
}

func TestRequestsAreCopies(t *testing.T) {
	a := New(Reply("a"))
	req := &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{
			llm.Text("q"),
			{Type: llm.PartFile, File: &llm.File{Name: "f.pdf", MIME: "application/pdf", Data: []byte("pdf")}},
		}}},
		Tools: []llm.Tool{{Name: "t", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	if _, err := a.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Parts[0].Text = "changed"
	req.Messages[0].Parts[1].File.Data[0] = 'X'
	req.Tools[0].Schema[2] = 'X'

	got := a.Requests()[0]
	if got.Messages[0].Parts[0].Text != "q" || string(got.Messages[0].Parts[1].File.Data) != "pdf" || string(got.Tools[0].Schema) != `{"type":"object"}` {
		t.Errorf("the record changed with the caller's request: %+v", got)
	}
	got.Messages[0].Parts[0].Text = "mine"
	if a.Requests()[0].Messages[0].Parts[0].Text != "q" {
		t.Error("Requests hands out the record itself")
	}
}

func TestThenRunsItsSideEffect(t *testing.T) {
	var seen []string
	note := func(tag string) func(*llm.Request) {
		return func(req *llm.Request) { seen = append(seen, tag+":"+req.Messages[0].Parts[0].Text) }
	}
	a := New(
		Reply("first").Then(note("after")),
		Then(note("before")), Then(nil), Reply("second"),
		Then(note("dangling")),
	)
	r, err := a.Call(context.Background(), ask("one"))
	if err != nil || r.Text() != "first" {
		t.Errorf("first call: %v, %v", r, err)
	}
	r, err = a.Call(context.Background(), ask("two"))
	if err != nil || r.Text() != "second" {
		t.Errorf("second call: %v, %v", r, err)
	}
	if want := "after:one before:two"; strings.Join(seen, " ") != want {
		t.Errorf("side effects %q, want %q", seen, want)
	}
	if a.Remaining() != 1 || len(a.Requests()) != 2 {
		t.Errorf("remaining %d, requests %d", a.Remaining(), len(a.Requests()))
	}
	// A Then with nothing after it is a script that ran out.
	if _, err := a.Call(context.Background(), ask("three")); err == nil || !errors.Is(a.Err(), ErrOutOfSteps) {
		t.Errorf("err %v, Err() %v", err, a.Err())
	}
}

func TestFailRespondAndUsage(t *testing.T) {
	limited := &llm.Error{Kind: llm.ErrRateLimited, RetryAfter: time.Second}
	reasoning := llm.Part{Type: llm.PartReasoning, Text: "thinking", Opaque: json.RawMessage(`{"x":1}`)}
	a := New(
		Fail(limited),
		Respond(llm.Response{Parts: []llm.Part{reasoning, llm.Text("hi")}, Stop: llm.StopEnd}),
		WithUsage(Reply("costly"), llm.Usage{Input: 150000, Output: 10, Raw: json.RawMessage(`{"n":1}`)}),
	).WithMaker("me")

	if _, err := a.Call(context.Background(), ask("q")); !errors.Is(err, limited) {
		t.Errorf("err = %v", err)
	}
	r, err := a.Call(context.Background(), ask("q"))
	if err != nil || r.Parts[0].Maker != "me" || r.Text() != "hi" {
		t.Errorf("respond: %+v %v", r, err)
	}
	r, _ = a.Call(context.Background(), ask("q"))
	if r.Usage.Input != 150000 || string(r.Usage.Raw) != `{"n":1}` {
		t.Errorf("usage = %+v", r.Usage)
	}
}

func TestHangEndsWithTheContext(t *testing.T) {
	a := New(Hang())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := a.Call(ctx, ask("q"))
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrTimeout {
		t.Errorf("err = %v", err)
	}
}

func TestSettings(t *testing.T) {
	caps := llm.Capabilities{ToolsWithHistory: true}
	a := New().WithName("openai_chat").WithProvider("deepseek").WithModel("deepseek-chat").
		WithDialect(toolschema.FullCommon).WithCapabilities(caps)
	if a.Name() != "openai_chat" || a.Provider() != "deepseek" || a.Model() != "deepseek-chat" ||
		a.Dialect() != toolschema.FullCommon || a.Capabilities() != caps || a.Maker() != "openai_chat||deepseek-chat" {
		t.Errorf("settings: %s %s %s %s %+v %s", a.Name(), a.Provider(), a.Model(), a.Dialect(), a.Capabilities(), a.Maker())
	}
	d := New()
	if d.Name() != DefaultName || d.Provider() != DefaultProvider || d.Dialect() != toolschema.OpenAI || !d.Capabilities().ToolChoiceNone {
		t.Errorf("defaults: %s %s %s %+v", d.Name(), d.Provider(), d.Dialect(), d.Capabilities())
	}
}

func TestConcurrentCallsTakeSuccessiveSteps(t *testing.T) {
	const n = 32
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Reply("r")
	}
	a := New(steps...)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Call(context.Background(), ask("q")); err != nil {
				t.Error(err)
			}
			_ = a.Requests()
		}()
	}
	wg.Wait()
	if a.Remaining() != 0 || len(a.Requests()) != n || a.Err() != nil {
		t.Errorf("remaining %d, requests %d, err %v", a.Remaining(), len(a.Requests()), a.Err())
	}
}
