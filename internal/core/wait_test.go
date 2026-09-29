package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit/ratelimittest"
)

// TestCatalogueMaxWait reads wait_s from the catalogue Core serves: the
// snapshot's three reads that wait take up to 25 s, other tools none, and a
// Core from before them none.
func TestCatalogueMaxWait(t *testing.T) {
	c := testCatalogue(t)
	for _, name := range []string{"conversation_inbox", "conversation_messages", "event_list"} {
		if got := c.MaxWait(name); got != 25*time.Second {
			t.Errorf("%s waits up to %s, want 25s", name, got)
		}
	}
	for _, name := range []string{"me_get", "conversation_get", "no_such_tool"} {
		if got := c.MaxWait(name); got != 0 {
			t.Errorf("%s waits up to %s", name, got)
		}
	}
	if (*Catalogue)(nil).MaxWait("conversation_inbox") != 0 {
		t.Error("no catalogue waits")
	}
	for schema, want := range map[string]time.Duration{
		`{"type":"object","properties":{"course_id":{"type":"string"}}}`:                         0,
		`{"type":"object","properties":{"wait_s":{"type":"integer","minimum":0,"maximum":10}}}`:  10 * time.Second,
		`{"type":"object","properties":{"wait_s":{"type":"integer","minimum":0,"maximum":300}}}`: MaxWait,
		`{"type":"object","properties":{"wait_s":{"type":"integer"}}}`:                           MaxWait,
		`{"type":"object","properties":{"wait_s":{"type":"integer","maximum":0}}}`:               0,
		`{"type":"object","properties":{"wait_s":{"type":"string"}}}`:                            0,
		`{"type":"object","properties":{"wait_s":{"type":["null","integer"],"maximum":5}}}`:      5 * time.Second,
	} {
		raw := `{"tools":[{"name":"conversation.inbox","kind":"read","method":"GET","path":"/x","input_schema":` + schema + `,"output_schema":{}}]}`
		cat, err := ParseCatalogue([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := cat.MaxWait("conversation_inbox"); got != want {
			t.Errorf("%s: %s, want %s", schema, got, want)
		}
	}
}

// TestInboxAndEventsWait: Inbox and Events send wait_s, in whole seconds up
// to 25, and mark the call as one that waits; with no wait, neither.
func TestInboxAndEventsWait(t *testing.T) {
	type seen struct {
		tool string
		args map[string]any
		wait time.Duration
	}
	calls := make(chan seen, 8)
	c := NewClient(callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
		var m map[string]any
		_ = json.Unmarshal(args, &m)
		calls <- seen{tool, m, WaitOf(ctx)}
		return &Envelope{Status: StatusExecuted, Result: json.RawMessage(`{}`)}, nil
	}))
	ctx := context.Background()
	for _, tc := range []struct {
		wait   time.Duration
		waitS  any
		marked time.Duration
	}{
		{0, nil, 0},
		{-time.Second, nil, 0},
		{25 * time.Second, 25.0, 25 * time.Second},
		{1500 * time.Millisecond, 2.0, 2 * time.Second},
		{time.Minute, 25.0, 25 * time.Second},
	} {
		if _, err := c.Inbox(ctx, "C", 20, tc.wait); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Events(ctx, "C", 7, 500, tc.wait); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			s := <-calls
			if s.args["wait_s"] != tc.waitS || s.wait != tc.marked {
				t.Errorf("%s with a wait of %s: wait_s %v, marked %s; want %v, %s", s.tool, tc.wait, s.args["wait_s"], s.wait, tc.waitS, tc.marked)
			}
		}
	}
}

type callerFunc func(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error)

func (f callerFunc) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	return f(ctx, tool, args)
}

// TestForCall gives a call that waits its wait plus WaitMargin where the
// client's timeout is shorter, and every other call the client as it is.
func TestForCall(t *testing.T) {
	base := &http.Client{Timeout: DefaultTimeout}
	ctx := context.Background()
	if c := forCall(ctx, base); c != base {
		t.Error("a call that does not wait was given another client")
	}
	long := forCall(WithWait(ctx, 25*time.Second), base)
	if long == base || long.Timeout != 40*time.Second || base.Timeout != DefaultTimeout {
		t.Errorf("a call waiting 25 s: timeout %s (the client's %s)", long.Timeout, base.Timeout)
	}
	if long.Transport != base.Transport {
		t.Error("the long poll's client does not share the transport")
	}
	for _, c := range []*http.Client{{Timeout: time.Minute}, {}} {
		if got := forCall(WithWait(ctx, 25*time.Second), c); got != c {
			t.Errorf("a client with a timeout of %s was given %s", c.Timeout, got.Timeout)
		}
	}
	if WaitOf(WithWait(ctx, -time.Second)) != 0 || WaitOf(ctx) != 0 {
		t.Error("WaitOf")
	}
}

// TestTimeoutsAllowLongPollsOnly: over both transports, a call that waits
// for news outlasts the client's own timeout by as long as it waits, and
// any other call still times out when the client says.
func TestTimeoutsAllowLongPollsOnly(t *testing.T) {
	const slow = 400 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"protocolVersion":"` + DefaultProtocol + `"}}`))
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-time.After(slow):
		case <-r.Context().Done():
			return
		}
		env := `{"status":"executed","result":{"conversations":[]}}`
		if req.Method == "tools/call" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"structuredContent":` + env + `}}`))
			return
		}
		_, _ = w.Write([]byte(env))
	}))
	t.Cleanup(srv.Close)
	hc := &http.Client{Timeout: slow / 4}
	callers := map[string]Caller{
		"mcp":  NewMCPCaller(MCPOptions{BaseURL: srv.URL, Token: testToken, HTTPClient: hc}),
		"rest": NewRESTCaller(RESTOptions{BaseURL: srv.URL, Token: testToken, HTTPClient: hc, Catalogue: testCatalogue(t)}),
	}
	for name, c := range callers {
		t.Run(name, func(t *testing.T) {
			client := NewClient(c)
			ctx := context.Background()
			_, err := client.Inbox(ctx, "C", 20, 0)
			var te *TransientError
			if !errors.As(err, &te) {
				t.Fatalf("a call that does not wait outlasted the client's timeout: %v", err)
			}
			if _, err := client.Inbox(ctx, "C", 20, time.Second); err != nil {
				t.Fatalf("a call that waits was cut short: %v", err)
			}
			if _, err := client.Events(ctx, "C", 0, 100, time.Second); err != nil {
				t.Fatalf("event_list that waits was cut short: %v", err)
			}
		})
	}
}

// TestRetryingAWaitingCall: a call that waits is not sent again after a
// transient failure or an error internal, which come back at once; after a
// 429 it is, and the agent is told to slow down.
func TestRetryingAWaitingCall(t *testing.T) {
	ctx := WithWait(context.Background(), 25*time.Second)
	for name, a := range map[string]answer{"transient": {err: transientErr}, "internal": {env: internalEnv}} {
		t.Run(name, func(t *testing.T) {
			next := &scripted{answers: []answer{a, {env: executedEnv}}}
			var clk clock
			env, err := NewRetrying(next, clk.options()).Call(ctx, "conversation_inbox", nil)
			if env != a.env || !errors.Is(err, a.err) || next.calls() != 1 || len(clk.sleeps) != 0 {
				t.Fatalf("got %v %v after %d calls and %d sleeps", env, err, next.calls(), len(clk.sleeps))
			}
		})
	}
	next := &scripted{answers: []answer{{err: &RateLimitedError{RetryAfter: 3 * time.Second}}, {env: executedEnv}}}
	var clk clock
	o := clk.options()
	told := 0
	o.OnRateLimited = func(time.Duration) { told++ }
	if env, err := NewRetrying(next, o).Call(ctx, "conversation_inbox", nil); err != nil || env != executedEnv || told != 1 || next.calls() != 2 {
		t.Fatalf("after a 429: %v %v, told %d, %d calls", env, err, told, next.calls())
	}
}

// TestLimitedLongPollIsOneCall: a call that waits takes one token of the
// agent's bucket and holds none while it waits: another call gets the
// next token meanwhile.
func TestLimitedLongPollIsOneCall(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(1, 2, clock)
	release := make(chan struct{})
	waiting := make(chan struct{})
	c := Limited(callerFunc(func(ctx context.Context, tool string, _ json.RawMessage) (*Envelope, error) {
		if WaitOf(ctx) > 0 {
			close(waiting)
			<-release
		}
		return &Envelope{Status: StatusExecuted}, nil
	}), b)
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(WithWait(WithPriority(context.Background(), PriorityPoll), 25*time.Second), "conversation_inbox", nil)
		done <- err
	}()
	<-waiting
	quick := make(chan error, 1)
	go func() {
		_, err := c.Call(WithPriority(context.Background(), PriorityAnswer), "conversation_messages", nil)
		quick <- err
	}()
	select {
	case err := <-quick:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call waited for a token while a long poll waited for news")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := b.Stats(); s.Granted != 2 || s.Waited != 0 || s.Tokens > 0.01 {
		t.Fatalf("the bucket after a long poll and a call: %+v", s)
	}
}
