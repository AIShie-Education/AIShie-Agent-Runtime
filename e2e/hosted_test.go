package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// hostedAgentAnswers is M2's registry against the real Core: a runtime
// runs, with its state in PostgreSQL and no agent at all; Yuki's agent is
// connected to it as the API does it, its token and her model key sealed
// in the runtime's database with its row; the registry's notification puts
// it in force at once, and it answers Yuki, having opened its token and
// key from the database, and never logging either. Core's me.get names
// its owner: the row that names Yuki is marked verified, and an agent of
// hers whose row names Ken does not run. Sato uploads his lecture slides
// as a .pptx, and asked about one, Yuki's helper reads them through Core
// and answers from the runtime's text of them.
func hostedAgentAnswers(t *testing.T, w *world) {
	st, dbURL := runtimeStore(t)
	v, kek := keyring(t)
	w.addSecret("the key that seals the hosted runtime's secrets", kek)
	m := newModel(t, documentsResponder)
	rt := w.startHosted(t, m, st, v, &config.Config{})

	// Connected, as the API connects an agent.
	id := "agt_" + uuid.NewString()
	tenant := "ten_" + w.yuki.id
	seal := func(kind, plaintext string) store.Secret {
		s, err := v.Seal(context.Background(), store.Secret{ID: vault.NewSecretID(), TenantID: tenant, Kind: kind, CreatedBy: w.yuki.id}, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tok, key := seal(store.SecretCoreToken, w.own.token), seal(store.SecretModelKey, w.modelKey)
	settings, err := json.Marshal(map[string]any{
		// OpenAI's own endpoint, as a hosted agent must: the runtime's
		// transport takes its calls to the scripted model.
		"model":   map[string]any{"adapter": "openai_chat", "model": "e2e-model", "key_source": "own", "params": map[string]any{"max_output_tokens": 500}},
		"polling": polling(),
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 60}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateHostedAgent(t.Context(), store.HostedAgent{
		ID: id, CoreActorID: w.own.id, OwnerActorID: w.yuki.id, TenantID: tenant, DisplayName: "Yuki's helper",
		TokenSecretID: tok.ID, TokenHint: tok.Hint, KeySecretID: key.ID, KeyHint: key.Hint, Settings: settings,
	}, tok, key)
	if err != nil {
		t.Fatal(err)
	}
	if want := w.own.token[:len("ais_")+12] + "…"; tok.Hint != want {
		t.Error("the token's hint is not ais_ and its public prefix")
	}
	rt.waitPolling(id)

	const q = "Can my hosted helper answer?"
	conv, msg := w.ask(t, w.yuki, w.own.member, q)
	answer := w.waitAnswer(t, w.yuki, conv, w.own.member)
	if !strings.HasPrefix(answer.text(), "Answer: "+q) || answer.replyTo() != msg {
		t.Errorf("the answer is %q in reply to %s; want one to %q in reply to %s", answer.text(), answer.replyTo(), q, msg)
	}
	var keyed bool
	for _, req := range m.Requests() {
		if req.Header.Get("Authorization") == "Bearer "+w.modelKey {
			keyed = true
		}
	}
	if !keyed {
		t.Error("no model request carried Yuki's key, opened from the database")
	}
	if at := rt.attempt(id, answerKey(conv, msg, 1)); at == nil || at.State != store.AttemptExecuted {
		t.Errorf("the attempt under answer:{x}:{m}:1 is %s", attemptState(at))
	}

	// The lecture slides, a .pptx in Core: read by the runtime, never the
	// model, and given to it as their text, slides and notes.
	w.upload(t, w.sato, "Week 3 slides", doctexttest.PPTXType, weekThreeSlides)
	slidesConv, slidesMsg := w.ask(t, w.yuki, w.own.member, slideQuestion)
	slides := w.waitAnswer(t, w.yuki, slidesConv, w.own.member)
	if want := "From the pptx: ## Slide 2: 排序的複雜度\n- 合併排序：O(n log n)\n  - 最壞情況也是 O(n log n)\nNotes: Ask who has seen quicksort."; slides.text() != want ||
		slides.replyTo() != slidesMsg {
		t.Errorf("the answer about the slides is %q in reply to %s; want %q in reply to %s", slides.text(), slides.replyTo(), want, slidesMsg)
	}
	for _, req := range m.Requests() {
		for _, msg := range req.Messages {
			if strings.Contains(msg.Text(), "download_url") || strings.Contains(msg.Text(), "/v1/blobs/") {
				t.Error("a model request holds the slides' download URL")
			}
		}
	}

	// Core's me.get named Yuki as its owner, as its row does: the row is
	// marked verified, which restarted nothing.
	row, err := st.HostedAgent(t.Context(), id)
	if err != nil || !row.OwnerVerified {
		t.Errorf("the row of the agent whose owner Core named: %+v, %v", row, err)
	}
	started := 0
	for _, line := range strings.Split(rt.log.String(), "\n") {
		if strings.Contains(line, `"msg":"agent started"`) && strings.Contains(line, `"agent":"`+id+`"`) {
			started++
		}
	}
	if started != 1 {
		t.Errorf("the hosted agent started %d times", started)
	}

	// Another agent of Yuki's, connected by Ken, who held its token
	// without owning it: Core names Yuki, so it does not run, and its state
	// names neither of them.
	second := w.newAgent(t, w.yuki, "Yuki's second helper", "")
	w.addSecret("the token of Yuki's second agent", second.token)
	id2 := "agt_" + uuid.NewString()
	tok2, key2 := seal(store.SecretCoreToken, second.token), seal(store.SecretModelKey, w.modelKey)
	_, err = st.CreateHostedAgent(t.Context(), store.HostedAgent{
		ID: id2, CoreActorID: second.id, OwnerActorID: w.ken.id, TenantID: tenant, DisplayName: "Yuki's second helper",
		TokenSecretID: tok2.ID, TokenHint: tok2.Hint, KeySecretID: key2.ID, KeyHint: key2.Hint, Settings: settings,
	}, tok2, key2)
	if err != nil {
		t.Fatal(err)
	}
	var changed store.AgentState
	eventually(t, answerWait, "the agent Ken connected stopped as owner_changed", func() bool {
		states, err := st.AgentStates(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range states {
			if s.AgentID == id2 {
				changed = s
			}
		}
		return changed.State == store.AgentOwnerChanged
	})
	for _, who := range []string{w.yuki.id, w.ken.id, second.id} {
		if strings.Contains(changed.Detail, who) {
			t.Errorf("the owner_changed state names an actor: %q", changed.Detail)
		}
	}
	if rt.inboxPolls(id2) != 0 {
		t.Error("the agent Ken connected polled its inbox")
	}
	if row, err := st.HostedAgent(t.Context(), id2); err != nil || row.OwnerVerified {
		t.Errorf("the row of the agent Ken connected: %+v, %v", row, err)
	}

	// Paused, it stops; its state says so.
	if _, err := st.SetHostedAgentPaused(t.Context(), id, true, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, answerWait, "the hosted agent paused", func() bool {
		for _, a := range rt.sup.Status() {
			if a.AgentID == id {
				return a.Paused && !a.Running
			}
		}
		return false
	})

	// What the runtime keeps and shows holds none of it in plaintext: not
	// its database (the sealed secrets, the registry, the states, the
	// ledger, every table), nor its status. The logs are searched with
	// every other log, the key that seals the secrets among what is
	// searched for.
	status, err := json.Marshal(rt.sup.Status())
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{"the hosted runtime's database": dumpDatabase(t, dbURL), "the hosted runtime's status": string(status)} {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty: nothing was searched", what)
		}
		for _, s := range w.secrets() {
			for _, part := range secretParts(s.value) {
				if strings.Contains(text, part) || strings.Contains(text, hex.EncodeToString([]byte(part))) {
					t.Errorf("%s holds %s", what, s.what)
				}
			}
		}
	}
}

// dumpDatabase is every row of every table of the database at dbURL, as
// text, bytes in hex: what a backup of it would hold.
func dumpDatabase(t *testing.T, dbURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, table := range tables {
		var text string
		q := "SELECT coalesce(string_agg(t::text, E'\\n'), '') FROM " + pgx.Identifier{table}.Sanitize() + " t"
		if err := conn.QueryRow(ctx, q).Scan(&text); err != nil {
			t.Fatal(err)
		}
		b.WriteString(table + "\n" + text + "\n")
	}
	return b.String()
}

// runtimeStore is a store of the runtime's own in PostgreSQL, on a scratch
// database of the server at TEST_DATABASE_URL (the local one by default),
// migrated up, closed and dropped when t ends. Without a server it skips,
// or fails when CI is true, as the other end-to-end tests do without Core.
func runtimeStore(t *testing.T) (*pgstore.Store, string) {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		admin = "postgres:///postgres"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); !ci {
			t.Skipf("the runtime's PostgreSQL (TEST_DATABASE_URL) cannot be reached: %v", err)
		}
		t.Fatalf("the runtime's PostgreSQL (TEST_DATABASE_URL) cannot be reached: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "aishie_e2e_t_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, admin); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	if err := pgstore.Migrate(u.String(), pgstore.Up); err != nil {
		t.Fatal(err)
	}
	st, err := pgstore.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, u.String()
}

// keyring is a vault on a key of its own, in a directory of t's, and the
// key as its file holds it.
func keyring(t *testing.T) (*vault.Vault, string) {
	t.Helper()
	dir := t.TempDir()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	text := base64.StdEncoding.EncodeToString(k)
	if err := os.WriteFile(filepath.Join(dir, "v1"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	return v, text
}

// startHosted runs the runtime as run does with the registry on: its state
// in st, its configuration yaml ∪ registry, rebuilt by the registry's
// watcher, its sealed secrets opened with v. Its calls to OpenAI's own
// endpoint go to m.
func (w *world) startHosted(t *testing.T, m *fakellm.Server, st *pgstore.Store, v *vault.Vault, yaml *config.Config) *instance {
	t.Helper()
	rt := &instance{t: t, reg: prometheus.NewRegistry(), st: st, log: &logBuffer{}, raw: &logBuffer{}}
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	logger := slog.New(teeHandler{
		redact.NewHandler(slog.NewJSONHandler(rt.log, opts), nil),
		slog.NewJSONHandler(rt.raw, opts),
	})
	w.addLog(t.Name(), rt.log.String)
	w.addLog(t.Name()+" (before redaction)", rt.raw.String)

	target, err := url.Parse(m.URL())
	if err != nil {
		t.Fatal(err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	o := registry.Options{CoreBaseURL: w.api.base}
	cfg, rev, err := registry.Build(t.Context(), yaml, st, o)
	if err != nil {
		t.Fatal(err)
	}
	rt.sup, err = worker.NewSupervisor(worker.Options{
		Config: cfg, Store: st, Metrics: metrics.New(rt.reg), Log: logger, WorkerID: "hosted-w1",
		Secrets:    secrets.Resolver{Dir: w.secretsDir, Sealed: vault.Opener{Vault: v, Store: st}},
		HTTPClient: &http.Client{Transport: toModel{next: tr, target: target}},
		// A hosted agent's model calls go through a client that follows no
		// redirect; the scripted model is on loopback, which the dial guard
		// of production refuses, so the guard is left out here.
		HostedHTTPClient: modelClient(tr, target),
		CoreRetry:        core.RetryOptions{Base: 50 * time.Millisecond, Max: time.Second},
		Timing: worker.Timing{
			LeaseEvery: time.Second, LeaseTTL: 5 * time.Second, Restart: 100 * time.Millisecond, RestartMax: time.Second,
			ModelBackoff: 50 * time.Millisecond, ModelBackoffMax: 500 * time.Millisecond,
			HoldBack: time.Second, HoldBackMax: 5 * time.Second, RetryLater: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	watcher := &registry.Watcher{
		Rev: st.RegistryRev, Listen: st.ListenRegistry, Log: logger,
		Poll: time.Hour, // the notification alone must do it
		Rebuild: func(ctx context.Context) (int64, error) {
			cfg, rev, err := registry.Build(ctx, yaml, st, o)
			if err != nil {
				return 0, err
			}
			rt.sup.Update(cfg)
			return rev, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	supDone, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(supDone)
		if err := rt.sup.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	go func() { defer close(watchDone); watcher.Run(ctx, rev) }()
	t.Cleanup(func() {
		cancel()
		for _, done := range []chan struct{}{supDone, watchDone} {
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the runtime did not stop within 30 s")
			}
		}
		tr.CloseIdleConnections()
		if t.Failed() {
			t.Logf("the log of the hosted runtime (redacted):\n%s", tail(rt.log.String(), 200))
		}
	})
	return rt
}

// modelClient is the hosted-model client of the tests: no redirect
// followed, as in production, and OpenAI's own endpoint taken to the
// scripted model at target, which is on loopback, where production's dial
// guard would not let it.
func modelClient(tr http.RoundTripper, target *url.URL) *http.Client {
	return netguard.NoRedirects(&http.Client{Transport: toModel{next: tr, target: target}})
}

// toModel takes the calls to OpenAI's own endpoint to the scripted model.
type toModel struct {
	next   http.RoundTripper
	target *url.URL
}

func (tm toModel) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.openai.com" {
		return tm.next.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = tm.target.Scheme, tm.target.Host, tm.target.Host
	return tm.next.RoundTrip(r)
}
