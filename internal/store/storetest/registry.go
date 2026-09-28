package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// hosted is a hosted agent as the API would create it, with its token
// sealed as secret token.
func hosted(id, actor, owner, token string) store.HostedAgent {
	return store.HostedAgent{
		ID: id, CoreActorID: actor, OwnerActorID: owner, OwnerVerified: true, TenantID: "ten_" + owner,
		DisplayName: "Agent " + id, TokenSecretID: token, TokenHint: "ais_k7v2m4qhx3ab…",
		Settings:  json.RawMessage(`{"model": {"adapter": "anthropic", "model": "claude-test", "fallback": {"adapter": "openai_chat", "model": "gpt-test", "key_source": "own"}}, "polling": {"inbox_idle_s": 20}}`),
		CreatedAt: at(time.Hour),
	}
}

func create(t *testing.T, s store.Store, a store.HostedAgent, secrets ...store.Secret) *store.HostedAgent {
	t.Helper()
	got, err := s.CreateHostedAgent(t.Context(), a, secrets...)
	if err != nil {
		t.Fatalf("CreateHostedAgent(%s): %v", a.ID, err)
	}
	return got
}

func getHosted(t *testing.T, s store.Store, id string) *store.HostedAgent {
	t.Helper()
	a, err := s.HostedAgent(t.Context(), id)
	if err != nil {
		t.Fatalf("HostedAgent(%s): %v", id, err)
	}
	return a
}

func rev(t *testing.T, s store.Store) int64 {
	t.Helper()
	r, err := s.RegistryRev(t.Context())
	if err != nil {
		t.Fatalf("RegistryRev: %v", err)
	}
	return r
}

// sameJSON fails unless got and want are the same JSON, whatever their
// spacing and key order.
func sameJSON(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %q is not JSON: %v", what, got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// sameHosted compares every field; settings as JSON, and the times only
// where want has them.
func sameHosted(t *testing.T, got, want store.HostedAgent) {
	t.Helper()
	sameJSON(t, "agent "+want.ID+" settings", got.Settings, want.Settings)
	if !want.CreatedAt.IsZero() {
		sameTime(t, "agent "+want.ID+" created_at", got.CreatedAt, want.CreatedAt)
	}
	if !want.UpdatedAt.IsZero() {
		sameTime(t, "agent "+want.ID+" updated_at", got.UpdatedAt, want.UpdatedAt)
	}
	got.Settings, want.Settings = nil, nil
	got.CreatedAt, want.CreatedAt, got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("agent %s:\n got %+v\nwant %+v", want.ID, got, want)
	}
}

func ids(as []store.HostedAgent) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}

func missingSecret(t *testing.T, s store.Store, id string) {
	t.Helper()
	if _, err := s.Secret(t.Context(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("secret %s: %v, want it destroyed", id, err)
	}
}

func testRegistry(t *testing.T, open Opener) {
	t.Run("people", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		p := store.Person{CoreActorID: "actor-1", DisplayName: "Sato", PlatformRole: "admin", LastSeenAt: at(time.Minute)}
		if err := s.PutPerson(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.DisplayName, p.PlatformRole, p.LastSeenAt = "Sato Hanako", "", time.Time{}
		before := time.Now()
		if err := s.PutPerson(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, err := s.Person(ctx, "actor-1")
		if err != nil {
			t.Fatal(err)
		}
		recent(t, "last_seen_at", got.LastSeenAt, before, time.Now())
		if got.DisplayName != "Sato Hanako" || got.PlatformRole != "" {
			t.Errorf("Person = %+v, want it replaced", got)
		}
		if _, err := s.Person(ctx, "actor-2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an unknown person: %v", err)
		}
		if err := s.PutPerson(ctx, store.Person{DisplayName: "x"}); err == nil {
			t.Error("a person without an actor id was taken")
		}
	})

	t.Run("created with its secrets, and read back every way", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		token, key := sealed("sec_t1", "ten_owner1", store.SecretCoreToken), sealed("sec_k1", "ten_owner1", store.SecretModelKey)
		a := hosted("agt_1", "actor-1", "owner1", "sec_t1")
		a.KeySecretID, a.KeyHint, a.KeyProvider = "sec_k1", "sk-…3f9a", "openai"
		got := create(t, s, a, token, key)
		want := a
		want.Version, want.UpdatedAt = 1, a.CreatedAt
		sameHosted(t, *got, want)
		sameHosted(t, *getHosted(t, s, "agt_1"), want)
		sameSecret(t, *getSecret(t, s, "sec_t1"), token)
		sameSecret(t, *getSecret(t, s, "sec_k1"), key)

		create(t, s, hosted("agt_0", "actor-0", "owner1", "sec_t0"), sealed("sec_t0", "ten_owner1", store.SecretCoreToken))
		create(t, s, hosted("agt_2", "actor-2", "owner2", "sec_t2"), sealed("sec_t2", "ten_owner2", store.SecretCoreToken))
		byActor, err := s.HostedAgentByActor(ctx, "actor-1")
		if err != nil || byActor.ID != "agt_1" {
			t.Errorf("HostedAgentByActor(actor-1) = %+v, %v", byActor, err)
		}
		if _, err := s.HostedAgentByActor(ctx, "actor-9"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("HostedAgentByActor of an actor not hosted: %v", err)
		}
		if _, err := s.HostedAgent(ctx, "agt_9"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("HostedAgent of an id not there: %v", err)
		}
		owned, err := s.HostedAgentsOwnedBy(ctx, "owner1")
		if err != nil || !slices.Equal(ids(owned), []string{"agt_0", "agt_1"}) {
			t.Errorf("HostedAgentsOwnedBy(owner1) = %v, %v", ids(owned), err)
		}
		all, err := s.HostedAgents(ctx)
		if err != nil || !slices.Equal(ids(all), []string{"agt_0", "agt_1", "agt_2"}) {
			t.Errorf("HostedAgents = %v, %v", ids(all), err)
		}
		sameHosted(t, all[1], want)
	})

	t.Run("a zero time is the store's now, and no settings are {}", func(t *testing.T) {
		s := open(t)
		a := hosted("agt_1", "actor-1", "owner1", "sec_t1")
		a.CreatedAt, a.Settings = time.Time{}, nil
		before := time.Now()
		got := create(t, s, a, sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		recent(t, "created_at", got.CreatedAt, before, time.Now())
		recent(t, "updated_at", got.UpdatedAt, before, time.Now())
		sameJSON(t, "settings", got.Settings, json.RawMessage(`{}`))
	})

	t.Run("refused, and nothing of it kept", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		putSecret(t, s, sealed("sec_loose", "ten_owner2", store.SecretCoreToken))
		putSecret(t, s, sealed("sec_key_as_token", "ten_owner2", store.SecretModelKey))
		for _, c := range []struct {
			name    string
			a       store.HostedAgent
			secrets []store.Secret
			exists  bool
		}{
			{"an id taken", hosted("agt_1", "actor-2", "owner2", "sec_t2"), []store.Secret{sealed("sec_t2", "ten_owner2", store.SecretCoreToken)}, true},
			{"a Core actor hosted already", hosted("agt_2", "actor-1", "owner2", "sec_t2"), []store.Secret{sealed("sec_t2", "ten_owner2", store.SecretCoreToken)}, true},
			{"a secret id taken", hosted("agt_2", "actor-2", "owner2", "sec_t1"), []store.Secret{sealed("sec_t1", "ten_owner2", store.SecretCoreToken)}, true},
			{"a token stored nowhere", hosted("agt_2", "actor-2", "owner2", "sec_nope"), nil, false},
			{"another agent's token", hosted("agt_2", "actor-2", "owner1", "sec_t1"), nil, false},
			{"a token of another tenant", hosted("agt_2", "actor-2", "owner3", "sec_loose"), nil, false},
			{"a model key as its token", hosted("agt_2", "actor-2", "owner2", "sec_key_as_token"), nil, false},
			{"a token that is a model key", hosted("agt_2", "actor-2", "owner2", "sec_t2"), []store.Secret{sealed("sec_t2", "ten_owner2", store.SecretModelKey)}, false},
			{"a secret it does not refer to", hosted("agt_2", "actor-2", "owner2", "sec_loose"),
				[]store.Secret{sealed("sec_stray", "ten_owner2", store.SecretModelKey)}, false},
			{"no id", func() store.HostedAgent { a := hosted("", "actor-2", "owner2", "sec_loose"); return a }(), nil, false},
			{"an id not agt_", hosted("agent-2", "actor-2", "owner2", "sec_loose"), nil, false},
			{"no Core actor", hosted("agt_2", "", "owner2", "sec_loose"), nil, false},
			{"no tenant", func() store.HostedAgent {
				a := hosted("agt_2", "actor-2", "owner2", "sec_loose")
				a.TenantID = ""
				return a
			}(), nil, false},
			{"settings that are no object", func() store.HostedAgent {
				a := hosted("agt_2", "actor-2", "owner2", "sec_loose")
				a.Settings = json.RawMessage(`[1, 2]`)
				return a
			}(), nil, false},
			{"settings that are null", func() store.HostedAgent {
				a := hosted("agt_2", "actor-2", "owner2", "sec_loose")
				a.Settings = json.RawMessage(`null`)
				return a
			}(), nil, false},
		} {
			r := rev(t, s)
			_, err := s.CreateHostedAgent(ctx, c.a, c.secrets...)
			switch {
			case err == nil:
				t.Errorf("%s: created", c.name)
			case c.exists && !errors.Is(err, store.ErrExists):
				t.Errorf("%s: err = %v, want ErrExists", c.name, err)
			}
			if got := rev(t, s); got != r {
				t.Errorf("%s: the revision moved on from %d to %d", c.name, r, got)
			}
			for _, sec := range c.secrets {
				if sec.ID != "sec_t1" {
					missingSecret(t, s, sec.ID)
				}
			}
		}
		all, err := s.HostedAgents(ctx)
		if err != nil || !slices.Equal(ids(all), []string{"agt_1"}) {
			t.Errorf("HostedAgents = %v, %v; want only the first", ids(all), err)
		}
		sameSecret(t, *getSecret(t, s, "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
	})

	t.Run("updated only at the version read (If-Match)", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := *create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		b := a
		b.DisplayName, b.Paused, b.OwnerActorID, b.OwnerVerified = "Renamed", true, "owner1", false
		b.Settings = json.RawMessage(`{"prompt":{"system_text":"Be brief."}}`)
		before := time.Now()
		got, err := s.UpdateHostedAgent(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		want := b
		want.Version, want.UpdatedAt = 2, time.Time{}
		sameHosted(t, *got, want)
		recent(t, "updated_at", got.UpdatedAt, before, time.Now())
		sameTime(t, "created_at", got.CreatedAt, a.CreatedAt)
		sameHosted(t, *getHosted(t, s, "agt_1"), want)

		// Written since it was read: refused, and left as it is.
		stale := a
		stale.DisplayName = "Stale"
		if _, err := s.UpdateHostedAgent(ctx, stale); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("an update at version 1 of an agent at 2: err = %v, want ErrConflict", err)
		}
		sameHosted(t, *getHosted(t, s, "agt_1"), want)

		gone := b
		gone.ID = "agt_9"
		if _, err := s.UpdateHostedAgent(ctx, gone); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an update of an agent not there: %v", err)
		}
		for name, mutate := range map[string]func(*store.HostedAgent){
			"another Core actor": func(x *store.HostedAgent) { x.CoreActorID = "actor-2" },
			"another tenant":     func(x *store.HostedAgent) { x.TenantID = "ten_other" },
		} {
			x := *getHosted(t, s, "agt_1")
			mutate(&x)
			if _, err := s.UpdateHostedAgent(ctx, x); err == nil {
				t.Errorf("%s: updated", name)
			}
		}
		if got := getHosted(t, s, "agt_1"); got.Version != 2 {
			t.Errorf("refused updates moved the version to %d", got.Version)
		}
	})

	t.Run("a token or key replaced destroys the one before, in the same write", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := *create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		a.TokenSecretID, a.TokenHint = "sec_t2", "ais_newprefix0000…"
		a.KeySecretID, a.KeyHint, a.KeyProvider = "sec_k1", "sk-…aaaa", "deepseek"
		a2, err := s.UpdateHostedAgent(ctx, a, sealed("sec_t2", "ten_owner1", store.SecretCoreToken), sealed("sec_k1", "ten_owner1", store.SecretModelKey))
		if err != nil {
			t.Fatal(err)
		}
		missingSecret(t, s, "sec_t1")
		getSecret(t, s, "sec_t2")
		getSecret(t, s, "sec_k1")
		if a2.TokenSecretID != "sec_t2" || a2.KeySecretID != "sec_k1" || a2.KeyHint != "sk-…aaaa" || a2.KeyProvider != "deepseek" || a2.Version != 2 {
			t.Errorf("after the replacement: %+v", a2)
		}
		if got := getHosted(t, s, "agt_1"); got.KeyProvider != "deepseek" {
			t.Errorf("the key's provider read back: %q", got.KeyProvider)
		}
		// A key taken away is destroyed.
		a2.KeySecretID, a2.KeyHint, a2.KeyProvider = "", "", ""
		a3, err := s.UpdateHostedAgent(ctx, *a2)
		if err != nil {
			t.Fatal(err)
		}
		missingSecret(t, s, "sec_k1")
		if a3.KeySecretID != "" || a3.KeyProvider != "" || a3.Version != 3 {
			t.Errorf("after the key went: %+v", a3)
		}
		// A secret an agent refers to is not destroyed on its own.
		if err := s.DeleteSecret(ctx, "sec_t2"); !errors.Is(err, store.ErrInUse) {
			t.Errorf("DeleteSecret of an agent's token: %v, want ErrInUse", err)
		}
		getSecret(t, s, "sec_t2")
		// A refused replacement keeps the old token and stores nothing.
		bad := *a3
		bad.TokenSecretID = "sec_t3"
		if _, err := s.UpdateHostedAgent(ctx, bad, sealed("sec_t3", "ten_other", store.SecretCoreToken)); err == nil {
			t.Fatal("a token of another tenant was taken")
		}
		missingSecret(t, s, "sec_t3")
		getSecret(t, s, "sec_t2")
	})

	t.Run("paused and resumed whatever its version", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		for i, paused := range []bool{true, true, false} {
			got, err := s.SetHostedAgentPaused(ctx, "agt_1", paused, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Paused != paused || got.Version != i+2 {
				t.Errorf("SetHostedAgentPaused(%v) = paused %v at version %d", paused, got.Paused, got.Version)
			}
		}
		if _, err := s.SetHostedAgentPaused(ctx, "agt_9", true, 0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("pausing an agent not there: %v", err)
		}
	})

	t.Run("paused and resumed at the version named, and at no other", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		got, err := s.SetHostedAgentPaused(ctx, "agt_1", true, 1)
		if err != nil || !got.Paused || got.Version != 2 {
			t.Fatalf("at its version: %+v, %v", got, err)
		}
		last := rev(t, s)
		if _, err := s.SetHostedAgentPaused(ctx, "agt_1", false, 1); !errors.Is(err, store.ErrConflict) {
			t.Errorf("at a version it has left: %v, want ErrConflict", err)
		}
		if got := getHosted(t, s, "agt_1"); !got.Paused || got.Version != 2 {
			t.Errorf("a refused resume wrote: %+v", got)
		}
		if r := rev(t, s); r != last {
			t.Errorf("a refused resume moved the revision from %d to %d", last, r)
		}
		if _, err := s.SetHostedAgentPaused(ctx, "agt_9", true, 1); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an agent not there, at a version: %v", err)
		}
	})

	t.Run("deleted only as it was read", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := *create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		// A new token put in since the delete read it (PUT /token).
		a.TokenSecretID, a.TokenHint = "sec_t2", "ais_newprefix0000…"
		if _, err := s.UpdateHostedAgent(ctx, a, sealed("sec_t2", "ten_owner1", store.SecretCoreToken)); err != nil {
			t.Fatal(err)
		}
		last := rev(t, s)
		for _, cond := range []store.DeleteIf{{TokenSecretID: "sec_t1"}, {Version: 1}, {TokenSecretID: "sec_t2", Version: 1}} {
			if err := s.DeleteHostedAgent(ctx, "agt_1", cond); !errors.Is(err, store.ErrConflict) {
				t.Errorf("DeleteHostedAgent(%+v): %v, want ErrConflict", cond, err)
			}
		}
		getHosted(t, s, "agt_1")
		getSecret(t, s, "sec_t2")
		if r := rev(t, s); r != last {
			t.Errorf("refused deletes moved the revision from %d to %d", last, r)
		}
		if err := s.DeleteHostedAgent(ctx, "agt_1", store.DeleteIf{TokenSecretID: "sec_t2", Version: 2}); err != nil {
			t.Fatalf("as it is: %v", err)
		}
		missingSecret(t, s, "sec_t2")
		if err := s.DeleteHostedAgent(ctx, "agt_1", store.DeleteIf{TokenSecretID: "sec_t2"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("gone, with a condition: %v, want ErrNotFound", err)
		}
	})

	t.Run("courses", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		create(t, s, hosted("agt_0", "actor-0", "owner1", "sec_t0"), sealed("sec_t0", "ten_owner1", store.SecretCoreToken))
		put := func(c store.HostedCourse) {
			t.Helper()
			if err := s.PutHostedCourse(ctx, c); err != nil {
				t.Fatalf("PutHostedCourse(%s, %s): %v", c.AgentID, c.CourseID, err)
			}
		}
		put(store.HostedCourse{AgentID: "agt_1", CourseID: "c2", Settings: json.RawMessage(`{"enabled": false}`), UpdatedBy: "owner1", UpdatedAt: at(time.Minute)})
		put(store.HostedCourse{AgentID: "agt_1", CourseID: "c1", Settings: json.RawMessage(`{"prompt_append_text": "Answer in English."}`), UpdatedBy: "owner1", UpdatedAt: at(time.Minute)})
		put(store.HostedCourse{AgentID: "agt_0", CourseID: "c9", UpdatedBy: "owner1"})
		// Replaced whole.
		put(store.HostedCourse{AgentID: "agt_1", CourseID: "c2", Settings: json.RawMessage(`{"answer": {"max_attempts": 2}}`), UpdatedBy: "admin", UpdatedAt: at(time.Hour)})

		cs, err := s.HostedCourses(ctx, "agt_1")
		if err != nil || len(cs) != 2 || cs[0].CourseID != "c1" || cs[1].CourseID != "c2" {
			t.Fatalf("HostedCourses(agt_1) = %+v, %v", cs, err)
		}
		sameJSON(t, "c1", cs[0].Settings, json.RawMessage(`{"prompt_append_text": "Answer in English."}`))
		sameJSON(t, "c2", cs[1].Settings, json.RawMessage(`{"answer": {"max_attempts": 2}}`))
		sameTime(t, "c2 updated_at", cs[1].UpdatedAt, at(time.Hour))
		if cs[1].UpdatedBy != "admin" {
			t.Errorf("c2 updated_by %q", cs[1].UpdatedBy)
		}
		all, err := s.ListHostedCourses(ctx)
		if err != nil || len(all) != 3 || all[0].AgentID != "agt_0" || all[1].CourseID != "c1" || all[2].CourseID != "c2" {
			t.Fatalf("ListHostedCourses = %+v, %v", all, err)
		}
		sameJSON(t, "c9", all[0].Settings, json.RawMessage(`{}`))

		if err := s.DeleteHostedCourse(ctx, "agt_1", "c2"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteHostedCourse(ctx, "agt_1", "c2"); err != nil {
			t.Fatalf("deleting it again: %v", err)
		}
		if cs, _ := s.HostedCourses(ctx, "agt_1"); len(cs) != 1 {
			t.Errorf("after DeleteHostedCourse: %+v", cs)
		}
		if err := s.PutHostedCourse(ctx, store.HostedCourse{AgentID: "agt_9", CourseID: "c1"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a course of an agent not there: %v", err)
		}
		for _, c := range []store.HostedCourse{{CourseID: "c1"}, {AgentID: "agt_1"}, {AgentID: "agt_1", CourseID: "c1", Settings: json.RawMessage(`"x"`)}} {
			if err := s.PutHostedCourse(ctx, c); err == nil {
				t.Errorf("PutHostedCourse(%+v) was taken", c)
			}
		}
	})

	t.Run("deleted with its courses and secrets, and nothing else", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := hosted("agt_1", "actor-1", "owner1", "sec_t1")
		a.KeySecretID = "sec_k1"
		create(t, s, a, sealed("sec_t1", "ten_owner1", store.SecretCoreToken), sealed("sec_k1", "ten_owner1", store.SecretModelKey))
		create(t, s, hosted("agt_2", "actor-2", "owner1", "sec_t2"), sealed("sec_t2", "ten_owner1", store.SecretCoreToken))
		putSecret(t, s, sealed("sec_school", "school", store.SecretModelKey))
		for _, agent := range []string{"agt_1", "agt_2"} {
			if err := s.PutHostedCourse(ctx, store.HostedCourse{AgentID: agent, CourseID: "c1"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.DeleteHostedAgent(ctx, "agt_1", store.DeleteIf{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.HostedAgent(ctx, "agt_1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the agent after its deletion: %v", err)
		}
		missingSecret(t, s, "sec_t1")
		missingSecret(t, s, "sec_k1")
		if cs, _ := s.HostedCourses(ctx, "agt_1"); len(cs) != 0 {
			t.Errorf("its courses after its deletion: %+v", cs)
		}
		getHosted(t, s, "agt_2")
		getSecret(t, s, "sec_t2")
		getSecret(t, s, "sec_school")
		if cs, _ := s.HostedCourses(ctx, "agt_2"); len(cs) != 1 {
			t.Errorf("another agent's courses: %+v", cs)
		}
		if err := s.DeleteHostedAgent(ctx, "agt_1", store.DeleteIf{}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting it again: %v, want ErrNotFound", err)
		}
		// Its Core actor may be hosted again.
		create(t, s, hosted("agt_3", "actor-1", "owner2", "sec_t3"), sealed("sec_t3", "ten_owner2", store.SecretCoreToken))
	})

	t.Run("the revision moves on with every write to an agent or a course, and with nothing else", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		last := rev(t, s)
		moved := func(what string) {
			t.Helper()
			if r := rev(t, s); r <= last {
				t.Errorf("%s: the revision is %d, was %d", what, r, last)
			} else {
				last = r
			}
		}
		still := func(what string) {
			t.Helper()
			if r := rev(t, s); r != last {
				t.Errorf("%s: the revision moved from %d to %d", what, last, r)
				last = r
			}
		}
		a := *create(t, s, hosted("agt_1", "actor-1", "owner1", "sec_t1"), sealed("sec_t1", "ten_owner1", store.SecretCoreToken))
		moved("create")
		a.DisplayName = "Renamed"
		if _, err := s.UpdateHostedAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
		moved("update")
		if _, err := s.UpdateHostedAgent(ctx, a); !errors.Is(err, store.ErrConflict) {
			t.Fatal(err)
		}
		still("a refused update")
		if _, err := s.SetHostedAgentPaused(ctx, "agt_1", true, 0); err != nil {
			t.Fatal(err)
		}
		moved("pause")
		if err := s.PutHostedCourse(ctx, store.HostedCourse{AgentID: "agt_1", CourseID: "c1"}); err != nil {
			t.Fatal(err)
		}
		moved("a course put")
		if err := s.DeleteHostedCourse(ctx, "agt_1", "c1"); err != nil {
			t.Fatal(err)
		}
		moved("a course deleted")
		// Deleting what is not there writes nothing, and every worker
		// is not made to rebuild for it.
		if err := s.DeleteHostedCourse(ctx, "agt_1", "c1"); err != nil {
			t.Fatal(err)
		}
		still("a course deleted that was not there")
		getHosted(t, s, "agt_1")
		if _, err := s.HostedAgents(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.PutPerson(ctx, store.Person{CoreActorID: "owner1"}); err != nil {
			t.Fatal(err)
		}
		putSecret(t, s, sealed("sec_x", "ten_owner1", store.SecretModelKey))
		if err := s.RewrapSecret(ctx, "sec_x", "local:v1", "local:v2", []byte("w")); err != nil {
			t.Fatal(err)
		}
		still("reads, a person, a secret")
		if err := s.DeleteHostedAgent(ctx, "agt_1", store.DeleteIf{}); err != nil {
			t.Fatal(err)
		}
		moved("delete")
	})
}
