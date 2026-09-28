package webauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	issuer   = "https://lms.example.edu"
	audience = "https://lms.example.edu/runtime"
	person   = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"
)

// signer mints assertions as Core does (Core's internal/auth/assertion.go).
type signer struct {
	key ed25519.PrivateKey
	kid string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{key: key, kid: Thumbprint(key.Public().(ed25519.PublicKey))}
}

func (s signer) public() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sign makes a JWS of header and claims, as given.
func (s signer) sign(t *testing.T, header, claims any) string {
	t.Helper()
	input := b64(mustJSON(t, header)) + "." + b64(mustJSON(t, claims))
	return input + "." + b64(ed25519.Sign(s.key, []byte(input)))
}

// at is the test's clock.
var at = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// claimsOf are the claims Core makes for Yuki at the test's clock, lasting
// five minutes.
func claimsOf() map[string]any {
	return map[string]any{
		"iss": issuer, "aud": audience, "sub": person,
		"iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(5 * time.Minute).Unix(),
		"jti": "j1", "kind": "human", "name": "Yuki", "email": "yuki@example.edu", "platform_role": "admin",
		"sid": "0192f3c1-0000-7000-8000-000000000001",
	}
}

func (s signer) header() map[string]any {
	return map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": s.kid}
}

// keysOf is a key set of the signers' keys.
type keysOf map[string]ed25519.PublicKey

func (k keysOf) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	if key, ok := k[kid]; ok {
		return key, nil
	}
	return nil, ErrUnknownKey
}

type keysDown struct{}

func (keysDown) Key(context.Context, string) (ed25519.PublicKey, error) {
	return nil, ErrKeysUnavailable
}

func verifier(keys Keys, now time.Time) *Verifier {
	return &Verifier{Keys: keys, Issuer: issuer, Audience: audience, Now: func() time.Time { return now }}
}

// A good assertion is taken, with what it says; within the clocks' skew of
// its times, it is taken too.
func TestVerifyTakesCoresAssertion(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, s.header(), claimsOf())
	for _, now := range []time.Time{at, at.Add(-4 * time.Second), at.Add(5*time.Minute + 4*time.Second)} {
		c, err := verifier(keysOf{s.kid: s.public()}, now).Verify(context.Background(), token)
		if err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
		if c.Subject != person || c.Name != "Yuki" || c.Email != "yuki@example.edu" || c.PlatformRole != "admin" ||
			c.SessionID != "0192f3c1-0000-7000-8000-000000000001" || c.ID != "j1" || !c.ExpiresAt.Equal(at.Add(5*time.Minute)) ||
			!c.IssuedAt.Equal(at) || !c.NotBefore.Equal(at) {
			t.Errorf("claims: %+v", c)
		}
	}
	// No email and no role: both empty.
	cl := claimsOf()
	delete(cl, "email")
	delete(cl, "platform_role")
	c, err := verifier(keysOf{s.kid: s.public()}, at).Verify(context.Background(), s.sign(t, s.header(), cl))
	if err != nil || c.Email != "" || c.PlatformRole != "" {
		t.Errorf("without email and role: %+v %v", c, err)
	}
}

// Every assertion that is not Core's, for this runtime, of a person, now,
// is refused, saying why, and the refusal never holds the assertion.
func TestVerifyRefuses(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	keys := keysOf{s.kid: s.public()}
	with := func(edit func(map[string]any)) string {
		c := claimsOf()
		edit(c)
		return s.sign(t, s.header(), c)
	}
	withHeader := func(edit func(map[string]any)) string {
		h := s.header()
		edit(h)
		return s.sign(t, h, claimsOf())
	}
	good := s.sign(t, s.header(), claimsOf())
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + b64(mustJSON(t, func() map[string]any { c := claimsOf(); c["sub"] = strings.Replace(person, "a", "b", 1); return c }())) + "." + parts[2]
	unsigned := b64(mustJSON(t, map[string]any{"alg": "none", "typ": "JWT"})) + "." + parts[1] + "."
	for _, tc := range []struct {
		name, token, reason string
		now                 time.Time
	}{
		{"nothing", "", ReasonMissing, at},
		{"not a JWS", "ais_0123456789abcdef", ReasonMalformed, at},
		{"four parts", good + ".x", ReasonMalformed, at},
		{"padded base64", parts[0] + "=." + parts[1] + "." + parts[2], ReasonMalformed, at},
		{"a header that is not JSON", b64([]byte("alg")) + "." + parts[1] + "." + parts[2], ReasonMalformed, at},
		{"claims that are a list", parts[0] + "." + b64([]byte("[]")) + "." + parts[2], ReasonMalformed, at},
		{"too long", good + strings.Repeat("A", MaxAssertionBytes), ReasonMalformed, at},
		{"unsigned, alg none", unsigned, ReasonInvalid, at},
		{"HMAC", withHeader(func(h map[string]any) { h["alg"] = "HS256" }), ReasonInvalid, at},
		{"RSA", withHeader(func(h map[string]any) { h["alg"] = "RS256" }), ReasonInvalid, at},
		{"no alg", withHeader(func(h map[string]any) { delete(h, "alg") }), ReasonInvalid, at},
		{"another typ", withHeader(func(h map[string]any) { h["typ"] = "at+jwt" }), ReasonInvalid, at},
		{"no kid", withHeader(func(h map[string]any) { delete(h, "kid") }), ReasonInvalid, at},
		{"critical extensions", withHeader(func(h map[string]any) { h["crit"] = []string{"exp"} }), ReasonInvalid, at},
		{"a key of its own", withHeader(func(h map[string]any) { h["jwk"] = jwkOf(other) }), ReasonInvalid, at},
		{"a key set of its own", withHeader(func(h map[string]any) { h["jku"] = "https://evil.example.org/keys" }), ReasonInvalid, at},
		{"a certificate URL", withHeader(func(h map[string]any) { h["x5u"] = "https://evil.example.org/cert" }), ReasonInvalid, at},
		{"a certificate chain", withHeader(func(h map[string]any) { h["x5c"] = []string{"MIIB"} }), ReasonInvalid, at},
		{"a key Core does not have", other.sign(t, other.header(), claimsOf()), ReasonInvalid, at},
		{"another key under Core's kid", other.sign(t, s.header(), claimsOf()), ReasonInvalid, at},
		{"claims changed after signing", tampered, ReasonInvalid, at},
		{"a signature cut short", parts[0] + "." + parts[1] + "." + b64([]byte("short")), ReasonInvalid, at},
		{"another issuer", with(func(c map[string]any) { c["iss"] = "https://evil.example.edu" }), ReasonInvalid, at},
		{"the issuer with a /", with(func(c map[string]any) { c["iss"] = issuer + "/" }), ReasonInvalid, at},
		{"no issuer", with(func(c map[string]any) { delete(c, "iss") }), ReasonInvalid, at},
		{"another audience", with(func(c map[string]any) { c["aud"] = "https://lms.example.edu/other" }), ReasonInvalid, at},
		{"the audience in capitals", with(func(c map[string]any) { c["aud"] = strings.ToUpper(audience) }), ReasonInvalid, at},
		{"the audience with a /", with(func(c map[string]any) { c["aud"] = audience + "/" }), ReasonInvalid, at},
		{"audiences as a list", with(func(c map[string]any) { c["aud"] = []string{audience} }), ReasonInvalid, at},
		{"an agent's", with(func(c map[string]any) { c["kind"] = "agent" }), ReasonInvalid, at},
		{"no kind", with(func(c map[string]any) { delete(c, "kind") }), ReasonInvalid, at},
		{"a sub that is not an actor id", with(func(c map[string]any) { c["sub"] = "yuki" }), ReasonInvalid, at},
		{"no sub", with(func(c map[string]any) { delete(c, "sub") }), ReasonInvalid, at},
		{"no jti", with(func(c map[string]any) { delete(c, "jti") }), ReasonInvalid, at},
		{"no sid", with(func(c map[string]any) { c["sid"] = "" }), ReasonInvalid, at},
		{"no exp", with(func(c map[string]any) { delete(c, "exp") }), ReasonInvalid, at},
		{"a time that is not whole seconds", with(func(c map[string]any) { c["exp"] = 1.5 }), ReasonInvalid, at},
		{"expired", good, ReasonExpired, at.Add(5*time.Minute + Leeway)},
		{"not good yet", with(func(c map[string]any) { c["nbf"] = at.Add(time.Minute).Unix() }), ReasonExpired, at},
		{"issued in the future", with(func(c map[string]any) { c["iat"] = at.Add(time.Minute).Unix() }), ReasonInvalid, at},
		{"made to last a day", with(func(c map[string]any) { c["exp"] = at.Add(24 * time.Hour).Unix() }), ReasonInvalid, at},
		{"made to last 901 s", with(func(c map[string]any) { c["exp"] = at.Add(MaxLifetime + time.Second).Unix() }), ReasonInvalid, at},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := verifier(keys, tc.now).Verify(context.Background(), tc.token)
			var e *Error
			if !errors.As(err, &e) || e.Reason != tc.reason {
				t.Fatalf("got %+v, %v; want refused as %s", c, err, tc.reason)
			}
			for _, p := range strings.Split(tc.token, ".") {
				if len(p) > 8 && strings.Contains(err.Error(), p) {
					t.Errorf("the refusal holds the assertion: %v", err)
				}
			}
		})
	}
	// Made to last exactly 900 s, it is taken; its sub in capitals is
	// given in lower case.
	c, err := verifier(keys, at).Verify(context.Background(), with(func(c map[string]any) {
		c["exp"] = at.Add(MaxLifetime).Unix()
		c["sub"] = strings.ToUpper(person)
	}))
	if err != nil || c.Subject != person {
		t.Errorf("900 s, sub in capitals: %+v %v", c, err)
	}
}

// Core's keys that cannot be had are said as such, not as a bad assertion.
func TestVerifyWithoutKeys(t *testing.T) {
	s := newSigner(t)
	_, err := verifier(keysDown{}, at).Verify(context.Background(), s.sign(t, s.header(), claimsOf()))
	var e *Error
	if !errors.Is(err, ErrKeysUnavailable) || errors.As(err, &e) {
		t.Errorf("got %v", err)
	}
}

// A signature of which any one bit is changed, written again in base64url,
// is assertion_invalid, never assertion_malformed: the end-to-end suite
// tampers with Core's assertions so, and not with a character of their
// text, which may leave one that is not base64url at all.
func TestAnyBitOfTheSignatureChangedIsInvalid(t *testing.T) {
	s := newSigner(t)
	parts := strings.Split(s.sign(t, s.header(), claimsOf()), ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	v := verifier(keysOf{s.kid: s.public()}, at)
	for bit := range 8 * len(sig) {
		tampered := append([]byte(nil), sig...)
		tampered[bit/8] ^= 1 << (bit % 8)
		_, err := v.Verify(context.Background(), parts[0]+"."+parts[1]+"."+b64(tampered))
		if e := (*Error)(nil); !errors.As(err, &e) || e.Reason != ReasonInvalid {
			t.Fatalf("bit %d changed: %v", bit, err)
		}
	}
}
