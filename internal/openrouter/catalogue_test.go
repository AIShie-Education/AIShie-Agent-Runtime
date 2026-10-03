package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stub is OpenRouter for the catalogue's tests: a model's endpoints for
// any model, the lists, each call counted by path; a call of a model held
// while hold is open.
type stub struct {
	mu    sync.Mutex
	calls map[string]int
	hold  chan struct{}
	block bool // a model's call waits until its request is done
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[r.URL.Path]++
	hold, block := s.hold, s.block
	s.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/endpoints") && strings.HasPrefix(r.URL.Path, "/models/"):
		if hold != nil {
			<-hold
		}
		if block {
			<-r.Context().Done()
			return
		}
		model := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/models/"), "/endpoints")
		fmt.Fprintf(w, `{"data":{"id":%q,"name":"M","endpoints":[{"tag":"groq","provider_name":"Groq","model_id":%[1]q,"pricing":{"prompt":"0.00000059","completion":"0.00000079"}}]}}`, model)
	case r.URL.Path == "/providers":
		fmt.Fprint(w, `{"data":[{"name":"Groq","slug":"groq","headquarters":"US","datacenters":null}]}`)
	case r.URL.Path == "/endpoints/zdr":
		fmt.Fprint(w, `{"data":[]}`)
	default:
		http.NotFound(w, r)
	}
}

func (s *stub) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// waitFor waits until cond holds, failing after a generous while.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited in vain for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Callers that want one model's list at once share one call to
// OpenRouter, and each has the list; a list kept is answered without a
// call.
func TestCatalogueSharesOneCall(t *testing.T) {
	st := &stub{hold: make(chan struct{})}
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	c := NewCatalogue(CatalogueOptions{BaseURL: srv.URL, Client: srv.Client()})
	const callers = 6
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range callers {
		wg.Go(func() {
			l, err := c.Endpoints(context.Background(), "meta-llama/llama-3.3-70b-instruct")
			if err == nil && len(l.Endpoints) == 1 && l.Endpoints[0].Slug == "groq" {
				ok.Add(1)
			}
		})
	}
	waitFor(t, "every caller to wait on the one call", func() bool {
		return c.flights.waiting("model\x00meta-llama/llama-3.3-70b-instruct") == callers
	})
	close(st.hold)
	wg.Wait()
	if ok.Load() != callers || st.count("/models/meta-llama/llama-3.3-70b-instruct/endpoints") != 1 {
		t.Fatalf("%d callers had the list, from %d calls", ok.Load(), st.count("/models/meta-llama/llama-3.3-70b-instruct/endpoints"))
	}
	if _, err := c.Endpoints(context.Background(), "META-LLAMA/llama-3.3-70b-instruct"); err != nil ||
		st.count("/models/meta-llama/llama-3.3-70b-instruct/endpoints") != 1 || st.count("/providers") != 1 {
		t.Errorf("a list kept, asked again in another case: %v, %d calls", err, st.count("/models/meta-llama/llama-3.3-70b-instruct/endpoints"))
	}
}

// A call that takes too long is OpenRouter unavailable, with no status;
// a caller whose own request ends first is answered at once, and the call
// it began goes on for the others.
func TestCatalogueTimesOut(t *testing.T) {
	st := &stub{block: true}
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	c := NewCatalogue(CatalogueOptions{BaseURL: srv.URL, Client: srv.Client()})
	c.callTimeout = 50 * time.Millisecond
	_, err := c.Endpoints(context.Background(), "a/b")
	var u *UnavailableError
	if !errors.As(err, &u) || u.HTTPStatus != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	// The call outlives the caller that began it, and ends at its own
	// timeout for the one still waiting.
	c.callTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Endpoints(ctx, "a/c")
		first <- err
	}()
	waitFor(t, "the first caller", func() bool { return c.flights.waiting("model\x00a/c") == 1 })
	go func() {
		_, err := c.Endpoints(context.Background(), "a/c")
		second <- err
	}()
	waitFor(t, "the second caller", func() bool { return c.flights.waiting("model\x00a/c") == 2 })
	cancel()
	if err := <-first; !errors.As(err, &u) || !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller gone: %v", err)
	}
	if err := <-second; !errors.As(err, &u) || !errors.Is(err, context.DeadlineExceeded) || st.count("/models/a/c/endpoints") != 1 {
		t.Fatalf("the caller still waiting: %v, %d calls", err, st.count("/models/a/c/endpoints"))
	}
}

// At most MaxModels models' lists are kept, the oldest read dropped
// first; a model OpenRouter no longer has is dropped as it says so.
func TestCatalogueBounds(t *testing.T) {
	st := &stub{}
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	c := NewCatalogue(CatalogueOptions{BaseURL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }})
	for i := range MaxModels + 1 {
		now = now.Add(time.Second)
		if _, err := c.Endpoints(context.Background(), fmt.Sprintf("a/m%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.models) != MaxModels || c.models["a/m0"] != nil || c.models["a/m1"] == nil {
		t.Fatalf("%d kept; the first %v, the second %v", len(c.models), c.models["a/m0"] != nil, c.models["a/m1"] != nil)
	}
	now = now.Add(FreshFor)
	c.o.BaseURL = srv.URL + "/gone" // every call a 404
	if _, err := c.Endpoints(context.Background(), "a/m1"); !errors.Is(err, ErrModelNotFound) || c.models["a/m1"] != nil {
		t.Fatalf("a model gone: %v", err)
	}
}

// A price in dollars a unit is pUSD, exactly where it has at most twelve
// places, rounded beyond; none, a negative one and one that does not
// read are nil.
func TestPUSD(t *testing.T) {
	for in, want := range map[string]any{
		`"0.0000001"`: int64(100000), `"0.00000032"`: int64(320000), `"0"`: int64(0), `0.0005`: int64(500000000),
		`"1e-7"`: int64(100000), `"0.00000431137724550898"`: int64(4311377), `"0.0000000000005"`: int64(1),
		`null`: nil, `"-1"`: nil, `"abc"`: nil, `"1/3"`: nil, `"1e999"`: nil, `true`: nil, `"99999999999"`: nil,
	} {
		got := pusd(json.RawMessage(in))
		switch w := want.(type) {
		case nil:
			if got != nil {
				t.Errorf("%s: %d, want none", in, *got)
			}
		case int64:
			if got == nil || *got != w {
				t.Errorf("%s: %v, want %d", in, got, w)
			}
		}
	}
}
