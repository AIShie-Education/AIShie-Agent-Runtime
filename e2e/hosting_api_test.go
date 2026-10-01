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
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// hostingThroughTheAPI is M2's API against the real Core, hosting by an
// agent's id (the contract's §10.2; AIShie-Core #52): Yuki hosts her
// own agent, a runtime agent, on the school's runtime from the web UI, as
// the front end does it. The runtime runs in-process, its state in
// PostgreSQL, with its API; its calls to OpenAI's own endpoint go to the
// scripted model. Nobody pastes a token: the API asks Core, with the
// runtime's own credential, whether the agent is hers and may be hosted
// (inspect), and hosts it by its id (needs_model); she tries her key and
// gives her model and key (If-Match); the worker is issued the agent's
// token by its id, and it answers her in the site. Paused, its token is
// revoked in Core and it calls Core no more; resumed, it is issued another.
// Revoked in Core by its owner, it needs a token, and her asking for one
// has it issued another. Deleted, its token is revoked in Core, the next GET
// is 404, and the store holds nothing of it but its ledger. Another's
// agent, a person, and her mcp agent are refused. No answer, log or row
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
	agentBody := func(id string) string {
		b, _ := json.Marshal(map[string]string{"agent_id": id})
		return string(b)
	}
	// inCore is the agent as the site's runtime reads it in Core.
	inCore := func() *core.RuntimeAgent {
		t.Helper()
		view, err := w.runtimeService().Agent(t.Context(), w.own.id)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}

	// What the front end reads first.
	var info api.Info
	decodeAs(answer{code: 200, body: mustGet(t, a, "/runtime/api/v1/info")}, 200, &info)
	if !info.Features.HostByID || !info.Features.OwnKey || info.Features.SchoolKey || info.Audience != audience {
		t.Errorf("GET /info: %+v", info)
	}
	var me api.Me
	decodeAs(call("GET", "me", ""), 200, &me)
	if me.ActorID != w.yuki.id || me.HostedAgents != 0 {
		t.Errorf("GET /me: %+v", me)
	}

	// Refused (the routes that ask Core allow five at once): Sato's tutor,
	// someone else's; her mcp agent, which her own tools reach, never the
	// runtime.
	if an := call("POST", "agents/inspect", agentBody(w.tutor.id)); an.code != 404 || reason(an) != "agent_not_found" {
		t.Errorf("another's agent: %d %s", an.code, an.body)
	}
	tools := w.createAgent(t, w.yuki, "Yuki's own tools", "mcp")
	var mcp api.Inspection
	decodeAs(call("POST", "agents/inspect", agentBody(tools)), 200, &mcp)
	if mcp.Hosting != "mcp" || mcp.Hostable || mcp.Reason == nil || *mcp.Reason != "mcp_agent" || mcp.SiteChat {
		t.Errorf("inspect of her mcp agent: %+v", mcp)
	}
	if an := call("POST", "agents", agentBody(tools)); an.code != 422 || reason(an) != "mcp_agent" {
		t.Errorf("hosting her mcp agent: %d %s", an.code, an.body)
	}

	// Inspect: hers, a runtime agent, with her delegate seat, hosted
	// nowhere yet.
	var ins api.Inspection
	decodeAs(call("POST", "agents/inspect", agentBody(w.own.id)), 200, &ins)
	if ins.CoreActorID != w.own.id || ins.OwnerActorID != w.yuki.id || ins.Hosting != "runtime" || !ins.Hostable || ins.Reason != nil ||
		ins.LiveSeats != 1 || ins.SiteChat || ins.Hosted != nil {
		t.Fatalf("inspect: %+v", ins)
	}
	// Hosted by its id: it needs a model, and the API was issued nothing.
	var agent api.HostedAgent
	decodeAs(call("POST", "agents", agentBody(w.own.id)), http.StatusCreated, &agent)
	if agent.Status != api.StatusNeedsModel || agent.Version != 1 || agent.CoreActorID != w.own.id || agent.OwnerActorID != w.yuki.id {
		t.Fatalf("POST /agents: %+v", agent)
	}
	id := agent.ID
	if view := inCore(); view.RuntimeToken != nil || view.SiteChat {
		t.Errorf("hosted without a model, the agent in Core: %+v", view)
	}

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
	// The worker was issued its token by its id, and keeps it sealed in
	// its row: the one Core holds live for it.
	issued := func() string {
		t.Helper()
		row, err := st.HostedAgent(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		view := inCore()
		if !row.TokenIssued || view.RuntimeToken == nil || view.RuntimeToken.CredentialID != row.TokenCredentialID || !view.SiteChat {
			t.Fatalf("the row %+v; the agent in Core %+v", row, view)
		}
		return row.TokenCredentialID
	}
	first := issued()

	// It answers her in the site.
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

	// Paused: its token is revoked in Core, nobody may ask it, and it calls
	// Core no more. Resumed: it is issued another, and runs again.
	var paused api.Paused
	decodeAs(call("POST", "agents/"+id+"/pause", ""), 200, &paused)
	if paused.Status != api.StatusPaused || paused.Revocation.Outcome != api.RevocationRevoked || paused.Revocation.Problem != nil {
		t.Errorf("paused: %+v", paused)
	}
	if view := inCore(); view.RuntimeToken != nil || view.SiteChat {
		t.Errorf("paused, the agent in Core: %+v", view)
	}
	if r, err := w.api.send(t.Context(), w.yuki.token, "POST", w.path("/conversations"),
		map[string]any{"respondent_member_id": w.own.member, "body": "Are you there?"}, ""); err != nil || r.Error == nil ||
		r.Error.Details["reason"] != "agent_not_hosted" {
		t.Errorf("asking the paused agent: %v %v", r, err)
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
	second := issued()
	if second == first {
		t.Error("resumed, the agent runs with the token revoked as it was paused")
	}

	// Its owner revokes its token in Core: it needs one, and her asking
	// for one has the worker issued another.
	w.api.call(t, http.StatusOK, w.yuki.token, "POST", "/v1/me/agents/"+w.own.id+"/credentials/"+second+"/revoke", nil)
	needs := waitStatus(api.StatusNeedsToken)
	if needs.Problem == nil || needs.Problem.Reason != "token_refused" {
		t.Errorf("revoked in Core: %+v", needs)
	}
	decodeAs(call("POST", "agents/"+id+"/token", ""), 200, &agent)
	waitStatus(api.StatusRunning)
	third := issued()
	if third == second {
		t.Error("asked for a new token, the agent runs with the one revoked")
	}

	// Deleted: its token revoked in Core; nothing kept of it.
	var deleted api.Deleted
	decodeAs(call("DELETE", "agents/"+id, ""), 200, &deleted)
	if deleted.Deleted.ID != id || deleted.Deleted.CoreActorID != w.own.id || deleted.Revocation.Outcome != api.RevocationRevoked {
		t.Errorf("DELETE: %+v", deleted)
	}
	if view := inCore(); view.RuntimeToken != nil || view.SiteChat {
		t.Errorf("deleted, the agent in Core: %+v", view)
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
		if strings.Contains(text, "ais_") && what == "the API's answers" {
			t.Errorf("%s hold a Core token's prefix", what)
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
