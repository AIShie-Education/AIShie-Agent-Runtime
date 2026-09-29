package bedrock

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const (
	claude     = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	nova       = "amazon.nova-pro-v1:0"
	testRegion = "us-east-1"
)

func ptr[T any](v T) *T { return &v }

// newTestAdapter is an adapter under static SigV4 credentials and a fixed
// clock, changed by opts.
func newTestAdapter(t *testing.T, opts ...func(*llm.Config)) *Adapter {
	t.Helper()
	cfg := llm.Config{Adapter: llm.AdapterBedrockConverse, Model: claude, Region: testRegion}
	for _, o := range opts {
		o(&cfg)
	}
	var a *Adapter
	var err error
	if cfg.APIKey != "" {
		a, err = New(cfg)
	} else {
		a, err = NewWithCredentials(cfg, credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", ""))
	}
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.now = func() time.Time { return time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC) }
	return a
}

// canonical is JSON with sorted keys and fixed indentation, numbers as
// written, so that goldens compare by meaning and diff readably.
func canonical(t *testing.T, b []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// golden compares got with testdata/golden/name, or writes it with -update.
func golden(t *testing.T, name string, got any) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	got2 := canonical(t, raw)
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got2, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(canonical(t, want), got2) {
		t.Errorf("%s differs from the golden file (run with -update after checking)\n--- got\n%s\n--- want\n%s", name, got2, want)
	}
}

// history is a turn of the loop: the question, the model's calls, Core's
// answers, as the worker builds it.
func history(maker string) []llm.Message {
	return []llm.Message{
		llm.UserText("Why did I lose marks on HW3?"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartReasoning, Maker: maker, Text: "I should look at the grade.",
				Opaque: json.RawMessage(`{"reasoningText":{"text":"I should look at the grade.","signature":"c2lnbmF0dXJl"}}`)},
			llm.Text("Let me check."),
			{Type: llm.PartToolCall, ID: "tooluse_kZJMlvQmRJ6eAyJE5GIl7Q", Name: "grade_list", Args: json.RawMessage(`{"assignment_id":"0192f3c1-0000-7000-8000-000000000003"}`)},
			{Type: llm.PartToolCall, ID: "call_2", Name: "assignment_get", Args: json.RawMessage(`{"assignment_id":"0192f3c1-0000-7000-8000-000000000003"}`)},
			{Type: llm.PartToolCall, ID: "toolu_01.fallback:7", Name: "submission_get", Args: json.RawMessage(`{}`), ArgsError: `{"submission_id":`},
			{Type: llm.PartToolCall, ID: "call_4", Name: "document_get", Args: json.RawMessage(`{"document_id":"0192f3c1-0000-7000-8000-000000000009"}`)},
		}},
		{Role: llm.RoleTool, Parts: []llm.Part{
			// json, success
			{Type: llm.PartToolResult, CallID: "tooluse_kZJMlvQmRJ6eAyJE5GIl7Q", Name: "grade_list",
				Content: `{"status":"executed","review_state":"none","result":{"grades":[{"points":"7.5","max_points":"10"}]}}`},
			// json, error: Core's envelope for a failed read
			{Type: llm.PartToolResult, CallID: "call_2", Name: "assignment_get", IsError: true,
				Content: `{"status":"error","error":{"code":"not_found","message":"no such assignment"}}`},
			// text, error: the runtime's own refusal of unparsable arguments
			{Type: llm.PartToolResult, CallID: "toolu_01.fallback:7", Name: "submission_get", IsError: true,
				Content: "The arguments are not a JSON object: unexpected end of JSON input"},
			// text, success: an envelope cut to size no longer parses
			{Type: llm.PartToolResult, CallID: "call_4", Name: "document_get",
				Content: `{"status":"executed","result":{"body_md":"Write a lexer…[truncated, 40000 bytes]`},
		}},
	}
}

// repeatedIDs is a loop in which another adapter numbered its calls afresh
// on each turn (Gemini's call_1) before a fallback to Bedrock.
func repeatedIDs() []llm.Message {
	call := func(id, name string) llm.Part {
		return llm.Part{Type: llm.PartToolCall, ID: id, Name: name, Args: json.RawMessage(`{}`)}
	}
	result := func(id, name, content string) llm.Part {
		return llm.Part{Type: llm.PartToolResult, CallID: id, Name: name, Content: content}
	}
	return []llm.Message{
		llm.UserText("What is due this week?"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{call("call_1", "course_get"), call("call_2", "assignment_list")}},
		{Role: llm.RoleTool, Parts: []llm.Part{
			result("call_1", "course_get", `{"status":"executed","result":{"title":"CS101"}}`),
			result("call_2", "assignment_list", `{"status":"executed","result":{"assignments":[]}}`),
		}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{call("call_1", "event_list"), call("call_1", "document_list")}},
		{Role: llm.RoleTool, Parts: []llm.Part{
			result("call_1", "event_list", `{"status":"executed","result":{"events":[]}}`),
			result("call_1", "document_list", `{"status":"executed","result":{"documents":[]}}`),
		}},
	}
}

var tools = []llm.Tool{
	{Name: "grade_list", Description: "List grades.", Schema: json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string","description":"(UUID)"}},"additionalProperties":false}`)},
	{Name: "assignment_get", Description: "Get an assignment.", Schema: json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string"}},"required":["assignment_id"]}`)},
	{Name: "course_get", Schema: nil},
}

func TestRequestGolden(t *testing.T) {
	pdf := &llm.File{Name: "HW3 feedback.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7 feedback")}
	pdfAgain := &llm.File{Name: "HW3_feedback.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7 another")}
	png := &llm.File{Name: "diagram.png", MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}
	notes := &llm.File{Name: "notes.md", MIME: "text/markdown; charset=utf-8", Data: []byte("# Notes\nLexers split text into tokens.")}
	zip := &llm.File{Name: "code.zip", MIME: "application/zip", Data: []byte("PK\x03\x04")}
	withFiles := func() []llm.Message {
		return []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("Here is my feedback."), {Type: llm.PartFile, File: pdf}}},
			{Role: llm.RoleAssistant, Parts: []llm.Part{
				{Type: llm.PartToolCall, ID: "tooluse_doc", Name: "document_get", Args: json.RawMessage(`{"document_id":"0192f3c1-0000-7000-8000-000000000009"}`)},
			}},
			{Role: llm.RoleTool, Parts: []llm.Part{
				{Type: llm.PartToolResult, CallID: "tooluse_doc", Name: "document_get", Content: `{"status":"executed","result":{"title":"HW3"}}`},
				{Type: llm.PartFile, File: pdfAgain},
				{Type: llm.PartFile, File: png},
				{Type: llm.PartFile, File: notes},
				{Type: llm.PartFile, File: zip},
			}},
		}
	}
	claudeMaker := llm.MakerOf(llm.AdapterBedrockConverse, "https://bedrock-runtime.us-east-1.amazonaws.com", claude)

	cases := []struct {
		name string
		opts []func(*llm.Config)
		req  llm.Request
	}{
		{
			name: "text",
			opts: []func(*llm.Config){func(c *llm.Config) {
				c.Params = llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3), TopP: ptr(0.9)}
			}},
			req: llm.Request{
				System: "You are CS101 Tutor.",
				Messages: []llm.Message{
					llm.UserText("[Earlier messages in this conversation are not shown.]"),
					{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("Lexing comes first."), llm.Text("   ")}},
					llm.UserText("And then?"),
				},
				ToolMode: llm.ToolAuto,
				Limits:   llm.Limits{MaxOutputTokens: 3000},
			},
		},
		{
			name: "tool_use_and_results",
			req:  llm.Request{System: "You are CS101 Tutor.", Messages: history("another|maker"), Tools: tools, ToolMode: llm.ToolAuto},
		},
		{
			name: "force_answer_flattens",
			req:  llm.Request{System: "You are CS101 Tutor.", Messages: history(claudeMaker), Tools: tools, ToolMode: llm.ToolNone, Limits: llm.Limits{MaxOutputTokens: 500}},
		},
		{
			name: "no_tools_flattens",
			req:  llm.Request{Messages: history("another|maker"), ToolMode: llm.ToolAuto},
		},
		{
			name: "documents",
			opts: []func(*llm.Config){func(c *llm.Config) { c.Capabilities.FileInput = ptr(true) }},
			req:  llm.Request{Messages: withFiles(), Tools: tools[:1], ToolMode: llm.ToolAuto},
		},
		{
			name: "documents_without_file_input",
			req:  llm.Request{Messages: withFiles(), Tools: tools[:1], ToolMode: llm.ToolAuto},
		},
		{
			name: "reasoning_replayed_to_its_maker",
			opts: []func(*llm.Config){func(c *llm.Config) {
				c.Reasoning.Effort = "medium"
				c.Params = llm.Params{MaxOutputTokens: 4000, Temperature: ptr(0.3)}
			}},
			req: llm.Request{Messages: history(claudeMaker), Tools: tools[:2], ToolMode: llm.ToolAuto},
		},
		{
			name: "reasoning_dropped_for_another_maker",
			opts: []func(*llm.Config){func(c *llm.Config) {
				c.Reasoning.Effort = "medium"
				c.Params = llm.Params{MaxOutputTokens: 4000, Temperature: ptr(0.3)}
			}},
			req: llm.Request{Messages: history(llm.MakerOf(llm.AdapterAnthropic, "", "claude-sonnet-4-5")), Tools: tools[:2], ToolMode: llm.ToolAuto},
		},
		{
			name: "thinking_on_first_turn",
			opts: []func(*llm.Config){func(c *llm.Config) {
				c.Reasoning.Effort = "low"
				c.Params = llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3), TopP: ptr(0.9)}
			}},
			req: llm.Request{System: "You are CS101 Tutor.", Messages: []llm.Message{llm.UserText("What is a lexer?")},
				Tools: tools[:1], ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 8000}},
		},
		{
			name: "repeated_call_ids",
			req:  llm.Request{Messages: repeatedIDs(), Tools: tools, ToolMode: llm.ToolAuto},
		},
		{
			name: "results_without_status_for_other_models",
			opts: []func(*llm.Config){func(c *llm.Config) { c.Model = "meta.llama3-3-70b-instruct-v1:0" }},
			req:  llm.Request{Messages: history("another|maker"), Tools: tools[:2], ToolMode: llm.ToolAuto},
		},
		{
			name: "strict_tools",
			opts: []func(*llm.Config){func(c *llm.Config) { c.Capabilities.StrictTools = ptr(true) }},
			req:  llm.Request{Messages: []llm.Message{llm.UserText("What is a lexer?")}, Tools: tools[:1], ToolMode: llm.ToolAuto},
		},
		{
			name: "adaptive_thinking_without_sampling",
			opts: []func(*llm.Config){func(c *llm.Config) {
				c.Model = "global.anthropic.claude-opus-4-7"
				c.Reasoning.Effort = "medium"
				c.Params = llm.Params{MaxOutputTokens: 4000, Temperature: ptr(0.3), TopP: ptr(0.9)}
			}},
			req: llm.Request{Messages: []llm.Message{llm.UserText("What is a lexer?")}, ToolMode: llm.ToolAuto},
		},
		{
			name: "reasoning_effort_ignored_for_other_models",
			opts: []func(*llm.Config){func(c *llm.Config) {
				c.Model = nova
				c.Reasoning.Effort = "high"
				c.Params = llm.Params{MaxOutputTokens: 4000, Temperature: ptr(0.3)}
			}},
			req: llm.Request{Messages: []llm.Message{llm.UserText("Hello")}, ToolMode: llm.ToolAuto},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t, tc.opts...)
			wire, err := a.translate(&tc.req)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			golden(t, "request/"+tc.name+".json", map[string]any{"request": tc.req, "wire": wire})
		})
	}
}

func TestResponseGolden(t *testing.T) {
	usage := `{"inputTokens":1234,"outputTokens":210,"totalTokens":1444}`
	cases := []struct {
		name, body string
	}{
		{"text", `{"output":{"message":{"role":"assistant","content":[{"text":"You lost 2.5 points on the lexer."},{"text":" See the rubric."}]}},
			"stopReason":"end_turn","usage":` + usage + `,"metrics":{"latencyMs":812}}`},
		{"tool_use", `{"output":{"message":{"role":"assistant","content":[
			{"text":"Let me check."},
			{"toolUse":{"toolUseId":"tooluse_kZJMlvQmRJ6eAyJE5GIl7Q","name":"grade_list","input":{"assignment_id":"0192f3c1-0000-7000-8000-000000000003"}}},
			{"toolUse":{"toolUseId":"tooluse_2","name":"assignment_get","input":{ "assignment_id" : "0192f3c1-0000-7000-8000-000000000003" }}}]}},
			"stopReason":"tool_use","usage":` + usage + `}`},
		{"tool_use_arguments_odd", `{"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"tooluse_str","name":"grade_list","input":"{\"assignment_id\":\"a\"}"}},
			{"toolUse":{"toolUseId":"tooluse_arr","name":"grade_list","input":["a"]}},
			{"toolUse":{"toolUseId":"","name":"course_get","input":{}}}]}},
			"stopReason":"tool_use","usage":` + usage + `}`},
		{"tool_use_under_end_turn", `{"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"tooluse_1","name":"course_get","input":{}}}]}},
			"stopReason":"end_turn","usage":` + usage + `}`},
		{"reasoning", `{"output":{"message":{"role":"assistant","content":[
			{"reasoningContent":{"reasoningText":{"text":"The rubric gives 2.5 for the lexer.","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}}},
			{"reasoningContent":{"redactedContent":"RXJyb3I6IGVuY3J5cHRlZA=="}},
			{"text":"You lost 2.5 points."}]}},
			"stopReason":"end_turn","usage":` + usage + `}`},
		{"unknown_blocks_dropped", `{"output":{"message":{"role":"assistant","content":[
			{"citationsContent":{"content":[{"text":"cited"}],"citations":[]}},{"text":"Answer."}]}},
			"stopReason":"end_turn","usage":` + usage + `}`},
		{"usage_cache_separate", `{"output":{"message":{"role":"assistant","content":[{"text":"Hi."}]}},"stopReason":"end_turn",
			"usage":{"inputTokens":22,"outputTokens":125,"totalTokens":12450,"cacheReadInputTokens":0,"cacheWriteInputTokens":12303}}`},
		{"usage_cache_read", `{"output":{"message":{"role":"assistant","content":[{"text":"Hi."}]}},"stopReason":"end_turn",
			"usage":{"inputTokens":4,"outputTokens":257,"totalTokens":16144,"cacheReadInputTokens":15883,"cacheWriteInputTokens":0}}`},
		{"usage_total_not_consulted", `{"output":{"message":{"role":"assistant","content":[{"text":"Hi."}]}},"stopReason":"end_turn",
			"usage":{"inputTokens":16000,"outputTokens":100,"totalTokens":16100,"cacheReadInputTokens":15000,"cacheWriteInputTokens":0}}`},
		{"usage_without_total", `{"output":{"message":{"role":"assistant","content":[{"text":"Hi."}]}},"stopReason":"end_turn",
			"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":20}}`},
		{"usage_missing", `{"output":{"message":{"role":"assistant","content":[{"text":"Hi."}]}},"stopReason":"end_turn"}`},
		{"no_message", `{"output":{},"stopReason":"guardrail_intervened","usage":` + usage + `}`},
		{"tool_use_null_input", `{"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"tooluse_n","name":"course_get","input":null}}]}},
			"stopReason":"tool_use","usage":` + usage + `}`},
		{"max_tokens_cuts_the_last_call", `{"output":{"message":{"role":"assistant","content":[
			{"text":"Let me check."},
			{"toolUse":{"toolUseId":"tooluse_whole","name":"course_get","input":{}}},
			{"toolUse":{"toolUseId":"tooluse_cut","name":"grade_list","input":{"assignment_id":"0192"}}}]}},
			"stopReason":"max_tokens","usage":` + usage + `}`},
		{"max_tokens_cuts_the_only_call", `{"output":{"message":{"role":"assistant","content":[
			{"text":"Let me check."},
			{"toolUse":{"toolUseId":"tooluse_cut","name":"grade_list","input":{"assignment_id":"0192"}}}]}},
			"stopReason":"max_tokens","usage":` + usage + `}`},
		{"context_window_cuts_the_last_call", `{"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"tooluse_cut","name":"grade_list","input":{}}}]}},
			"stopReason":"model_context_window_exceeded","usage":` + usage + `}`},
		{"malformed_tool_use_runs_no_call", `{"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"tooluse_bad","name":"grade_list","input":{"assignment_id":7}}}]}},
			"stopReason":"malformed_tool_use","usage":` + usage + `}`},
		{"guardrail_runs_no_call", `{"output":{"message":{"role":"assistant","content":[
			{"text":"Sorry, the model cannot answer this question."},
			{"toolUse":{"toolUseId":"tooluse_g","name":"course_get","input":{}}}]}},
			"stopReason":"guardrail_intervened","usage":` + usage + `}`},
		{"tool_use_without_a_call", `{"output":{"message":{"role":"assistant","content":[{"text":"Let me check."}]}},
			"stopReason":"tool_use","usage":` + usage + `}`},
	}
	for _, stop := range []string{
		"end_turn", "stop_sequence", "tool_use", "max_tokens", "guardrail_intervened", "content_filtered",
		"model_context_window_exceeded", "malformed_model_output", "malformed_tool_use", "a_reason_not_yet_known",
	} {
		content := `[{"text":"Partial answer"}]`
		if stop == "tool_use" {
			content = `[{"toolUse":{"toolUseId":"tooluse_1","name":"course_get","input":{}}}]`
		}
		cases = append(cases, struct{ name, body string }{"stop_" + stop,
			`{"output":{"message":{"role":"assistant","content":` + content + `}},"stopReason":"` + stop + `","usage":` + usage + `}`})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t)
			resp, err := a.parse([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			golden(t, "response/"+tc.name+".json", map[string]any{"wire": json.RawMessage(tc.body), "response": resp})
		})
	}
}
