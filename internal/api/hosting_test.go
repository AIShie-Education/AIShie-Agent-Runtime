package api

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// hostWorld is the API over a fake Core: Yuki and Ken, and Yuki's agent,
// seated as her delegate in CS101, a runtime agent; the API asks Core
// with the runtime's own credential, svc.
type hostWorld struct {
	*fixture
	t      *testing.T
	fc     *fakecore.Core
	srv    *httptest.Server
	v      *vault.Vault
	co     fakecore.Course
	yuki   fakecore.Actor
	ken    fakecore.Actor
	helper fakecore.Actor
	yukiM  fakecore.Member
	svc    fakecore.Token
	actors *fakeActors
	// tokens are every token the test handed out, which nothing may
	// repeat.
	tokens []string
}

// fakeActors is the worker's view of which agent runs as which actor.
type fakeActors struct {
	mu   sync.Mutex
	yaml map[string]string
}

func (a *fakeActors) ActorAgent(_, actorID string) (string, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.yaml[actorID]
	return id, false, ok
}

func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	dir := t.TempDir()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v1"), []byte(base64.StdEncoding.EncodeToString(k)), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newHostWorld(t *testing.T, o fakecore.Options, edit func(*Options)) *hostWorld {
	t.Helper()
	h := &hostWorld{t: t, v: testVault(t), actors: &fakeActors{yaml: map[string]string{}}}
	h.fixture = newFixture(t, func(opt *Options) {
		opt.Vault, opt.Actors = h.v, h.actors
		if edit != nil {
			edit(opt)
		}
	})
	// The fake Core keeps the API's time, so that what it says of when a
	// token was used is on the API's clock.
	if o.Now == nil {
		o.Now = h.clock
	}
	h.fc = fakecore.New(o)
	h.srv = httptest.NewServer(h.fc.Handler())
	t.Cleanup(h.srv.Close)
	h.s.o.CoreBaseURL, h.s.coreHTTP = h.srv.URL, coreClient(h.srv.Client())
	h.svc = h.fc.IssueRuntimeServiceToken("runtime")
	if h.s.o.Runtime == nil {
		h.s.o.Runtime = h.runtime(h.svc.Token)
	}
	// The tests' clock stands still: the token routes' allowance would run
	// out. TestTokenBucket holds it to its rate.
	h.s.token.reset(Rate{PerMinute: 60000, Burst: 10000})
	h.co = h.fc.AddCourse("CS101")
	h.yuki, h.ken = h.fc.AddPerson("Yuki"), h.fc.AddPerson("Ken")
	var err error
	h.yukiM, err = h.fc.Seat(h.yuki.ID, h.co.ID, fakecore.SeatOptions{Preset: "student"})
	h.ok(err)
	h.helper, err = h.fc.AddAgent("Yuki's helper", h.yuki.ID)
	h.ok(err)
	_, err = h.fc.Seat(h.helper.ID, h.co.ID, fakecore.SeatOptions{Preset: "delegate", Principal: h.yukiM.ID})
	h.ok(err)
	h.tokens = append(h.tokens, h.yuki.Token, h.ken.Token, h.helper.Token, h.svc.Token)
	return h
}

// runtime is the runtime's client of Core's agent_runtime service, with
// the credential token.
func (h *hostWorld) runtime(token string) *core.RuntimeService {
	return core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: h.srv.URL, HTTPClient: h.srv.Client(),
		Credential: func(context.Context) (string, error) { return token, nil }, Once: true}))
}

func (h *hostWorld) ok(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

// as is an assertion of the person's, made now on the API's clock.
func (h *hostWorld) as(p fakecore.Actor) string {
	h.t.Helper()
	now := h.clock()
	return h.core.assert(h.t, claims(p.ID, "", func(c map[string]any) {
		c["name"], c["iat"], c["nbf"], c["exp"] = p.Name, now.Unix(), now.Unix(), now.Add(5*time.Minute).Unix()
	}))
}

// issued has the hosted agent id issued its token, as the worker running
// it is: Core's runtime token, sealed in its row. It returns the token.
func (h *hostWorld) issued(id string) fakecore.Token {
	h.t.Helper()
	ctx := context.Background()
	row, err := h.st.HostedAgent(ctx, id)
	h.ok(err)
	tok, err := h.fc.IssueRuntimeToken(row.CoreActorID)
	h.ok(err)
	h.tokens = append(h.tokens, tok.Token)
	sealed, err := h.v.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: row.TenantID, Kind: store.SecretCoreToken}, tok.Token)
	h.ok(err)
	row.TokenSecretID, row.TokenHint, row.TokenIssued, row.TokenCredentialID = sealed.ID, sealed.Hint, true, tok.CredentialID
	_, err = h.st.UpdateHostedAgent(ctx, *row, sealed)
	h.ok(err)
	return tok
}

// call sends a request of the person's, with a JSON body when body is not
// "", and the headers given.
func (h *hostWorld) call(method, path string, as fakecore.Actor, body string, headers ...string) answer {
	h.t.Helper()
	r := req{method: method, path: Prefix + path, token: h.as(as), headers: headers}
	if body != "" {
		r.body = strings.NewReader(body)
		r.headers = append(r.headers, "Content-Type", "application/json")
	}
	return h.send(r)
}

// agentBody is inspect's and POST /agents' body.
func agentBody(agentID string) string {
	b, _ := json.Marshal(map[string]string{"agent_id": agentID})
	return string(b)
}

// host hosts the agent by its id as the person, and returns it.
func (h *hostWorld) host(as fakecore.Actor, agentID string) HostedAgent {
	h.t.Helper()
	a := h.call("POST", "agents", as, agentBody(agentID))
	if a.code != http.StatusCreated {
		h.t.Fatalf("host: %d %s", a.code, a.body)
	}
	var v HostedAgent
	a.decode(h.t, &v)
	return v
}

// events are the audit's events of action.
func (h *hostWorld) events(action string) []store.AuditEvent {
	h.t.Helper()
	all, err := h.st.AuditEvents(context.Background(), at.AddDate(-1, 0, 0), 1000)
	h.ok(err)
	var out []store.AuditEvent
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// noSecrets holds what the runtime keeps and says to holding none of the
// tokens handed out, in plaintext or in part: every answer given, the
// log, the audit, and every row of the store.
func (h *hostWorld) noSecrets(answers ...answer) {
	h.t.Helper()
	var b strings.Builder
	for _, a := range answers {
		b.WriteString(a.body)
	}
	b.WriteString(h.log.String())
	events, err := h.st.AuditEvents(context.Background(), at.AddDate(-1, 0, 0), 1000)
	h.ok(err)
	for _, e := range events {
		raw, _ := json.Marshal(e)
		b.Write(raw)
	}
	rows, err := h.st.HostedAgents(context.Background())
	h.ok(err)
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		b.Write(raw)
	}
	secrets, err := h.st.ListSecrets(context.Background(), "", 1000)
	h.ok(err)
	for _, s := range secrets {
		b.Write(s.Ciphertext)
		b.Write(s.WrappedDEK)
		b.WriteString(s.Hint)
	}
	text := b.String()
	for _, tok := range h.tokens {
		secret := tok[strings.Index(tok, "_")+12+2:]
		if strings.Contains(text, tok) || strings.Contains(text, secret) || strings.Contains(text, secret[:16]) {
			h.t.Errorf("a token is kept or said: %.20s…", tok)
		}
	}
}

// reset gives the limiter another rate, and forgets every bucket.
func (l *limiter) reset(r Rate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perSec, l.burst = float64(r.PerMinute)/60, float64(r.Burst)
	l.byKey = map[string]*list.Element{}
	l.order = list.New()
}
