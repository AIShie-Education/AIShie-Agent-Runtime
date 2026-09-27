package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		header http.Header
		body   string
		kind   llm.ErrorKind
		code   string
	}{
		{"openai rate limit", 429, http.Header{"Retry-After": {"7"}}, `{"error":{"code":"rate_limit_exceeded","message":"slow down","type":"requests"}}`, llm.ErrRateLimited, "rate_limit_exceeded"},
		{"openai out of quota", 429, nil, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, llm.ErrAuth, "insufficient_quota"},
		{"openai context", 400, nil, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 128000 tokens"}}`, llm.ErrContextOverflow, "context_length_exceeded"},
		{"anthropic overloaded", 529, nil, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, llm.ErrOverloaded, "overloaded_error"},
		{"anthropic too long", 400, nil, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, llm.ErrContextOverflow, "invalid_request_error"},
		{"gemini exhausted", 429, nil, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`, llm.ErrRateLimited, "RESOURCE_EXHAUSTED"},
		{"azure content filter, numeric status", 400, nil, `{"error":{"code":"content_filter","message":"The response was filtered","status":400,"innererror":{"code":"ResponsibleAIPolicyViolation"}}}`, llm.ErrContentFilter, "content_filter"},
		{"aws throttling", 429, nil, `{"message":"Too many requests","__type":"ThrottlingException"}`, llm.ErrRateLimited, "ThrottlingException"},
		{"ollama plain error", 400, nil, `{"error":"model not found"}`, llm.ErrBadRequest, ""},
		{"not json", 502, nil, `<html>bad gateway</html>`, llm.ErrServer, ""},
		{"unauthorized", 401, nil, `{"error":{"message":"Incorrect API key provided"}}`, llm.ErrAuth, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := Classify(c.status, c.header, []byte(c.body))
			if e.Kind != c.kind || e.Code != c.code {
				t.Errorf("Classify = %s %q, want %s %q (%s)", e.Kind, e.Code, c.kind, c.code, e.Message)
			}
			if e.Status != c.status {
				t.Errorf("Status = %d", e.Status)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	if got := RetryAfter(http.Header{"Retry-After": {"7"}}); got != 7*time.Second {
		t.Errorf("seconds: %s", got)
	}
	if got := RetryAfter(http.Header{"Retry-After-Ms": {"1500"}, "Retry-After": {"9"}}); got != 1500*time.Millisecond {
		t.Errorf("milliseconds first: %s", got)
	}
	date := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := RetryAfter(http.Header{"Retry-After": {date}}); got < 25*time.Second || got > 31*time.Second {
		t.Errorf("date: %s", got)
	}
	if RetryAfter(nil) != 0 || RetryAfter(http.Header{"Retry-After": {"soon"}}) != 0 {
		t.Error("nothing usable is no wait")
	}
}

func TestPostJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Extra") != "1" {
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/fail" {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	resp, err := PostJSON(context.Background(), srv.Client(), Join(srv.URL, "ok"), map[string]string{"X-Extra": "1"}, []byte(`{}`))
	if err != nil || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("ok: %v %v", resp, err)
	}
	_, err = PostJSON(context.Background(), srv.Client(), srv.URL+"/fail", map[string]string{"X-Extra": "1"}, []byte(`{}`))
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrOverloaded || e.RetryAfter != 2*time.Second || !e.Retryable() {
		t.Fatalf("fail: %#v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer slow.Close()
	_, err = PostJSON(ctx, slow.Client(), slow.URL, nil, nil)
	if !errors.As(err, &e) || e.Kind != llm.ErrTimeout {
		t.Fatalf("timeout: %#v", err)
	}
}

func TestJoin(t *testing.T) {
	for _, c := range [][3]string{
		{"https://api.openai.com/v1", "chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
	} {
		if got := Join(c[0], c[1]); got != c[2] {
			t.Errorf("Join(%q, %q) = %q", c[0], c[1], got)
		}
	}
}

func TestClipKeepsRunes(t *testing.T) {
	s := strings.Repeat("字", 300)
	got := llm.Clip(s)
	if len(got) > 400 || !strings.HasSuffix(got, "…") {
		t.Errorf("Clip: %d bytes", len(got))
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("Clip cut a rune")
		}
	}
}
