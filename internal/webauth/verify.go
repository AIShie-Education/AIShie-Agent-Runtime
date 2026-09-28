package webauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Why an assertion is refused (the API contract's closed list, §2.3): the
// API sends it as details.reason, and the front end asks Core for a new
// assertion on any of them.
const (
	ReasonMissing   = "assertion_missing"   // no assertion was given
	ReasonMalformed = "assertion_malformed" // too long, not three base64url parts, or not JSON
	ReasonInvalid   = "assertion_invalid"   // not Core's, not for this runtime, or not of a person
	ReasonExpired   = "assertion_expired"   // past its exp, or before its nbf
)

// Error is an assertion refused: its Reason, and the check it failed, in
// words for a debug log line. It never holds the assertion.
type Error struct {
	Reason string
	msg    string
}

func (e *Error) Error() string { return "webauth: " + e.Reason + ": " + e.msg }

func refuse(reason, msg string) *Error { return &Error{Reason: reason, msg: msg} }

// The Verifier's bounds.
const (
	// Leeway is how far the runtime's clock and Core's may disagree.
	Leeway = 5 * time.Second
	// MaxLifetime is the longest an assertion may be made to last
	// (exp - nbf): Core's longest ASSERTION_TTL.
	MaxLifetime = 15 * time.Minute
	// MaxAssertionBytes bounds an assertion, as it bounds the whole
	// Authorization header; Core's are under 1 KB.
	MaxAssertionBytes = 8 << 10
)

// Verifier checks Core's assertions (the API contract, §3.2).
type Verifier struct {
	// Keys finds the key an assertion names.
	Keys Keys
	// Issuer is the iss an assertion must have, byte for byte: Core's
	// PUBLIC_URL, which is the runtime's CORE_BASE_URL.
	Issuer string
	// Audience is the aud an assertion must have, byte for byte: this
	// runtime, as Core's RUNTIME_AUDIENCES names it (API_AUDIENCE).
	Audience string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Claims are what a good assertion says of the person Core vouches for.
// Name and Email are for display, never for deciding anything.
type Claims struct {
	// Subject is the person's actor id in Core, in lower case.
	Subject string
	Name    string
	Email   string
	// PlatformRole is root, admin or "", as Core had it when it made the
	// assertion.
	PlatformRole string
	// SessionID is the credential the person asked Core with.
	SessionID string
	// ID is the assertion's jti.
	ID        string
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time
}

// header is an assertion's protected header: what is checked of it, and
// what must not be there at all.
type header struct {
	Alg  *string         `json:"alg"`
	Typ  *string         `json:"typ"`
	Kid  *string         `json:"kid"`
	Crit json.RawMessage `json:"crit"`
	Jku  json.RawMessage `json:"jku"`
	Jwk  json.RawMessage `json:"jwk"`
	X5u  json.RawMessage `json:"x5u"`
	X5c  json.RawMessage `json:"x5c"`
}

// uuidRe is a UUID as Core writes an actor id, in either case.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Verify checks token, a JWS in compact form, as the API contract's §3.2
// orders it, and returns what it says. First its shape: at most
// MaxAssertionBytes, three base64url parts, a header and claims that are
// JSON objects (else assertion_malformed). Then its header: alg exactly
// EdDSA, typ JWT when there is one, a kid, and none of crit, jku, jwk, x5u
// and x5c, which would have it trust something other than Core's key. Then
// the key its kid names, and the signature over exactly the first two
// parts. Only then its claims: iss and aud Issuer and Audience byte for
// byte (aud a string, not a list), kind human, sub a UUID, jti and sid
// there (else assertion_invalid); and its times, with Leeway: before exp
// and not before nbf (else assertion_expired), not issued in the future,
// and made to last no longer than MaxLifetime (else assertion_invalid).
//
// A refusal is an *Error; Core's keys that cannot be had are
// ErrKeysUnavailable. Neither holds the token.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	if token == "" {
		return nil, refuse(ReasonMissing, "no assertion")
	}
	if len(token) > MaxAssertionBytes {
		return nil, refuse(ReasonMalformed, "longer than an assertion is")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, refuse(ReasonMalformed, "not a JWS in compact form")
	}
	enc := base64.RawURLEncoding.Strict()
	rawHeader, err1 := enc.DecodeString(parts[0])
	rawClaims, err2 := enc.DecodeString(parts[1])
	sig, err3 := enc.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, refuse(ReasonMalformed, "a part is not base64url")
	}
	var h header
	if !isObject(rawHeader) || json.Unmarshal(rawHeader, &h) != nil {
		return nil, refuse(ReasonMalformed, "the header is not a JSON object")
	}
	if !isObject(rawClaims) {
		return nil, refuse(ReasonMalformed, "the claims are not a JSON object")
	}
	switch {
	case h.Alg == nil || *h.Alg != "EdDSA":
		return nil, refuse(ReasonInvalid, "not signed with EdDSA")
	case h.Typ != nil && *h.Typ != "JWT":
		return nil, refuse(ReasonInvalid, "typ is not JWT")
	case h.Kid == nil || *h.Kid == "":
		return nil, refuse(ReasonInvalid, "the header names no key")
	case h.Crit != nil || h.Jku != nil || h.Jwk != nil || h.X5u != nil || h.X5c != nil:
		return nil, refuse(ReasonInvalid, "the header names critical extensions, or a key of its own")
	}
	key, err := v.Keys.Key(ctx, *h.Kid)
	switch {
	case errors.Is(err, ErrUnknownKey):
		return nil, refuse(ReasonInvalid, "its key is not one of Core's")
	case err != nil:
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, refuse(ReasonInvalid, "the signature does not check with Core's key")
	}
	return v.claims(rawClaims)
}

// isObject reports whether raw is one JSON object.
func isObject(raw []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// claims checks the claims of an assertion whose signature checked.
func (v *Verifier) claims(raw []byte) (*Claims, error) {
	var c struct {
		Iss          *string `json:"iss"`
		Aud          *string `json:"aud"`
		Sub          *string `json:"sub"`
		Iat          *int64  `json:"iat"`
		Nbf          *int64  `json:"nbf"`
		Exp          *int64  `json:"exp"`
		Jti          *string `json:"jti"`
		Kind         *string `json:"kind"`
		Name         *string `json:"name"`
		Email        *string `json:"email"`
		PlatformRole *string `json:"platform_role"`
		Sid          *string `json:"sid"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&c); err != nil {
		// A member of another type: an aud that is a list, a time that
		// is not whole seconds.
		return nil, refuse(ReasonInvalid, "a claim is not of the type Core writes")
	}
	switch {
	case c.Iss == nil || *c.Iss != v.Issuer:
		return nil, refuse(ReasonInvalid, "not issued by this runtime's Core")
	case c.Aud == nil || *c.Aud != v.Audience:
		return nil, refuse(ReasonInvalid, "not made for this runtime")
	case c.Kind == nil || *c.Kind != "human":
		return nil, refuse(ReasonInvalid, "not of a person")
	case c.Sub == nil || !uuidRe.MatchString(*c.Sub):
		return nil, refuse(ReasonInvalid, "its sub is not an actor id")
	case c.Jti == nil || *c.Jti == "" || c.Sid == nil || *c.Sid == "":
		return nil, refuse(ReasonInvalid, "it lacks jti or sid")
	case c.Iat == nil || c.Nbf == nil || c.Exp == nil:
		return nil, refuse(ReasonInvalid, "it lacks iat, nbf or exp")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	t := now()
	iat, nbf, exp := time.Unix(*c.Iat, 0), time.Unix(*c.Nbf, 0), time.Unix(*c.Exp, 0)
	switch {
	case !t.Before(exp.Add(Leeway)):
		return nil, refuse(ReasonExpired, "past its exp")
	case t.Before(nbf.Add(-Leeway)):
		return nil, refuse(ReasonExpired, "before its nbf")
	case iat.After(t.Add(Leeway)):
		return nil, refuse(ReasonInvalid, "issued in the future")
	case exp.Sub(nbf) > MaxLifetime:
		return nil, refuse(ReasonInvalid, "made to last longer than Core makes any")
	}
	out := &Claims{Subject: strings.ToLower(*c.Sub), SessionID: *c.Sid, ID: *c.Jti,
		IssuedAt: iat.UTC(), NotBefore: nbf.UTC(), ExpiresAt: exp.UTC()}
	for dst, src := range map[*string]*string{&out.Name: c.Name, &out.Email: c.Email, &out.PlatformRole: c.PlatformRole} {
		if src != nil {
			*dst = *src
		}
	}
	return out, nil
}
