package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// testKey is a made-up key; tests check that it never reaches an error.
const testKey = "sk-ant-api03-test-key-never-shown"

func newAdapter(t *testing.T, cfg llm.Config) *Adapter {
	t.Helper()
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-5"
	}
	if cfg.APIKey == "" {
		cfg.APIKey = testKey
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func ptr[T any](v T) *T { return &v }

var (
	gradeList = llm.Tool{
		Name:        "grade_list",
		Description: "List the grades of an assignment.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string","description":"the assignment (UUID)"}},"required":["assignment_id"],"additionalProperties":false}`),
	}
	assignmentGet = llm.Tool{
		Name:        "assignment_get",
		Description: "Read one assignment.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string"}},"required":["assignment_id"],"additionalProperties":false}`),
	}
	documentGet = llm.Tool{
		Name:        "document_get",
		Description: "Read one document.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"document_id":{"type":"string"}},"required":["document_id"],"additionalProperties":false}`),
	}

	question = llm.UserText("Why did I lose marks on HW3?")

	// A thinking block as Anthropic sends it, with characters that JSON
	// encoders like to escape.
	thinkingBlock = json.RawMessage(`{"type":"thinking","thinking":"The student asks about <HW3> & its marks.","signature":"EqQBCkgIARABGAIiQLu8m1Xz/signature+bytes=="}`)
	redactedBlock = json.RawMessage(`{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cr3qpPkNRj2YfWXGmKDxH4mPnZ5sQ7vB5URj"}`)

	gradesEnvelope     = `{"status":"executed","result":{"grades":[{"points":"7.5","max":"10"}]}}`
	assignmentEnvelope = `{"status":"executed","result":{"assignment":{"title":"HW3","rubric":"…"}}}`
)

func twoCalls(reasoning ...llm.Part) []llm.Message {
	assistant := append(reasoning,
		llm.Text("Let me check."),
		llm.Part{Type: llm.PartToolCall, ID: "toolu_01", Name: "grade_list", Args: json.RawMessage(`{"assignment_id":"0192f3c1-0000-7000-8000-000000000003"}`)},
		llm.Part{Type: llm.PartToolCall, ID: "toolu_02", Name: "assignment_get", Args: json.RawMessage(`{"assignment_id":"0192f3c1-0000-7000-8000-000000000003"}`)},
	)
	return []llm.Message{
		question,
		{Role: llm.RoleAssistant, Parts: assistant},
		{Role: llm.RoleTool, Parts: []llm.Part{
			{Type: llm.PartToolResult, CallID: "toolu_01", Name: "grade_list", Content: gradesEnvelope},
			{Type: llm.PartToolResult, CallID: "toolu_02", Name: "assignment_get", Content: assignmentEnvelope},
		}},
	}
}

func withFiles() []llm.Message {
	return []llm.Message{
		question,
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartToolCall, ID: "toolu_01", Name: "document_get", Args: json.RawMessage(`{"document_id":"0192f3c1-0000-7000-8000-000000000009"}`)},
		}},
		{Role: llm.RoleTool, Parts: []llm.Part{
			{Type: llm.PartToolResult, CallID: "toolu_01", Name: "document_get", Content: `{"status":"executed","result":{"document":{"title":"HW3 feedback"}}}`},
			{Type: llm.PartFile, File: &llm.File{Name: "feedback.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7 feedback")}},
			{Type: llm.PartFile, File: &llm.File{Name: "graph.png", MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}},
			{Type: llm.PartFile, File: &llm.File{Name: "notes.md", MIME: "text/markdown; charset=utf-8", Data: []byte("# Notes\nQuestion 2 lost 2.5 marks.")}},
			{Type: llm.PartFile, File: &llm.File{Name: "rubric.docx", MIME: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Data: []byte("PK\x03\x04")}},
		}},
		llm.UserText("The graph is the one from the lecture."),
	}
}

// TestRequestGolden holds the requests the adapter makes to golden files:
// testdata/golden/request_*.anthropic.json.
func TestRequestGolden(t *testing.T) {
	sonnet45 := llm.MakerOf(llm.AdapterAnthropic, DefaultBaseURL, "claude-sonnet-4-5")
	cases := []struct {
		name string
		cfg  llm.Config
		req  llm.Request
	}{
		{
			name: "plain_text",
			cfg:  llm.Config{Params: llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3)}},
			req:  llm.Request{System: "You are CS101's tutor.", Messages: []llm.Message{question}, ToolMode: llm.ToolAuto},
		},
		{
			name: "parallel_tools",
			req: llm.Request{
				System: "You are CS101's tutor.", Messages: twoCalls(), ToolMode: llm.ToolAuto,
				Tools: []llm.Tool{gradeList, assignmentGet}, Limits: llm.Limits{MaxOutputTokens: 2000},
			},
		},
		{
			// A call whose arguments did not parse is replayed with {}, a
			// Kimi id is made one Anthropic takes, and parallel calls are
			// off.
			name: "tool_errors",
			cfg:  llm.Config{Capabilities: llm.CapabilityOverrides{ParallelToolCalls: ptr(false)}},
			req: llm.Request{
				System: "You are CS101's tutor.", ToolMode: llm.ToolAuto, Tools: []llm.Tool{gradeList, assignmentGet},
				Messages: []llm.Message{
					question,
					{Role: llm.RoleAssistant, Parts: []llm.Part{
						{Type: llm.PartToolCall, ID: "toolu_01", Name: "grade_list", Args: json.RawMessage(`{}`), ArgsError: `{"assignment_id": `},
						{Type: llm.PartToolCall, ID: "functions.assignment_get:1", Name: "assignment_get", Args: json.RawMessage(`{"assignment_id":"hw3"}`)},
					}},
					{Role: llm.RoleTool, Parts: []llm.Part{
						{Type: llm.PartToolResult, CallID: "toolu_01", Name: "grade_list", Content: "the arguments are not a JSON object", IsError: true},
						{Type: llm.PartToolResult, CallID: "functions.assignment_get:1", Name: "assignment_get", IsError: true,
							Content: `{"status":"error","error":{"code":"invalid_argument","message":"assignment_id: not a UUID"}}`},
					}},
				},
			},
		},
		{
			name: "thinking_replayed",
			cfg:  llm.Config{Reasoning: llm.Reasoning{Effort: "medium"}, Params: llm.Params{Temperature: ptr(0.3)}},
			req: llm.Request{
				System: "You are CS101's tutor.", ToolMode: llm.ToolAuto, Tools: []llm.Tool{gradeList, assignmentGet},
				Limits: llm.Limits{MaxOutputTokens: 2000},
				Messages: twoCalls(
					llm.Part{Type: llm.PartReasoning, Text: "The student asks about <HW3> & its marks.", Maker: sonnet45, Opaque: thinkingBlock},
					llm.Part{Type: llm.PartReasoning, Maker: sonnet45, Opaque: redactedBlock},
				),
			},
		},
		{
			// The second round of a turn that began thinking: the model
			// thought only at its start, and the turn goes on thinking.
			name: "thinking_second_round",
			cfg:  llm.Config{Reasoning: llm.Reasoning{Effort: "medium"}, Params: llm.Params{Temperature: ptr(0.3)}},
			req: llm.Request{
				System: "You are CS101's tutor.", ToolMode: llm.ToolAuto, Tools: []llm.Tool{gradeList, assignmentGet},
				Limits: llm.Limits{MaxOutputTokens: 2000},
				Messages: append(twoCalls(llm.Part{Type: llm.PartReasoning, Text: "The student asks about <HW3> & its marks.", Maker: sonnet45, Opaque: thinkingBlock}),
					llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
						llm.Text("The rubric too."),
						{Type: llm.PartToolCall, ID: "toolu_03", Name: "assignment_get", Args: json.RawMessage(`{"assignment_id":"0192f3c1-0000-7000-8000-000000000004"}`)},
					}},
					llm.Message{Role: llm.RoleTool, Parts: []llm.Part{
						{Type: llm.PartToolResult, CallID: "toolu_03", Name: "assignment_get", Content: assignmentEnvelope},
					}},
				),
			},
		},
		{
			// Reasoning another model made is dropped; with a thinking
			// budget the API would then refuse thinking, so it is off for
			// this call and the configured temperature comes back.
			name: "thinking_other_maker",
			cfg:  llm.Config{Reasoning: llm.Reasoning{Effort: "medium"}, Params: llm.Params{Temperature: ptr(0.3)}},
			req: llm.Request{
				System: "You are CS101's tutor.", ToolMode: llm.ToolAuto, Tools: []llm.Tool{gradeList, assignmentGet},
				Limits: llm.Limits{MaxOutputTokens: 2000},
				Messages: twoCalls(
					llm.Part{Type: llm.PartReasoning, Maker: llm.MakerOf(llm.AdapterAnthropic, DefaultBaseURL, "claude-opus-4-1"), Opaque: thinkingBlock},
					llm.Part{Type: llm.PartReasoning, Maker: llm.MakerOf(llm.AdapterOpenAIResponses, "", "o4-mini"), Opaque: json.RawMessage(`{"type":"reasoning","encrypted_content":"gAAAA"}`)},
				),
			},
		},
		{
			name: "force_answer_tool_choice_none",
			req: llm.Request{
				System: "You are CS101's tutor.", Messages: twoCalls(), ToolMode: llm.ToolNone,
				Tools: []llm.Tool{gradeList, assignmentGet},
			},
		},
		{
			name: "force_answer_flattened",
			cfg:  llm.Config{Capabilities: llm.CapabilityOverrides{ToolChoiceNone: ptr(false)}},
			req: llm.Request{
				System: "You are CS101's tutor.", Messages: twoCalls(), ToolMode: llm.ToolNone,
				Tools: []llm.Tool{gradeList, assignmentGet},
			},
		},
		{
			name: "files",
			req:  llm.Request{System: "You are CS101's tutor.", Messages: withFiles(), ToolMode: llm.ToolAuto, Tools: []llm.Tool{documentGet}},
		},
		{
			name: "files_not_taken",
			cfg:  llm.Config{Capabilities: llm.CapabilityOverrides{FileInput: ptr(false)}},
			req:  llm.Request{System: "You are CS101's tutor.", Messages: withFiles(), ToolMode: llm.ToolAuto, Tools: []llm.Tool{documentGet}},
		},
		{
			name: "reasoning_budget",
			cfg: llm.Config{
				Model: "claude-haiku-4-5", Reasoning: llm.Reasoning{Effort: "high"},
				Params: llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3), TopP: ptr(0.9)},
			},
			req: llm.Request{System: "You are CS101's tutor.", Messages: []llm.Message{question}, ToolMode: llm.ToolAuto, Tools: []llm.Tool{gradeList}},
		},
		{
			name: "reasoning_adaptive",
			cfg: llm.Config{
				Model: "claude-opus-4-7", Reasoning: llm.Reasoning{Effort: "minimal"},
				Params: llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3)},
			},
			req: llm.Request{System: "You are CS101's tutor.", Messages: []llm.Message{question}, ToolMode: llm.ToolAuto, Tools: []llm.Tool{gradeList}},
		},
		{
			name: "reasoning_thinks_by_default",
			cfg:  llm.Config{Model: "claude-opus-5", Params: llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3)}},
			req:  llm.Request{System: "You are CS101's tutor.", Messages: []llm.Message{question}, ToolMode: llm.ToolAuto},
		},
		{
			// Blank text and empty messages go; neighbours of one role
			// merge; top_p goes when it is the only sampling setting.
			name: "merged_messages",
			cfg:  llm.Config{Params: llm.Params{TopP: ptr(0.9)}},
			req: llm.Request{
				ToolMode: llm.ToolAuto,
				Messages: []llm.Message{
					llm.UserText("Hello."),
					llm.UserText("  \n"),
					llm.UserText("Why did I lose marks on HW3?"),
					{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("")}},
					{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("Which question?")}},
					llm.UserText("Question 2."),
				},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAdapter(t, c.cfg)
			body, err := a.encodeRequest(&c.req, false)
			if err != nil {
				t.Fatalf("encodeRequest: %v", err)
			}
			checkGolden(t, "request_"+c.name+".anthropic.json", body)
		})
	}
}

// A replayed thinking block goes out as it came: its text is not escaped,
// and nothing in it changes.
func TestThinkingBlocksAreSentVerbatim(t *testing.T) {
	a := newAdapter(t, llm.Config{Reasoning: llm.Reasoning{Effort: "low"}})
	req := llm.Request{
		Messages: twoCalls(
			llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: thinkingBlock},
			llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: redactedBlock},
		),
		Tools: []llm.Tool{gradeList, assignmentGet},
	}
	body, err := a.encodeRequest(&req, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []json.RawMessage{thinkingBlock, redactedBlock} {
		if !bytes.Contains(body, b) {
			t.Errorf("the request does not carry %s verbatim:\n%s", b, body)
		}
	}
	if bytes.Contains(body, []byte("\\u003c")) || bytes.Contains(body, []byte("\\u0026")) {
		t.Errorf("the request escapes HTML characters:\n%s", body)
	}
}

// An answer thought and called tools; the next request carries its
// thinking back exactly, in place, with the tool calls after it.
func TestResponseRoundTrips(t *testing.T) {
	a := newAdapter(t, llm.Config{Reasoning: llm.Reasoning{Effort: "low"}})
	resp, err := a.decodeResponse(httpResponse(readGolden(t, "response_thinking.anthropic.json")))
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{question, {Role: llm.RoleAssistant, Parts: resp.Parts}}
	var results []llm.Part
	for _, c := range resp.ToolCalls() {
		results = append(results, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name, Content: gradesEnvelope})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleTool, Parts: results})
	w, err := a.buildRequest(&llm.Request{Messages: msgs, Tools: []llm.Tool{gradeList}})
	if err != nil {
		t.Fatal(err)
	}
	if w.Thinking == nil || w.Thinking.Type != "enabled" {
		t.Fatalf("thinking = %+v, want enabled: the last tool turn starts with a thinking block", w.Thinking)
	}
	sent := w.Messages[1].Content
	var in struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(readGolden(t, "response_thinking.anthropic.json"), &in); err != nil {
		t.Fatal(err)
	}
	if len(sent) != len(in.Content) {
		t.Fatalf("%d blocks sent back, want the %d that came", len(sent), len(in.Content))
	}
	for i, b := range sent {
		got, err := marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, got, in.Content[i]) {
			t.Errorf("block %d sent back as %s, want %s", i, got, in.Content[i])
		}
	}
}

func jsonEqual(t *testing.T, x, y []byte) bool {
	t.Helper()
	cx, err := canonical(x)
	if err != nil {
		t.Fatal(err)
	}
	cy, err := canonical(y)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(cx, cy)
}

func TestSampling(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		effort      string
		params      llm.Params
		temperature *float64
		topP        *float64
	}{
		{"temperature only", "claude-sonnet-4-5", "", llm.Params{Temperature: ptr(0.3)}, ptr(0.3), nil},
		{"top_p only", "claude-sonnet-4-5", "", llm.Params{TopP: ptr(0.9)}, nil, ptr(0.9)},
		{"both: temperature wins", "claude-sonnet-4-5", "", llm.Params{Temperature: ptr(0.3), TopP: ptr(0.9)}, ptr(0.3), nil},
		{"temperature over 1 is held to 1", "claude-sonnet-4-5", "", llm.Params{Temperature: ptr(1.4)}, ptr(1.0), nil},
		{"zero temperature is sent", "claude-sonnet-4-5", "", llm.Params{Temperature: ptr(0.0)}, ptr(0.0), nil},
		{"none while thinking", "claude-sonnet-4-5", "low", llm.Params{Temperature: ptr(0.3)}, nil, nil},
		{"4.6 takes sampling", "claude-sonnet-4-6", "", llm.Params{Temperature: ptr(0.3)}, ptr(0.3), nil},
		{"4.7 refuses sampling", "claude-opus-4-7", "", llm.Params{Temperature: ptr(0.3), TopP: ptr(0.9)}, nil, nil},
		{"5 refuses sampling", "claude-sonnet-5", "", llm.Params{TopP: ptr(0.9)}, nil, nil},
		{"another server takes both one at a time", "deepseek-chat", "", llm.Params{Temperature: ptr(0.7), TopP: ptr(0.9)}, ptr(0.7), nil},
		{"top_p over 1 is held to 1", "claude-sonnet-4-5", "", llm.Params{TopP: ptr(1.5)}, nil, ptr(1.0)},
		{"negative top_p is held to 0", "claude-sonnet-4-5", "", llm.Params{TopP: ptr(-0.1)}, nil, ptr(0.0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAdapter(t, llm.Config{Model: c.model, Params: c.params, Reasoning: llm.Reasoning{Effort: c.effort}})
			w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}})
			if err != nil {
				t.Fatal(err)
			}
			if !sameFloat(w.Temperature, c.temperature) || !sameFloat(w.TopP, c.topP) {
				t.Errorf("temperature %v, top_p %v; want %v, %v", show(w.Temperature), show(w.TopP), show(c.temperature), show(c.topP))
			}
		})
	}
}

func sameFloat(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }

func show(f *float64) any {
	if f == nil {
		return "absent"
	}
	return *f
}

func TestReasoning(t *testing.T) {
	cases := []struct {
		model     string
		effort    string
		thinking  *thinking
		effortOut string
		maxTokens int
	}{
		{"claude-sonnet-4-5", "", nil, "", 1000},
		{"claude-sonnet-4-5", "minimal", &thinking{Type: "enabled", BudgetTokens: 1024}, "", 2024},
		{"claude-sonnet-4-5", "low", &thinking{Type: "enabled", BudgetTokens: 1024}, "", 2024},
		{"claude-sonnet-4-5", "medium", &thinking{Type: "enabled", BudgetTokens: 4096}, "", 5096},
		{"claude-haiku-4-5", "high", &thinking{Type: "enabled", BudgetTokens: 16384}, "", 17384},
		{"deepseek-reasoner", "high", &thinking{Type: "enabled", BudgetTokens: 16384}, "", 17384},
		{"claude-sonnet-4-6", "medium", &thinking{Type: "adaptive"}, "medium", 5096},
		{"claude-opus-4-8", "high", &thinking{Type: "adaptive"}, "high", 17384},
		{"claude-opus-4-7", "", nil, "", 1000},
		{"claude-opus-5-5", "minimal", &thinking{Type: "adaptive"}, "low", 2024},
		{"claude-opus-5", "", nil, "", 17384},
		{"claude-fable-5-1", "", nil, "", 17384},
	}
	for _, c := range cases {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			a := newAdapter(t, llm.Config{Model: c.model, Reasoning: llm.Reasoning{Effort: c.effort}, Params: llm.Params{MaxOutputTokens: 1000}})
			w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}})
			if err != nil {
				t.Fatal(err)
			}
			if (w.Thinking == nil) != (c.thinking == nil) || (w.Thinking != nil && *w.Thinking != *c.thinking) {
				t.Errorf("thinking = %+v, want %+v", w.Thinking, c.thinking)
			}
			var effort string
			if w.OutputConfig != nil {
				effort = w.OutputConfig.Effort
			}
			if effort != c.effortOut {
				t.Errorf("output_config.effort = %q, want %q", effort, c.effortOut)
			}
			if w.MaxTokens != c.maxTokens {
				t.Errorf("max_tokens = %d, want %d", w.MaxTokens, c.maxTokens)
			}
		})
	}
}

// TestLeastReasoning: a call asking for the least reasoning (ForceAnswer's,
// a continuation's) thinks at low in place of medium or high, and a model
// that thinks unasked is asked at low, adaptively; one that does not think
// is asked nothing. DeepSeek's /anthropic, whose models think unasked, is
// sent its switch, thinking off, configured or not; an ordinary call to it
// is sent what it was before.
func TestLeastReasoning(t *testing.T) {
	const deepseek = "https://api.deepseek.com/anthropic"
	cases := []struct {
		model     string
		effort    string
		thinking  *thinking
		effortOut string
		maxTokens int
		base      string
	}{
		{"deepseek-flash", "", &thinking{Type: "disabled"}, "", 1000, deepseek},
		{"deepseek-v4-pro", "high", &thinking{Type: "disabled"}, "", 1000, deepseek},
		{"claude-opus-5", "", &thinking{Type: "disabled"}, "", 1000, deepseek},
		{"claude-sonnet-4-5", "medium", &thinking{Type: "enabled", BudgetTokens: 1024}, "", 2024, ""},
		{"claude-sonnet-4-5", "minimal", &thinking{Type: "enabled", BudgetTokens: 1024}, "", 2024, ""},
		{"claude-sonnet-4-5", "", nil, "", 1000, ""},
		{"claude-opus-4-8", "high", &thinking{Type: "adaptive"}, "low", 2024, ""},
		{"claude-opus-4-7", "", nil, "", 1000, ""},
		{"claude-opus-5", "", &thinking{Type: "adaptive"}, "low", 2024, ""},
		{"claude-fable-5-1", "high", &thinking{Type: "adaptive"}, "low", 2024, ""},
	}
	for _, c := range cases {
		t.Run(c.model+"/"+c.effort+"/"+c.base, func(t *testing.T) {
			a := newAdapter(t, llm.Config{Model: c.model, BaseURL: c.base, Reasoning: llm.Reasoning{Effort: c.effort}, Params: llm.Params{MaxOutputTokens: 1000}})
			if c.base == deepseek {
				ordinary, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}, ToolMode: llm.ToolNone})
				if err != nil {
					t.Fatal(err)
				}
				if ordinary.Thinking != nil && ordinary.Thinking.Type == "disabled" {
					t.Errorf("an ordinary call to DeepSeek is sent thinking %+v", ordinary.Thinking)
				}
			}
			w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}, ToolMode: llm.ToolNone, LeastReasoning: true})
			if err != nil {
				t.Fatal(err)
			}
			if (w.Thinking == nil) != (c.thinking == nil) || (w.Thinking != nil && *w.Thinking != *c.thinking) {
				t.Errorf("thinking = %+v, want %+v", w.Thinking, c.thinking)
			}
			var effort string
			if w.OutputConfig != nil {
				effort = w.OutputConfig.Effort
			}
			if effort != c.effortOut || w.MaxTokens != c.maxTokens {
				t.Errorf("output_config.effort = %q, max_tokens = %d; want %q, %d", effort, w.MaxTokens, c.effortOut, c.maxTokens)
			}
		})
	}
}

func TestMaxTokens(t *testing.T) {
	cases := []struct {
		name        string
		call, agent int
		want        int
	}{
		{"the call's cap", 2000, 1500, 2000},
		{"the agent's cap", 0, 1500, 1500},
		{"the default", 0, 0, DefaultMaxTokens},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAdapter(t, llm.Config{Params: llm.Params{MaxOutputTokens: c.agent}})
			w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}, Limits: llm.Limits{MaxOutputTokens: c.call}})
			if err != nil {
				t.Fatal(err)
			}
			if w.MaxTokens != c.want {
				t.Errorf("max_tokens = %d, want %d", w.MaxTokens, c.want)
			}
		})
	}
}

// With no tools to declare, a history that holds tool calls is flattened,
// since the API refuses tool blocks in a request without tools.
func TestNoToolsFlattensToolHistory(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	w, err := a.buildRequest(&llm.Request{Messages: twoCalls(), ToolMode: llm.ToolAuto})
	if err != nil {
		t.Fatal(err)
	}
	if w.Tools != nil || w.ToolChoice != nil {
		t.Errorf("tools %v, tool_choice %v; want neither", w.Tools, w.ToolChoice)
	}
	for _, m := range w.Messages {
		for _, b := range m.Content {
			if b.Type != "text" {
				t.Errorf("a %s block is left in a request without tools", b.Type)
			}
		}
	}
}

func TestFileLimits(t *testing.T) {
	big := &llm.File{Name: "scan.png", MIME: "image/png", Data: bytes.Repeat([]byte{0xff}, maxImageBytes)}
	a := newAdapter(t, llm.Config{})
	files := maxFileBytes
	if b := a.fileBlock(big, &files); b.Type != "text" || !strings.Contains(b.Text, "too large") {
		t.Errorf("an image over the API's limit became %s %q, want a note", b.Type, b.Text)
	}
	if files != maxFileBytes {
		t.Errorf("a file not shown took %d bytes of the request's allowance", maxFileBytes-files)
	}

	// Files are shown in order until the allowance is spent; a later one
	// that does not fit is a note, and an earlier one keeps its place.
	pdf := &llm.File{Name: "a.pdf", MIME: "application/pdf", Data: bytes.Repeat([]byte("x"), 3<<20)}
	files = 5 << 20
	if b := a.fileBlock(pdf, &files); b.Type != "document" {
		t.Fatalf("the first PDF became %s, want a document", b.Type)
	}
	if b := a.fileBlock(pdf, &files); b.Type != "text" {
		t.Errorf("a PDF past the allowance became %s, want a note", b.Type)
	}
	// Text is shown until the allowance is spent, as files are.
	text := &llm.File{Name: "notes.txt", MIME: "text/plain", Data: bytes.Repeat([]byte("a"), 64)}
	few := 63
	if b := a.fileBlock(text, &few); !strings.Contains(b.Text, "too large") || few != 63 {
		t.Errorf("a text file past the allowance became %q, leaving %d", b.Text, few)
	}
	few = 64
	if b := a.fileBlock(text, &few); !strings.HasPrefix(b.Text, `[file "notes.txt"]`) || few != 0 {
		t.Errorf("a text file within the allowance became %q, leaving %d", b.Text, few)
	}
	// Text that is not UTF-8 is not text.
	if b := a.fileBlock(&llm.File{Name: "x.txt", MIME: "text/plain", Data: []byte{0xff, 0xfe}}, &files); !strings.HasPrefix(b.Text, "[The file") {
		t.Errorf("a text file that is not UTF-8 became %q, want a note", b.Text)
	}
	// image/jpg is image/jpeg.
	if b := a.fileBlock(&llm.File{Name: "x.jpg", MIME: "image/jpg", Data: []byte("jpeg")}, &files); b.Source == nil || b.Source.MediaType != "image/jpeg" {
		t.Errorf("image/jpg became %+v, want an image/jpeg image", b)
	}
}

func TestWireID(t *testing.T) {
	for in, want := range map[string]string{
		"toolu_01A09q90qw90lq917835lq9": "toolu_01A09q90qw90lq917835lq9",
		"call_1":                        "call_1",
		"functions.grade_list:0":        "functions_grade_list_0",
		"tooluse-abc_DEF":               "tooluse-abc_DEF",
		"":                              "call",
	} {
		if got := wireID(in); got != want {
			t.Errorf("wireID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestsThatCannotBeMade(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	cases := []struct {
		name string
		req  *llm.Request
	}{
		{"no request", nil},
		{"no messages", &llm.Request{}},
		{"only blank text", &llm.Request{Messages: []llm.Message{llm.UserText(" ")}}},
		{"an unknown role", &llm.Request{Messages: []llm.Message{{Role: "system", Parts: []llm.Part{llm.Text("hi")}}}}},
		{"a schema that is not an object", &llm.Request{Messages: []llm.Message{question}, Tools: []llm.Tool{{Name: "x", Schema: json.RawMessage(`[1]`)}}}},
		{"an unknown tool mode", &llm.Request{Messages: []llm.Message{question}, Tools: []llm.Tool{gradeList}, ToolMode: "required"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := a.encodeRequest(c.req, false)
			var le *llm.Error
			if !errors.As(err, &le) || le.Kind != llm.ErrBadRequest {
				t.Fatalf("err = %v, want a bad_request *llm.Error", err)
			}
		})
	}
}

// Only a thinking or redacted_thinking block of this adapter's goes back;
// anything else carried as reasoning is dropped rather than sent.
func TestOnlyThinkingBlocksAreReplayed(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	for opaque, want := range map[string]string{
		string(thinkingBlock):             "thinking",
		string(redactedBlock):             "redacted_thinking",
		`{"type":"text","text":"sneaky"}`: "",
		`{"type":"thinking"`:              "",
		``:                                "",
	} {
		blocks := a.assistantBlocks([]llm.Part{{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(opaque)}})
		got := ""
		if len(blocks) == 1 {
			got = blocks[0].Type
		}
		if got != want || len(blocks) > 1 {
			t.Errorf("opaque %s became %d blocks (%q), want %q", opaque, len(blocks), got, want)
		}
	}
}

// A tool without a schema takes no arguments; the API still requires one.
func TestToolWithoutSchema(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}, Tools: []llm.Tool{{Name: "course_get"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(w.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("input_schema = %s", w.Tools[0].InputSchema)
	}
}

func TestMediaType(t *testing.T) {
	for in, want := range map[string]string{
		"application/pdf":             "application/pdf",
		"Application/PDF; name=x.pdf": "application/pdf",
		"image/jpg":                   "image/jpeg",
		"IMAGE/PNG ":                  "image/png",
		"not a media type;;":          "not a media type;;",
	} {
		if got := mediaType(in); got != want {
			t.Errorf("mediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

// A tool loop of several rounds is one assistant turn, and a model thinking
// with a budget thinks only at its start: the later rounds' messages carry
// no thinking block, and the turn keeps thinking because its first message
// did. A turn another model began goes without, to its end.
func TestThinkingLastsTheTurn(t *testing.T) {
	a := newAdapter(t, llm.Config{Reasoning: llm.Reasoning{Effort: "medium"}, Params: llm.Params{Temperature: ptr(0.3)}})
	call := func(id string) llm.Part {
		return llm.Part{Type: llm.PartToolCall, ID: id, Name: "grade_list", Args: json.RawMessage(`{"assignment_id":"hw3"}`)}
	}
	result := func(id string) llm.Message {
		return llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: id, Name: "grade_list", Content: gradesEnvelope}}}
	}
	ours := llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: thinkingBlock}
	theirs := llm.Part{Type: llm.PartReasoning, Maker: llm.MakerOf(llm.AdapterOpenAIChat, "https://api.deepseek.com", "deepseek-reasoner"), Opaque: thinkingBlock}
	cases := []struct {
		name  string
		msgs  []llm.Message
		think bool
	}{
		{"a question", []llm.Message{question}, true},
		{"the first round, thought", []llm.Message{
			question, {Role: llm.RoleAssistant, Parts: []llm.Part{ours, call("t1")}}, result("t1"),
		}, true},
		{"the third round of a turn that began thinking", []llm.Message{
			question,
			{Role: llm.RoleAssistant, Parts: []llm.Part{ours, call("t1")}}, result("t1"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("And the rubric."), call("t2")}}, result("t2"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{call("t3")}}, result("t3"),
		}, true},
		{"a turn another model began", []llm.Message{
			question, {Role: llm.RoleAssistant, Parts: []llm.Part{theirs, call("t1")}}, result("t1"),
		}, false},
		{"the second round of a turn another model began", []llm.Message{
			question,
			{Role: llm.RoleAssistant, Parts: []llm.Part{theirs, call("t1")}}, result("t1"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{call("t2")}}, result("t2"),
		}, false},
		{"a new question after a turn another model began", []llm.Message{
			question,
			{Role: llm.RoleAssistant, Parts: []llm.Part{theirs, call("t1")}}, result("t1"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("You lost 2.5 marks.")}},
			llm.UserText("And on HW4?"),
		}, true},
		{"a later turn that began thinking", []llm.Message{
			question,
			{Role: llm.RoleAssistant, Parts: []llm.Part{theirs, call("t1")}}, result("t1"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("You lost 2.5 marks.")}},
			llm.UserText("And on HW4?"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{ours, call("t2")}}, result("t2"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{call("t3")}}, result("t3"),
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, err := a.buildRequest(&llm.Request{Messages: c.msgs, Tools: []llm.Tool{gradeList}, ToolMode: llm.ToolAuto})
			if err != nil {
				t.Fatal(err)
			}
			if got := w.Thinking != nil; got != c.think {
				t.Errorf("thinking %+v, want on: %v", w.Thinking, c.think)
			}
			// Sampling comes back exactly when thinking is off.
			if got := w.Temperature != nil; got == c.think {
				t.Errorf("temperature %v with thinking on: %v", show(w.Temperature), c.think)
			}
		})
	}
}
