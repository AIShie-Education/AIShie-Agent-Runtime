package fakellm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/openaichat"
)

const key = "sk-fake-0123456789"

var assignmentList = llm.Tool{Name: "assignment_list", Description: "The course's assignments.",
	Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}

// adapter is the runtime's openai_chat adapter pointed at s.
func adapter(t *testing.T, s *fakellm.Server) *openaichat.Adapter {
	t.Helper()
	s.Start()
	t.Cleanup(s.Close)
	a, err := openaichat.New(llm.Config{Adapter: llm.AdapterOpenAIChat, BaseURL: s.URL(), Model: "fake-model", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDefaultResponderThroughTheAdapter(t *testing.T) {
	s := fakellm.New(fakellm.DefaultResponder)
	a := adapter(t, s)
	ctx := context.Background()
	req := &llm.Request{
		System:   "You are a tutor.",
		Messages: []llm.Message{llm.UserText("When is the next assignment due?")},
		Tools:    []llm.Tool{assignmentList}, ToolMode: llm.ToolAuto,
	}

	first, err := a.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	calls := first.ToolCalls()
	if first.Stop != llm.StopToolCalls || len(calls) != 1 || calls[0].Name != "assignment_list" || string(calls[0].Args) != "{}" {
		t.Fatalf("first turn = %+v", first)
	}
	if first.Usage.Estimated || first.Usage.Input == 0 || first.Usage.Output == 0 {
		t.Errorf("usage = %+v", first.Usage)
	}

	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, Parts: first.Parts},
		llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: calls[0].ID, Name: "assignment_list",
			Content: `{"status":"executed","result":{"assignments":[{"title":"HW3","due_at":"2026-10-02"}]}}`}}})
	second, err := a.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Answer: When is the next assignment due? (from 1 tool result)"; second.Stop != llm.StopEnd || second.Text() != want {
		t.Errorf("second turn = %q (%s), want %q", second.Text(), second.Stop, want)
	}

	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if reqs[0].Header.Get("Authorization") != "Bearer "+key || reqs[0].Model != "fake-model" || len(reqs[0].Tools) != 1 {
		t.Errorf("first request = %+v", reqs[0])
	}
	if got := reqs[1].Messages; len(got) != 4 || got[2].ToolCalls[0].ID != calls[0].ID || got[3].ToolCallID != calls[0].ID {
		t.Errorf("second request's messages = %+v", got)
	}
}

func TestDefaultResponderAnswersWithoutTools(t *testing.T) {
	long := strings.Repeat("é", 250)
	for _, c := range []struct {
		name string
		req  *llm.Request
		want string
	}{
		{"no assignment in the question", &llm.Request{Messages: []llm.Message{llm.UserText("Why did I lose marks?")}, Tools: []llm.Tool{assignmentList}},
			"Answer: Why did I lose marks?"},
		{"no tools", &llm.Request{Messages: []llm.Message{llm.UserText("Which assignment is next?")}},
			"Answer: Which assignment is next?"},
		{"a long question is cut", &llm.Request{Messages: []llm.Message{llm.UserText(long)}},
			"Answer: " + strings.Repeat("é", 200)},
		// ForceAnswer against a server of unknown capabilities: the adapter
		// leaves the tools out and gives the history as text, and the
		// question is still the asker's, not the results'.
		{"a forced answer", &llm.Request{
			Messages: []llm.Message{
				llm.UserText("Which assignment is next?"),
				{Role: llm.RoleAssistant, Parts: []llm.Part{
					{Type: llm.PartToolCall, ID: "c1", Name: "assignment_list", Args: json.RawMessage(`{}`)},
					{Type: llm.PartToolCall, ID: "c2", Name: "document_get", Args: json.RawMessage(`{"document_id":"d1"}`)}}},
				{Role: llm.RoleTool, Parts: []llm.Part{
					{Type: llm.PartToolResult, CallID: "c1", Name: "assignment_list", Content: `{"status":"executed"}`},
					{Type: llm.PartToolResult, CallID: "c2", Name: "document_get", Content: `{"status":"error"}`, IsError: true}}},
			},
			Tools: []llm.Tool{assignmentList}, ToolMode: llm.ToolNone},
			"Answer: Which assignment is next? (from 2 tool results)"},
		// Files that came with the results follow them as a user message.
		{"files after the results", &llm.Request{
			Messages: []llm.Message{
				llm.UserText("What does the HW3 handout say?"),
				{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ID: "c1", Name: "assignment_list", Args: json.RawMessage(`{}`)}}},
				{Role: llm.RoleTool, Parts: []llm.Part{
					{Type: llm.PartToolResult, CallID: "c1", Name: "assignment_list", Content: `{"status":"executed"}`},
					{Type: llm.PartFile, File: &llm.File{Name: "hw3.md", MIME: "text/markdown", Data: []byte("# HW3")}}}},
			},
			Tools: []llm.Tool{assignmentList}, ToolMode: llm.ToolAuto},
			"Answer: What does the HW3 handout say? (from 1 tool result)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := fakellm.New(fakellm.DefaultResponder)
			resp, err := adapter(t, s).Call(context.Background(), c.req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Stop != llm.StopEnd || resp.Text() != c.want {
				t.Errorf("got %q (%s), want %q", resp.Text(), resp.Stop, c.want)
			}
			if c.req.ToolMode == llm.ToolNone {
				r := s.Requests()[0]
				if len(r.Tools) != 0 || len(r.ToolChoice) != 0 {
					t.Errorf("a forced answer declared tools: %s", r.Raw)
				}
			}
		})
	}
}

// TestDefaultResponderKnowsAnAssignment holds which questions make the
// default model look the assignments up.
func TestDefaultResponderKnowsAnAssignment(t *testing.T) {
	for q, calls := range map[string]bool{
		"Why did I lose marks on HW3?":       true,
		"when is hw 2 due":                   true,
		"Is the homework graded yet?":        true,
		"Which Assignments are left?":        true,
		"How do I write a for loop?":         false,
		"What does the word 'shwa' mean?":    false,
		"Is the show on Friday? (HWY 3 map)": false,
	} {
		resp := fakellm.DefaultResponder(fakellm.ChatRequest{Model: "m",
			Messages: []fakellm.ChatMessage{{Role: "user", Content: q}},
			Tools:    []fakellm.Tool{{Type: "function", Function: fakellm.ToolFunction{Name: "assignment_list"}}}})
		if got := len(resp.Choices[0].Message.ToolCalls) == 1; got != calls {
			t.Errorf("%q: calls assignment_list = %v, want %v", q, got, calls)
		}
	}
}

func TestScriptThroughTheAdapter(t *testing.T) {
	s := fakellm.NewScript(
		fakellm.ChatResponse{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"3"}},
			Error: &fakellm.Error{Message: "slow down", Type: "requests", Code: "rate_limit_exceeded"}},
		fakellm.Failure(http.StatusBadRequest, "context_length_exceeded", "This model's maximum context length is 8192 tokens."),
		fakellm.CallTools(fakellm.FunctionCall{Name: "grade_list", Arguments: `{"assignment_id":"hw3"}`},
			fakellm.FunctionCall{Name: "assignment_get", Arguments: `{"assignment_id":"hw3"`}),
		fakellm.Reply("Done."),
		fakellm.ChatResponse{Choices: []fakellm.Choice{{Message: fakellm.ChatMessage{Content: "No usage."}, FinishReason: "length"}}},
	)
	a := adapter(t, s)
	ctx := context.Background()
	ask := &llm.Request{Messages: []llm.Message{llm.UserText("q")}}

	var e *llm.Error
	if _, err := a.Call(ctx, ask); !errors.As(err, &e) || e.Kind != llm.ErrRateLimited || e.RetryAfter != 3*time.Second {
		t.Errorf("429: %v", err)
	}
	if _, err := a.Call(ctx, ask); !errors.As(err, &e) || e.Kind != llm.ErrContextOverflow {
		t.Errorf("400: %v", err)
	}
	resp, err := a.Call(ctx, ask)
	if err != nil || len(resp.ToolCalls()) != 2 || resp.ToolCalls()[1].ArgsError == "" || resp.ToolCalls()[0].ID != "call_1" {
		t.Errorf("tool calls: %+v, %v", resp, err)
	}
	if resp, err := a.Call(ctx, ask); err != nil || resp.Text() != "Done." || resp.Stop != llm.StopEnd {
		t.Errorf("reply: %+v, %v", resp, err)
	}
	if resp, err := a.Call(ctx, ask); err != nil || resp.Stop != llm.StopMaxTokens || !resp.Usage.Estimated {
		t.Errorf("no usage: %+v, %v", resp, err)
	}
	if s.Remaining() != 0 {
		t.Errorf("%d left", s.Remaining())
	}
	if _, err := a.Call(ctx, ask); !errors.As(err, &e) || e.Kind != llm.ErrServer {
		t.Errorf("past the script: %v", err)
	}
}

func TestScriptBeforeResponder(t *testing.T) {
	s := fakellm.New(fakellm.DefaultResponder)
	s.Enqueue(fakellm.Reply("scripted"))
	a := adapter(t, s)
	ask := &llm.Request{Messages: []llm.Message{llm.UserText("q")}}
	for _, want := range []string{"scripted", "Answer: q"} {
		resp, err := a.Call(context.Background(), ask)
		if err != nil || resp.Text() != want {
			t.Errorf("got %v %v, want %q", resp, err, want)
		}
	}
}

func TestADelayedAnswerMeetsTheCallersTimeout(t *testing.T) {
	s := fakellm.NewScript(fakellm.ChatResponse{Delay: time.Minute, Choices: []fakellm.Choice{{Message: fakellm.ChatMessage{Content: "late"}}}})
	a := adapter(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := a.Call(ctx, &llm.Request{Messages: []llm.Message{llm.UserText("q")}})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrTimeout {
		t.Errorf("err = %v", err)
	}
}

func TestMalformedConversationsAreRefused(t *testing.T) {
	s := fakellm.New(fakellm.DefaultResponder)
	a := adapter(t, s)
	// A result with no call before it: OpenAI refuses this, and so does
	// the fake.
	_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{
		llm.UserText("q"),
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "nobody", Content: "{}"}}},
	}, Tools: []llm.Tool{assignmentList}})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrBadRequest || !strings.Contains(e.Message, "tool message") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	user := fakellm.ChatMessage{Role: "user", Content: "q"}
	call := fakellm.ChatMessage{Role: "assistant", ToolCalls: []fakellm.ToolCall{{ID: "c1", Type: "function"}, {ID: "c2", Type: "function"}}}
	result := func(id string) fakellm.ChatMessage {
		return fakellm.ChatMessage{Role: "tool", ToolCallID: id, Content: "{}"}
	}
	tool := fakellm.Tool{Type: "function"}
	for _, c := range []struct {
		name string
		req  fakellm.ChatRequest
		ok   bool
	}{
		{"fine", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user, call, result("c1"), result("c2"), user}}, true},
		{"no model", fakellm.ChatRequest{Messages: []fakellm.ChatMessage{user}}, false},
		{"no messages", fakellm.ChatRequest{Model: "m"}, false},
		{"stream", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user}, Stream: true}, false},
		{"tool_choice without tools", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user}, ToolChoice: json.RawMessage(`"none"`)}, false},
		{"parallel_tool_calls without tools", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user}, ParallelToolCalls: new(bool)}, false},
		{"parallel_tool_calls with tools", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user}, ParallelToolCalls: new(bool), Tools: []fakellm.Tool{tool}}, true},
		{"tool_choice with tools", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user}, ToolChoice: json.RawMessage(`"none"`), Tools: []fakellm.Tool{tool}}, true},
		{"a call unanswered", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user, call, result("c1"), user}}, false},
		{"a call unanswered at the end", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user, call, result("c1")}}, false},
		{"an answer twice", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{user, call, result("c1"), result("c1")}}, false},
		{"an unknown role", fakellm.ChatRequest{Model: "m", Messages: []fakellm.ChatMessage{{Role: "function"}}}, false},
	} {
		if got := fakellm.Validate(c.req); (got == "") != c.ok {
			t.Errorf("%s: Validate = %q", c.name, got)
		}
	}
}

func TestOtherEndpoints(t *testing.T) {
	s := fakellm.New(fakellm.DefaultResponder).Start()
	defer s.Close()
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/v1/models", http.StatusOK},
		{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/embeddings", http.StatusNotFound},
		{http.MethodPost, "/v1/chat/completions", http.StatusBadRequest}, // an empty body
	} {
		req, _ := http.NewRequest(c.method, strings.TrimSuffix(s.URL(), "/v1")+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || !json.Valid(body) {
			t.Errorf("%s %s = %d %s", c.method, c.path, resp.StatusCode, body)
		}
	}
	if s := fakellm.New(nil); s.URL() != "" {
		t.Errorf("an unstarted server has URL %q", s.URL())
	}
}

func TestConcurrentRequests(t *testing.T) {
	s := fakellm.New(fakellm.DefaultResponder)
	a := adapter(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("q")}}); err != nil {
				t.Error(err)
			}
			_ = s.Requests()
		}()
	}
	wg.Wait()
	if n := len(s.Requests()); n != 16 {
		t.Errorf("%d requests", n)
	}
}
