package gemini

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// TestStreamGolden streams each of testdata/stream/*.sse, written as
// streamGenerateContent?alt=sse sends its chunks, into <name>.internal.json
// beside it: the response or the error, and the pieces of text told. Where
// testdata/golden/<name> holds the same answer whole, Stream must make
// exactly what Call makes of it.
func TestStreamGolden(t *testing.T) {
	for _, c := range []struct {
		name  string
		whole bool
		why   string
	}{
		{"thought_signature", true, "text in pieces, the signature on the last, joined onto the one part"},
		{"thought_parts", true, "a thought in pieces, a part of a kind the adapter does not read, then the text"},
		{"parallel", true, "two calls whole in one chunk"},
		{"overloaded_mid_stream", false, "an error part way: overloaded, retryable"},
		{"cut_off", false, "no finishReason before the end: a network error, retryable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sse := readFile(t, filepath.Join("testdata", "stream", c.name+".sse"))
			f := newFake(t, answer{status: http.StatusOK, header: map[string]string{"Content-Type": "text/event-stream"}, body: sse})
			a := f.newAdapter(t)
			var deltas []string
			resp, err := a.Stream(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("How did I do on HW3?")}},
				func(d string) { deltas = append(deltas, d) })
			if u := f.last(t).url; u != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
				t.Errorf("sent to %s", u)
			}
			got := map[string]any{"deltas": deltas}
			if err != nil {
				var e *llm.Error
				if !errors.As(err, &e) {
					t.Fatalf("the error is %T: %v", err, err)
				}
				got["error"] = map[string]any{"kind": e.Kind, "status": e.Status, "code": e.Code, "message": e.Message, "retryable": e.Retryable()}
			} else {
				got["response"] = resp
				if strings.Join(deltas, "") != resp.Text() {
					t.Errorf("told %q; the text is %q", deltas, resp.Text())
				}
			}
			checkGolden(t, filepath.Join("testdata", "stream", c.name+".internal.json"), mustJSON(t, got))
			if !c.whole {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wf := newFake(t, ok(readFile(t, filepath.Join("testdata", "golden", c.name, "wire_response.json"))))
			want, err := wf.newAdapter(t).Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("How did I do on HW3?")}})
			if err != nil {
				t.Fatal(err)
			}
			if g, w := canonical(t, mustJSON(t, resp)), canonical(t, mustJSON(t, want)); !bytes.Equal(g, w) {
				t.Errorf("streamed:\n%s\nwhole:\n%s", g, w)
			}
		})
	}
}

// A stream refused in words that name it is answered by a whole call, and
// the adapter streams no more.
func TestStreamRefused(t *testing.T) {
	f := newFake(t)
	f.answerBy(func([]byte) answer { return answer{} })
	var urls []string
	f.answerBy(func(body []byte) answer {
		n := len(f.requests())
		if n == 1 {
			return answer{status: http.StatusBadRequest, body: `[{"error":{"code":400,"message":"Streaming is not supported for this model.","status":"INVALID_ARGUMENT"}}]`}
		}
		return ok(readFile(t, filepath.Join("testdata", "golden", "text", "wire_response.json")))
	})
	a := f.newAdapter(t)
	for range 2 {
		var told []string
		resp, err := a.Stream(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}}, func(d string) { told = append(told, d) })
		if err != nil || resp.Stop != llm.StopEnd || len(told) != 0 {
			t.Fatalf("%+v %q %v", resp, told, err)
		}
	}
	for _, r := range f.requests() {
		urls = append(urls, r.url)
	}
	if len(urls) != 3 || !strings.Contains(urls[0], "stream") || strings.Contains(urls[1], "stream") || strings.Contains(urls[2], "stream") {
		t.Errorf("sent to %q", urls)
	}
}

// A stream that stops coming ends with the call's time: a timeout.
func TestStreamTimesOut(t *testing.T) {
	f := newFake(t)
	f.answerBy(func([]byte) answer {
		time.Sleep(500 * time.Millisecond)
		return answer{status: http.StatusOK, header: map[string]string{"Content-Type": "text/event-stream"}, body: ""}
	})
	a := f.newAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := a.Stream(ctx, &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}}, func(string) {})
	var e *llm.Error
	if !errors.As(err, &e) || !e.Retryable() {
		t.Fatalf("%v", err)
	}
}
