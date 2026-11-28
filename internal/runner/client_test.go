package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
)

func testModel() Model {
	base := "https://api.example.test/v1"
	return Model{Adapter: "openai_chat", Provider: "openai_compatible", ModelID: "model-x", BaseURL: &base,
		APIKey: "sk-test-not-for-logging", Headers: map[string]string{}, OpenRouter: nil, MaxOutputTokens: 4000, Hosted: true}
}

func TestClientRun(t *testing.T) {
	t.Parallel()
	var request RunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/run" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		_, _ = fmt.Fprintln(w, `{"type":"step","kind":"thinking"}`)
		_, _ = fmt.Fprintln(w, `{"type":"tool_start","id":"call_1","name":"course_materials_search"}`)
		_, _ = fmt.Fprintln(w, `{"type":"tool_end","id":"call_1","name":"course_materials_search","target":"Week 3"}`)
		_, _ = fmt.Fprintln(w, `{"type":"text_delta","delta":"answer"}`)
		_, _ = fmt.Fprintln(w, `{"type":"done","text":"answer","sources":[],"sources_stated":true,"usage":{"input":12,"output":3,"reasoning":1,"cache_read":2,"cache_write":1},"steps":2,"tool_calls":1,"writes":{"executed":0,"proposed":1,"denied":0,"failed":0,"refused":0,"guarded":0},"finish_reason":"stop"}`)
	}))
	defer srv.Close()

	req := RunRequest{CoreMCPURL: "http://core.test/mcp", CoreToken: "ais_secret", CourseID: "course", ConversationID: "conv",
		MessageID: "message", Attempt: 1, AllowedTools: []string{"document_get"}, DeniedTools: []string{}, Seat: Seat{SelfMemberID: "self"},
		SystemPrompt: "system", Messages: []prompt.Message{prompt.UserText("question")}, Model: testModel(),
		Limits: Limits{MaxSteps: 8, MaxToolCalls: 12, MaxWrites: 5, TimeoutMS: 120000, MaxResultBytes: DefaultMaxResultBytes}}
	var events []Event
	done, err := NewClient(srv.URL, srv.Client()).Run(t.Context(), req, func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Text != "answer" || !done.SourcesStated || done.Usage.Input != 12 || done.Usage.CacheRead != 2 || done.Writes.Proposed != 1 {
		t.Errorf("done = %+v", done)
	}
	if len(events) != 5 || events[1].Name != "course_materials_search" || events[4].Type != "done" {
		t.Errorf("events = %+v", events)
	}
	if request.CoreToken != "ais_secret" || request.Model.APIKey != "sk-test-not-for-logging" || request.Model.BaseURL == nil || len(request.Messages) != 1 {
		t.Errorf("request = %+v", request)
	}
}

func TestClientRunErrorDoesNotLeakResponseBody(t *testing.T) {
	t.Parallel()
	const secret = "sk-must-never-appear"
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"http", http.StatusBadGateway, `{"error":"` + secret + `"}`},
		{"event", http.StatusOK, `{"type":"error","kind":"provider_failed","retryable":true,"http_status":503,"message":"provider unavailable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprintln(w, tc.body)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, srv.Client()).Run(t.Context(), RunRequest{}, nil)
			var re *Error
			if !errors.As(err, &re) || !re.Retryable || strings.Contains(err.Error(), secret) {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestClientRunCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := NewClient(srv.URL, srv.Client()).Run(ctx, RunRequest{}, nil)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the HTTP request was not cancelled")
	}
}

func TestClientProbe(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var body ProbeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/probe" {
			t.Errorf("path = %s", r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"result":"key_refused","http_status":401,"provider_code":"invalid_api_key","latency_ms":123}`)
	}))
	defer srv.Close()
	model := testModel()
	model.OpenRouter = nil
	got, err := NewClient(srv.URL, srv.Client()).Probe(t.Context(), model)
	if err != nil || got.Result != ResultKeyRefused || got.HTTPStatus == nil || *got.HTTPStatus != 401 || got.ProviderCode == nil || got.LatencyMS != 123 {
		t.Fatalf("Probe = %+v, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body.TimeoutMS != 25000 || body.Model.APIKey != model.APIKey {
		t.Errorf("body = %+v", body)
	}
}

func TestClientHealthy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	m, err := Start(t.Context(), ProcessOptions{URL: srv.URL, HTTPClient: srv.Client(), StartTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if m.URL() != srv.URL || m.Client() == nil {
		t.Fatalf("manager = %+v", m)
	}
	if err := m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestModelOf(t *testing.T) {
	t.Parallel()
	temp := 0.3
	m := ModelOf(config.Model{Adapter: "openai_chat", Model: "x", BaseURL: "https://api.deepseek.com", Region: "r",
		Params: config.ModelParams{MaxOutputTokens: 123, Temperature: &temp}, Reasoning: config.Reasoning{Effort: "low"}}, "secret", true)
	if m.Provider != "deepseek" || m.ModelID != "x" || m.BaseURL == nil || m.APIKey != "secret" || m.MaxOutputTokens != 123 || m.Temperature != &temp || m.ReasoningEffort != "low" || !m.Hosted || m.Headers == nil {
		t.Errorf("ModelOf = %+v", m)
	}
}
