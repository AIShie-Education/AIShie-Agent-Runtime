package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// The golden tests run the adapter end to end against a fake provider.
// Each case in testdata/golden has up to three files:
//
//	<case>.response.json  what the provider answers: written by hand, never rewritten
//	<case>.request.json   golden: the body the adapter POSTed to /responses
//	<case>.result.json    golden: the llm.Response the adapter returned
//
// go test -update rewrites the goldens from what the adapter does now; read
// the diff before committing it.
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const (
	testModel  = "gpt-test"
	testKey    = "sk-test-0123456789abcdefghijklmnopqrstuvwxyz"
	assignment = "0192f3c1-7a2b-7c3d-8e4f-5a6b7c8d9e0f"
)

// provider is a fake Responses API. It records every request and answers
// each with the same status and body.
type provider struct {
	srv    *httptest.Server
	status int
	header http.Header
	body   []byte

	mu   sync.Mutex
	reqs []captured
}

type captured struct {
	Host   string
	Path   string
	Header http.Header
	Body   []byte
}

func newProvider(t *testing.T, status int, body []byte) *provider {
	t.Helper()
	p := &provider{status: status, body: body, header: http.Header{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		if _, err := b.ReadFrom(r.Body); err != nil {
			t.Errorf("reading the request: %v", err)
		}
		p.mu.Lock()
		p.reqs = append(p.reqs, captured{Host: r.Host, Path: r.URL.Path, Header: r.Header.Clone(), Body: b.Bytes()})
		p.mu.Unlock()
		for k, vs := range p.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.status)
		_, _ = w.Write(p.body)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// fixture is a provider answering with testdata/golden/<name>.response.json.
func fixture(t *testing.T, name string) *provider {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "golden", name+".response.json"))
	if err != nil {
		t.Fatal(err)
	}
	return newProvider(t, http.StatusOK, body)
}

// answer makes the provider answer the next calls with another fixture.
func (p *provider) answer(t *testing.T, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "golden", name+".response.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.body = body
	p.mu.Unlock()
}

func (p *provider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *provider) last(t *testing.T) captured {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		t.Fatal("the provider was not called")
	}
	return p.reqs[len(p.reqs)-1]
}

// client sends every request to the fake, whatever its URL, so that
// adapters keep their real base URLs, and with them stable Makers.
func (p *provider) client() *http.Client {
	target, _ := url.Parse(p.srv.URL)
	return &http.Client{Transport: redirect{target: target, next: p.srv.Client().Transport}}
}

type redirect struct {
	target *url.URL
	next   http.RoundTripper
}

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	return r.next.RoundTrip(req)
}

// newAdapter is an adapter for OpenAI's default base, talking to p, with
// cfg changed by mod.
func newAdapter(t *testing.T, p *provider, mod func(*llm.Config)) *Adapter {
	t.Helper()
	cfg := llm.Config{Adapter: llm.AdapterOpenAIResponses, Model: testModel, APIKey: testKey, HTTPClient: p.client()}
	if mod != nil {
		mod(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// golden compares got with testdata/golden/<name> as canonical JSON, or
// writes it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	canon := canonical(t, got)
	if *update {
		if err := os.WriteFile(path, canon, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test -update writes it)", err)
	}
	if !bytes.Equal(canonical(t, want), canon) {
		t.Errorf("%s differs from the golden file\n--- got\n%s\n--- want\n%s", name, canon, canonical(t, want))
	}
}

// canonical is JSON with sorted keys and fixed indentation, numbers kept
// as written.
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

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var testTools = []llm.Tool{
	{
		Name:        "grade_list",
		Description: "Lists the grades the seat may read.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string","description":"Only this assignment's grades (optional; omit or null)"}},"additionalProperties":false}`),
	},
	{
		Name:        "assignment_get",
		Description: "Reads one assignment.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string","format":"uuid"}},"required":["assignment_id"],"additionalProperties":false}`),
	},
}

var strictTools = []llm.Tool{
	{
		Name:        "grade_list",
		Description: "Lists the grades the seat may read.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":["string","null"],"description":"Only this assignment's grades (optional; omit or null)"}},"required":["assignment_id"],"additionalProperties":false}`),
	},
	{
		Name:        "assignment_get",
		Description: "Reads one assignment.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string","format":"uuid"}},"required":["assignment_id"],"additionalProperties":false}`),
	},
}

const (
	question = "Why did I lose marks on HW3?"
	system   = "You are a tutor."
	executed = `{"status":"executed","result":{"grades":[{"assignment_id":"0192f3c1-7a2b-7c3d-8e4f-5a6b7c8d9e0f","points":"8.0","max_points":"10.0"}]}}`
	notFound = `{"status":"error","error":{"code":"not_found","message":"no such assignment","details":{}}}`
)

func args(s string) json.RawMessage { return json.RawMessage(s) }

// plainRequest is a question with no tools; the cases that use it check
// only the result, since "text" checks its request.
func plainRequest(*Adapter) *llm.Request {
	return &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 2000}}
}

// toolHistory is a question, this adapter's two calls and their results.
func toolHistory(a *Adapter) []llm.Message {
	return []llm.Message{
		llm.UserText(question),
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartText, Text: "Let me check.", Maker: a.Maker(), Opaque: args(`{"id":"msg_A","phase":"commentary"}`)},
			{Type: llm.PartToolCall, ID: "call_A", Name: "grade_list", Args: args(`{"assignment_id":"` + assignment + `"}`), Maker: a.Maker(), Opaque: args(`{"id":"fc_A"}`)},
			{Type: llm.PartToolCall, ID: "call_B", Name: "assignment_get", Args: args(`{"assignment_id":"` + assignment + `"}`), Maker: a.Maker(), Opaque: args(`{"id":"fc_B"}`)},
		}},
		{Role: llm.RoleTool, Parts: []llm.Part{
			{Type: llm.PartToolResult, CallID: "call_A", Name: "grade_list", Content: executed},
			{Type: llm.PartToolResult, CallID: "call_B", Name: "assignment_get", Content: notFound, IsError: true},
		}},
	}
}

type goldenCase struct {
	name string
	cfg  func(*llm.Config)
	// req builds the request; nil is plainRequest, and then only the result
	// is compared.
	req func(a *Adapter) *llm.Request
}

var goldenCases = []goldenCase{
	{name: "text", req: plainRequest},
	{name: "tools_parallel", req: func(*Adapter) *llm.Request {
		return &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, Tools: testTools, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 2000}}
	}},
	{name: "tool_outputs", req: func(a *Adapter) *llm.Request {
		return &llm.Request{System: system, Messages: toolHistory(a), Tools: testTools, ToolMode: llm.ToolAuto}
	}},
	// Another model's turn: its reasoning is dropped, and its item id and
	// phase are not sent; its calls go back by call_id, and arguments that
	// did not parse go back as the model wrote them.
	{name: "history_other_maker", req: func(*Adapter) *llm.Request {
		other := llm.MakerOf(llm.AdapterAnthropic, "https://api.anthropic.com", "claude-test")
		return &llm.Request{System: system, Tools: testTools, ToolMode: llm.ToolAuto, Messages: []llm.Message{
			llm.UserText(question),
			{Role: llm.RoleAssistant, Parts: []llm.Part{
				{Type: llm.PartReasoning, Text: "thinking", Maker: other, Opaque: args(`{"type":"reasoning","encrypted_content":"not-for-openai","id":"rs_x"}`)},
				{Type: llm.PartText, Text: "Checking.", Maker: other, Opaque: args(`{"phase":"commentary"}`)},
				{Type: llm.PartToolCall, ID: "toolu_1", Name: "grade_list", Args: args(`{}`), Maker: other, Opaque: args(`{"id":"fc_other"}`)},
				{Type: llm.PartToolCall, ID: "toolu_2", Name: "assignment_get", Args: args(`{}`), ArgsError: `{"assignment_id": "0192`},
			}},
			{Role: llm.RoleTool, Parts: []llm.Part{
				{Type: llm.PartToolResult, CallID: "toolu_1", Name: "grade_list", Content: executed},
				{Type: llm.PartToolResult, CallID: "toolu_2", Name: "assignment_get", Content: `{"status":"error","error":{"code":"invalid_argument","message":"the arguments are not a JSON object"}}`, IsError: true},
			}},
		}}
	}},
	// ForceAnswer where the API takes tool_choice none.
	{name: "force_answer", req: func(a *Adapter) *llm.Request {
		return &llm.Request{System: system, Messages: toolHistory(a), Tools: testTools, ToolMode: llm.ToolNone, Limits: llm.Limits{MaxOutputTokens: 500}}
	}},
	// ForceAnswer where the agent says it does not: the tools are left out
	// and the history goes as text.
	{name: "force_answer_flattened", cfg: func(c *llm.Config) { c.Capabilities.ToolChoiceNone = ptr(false) }, req: func(a *Adapter) *llm.Request {
		return &llm.Request{System: system, Messages: toolHistory(a), Tools: testTools, ToolMode: llm.ToolNone, Limits: llm.Limits{MaxOutputTokens: 500}}
	}},
	{name: "files", req: filesRequest},
	{name: "files_not_taken", cfg: func(c *llm.Config) { c.Capabilities.FileInput = ptr(false) }, req: filesRequest},
	{name: "params", cfg: func(c *llm.Config) {
		c.Params = llm.Params{MaxOutputTokens: 1500, Temperature: ptr(0.3), TopP: ptr(0.9)}
		c.Capabilities.ParallelToolCalls = ptr(false)
	}, req: func(*Adapter) *llm.Request {
		return &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, Tools: testTools[:1], ToolMode: llm.ToolAuto}
	}},
	// The tools as the openai_strict dialect gives them: every property
	// required, the optional ones nullable.
	{name: "strict_tools", cfg: func(c *llm.Config) { c.Capabilities.StrictTools = ptr(true) }, req: func(*Adapter) *llm.Request {
		return &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, Tools: strictTools, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 2000}}
	}},
	{name: "refusal"},
	{name: "refusal_incomplete"},
	// A refused turn's calls are disowned: none runs.
	{name: "refusal_with_call"},
	{name: "commentary_and_calls"},
	{name: "args"},
	{name: "usage"},
	{name: "usage_absent"},
	{name: "incomplete_max_output_tokens"},
	{name: "incomplete_reasoning_only"},
	// A call cut off by the cap never runs; complete calls before it do.
	{name: "incomplete_call_cut"},
	{name: "incomplete_calls_before_cut"},
	{name: "incomplete_content_filter"},
	{name: "incomplete_content_filter_calls"},
	{name: "incomplete_max_messages"},
	{name: "incomplete_steered"},
	{name: "incomplete_no_reason"},
	{name: "failed"},
	{name: "cancelled"},
	{name: "queued"},
	{name: "status_absent"},
	// Items and content the runtime does not read are skipped, whatever
	// their shape; null usage details are zero.
	{name: "unknown_items"},
}

// filesRequest gives files of every kind, in a question and beside a
// result: images and PDFs go as files, text as text, and the rest as notes.
func filesRequest(*Adapter) *llm.Request {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF")
	pdf := []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n")
	return &llm.Request{System: system, Tools: testTools, ToolMode: llm.ToolAuto, Messages: []llm.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{
			llm.Text("Here are my essay, its chart and my notes."),
			{Type: llm.PartFile, File: &llm.File{Name: "chart.png", MIME: "image/png", Data: png}},
			{Type: llm.PartFile, File: &llm.File{Name: "essay.pdf", MIME: "application/pdf", Data: pdf}},
			{Type: llm.PartFile, File: &llm.File{Name: "notes.txt", MIME: "text/plain; charset=utf-8", Data: []byte("Loop invariants: see week 3.")}},
		}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartToolCall, ID: "call_doc", Name: "document_get", Args: args(`{"document_id":"` + assignment + `"}`)},
		}},
		{Role: llm.RoleTool, Parts: []llm.Part{
			{Type: llm.PartToolResult, CallID: "call_doc", Name: "document_get", Content: `{"status":"executed","result":{"title":"Syllabus","file":"given below"}}`},
			{Type: llm.PartFile, File: &llm.File{Name: "syllabus.pdf", MIME: "Application/PDF; name=syllabus.pdf", Data: pdf}},
			{Type: llm.PartFile, File: &llm.File{Name: "board.jpg", MIME: "image/jpg", Data: jpeg}},
			{Type: llm.PartFile, File: &llm.File{Name: "rubric.json", MIME: "application/json", Data: []byte(`{"criteria":["correctness","style"]}`)}},
			{Type: llm.PartFile, File: &llm.File{Name: "slides.pptx", MIME: "application/vnd.openxmlformats-officedocument.presentationml.presentation", Data: []byte("PK\x03\x04")}},
			{Type: llm.PartFile, File: &llm.File{Name: "latin1.txt", MIME: "text/plain", Data: []byte("caf\xe9")}},
			{Type: llm.PartFile, File: &llm.File{Data: []byte("plain bytes")}},
		}},
	}}
}

func ptr[T any](v T) *T { return &v }

func TestGolden(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.name, func(t *testing.T) {
			p := fixture(t, c.name)
			a := newAdapter(t, p, c.cfg)
			build := c.req
			if build == nil {
				build = plainRequest
			}
			resp, err := a.Call(context.Background(), build(a))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if c.req != nil {
				golden(t, c.name+".request.json", p.last(t).Body)
			}
			golden(t, c.name+".result.json", marshal(t, resp))
		})
	}
}

// TestGoldenReasoningRoundTrip follows one loop: a reasoning model calls
// two tools; its next turn gets its reasoning back verbatim, with
// encrypted_content, and its calls' item ids; another model given the same
// history gets neither.
func TestGoldenReasoningRoundTrip(t *testing.T) {
	p := fixture(t, "reasoning_turn1")
	effort := func(c *llm.Config) { c.Reasoning = llm.Reasoning{Effort: "low"} }
	a := newAdapter(t, p, effort)
	first := &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, Tools: testTools, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 2000}}
	resp, err := a.Call(context.Background(), first)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	golden(t, "reasoning_turn1.request.json", p.last(t).Body)
	golden(t, "reasoning_turn1.result.json", marshal(t, resp))
	if resp.Stop != llm.StopToolCalls || len(resp.ToolCalls()) != 2 {
		t.Fatalf("turn 1: stop %s with %d calls, want tool_calls with 2", resp.Stop, len(resp.ToolCalls()))
	}

	var results []llm.Part
	for _, c := range resp.ToolCalls() {
		results = append(results, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name, Content: executed})
	}
	second := &llm.Request{System: system, Tools: testTools, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 2000}, Messages: []llm.Message{
		llm.UserText(question),
		{Role: llm.RoleAssistant, Parts: resp.Parts},
		{Role: llm.RoleTool, Parts: results},
	}}

	p.answer(t, "reasoning_turn2")
	resp2, err := a.Call(context.Background(), second)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	same := p.last(t).Body
	golden(t, "reasoning_turn2_same.request.json", same)
	golden(t, "reasoning_turn2.result.json", marshal(t, resp2))
	for _, want := range []string{`"encrypted_content":"gAAAAABoEncryptedTurnOne=="`, `"id":"fc_rs_1"`, `"id":"fc_rs_2"`, `"call_id":"call_rs_grades"`} {
		if !bytes.Contains(same, []byte(want)) {
			t.Errorf("the maker's own turn 2 lacks %s", want)
		}
	}

	other := newAdapter(t, p, func(c *llm.Config) { effort(c); c.Model = "gpt-other" })
	if _, err := other.Call(context.Background(), second); err != nil {
		t.Fatalf("turn 2, another model: %v", err)
	}
	body := p.last(t).Body
	golden(t, "reasoning_turn2_other.request.json", body)
	for _, unwanted := range []string{"gAAAAABoEncryptedTurnOne", "rs_turn1", `"type":"reasoning"`, "fc_rs_"} {
		if bytes.Contains(body, []byte(unwanted)) {
			t.Errorf("another model's turn 2 holds %s", unwanted)
		}
	}
	if !bytes.Contains(body, []byte(`"call_id":"call_rs_grades"`)) {
		t.Error("another model's turn 2 lost the call_id that links a call to its output")
	}
}

// TestGoldenReasoningUnencrypted: reasoning that came without
// encrypted_content (no effort asked, so none included) cannot go back to a
// store that kept nothing, and takes its turn's item ids with it.
func TestGoldenReasoningUnencrypted(t *testing.T) {
	p := fixture(t, "reasoning_unencrypted")
	a := newAdapter(t, p, nil)
	first := &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, Tools: testTools, ToolMode: llm.ToolAuto}
	resp, err := a.Call(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "reasoning_unencrypted.result.json", marshal(t, resp))
	second := &llm.Request{System: system, Tools: testTools, ToolMode: llm.ToolAuto, Messages: []llm.Message{
		llm.UserText(question),
		{Role: llm.RoleAssistant, Parts: resp.Parts},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "call_plain", Name: "grade_list", Content: executed}}},
	}}
	if _, err := a.Call(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	body := p.last(t).Body
	golden(t, "reasoning_unencrypted_turn2.request.json", body)
	if bytes.Contains(body, []byte("rs_plain")) || bytes.Contains(body, []byte("fc_plain")) {
		t.Errorf("turn 2 holds an item id the API cannot resolve:\n%s", body)
	}
	if strings.Contains(string(body), `"include"`) {
		t.Errorf("turn 2 asks for encrypted reasoning with no effort set:\n%s", body)
	}
}
