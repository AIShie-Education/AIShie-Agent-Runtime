package gemini

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// testKey is shaped like a real key, so that a test finding it in an error
// or a URL means something.
const testKey = "AIzaSyTEST-not-a-real-key-0123456789abc"

// testMaker is the Maker of the adapter newAdapter builds by default.
const testMaker = "gemini|https://generativelanguage.googleapis.com|gemini-2.5-flash"

// answer is one reply of the fake Gemini.
type answer struct {
	status int
	header map[string]string
	body   string
}

func ok(body string) answer { return answer{status: http.StatusOK, body: body} }

// recorded is one request the fake Gemini received.
type recorded struct {
	method string
	url    string
	header http.Header
	body   []byte
}

// fakeGemini stands in for generativelanguage.googleapis.com. Adapters are
// built with the default base URL and a transport that sends every request
// here, so that the URL checked is the one the adapter built. It gives its
// answers in order, and the last one again after that.
type fakeGemini struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []recorded
	answers []answer
	// respond, when set, answers each request by its body instead.
	respond func(body []byte) answer
}

// originalURL carries, from the redirecting transport to the handler, the
// URL the adapter asked for.
const originalURL = "X-Test-Original-Url"

func newFake(t *testing.T, answers ...answer) *fakeGemini {
	t.Helper()
	f := &fakeGemini{answers: answers}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGemini) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h := r.Header.Clone()
	h.Del(originalURL)
	f.mu.Lock()
	n := len(f.got)
	f.got = append(f.got, recorded{method: r.Method, url: r.Header.Get(originalURL), header: h, body: body})
	a := answer{status: http.StatusOK, body: "{}"}
	if len(f.answers) > 0 {
		a = f.answers[min(n, len(f.answers)-1)]
	}
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		a = respond(body)
	}
	for k, v := range a.header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.status)
	_, _ = io.WriteString(w, a.body)
}

// answerBy makes the fake answer each request by its body.
func (f *fakeGemini) answerBy(respond func(body []byte) answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

// requests is what the fake has received so far.
func (f *fakeGemini) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.got...)
}

// last is the latest request received.
func (f *fakeGemini) last(t *testing.T) recorded {
	t.Helper()
	got := f.requests()
	if len(got) == 0 {
		t.Fatal("the adapter sent nothing")
	}
	return got[len(got)-1]
}

// redirect sends every request to the fake, keeping the Host the adapter
// asked for and noting the whole URL in a header.
type redirect struct {
	target *url.URL
	next   http.RoundTripper
}

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set(originalURL, req.URL.String())
	out.URL.Scheme, out.URL.Host = r.target.Scheme, r.target.Host
	return r.next.RoundTrip(out)
}

// client is an HTTP client whose every request reaches the fake.
func (f *fakeGemini) client() *http.Client {
	target, _ := url.Parse(f.srv.URL)
	return &http.Client{Transport: redirect{target: target, next: f.srv.Client().Transport}}
}

// testConfig is the configuration tests start from: gemini-2.5-flash on
// the default endpoint, through the fake.
func (f *fakeGemini) testConfig() llm.Config {
	return llm.Config{Adapter: llm.AdapterGemini, Model: "gemini-2.5-flash", APIKey: testKey, HTTPClient: f.client()}
}

// newAdapter builds an adapter on the fake, with cfg changed by each of
// change.
func (f *fakeGemini) newAdapter(t *testing.T, change ...func(*llm.Config)) *Adapter {
	t.Helper()
	cfg := f.testConfig()
	for _, c := range change {
		if c != nil {
			c(&cfg)
		}
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// checkGolden compares got with the golden file at path, as canonical JSON,
// or writes it there under -update.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	got = canonical(t, got)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test -update writes it)", err)
	}
	if want = canonical(t, want); !bytes.Equal(want, got) {
		t.Errorf("%s is not what the adapter made (go test -update rewrites it)\n--- want\n%s--- got\n%s", path, want, got)
	}
}

// canonical is JSON with sorted keys, two-space indents and numbers as
// written.
func canonical(t *testing.T, b []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wireBody decodes a request body the adapter sent.
func wireBody(t *testing.T, r recorded) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("the adapter sent something that is not JSON: %v\n%s", err, r.body)
	}
	return v
}
