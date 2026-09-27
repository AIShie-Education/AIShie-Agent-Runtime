package openairesponses

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
)

// Azure OpenAI's refusals can carry error.status as a number, as its
// content filter's 400 does:
//
//	{"error":{"code":"content_filter","message":"…","status":400,…}}
//
// httpx reads error.status as a string, and on a number reads neither code
// nor message, so that refusal would be classified a plain bad request
// rather than content_filter, and the loop would not answer it with the
// refusal text. Until httpx takes a number there, the adapter's client
// drops a non-string error.status from refusals before httpx reads them.

// withErrorFix is client with its transport wrapped by errorStatusFix. The
// client itself is not changed: it may be shared.
func withErrorFix(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c := *client
	c.Transport = errorStatusFix{next: next}
	return &c
}

// errorStatusFix rewrites the body of a 4xx or 5xx answer with
// fixErrorStatus; every other answer passes untouched.
type errorStatusFix struct {
	next http.RoundTripper
}

func (f errorStatusFix) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.next.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	// httpx reads at most this much and refuses more; a body cut here is
	// passed on as it is, and refused the same way.
	data, err := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxResponseBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	data = fixErrorStatus(data)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// fixErrorStatus is body without error.status when that is not a string;
// body itself otherwise, and whenever it is not such an object.
func fixErrorStatus(body []byte) []byte {
	var outer map[string]json.RawMessage
	if json.Unmarshal(body, &outer) != nil {
		return body
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(outer["error"], &inner) != nil || inner == nil {
		return body
	}
	status, ok := inner["status"]
	if !ok {
		return body
	}
	var s *string
	if json.Unmarshal(status, &s) == nil {
		return body
	}
	delete(inner, "status")
	fixed, err := json.Marshal(inner)
	if err != nil {
		return body
	}
	outer["error"] = fixed
	out, err := json.Marshal(outer)
	if err != nil {
		return body
	}
	return out
}
