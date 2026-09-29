package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

func events(t *testing.T, body string) []Event {
	t.Helper()
	s := NewStream(context.Background(), 200, http.Header{"Content-Type": {"text/event-stream"}}, io.NopCloser(strings.NewReader(body)))
	var out []Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func TestStreamEvents(t *testing.T) {
	long := strings.Repeat("x", 200<<10) // past the reader's buffer
	got := events(t, ": comment\n\n"+
		"event: message_start\ndata: {\"a\":1}\n\n"+
		"data: one\ndata: two\n\n"+
		"id: 7\nretry: 100\n\n"+ // no data: no event
		"data:no space\r\n\r\n"+
		"event: ping\n\n"+ // no data: no event, and its name is forgotten
		"data: "+long+"\n\n"+
		"data: [DONE]") // the end, without a blank line
	want := []Event{
		{Name: "message_start", Data: []byte(`{"a":1}`)},
		{Data: []byte("one\ntwo")},
		{Data: []byte("no space")},
		{Data: []byte(long)},
		{Data: []byte("[DONE]")},
	}
	if len(got) != len(want) {
		t.Fatalf("%d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Data) != string(want[i].Data) {
			t.Errorf("event %d: %q %.40q, want %q %.40q", i, got[i].Name, got[i].Data, want[i].Name, want[i].Data)
		}
	}
}

func TestPostStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = io.WriteString(w, "data: a\n\ndata: b\n\n")
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"whole":true}`)
		case "/refused":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key sk-secret-123456789"}}`)
		}
	}))
	defer srv.Close()
	headers := map[string]string{"Authorization": "Bearer sk-secret-123456789"}

	s, err := PostStream(context.Background(), nil, srv.URL+"/sse", headers, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.EventStream() {
		t.Error("text/event-stream is not an event stream")
	}
	first, err1 := s.Next()
	second, err2 := s.Next()
	_, end := s.Next()
	if err1 != nil || err2 != nil || string(first.Data) != "a" || string(second.Data) != "b" || !errors.Is(end, io.EOF) {
		t.Errorf("%q %v, %q %v, then %v", first.Data, err1, second.Data, err2, end)
	}
	_ = s.Close()

	s, err = PostStream(context.Background(), nil, srv.URL+"/json", headers, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.EventStream() {
		t.Error("application/json is an event stream")
	}
	if whole, err := s.ReadAll(); err != nil || string(whole.Body) != `{"whole":true}` || whole.Status != 200 {
		t.Errorf("%+v %v", whole, err)
	}
	_ = s.Close()

	_, err = PostStream(context.Background(), nil, srv.URL+"/refused", headers, []byte(`{}`))
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrAuth || strings.Contains(e.Message, "sk-secret") {
		t.Errorf("%v", err)
	}
}

// A stream that ends in the middle of an event gives what it has of it,
// as it gives an event without the blank line after it: an adapter finds
// it does not decode, or that its answer never finished.
func TestStreamCutInAnEvent(t *testing.T) {
	got := events(t, "data: {\"a\":1}\n\ndata: {\"b\"")
	if len(got) != 2 || string(got[1].Data) != `{"b"` {
		t.Fatalf("%q", got)
	}
}

func TestBrokenIsRetryable(t *testing.T) {
	if e := Broken(200); !e.Retryable() || e.Kind != llm.ErrNetwork {
		t.Errorf("%+v", e)
	}
}
