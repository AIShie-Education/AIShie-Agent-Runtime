package bedrock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

var toolUseIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestToolUseID(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		id   string
		kept bool
	}{
		{"tooluse_kZJMlvQmRJ6eAyJE5GIl7Q", true},
		{"call_1", true},
		{"toolu_01A09q90qw90lq917835lq9", true},
		{strings.Repeat("a", 64), true},
		{long, false},
		{"fc_67.abc", false},
		{"call:1", false},
		{"", false},
		{"ключ", false},
	}
	for _, tc := range cases {
		got := toolUseID(tc.id)
		if (got == tc.id) != tc.kept {
			t.Errorf("toolUseID(%q) = %q, kept %v", tc.id, got, got == tc.id)
		}
		if !toolUseIDPattern.MatchString(got) {
			t.Errorf("toolUseID(%q) = %q, not a valid toolUseId", tc.id, got)
		}
		if again := toolUseID(tc.id); again != got {
			t.Errorf("toolUseID(%q) is not deterministic: %q, %q", tc.id, got, again)
		}
	}
	if toolUseID("fc_67.abc") == toolUseID("fc_67:abc") {
		t.Error("two ids map to one")
	}
}

func TestDocumentNames(t *testing.T) {
	st := newFileState()
	cases := []struct{ in, want string }{
		{"HW3 feedback.pdf", "HW3 feedback"},
		{"HW3_feedback.pdf", "HW3 feedback (2)"},
		{"  report   (final) [v2].docx ", "report (final) [v2]"},
		{"notes.v2.md", "notes v2"},
		{"課題.pdf", "document"},
		{"課題2.pdf", "2"},
		{"課題.docx", "document (2)"},
		{".pdf", "pdf"},
		{"", "document (3)"},
		{"../../etc/passwd", "etc passwd"},
		{strings.Repeat("x", 150) + ".pdf", strings.Repeat("x", maxDocumentName)},
	}
	for _, tc := range cases {
		if got := st.uniqueName(tc.in); got != tc.want {
			t.Errorf("uniqueName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFileBlocksKeepWithinConverseLimits(t *testing.T) {
	a := newTestAdapter(t, func(c *llm.Config) { c.Capabilities.FileInput = ptr(true) })
	st := newFileState()
	pdf := func(name string, n int) *llm.File {
		return &llm.File{Name: name, MIME: "application/pdf", Data: bytes.Repeat([]byte("x"), n)}
	}
	kinds := func(bs []block) string {
		var s []string
		for _, b := range bs {
			switch {
			case b.Document != nil:
				s = append(s, "document:"+b.Document.Format)
			case b.Image != nil:
				s = append(s, "image:"+b.Image.Format)
			default:
				s = append(s, "text")
			}
		}
		return strings.Join(s, ",")
	}
	cases := []struct {
		name string
		file *llm.File
		want string
		text string
	}{
		{"a document", pdf("a.pdf", 10), "text,document:pdf", `as the document "a"`},
		{"a document too large", pdf("big.pdf", maxDocumentBytes+1), "text", "a document may be at most 4.5 MB"},
		{"a text document too large goes as text", &llm.File{Name: "big.txt", MIME: "text/plain", Data: bytes.Repeat([]byte("y"), maxDocumentBytes+1)}, "text", `[The file "big.txt" follows.]`},
		{"an image", &llm.File{Name: "p.jpg", MIME: "image/jpeg", Data: []byte("jpg")}, "text,image:jpeg", `[The file "p.jpg" follows.]`},
		{"an image too large", &llm.File{Name: "huge.png", MIME: "image/png", Data: bytes.Repeat([]byte("z"), maxImageBytes+1)}, "text", "an image may be at most 3.75 MB"},
		{"a format by extension", &llm.File{Name: "sheet.xlsx", MIME: "application/octet-stream", Data: []byte("PK")}, "text,document:xlsx", `as the document "sheet"`},
		{"JSON as text", &llm.File{Name: "data.json", MIME: "application/json", Data: []byte(`{"a":1}`)}, "text", `{"a":1}`},
		{"an empty file", &llm.File{Name: "empty.pdf", MIME: "application/pdf"}, "text", "it is empty"},
		{"an unknown type", &llm.File{Name: "x.bin", MIME: "", Data: []byte{0, 1}}, "text", "of unknown type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := a.fileBlocks(tc.file, st)
			if kinds(got) != tc.want || !strings.Contains(got[0].Text, tc.text) {
				t.Errorf("fileBlocks = %s %q, want %s containing %q", kinds(got), got[0].Text, tc.want, tc.text)
			}
		})
	}

	// Five documents at most, and twenty images: the rest are described.
	st = newFileState()
	for i := range maxDocuments + 1 {
		got := a.fileBlocks(pdf("d.pdf", 1), st)
		if want := i < maxDocuments; (kinds(got) == "text,document:pdf") != want {
			t.Errorf("document %d: %s", i+1, kinds(got))
		}
	}
	for i := range maxImages + 1 {
		got := a.fileBlocks(&llm.File{Name: "i.png", MIME: "image/png", Data: []byte("p")}, st)
		if want := i < maxImages; (kinds(got) == "text,image:png") != want {
			t.Errorf("image %d: %s", i+1, kinds(got))
		}
	}
}

func TestReasoning(t *testing.T) {
	withTool := []message{{Role: "user"}, {Role: "assistant", Content: []block{{Text: "x"}, {ToolUse: &toolUseBlock{ToolUseID: "t"}}}}, {Role: "user"}}
	withReasonedTool := []message{{Role: "user"}, {Role: "assistant", Content: []block{{ReasoningContent: json.RawMessage(`{}`)}, {ToolUse: &toolUseBlock{ToolUseID: "t"}}}}, {Role: "user"}}
	plain := []message{{Role: "user"}, {Role: "assistant", Content: []block{{Text: "x"}}}, {Role: "user"}}
	budget := func(n int) string { return fmt.Sprintf(`{"thinking":{"type":"enabled","budget_tokens":%d}}`, n) }
	adaptive := func(effort string) string {
		return `{"thinking":{"type":"adaptive"},"output_config":{"effort":"` + effort + `"}}`
	}
	cases := []struct {
		name, model, effort string
		maxTokens           int
		msgs                []message
		want                string // "" for nothing
	}{
		{"no effort", claude, "", 4000, plain, ""},
		{"unknown effort", claude, "extreme", 4000, plain, ""},
		{"not Anthropic", nova, "high", 40000, plain, ""},
		{"an application inference profile", "arn:aws:bedrock:us-east-1:1:application-inference-profile/a1b2c3", "high", 40000, plain, ""},
		{"a Claude from before thinking", "anthropic.claude-3-5-sonnet-20241022-v2:0", "high", 40000, plain, ""},
		{"Claude 3 Haiku", "us.anthropic.claude-3-haiku-20240307-v1:0", "low", 40000, plain, ""},
		{"no cap", claude, "low", 0, plain, ""},
		{"cap too small", claude, "low", 2000, plain, ""},
		{"minimal", claude, "minimal", 4000, plain, budget(1024)},
		{"low under a cap", claude, "low", 3000, plain, budget(1500)},
		{"Claude 3.7", "us.anthropic.claude-3-7-sonnet-20250219-v1:0", "medium", 32000, plain, budget(8192)},
		{"high", "anthropic.claude-opus-4-1-20250805-v1:0", "high", 64000, plain, budget(16384)},
		{"an ARN", "arn:aws:bedrock:us-east-1:1:inference-profile/global.anthropic.claude-haiku-4-5-20251001-v1:0", "medium", 32000, plain, budget(8192)},
		{"a tool turn without its thinking", claude, "medium", 32000, withTool, ""},
		{"a tool turn with its thinking", claude, "medium", 32000, withReasonedTool, budget(8192)},
		{"4.6 thinks adaptively", "global.anthropic.claude-opus-4-6-v1", "medium", 4000, plain, adaptive("medium")},
		{"4.7, minimal", "anthropic.claude-opus-4-7", "minimal", 0, plain, adaptive("low")},
		{"5, with no cap", "us.anthropic.claude-sonnet-5-20260801-v1:0", "high", 0, plain, adaptive("high")},
		{"adaptive, a tool turn without its thinking", "anthropic.claude-opus-4-7", "high", 32000, withTool, ""},
	}
	// A call asking for the least reasoning (ForceAnswer's) is made at
	// low in place of medium or high; with nothing configured, a Claude
	// that thinks unasked (from Opus 5) is asked at low, adaptively, and
	// any other is asked nothing still.
	least := []struct {
		name, model, effort string
		maxTokens           int
		msgs                []message
		want                string
	}{
		{"least: high is low", "anthropic.claude-opus-4-1-20250805-v1:0", "high", 64000, plain, budget(2048)},
		{"least: adaptive medium is low", "global.anthropic.claude-opus-4-6-v1", "medium", 4000, plain, adaptive("low")},
		{"least: minimal stays", claude, "minimal", 4000, plain, budget(1024)},
		{"least: no effort", claude, "", 4000, plain, ""},
		{"least: opus 5, no effort", "us.anthropic.claude-opus-5-20260901-v1:0", "", 4000, plain, adaptive("low")},
		{"least: sonnet 5, no effort, no cap", "us.anthropic.claude-sonnet-5-20260801-v1:0", "", 0, plain, adaptive("low")},
		{"least: opus 4.7, no effort", "anthropic.claude-opus-4-7", "", 4000, plain, ""},
		{"least: opus 5, a tool turn without its thinking", "global.anthropic.claude-opus-5-v1", "", 4000, withTool, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkReasoning(t, tc.model, tc.effort, tc.maxTokens, tc.msgs, false, tc.want)
		})
	}
	for _, tc := range least {
		t.Run(tc.name, func(t *testing.T) {
			checkReasoning(t, tc.model, tc.effort, tc.maxTokens, tc.msgs, true, tc.want)
		})
	}
}

// checkReasoning holds the reasoning asked of model at effort to want
// ("" for nothing).
func checkReasoning(t *testing.T, model, effort string, maxTokens int, msgs []message, least bool, want string) {
	t.Helper()
	a := newTestAdapter(t, func(c *llm.Config) { c.Model = model; c.Reasoning.Effort = effort })
	got := a.reasoning(maxTokens, msgs, least)
	if want == "" {
		if got != nil {
			b, _ := json.Marshal(got)
			t.Errorf("reasoning = %s, want nothing", b)
		}
		return
	}
	b, err := json.Marshal(got)
	if err != nil || string(b) != want {
		t.Errorf("reasoning = %s, want %s", b, want)
	}
}

// Temperature and top_p go to a model unless it thinks on this call or
// refuses them always.
func TestSamplingIsLeftOutWhereClaudeRefusesIt(t *testing.T) {
	cases := []struct {
		name, model, effort string
		want                bool
	}{
		{"Nova", nova, "high", true},
		{"Claude 4.5 without thinking", claude, "", true},
		{"Claude 4.5 thinking", claude, "medium", false},
		{"Claude 4.7, which refuses them", "global.anthropic.claude-opus-4-7", "", false},
		{"Claude 5", "anthropic.claude-sonnet-5-v1:0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t, func(c *llm.Config) {
				c.Model, c.Reasoning.Effort = tc.model, tc.effort
				c.Params = llm.Params{MaxOutputTokens: 8000, Temperature: ptr(0.3), TopP: ptr(0.9)}
			})
			wire, err := a.translate(&llm.Request{Messages: []llm.Message{llm.UserText("Hi")}})
			if err != nil {
				t.Fatal(err)
			}
			ic := wire.InferenceConfig
			if got := ic.Temperature != nil && ic.TopP != nil; got != tc.want || ic.MaxTokens != 8000 {
				t.Errorf("inferenceConfig = %+v, sampling sent %v, want %v", ic, got, tc.want)
			}
		})
	}
}

func TestCallIDs(t *testing.T) {
	ids := newCallIDs()
	ids.newTurn()
	first := []string{ids.call("call_1"), ids.call("fc_1.x"), ids.call("tooluse_A")}
	ids.newTurn()
	second := []string{ids.call("call_1"), ids.call("call_1"), ids.call("tooluse_A")}
	all := append(append([]string{}, first...), second...)
	seen := map[string]bool{}
	for _, id := range all {
		if seen[id] || !toolUseIDPattern.MatchString(id) {
			t.Errorf("ids %v: %q repeats or is not a toolUseId", all, id)
		}
		seen[id] = true
	}
	if first[0] != "call_1" || first[2] != "tooluse_A" {
		t.Errorf("valid ids were not kept: %v", first)
	}
	// Results answer the last turn, in order among calls sharing an id.
	for i, id := range []string{"call_1", "call_1", "tooluse_A"} {
		if got := ids.result(id); got != second[i] {
			t.Errorf("result %d (%s) = %s, want %s", i, id, got, second[i])
		}
	}
	// A result with no call before it is mapped as a call would be.
	if got := ids.result("orphan:1"); got != toolUseID("orphan:1") {
		t.Errorf("an orphan result = %s", got)
	}
	// The same history maps the same way every time.
	again := newCallIDs()
	again.newTurn()
	again.call("call_1")
	again.call("fc_1.x")
	again.call("tooluse_A")
	again.newTurn()
	if got := again.call("call_1"); got != second[0] {
		t.Errorf("not deterministic: %s, then %s", second[0], got)
	}
}

func TestFamilyOf(t *testing.T) {
	cases := []struct {
		model string
		want  family
	}{
		{"anthropic.claude-3-haiku-20240307-v1:0", family{anthropic: true, toolStatus: true}},
		{"anthropic.claude-v2:1", family{anthropic: true, toolStatus: true}},
		{"us.anthropic.claude-3-7-sonnet-20250219-v1:0", family{anthropic: true, thinks: true, toolStatus: true}},
		{claude, family{anthropic: true, thinks: true, toolStatus: true}},
		{"anthropic.claude-sonnet-4-20250514-v1:0", family{anthropic: true, thinks: true, toolStatus: true}},
		{"global.anthropic.claude-opus-4-6-v1", family{anthropic: true, thinks: true, adaptive: true, toolStatus: true}},
		{"arn:aws:bedrock:us-east-1:1:inference-profile/us.anthropic.claude-opus-4-7", family{anthropic: true, thinks: true, adaptive: true, noSampling: true, toolStatus: true}},
		{"anthropic.claude-fable-1-v1:0", family{anthropic: true, thinks: true, adaptive: true, thinksByDefault: true, noSampling: true, toolStatus: true}},
		{"us.anthropic.claude-opus-5-5-v1:0", family{anthropic: true, thinks: true, adaptive: true, thinksByDefault: true, noSampling: true, toolStatus: true}},
		{nova, family{toolStatus: true}},
		{"us.amazon.nova-lite-v1:0", family{toolStatus: true}},
		{"meta.llama3-3-70b-instruct-v1:0", family{}},
		{"mistral.mistral-large-2407-v1:0", family{}},
		{"arn:aws:bedrock:us-east-1:1:application-inference-profile/a1b2c3", family{}},
	}
	for _, tc := range cases {
		if got := familyOf(tc.model); got != tc.want {
			t.Errorf("familyOf(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

func TestTranslateAlternatesAndDropsWhatConverseRefuses(t *testing.T) {
	a := newTestAdapter(t)
	req := &llm.Request{
		System: "   ",
		Messages: []llm.Message{
			llm.UserText("First."),
			llm.UserText("Second."),
			{Role: llm.RoleAssistant, Parts: []llm.Part{
				{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`"not an object"`)},
				llm.Text(""),
				{Type: llm.PartToolCall, ID: "call_1", Name: "course_get", Args: json.RawMessage(`[1]`)},
			}},
			{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "call_1", Content: "  "}}},
			llm.UserText("Third."),
		},
		Tools:    []llm.Tool{{Name: "course_get", Description: "Get the course."}},
		ToolMode: llm.ToolAuto,
	}
	wire, err := a.translate(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(wire.Messages)
	want := `[{"role":"user","content":[{"text":"First."},{"text":"Second."}]},` +
		`{"role":"assistant","content":[{"toolUse":{"toolUseId":"call_1","name":"course_get","input":{}}}]},` +
		`{"role":"user","content":[{"toolResult":{"toolUseId":"call_1","content":[{"text":"(no content)"}],"status":"success"}},{"text":"Third."}]}]`
	if string(got) != want {
		t.Errorf("messages =\n%s\nwant\n%s", got, want)
	}
	if wire.System != nil || wire.InferenceConfig != nil {
		t.Errorf("system = %v, inferenceConfig = %v; want both left out", wire.System, wire.InferenceConfig)
	}
}

// A user message before a tool message is merged into it, and the results
// still come first.
func TestTranslateKeepsResultsFirstWhenMerging(t *testing.T) {
	a := newTestAdapter(t)
	wire, err := a.translate(&llm.Request{
		Messages: []llm.Message{
			llm.UserText("Q"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ID: "call_1", Name: "course_get", Args: json.RawMessage(`{}`)}}},
			llm.UserText("Also, hurry."),
			{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "call_1", Content: `{"status":"executed"}`}}},
		},
		Tools: tools, ToolMode: llm.ToolAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(wire.Messages[2])
	want := `{"role":"user","content":[{"toolResult":{"toolUseId":"call_1","content":[{"json":{"status":"executed"}}],"status":"success"}},{"text":"Also, hurry."}]}`
	if len(wire.Messages) != 3 || string(got) != want {
		t.Errorf("messages = %d, last =\n%s\nwant\n%s", len(wire.Messages), got, want)
	}
}

func TestErrorType(t *testing.T) {
	for in, want := range map[string]string{
		"ValidationException:http://internal.amazon.com/coral/com.amazon.bedrock/": "ValidationException",
		"com.amazon.coral.validate#ValidationException":                            "ValidationException",
		" ThrottlingException ": "ThrottlingException",
		"":                      "",
	} {
		if got := errorType(in); got != want {
			t.Errorf("errorType(%q) = %q, want %q", in, got, want)
		}
	}
}
