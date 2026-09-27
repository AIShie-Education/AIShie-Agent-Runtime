package bedrock

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
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

func TestThinkingBudget(t *testing.T) {
	withTool := []message{{Role: "user"}, {Role: "assistant", Content: []block{{Text: "x"}, {ToolUse: &toolUseBlock{ToolUseID: "t"}}}}, {Role: "user"}}
	withReasonedTool := []message{{Role: "user"}, {Role: "assistant", Content: []block{{ReasoningContent: json.RawMessage(`{}`)}, {ToolUse: &toolUseBlock{ToolUseID: "t"}}}}, {Role: "user"}}
	plain := []message{{Role: "user"}, {Role: "assistant", Content: []block{{Text: "x"}}}, {Role: "user"}}
	cases := []struct {
		name, model, effort string
		maxTokens           int
		msgs                []message
		want                int
	}{
		{"no effort", claude, "", 4000, plain, 0},
		{"unknown effort", claude, "extreme", 4000, plain, 0},
		{"not Anthropic", nova, "high", 40000, plain, 0},
		{"no cap", claude, "low", 0, plain, 0},
		{"cap too small", claude, "low", 2000, plain, 0},
		{"minimal", claude, "minimal", 4000, plain, 1024},
		{"low under a cap", claude, "low", 3000, plain, 1500},
		{"high", "anthropic.claude-opus-4-1-20250805-v1:0", "high", 64000, plain, 16384},
		{"an ARN", "arn:aws:bedrock:us-east-1:1:inference-profile/global.anthropic.claude-haiku-4-5-20251001-v1:0", "medium", 32000, plain, 8192},
		{"a tool turn without its thinking", claude, "medium", 32000, withTool, 0},
		{"a tool turn with its thinking", claude, "medium", 32000, withReasonedTool, 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t, func(c *llm.Config) { c.Model = tc.model; c.Reasoning.Effort = tc.effort })
			if got := a.thinkingBudget(tc.maxTokens, tc.msgs); got != tc.want {
				t.Errorf("thinkingBudget = %d, want %d", got, tc.want)
			}
		})
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
