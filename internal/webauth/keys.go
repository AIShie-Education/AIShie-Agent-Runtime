// Package webauth checks the assertions people bring to the runtime's API
// (docs/design.md §11.4): short-lived JWTs that Core signs with Ed25519 for
// the person signed in there, with this runtime as their audience (the
// product owner's D2). The runtime is no OIDC client and never sees a
// credential of Core's: an assertion is all it is given, and it is checked
// against the public key Core publishes at GET /v1/auth/keys, or against
// one the operator pins.
package webauth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KeysPath is where Core publishes the key set that checks its assertions.
const KeysPath = "/v1/auth/keys"

// Keys finds the public key an assertion's header names by its kid.
type Keys interface {
	// Key is the key kid names: ErrUnknownKey when there is none, and
	// ErrKeysUnavailable when the keys cannot be had at all.
	Key(ctx context.Context, kid string) (ed25519.PublicKey, error)
}

// ErrUnknownKey is a kid the key set does not have.
var ErrUnknownKey = errors.New("webauth: no key has that id")

// ErrKeysUnavailable is a key set that could not be fetched, and none kept
// from before that is still good enough to use.
var ErrKeysUnavailable = errors.New("webauth: Core's keys cannot be fetched")

// How RemoteKeys keeps Core's key set (the API contract, §3.2).
const (
	// maxMaxAge is the longest a key set is kept, and how long when Core
	// does not say; Core says five minutes.
	maxMaxAge = 5 * time.Minute
	// refetchEvery is how often an unknown kid, or a set past its age,
	// may send for the key set again: a key Core has just started
	// signing with is found at once, and assertions naming made-up kids
	// do not have the runtime ask Core at every request.
	refetchEvery = 30 * time.Second
	// staleFor is how long past its age a key set is still used while
	// Core cannot be reached, so that a moment of Core's being down does
	// not sign everyone out of the runtime.
	staleFor = time.Hour
	// maxKeySetBytes bounds the key set's body; Core's is a few hundred
	// bytes.
	maxKeySetBytes = 64 << 10
	// fetchTimeout bounds one fetch.
	fetchTimeout = 5 * time.Second
)

// RemoteKeys is Core's key set, fetched from its KeysPath and kept for as
// long as Core's Cache-Control says (bounded), fetched again at once, at
// most every refetchEvery, when an assertion names a key it does not have.
// One fetch is made at a time; the callers that meet it wait for it. It
// follows no redirect: the keys are the configured Core's.
type RemoteKeys struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu sync.Mutex
	// keys are the key set as last fetched, nil before the first fetch
	// that succeeded, and expires is when Core said it goes out of date.
	keys    map[string]ed25519.PublicKey
	expires time.Time
	// tried is when the last fetch was begun, whether or not it
	// succeeded; inflight is closed when the one in progress ends.
	tried    time.Time
	inflight chan struct{}
	// failed is why the last fetch failed, nil when it succeeded.
	failed error
}

// NewRemoteKeys fetches the key set from Core at baseURL through client
// (its transport and proxy; a timeout of its own is set on each fetch).
func NewRemoteKeys(baseURL string, client *http.Client) *RemoteKeys {
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &RemoteKeys{url: strings.TrimRight(baseURL, "/") + KeysPath, client: &c, now: time.Now}
}

// SetClock replaces the clock, for tests.
func (k *RemoteKeys) SetClock(now func() time.Time) { k.now = now }

// Key is the key of kid, from the key set as kept, or as fetched again: a
// fetch is made when the set kept is past its age or lacks kid, and none
// has been tried within refetchEvery. A caller that meets a fetch in
// progress waits for it, and makes none of its own.
func (k *RemoteKeys) Key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	k.mu.Lock()
	now := k.now()
	if key, ok := k.keys[kid]; ok && now.Before(k.expires) {
		k.mu.Unlock()
		return key, nil
	}
	if wait := k.inflight; wait != nil {
		k.mu.Unlock()
		select {
		case <-wait:
			return k.kept(kid)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !k.tried.IsZero() && now.Sub(k.tried) < refetchEvery {
		k.mu.Unlock()
		return k.kept(kid)
	}
	done := make(chan struct{})
	k.inflight, k.tried = done, now
	k.mu.Unlock()

	keys, age, err := k.fetch(ctx)

	k.mu.Lock()
	if k.failed = err; err == nil {
		k.keys, k.expires = keys, k.now().Add(age)
	}
	k.inflight = nil
	close(done)
	k.mu.Unlock()
	return k.kept(kid)
}

// kept is the key of kid in the key set kept, if that set is still within
// staleFor of its age: ErrKeysUnavailable when there is no such set, and
// ErrUnknownKey when it lacks kid.
func (k *RemoteKeys) kept(kid string) (ed25519.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.keys == nil || !k.now().Before(k.expires.Add(staleFor)) {
		if k.failed != nil {
			return nil, fmt.Errorf("%w: %w", ErrKeysUnavailable, k.failed)
		}
		return nil, ErrKeysUnavailable
	}
	if key, ok := k.keys[kid]; ok {
		return key, nil
	}
	return nil, ErrUnknownKey
}

// fetch reads the key set once, and how long Core says to keep it. The
// callers waiting for it are not failed by the one that began it going
// away: it ends at its own timeout.
func (k *RemoteKeys) fetch(ctx context.Context) (map[string]ed25519.PublicKey, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("webauth: GET %s: %w", KeysPath, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("webauth: GET %s: %w", KeysPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("webauth: GET %s: HTTP %d", KeysPath, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeySetBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("webauth: GET %s: %w", KeysPath, err)
	}
	if len(body) > maxKeySetBytes {
		return nil, 0, fmt.Errorf("webauth: GET %s: the key set is larger than %d bytes", KeysPath, maxKeySetBytes)
	}
	keys, err := ParseKeySet(body)
	if err != nil {
		return nil, 0, err
	}
	return keys, maxAge(resp.Header.Get("Cache-Control")), nil
}

// maxAge is Cache-Control's max-age, at most maxMaxAge, or maxMaxAge when
// it says none.
func maxAge(cc string) time.Duration {
	for _, d := range strings.Split(cc, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(d), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		n, err := strconv.Atoi(strings.Trim(value, `"`))
		if err != nil || n < 0 {
			break
		}
		return min(time.Duration(n)*time.Second, maxMaxAge)
	}
	return maxMaxAge
}

// jwk is one key of a JSON Web Key Set (RFC 7517), as Core writes an
// Ed25519 one (RFC 8037).
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// ParseKeySet reads a JSON Web Key Set and keeps its Ed25519 signing keys
// by kid: kty OKP, crv Ed25519, alg EdDSA or none said, use sig or none
// said, a kid, and x the 32 bytes of the key. Other keys are passed over,
// and a kid given twice is kept as first given; a set with no key kept is
// refused.
func ParseKeySet(body []byte) (map[string]ed25519.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("webauth: the key set is not a JSON Web Key Set")
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || (k.Alg != "" && k.Alg != "EdDSA") || (k.Use != "" && k.Use != "sig") || k.Kid == "" {
			continue
		}
		x, err := base64.RawURLEncoding.Strict().DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		if _, dup := keys[k.Kid]; !dup {
			keys[k.Kid] = ed25519.PublicKey(x)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("webauth: the key set has no Ed25519 signing key")
	}
	return keys, nil
}

// PinnedKey is the one key the operator gives the runtime
// (CORE_ASSERTION_KEY) in place of fetching Core's: an assertion must name
// it by its kid, its RFC 7638 thumbprint, as Core does.
type PinnedKey struct {
	key ed25519.PublicKey
	kid string
}

// Key is the pinned key, if kid is its thumbprint.
func (p PinnedKey) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	if kid != p.kid {
		return nil, ErrUnknownKey
	}
	return p.key, nil
}

// ParsePinnedKey reads an Ed25519 public key written as a JWK's x is: its
// 32 bytes in base64url, unpadded.
func ParsePinnedKey(s string) (PinnedKey, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return PinnedKey{}, errors.New("not an Ed25519 public key: 32 bytes in base64url, unpadded, as a JWK's x")
	}
	key := ed25519.PublicKey(b)
	return PinnedKey{key: key, kid: Thumbprint(key)}, nil
}

// Thumbprint is the RFC 7638 thumbprint of an Ed25519 public key, which
// Core uses as its kid.
func Thumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
