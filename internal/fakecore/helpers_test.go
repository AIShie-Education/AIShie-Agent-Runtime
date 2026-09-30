package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// protocolVersion is the MCP revision the runtime pins (design §2.1).
const protocolVersion = "2025-11-25"

// httpAnswer is an HTTP answer, whole.
type httpAnswer struct {
	Status int
	Header http.Header
	Body   []byte
}

// mcpClient is a JSON-RPC client of an MCP endpoint written out by hand, as
// the runtime's is, so that a test sees every HTTP status and header.
type mcpClient struct {
	url   string
	token string
	hc    *http.Client
	id    atomic.Int64
}

func newMCPClient(base, token string, hc *http.Client) *mcpClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &mcpClient{url: strings.TrimSuffix(base, "/") + "/mcp", token: token, hc: hc}
}

func (m *mcpClient) post(ctx context.Context, msg any) (httpAnswer, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return httpAnswer{}, err
	}
	return m.postRaw(ctx, body)
}

func (m *mcpClient) postRaw(ctx context.Context, body []byte) (httpAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(body))
	if err != nil {
		return httpAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return httpAnswer{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return httpAnswer{Status: resp.StatusCode, Header: resp.Header, Body: b}, err
}

// initialize opens a session at the pinned revision, and says it is
// initialized, as a client does before calling tools.
func (m *mcpClient) initialize(ctx context.Context) (httpAnswer, error) {
	a, err := m.post(ctx, map[string]any{"jsonrpc": "2.0", "id": m.id.Add(1), "method": "initialize", "params": map[string]any{
		"protocolVersion": protocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "fakecore-test", "version": "1"}}})
	if err != nil || a.Status != http.StatusOK {
		return a, err
	}
	_, err = m.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return a, err
}

// toolAnswer is what a tools/call came back with.
type toolAnswer struct {
	httpAnswer
	IsError    bool
	Structured map[string]any
	Text       string
	RPCError   json.RawMessage
}

func (a toolAnswer) status() string {
	s, _ := a.Structured["status"].(string)
	return s
}

func (a toolAnswer) str(path ...string) string {
	var v any = a.Structured
	for _, p := range path {
		m, _ := v.(map[string]any)
		v = m[p]
	}
	s, _ := v.(string)
	return s
}

func (m *mcpClient) call(ctx context.Context, tool string, args any) (toolAnswer, error) {
	h, err := m.post(ctx, map[string]any{"jsonrpc": "2.0", "id": m.id.Add(1), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	a := toolAnswer{httpAnswer: h}
	if err != nil || h.Status != http.StatusOK {
		return a, err
	}
	var rpc struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(h.Body, &rpc); err != nil {
		return a, fmt.Errorf("the answer is not JSON-RPC: %w: %s", err, h.Body)
	}
	if rpc.Result == nil {
		a.RPCError = rpc.Error
		return a, nil
	}
	a.IsError = rpc.Result.IsError
	if len(rpc.Result.Content) > 0 {
		a.Text = rpc.Result.Content[0].Text
	}
	a.Structured = map[string]any{}
	if err := decodeNumbers(rpc.Result.StructuredContent, &a.Structured); err != nil {
		return a, fmt.Errorf("structuredContent: %w", err)
	}
	return a, nil
}

// restClient calls Core's REST routes as one actor.
type restClient struct {
	base  string
	token string
	hc    *http.Client
}

func (r *restClient) do(ctx context.Context, method, path string, body any, key string) (httpAnswer, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return httpAnswer{}, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(r.base, "/")+path, rd)
	if err != nil {
		return httpAnswer{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	hc := r.hc
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return httpAnswer{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return httpAnswer{Status: resp.StatusCode, Header: resp.Header, Body: b}, err
}

func decodeNumbers(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// ---------------------------------------------------------------------------
// Normalising what was recorded
// ---------------------------------------------------------------------------

var (
	uuidRE  = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timeRE  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
	tokenRE = regexp.MustCompile(`ais_[A-Za-z0-9_-]+`)
	// goTypeRE is the name of a Go type that encoding/json puts in the
	// message of a value it cannot decode: Core's input type, or the fake's.
	goTypeRE = regexp.MustCompile(`Go struct field [A-Za-z0-9_]+\.`)
)

// longText is where a string is too long to keep in a fixture as it is:
// the bodies at Core's limit of 20,000 characters, not Core's instructions.
const longText = 16384

// normalizer replaces what differs between two runs of one scenario with
// placeholders stable within one file: every UUID by <id:n> in the order
// first met (written in upper case, <ID:n> with its lower case's n),
// timestamps by <time>, tokens by <token> and their public prefixes by
// <prefix>, uploads' tokens by <upload_token>, the feed's sequence numbers by
// <seq:n>, long text by its length, and the Go type a message names by
// <type>. Objects are walked in the order of their sorted keys, so the
// numbering depends only on what the file holds.
type normalizer struct {
	ids  map[string]string
	seqs map[string]string
}

func newNormalizer() *normalizer {
	return &normalizer{ids: map[string]string{}, seqs: map[string]string{}}
}

func (z *normalizer) walk(key string, v any, inEvent bool) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_, event := x["occurred_at"]
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[k] = z.walk(k, x[k], event)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = z.walk(key, e, false)
		}
		return out
	case string:
		if key == "token_prefix" {
			// A token's public prefix: random, as the token is.
			return "<prefix>"
		}
		if key == "upload_token" {
			// An upload's token: a credential, signed by Core in a form
			// of its own, which the fake does not copy.
			return "<upload_token>"
		}
		return z.text(x)
	case json.Number:
		if (key == "seq" && inEvent) || key == "next_seq" || key == "since_seq" {
			return z.seq(string(x))
		}
		return x
	case float64:
		return z.walk(key, json.Number(fmt.Sprint(x)), inEvent)
	}
	return v
}

func (z *normalizer) text(s string) string {
	if timeRE.MatchString(s) {
		return "<time>"
	}
	if len(s) > longText {
		return fmt.Sprintf("<text:%d chars>", utf8.RuneCountInString(s))
	}
	s = tokenRE.ReplaceAllString(s, "<token>")
	s = goTypeRE.ReplaceAllString(s, "Go struct field <type>.")
	return uuidRE.ReplaceAllStringFunc(s, func(id string) string {
		lower := strings.ToLower(id)
		p, ok := z.ids[lower]
		if !ok {
			p = fmt.Sprintf("<id:%d>", len(z.ids)+1)
			z.ids[lower] = p
		}
		if id != lower {
			return strings.Replace(p, "<id:", "<ID:", 1)
		}
		return p
	})
}

func (z *normalizer) seq(n string) any {
	if n == "0" {
		return json.Number("0")
	}
	if p, ok := z.seqs[n]; ok {
		return p
	}
	p := fmt.Sprintf("<seq:%d>", len(z.seqs)+1)
	z.seqs[n] = p
	return p
}

// normalize is v with what differs between runs replaced, through a JSON
// round trip so that any Go value comes out as plain JSON values.
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var plain any
	if err := decodeNumbers(b, &plain); err != nil {
		return nil, err
	}
	return newNormalizer().walk("", plain, false), nil
}

// indented is v as a fixture file holds it.
func indented(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	_ = enc.Encode(v)
	return buf.Bytes()
}

// diff describes where two normalized documents differ, line by line, for a
// failure message; "" when they are equal.
func diff(want, got any) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	w, g := strings.Split(string(indented(want)), "\n"), strings.Split(string(indented(got)), "\n")
	var out strings.Builder
	shown := 0
	for i := 0; i < max(len(w), len(g)) && shown < 12; i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			fmt.Fprintf(&out, "line %d:\n  core: %s\n  fake: %s\n", i+1, a, b)
			shown++
		}
	}
	if shown == 0 {
		return "the documents differ in a way the line diff does not show"
	}
	return out.String()
}
