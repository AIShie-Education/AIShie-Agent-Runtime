package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/vault"
)

// registryWorld is a fake Core with two agents of Yuki's, a migrated
// database, and a keyring: what a runtime with hosted agents runs on.
type registryWorld struct {
	fc      *fakecore.Core
	coreURL string
	dbURL   string
	st      *pgstore.Store
	v       *vault.Vault
	kms     string
	// yuki owns the agents in Core, and connected them here.
	yuki   fakecore.Actor
	agents []fakecore.Actor
}

func newRegistryWorld(t *testing.T) *registryWorld {
	t.Helper()
	fc, srv := fakeCore(t)
	w := &registryWorld{fc: fc, coreURL: srv.URL, dbURL: scratchDatabase(t)}
	if err := pgstore.Migrate(w.dbURL, pgstore.Up); err != nil {
		t.Fatal(err)
	}
	st, err := pgstore.Open(t.Context(), w.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	w.st = st
	dir := t.TempDir()
	writeKey(t, dir, "v1")
	w.kms = "local:" + filepath.Join(dir, "v1")
	if w.v, err = vault.Open(w.kms); err != nil {
		t.Fatal(err)
	}
	co := fc.AddCourse("CS101")
	yuki := fc.AddPerson("Yuki")
	w.yuki = yuki
	seat, err := fc.Seat(yuki.ID, co.ID, fakecore.SeatOptions{Preset: "student"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		a, err := fc.AddAgent(fmt.Sprintf("Yuki's helper %d", i), yuki.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fc.Seat(a.ID, co.ID, fakecore.SeatOptions{Preset: "delegate", Principal: seat.ID}); err != nil {
			t.Fatal(err)
		}
		w.agents = append(w.agents, a)
	}
	return w
}

// host connects agent i as the hosted agent id, with settings, as the API
// would.
func (w *registryWorld) host(t *testing.T, id string, i int, settings string) {
	t.Helper()
	w.hostActor(t, id, w.agents[i], settings)
}

// hostActor connects actor as the hosted agent id, with its token and
// settings, as Yuki.
func (w *registryWorld) hostActor(t *testing.T, id string, actor fakecore.Actor, settings string) {
	t.Helper()
	w.hostAs(t, id, actor, w.yuki.ID, settings)
}

// hostAs connects actor as the hosted agent id, with its token and
// settings, as the person owner.
func (w *registryWorld) hostAs(t *testing.T, id string, actor fakecore.Actor, owner, settings string) {
	t.Helper()
	seal := func(kind, plaintext string) store.Secret {
		s, err := w.v.Seal(context.Background(), store.Secret{ID: vault.NewSecretID(), TenantID: "ten_yuki", Kind: kind}, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tok, key := seal(store.SecretCoreToken, actor.Token), seal(store.SecretModelKey, "sk-test-0123456789abcdefghij")
	_, err := w.st.CreateHostedAgent(t.Context(), store.HostedAgent{
		ID: id, CoreActorID: actor.ID, OwnerActorID: owner, TenantID: "ten_yuki", DisplayName: "Hosted " + id,
		TokenSecretID: tok.ID, TokenHint: tok.Hint, KeySecretID: key.ID, KeyHint: key.Hint, Settings: json.RawMessage(settings),
	}, tok, key)
	if err != nil {
		t.Fatal(err)
	}
}

// hostedSettings are an own-key agent's settings on OpenAI's own endpoint,
// which the registry allows: the tests never ask it anything.
const hostedSettings = `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own"},
	"polling": {"inbox_hot_s": 0.02, "inbox_idle_s": 0.05, "inbox_max_s": 0.1, "events_s": 0.1, "memberships_s": 1, "assumed_core_rate_per_min": 600000}}`

// state waits for agent id's state in the store.
func (w *registryWorld) waitState(t *testing.T, out *lines, id, want string) store.AgentState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		states, err := w.st.AgentStates(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range states {
			if st.AgentID == id && st.State == want {
				return st
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent %s never %s: %+v\n%s", id, want, states, out.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunReloadsTheRegistry: run with the store in PostgreSQL runs the
// registry's hosted agents from their sealed secrets, beside YAML's (none
// here); an agent connected while it runs is started at once, told by the
// registry's notification; one paused stops; and none of its secrets is
// logged.
func TestRunReloadsTheRegistry(t *testing.T) {
	w := newRegistryWorld(t)
	w.host(t, "agt_first", 0, hostedSettings)
	cmd, out, exited := child(t, "run", "CONFIG="+t.TempDir(), "DATABASE_URL="+w.dbURL, "KMS_KEY_ID="+w.kms,
		"CORE_BASE_URL="+w.coreURL, "HTTP_ADDR=127.0.0.1:0", "LOG_FORMAT=json", "SHUTDOWN_GRACE=2s", "WORKER_ID=child")
	started := out.wait(t, `"msg":"aishie-runtime started"`)
	if !strings.Contains(started, `"hosted":1`) || !strings.Contains(started, `"registry":true`) || !strings.Contains(started, `"kek":"local:v1"`) {
		t.Errorf("the started line: %s", started)
	}
	w.waitState(t, out, "agt_first", store.AgentRunning)
	var addr struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal([]byte(started), &addr); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr.Addr + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Agents []struct {
			AgentID string `json:"agent_id"`
			Hosted  bool   `json:"hosted"`
		} `json:"agents"`
	}
	err = json.NewDecoder(resp.Body).Decode(&status)
	_ = resp.Body.Close()
	if err != nil || len(status.Agents) != 1 || status.Agents[0].AgentID != "agt_first" || !status.Agents[0].Hosted {
		t.Errorf("/status: %+v, %v", status, err)
	}

	// Connected while it runs.
	w.host(t, "agt_second", 1, hostedSettings)
	w.waitState(t, out, "agt_second", store.AgentRunning)
	out.wait(t, `"msg":"the registry of hosted agents changed"`)

	if _, err := w.st.SetHostedAgentPaused(t.Context(), "agt_first", true); err != nil {
		t.Fatal(err)
	}
	w.waitState(t, out, "agt_first", store.AgentPaused)

	a, err := w.st.HostedAgent(t.Context(), "agt_second")
	if err != nil {
		t.Fatal(err)
	}
	a.Settings = json.RawMessage(`{"model": {"adapter": "openai_chat", "model": "m", "key_source": "own", "base_url": "https://10.1.2.3/v1"}}`)
	if _, err := w.st.UpdateHostedAgent(t.Context(), *a); err != nil {
		t.Fatal(err)
	}
	st := w.waitState(t, out, "agt_second", store.AgentError)
	if !strings.Contains(st.Detail, "not run: agent.model: base_url 10.1.2.3 is not an official provider's endpoint") {
		t.Errorf("the detail of the agent that does not pass: %q", st.Detail)
	}
	out.wait(t, `"msg":"hosted agents not run: their configuration does not pass; each one's state says why","agents":["agt_second"]`)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, exited, 15*time.Second, out); err != nil {
		t.Errorf("the runtime exited with %v:\n%s", err, out.text())
	}
	for _, s := range append([]string{"ais_", "sk-test"}, w.agents[0].Token, w.agents[1].Token) {
		if strings.Contains(out.text(), s) {
			t.Errorf("a log line holds a secret (%.8s…)", s)
		}
	}
}

// TestRunWithTheRegistryOff: with the store in memory, the registry is off,
// and run says so.
func TestRunWithTheRegistryOff(t *testing.T) {
	cmd, out, exited := child(t, "run", "CONFIG="+t.TempDir(), "DATABASE_URL=", "HTTP_ADDR=127.0.0.1:0", "LOG_FORMAT=json",
		"SHUTDOWN_GRACE=1s", "WORKER_ID=child")
	out.wait(t, `"msg":"the registry of hosted agents is off: it is kept in PostgreSQL, which DATABASE_URL names"`)
	out.wait(t, `"registry":false`)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, exited, 15*time.Second, out); err != nil {
		t.Errorf("exit: %v", err)
	}
}

// TestCheckReadsTheRegistry: check shows the hosted agents run would run,
// and those it would not, with why, and passes: they keep no other from
// running. (check --live connects them as it does YAML agents with sealed
// secrets, TestCheckLiveOpensSealedSecrets, and tries their keys at their
// providers, which a test does not.) A registry it cannot read, as before a deploy's migrate up, is
// said, and passes too.
func TestCheckReadsTheRegistry(t *testing.T) {
	w := newRegistryWorld(t)
	w.host(t, "agt_ok", 0, hostedSettings)
	w.host(t, "agt_bad", 1, `{"model": {"adapter": "openai_chat", "model": "m", "key_source": "school"}}`)
	getenv := env("CONFIG", t.TempDir(), "DATABASE_URL", w.dbURL, "CORE_BASE_URL", w.coreURL, "KMS_KEY_ID", w.kms)
	code, out, errs := runCmd(t, getenv, "check")
	for _, want := range []string{
		`agent agt_ok (hosted): "Hosted agt_ok"`,
		"hosted agent agt_bad: NOT RUN: agent.model: the school's key is not offered to hosted agents yet",
		"the configuration passes: 1 agents, 1 of them hosted; 1 hosted agents not run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check does not say %q:\n%s%s", want, out, errs)
		}
	}
	if code != exitOK {
		t.Errorf("check: %d\n%s%s", code, out, errs)
	}

	fresh := scratchDatabase(t)
	code, out, errs = runCmd(t, env("CONFIG", t.TempDir(), "DATABASE_URL", fresh), "check")
	if code != exitOK || !strings.Contains(out, "registry: not read: pgstore: the schema is at version 0") {
		t.Errorf("check on a schema not migrated: %d\n%s%s", code, out, errs)
	}
}

// TestCheckLiveRefusesAPersonsToken: check --live holds a hosted agent's
// token to what run does: its own actor's, an agent's. A person's own token
// fails the check at me_get, and nothing more is read with it.
func TestCheckLiveRefusesAPersonsToken(t *testing.T) {
	w := newRegistryWorld(t)
	person := w.fc.AddPerson("Mallory")
	w.hostActor(t, "agt_person", person, hostedSettings)
	getenv := env("CONFIG", t.TempDir(), "DATABASE_URL", w.dbURL, "CORE_BASE_URL", w.coreURL, "KMS_KEY_ID", w.kms)
	code, out, errs := runCmd(t, getenv, "check", "--live")
	if code != exitFailure || !strings.Contains(out, "agent agt_person: FAILED: me_get: its token is not an agent's in Core") {
		t.Errorf("check --live of a hosted agent on a person's token: %d\n%s%s", code, out, errs)
	}
	for _, c := range w.fc.Calls() {
		if c.ActorID == person.ID && c.Tool != "me_get" {
			t.Errorf("check --live called %s with the person's token", c.Tool)
		}
	}
}

// TestCheckLiveChecksTheOwner: check --live holds a hosted agent's owner
// to what run does, and fails one Core names another owner for, or none,
// with the state run would give it and what that state says, naming no one;
// nothing more is read with its token.
func TestCheckLiveChecksTheOwner(t *testing.T) {
	w := newRegistryWorld(t)
	ken := w.fc.AddPerson("Ken")
	w.hostAs(t, "agt_kens", w.agents[0], ken.ID, hostedSettings)
	unowned, err := w.fc.AddAgent("Nobody's helper", w.yuki.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.fc.SetOwner(unowned.ID, ""); err != nil {
		t.Fatal(err)
	}
	if unowned.Token, err = w.fc.IssueToken(unowned.ID); err != nil {
		t.Fatal(err)
	}
	w.hostActor(t, "agt_nobodys", unowned, hostedSettings)
	getenv := env("CONFIG", t.TempDir(), "DATABASE_URL", w.dbURL, "CORE_BASE_URL", w.coreURL, "KMS_KEY_ID", w.kms)
	code, out, errs := runCmd(t, getenv, "check", "--live")
	for _, want := range []string{
		"agent agt_kens: FAILED: owner_changed: the agent's owner in Core is no longer the person who connected it here: its owner must connect it again",
		"agent agt_nobodys: FAILED: owner_changed: Core names no owner for the agent now",
		"2 of 2 agents failed the live check",
	} {
		if !strings.Contains(out+errs, want) {
			t.Errorf("check --live does not say %q: %d\n%s%s", want, code, out, errs)
		}
	}
	if code != exitFailure {
		t.Errorf("check --live: %d", code)
	}
	for _, id := range []string{w.yuki.ID, ken.ID} {
		if strings.Contains(out+errs, id) {
			t.Errorf("check --live names an owner's id:\n%s%s", out, errs)
		}
	}
	for _, c := range w.fc.Calls() {
		if (c.ActorID == w.agents[0].ID || c.ActorID == unowned.ID) && c.Tool != "me_get" {
			t.Errorf("check --live called %s with the token of an agent whose owner does not pass", c.Tool)
		}
	}
}

// TestBuildIsNotHeldUpByTheDatabase: a read of the registry that the
// database does not answer (here a lock held on it) ends at its timeout,
// as an error, with the hosted agents as last built: SIGHUP, which waits
// for it, and the signals after SIGHUP, are held up no longer.
func TestBuildIsNotHeldUpByTheDatabase(t *testing.T) {
	w := newRegistryWorld(t)
	w.host(t, "agt_ok", 0, hostedSettings)
	h := &hosting{env: config.Env{CoreBaseURL: w.coreURL}, pg: w.st, log: slog.New(slog.DiscardHandler), yaml: &config.Config{}}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cfg, _, err := h.build(t.Context()); err != nil || len(cfg.Agents) != 1 {
		t.Fatalf("the first build: %v, %+v", err, cfg)
	}

	defer func(d time.Duration) { registryTimeout = d }(registryTimeout)
	registryTimeout = 200 * time.Millisecond
	lock, err := pgx.Connect(t.Context(), w.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close(context.Background()) }()
	tx, err := lock.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `LOCK TABLE registry_rev IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		cfg *config.Config
		err error
	}
	done := make(chan result, 1)
	go func() {
		cfg, _, err := h.build(t.Context())
		done <- result{cfg, err}
	}()
	select {
	case r := <-done:
		if r.err == nil || len(r.cfg.Agents) != 1 || r.cfg.Agents[0].ID != "agt_ok" {
			t.Errorf("a build the database does not answer: %v, %+v", r.err, r.cfg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a build the database does not answer did not end")
	}
}
