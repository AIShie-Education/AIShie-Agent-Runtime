package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// TestARChatEndToEnd is the deliberately small replacement for the old Core
// conversation E2E suite. It uses a real Core for identity, grants and MCP
// course tools, a real AR process with its own PostgreSQL schema, and the real
// Node runner against a local OpenAI-compatible mock. No Core conversation or
// message is created.
func TestARChatEndToEnd(t *testing.T) {
	api, root := liveCore(t)
	world := newChatWorld(t, api, root)
	mock := newMockChatModel(t)
	defer mock.Close()
	ar := newARProcess(t, world, mock)
	ar.start(t)
	defer ar.stop(false)

	t.Run("create, SSE, MCP sources and usage", func(t *testing.T) {
		created := world.create(t, ar, "How does quicksort choose a pivot?", "basic")
		events := world.stream(t, ar, created.Conversation.ID)
		defer events.Close()
		snap := world.wait(t, ar, created.Conversation.ID, func(s store.ChatSnapshot) bool {
			return postedAssistant(s, "Quicksort chooses a pivot.")
		})
		assertUsageAndSource(t, snap)
		events.Require(t, "draft", "message")
		mock.mu.Lock()
		search, read := mock.searchCalls, mock.documentReads
		mock.mu.Unlock()
		if search == 0 || read == 0 {
			t.Fatalf("MCP tools were not exercised: search=%d document_get=%d", search, read)
		}
	})

	t.Run("proposed answer is approved", func(t *testing.T) {
		world.answerLevel(t, coreConfirmRequired)
		created := world.create(t, ar, "Please answer with approval.", "approval")
		world.wait(t, ar, created.Conversation.ID, func(s store.ChatSnapshot) bool {
			return s.Conversation.State == store.ChatReplyPendingApproval
		})
		approval := world.pendingApproval(t, ar, created.Conversation.ID)
		world.decide(t, ar, approval.ID, "approve")
		world.wait(t, ar, created.Conversation.ID, func(s store.ChatSnapshot) bool {
			return postedAssistant(s, "This answer was approved.")
		})
		world.answerLevel(t, "autonomous")
	})

	t.Run("withdraw aborts the model request", func(t *testing.T) {
		created := world.create(t, ar, "Withdraw this slow question.", "withdraw")
		mock.wait(t, mock.withdrawStarted)
		withdrawn := world.withdraw(t, ar, created.Conversation.ID, created.Message.ID)
		if withdrawn.Conversation.State == store.ChatClosed {
			t.Fatalf("withdraw closed the conversation: %+v", withdrawn)
		}
		world.wait(t, ar, created.Conversation.ID, func(s store.ChatSnapshot) bool {
			for _, m := range s.Messages {
				if m.ID == created.Message.ID && m.Status == store.ChatRetracted {
					return true
				}
			}
			return false
		})
		mock.wait(t, mock.withdrawCancelled)
	})

	t.Run("restart recovers a persisted generation", func(t *testing.T) {
		created := world.create(t, ar, "Recover this question after a crash.", "recovery")
		mock.wait(t, mock.recoveryStarted)
		ar.stop(true)
		mock.mu.Lock()
		mock.blockRecovery = false
		mock.mu.Unlock()
		ar.start(t)
		world.wait(t, ar, created.Conversation.ID, func(s store.ChatSnapshot) bool {
			return postedAssistant(s, "Recovered answer.")
		})
	})

	ar.stop(false)
	for _, secret := range []string{api.svc, mock.key, world.root, world.teacherToken, world.studentToken} {
		if strings.Contains(ar.logs(), secret) {
			t.Fatalf("AR log contains a secret: %s", redact.String(ar.logs()))
		}
	}
}

const coreConfirmRequired = "confirm_required"

type chatWorld struct {
	api                     *coreAPI
	root                    string
	course                  string
	teacherID, teacherToken string
	studentID, studentToken string
	agentID, tutorMember    string
	audience                string
}

func newChatWorld(t *testing.T, api *coreAPI, root string) *chatWorld {
	t.Helper()
	_, admin := api.newPerson(t, root, "Chat Admin", map[string]any{"platform_role": "admin"})
	today := time.Now().UTC()
	term := result[struct {
		ID string `json:"id"`
	}](t, api, admin, "POST", "/v1/terms", map[string]any{
		"name": "AR chat " + randomHex(4), "starts_on": today.AddDate(0, 0, -1).Format(time.DateOnly),
		"ends_on": today.AddDate(0, 1, 0).Format(time.DateOnly)}).ID
	dept := result[struct {
		ID string `json:"id"`
	}](t, api, admin, "POST", "/v1/departments",
		map[string]any{"name": "AR chat " + randomHex(4)}).ID
	course := result[struct {
		CourseID string `json:"course_id"`
	}](t, api, admin, "POST", "/v1/courses", map[string]any{"dept_id": dept, "term_id": term,
		"code": "CS101", "section": "A", "title": "AR chat computing"}).CourseID
	api.call(t, http.StatusOK, admin, "POST", "/v1/courses/"+course+"/activate", nil)
	teacherID, teacher := api.newPerson(t, admin, "Teacher", nil)
	studentID, student := api.newPerson(t, admin, "Student", nil)
	api.call(t, http.StatusOK, admin, "POST", "/v1/courses/"+course+"/instructors", map[string]any{"actor_id": teacherID})
	studentMember := result[struct {
		MemberID string `json:"member_id"`
	}](t, api, teacher, "POST", "/v1/courses/"+course+"/members", map[string]any{"actor_id": studentID, "preset": "student"}).MemberID
	if studentMember == "" {
		t.Fatal("student seat was not created")
	}
	agent := result[struct {
		ActorID string `json:"actor_id"`
	}](t, api, teacher, "POST", "/v1/me/agents", map[string]any{"display_name": "AR tutor", "hosting": "runtime"}).ActorID
	tutorMember := result[struct {
		MemberID string `json:"member_id"`
	}](t, api, teacher, "POST", "/v1/courses/"+course+"/delegates", map[string]any{"actor_id": agent, "preset": "course_tutor"}).MemberID
	if tutorMember == "" {
		t.Fatal("course tutor seat was not created")
	}
	// A published body-only material needs no upload, OCR or Office pipeline.
	document := result[struct {
		DocumentID string `json:"document_id"`
	}](t, api, teacher, "POST", "/v1/courses/"+course+"/documents", map[string]any{
		"kind": "material", "title": "Week 3: Quicksort", "body_md": "# Quicksort\n\nQuicksort chooses a pivot before partitioning."}).DocumentID
	if document == "" {
		t.Fatal("course material was not created")
	}
	api.call(t, http.StatusOK, teacher, "POST", "/v1/courses/"+course+"/documents/"+document+"/publish", map[string]any{})
	audience := os.Getenv("E2E_RUNTIME_AUDIENCE")
	if audience == "" {
		t.Fatal("E2E_RUNTIME_AUDIENCE is not set; use scripts/e2e.sh")
	}
	return &chatWorld{api: api, root: root, course: course, teacherID: teacherID, teacherToken: teacher,
		studentID: studentID, studentToken: student, agentID: agent, tutorMember: tutorMember, audience: audience}
}

func (w *chatWorld) assertion(t *testing.T, person string) string {
	t.Helper()
	resp, err := w.api.hc.Do(mustRequest(t, http.MethodPost, w.api.base+"/v1/auth/assertion",
		strings.NewReader(`{"audience":`+strconvQuote(w.audience)+`}`), person))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Assertion string `json:"assertion"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Assertion == "" {
		t.Fatalf("assertion: HTTP %d", resp.StatusCode)
	}
	return out.Assertion
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustRequest(t *testing.T, method, target string, body io.Reader, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (w *chatWorld) create(t *testing.T, ar *arProcess, body, key string) chat.CreateResponse {
	t.Helper()
	raw := ar.call(t, w.studentToken, w.audience, http.MethodPost,
		"/runtime/api/v1/courses/"+w.course+"/conversations", map[string]any{"agent_id": w.agentID, "body": body, "idempotency_key": key})
	var out chat.CreateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Conversation.ID == "" || out.Message.ID == "" {
		t.Fatalf("create returned no ids: %s", redact.String(string(raw)))
	}
	return out
}

func (w *chatWorld) wait(t *testing.T, ar *arProcess, id string, predicate func(store.ChatSnapshot) bool) store.ChatSnapshot {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	var last store.ChatSnapshot
	for time.Now().Before(deadline) {
		raw := ar.call(t, w.studentToken, w.audience, http.MethodGet,
			"/runtime/api/v1/courses/"+w.course+"/conversations/"+id+"/messages?limit=50", nil)
		if err := json.Unmarshal(raw, &last); err == nil && predicate(last) {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("chat did not reach the expected state: %+v; AR log: %s", last, redact.String(ar.logs()))
	return store.ChatSnapshot{}
}

func postedAssistant(s store.ChatSnapshot, text string) bool {
	for _, m := range s.Messages {
		if m.Role == store.ChatRoleAssistant && m.Status == store.ChatPosted && strings.Contains(m.Body, text) {
			return true
		}
	}
	return false
}

func assertUsageAndSource(t *testing.T, snap store.ChatSnapshot) {
	t.Helper()
	for _, m := range snap.Messages {
		if m.Role != store.ChatRoleAssistant || m.Status != store.ChatPosted {
			continue
		}
		if m.Usage == nil || m.Usage.Input <= 0 || m.Usage.Output <= 0 {
			t.Errorf("no usage on the answer: %+v", m)
		}
		if m.SourcesStated == nil || !*m.SourcesStated || len(m.Sources) == 0 {
			t.Errorf("the MCP read was not cited: %+v", m)
		}
		return
	}
	t.Fatal("no posted assistant message")
}

func (w *chatWorld) answerLevel(t *testing.T, level string) {
	t.Helper()
	w.api.call(t, http.StatusOK, w.teacherToken, "POST", "/v1/courses/"+w.course+"/members/"+w.tutorMember+"/perms",
		map[string]any{"perms": map[string]string{"conversation_answer": level}})
}

func (w *chatWorld) pendingApproval(t *testing.T, ar *arProcess, conversation string) store.ChatApproval {
	t.Helper()
	var found store.ChatApproval
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw := ar.call(t, w.teacherToken, w.audience, http.MethodGet,
			"/runtime/api/v1/chat/approvals?course_id="+w.course+"&state=pending", nil)
		var out struct {
			Approvals []store.ChatApproval `json:"approvals"`
		}
		if json.Unmarshal(raw, &out) == nil {
			for _, a := range out.Approvals {
				if a.ConversationID == conversation {
					return a
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no pending approval for %s: %s", conversation, redact.String(ar.logs()))
	return found
}

func (w *chatWorld) decide(t *testing.T, ar *arProcess, id, decision string) {
	t.Helper()
	ar.call(t, w.teacherToken, w.audience, http.MethodPost, "/runtime/api/v1/chat/approvals/"+id+"/decision",
		map[string]any{"decision": decision, "idempotency_key": "decide-" + id})
}

func (w *chatWorld) withdraw(t *testing.T, ar *arProcess, conversation, message string) store.ChatSnapshot {
	t.Helper()
	raw := ar.call(t, w.studentToken, w.audience, http.MethodPost,
		"/runtime/api/v1/courses/"+w.course+"/conversations/"+conversation+"/withdraw", map[string]any{"message_id": message})
	var out chat.Withdrawal
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("withdraw: %s", redact.String(string(raw)))
	}
	return out.Snapshot
}

type arProcess struct {
	world                                               *chatWorld
	binary, config, database, entry, runnerBin, keyring string
	coreURL, audience, serviceRef, modelKeyRef          string
	httpPort, apiPort, runnerPort                       int

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan error
	log     lockedLog
	stopped bool
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newARProcess(t *testing.T, w *chatWorld, model *mockChatModel) *arProcess {
	t.Helper()
	binary := os.Getenv("E2E_AR_BIN")
	if binary == "" {
		binary = filepath.Join(repoRoot(t), "bin", "aishie-runtime")
	}
	entry := os.Getenv("E2E_RUNNER_ENTRY")
	if entry == "" {
		entry = filepath.Join(repoRoot(t), "runner", "dist", "index.js")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node 22 is required for the real runner")
	}
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(dir, "keys")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(keyDir, "kek")
	if err := os.WriteFile(kek, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	database := newARDatabase(t)
	config := filepath.Join(dir, "agent.yaml")
	modelKeyVar := "E2E_CHAT_MODEL_KEY"
	t.Setenv(modelKeyVar, model.key)
	serviceVar := "E2E_CHAT_SERVICE_CREDENTIAL"
	t.Setenv(serviceVar, w.api.svc)
	text := fmt.Sprintf(`agent:
  id: e2e-tutor
  display_name: E2E tutor
  core:
    base_url: %q
    transport: mcp
    agent_id: %q
  model:
    adapter: openai_chat
    provider: openai_compatible
    model: e2e-model
    base_url: %q
    key_ref: env://%s
    params: {max_output_tokens: 1000}
  tools: {mode: derived, writes: false}
`, w.api.base, w.agentID, model.URL+"/v1", modelKeyVar)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return &arProcess{world: w, binary: binary, config: config, database: database, entry: entry, runnerBin: node, keyring: kek,
		coreURL: w.api.base, audience: w.audience, serviceRef: "env://" + serviceVar, modelKeyRef: "env://" + modelKeyVar,
		httpPort: freePort(t), apiPort: freePort(t), runnerPort: freePort(t)}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func newARDatabase(t *testing.T) string {
	t.Helper()
	maintenance := os.Getenv("TEST_DATABASE_URL")
	if maintenance == "" {
		maintenance = "postgres:///postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, maintenance)
	if err != nil {
		t.Skipf("no PostgreSQL for AR E2E: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	name := "aishie_chat_e2e_" + strings.ToLower(randomHex(6))
	if _, err := conn.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(maintenance)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	database := u.String()
	binary := os.Getenv("E2E_AR_BIN")
	if binary == "" {
		binary = filepath.Join(repoRoot(t), "bin", "aishie-runtime")
	}
	cmd := exec.Command(binary, "migrate", "up")
	cmd.Env = append(os.Environ(), "DATABASE_URL="+database)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("AR migrate: %v: %s", err, redact.String(string(out)))
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), maintenance)
		if err == nil {
			_, _ = conn.Exec(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`)
			_ = conn.Close(context.Background())
		}
	})
	return database
}

func (p *arProcess) start(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd := exec.Command(p.binary, "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+p.database, "CONFIG="+p.config, "CORE_BASE_URL="+p.coreURL,
		"CORE_SERVICE_CREDENTIAL="+p.serviceRef, "KMS_KEY_ID=local:"+p.keyring,
		"HTTP_ADDR=127.0.0.1:"+fmt.Sprint(p.httpPort), "API_ADDR=127.0.0.1:"+fmt.Sprint(p.apiPort),
		"API_AUDIENCE="+p.audience, "RUNNER_BIN="+p.runnerBin, "RUNNER_ENTRY="+p.entry,
		"RUNNER_PORT="+fmt.Sprint(p.runnerPort), "SHUTDOWN_GRACE=2s", "WORKER_ID=e2e-chat-worker")
	cmd.Stdout, cmd.Stderr = &p.log, &p.log
	p.done = make(chan error, 1)
	p.stopped = false
	p.cmd = cmd
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.base() + "/runtime/api/v1/info") // #nosec G107 -- local test listener.
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-p.done:
			t.Fatalf("AR exited: %v: %s", err, redact.String(p.logs()))
		default:
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("AR did not become healthy: %s", redact.String(p.logs()))
}

func (p *arProcess) base() string { return "http://127.0.0.1:" + fmt.Sprint(p.apiPort) }

func (p *arProcess) stop(hard bool) {
	p.mu.Lock()
	if p.stopped || p.cmd == nil {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if hard {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func (p *arProcess) logs() string {
	return p.log.String()
}

func (p *arProcess) call(t *testing.T, person, audience, method, path string, body any) []byte {
	t.Helper()
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 10; attempt++ {
		assertion := p.world.assertion(t, person)
		req, err := http.NewRequestWithContext(t.Context(), method, p.base()+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+assertion)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := time.Second
			var e struct {
				Error struct {
					Details struct {
						RetryAfter float64 `json:"retry_after_seconds"`
					} `json:"details"`
				} `json:"error"`
			}
			_ = json.Unmarshal(raw, &e)
			if e.Error.Details.RetryAfter > 0 {
				wait = time.Duration(e.Error.Details.RetryAfter * float64(time.Second))
			}
			time.Sleep(wait)
			continue
		}
		if resp.StatusCode >= 400 {
			t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, redact.String(string(raw)))
		}
		return raw
	}
	t.Fatalf("%s %s remained rate limited", method, path)
	return nil
}

type chatEvents struct {
	cancel  context.CancelFunc
	resp    *http.Response
	done    chan struct{}
	logs    func() string
	mu      sync.Mutex
	frames  []string
	scanErr error
}

func (w *chatWorld) stream(t *testing.T, ar *arProcess, id string) *chatEvents {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ar.base()+"/runtime/api/v1/courses/"+w.course+"/conversations/"+id+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.assertion(t, w.studentToken))
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // events.Close and its reader goroutine own the streaming body
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("SSE: HTTP %d", resp.StatusCode)
	}
	events := &chatEvents{cancel: cancel, resp: resp, done: make(chan struct{}), logs: ar.logs}
	go func() {
		defer close(events.done)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 4096), 1<<20)
		var frame string
		for sc.Scan() {
			if sc.Text() != "" {
				frame += sc.Text() + "\n"
				continue
			}
			events.mu.Lock()
			events.frames = append(events.frames, frame)
			events.mu.Unlock()
			frame = ""
		}
		events.mu.Lock()
		events.scanErr = sc.Err()
		events.mu.Unlock()
	}()
	return events
}

func (e *chatEvents) Require(t *testing.T, kinds ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		all := strings.Join(e.frames, "\n")
		e.mu.Unlock()
		ok := true
		for _, kind := range kinds {
			if !strings.Contains(all, "event: "+kind) {
				ok = false
			}
		}
		if ok {
			return
		}
		select {
		case <-e.done:
		case <-time.After(50 * time.Millisecond):
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t.Fatalf("SSE lacks %v (scanner: %v): %s\nAR log: %s", kinds, e.scanErr, strings.Join(e.frames, "\n"), redact.String(e.logs()))
}

func (e *chatEvents) Close() {
	e.cancel()
	_ = e.resp.Body.Close()
	<-e.done
}

type mockChatModel struct {
	*httptest.Server
	key string

	mu                sync.Mutex
	searchCalls       int
	documentReads     int
	blockRecovery     bool
	withdrawStarted   chan struct{}
	withdrawCancelled chan struct{}
	recoveryStarted   chan struct{}
}

func newMockChatModel(t *testing.T) *mockChatModel {
	m := &mockChatModel{key: "sk-e2e-chat-" + randomHex(8), blockRecovery: true,
		withdrawStarted: make(chan struct{}), withdrawCancelled: make(chan struct{}), recoveryStarted: make(chan struct{})}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *mockChatModel) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+m.key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	text := string(raw)
	if strings.Contains(text, "Withdraw this slow question") {
		closeOnce(m.withdrawStarted)
		<-r.Context().Done()
		closeOnce(m.withdrawCancelled)
		return
	}
	m.mu.Lock()
	blockRecovery := m.blockRecovery
	m.mu.Unlock()
	if strings.Contains(text, "Recover this question after a crash") {
		closeOnce(m.recoveryStarted)
		if blockRecovery {
			<-r.Context().Done()
			return
		}
	}
	if strings.Contains(text, "How does quicksort choose a pivot?") {
		time.Sleep(500 * time.Millisecond)
	}
	switch {
	case strings.Contains(text, "How does quicksort choose a pivot?") && !strings.Contains(text, `"role":"tool"`):
		m.mu.Lock()
		m.searchCalls++
		m.mu.Unlock()
		m.tool(w, "call_search", "course_materials_search", `{"query":"quicksort pivot"}`)
	case strings.Contains(text, "How does quicksort choose a pivot?") && strings.Contains(text, `"role":"tool"`) && m.documentReads == 0:
		id := regexp.MustCompile(`"document_id"\s*:\s*"([^"]+)"`).FindStringSubmatch(strings.ReplaceAll(text, `\"`, `"`))
		if len(id) != 2 {
			http.Error(w, "search result has no document id", http.StatusBadGateway)
			return
		}
		m.mu.Lock()
		m.documentReads++
		m.mu.Unlock()
		m.tool(w, "call_read", "document_get", `{"document_id":`+strconvQuote(id[1])+`}`)
	case strings.Contains(text, "How does quicksort choose a pivot?"):
		m.text(w, "Quicksort chooses a pivot.")
	case strings.Contains(text, "Please answer with approval."):
		m.text(w, "This answer was approved.")
	case strings.Contains(text, "Recover this question after a crash"):
		m.text(w, "Recovered answer.")
	default:
		m.text(w, "Acknowledged.")
	}
}

func (m *mockChatModel) text(w http.ResponseWriter, text string) {
	m.stream(w, text, [3]string{})
}

func (m *mockChatModel) tool(w http.ResponseWriter, id, name, args string) {
	m.stream(w, "", [3]string{id, name, args})
}

func (m *mockChatModel) stream(w http.ResponseWriter, text string, tool [3]string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	chunk := func(delta any, finish any, usage bool) {
		body := map[string]any{"id": "chatcmpl-e2e", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "e2e-model",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage {
			body["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
				"prompt_tokens_details": map[string]any{"cached_tokens": 2}}
		}
		raw, _ := json.Marshal(body)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if tool[0] != "" {
		chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": tool[0], "type": "function",
			"function": map[string]any{"name": tool[1], "arguments": tool[2]}}}}, nil, false)
		chunk(map[string]any{}, "tool_calls", true)
	} else {
		chunk(map[string]any{"role": "assistant", "content": text}, nil, false)
		chunk(map[string]any{}, "stop", true)
	}
	_, _ = fmt.Fprintln(w, "data: [DONE]")
	if flusher != nil {
		flusher.Flush()
	}
}

func (m *mockChatModel) wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the expected model call was not observed")
	}
}

func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }() // duplicate model calls are harmless.
	close(ch)
}
