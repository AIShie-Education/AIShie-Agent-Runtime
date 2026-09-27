package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testCatalogue(t *testing.T) *Catalogue {
	t.Helper()
	c, err := ParseCatalogue(readCatalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRESTRequests lays calls out as Core's httpapi reads them: path
// parameters from the arguments, a GET's others in the query string as
// Core's coerce reads them back, a POST's in the body with the key in a
// header; and POST /v1/tools/{name} where the route cannot carry them.
func TestRESTRequests(t *testing.T) {
	cat := testCatalogue(t)
	const C, X, M = "0192f3c1-0000-7000-8000-00000000000c", "0192f3c1-0000-7000-8000-00000000000a", "0192f3c1-0000-7000-8000-00000000000b"
	sub := func(s string) string { return strings.NewReplacer("$C", C, "$X", X, "$M", M).Replace(s) }
	for _, tc := range []struct {
		name, tool, args string
		method, path     string
		key              string
		body             string // "" for none
	}{
		{"a write", "conversation_answer",
			`{"course_id":"$C","conversation_id":"$X","in_reply_to_message_id":"$M","body":"a <b> & c","idempotency_key":"answer:$X:$M:1"}`,
			"POST", "/v1/courses/$C/conversations/$X/answer", "answer:$X:$M:1", `{"in_reply_to_message_id":"$M","body":"a <b> & c"}`},
		{"a write's body keeps its order and values, whitespace aside", "conversation_close",
			`{ "reason" : "doneé",  "idempotency_key":"close:$X", "conversation_id":"$X","course_id":"$C" }`,
			"POST", "/v1/courses/$C/conversations/$X/close", "close:$X", `{"reason":"doneé"}`},
		{"a write with nothing but its path", "agent_suspend", `{"actor_id":"$M","idempotency_key":"k"}`,
			"POST", "/v1/me/agents/$M/suspend", "k", `{}`},
		{"a read with no arguments", "me_get", `{}`, "GET", "/v1/me", "", ""},
		{"no arguments at all", "me_memberships", ``, "GET", "/v1/me/memberships", "", ""},
		{"null arguments", "me_memberships", `null`, "GET", "/v1/me/memberships", "", ""},
		{"an integer in the query", "conversation_inbox", `{"course_id":"$C","limit":20}`,
			"GET", "/v1/courses/$C/conversations/inbox?limit=20", "", ""},
		{"a null left out where the schema allows it", "conversation_messages",
			`{"course_id":"$C","conversation_id":"$X","after_seq":null,"before_seq":7,"limit":30}`,
			"GET", "/v1/courses/$C/conversations/$X/messages?before_seq=7&limit=30", "", ""},
		{"an array as repeated values", "action_list_mine",
			`{"course_id":"$C","exclude_types":["conversation.ask","conversation.answer"],"after":"$M"}`,
			"GET", "/v1/courses/$C/actions/mine?after=$M&exclude_types=conversation.ask&exclude_types=conversation.answer", "", ""},
		{"a boolean", "document_list", `{"course_id":"$C","include_archived":true,"kind":"material"}`,
			"GET", "/v1/courses/$C/documents?include_archived=true&kind=material", "", ""},
		{"text that needs escaping", "conversation_list", `{"course_id":"$C","state":"a&b=c d/é"}`,
			"GET", "/v1/courses/$C/conversations?state=a%26b%3Dc+d%2F%C3%A9", "", ""},
		{"a path parameter that needs escaping", "conversation_get", `{"course_id":"a/b c","conversation_id":"$X"}`,
			"GET", "/v1/courses/a%2Fb%20c/conversations/$X", "", ""},
		{"a read's idempotency_key stays an argument", "me_get", `{"idempotency_key":"k"}`,
			"GET", "/v1/me?idempotency_key=k", "", ""},
		// Where the route cannot carry the arguments exactly, the generic
		// route takes them all in the body, and Core judges them.
		{"an empty array has no query form", "action_list_mine", `{"course_id":"$C","exclude_types":[]}`,
			"POST", "/v1/tools/action.list_mine", "", `{"course_id":"$C","exclude_types":[]}`},
		{"text Core would read back as a number", "conversation_inbox", `{"course_id":"$C","limit":"20"}`,
			"POST", "/v1/tools/conversation.inbox", "", `{"course_id":"$C","limit":"20"}`},
		{"a number Core would read back as text", "conversation_list", `{"course_id":"$C","after":5}`,
			"POST", "/v1/tools/conversation.list", "", `{"course_id":"$C","after":5}`},
		{"a null the schema does not allow", "conversation_inbox", `{"course_id":"$C","limit":null}`,
			"POST", "/v1/tools/conversation.inbox", "", `{"course_id":"$C","limit":null}`},
		{"an object has no query form", "conversation_list", `{"course_id":"$C","state":{"a":1}}`,
			"POST", "/v1/tools/conversation.list", "", `{"course_id":"$C","state":{"a":1}}`},
		{"a missing path parameter", "conversation_answer", `{"course_id":"$C","body":"hi","idempotency_key":"k"}`,
			"POST", "/v1/tools/conversation.answer", "k", `{"course_id":"$C","body":"hi"}`},
		{"a path parameter that is not a string", "conversation_get", `{"course_id":5,"conversation_id":"$X"}`,
			"POST", "/v1/tools/conversation.get", "", `{"course_id":5,"conversation_id":"$X"}`},
		{"an empty path parameter", "conversation_get", `{"course_id":"","conversation_id":"$X"}`,
			"POST", "/v1/tools/conversation.get", "", `{"course_id":"","conversation_id":"$X"}`},
		{"a path parameter that would change the path", "document_get", `{"course_id":"$C","document_id":".."}`,
			"POST", "/v1/tools/document.get", "", `{"course_id":"$C","document_id":".."}`},
		{"and its shorter form", "document_get", `{"course_id":"$C","document_id":"."}`,
			"POST", "/v1/tools/document.get", "", `{"course_id":"$C","document_id":"."}`},
		{"dots within a value are only text", "document_get", `{"course_id":"$C","document_id":"../x"}`,
			"GET", "/v1/courses/$C/documents/..%2Fx", "", ""},
		{"a key that is not a string stays in the body for Core to refuse", "conversation_close",
			`{"course_id":"$C","conversation_id":"$X","idempotency_key":7}`,
			"POST", "/v1/courses/$C/conversations/$X/close", "", `{"idempotency_key":7}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl, ok := cat.Tool(tc.tool)
			if !ok {
				t.Fatalf("no %s", tc.tool)
			}
			r, err := buildRequest(tl, json.RawMessage(sub(tc.args)))
			if err != nil {
				t.Fatal(err)
			}
			if r.method != tc.method || r.path != sub(tc.path) || r.key != sub(tc.key) {
				t.Errorf("got %s %s key %q, want %s %s key %q", r.method, r.path, r.key, tc.method, sub(tc.path), sub(tc.key))
			}
			if tc.body == "" && r.body != nil || tc.body != "" && string(r.body) != sub(tc.body) {
				t.Errorf("body %s, want %s", r.body, sub(tc.body))
			}
		})
	}
}

func TestRESTRefusesArgumentsItCannotSend(t *testing.T) {
	cat := testCatalogue(t)
	answer, _ := cat.Tool("conversation_answer")
	for _, args := range []string{`[1]`, `"x"`, `{"a":1,"a":2}`, `{"a":1} {}`, `{"a":`, `{"idempotency_key":"a\nb"}`,
		// A header's value comes to Core without its leading and trailing
		// blanks: another key than the one given.
		`{"idempotency_key":" answer:x:m:1"}`, `{"idempotency_key":"answer:x:m:1\t"}`} {
		_, err := buildRequest(answer, json.RawMessage(args))
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Errorf("%s: got %v", args, err)
		}
	}
	c := NewRESTCaller(RESTOptions{BaseURL: "http://127.0.0.1:1", Token: testToken, Catalogue: cat})
	var pe *ProtocolError
	if _, err := c.Call(context.Background(), "no_such_tool", nil); !errors.As(err, &pe) {
		t.Errorf("an unknown tool: %v", err)
	}
	if _, err := NewRESTCaller(RESTOptions{BaseURL: "http://127.0.0.1:1"}).Call(context.Background(), "me_get", nil); !errors.As(err, &pe) {
		t.Errorf("no catalogue: %v", err)
	}
}

// restSeen is one request the fake REST Core took.
type restSeen struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

func newFakeREST(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []restSeen) {
	var mu sync.Mutex
	var seen []restSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, restSeen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(body)})
		mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []restSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]restSeen(nil), seen...)
	}
}

func TestRESTSends(t *testing.T) {
	srv, seen := newFakeREST(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "executed", "result": map[string]any{}})
	})
	c := NewRESTCaller(RESTOptions{BaseURL: srv.URL + "/", Token: testToken, Catalogue: testCatalogue(t)})
	ctx := context.Background()
	if _, err := c.Call(ctx, "conversation_answer", json.RawMessage(`{"course_id":"C","conversation_id":"X","in_reply_to_message_id":"M","body":"hi","idempotency_key":"answer:X:M:1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "conversation_inbox", json.RawMessage(`{"course_id":"C","limit":5}`)); err != nil {
		t.Fatal(err)
	}
	got := seen()
	post, get := got[0], got[1]
	if post.Method != "POST" || post.Path != "/v1/courses/C/conversations/X/answer" || post.Body != `{"in_reply_to_message_id":"M","body":"hi"}` ||
		post.Header.Get("Idempotency-Key") != "answer:X:M:1" || post.Header.Get("Content-Type") != "application/json" ||
		post.Header.Get("Authorization") != "Bearer "+testToken || post.Header.Get("Accept") != "application/json" {
		t.Errorf("the POST: %+v", post)
	}
	if get.Method != "GET" || get.Path != "/v1/courses/C/conversations/inbox" || get.Query != "limit=5" || get.Body != "" ||
		get.Header.Get("Content-Type") != "" || get.Header.Get("Idempotency-Key") != "" || get.Header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("the GET: %+v", get)
	}
}

func TestRESTAnswers(t *testing.T) {
	cat := testCatalogue(t)
	body := func(status int, v string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, v)
		}
	}
	envelope := func(status Status, code, reason string) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, env *Envelope, err error) {
			t.Helper()
			if err != nil || env.Status != status || env.Code() != code || env.Reason() != reason {
				t.Fatalf("got %+v %v, want %s %s %s", env, err, status, code, reason)
			}
		}
	}
	transient := func(status int) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, env *Envelope, err error) {
			t.Helper()
			var te *TransientError
			if !errors.As(err, &te) || te.Status != status || env != nil {
				t.Fatalf("got %v %v, want a *TransientError %d", env, err, status)
			}
		}
	}
	protocol := func(t *testing.T, env *Envelope, err error) {
		t.Helper()
		var pe *ProtocolError
		if !errors.As(err, &pe) || env != nil {
			t.Fatalf("got %v %v, want a *ProtocolError", env, err)
		}
	}
	limited := func(want time.Duration) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, _ *Envelope, err error) {
			t.Helper()
			var rl *RateLimitedError
			if !errors.As(err, &rl) || rl.RetryAfter != want {
				t.Fatalf("got %v, want rate limited for %s", err, want)
			}
		}
	}
	tooMany := `{"error":{"code":"rate_limited","message":"too many calls; try again in 4 seconds","details":{"retry_after_seconds":4}}}`
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request)
		check  func(*testing.T, *Envelope, error)
	}{
		{"200 executed", body(200, `{"status":"executed","action_id":"a1","review_state":"pending","result":{"message_id":"m2"}}`),
			func(t *testing.T, env *Envelope, err error) {
				envelope(StatusExecuted, "", "")(t, env, err)
				if env.ActionID != "a1" || env.ReviewState != ReviewPending {
					t.Fatalf("got %+v", env)
				}
			}},
		{"202 proposed", body(202, `{"status":"proposed","action_id":"a1","review_state":"none"}`), envelope(StatusProposed, "", "")},
		{"403 denied", body(403, `{"status":"denied","action_id":"a1","error":{"code":"forbidden","message":"no"}}`), envelope(StatusDenied, CodeForbidden, "")},
		{"409 already answered", body(409, `{"status":"failed","action_id":"a1","error":{"code":"conflict","message":"answered","details":{"reason":"already_answered"}}}`),
			envelope(StatusFailed, CodeConflict, ReasonAlreadyAnswered)},
		{"409 replayed rejected", body(409, `{"status":"rejected","action_id":"a1","review_state":"none","replayed":true}`),
			func(t *testing.T, env *Envelope, err error) {
				envelope(StatusRejected, "", "")(t, env, err)
				if !env.Replayed {
					t.Fatal("not replayed")
				}
			}},
		{"409 idempotency_conflict, never attempted", body(409, `{"error":{"code":"idempotency_conflict","message":"used before","details":{"action_id":"a0"}}}`),
			func(t *testing.T, env *Envelope, err error) {
				envelope(StatusError, CodeIdempotencyConflict, "")(t, env, err)
				if env.Detail("action_id") != "a0" {
					t.Fatalf("details: %+v", env.Error)
				}
			}},
		{"400 invalid_argument", body(400, `{"error":{"code":"invalid_argument","message":"body is required"}}`), envelope(StatusError, CodeInvalidArgument, "")},
		{"404 not_found", body(404, `{"error":{"code":"not_found","message":"no such conversation"}}`), envelope(StatusError, CodeNotFound, "")},
		{"500 internal, as MCP gives it", body(500, `{"error":{"code":"internal","message":"something went wrong on our side"}}`), envelope(StatusError, CodeInternal, "")},
		{"500 with a recorded outcome", body(500, `{"status":"failed","action_id":"a1","error":{"code":"odd","message":"?"}}`), envelope(StatusFailed, "odd", "")},
		{"503 with a status but no action: not Core's", body(503, `{"status":"error","message":"upstream unavailable"}`), transient(503)},
		{"401", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="aishiteru"`)
			body(401, `{"error":{"code":"unauthenticated","message":"the credential is missing or not valid"}}`)(w, nil)
		}, func(t *testing.T, env *Envelope, err error) {
			if !errors.Is(err, ErrUnauthenticated) || env != nil {
				t.Fatalf("got %v %v", env, err)
			}
		}},
		{"429 with Retry-After", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "4")
			body(429, tooMany)(w, nil)
		}, limited(4 * time.Second)},
		{"429, the body's", body(429, tooMany), limited(4 * time.Second)},
		{"429 with neither", body(429, `{}`), limited(time.Second)},
		{"502 from a proxy", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(502)
			_, _ = io.WriteString(w, "<html>Bad Gateway</html>")
		}, transient(502)},
		{"503 whose code is not Core's", body(503, `{"error":{"code":"unavailable","message":"maintenance"}}`), transient(503)},
		{"504, no body", body(504, ``), transient(504)},
		{"200 without a status", body(200, `{"error":{"code":"internal","message":"?"}}`), protocol},
		{"200 not JSON", body(200, `<html></html>`), protocol},
		{"400 not JSON", body(400, `Bad Request`), protocol},
		{"200 cut short", body(200, `{"status":"exec`), transient(200)},
		{"body cut short", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":`)
		}, transient(200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newFakeREST(t, tc.answer)
			c := NewRESTCaller(RESTOptions{BaseURL: srv.URL, Token: testToken, Catalogue: cat})
			env, err := c.Call(context.Background(), "conversation_answer",
				json.RawMessage(`{"course_id":"C","conversation_id":"X","in_reply_to_message_id":"M","body":"hi","idempotency_key":"k"}`))
			tc.check(t, env, err)
			noSecret(t, err)
		})
	}
}

func TestRESTTransportErrors(t *testing.T) {
	cat := testCatalogue(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	var te *TransientError
	if _, err := NewRESTCaller(RESTOptions{BaseURL: url, Token: testToken, Catalogue: cat}).Call(context.Background(), "me_get", nil); !errors.As(err, &te) {
		t.Fatalf("nothing listening: %v", err)
	}

	release := make(chan struct{})
	slow, _ := newFakeREST(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := NewRESTCaller(RESTOptions{BaseURL: slow.URL, Token: testToken, Catalogue: cat}).Call(ctx, "me_get", nil)
	if !errors.As(err, &te) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}

	for _, status := range []int{400, 500, 502} {
		echo, _ := newFakeREST(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, "you said %s", r.Header.Get("Authorization"))
		})
		_, err := NewRESTCaller(RESTOptions{BaseURL: echo.URL, Token: testToken, Catalogue: cat}).Call(context.Background(), "me_get", nil)
		if err == nil {
			t.Fatalf("HTTP %d: no error", status)
		}
		noSecret(t, err)
	}

	big, _ := newFakeREST(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "executed", "result": strings.Repeat("x", 4096)})
	})
	var pe *ProtocolError
	_, err = NewRESTCaller(RESTOptions{BaseURL: big.URL, Token: testToken, Catalogue: cat, MaxResponseBytes: 1024}).Call(context.Background(), "me_get", nil)
	if !errors.As(err, &pe) {
		t.Fatalf("too large: %v", err)
	}
}

// A redirect is never followed: the call it would make is not the call
// asked for.
func TestRESTFollowsNoRedirect(t *testing.T) {
	cat := testCatalogue(t)
	srv, seen := newFakeREST(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			http.Redirect(w, r, "/v1/me/memberships", http.StatusFound)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "executed", "result": map[string]any{}})
	})
	follows := srv.Client()
	for name, client := range map[string]*http.Client{"the default client": nil, "a client that follows": follows} {
		c := NewRESTCaller(RESTOptions{BaseURL: srv.URL, Token: testToken, Catalogue: cat, HTTPClient: client})
		var pe *ProtocolError
		if _, err := c.Call(context.Background(), "me_get", nil); !errors.As(err, &pe) || !strings.Contains(pe.Message, "redirected") {
			t.Errorf("%s: got %v", name, err)
		}
	}
	for _, r := range seen() {
		if r.Path != "/v1/me" {
			t.Errorf("the redirect was followed to %s", r.Path)
		}
	}
	if n := len(seen()); n != 2 {
		t.Errorf("%d requests, want /v1/me twice", n)
	}
	if follows.CheckRedirect != nil {
		t.Error("the caller's own client was changed")
	}
}
