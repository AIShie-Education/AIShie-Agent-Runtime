package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// schoolKey is the school's key in the tests' secret store: no log, answer
// or row may hold it.
const schoolKey = "sk-school-e2e-0123456789abcdefghijkl"

// schoolPlanThroughTheAPI is the school's AI plan (D8) against the real
// Core: the runtime offers one model on the school's key, which is a file
// in its secret store; Yuki connects her agent and chooses the offer, with
// no key of her own; it answers her with the school's key, at the offer's
// endpoint, which the operator chose. Her quota for the day, one answer,
// spent, her next question is given the plan's notice, in English and
// Chinese, with no model call. With her own model and key behind the
// plan, her next is answered with her key, and the school's quota stays
// where it was. No log, answer or row holds the school's key or its
// reference.
func schoolPlanThroughTheAPI(t *testing.T, w *world) {
	audience := os.Getenv("E2E_RUNTIME_AUDIENCE")
	if audience == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_RUNTIME_AUDIENCE is not set, and CI is: start Core with scripts/ci-core.sh")
		}
		t.Skip("E2E_RUNTIME_AUDIENCE is not set: this Core makes no assertion for a runtime (scripts/ci-core.sh sets it)")
	}
	st, dbURL := runtimeStore(t)
	v, kek := keyring(t)
	w.addSecret("the key that seals the school plan's runtime's secrets", kek)
	w.addSecret("the school's key", schoolKey)
	w.secretsDir = t.TempDir()
	keyFile := filepath.Join(w.secretsDir, "school", "keys", "e2e")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(schoolKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(t, fakellm.DefaultResponder)
	one := 1
	// The offer's endpoint is the scripted model's own address, as an
	// operator's gateway would be: the hosted-model client would not call
	// it, the runtime's own does.
	yaml := &config.Config{Runtime: config.Runtime{
		Defaults: map[string]any{"polling": polling(), "budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 60}}},
		School: config.School{
			Offers: []config.SchoolOffer{{ID: "standard", Label: "School AI (e2e)", Adapter: "openai_chat", Model: "e2e-school-model",
				BaseURL: m.URL(), KeyRef: "secret://school/keys/e2e"}},
			PerOwnerDay: config.Quota{Answers: &one},
		},
	}}
	rt := w.startHosted(t, m, st, v, yaml)
	a := w.startAPI(t, st, audience, func(o *api.Options) {
		o.Vault, o.Actors, o.Hosting = v, rt.sup, staticHosting{yaml: yaml}
	})
	type answer struct {
		code int
		body []byte
	}
	call := func(method, path, body string, headers ...string) answer {
		t.Helper()
		code, _, raw := a.do(t, method, "/runtime/api/v1/"+path, w.assertion(t, w.yuki.token, "", audience), body, headers...)
		return answer{code, raw}
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

	var info api.Info
	decodeAs(answer{200, mustGet(t, a, "/runtime/api/v1/info")}, 200, &info)
	if !info.Features.SchoolKey {
		t.Errorf("GET /info: %+v", info.Features)
	}
	var models api.Models
	decodeAs(call("GET", "models", ""), 200, &models)
	if o := models.SchoolKey.Offers; !models.SchoolKey.Offered || len(o) != 1 || o[0].ID != "standard" || o[0].Label != "School AI (e2e)" ||
		o[0].Model != "e2e-school-model" || models.SchoolKey.Limits.PerOwnerDay != 1 {
		t.Fatalf("GET /models' school_key: %+v", models.SchoolKey)
	}

	// Yuki connects her agent, and puts it on the school's plan.
	tok := result[struct {
		Token string `json:"token"`
	}](t, w.api, w.yuki.token, "POST", "/v1/me/agents/"+w.own.id+"/tokens", map[string]any{"label": "AIShie runtime"}).Token
	w.addSecret("a token of Yuki's agent issued for the school plan's runtime", tok)
	connect, _ := json.Marshal(map[string]string{"token": tok, "core_actor_id": w.own.id})
	var agent api.HostedAgent
	decodeAs(call("POST", "agents", string(connect)), http.StatusCreated, &agent)
	id := agent.ID
	decodeAs(call("PATCH", "agents/"+id, `{"model":{"school":{"offer":"standard"}}}`, "If-Match", `"1"`), 200, &agent)
	if agent.Model.School == nil || agent.Model.School.Label != "School AI (e2e)" || agent.Model.Own != nil || agent.OwnKey != nil ||
		agent.Today.School == nil || agent.Today.School.Limit != 1 {
		t.Fatalf("on the plan: %+v", agent)
	}
	waitStatus := func(want string, version int) {
		t.Helper()
		var got api.HostedAgent
		eventually(t, answerWait, "the hosted agent "+want, func() bool {
			an := call("GET", "agents/"+id, "")
			return an.code == 200 && json.Unmarshal(an.body, &got) == nil && got.Status == want && got.Version == version
		})
	}
	waitStatus(api.StatusRunning, 2)
	calls := func() []fakellm.ChatRequest { return m.Requests() }

	// It answers her, on the school's key.
	const q1 = "Does the school's AI answer me?"
	conv, _ := w.ask(t, w.yuki, w.own.member, q1)
	if ans := w.waitAnswer(t, w.yuki, conv, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q1) {
		t.Fatalf("the plan's answer: %q", ans.text())
	}
	reqs := calls()
	if len(reqs) == 0 || reqs[len(reqs)-1].Model != "e2e-school-model" || reqs[len(reqs)-1].Header.Get("Authorization") != "Bearer "+schoolKey {
		t.Fatalf("the offer's call: %d calls", len(reqs))
	}
	var used api.HostedAgent
	eventually(t, answerWait, "the plan's use counting the answer", func() bool {
		an := call("GET", "agents/"+id, "")
		return an.code == 200 && json.Unmarshal(an.body, &used) == nil && used.Today.School != nil && used.Today.School.Used == 1
	})

	// Her day's quota spent: the plan's notice, and no model call.
	before := len(calls())
	conv2, _ := w.ask(t, w.yuki, w.own.member, "And again?")
	want := config.SchoolQuotaTextEn + "\n\n" + config.SchoolQuotaTextZhHant
	if ans := w.waitAnswer(t, w.yuki, conv2, w.own.member); ans.text() != want {
		t.Errorf("over the plan's quota: %q", ans.text())
	}
	if n := len(calls()) - before; n != 0 {
		t.Errorf("%d model calls over the quota", n)
	}

	// Her own model and key behind the plan: her key answers.
	own, _ := json.Marshal(map[string]any{"model": map[string]any{"own": map[string]any{"provider": "openai", "model": "e2e-own-model"}},
		"own_key": map[string]any{"value": w.modelKey}})
	decodeAs(call("PATCH", "agents/"+id, string(own), "If-Match", `"2"`), 200, &agent)
	if agent.Model.School == nil || !agent.Model.School.Fallback || agent.Model.Own == nil {
		t.Fatalf("with her key behind the plan: %+v", agent)
	}
	waitStatus(api.StatusRunning, 3)
	const q3 = "And with my own key?"
	conv3, _ := w.ask(t, w.yuki, w.own.member, q3)
	if ans := w.waitAnswer(t, w.yuki, conv3, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q3) {
		t.Fatalf("the fallback's answer: %q", ans.text())
	}
	reqs = calls()
	if last := reqs[len(reqs)-1]; last.Model != "e2e-own-model" || last.Header.Get("Authorization") != "Bearer "+w.modelKey {
		t.Errorf("the fallback's call: model %q", last.Model)
	}
	eventually(t, answerWait, "today's use counting the fallback's answer", func() bool {
		an := call("GET", "agents/"+id, "")
		return an.code == 200 && json.Unmarshal(an.body, &used) == nil && used.Today.Answers >= 2
	})
	if used.Today.School == nil || used.Today.School.Used != 1 {
		t.Errorf("the plan's use after the fallback: %+v", used.Today.School)
	}

	// Nothing the runtime keeps or says holds the school's key, or refers
	// to it (its logs are searched with the others').
	for what, text := range map[string]string{"the runtime's database": dumpDatabase(t, dbURL), "the API's answers": a.answers.String()} {
		if strings.Contains(text, schoolKey) || strings.Contains(text, "school/keys") {
			t.Errorf("%s holds or refers to the school's key", what)
		}
	}
}
