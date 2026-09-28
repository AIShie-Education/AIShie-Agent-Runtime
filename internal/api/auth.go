package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/webauth"
)

// wwwAuthenticate is what a 401 names as the way in.
const wwwAuthenticate = `Bearer realm="aishie-runtime"`

// Caller is the person a request's assertion names (the API contract,
// §3.2). Name is for display, never for deciding anything.
type Caller struct {
	// ActorID is the person's actor id in Core, in lower case.
	ActorID      string
	Name         string
	PlatformRole string
	// SessionID is the Core credential the person asked for the
	// assertion with.
	SessionID string
	// ExpiresAt is when the assertion runs out.
	ExpiresAt time.Time
	// IsAdmin is whether they are one of the runtime's administrators
	// (D4): no v1 route grants them more than an owner.
	IsAdmin bool
}

// authed serves h to a request whose bearer token is an assertion the
// Verifier takes, within the person's allowance of calls, a request taking
// no query and no body (the API contract, §1).
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *Caller)) http.Handler {
	return s.authedBody(func(w http.ResponseWriter, r *http.Request, c *Caller) {
		if noBody(w, r) {
			h(w, r, c)
		}
	})
}

// authedBody is authed for a route that reads its own query and body
// (decodeBody). A refused assertion is counted and logged at debug with its
// reason alone, and takes from the client address's allowances; past
// them, the answer is 429 rather than 401.
func (s *Server) authedBody(h func(http.ResponseWriter, *http.Request, *Caller)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, sent, bad := bearer(r)
		var claims *webauth.Claims
		err := error(&webauth.Error{Reason: webauth.ReasonMalformed})
		if !bad {
			claims, err = s.o.Verifier.Verify(r.Context(), token)
		}
		if err != nil {
			s.refused(w, r, err, sent)
			return
		}
		c := &Caller{ActorID: claims.Subject, Name: claims.Name, PlatformRole: claims.PlatformRole, SessionID: claims.SessionID,
			ExpiresAt: claims.ExpiresAt, IsAdmin: s.isAdmin(claims)}
		if rec, ok := w.(*recorder); ok {
			rec.actor = c.ActorID
		}
		if ok, wait := s.general.take(c.ActorID, s.o.Now()); !ok {
			writeRateLimited(w, wait)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)), c)
	})
}

type callerKey struct{}

// callerOf is the request's Caller, nil outside authed.
func callerOf(r *http.Request) *Caller {
	c, _ := r.Context().Value(callerKey{}).(*Caller)
	return c
}

// refused answers a request whose assertion was refused: 401 with its
// reason, or 429 once the client's address has spent its allowances, which
// a refusal takes from. Core's keys that cannot be had are the runtime's
// trouble, not the caller's: 503 keys_unavailable, taking nothing.
func (s *Server) refused(w http.ResponseWriter, r *http.Request, err error, sent bool) {
	var refusal *webauth.Error
	if !errors.As(err, &refusal) {
		s.m.authFailures.WithLabelValues(ReasonKeysUnavailable).Inc()
		s.o.Log.Warn("the API cannot check assertions: Core's keys cannot be had", "err", err)
		WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonKeysUnavailable,
			Message: "the runtime cannot check assertions now: Core's keys cannot be had"})
		return
	}
	s.m.authFailures.WithLabelValues(refusal.Reason).Inc()
	s.o.Log.Debug("an assertion was refused", "reason", refusal.Reason, "why", err.Error())
	now, addr := s.o.Now(), clientAddr(r)
	okIP, waitIP := s.perIP.take(addr, now)
	okFail, waitFail := s.failures.take(addr, now)
	if !okIP || !okFail {
		writeRateLimited(w, max(waitIP, waitFail))
		return
	}
	v := wwwAuthenticate
	if sent {
		v += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", v)
	WriteError(w, Error{Code: CodeUnauthenticated, Reason: refusal.Reason, Message: refusalWords[refusal.Reason]})
}

// refusalWords say why an assertion was refused, for developers.
var refusalWords = map[string]string{
	webauth.ReasonMissing:   "no assertion: send Authorization: Bearer with an assertion Core made for this runtime",
	webauth.ReasonMalformed: "the Authorization header does not hold an assertion Core makes",
	webauth.ReasonInvalid:   "the assertion is not one Core made for this runtime, of a person",
	webauth.ReasonExpired:   "the assertion has expired, or is not good yet: ask Core for a new one",
}

// bearer is the request's bearer token; sent is whether an Authorization
// header was given at all, and bad whether it was one no assertion could
// be: longer than webauth.MaxAssertionBytes, or not "Bearer <token>".
func bearer(r *http.Request) (token string, sent, bad bool) {
	h := r.Header.Values("Authorization")
	if len(h) == 0 {
		return "", false, false
	}
	if len(h) > 1 || len(h[0]) > webauth.MaxAssertionBytes {
		return "", true, true
	}
	scheme, token, ok := strings.Cut(h[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", true, true
	}
	return strings.TrimSpace(token), true, false
}
