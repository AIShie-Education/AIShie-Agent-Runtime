package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// hostingThroughTheAPI is M2's API against the real Core (the contract's
// §10.2): Yuki hosts her own agent on the school's runtime from the web UI,
// as the front end does it. The runtime runs in-process, its state in
// PostgreSQL, with its API; its calls to OpenAI's own endpoint go to the
// scripted model. Yuki's session issues her agent a token labelled "AIshie
// runtime"; the API inspects and connects it (needs_model), tries her key,
// and takes her model and key (If-Match); the worker runs it, and it
// answers her. Paused, it calls Core no more; resumed, it runs again. A
// second token replaces the first, which the new one revokes in Core; the
// agent deleted, its token is revoked in Core with itself, the next GET is
// 404, and the store holds nothing of it but its ledger. A person's
// session and another's agent's token are refused. No answer, log or row
// holds a token, the key or an assertion.
func hostingThroughTheAPI(t *testing.T, w *world) {
	audience := os.Getenv("E2E_RUNTIME_AUDIENCE")
	if audience == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_RUNTIME_AUDIENCE is not set, and CI is: start Core with scripts/ci-core.sh")
		}
		t.Skip("E2E_RUNTIME_AUDIENCE is not set: this Core makes no assertion for a runtime (scripts/ci-core.sh sets it)")
	}
	st, dbURL := runtimeStore(t)
	v, kek := keyring(t)
	w.addSecret("the key that seals the hosted runtime's secrets", kek)
	m := newModel(t, fakellm.DefaultResponder)
	// The runtime's defaults poll as the other tests do, and hosted agents
	// take them.
	yaml := &config.Config{Runtime: config.Runtime{Defaults: map[string]any{
		"polling": polling(), "budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 60}},
	}}}
	rt := w.startHosted(t, m, st, v, yaml)
	target, err := url.Parse(m.URL())
	if err != nil {
		t.Fatal(err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	prices, err := pricing.Parse([]byte("version: e2e\nprices:\n  - {provider: openai, model: e2e-model, from: 2025-01-01, usd_per_mtok: {input: 1, output: 2}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := w.startAPI(t, st, audience, func(o *api.Options) {
		o.Vault, o.Actors, o.Hosting, o.ModelHTTP = v, rt.sup, staticHosting{yaml: yaml, prices: prices}, modelClient(tr, target)
	})
	as := func() string { return w.assertion(t, w.yuki.token, "", audience) }
	type answer struct {
		code int
		hdr  http.Header
		body []byte
	}
	call := func(method, path, body string, headers ...string) answer {
		t.Helper()
		code, hdr, raw := a.do(t, method, "/runtime/api/v1/"+path, as(), body, headers...)
		return answer{code, hdr, raw}
	}
	decodeAs := func(an answer, want int, v any) {
		t.Helper()
		if an.code != want {
			t.Fatalf("%d, want %d: %s", an.code, want, an.body)
		}
		if err := json.Unmarshal(an.body, v); err != nil {
			t.Fatalf("%v: %s", err, an.body)
		}
	}
	reason := func(an answer) string {
		var e struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(an.body, &e)
		s, _ := e.Error.Details["reason"].(string)
		return s
	}

	// What the front end reads first.
	var info api.Info
	decodeAs(answer{code: 200, body: mustGet(t, a, "/runtime/api/v1/info")}, 200, &info)
	if !info.Features.ConnectByToken || !info.Features.OwnKey || info.Features.SchoolKey || info.Audience != audience {
		t.Errorf("GET /info: %+v", info)
	}
	var me api.Me
	decodeAs(call("GET", "me", ""), 200, &me)
	if me.ActorID != w.yuki.id || me.HostedAgents != 0 {
		t.Errorf("GET /me: %+v", me)
	}

	// Yuki's session issues her agent a token for the runtime.
	issue := func() (token, credential string) {
		out := result[struct {
			Token        string `json:"token"`
			CredentialID string `json:"credential_id"`
		}](t, w.api, w.yuki.token, "POST", "/v1/me/agents/"+w.own.id+"/tokens", map[string]any{"label": "AIshie runtime"})
		w.addSecret("a token of Yuki's agent issued for the runtime", out.Token)
		return out.Token, out.CredentialID
	}
	first, firstCred := issue()
	tokenBody := func(token string) string {
		b, _ := json.Marshal(map[string]string{"token": token, "core_actor_id": w.own.id})
		return string(b)
	}

	// Refused: her own session, a person's; the tutor's token, another's
	// agent's.
	if an := call("POST", "agents/inspect", tokenBody(w.yuki.token)); an.code != 422 || reason(an) != "token_not_agent" {
		t.Errorf("a person's session: %d %s", an.code, an.body)
	}
	if an := call("POST", "agents/inspect", `{"token":"`+w.tutor.token+`"}`); an.code != 403 || reason(an) != "not_owner" {
		t.Errorf("another's agent's token: %d %s", an.code, an.body)
	}

	// Inspect: her delegate seat.
	var ins api.Inspection
	decodeAs(call("POST", "agents/inspect", tokenBody(first)), 200, &ins)
	if ins.CoreActorID != w.own.id || ins.OwnerActorID != w.yuki.id || ins.Hosted != nil || len(ins.Seats) != 1 ||
		ins.Seats[0].Kind != "delegate" || !ins.Seats[0].Answers || ins.OtherTokens == nil {
		t.Fatalf("inspect: %+v", ins)
	}
	// Connect: needs a model.
	var agent api.HostedAgent
	decodeAs(call("POST", "agents", tokenBody(first)), http.StatusCreated, &agent)
	if agent.Status != api.StatusNeedsModel || agent.Version != 1 || agent.Token.Prefix != ins.Token.Prefix {
		t.Fatalf("connect: %+v", agent)
	}
	id := agent.ID

	// Her key tried: the scripted model takes it.
	keyBody, _ := json.Marshal(map[string]string{"provider": "openai", "model": "e2e-model", "key": w.modelKey})
	var kt api.KeyTest
	decodeAs(call("POST", "keys/test", string(keyBody)), 200, &kt)
	if kt.Result != "ok" {
		t.Errorf("keys/test: %+v", kt)
	}
	// Her model and key, at the version she read.
	patch, _ := json.Marshal(map[string]any{"model": map[string]any{"own": map[string]any{"provider": "openai", "model": "e2e-model"}},
		"own_key": map[string]any{"value": w.modelKey}})
	decodeAs(call("PATCH", "agents/"+id, string(patch), "If-Match", `"1"`), 200, &agent)
	if agent.Version != 2 || agent.Model.Own == nil || !agent.Model.Own.PriceKnown || agent.OwnKey == nil {
		t.Errorf("PATCH: %+v", agent)
	}
	waitStatus := func(want string) api.HostedAgent {
		t.Helper()
		var got api.HostedAgent
		eventually(t, answerWait, "the hosted agent "+want, func() bool {
			an := call("GET", "agents/"+id, "")
			return an.code == 200 && json.Unmarshal(an.body, &got) == nil && got.Status == want
		})
		return got
	}
	waitStatus(api.StatusRunning)

	// It answers her.
	const q = "Does the agent I host through the API answer?"
	conv, msg := w.ask(t, w.yuki, w.own.member, q)
	ans := w.waitAnswer(t, w.yuki, conv, w.own.member)
	if !strings.HasPrefix(ans.text(), "Answer: "+q) || ans.replyTo() != msg {
		t.Errorf("the answer is %q in reply to %s", ans.text(), ans.replyTo())
	}
	// Today's use counts it (its cost is unknown: the worker here has no
	// price table). The ledger has it once the worker has recorded the
	// answer, which is after Core took it: waited for, not read at once.
	var used api.HostedAgent
	eventually(t, answerWait, "today's use counting the answer", func() bool {
		an := call("GET", "agents/"+id, "")
		return an.code == 200 && json.Unmarshal(an.body, &used) == nil && used.Today.Answers >= 1
	})
	if used.Status != api.StatusRunning || used.Today.CostUSD != "0.000000" || len(used.Seats) != 1 {
		t.Errorf("after an answer: %+v", used)
	}

	// Paused: it calls Core no more. Resumed: it runs again.
	decodeAs(call("POST", "agents/"+id+"/pause", ""), 200, &agent)
	if agent.Status != api.StatusPaused {
		t.Errorf("paused: %+v", agent)
	}
	eventually(t, answerWait, "the paused agent stopped", func() bool {
		for _, s := range rt.sup.Status() {
			if s.AgentID == id {
				return s.Paused && !s.Running
			}
		}
		return false
	})
	time.Sleep(500 * time.Millisecond) // a poll in flight lands
	polls := rt.inboxPolls(id)
	time.Sleep(2 * time.Second)
	if more := rt.inboxPolls(id) - polls; more != 0 {
		t.Errorf("%v inbox polls while paused", more)
	}
	decodeAs(call("POST", "agents/"+id+"/resume", ""), 200, &agent)
	waitStatus(api.StatusRunning)

	// A second token replaces the first, which it revokes in Core.
	second, secondCred := issue()
	var replaced api.TokenReplaced
	decodeAs(call("PUT", "agents/"+id+"/token", `{"token":"`+second+`"}`), 200, &replaced)
	if replaced.PreviousToken.Revocation != "revoked" || replaced.PreviousToken.Prefix != ins.Token.Prefix || replaced.Agent.Token.Prefix == ins.Token.Prefix {
		t.Errorf("PUT /token: %+v", replaced)
	}
	creds := result[struct {
		Credentials []struct {
			ID        string  `json:"id"`
			RevokedAt *string `json:"revoked_at"`
		} `json:"credentials"`
	}](t, w.api, w.yuki.token, "GET", "/v1/me/agents/"+w.own.id+"/credentials", nil).Credentials
	revoked := map[string]bool{}
	for _, c := range creds {
		revoked[c.ID] = c.RevokedAt != nil
	}
	if !revoked[firstCred] || revoked[secondCred] {
		t.Errorf("Core's credentials after the replacement: %+v", creds)
	}
	waitStatus(api.StatusRunning)

	// Deleted: its token revoked in Core with itself; nothing kept of it.
	var deleted api.Deleted
	decodeAs(call("DELETE", "agents/"+id, ""), 200, &deleted)
	if deleted.Deleted.ID != id || deleted.Token.Revocation != "revoked" {
		t.Errorf("DELETE: %+v", deleted)
	}
	creds = result[struct {
		Credentials []struct {
			ID        string  `json:"id"`
			RevokedAt *string `json:"revoked_at"`
		} `json:"credentials"`
	}](t, w.api, w.yuki.token, "GET", "/v1/me/agents/"+w.own.id+"/credentials", nil).Credentials
	for _, c := range creds {
		if c.ID == secondCred && c.RevokedAt == nil {
			t.Error("the deleted agent's token is not revoked in Core")
		}
	}
	if an := call("GET", "agents/"+id, ""); an.code != 404 || reason(an) != "agent_not_found" {
		t.Errorf("GET after DELETE: %d %s", an.code, an.body)
	}
	eventually(t, answerWait, "the deleted agent stopped", func() bool {
		for _, s := range rt.sup.Status() {
			if s.AgentID == id {
				return false
			}
		}
		return true
	})
	// What a worker wrote as it stopped is purged too, by housekeeping or
	// here: nothing of it is left but its ledger.
	if err := st.PurgeAgent(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HostedAgent(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row: %v", err)
	}
	if seats, _ := st.KnownSeats(context.Background(), id); len(seats) != 0 {
		t.Errorf("its seats: %+v", seats)
	}
	if sp, err := st.Spend(context.Background(), store.SpendScope{AgentID: id}, time.Now().Add(-time.Hour)); err != nil || sp.Answers < 1 {
		t.Errorf("its ledger: %+v %v", sp, err)
	}
	if secrets, err := st.ListSecrets(context.Background(), "", 100); err != nil || len(secrets) != 0 {
		t.Errorf("secrets left: %d %v", len(secrets), err)
	}

	// Nothing the runtime keeps or says holds a secret: its database, and
	// every answer of the API's (its logs are searched with the others').
	for what, text := range map[string]string{"the hosted runtime's database": dumpDatabase(t, dbURL), "the API's answers": a.answers.String()} {
		for _, s := range w.secrets() {
			for _, part := range secretParts(s.value) {
				if strings.Contains(text, part) || strings.Contains(text, hex.EncodeToString([]byte(part))) {
					t.Errorf("%s holds %s", what, s.what)
				}
			}
		}
	}
}

// staticHosting is the configuration in force, as the API reads it.
type staticHosting struct {
	yaml   *config.Config
	prices *pricing.Table
}

func (h staticHosting) YAML() *config.Config   { return h.yaml }
func (h staticHosting) Prices() *pricing.Table { return h.prices }

// mustGet is GET path's body, the request carrying no assertion.
func mustGet(t *testing.T, a *apiInstance, path string) []byte {
	t.Helper()
	code, raw := a.get(t, path, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, raw)
	}
	return raw
}
