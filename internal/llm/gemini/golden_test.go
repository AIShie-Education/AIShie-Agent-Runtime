package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

func ptr[T any](v T) *T { return &v }

// goldenCases are the translations checked both ways, end to end through
// the fake. Each directory under testdata/golden holds two files written by
// hand, request.json (the internal request) and wire_response.json (what
// Gemini answers), and two the adapter made of them, wire_request.json
// (the URL and body it sent) and response.json (the internal response it
// returned). go test -update rewrites the last two.
var goldenCases = []struct {
	name   string
	change func(*llm.Config)
	why    string
}{
	{name: "text", why: "a question and a text answer"},
	{name: "function_call_id", why: "a call Gemini gave an id: the id goes back on the call and its response"},
	{name: "function_call_no_id", why: "a call without an id: call_1 inside, no id on the wire"},
	{name: "parallel", why: "two calls and two responses, in call order; the signature stays on the first"},
	{name: "thought_signature", why: "signatures go back on the same part to the same maker, and never to another"},
	{name: "signature_other_model_gemini3", change: func(c *llm.Config) { c.Model = "gemini-3.5-flash" },
		why: "Gemini 3 given another model's calls mid-loop: the stand-in on each step's first call, its own signature on its own"},
	{name: "thought_parts", change: func(c *llm.Config) { c.Reasoning.Effort = "low" }, why: "thought parts, and parts of kinds the adapter does not read, come back as reasoning, replayed verbatim to their maker only"},
	{name: "is_error", why: "results under output, errors under error; JSON as JSON, anything else as a string"},
	{name: "force_answer", why: "ForceAnswer keeps the tools and says NONE"},
	{name: "force_answer_flattened", change: func(c *llm.Config) { c.Capabilities.ToolChoiceNone = ptr(false) }, why: "without tool_choice none, the tools and the tool history go"},
	{name: "files", why: "files Gemini takes as inlineData, after every functionResponse; other text as text; other types as a line"},
	{name: "files_not_accepted", change: func(c *llm.Config) { c.Capabilities.FileInput = ptr(false) }, why: "a model not given files gets text as text, and a line for the rest"},
	{name: "prompt_blocked", why: "no candidate and a blockReason is content_filter"},
	{name: "usage", why: "every usage field, and raw verbatim"},
	{name: "generation_config", change: func(c *llm.Config) {
		c.Params = llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3), TopP: ptr(0.9)}
		c.Reasoning.Effort = "high"
	}, why: "the call's cap over the configured one, and the thinking budget beside it; a thinking budget for 2.5"},
	{name: "generation_config_gemini3", change: func(c *llm.Config) {
		c.Model = "gemini-3.1-pro-preview"
		c.Params = llm.Params{MaxOutputTokens: 1500}
		c.Reasoning.Effort = "medium"
	}, why: "a thinking level for Gemini 3, with the default allowance; the configured cap when the call sets none"},
	{name: "tools_openapi", change: func(c *llm.Config) { c.Dialect = toolschema.GeminiOpenAPI }, why: "parameters for the OpenAPI dialect; no schema for a tool without arguments"},
}

func TestGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join("testdata", "golden", tc.name)
			var req llm.Request
			readJSON(t, filepath.Join(dir, "request.json"), &req)
			f := newFake(t, ok(readFile(t, filepath.Join(dir, "wire_response.json"))))
			a := f.newAdapter(t, tc.change)

			resp, err := a.Call(context.Background(), &req)
			if err != nil {
				t.Fatalf("%s: Call: %v", tc.why, err)
			}
			sent := f.last(t)
			wire := map[string]any{"url": sent.url, "body": json.RawMessage(sent.body)}
			checkGolden(t, filepath.Join(dir, "wire_request.json"), mustJSON(t, wire))
			checkGolden(t, filepath.Join(dir, "response.json"), mustJSON(t, resp))
		})
	}
}

// finishRows are every finishReason generateContent documents, and some it
// does not, each with a text part, and a few with calls or nothing.
var finishRows = []struct {
	reason string
	parts  string
}{
	{"STOP", textPart},
	{"MAX_TOKENS", textPart},
	{"SAFETY", textPart},
	{"RECITATION", textPart},
	{"LANGUAGE", textPart},
	{"BLOCKLIST", textPart},
	{"PROHIBITED_CONTENT", textPart},
	{"SPII", textPart},
	{"IMAGE_SAFETY", textPart},
	{"IMAGE_PROHIBITED_CONTENT", textPart},
	{"IMAGE_RECITATION", textPart},
	{"MALFORMED_FUNCTION_CALL", ""},
	{"UNEXPECTED_TOOL_CALL", ""},
	{"TOO_MANY_TOOL_CALLS", ""},
	{"MISSING_THOUGHT_SIGNATURE", ""},
	{"MALFORMED_RESPONSE", ""},
	{"OTHER", textPart},
	{"IMAGE_OTHER", ""},
	{"NO_IMAGE", ""},
	{"FINISH_REASON_UNSPECIFIED", textPart},
	{"", textPart},
	{"A_REASON_FROM_THE_FUTURE", textPart},
	// A call means tool_calls whatever the reason said (rule 3).
	{"STOP", callPart},
	{"MAX_TOKENS", callPart},
}

const (
	textPart = `{"text":"Partial answer"}`
	callPart = `{"functionCall":{"name":"course_get","args":{}}}`
)

// TestGoldenFinishReasons maps every finishReason through the adapter into
// one golden table.
func TestGoldenFinishReasons(t *testing.T) {
	type row struct {
		FinishReason string   `json:"finish_reason"`
		Parts        []string `json:"parts"`
		Stop         llm.Stop `json:"stop"`
		RawStop      string   `json:"raw_stop"`
	}
	var answers []answer
	for _, r := range finishRows {
		cand := fmt.Sprintf(`{"content":{"role":"model","parts":[%s]},"finishReason":%q}`, r.parts, r.reason)
		if r.reason == "" {
			cand = fmt.Sprintf(`{"content":{"role":"model","parts":[%s]}}`, r.parts)
		}
		answers = append(answers, ok(`{"candidates":[`+cand+`]}`))
	}
	f := newFake(t, answers...)
	a := f.newAdapter(t)
	var got []row
	for _, r := range finishRows {
		resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}})
		if err != nil {
			t.Fatalf("%q: %v", r.reason, err)
		}
		types := []string{}
		for _, p := range resp.Parts {
			types = append(types, string(p.Type))
		}
		got = append(got, row{FinishReason: r.reason, Parts: types, Stop: resp.Stop, RawStop: resp.RawStop})
	}
	checkGolden(t, filepath.Join("testdata", "golden", "finish_reasons.json"), mustJSON(t, got))
}
