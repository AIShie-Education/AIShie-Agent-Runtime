package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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

// listsAt is a catalogue of OpenRouter answering each path with its body
// in bodies, and 404 elsewhere.
func listsAt(t *testing.T, bodies map[string]string) *Catalogue {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewCatalogue(CatalogueOptions{BaseURL: srv.URL, Client: srv.Client()})
}

// entries is a JSON list of n entries, each entry(i).
func entries(n int, entry func(i int) string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(entry(i))
	}
	b.WriteByte(']')
	return b.String()
}

// An answer of more entries than OpenRouter lists, though within MaxBody,
// is refused as the entry past the bound is reached: a model's list is
// OpenRouter unavailable, /providers and /endpoints/zdr are as if not
// read. Each bound itself is read, and a provider is found by its slug's
// base, or by its name in any case, the first of each, however many
// there are.
func TestCatalogueBoundsEntries(t *testing.T) {
	const model = "/models/a/b/endpoints"
	tag := func(i int) string { return fmt.Sprintf(`{"tag":"t%d"}`, i) }
	// As a hostile answer had it: some 472,000 endpoints and 283,000
	// providers, each body just within MaxBody.
	huge := `{"data":{"id":"a/b","endpoints":` + entries(472201, tag) + `}}`
	hugeProviders := `{"data":` + entries(283322, func(i int) string { return fmt.Sprintf(`{"slug":"p%d"}`, i) }) + `}`
	if len(huge) > MaxBody || len(hugeProviders) > MaxBody {
		t.Fatalf("the bodies are %d and %d bytes, past MaxBody", len(huge), len(hugeProviders))
	}
	c := listsAt(t, map[string]string{model: huge, "/providers": hugeProviders, "/endpoints/zdr": `{"data":[]}`})
	_, err := c.Endpoints(context.Background(), "a/b")
	var u *UnavailableError
	if !errors.As(err, &u) || u.HTTPStatus != http.StatusOK || !strings.Contains(err.Error(), fmt.Sprintf("more than %d entries", MaxEndpoints)) {
		t.Fatalf("%d endpoints: %v", 472201, err)
	}
	if c.providers != nil {
		t.Errorf("%d providers were kept", 283322)
	}

	// MaxEndpoints endpoints, MaxProviders providers, and MaxZDR ZDR
	// endpoints are read; the last endpoint's provider is the last
	// provider, by its name in another case; and one more of each list
	// is refused.
	providers := func(n int) string {
		return `{"data":` + entries(n, func(i int) string { return fmt.Sprintf(`{"slug":"p%d","name":"Provider %d"}`, i, i) }) + `}`
	}
	endpoints := func(n int) string {
		return `{"data":{"id":"a/b","endpoints":` + entries(n, func(i int) string {
			return fmt.Sprintf(`{"tag":"t%d","provider_name":"PROVIDER %d"}`, i, i+MaxProviders-MaxEndpoints)
		}) + `}}`
	}
	zdr := func(n int) string {
		return `{"data":` + entries(n, func(i int) string { return fmt.Sprintf(`{"model_id":"a/b","tag":"t%d"}`, i+MaxEndpoints-1) }) + `}`
	}
	c = listsAt(t, map[string]string{model: endpoints(MaxEndpoints), "/providers": providers(MaxProviders), "/endpoints/zdr": zdr(MaxZDR)})
	l, err := c.Endpoints(context.Background(), "a/b")
	if err != nil || len(l.Endpoints) != MaxEndpoints {
		t.Fatalf("%d endpoints at the bound: %v", MaxEndpoints, err)
	}
	last := l.Endpoints[MaxEndpoints-1]
	if last.Provider == nil || *last.Provider != fmt.Sprintf("p%d", MaxProviders-1) || last.ZDR == nil || !*last.ZDR {
		t.Errorf("the last endpoint at the bounds: provider %v, ZDR %v", last.Provider, last.ZDR)
	}
	c = listsAt(t, map[string]string{model: endpoints(MaxEndpoints + 1), "/providers": providers(MaxProviders + 1), "/endpoints/zdr": zdr(MaxZDR + 1)})
	if _, err := c.Endpoints(context.Background(), "a/b"); !errors.As(err, &u) {
		t.Errorf("%d endpoints: %v", MaxEndpoints+1, err)
	}
	c = listsAt(t, map[string]string{model: endpoints(1), "/providers": providers(MaxProviders + 1), "/endpoints/zdr": zdr(MaxZDR + 1)})
	if l, err := c.Endpoints(context.Background(), "a/b"); err != nil || l.Endpoints[0].Provider != nil || l.Endpoints[0].ZDR != nil {
		t.Errorf("%d providers and %d ZDR endpoints: %+v, %v", MaxProviders+1, MaxZDR+1, l, err)
	}

	// The slug's base comes before a name, and the first of each before
	// the others.
	c = listsAt(t, map[string]string{
		model: `{"data":{"id":"a/b","endpoints":[{"tag":"groq/eu","provider_name":"Groq"},{"tag":"other","provider_name":"GROQ"},` +
			`{"tag":"twice","provider_name":"Twice"}]}}`,
		"/providers": `{"data":[{"slug":"g","name":"groq"},{"slug":"groq","name":"Something"},{"slug":"groq","name":"Later"},` +
			`{"slug":"twice-1","name":"Twice"},{"slug":"twice-2","name":"twice"}]}`,
		"/endpoints/zdr": `{"data":[]}`,
	})
	l, err = c.Endpoints(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"groq", "g", "twice-1"} {
		if got := l.Endpoints[i].Provider; got == nil || *got != want {
			t.Errorf("%s's provider: %v, want %s", l.Endpoints[i].Slug, got, want)
		}
	}
}

// Of what an answer lists, only what can be shown is kept: an endpoint
// whose tag order, only and ignore could not name is left out; a name or
// an id past MaxText, a link past MaxURL, a quantization not a word and a
// provider whose slug is not one are not kept; a datacenter is kept once.
func TestCatalogueKeepsWhatCanBeShown(t *testing.T) {
	long := strings.Repeat("x", MaxText+1)
	link := `"https://example.com/` + strings.Repeat("p", MaxURL) + `"`
	c := listsAt(t, map[string]string{
		"/models/a/b/endpoints": `{"data":{"id":"a/b","name":"` + long + `","endpoints":[` +
			`{"tag":""},{"tag":"Groq"},{"tag":"groq eu"},{"tag":"` + strings.Repeat("g", MaxSlug+1) + `"},` +
			`{"tag":"groq","provider_name":"` + long + `","quantization":"fp8","model_id":"` + long + `"},` +
			`{"tag":"novita/bf16","provider_name":"Novita","quantization":"FP 8!"},` +
			`{"tag":"together","provider_name":"Together","quantization":"` + strings.Repeat("q", 33) + `"}]}}`,
		"/providers": `{"data":[{"slug":"Novita","name":"Novita"},{"slug":"novita","name":"N","datacenters":["US","DE","US","us"],` +
			`"privacy_policy_url":` + link + `,"terms_of_service_url":"https://novita.ai/terms"},{"slug":"together","name":"` + long + `"}]}`,
		"/endpoints/zdr": `{"data":[{"model_id":"a/b","tag":"groq"},{"model_id":"","tag":"together"}]}`,
	})
	l, err := c.Endpoints(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if l.Name != "" || len(l.Endpoints) != 3 {
		t.Fatalf("name %q, %d endpoints", l.Name, len(l.Endpoints))
	}
	groq, novita, together := l.Endpoints[0], l.Endpoints[1], l.Endpoints[2]
	if groq.Slug != "groq" || groq.ProviderName != "" || groq.Quantization != "fp8" || groq.ZDR == nil || !*groq.ZDR {
		t.Errorf("groq: %+v", groq)
	}
	if novita.Quantization != "unknown" || novita.Provider == nil || *novita.Provider != "novita" ||
		!slices.Equal(novita.Datacenters, []string{"US", "DE"}) || novita.PrivacyPolicyURL != nil || novita.TermsOfServiceURL == nil {
		t.Errorf("novita: %+v", novita)
	}
	if together.Quantization != "unknown" || together.Provider == nil || *together.Provider != "together" || together.ZDR == nil || *together.ZDR {
		t.Errorf("together: %+v", together)
	}
	c = listsAt(t, map[string]string{"/models/a/b/endpoints": `{"data":{"id":"` + long + `","endpoints":[]}}`})
	var u *UnavailableError
	if _, err := c.Endpoints(context.Background(), "a/b"); !errors.As(err, &u) {
		t.Errorf("an id past MaxText: %v", err)
	}
}

// Two names have one key exactly when strings.EqualFold holds of them.
func TestFoldKey(t *testing.T) {
	names := []string{"Groq", "GROQ", "groq", "Grоq" /* a Cyrillic o */, "ſ", "S", "s", "K", "K", "k", "ǅ", "Ǆ", "ǆ", "Straße", "STRASSE", "", "\xff", "�"}
	for _, a := range names {
		for _, b := range names {
			if got, want := foldKey(a) == foldKey(b), strings.EqualFold(a, b); got != want {
				t.Errorf("%q and %q: one key %v, EqualFold %v", a, b, got, want)
			}
		}
	}
}
