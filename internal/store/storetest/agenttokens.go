package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// testAgentTokens holds the operator's agents' tokens: one an agent, put
// only in place of the one the caller read, the one replaced destroyed,
// forgotten only while it is the one named.
func testAgentTokens(t *testing.T, open Opener) {
	t.Run("put in place of the one read, and forgotten as it is", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		if _, err := s.AgentToken(ctx, "tutor"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an agent with no token: %v", err)
		}
		tok := func(secret, cred string) store.AgentToken {
			return store.AgentToken{AgentID: "tutor", CoreActorID: "actor-1", SecretID: secret, CredentialID: cred, Hint: "ais_k7v2m4qhx3ab…",
				IssuedAt: at(time.Minute), IssuedBy: "w1"}
		}
		first := tok("sec_a1", "cred-1")
		if err := s.PutAgentToken(ctx, first, sealed("sec_a1", "operator", store.SecretCoreToken), ""); err != nil {
			t.Fatal(err)
		}
		got, err := s.AgentToken(ctx, "tutor")
		if err != nil {
			t.Fatal(err)
		}
		sameTime(t, "issued_at", got.IssuedAt, first.IssuedAt)
		got.IssuedAt = first.IssuedAt
		if *got != first {
			t.Errorf("AgentToken = %+v, want %+v", got, first)
		}
		sameSecret(t, *getSecret(t, s, "sec_a1"), sealed("sec_a1", "operator", store.SecretCoreToken))
		if err := s.DeleteSecret(ctx, "sec_a1"); !errors.Is(err, store.ErrInUse) {
			t.Errorf("DeleteSecret of an agent's token: %v, want ErrInUse", err)
		}

		// Another put it first: refused, and nothing kept of it.
		for _, prev := range []string{"", "sec_other"} {
			if err := s.PutAgentToken(ctx, tok("sec_a2", "cred-2"), sealed("sec_a2", "operator", store.SecretCoreToken), prev); !errors.Is(err, store.ErrConflict) {
				t.Errorf("put in place of %q: %v, want ErrConflict", prev, err)
			}
			missingSecret(t, s, "sec_a2")
		}
		second := tok("sec_a3", "cred-3")
		if err := s.PutAgentToken(ctx, second, sealed("sec_a3", "operator", store.SecretCoreToken), "sec_a1"); err != nil {
			t.Fatal(err)
		}
		missingSecret(t, s, "sec_a1")
		if got, err := s.AgentToken(ctx, "tutor"); err != nil || got.SecretID != "sec_a3" || got.CredentialID != "cred-3" {
			t.Errorf("after the replacement: %+v, %v", got, err)
		}

		// Refused before anything is written.
		for name, c := range map[string]struct {
			t   store.AgentToken
			sec store.Secret
		}{
			"no agent":         {store.AgentToken{CoreActorID: "a", SecretID: "sec_b1"}, sealed("sec_b1", "operator", store.SecretCoreToken)},
			"no agent in Core": {store.AgentToken{AgentID: "x", SecretID: "sec_b1"}, sealed("sec_b1", "operator", store.SecretCoreToken)},
			"a model key":      {store.AgentToken{AgentID: "x", CoreActorID: "a", SecretID: "sec_b1"}, sealed("sec_b1", "operator", store.SecretModelKey)},
			"another secret":   {store.AgentToken{AgentID: "x", CoreActorID: "a", SecretID: "sec_b1"}, sealed("sec_b2", "operator", store.SecretCoreToken)},
		} {
			if err := s.PutAgentToken(ctx, c.t, c.sec, ""); err == nil {
				t.Errorf("%s: put", name)
			}
			missingSecret(t, s, c.sec.ID)
		}

		// Forgotten only while it is the one named.
		if err := s.DeleteAgentToken(ctx, "tutor", "sec_a1"); err != nil {
			t.Fatal(err)
		}
		getSecret(t, s, "sec_a3")
		if err := s.DeleteAgentToken(ctx, "tutor", "sec_a3"); err != nil {
			t.Fatal(err)
		}
		missingSecret(t, s, "sec_a3")
		if _, err := s.AgentToken(ctx, "tutor"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("after it was forgotten: %v", err)
		}
		if err := s.DeleteAgentToken(ctx, "tutor", ""); err != nil {
			t.Errorf("forgetting none: %v", err)
		}
		if err := s.PutAgentToken(ctx, tok("sec_a4", "cred-4"), sealed("sec_a4", "operator", store.SecretCoreToken), ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteAgentToken(ctx, "tutor", ""); err != nil {
			t.Fatal(err)
		}
		missingSecret(t, s, "sec_a4")
	})
}
