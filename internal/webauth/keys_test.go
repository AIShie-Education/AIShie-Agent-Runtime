package webauth

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKeys serves a key set at KeysPath, as Core does, counting requests.
type fakeKeys struct {
	srv   *httptest.Server
	hits  atomic.Int32
	mu    sync.Mutex
	set   []jwk
	cc    string
	down  bool
	block chan struct{}
}

func newFakeKeys(t *testing.T, signers ...signer) *fakeKeys {
	t.Helper()
	f := &fakeKeys{cc: "public, max-age=300"}
	for _, s := range signers {
		f.set = append(f.set, jwkOf(s))
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != KeysPath {
			http.NotFound(w, r)
			return
		}
		f.hits.Add(1)
		f.mu.Lock()
		set, cc, down, block := f.set, f.cc, f.down, f.block
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", cc)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": set})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func jwkOf(s signer) jwk {
	return jwk{Kty: "OKP", Crv: "Ed25519", X: b64(s.public()), Kid: s.kid, Alg: "EdDSA", Use: "sig"}
}

func (f *fakeKeys) edit(fn func(f *fakeKeys)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// clock is a clock a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func remote(f *fakeKeys, c *clock) *RemoteKeys {
	k := NewRemoteKeys(f.srv.URL+"/", f.srv.Client())
	k.SetClock(c.now)
	return k
}

// The key set is fetched once and kept for as long as Core says; then
// fetched again. A kid it lacks fetches it again at once, but no more
// often than refetchEvery.
func TestRemoteKeysKeepsAndRefetches(t *testing.T) {
	ctx := context.Background()
	s, next := newSigner(t), newSigner(t)
	f := newFakeKeys(t, s)
	c := &clock{t: at}
	k := remote(f, c)
	for range 3 {
		if key, err := k.Key(ctx, s.kid); err != nil || !key.Equal(s.public()) {
			t.Fatalf("Key: %v", err)
		}
	}
	if n := f.hits.Load(); n != 1 {
		t.Errorf("%d fetches for three asks", n)
	}
	c.add(301 * time.Second)
	if _, err := k.Key(ctx, s.kid); err != nil || f.hits.Load() != 2 {
		t.Errorf("past its age: %v, %d fetches", err, f.hits.Load())
	}

	// Core signs with a new key: the first assertion that names it
	// fetches the set again.
	c.add(refetchEvery)
	f.edit(func(f *fakeKeys) { f.set = append(f.set, jwkOf(next)) })
	if key, err := k.Key(ctx, next.kid); err != nil || !key.Equal(next.public()) || f.hits.Load() != 3 {
		t.Errorf("a new key: %v, %d fetches", err, f.hits.Load())
	}
	// Made-up kids fetch it again at most every refetchEvery.
	for range 5 {
		if _, err := k.Key(ctx, "made-up"); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("a made-up kid: %v", err)
		}
	}
	if n := f.hits.Load(); n != 3 {
		t.Errorf("%d fetches after made-up kids within %s of the last; want 3", n, refetchEvery)
	}
	c.add(refetchEvery)
	_, _ = k.Key(ctx, "made-up")
	if n := f.hits.Load(); n != 4 {
		t.Errorf("%d fetches after %s; want 4", n, refetchEvery)
	}
}

// While Core cannot be reached, the set kept is used for staleFor past its
// age, and asked for again no more often than refetchEvery; after that, the
// keys are unavailable. Before any fetch has worked, they are unavailable
// at once.
func TestRemoteKeysWhileCoreIsDown(t *testing.T) {
	ctx := context.Background()
	s := newSigner(t)
	f := newFakeKeys(t, s)
	f.edit(func(f *fakeKeys) { f.down = true })
	c := &clock{t: at}
	k := remote(f, c)
	if _, err := k.Key(ctx, s.kid); !errors.Is(err, ErrKeysUnavailable) || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("before any fetch worked: %v", err)
	}
	if _, err := k.Key(ctx, s.kid); !errors.Is(err, ErrKeysUnavailable) || f.hits.Load() != 1 {
		t.Errorf("asked again at once: %v, %d fetches", err, f.hits.Load())
	}

	f.edit(func(f *fakeKeys) { f.down = false })
	c.add(refetchEvery)
	if _, err := k.Key(ctx, s.kid); err != nil {
		t.Fatal(err)
	}
	f.edit(func(f *fakeKeys) { f.down = true })
	c.add(5*time.Minute + time.Second)
	if _, err := k.Key(ctx, s.kid); err != nil {
		t.Errorf("past its age, Core down: %v", err)
	}
	c.add(staleFor)
	if _, err := k.Key(ctx, s.kid); !errors.Is(err, ErrKeysUnavailable) {
		t.Errorf("past staleFor: %v", err)
	}
}

// Callers that meet a fetch in progress wait for it: one fetch serves all.
func TestRemoteKeysFetchesOnceAtATime(t *testing.T) {
	s := newSigner(t)
	f := newFakeKeys(t, s)
	block := make(chan struct{})
	f.edit(func(f *fakeKeys) { f.block = block })
	k := remote(f, &clock{t: at})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := k.Key(context.Background(), s.kid)
			errs <- err
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := f.hits.Load(); n != 1 {
		t.Errorf("%d fetches for eight callers at once", n)
	}
}

// A redirect is not followed, and a body too large is not read.
func TestRemoteKeysRefusesWhatIsNotCores(t *testing.T) {
	elsewhere := newFakeKeys(t, newSigner(t))
	redirect := httptest.NewServer(http.RedirectHandler(elsewhere.srv.URL+KeysPath, http.StatusFound))
	t.Cleanup(redirect.Close)
	k := NewRemoteKeys(redirect.URL, redirect.Client())
	if _, err := k.Key(context.Background(), "k"); !errors.Is(err, ErrKeysUnavailable) || elsewhere.hits.Load() != 0 {
		t.Errorf("a redirect: %v, %d fetches elsewhere", err, elsewhere.hits.Load())
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[` + strings.Repeat(" ", maxKeySetBytes) + `]}`))
	}))
	t.Cleanup(big.Close)
	if _, err := NewRemoteKeys(big.URL, big.Client()).Key(context.Background(), "k"); !errors.Is(err, ErrKeysUnavailable) ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("a key set too large: %v", err)
	}
}

func TestParseKeySet(t *testing.T) {
	s, t2 := newSigner(t), newSigner(t)
	good := jwkOf(s)
	noAlg := jwkOf(t2)
	noAlg.Alg, noAlg.Use = "", ""
	rsa := jwk{Kty: "RSA", Kid: "rsa", X: good.X}
	x25519 := jwk{Kty: "OKP", Crv: "X25519", Kid: "x", X: good.X}
	enc := good
	enc.Kid, enc.Use = "enc", "enc"
	short := good
	short.Kid, short.X = "short", b64([]byte("short"))
	dup := jwkOf(t2)
	dup.Kid = s.kid
	raw, err := json.Marshal(map[string]any{"keys": []jwk{good, noAlg, rsa, x25519, enc, short, dup}})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseKeySet(raw)
	if err != nil || len(keys) != 2 || !keys[s.kid].Equal(s.public()) || !keys[t2.kid].Equal(t2.public()) {
		t.Errorf("keys %v, %v", keys, err)
	}
	for _, bad := range []string{`{"keys":[]}`, `{"keys":[{"kty":"RSA","kid":"r"}]}`, `[]`, `not json`} {
		if _, err := ParseKeySet([]byte(bad)); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
}

func TestMaxAge(t *testing.T) {
	for cc, want := range map[string]time.Duration{
		"public, max-age=300": 5 * time.Minute, "max-age=1": time.Second, "max-age=999999": maxMaxAge,
		"": maxMaxAge, "no-store": maxMaxAge, "max-age=x": maxMaxAge, `Max-Age="60"`: time.Minute,
	} {
		if got := maxAge(cc); got != want {
			t.Errorf("%q: %s, want %s", cc, got, want)
		}
	}
}

// A pinned key checks the assertions that name it by its thumbprint, as
// Core names its key; one that names another kid is refused, and the key
// is written as a JWK's x.
func TestPinnedKey(t *testing.T) {
	s := newSigner(t)
	p, err := ParsePinnedKey(b64(s.public()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier(p, at).Verify(context.Background(), s.sign(t, s.header(), claimsOf())); err != nil {
		t.Errorf("a pinned key: %v", err)
	}
	h := s.header()
	h["kid"] = "another"
	var e *Error
	if _, err := verifier(p, at).Verify(context.Background(), s.sign(t, h, claimsOf())); !errors.As(err, &e) || e.Reason != ReasonInvalid {
		t.Errorf("another kid: %v", err)
	}
	other := newSigner(t)
	if _, err := verifier(p, at).Verify(context.Background(), other.sign(t, s.header(), claimsOf())); !errors.As(err, &e) || e.Reason != ReasonInvalid {
		t.Errorf("another key's assertion: %v", err)
	}
	for _, bad := range []string{"", "short", b64(s.public()) + "=", " " + b64(s.public()), b64(make([]byte, ed25519.PublicKeySize+1))} {
		if _, err := ParsePinnedKey(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}
