package fakecore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRecordFixtures records the fixtures the fake is held to, from a real
// Core (handout §8.2 item 2): every scenario of scenarios_test.go, each in a
// world of its own seated through Core's REST API, written to
// testdata/fixtures/<scenario>.json with its ids, tokens and times
// replaced by placeholders.
//
// It runs only when asked, against a Core of the test's own:
//
//	CORE_RATE_LIMIT_PER_MINUTE=600 PROPOSAL_TTL=20s CORE_BIN=… DATABASE_URL=… scripts/ci-core.sh start
//	. "$CORE_DIR/env"
//	RECORD_FIXTURES=1 RECORD_PROPOSAL_TTL=20s go test -run TestRecordFixtures ./internal/fakecore/
//
// Core's limit (600 a minute, bursts of 100) is what rate_limited records;
// without it that scenario is skipped. RECORD_PROPOSAL_TTL is the
// PROPOSAL_TTL Core was started with, which replayed_cancelled waits out;
// without it that scenario is skipped. A skipped scenario keeps the fixture
// it had.
func TestRecordFixtures(t *testing.T) {
	if os.Getenv("RECORD_FIXTURES") != "1" {
		t.Skip("set RECORD_FIXTURES=1, E2E_CORE_URL and E2E_ROOT_TOKEN to record the fixtures from a real Core")
	}
	base, root := os.Getenv("E2E_CORE_URL"), os.Getenv("E2E_ROOT_TOKEN")
	if base == "" || root == "" {
		t.Skip("E2E_CORE_URL and E2E_ROOT_TOKEN are not set")
	}
	lc := newLiveCore(t, base, root)
	lc.checkCatalogue()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			w := lc.newWorld(t)
			s := &steps{}
			sc.run(t, w, s)
			if t.Skipped() || t.Failed() {
				return
			}
			fx, err := s.fixture(sc.name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixturePath(sc.name), indented(fx), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// liveCore is a real Core the recorder seats worlds in, through its REST
// API.
type liveCore struct {
	t     *testing.T
	base  string
	hc    *http.Client
	admin string
	dept  string
	mori  person
	yuki  person
	ken   person
	n     int
	// svc is the site's agent runtime's credential (the agent_runtime
	// service's), which the administrator issues once: the runtime agents'
	// tokens are issued through it, by the agents' ids, as the site's
	// runtime is issued them.
	svc string
}

type person struct{ id, token string }

func newLiveCore(t *testing.T, base, root string) *liveCore {
	lc := &liveCore{t: t, base: strings.TrimSuffix(base, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
	lc.admin = lc.person(root, "Admin", map[string]any{"platform_role": "admin"}).token
	lc.dept = str(lc.result(lc.admin, "POST", "/v1/departments", map[string]any{"name": "Computing " + uuid.NewString()[:8]}), "id")
	lc.mori, lc.yuki, lc.ken = lc.register("Mori"), lc.register("Yuki"), lc.register("Ken")
	// replace: a Core recorded from again keeps no more than this one.
	lc.svc = str(lc.result(lc.admin, "POST", "/v1/services/agent_runtime/credentials", map[string]any{"label": "record", "replace": true}), "token")
	return lc
}

// runtimeAgent is owner making an agent of their own hosted by the site's
// runtime (agent.create, hosting runtime), and the runtime being issued its
// token through the service: its id, token and credential's id.
func (lc *liveCore) runtimeAgent(ownerToken, name string) (id, token, credentialID string) {
	lc.t.Helper()
	id = str(lc.result(ownerToken, "POST", "/v1/me/agents", map[string]any{"display_name": name, "hosting": "runtime"}), "actor_id")
	token, credentialID = lc.issueRuntimeToken(id)
	return id, token, credentialID
}

// issueRuntimeToken is the site's runtime issued the agent's token through
// the service, which revokes the one before.
func (lc *liveCore) issueRuntimeToken(agentID string) (token, credentialID string) {
	lc.t.Helper()
	tok := lc.result(lc.svc, "POST", "/v1/services/agent_runtime/agents/"+agentID+"/token", map[string]any{})
	return str(tok, "token"), str(tok, "credential_id")
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// register is the admin registering a person, who signs in with a
// password (person).
func (lc *liveCore) register(name string) person { return lc.person(lc.admin, name, nil) }

// person is registrar registering a person, with the fields more adds (a
// platform role, say), who then signs in with a password as people do:
// people hold no API tokens, only agents do. They are given an email of
// their own, invited to choose a password (actor.invite), choose one as the
// front end's page for invitations does (POST /v1/auth/invite), and sign in
// with it (POST /v1/auth/login). Their token is that session, which Core
// takes as a bearer token. Nothing recorded shows a person's email.
func (lc *liveCore) person(registrar, name string, more map[string]any) person {
	lc.t.Helper()
	email := strings.ToLower(name) + "-" + uuid.NewString() + "@record.test"
	body := map[string]any{"kind": "human", "display_name": name, "email": email}
	maps.Copy(body, more)
	id := str(lc.result(registrar, "POST", "/v1/actors", body), "actor_id")
	invite := str(lc.result(registrar, "POST", "/v1/actors/"+id+"/invite", map[string]any{}), "token")
	password := rand.Text()
	lc.session("/v1/auth/invite", map[string]string{"token": invite, "password": password})
	return person{id, lc.session("/v1/auth/login", map[string]string{"email": email, "password": password})}
}

// session posts body to path, where Core signs someone in, and returns the
// session Core sets as its cookie, waiting out a 429.
func (lc *liveCore) session(path string, body map[string]string) string {
	lc.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		lc.t.Fatal(err)
	}
	for range 20 {
		resp, err := lc.hc.Post(lc.base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			lc.t.Fatalf("POST %s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			time.Sleep(time.Duration(max(secs, 1)) * time.Second)
			continue
		}
		for _, ck := range resp.Cookies() {
			if ck.Name == "ais_session" && ck.Value != "" {
				return ck.Value
			}
		}
		lc.t.Fatalf("POST %s: HTTP %d, and no session: %s", path, resp.StatusCode, raw)
	}
	lc.t.Fatalf("POST %s: still rate limited", path)
	return ""
}

// checkCatalogue holds the Core being recorded to the catalogue the fake
// serves: a fixture recorded from another Core would hold the fake to that
// one.
func (lc *liveCore) checkCatalogue() {
	a := lc.raw("", "GET", "/v1/tools", nil)
	var live, snap any
	if err := json.Unmarshal(a.Body, &live); err != nil {
		lc.t.Fatalf("GET /v1/tools: %v", err)
	}
	if err := json.Unmarshal(catalogueJSON, &snap); err != nil {
		lc.t.Fatal(err)
	}
	lb, _ := json.Marshal(live)
	sb, _ := json.Marshal(snap)
	if !bytes.Equal(lb, sb) {
		lc.t.Fatalf("this Core's GET /v1/tools (sha256 %x) is not the catalogue the fake serves (sha256 %x): "+
			"record from the pinned Core, or update testdata/catalogue.json first", sha256.Sum256(lb), sha256.Sum256(sb))
	}
}

// raw makes one REST call, waiting out a 429.
func (lc *liveCore) raw(token, method, path string, body any) httpAnswer {
	lc.t.Helper()
	r := &restClient{base: lc.base, token: token, hc: lc.hc}
	key := ""
	if method == "POST" {
		key = uuid.NewString()
	}
	for range 20 {
		a, err := r.do(context.Background(), method, path, body, key)
		if err != nil {
			lc.t.Fatalf("%s %s: %v", method, path, err)
		}
		if a.Status != http.StatusTooManyRequests {
			return a
		}
		secs, _ := strconv.Atoi(a.Header.Get("Retry-After"))
		time.Sleep(time.Duration(max(secs, 1)) * time.Second)
	}
	lc.t.Fatalf("%s %s: still rate limited", method, path)
	return httpAnswer{}
}

// result makes a REST call that must be executed, and returns its result.
func (lc *liveCore) result(token, method, path string, body any) map[string]any {
	lc.t.Helper()
	a := lc.raw(token, method, path, body)
	var out struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(a.Body, &out); err != nil || a.Status != http.StatusOK || out.Status != "executed" {
		lc.t.Fatalf("%s %s: %d %s", method, path, a.Status, a.Body)
	}
	return out.Result
}

// liveWorld is a world in a real Core, seated as fakeWorld is.
type liveWorld struct {
	t        *testing.T
	lc       *liveCore
	courseID string
	sato     person
	satoM    string
	tutor    person
	tutorM   string
	seats    []string
	people   []person
	agentC   *mcpClient
	opener   map[string]int
	author   map[string]int
	own      *mcpClient
	ownM     string
	clients  map[string]*mcpClient
	// rootComponent is the course's grade tree's root; hw and syllabus
	// are made when first asked for (assignment, material).
	rootComponent, hw, syllabus string
}

func (lc *liveCore) newWorld(t *testing.T) *liveWorld {
	lc.n++
	lc.t = t
	w := &liveWorld{t: t, lc: lc, opener: map[string]int{}, author: map[string]int{}, people: []person{lc.yuki, lc.ken},
		clients: map[string]*mcpClient{}}
	term := str(lc.result(lc.admin, "POST", "/v1/terms", map[string]any{"name": fmt.Sprintf("Recording %d %s", lc.n, uuid.NewString()[:8]),
		"starts_on": "2026-09-01", "ends_on": "2026-12-20"}), "id")
	co := lc.result(lc.admin, "POST", "/v1/courses", map[string]any{"dept_id": lc.dept, "term_id": term, "code": "CS101",
		"section": "A", "title": "CS101"})
	w.courseID, w.rootComponent = str(co, "course_id"), str(co, "root_component_id")
	c := "/v1/courses/" + w.courseID
	lc.result(lc.admin, "POST", c+"/activate", map[string]any{})
	w.sato = lc.register("Sato")
	w.satoM = str(lc.result(lc.admin, "POST", c+"/instructors", map[string]any{"actor_id": w.sato.id}), "member_id")
	lc.result(lc.admin, "POST", c+"/instructors", map[string]any{"actor_id": lc.mori.id})
	for _, p := range w.people {
		w.seats = append(w.seats, str(lc.result(w.sato.token, "POST", c+"/members", map[string]any{"actor_id": p.id, "preset": "student"}), "member_id"))
	}
	w.tutor.id, w.tutor.token, _ = lc.runtimeAgent(w.sato.token, "CS101 Tutor")
	w.tutorM = str(lc.result(w.sato.token, "POST", c+"/delegates", map[string]any{"actor_id": w.tutor.id, "preset": "course_tutor"}), "member_id")
	w.agentC = newMCPClient(lc.base, w.tutor.token, lc.hc)
	if a, err := w.agentC.initialize(context.Background()); err != nil || a.Status != http.StatusOK {
		t.Fatalf("initialize: %v %d %s", err, a.Status, a.Body)
	}
	return w
}

func (w *liveWorld) course() string           { return w.courseID }
func (w *liveWorld) tutorSeat() string        { return w.tutorM }
func (w *liveWorld) studentSeat(i int) string { return w.seats[i] }
func (w *liveWorld) agent() *mcpClient        { return w.agentC }
func (w *liveWorld) base() string             { return w.lc.base }
func (w *liveWorld) tutorToken() string       { return w.tutor.token }
func (w *liveWorld) rest() *restClient {
	return &restClient{base: w.lc.base, token: w.tutor.token, hc: w.lc.hc}
}
func (w *liveWorld) runtimeService() *restClient {
	return &restClient{base: w.lc.base, token: w.lc.svc, hc: w.lc.hc}
}

// textService is a credential of the document_text service's, issued as
// it is needed.
func (w *liveWorld) textService() *restClient {
	w.t.Helper()
	tok := str(w.lc.result(w.lc.admin, "POST", "/v1/services/document_text/credentials", map[string]any{"label": "record"}), "token")
	return &restClient{base: w.lc.base, token: tok, hc: w.lc.hc}
}

func (w *liveWorld) path(rest string) string { return "/v1/courses/" + w.courseID + rest }

func (w *liveWorld) ask(student int, body string) (string, string) {
	w.t.Helper()
	res := w.lc.result(w.people[student].token, "POST", w.path("/conversations"), map[string]any{"respondent_member_id": w.tutorM, "body": body})
	conv, msg := str(res, "conversation_id"), str(res, "message_id")
	w.opener[conv], w.author[msg] = student, student
	return conv, msg
}

func (w *liveWorld) followUp(conv, body string) string {
	w.t.Helper()
	msg := str(w.lc.result(w.people[w.opener[conv]].token, "POST", w.path("/conversations/"+conv+"/ask"), map[string]any{"body": body}), "message_id")
	w.author[msg] = w.opener[conv]
	return msg
}

func (w *liveWorld) retract(msg, reason string) {
	w.t.Helper()
	body := map[string]any{}
	if reason != "" {
		body["reason"] = reason
	}
	w.lc.result(w.people[w.author[msg]].token, "POST", w.path("/conversation-messages/"+msg+"/retract"), body)
}

func (w *liveWorld) closeAsOpener(conv, reason string) {
	w.t.Helper()
	w.lc.result(w.people[w.opener[conv]].token, "POST", w.path("/conversations/"+conv+"/close"), map[string]any{"reason": reason})
}

func (w *liveWorld) setTutorLevel(level string) {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", w.path("/members/"+w.tutorM+"/perms"), map[string]any{"perms": map[string]any{permConversationAnswer: level}})
}

func (w *liveWorld) pauseTutor() {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", w.path("/members/"+w.tutorM+"/pause"), map[string]any{})
}

func (w *liveWorld) pauseStudent(i int) {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", w.path("/members/"+w.seats[i]+"/pause"), map[string]any{})
}

func (w *liveWorld) removeStudent(i int) {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", w.path("/members/"+w.seats[i]+"/remove"), map[string]any{})
}

func (w *liveWorld) removeTutor() {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", w.path("/members/"+w.tutorM+"/remove"), map[string]any{})
}

func (w *liveWorld) archiveCourse() {
	w.t.Helper()
	w.lc.result(w.lc.admin, "POST", w.path("/archive"), map[string]any{})
}

// ownAgent is Yuki's own agent, seated as Core seats a student's: Yuki asks
// for it (a proposal, her agent_delegate being confirm_required) and Sato
// approves.
func (w *liveWorld) ownAgent() *mcpClient {
	w.t.Helper()
	if w.own != nil {
		return w.own
	}
	yuki := w.people[0]
	id, token, _ := w.lc.runtimeAgent(yuki.token, "Yuki's helper")
	a := w.lc.raw(yuki.token, "POST", w.path("/delegates"), map[string]any{"actor_id": id, "preset": "delegate"})
	var prop struct {
		Status   string `json:"status"`
		ActionID string `json:"action_id"`
	}
	if err := json.Unmarshal(a.Body, &prop); err != nil || prop.Status != actProposed {
		w.t.Fatalf("the delegate request: %d %s", a.Status, a.Body)
	}
	res := w.lc.result(w.sato.token, "POST", w.path("/actions/"+prop.ActionID+"/decide"), map[string]any{"decision": "approve"})
	inner, _ := res["result"].(map[string]any)
	if w.ownM = str(inner, "member_id"); w.ownM == "" {
		w.t.Fatalf("the delegate approved, but: %v", res)
	}
	w.own = newMCPClient(w.lc.base, token, w.lc.hc)
	if h, err := w.own.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		w.t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	return w.own
}

func (w *liveWorld) ownSeat() string {
	w.ownAgent()
	return w.ownM
}

// as is a person of the world over MCP, with their own token.
func (w *liveWorld) as(who string) *mcpClient {
	w.t.Helper()
	if c := w.clients[who]; c != nil {
		return c
	}
	tokens := map[string]string{"sato": w.sato.token, "mori": w.lc.mori.token, "yuki": w.people[0].token, "ken": w.people[1].token}
	token, ok := tokens[who]
	if !ok {
		w.t.Fatalf("nobody called %q in this world", who)
	}
	c := newMCPClient(w.lc.base, token, w.lc.hc)
	if h, err := c.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		w.t.Fatalf("initialize as %s: %v %d %s", who, err, h.Status, h.Body)
	}
	w.clients[who] = c
	return c
}

// listedTutor is a second agent of Sato's, seated with member.add_delegate
// as an instructor seats a tutor for some students.
func (w *liveWorld) listedTutor(student int) (string, *mcpClient) {
	w.t.Helper()
	id, token, _ := w.lc.runtimeAgent(w.sato.token, "Lab Tutor")
	seat := str(w.lc.result(w.sato.token, "POST", w.path("/delegates"), map[string]any{"actor_id": id, "preset": "tutor",
		"student_scope": "listed", "listed_students": []string{w.seats[student]}, "answers_course": true}), "member_id")
	c := newMCPClient(w.lc.base, token, w.lc.hc)
	if h, err := c.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		w.t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	return seat, c
}

// ownerAgent is an agent of Sato's, seated with member.add_delegate as an
// instructor seats an assistant of their own.
func (w *liveWorld) ownerAgent(perms map[string]string) (string, *mcpClient) {
	w.t.Helper()
	id, token, _ := w.lc.runtimeAgent(w.sato.token, "Sato's assistant")
	seat := str(w.lc.result(w.sato.token, "POST", w.path("/delegates"), map[string]any{"actor_id": id, "preset": "delegate",
		"perms": perms}), "member_id")
	c := newMCPClient(w.lc.base, token, w.lc.hc)
	if h, err := c.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		w.t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	return seat, c
}

// registrar is an agent nobody owns: the administrator registers it, hosted
// by the site's runtime, which is issued its token, and Sato seats it with
// member.add.
func (w *liveWorld) registrar(perms map[string]string) (string, *mcpClient) {
	w.t.Helper()
	id := str(w.lc.result(w.lc.admin, "POST", "/v1/actors", map[string]any{"kind": "agent", "display_name": "CS101 Registrar",
		"hosting": "runtime"}), "actor_id")
	token, _ := w.lc.issueRuntimeToken(id)
	seat := str(w.lc.result(w.sato.token, "POST", w.path("/members"), map[string]any{"actor_id": id, "preset": "ta", "perms": perms}), "member_id")
	c := newMCPClient(w.lc.base, token, w.lc.hc)
	if h, err := c.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		w.t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	return seat, c
}

// newcomer is a person the admin registers and nobody signs in as: only
// their id is wanted.
func (w *liveWorld) newcomer(name string) string {
	w.t.Helper()
	return str(w.lc.result(w.lc.admin, "POST", "/v1/actors", map[string]any{"kind": "human", "display_name": name}), "actor_id")
}

// mcpAgent is an agent of Sato's hosted mcp: his own tools reach it over
// MCP with a token he issues, and he seats it as the course's tutor.
func (w *liveWorld) mcpAgent() (string, string, *mcpClient) {
	w.t.Helper()
	id := str(w.lc.result(w.sato.token, "POST", "/v1/me/agents", map[string]any{"display_name": "Sato's tools", "hosting": "mcp"}), "actor_id")
	token := str(w.lc.result(w.sato.token, "POST", "/v1/me/agents/"+id+"/tokens", map[string]any{"label": "my editor"}), "token")
	seat := str(w.lc.result(w.sato.token, "POST", w.path("/delegates"), map[string]any{"actor_id": id, "preset": "course_tutor"}), "member_id")
	c := newMCPClient(w.lc.base, token, w.lc.hc)
	if h, err := c.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		w.t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	return id, seat, c
}

func (w *liveWorld) actorOf(who string) string {
	w.t.Helper()
	switch who {
	case "yuki":
		return w.lc.yuki.id
	case "sato":
		return w.sato.id
	case "tutor":
		return w.tutor.id
	}
	w.t.Fatalf("nobody called %q in this world", who)
	return ""
}

// setLevel is Sato setting a permission of a seat.
func (w *liveWorld) setLevel(seat, perm, level string) {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", w.path("/members/"+seat+"/perms"), map[string]any{"perms": map[string]string{perm: level}})
}

// pausePrincipal is Mori, the other instructor, pausing Sato's seat.
func (w *liveWorld) pausePrincipal() {
	w.t.Helper()
	w.lc.result(w.lc.mori.token, "POST", w.path("/members/"+w.satoM+"/pause"), map[string]any{})
}

func (w *liveWorld) askOwn(body string) (string, string) {
	w.t.Helper()
	w.ownAgent()
	res := w.lc.result(w.people[0].token, "POST", w.path("/conversations"), map[string]any{"respondent_member_id": w.ownM, "body": body})
	return str(res, "conversation_id"), str(res, "message_id")
}

func (w *liveWorld) decide(actionID string, body map[string]any) {
	w.t.Helper()
	res := w.lc.result(w.lc.mori.token, "POST", w.path("/actions/"+actionID+"/decide"), body)
	w.t.Logf("decided %s: %v", body["decision"], res["outcome"])
}

func (w *liveWorld) approve(actionID string) {
	w.decide(actionID, map[string]any{"decision": "approve"})
}

func (w *liveWorld) reject(actionID, reason string) {
	w.decide(actionID, map[string]any{"decision": "reject", "reason": reason})
}

// expire waits for Core's sweep to cancel the proposal, which it does once
// it is older than PROPOSAL_TTL.
func (w *liveWorld) expire(actionID string) bool {
	w.t.Helper()
	ttl, err := time.ParseDuration(os.Getenv("RECORD_PROPOSAL_TTL"))
	if err != nil || ttl <= 0 {
		return false
	}
	deadline := time.Now().Add(ttl + 30*time.Second)
	for time.Now().Before(deadline) {
		a := w.lc.raw(w.lc.mori.token, "GET", w.path("/actions/"+actionID), nil)
		var out struct {
			Result struct {
				Status string `json:"status"`
			} `json:"result"`
		}
		if json.Unmarshal(a.Body, &out) == nil && out.Result.Status == actCancelled {
			return true
		}
		time.Sleep(time.Second)
	}
	w.t.Fatalf("the proposal %s did not expire within %s of the PROPOSAL_TTL %s", actionID, 30*time.Second, ttl)
	return false
}

func (w *liveWorld) suspendTutor() {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", "/v1/me/agents/"+w.tutor.id+"/suspend", map[string]any{})
}

func (w *liveWorld) reactivateTutor() {
	w.t.Helper()
	w.lc.result(w.sato.token, "POST", "/v1/me/agents/"+w.tutor.id+"/reactivate", map[string]any{})
}

// revokeTutorToken is the site's runtime ending the tutor's hosting: its
// token revoked through the service.
func (w *liveWorld) revokeTutorToken() {
	w.t.Helper()
	w.lc.result(w.lc.svc, "POST", "/v1/services/agent_runtime/agents/"+w.tutor.id+"/token/revoke", map[string]any{})
}

// assignment is HW1, published: Sato makes a component for it and it, as
// the fake's canned course has it.
func (w *liveWorld) assignment() string {
	w.t.Helper()
	if w.hw != "" {
		return w.hw
	}
	bucket := str(w.lc.result(w.sato.token, "POST", w.path("/components"), map[string]any{"parent_id": w.rootComponent,
		"name": "Assignments", "weight": 100}), "id")
	w.hw = str(w.lc.result(w.sato.token, "POST", w.path("/assignments"), map[string]any{"title": "HW1", "points_possible": 100,
		"component_id": bucket}), "id")
	w.lc.result(w.sato.token, "POST", w.path("/assignments/"+w.hw+"/publish"), map[string]any{})
	return w.hw
}

// submit is a student handing in work on HW1: a draft, then submitted.
func (w *liveWorld) submit(student int) string {
	w.t.Helper()
	id := str(w.lc.result(w.people[student].token, "POST", w.path("/submissions"), map[string]any{"assignment_id": w.assignment(),
		"body": "My answers."}), "submission_id")
	w.lc.result(w.people[student].token, "POST", w.path("/submissions/"+id+"/submit"), map[string]any{})
	return id
}

// material is the syllabus: Sato's text, made and published, as the fake's
// canned course has it.
// officeMaterial is Sato uploading each file (document.upload_url, and
// the PUT), making a material of them, in order, and publishing it.
func (w *liveWorld) officeMaterial(title string, files ...namedFile) (string, []string) {
	w.t.Helper()
	var named []map[string]any
	for _, f := range files {
		up := w.lc.result(w.sato.token, "GET", w.path("/upload-url?"+url.Values{"kind": {"material"}, "content_type": {f.contentType},
			"filename": {f.name}}.Encode()), nil)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, str(up, "upload_url"), bytes.NewReader(f.data))
		if err != nil {
			w.t.Fatal(err)
		}
		headers, _ := up["headers"].(map[string]any)
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
		if a := doHTTP(w.t, req); a.Status != http.StatusOK {
			w.t.Fatalf("PUT %s: %d %s", f.name, a.Status, a.Body)
		}
		named = append(named, map[string]any{"upload_token": str(up, "upload_token"), "filename": f.name})
	}
	res := w.lc.result(w.sato.token, "POST", w.path("/documents"), map[string]any{"kind": "material", "title": title, "files": named})
	id := str(res, "document_id")
	w.lc.result(w.sato.token, "POST", w.path("/documents/"+id+"/publish"), map[string]any{"version_id": str(res, "version_id")})
	got := w.lc.result(w.sato.token, "GET", w.path("/documents/"+id), nil)
	version, _ := got["version"].(map[string]any)
	list, _ := version["files"].([]any)
	var ids []string
	for _, f := range list {
		m, _ := f.(map[string]any)
		ids = append(ids, str(m, "id"))
	}
	if len(ids) != len(files) {
		w.t.Fatalf("the material has %d files: %v", len(ids), got)
	}
	return id, ids
}

func (w *liveWorld) material() string {
	w.t.Helper()
	if w.syllabus != "" {
		return w.syllabus
	}
	res := w.lc.result(w.sato.token, "POST", w.path("/documents"), map[string]any{"kind": "material", "title": "Syllabus",
		"body_md": "# CS101 syllabus"})
	w.syllabus = str(res, "document_id")
	w.lc.result(w.sato.token, "POST", w.path("/documents/"+w.syllabus+"/publish"), map[string]any{"version_id": str(res, "version_id")})
	return w.syllabus
}
