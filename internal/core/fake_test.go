package core

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got with testdata/name, or writes it there with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from what was written (go test -update rewrites it):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// testToken is what the fakes are called with. Nothing may repeat it.
const testToken = "ais_TestTokenThatMustNotLeak_0123"

// rpcSeen is one request a fake Core took.
type rpcSeen struct {
	Header http.Header
	Body   []byte
	Method string
	ID     json.RawMessage
	Params json.RawMessage
}

// fakeMCP answers as Core's /mcp does: initialize with Core's revision and
// instructions, notifications with 202, and tools/call with call.
type fakeMCP struct {
	*httptest.Server
	t *testing.T

	mu   sync.Mutex
	seen []rpcSeen

	// protocol is what initialize answers; the client's revision when "".
	protocol string
	// intercept, when set and returning true, has answered the request
	// itself, whatever its method.
	intercept func(w http.ResponseWriter, r *http.Request, m rpcSeen) bool
	// call answers tools/call.
	call func(w http.ResponseWriter, id json.RawMessage, name string, args json.RawMessage)
	// list answers tools/list.
	list func(w http.ResponseWriter, id json.RawMessage, cursor string)
}

func newFakeMCP(t *testing.T) *fakeMCP {
	f := &fakeMCP{t: t}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &m)
	s := rpcSeen{Header: r.Header.Clone(), Body: body, Method: m.Method, ID: m.ID, Params: m.Params}
	f.mu.Lock()
	f.seen = append(f.seen, s)
	intercept, call, list, protocol := f.intercept, f.call, f.list, f.protocol
	f.mu.Unlock()
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if intercept != nil && intercept(w, r, s) {
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		// As the SDK's RequireBearerToken answers.
		http.Error(w, "invalid token: the credential is missing or not valid", http.StatusUnauthorized)
		return
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if protocol == "" {
			protocol = p.ProtocolVersion
		}
		writeResult(w, m.ID, map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "aishie-core", "version": "test"},
			"instructions":    "Start with me_memberships.",
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if call == nil {
			writeRPCError(w, m.ID, -32602, "unknown tool "+p.Name)
			return
		}
		call(w, m.ID, p.Name, p.Arguments)
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(m.Params, &p)
		list(w, m.ID, p.Cursor)
	default:
		writeRPCError(w, m.ID, -32601, "method not found")
	}
}

// requests are the requests taken so far.
func (f *fakeMCP) requests() []rpcSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rpcSeen(nil), f.seen...)
}

// count is how many requests of method were taken.
func (f *fakeMCP) count(method string) int {
	n := 0
	for _, s := range f.requests() {
		if s.Method == method {
			n++
		}
	}
	return n
}

func (f *fakeMCP) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// toolResult is a CallToolResult as Core makes one: the envelope twice.
func toolResult(env string) map[string]any {
	var s map[string]any
	_ = json.Unmarshal([]byte(env), &s)
	status, _ := s["status"].(string)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": env}},
		"structuredContent": json.RawMessage(env),
		"isError":           status != "executed" && status != "proposed",
	}
}

// answerWith answers every tools/call with env.
func answerWith(env string) func(http.ResponseWriter, json.RawMessage, string, json.RawMessage) {
	return func(w http.ResponseWriter, id json.RawMessage, _ string, _ json.RawMessage) {
		writeResult(w, id, toolResult(env))
	}
}

// noSecret fails if err's text holds the token or anything shaped like one.
func noSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if s := err.Error(); strings.Contains(s, testToken) || strings.Contains(s, "ais_") {
		t.Errorf("an error carries a token: %s", s)
	}
}
