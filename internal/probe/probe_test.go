package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
)

// world is a fake Core with Yuki, her agent seated as her delegate, and
// Ken.
type world struct {
	t     *testing.T
	fc    *fakecore.Core
	url   string
	cat   *core.Catalogue
	yuki  fakecore.Actor
	ken   fakecore.Actor
	agent fakecore.Actor
}

func newWorld(t *testing.T, o fakecore.Options) *world {
	t.Helper()
	fc := fakecore.New(o)
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	w := &world{t: t, fc: fc, url: srv.URL}
	co := fc.AddCourse("CS101")
	w.yuki, w.ken = fc.AddPerson("Yuki"), fc.AddPerson("Ken")
	seat, err := fc.Seat(w.yuki.ID, co.ID, fakecore.SeatOptions{Preset: "student"})
	w.ok(err)
	w.agent, err = fc.AddAgent("Yuki's helper", w.yuki.ID)
	w.ok(err)
	_, err = fc.Seat(w.agent.ID, co.ID, fakecore.SeatOptions{Preset: "delegate", Principal: seat.ID})
	w.ok(err)
	w.cat, err = core.FetchCatalogue(context.Background(), srv.Client(), srv.URL)
	w.ok(err)
	return w
}

func (w *world) ok(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) client(token string) *core.Client { return NewClient(w.url, token, nil) }

// Inspect refuses a token in the contract's order, and gives the agent and
// its seats for one that passes; me_memberships is read only then.
func TestInspect(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	ins, err := Inspect(t.Context(), w.client(w.agent.Token), w.cat, Want{ActorID: strings.ToUpper(w.agent.ID), Owner: strings.ToUpper(w.yuki.ID)})
	if err != nil || ins.Me.ID != w.agent.ID || ins.Me.OwnerActorID != w.yuki.ID || len(ins.Memberships) != 1 ||
		ins.Memberships[0].PrincipalMemberID == nil {
		t.Fatalf("%+v %v", ins, err)
	}

	unowned, err := w.fc.AddAgent("Nobody's", w.ken.ID)
	w.ok(err)
	w.ok(w.fc.SetOwner(unowned.ID, ""))
	unownedToken, err := w.fc.IssueToken(unowned.ID)
	w.ok(err)
	revoked, err := w.fc.IssueToken(w.agent.ID)
	w.ok(err)
	w.ok(w.fc.Revoke(revoked))
	suspended, err := w.fc.AddAgent("Suspended", w.yuki.ID)
	w.ok(err)
	w.ok(w.fc.SuspendActor(suspended.ID))
	old := newWorld(t, fakecore.Options{BeforeOwners: true})

	for _, tc := range []struct {
		name   string
		w      *world
		token  string
		want   Want
		reason string
	}{
		{"a revoked token", w, revoked, Want{Owner: w.yuki.ID}, ReasonTokenRefused},
		{"a token Core never issued", w, "ais_aaaaaaaaaaaa_" + strings.Repeat("A", 43), Want{Owner: w.yuki.ID}, ReasonTokenRefused},
		{"a person's", w, w.yuki.Token, Want{Owner: w.yuki.ID}, ReasonTokenNotAgent},
		{"a suspended agent's", w, suspended.Token, Want{Owner: w.yuki.ID}, ReasonAgentSuspended},
		{"another agent's", w, w.agent.Token, Want{ActorID: suspended.ID, Owner: w.yuki.ID}, ReasonTokenOtherAgent},
		{"on a Core from before owners", old, old.agent.Token, Want{Owner: old.yuki.ID}, ReasonCoreTooOld},
		{"an agent nobody owns", w, unownedToken, Want{Owner: w.ken.ID}, ReasonAgentUnowned},
		{"another's agent", w, w.agent.Token, Want{Owner: w.ken.ID}, ReasonNotOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tc.w.fc.Calls())
			_, err := Inspect(t.Context(), tc.w.client(tc.token), tc.w.cat, tc.want)
			var pe *Error
			if !errors.As(err, &pe) || pe.Reason != tc.reason {
				t.Fatalf("%v, want %s", err, tc.reason)
			}
			for _, c := range tc.w.fc.Calls()[before:] {
				if c.Tool == "me_memberships" {
					t.Error("me_memberships was read for a token refused")
				}
			}
			if strings.Contains(err.Error(), tc.token) {
				t.Error("the error holds the token")
			}
		})
	}

	w.fc.Inject(func(fakecore.InjectedCall) *fakecore.Injection {
		return &fakecore.Injection{Status: http.StatusServiceUnavailable}
	})
	_, err = Inspect(t.Context(), w.client(w.agent.Token), w.cat, Want{Owner: w.yuki.ID})
	if pe := (*Error)(nil); !errors.As(err, &pe) || pe.Reason != ReasonCoreUnavailable {
		t.Errorf("Core answering 503: %v", err)
	}
	w.fc.Inject(nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, err = Inspect(t.Context(), NewClient(dead.URL, w.agent.Token, nil), w.cat, Want{Owner: w.yuki.ID})
	if pe := (*Error)(nil); !errors.As(err, &pe) || pe.Reason != ReasonCoreUnavailable || strings.Contains(err.Error(), w.agent.Token) {
		t.Errorf("Core not there: %v", err)
	}
}

// RevokeReplaced revokes the token of a prefix with a replacement's, and
// RevokeToken with the token itself; each says a dead one was dead
// already, and why it failed for a suspended agent and a Core not
// answering.
func TestRevokeToken(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	now := time.Now()
	prefix := Prefix(w.agent.Token)
	if prefix == "" || HintPrefix("ais_"+prefix+"…") != prefix || HintPrefix("…") != "" || HintPrefix("ais_ABCDEFGHIJKL…") != "" {
		t.Fatalf("the prefix of %q", prefix)
	}

	// A replacement revokes the token it replaces.
	next, err := w.fc.IssueLabelledToken(w.agent.ID, "next")
	w.ok(err)
	r := RevokeReplaced(t.Context(), w.client(next.Token), prefix, now)
	if r.Outcome != Revoked || r.Problem != "" || r.CredentialID == "" {
		t.Fatalf("%+v", r)
	}
	if _, err := w.client(w.agent.Token).Me(t.Context()); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("the revoked token still works: %v", err)
	}
	// Dead already: listed revoked, or never listed.
	if r := RevokeReplaced(t.Context(), w.client(next.Token), prefix, now); r.Outcome != AlreadyInvalid {
		t.Errorf("again: %+v", r)
	}
	if r := RevokeReplaced(t.Context(), w.client(next.Token), "aaaaaaaaaaaa", now); r.Outcome != AlreadyInvalid {
		t.Errorf("a prefix of no token: %+v", r)
	}
	// The token itself, already revoked, is refused: dead already.
	if r := RevokeToken(t.Context(), w.client(w.agent.Token), prefix, now); r.Outcome != AlreadyInvalid {
		t.Errorf("with the revoked token itself: %+v", r)
	}
	// A suspended agent's calls are denied.
	w.ok(w.fc.SuspendActor(w.agent.ID))
	if r := RevokeToken(t.Context(), w.client(next.Token), next.Prefix, now); r.Outcome != Failed || r.Problem != ProblemAgentSuspended {
		t.Errorf("suspended: %+v", r)
	}
	w.ok(w.fc.ReactivateActor(w.agent.ID))
	// Core not answering leaves the token as it was.
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "credential_revoke" {
			return &fakecore.Injection{Status: http.StatusInternalServerError}
		}
		return nil
	})
	if r := RevokeToken(t.Context(), w.client(next.Token), next.Prefix, now); r.Outcome != Failed || r.Problem != ProblemCoreUnavailable || r.CredentialID != next.CredentialID {
		t.Errorf("Core answering 500: %+v", r)
	}
	w.fc.Inject(nil)
	// The token revokes itself: Core answers, and its next use is a 401.
	if r := RevokeToken(t.Context(), w.client(next.Token), next.Prefix, now); r.Outcome != Revoked {
		t.Errorf("revoking itself: %+v", r)
	}
	if _, err := w.client(next.Token).Me(t.Context()); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("the token that revoked itself: %v", err)
	}
	for _, c := range w.fc.Credentials(w.agent.ID) {
		if c.RevokedAt == nil {
			t.Errorf("credential %s (%s) is not revoked", c.ID, c.Label)
		}
	}
	if r := RevokeToken(t.Context(), w.client(next.Token), "", now); r.Outcome != Failed {
		t.Errorf("no prefix: %+v", r)
	}
}

// A token that replaced another revokes it as RevokeToken does; but when
// Core refuses it (401), it was itself replaced meanwhile, which says
// nothing of the token it replaced: that one is failed, core_refused, and
// left working for its owner to revoke, never already_invalid.
func TestRevokeReplaced(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	now := time.Now()
	prefix := Prefix(w.agent.Token)
	next, err := w.fc.IssueLabelledToken(w.agent.ID, "next")
	w.ok(err)
	third, err := w.fc.IssueLabelledToken(w.agent.ID, "third")
	w.ok(err)
	// next is replaced by third, and revoked with it, before next revokes
	// the first token.
	if r := RevokeReplaced(t.Context(), w.client(third.Token), next.Prefix, now); r.Outcome != Revoked {
		t.Fatalf("third revoking next: %+v", r)
	}
	r := RevokeReplaced(t.Context(), w.client(next.Token), prefix, now)
	if r.Outcome != Failed || r.Problem != ProblemCoreRefused {
		t.Errorf("with a replacing token Core refuses: %+v, want failed (core_refused)", r)
	}
	if _, err := w.client(w.agent.Token).Me(t.Context()); err != nil {
		t.Errorf("the first token was revoked after all: %v", err)
	}
	// With a replacing token that works, it is revoked.
	if r := RevokeReplaced(t.Context(), w.client(third.Token), prefix, now); r.Outcome != Revoked {
		t.Errorf("third revoking the first: %+v", r)
	}
	if r := RevokeReplaced(t.Context(), w.client(third.Token), prefix, now); r.Outcome != AlreadyInvalid {
		t.Errorf("again: %+v", r)
	}
}

// Only tokens of Core's shape are taken, and their prefix read.
func TestTokenShape(t *testing.T) {
	good := "ais_k7v2m4qhx3ab_" + strings.Repeat("x", 43)
	for tok, want := range map[string]bool{
		good: true, "ais_K7V2M4QHX3AB_" + strings.Repeat("x", 43): false, "ais_k7v2m4qhx3a1_" + strings.Repeat("x", 43): false,
		"ais_k7v2m4qhx3ab_" + strings.Repeat("x", 31): false, "aisinv_k7v2m4qhx3ab_" + strings.Repeat("x", 43): false,
		good + "\n": false, " " + good: false, "": false,
	} {
		if IsToken(tok) != want {
			t.Errorf("IsToken(%.20q…) = %v", tok, !want)
		}
	}
	if Prefix(good) != "k7v2m4qhx3ab" || Prefix("sk-nope") != "" {
		t.Error("Prefix")
	}
}
