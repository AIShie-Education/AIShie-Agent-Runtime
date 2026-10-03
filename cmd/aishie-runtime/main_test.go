package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/worker"
)

// childArgs, when set, makes the test binary the runtime itself, running
// the command it names: how TestRunServesAndStops sends it real signals.
const childArgs = "AISHIE_RUNTIME_TEST_ARGS"

func TestMain(m *testing.M) {
	if args := os.Getenv(childArgs); args != "" {
		ctx, sigs := signals(strings.Fields(args))
		os.Exit(run(ctx, strings.Fields(args), os.Getenv, os.Stdout, os.Stderr, sigs))
	}
	os.Exit(m.Run())
}

// env is an environment of the given variables alone.
func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func runCmd(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := run(context.Background(), args, getenv, &out, &errs, nil)
	return code, out.String(), errs.String()
}

func TestUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"version"}, exitOK, "aishie-runtime " + version.Version},
		{[]string{"help"}, exitOK, "DATABASE_URL"},
		{nil, exitUsage, "Usage:"},
		{[]string{"nonsense"}, exitUsage, "unknown command"},
		{[]string{"run", "extra"}, exitUsage, "run takes no arguments"},
		{[]string{"check", "--dead"}, exitUsage, "check takes only --live"},
		{[]string{"migrate"}, exitUsage, "migrate takes"},
		{[]string{"migrate", "sideways"}, exitUsage, "not \"sideways\""},
		{[]string{"migrate", "down"}, exitUsage, "--yes"},
		{[]string{"migrate", "up", "--yes"}, exitUsage, "takes no --yes"},
		{[]string{"migrate", "version"}, exitFailure, "DATABASE_URL is not set"},
		{[]string{"catalogue"}, exitUsage, "--core URL"},
		{[]string{"keys"}, exitUsage, "keys takes check or rewrap"},
		{[]string{"keys", "rotate"}, exitUsage, "keys takes check or rewrap"},
		{[]string{"keys", "check"}, exitFailure, "DATABASE_URL is not set"},
	} {
		code, out, errs := runCmd(t, env(), c.args...)
		if code != c.code || !strings.Contains(out+errs, c.out) {
			t.Errorf("%v: %d\n%s%s", c.args, code, out, errs)
		}
	}
}

func TestCheckExamples(t *testing.T) {
	code, out, errs := runCmd(t, env("CONFIG", "../../examples/runtime.yaml,../../examples/agents"), "check")
	if code != exitOK {
		t.Fatalf("check: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		`agent cs101-tutor: "CS101 Tutor"`,
		"model: anthropic claude-sonnet-4-5 (anthropic) on the school key, 1500 output tokens a call; fallback openai_chat deepseek-chat (deepseek)",
		"course 0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b: model anthropic claude-haiku-4-5",
		"prompt appended from prompts/cs101_style.md",
		"course 0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1c: disabled",
		`agent yuki-helper: "Yuki's helper"`,
		"quotas: per agent 100 answers and $1.00 a day",
		"prices: ../../examples/prices.example.yaml (version example-2026-09-27)",
		`school plan: offer standard, "School AI (Claude Haiku)": anthropic claude-haiku-4-5 (anthropic)`,
		`school plan: offer deepseek, "School AI (DeepSeek)": openai_chat deepseek-chat (deepseek)`,
		`school plan: offer llama, "School AI (Llama 3.3 70B)": openai_chat meta-llama/llama-3.3-70b-instruct (openrouter)`,
		`school plan: offer llama's upstream routing, sent to OpenRouter with every call: {"order":["groq"],"allow_fallbacks":true,` +
			`"require_parameters":true,"data_collection":"deny","only":["groq","deepinfra","together"],"quantizations":["fp8","fp16","bf16","unknown"],` +
			`"max_price":{"prompt":"1.04","completion":"1.04"}}`,
		"school plan: per owner 100 answers a day; per asker 20 answers a day; across the school 5000 answers a day (UTC days)",
		"the configuration passes: 2 agents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret://school") {
		t.Errorf("check shows the reference of a school's key:\n%s", out)
	}
}

// TestCheckShowsDeprecatedSettings: a configuration written before the
// runtime stopped closing conversations passes, and check says what of it
// is taken but done as the runtime does now: close as skip, and a closing
// reason unused, where each is written. The examples have none.
func TestCheckShowsDeprecatedSettings(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"runtime.yaml": "runtime:\n  defaults:\n    answer: {on_attempts_exhausted: close}\n",
		"a1.yaml": `agent:
  id: a1
  display_name: A1
  core: {base_url: "https://lms.example.edu", agent_id: "0192f3c1-0000-7000-8000-0000000000a1"}
  model: {adapter: openai_chat, model: gpt-4.1-mini, key_ref: "env://OPENAI_API_KEY"}
  prompt: {close_reason_text: "Closed."}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs := runCmd(t, env("CONFIG", dir), "check")
	if code != exitOK {
		t.Fatalf("check: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"runtime: deprecated: runtime.defaults.answer.on_attempts_exhausted: close is deprecated, and done as skip",
		"answers: at most 3 attempts, then skip",
		"  deprecated: agent.prompt.close_reason_text: is deprecated, and unused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check does not say %q:\n%s", want, out)
		}
	}
	_, out, _ = runCmd(t, env("CONFIG", "../../examples/runtime.yaml,../../examples/agents"), "check")
	if strings.Contains(out, "deprecated") {
		t.Errorf("the examples hold deprecated settings:\n%s", out)
	}
}

// TestCheckOCR: check says whether OCR runs here, and why not; with
// OCR=on, a runtime without its programs does not pass.
func TestCheckOCR(t *testing.T) {
	examples := []string{"CONFIG", "../../examples/runtime.yaml,../../examples/agents"}
	code, out, errs := runCmd(t, env(append(examples, "OCR", "off")...), "check")
	if code != exitOK || !strings.Contains(out, "ocr: off (OCR=off)") {
		t.Errorf("OCR=off: %d\n%s%s", code, out, errs)
	}
	t.Setenv("PATH", t.TempDir())
	code, out, errs = runCmd(t, env(examples...), "check")
	if code != exitOK || !strings.Contains(out, "ocr: off: ocr: not available: tesseract, pdftoppm, prlimit not installed") {
		t.Errorf("OCR=auto, without the programs: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(append(examples, "OCR", "on")...), "check")
	if code != exitFailure || !strings.Contains(errs, "OCR=on, and OCR cannot run here") {
		t.Errorf("OCR=on, without the programs: %d\n%s%s", code, out, errs)
	}
}

// TestCheckOffice: check says whether Office files are converted here, and
// whether PDFs are cut into parts, and why not; with OFFICE_PDF=on, a
// runtime without LibreOffice does not pass.
func TestCheckOffice(t *testing.T) {
	examples := []string{"CONFIG", "../../examples/runtime.yaml,../../examples/agents"}
	code, out, errs := runCmd(t, env(append(examples, "OFFICE_PDF", "off")...), "check")
	if code != exitOK || !strings.Contains(out, "office: off (OFFICE_PDF=off)") {
		t.Errorf("OFFICE_PDF=off: %d\n%s%s", code, out, errs)
	}
	t.Setenv("PATH", t.TempDir())
	code, out, errs = runCmd(t, env(append(examples, "PDF_PART_PAGES", "20")...), "check")
	if code != exitOK || !strings.Contains(out, "office: off: office: not available: prlimit, soffice not installed") ||
		!strings.Contains(out, "pdf parts: off, PDFs are given whole: office: not available: pdfseparate, pdftocairo, pdfunite, prlimit not installed") {
		t.Errorf("OFFICE_PDF=auto, without the programs: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(append(examples, "OFFICE_PDF", "on")...), "check")
	if code != exitFailure || !strings.Contains(errs, "OFFICE_PDF=on, and LibreOffice cannot run here") {
		t.Errorf("OFFICE_PDF=on, without the programs: %d\n%s%s", code, out, errs)
	}
}

func TestCheckRefusesDollarsWithoutPrices(t *testing.T) {
	code, out, errs := runCmd(t, env("CONFIG", "../../examples/agents/delegate.yaml"), "check")
	if code != exitFailure || !strings.Contains(errs, `agent "yuki-helper": it has a quota in dollars, and there is no price table`) {
		t.Errorf("check: %d\n%s%s", code, out, errs)
	}
	dir := t.TempDir()
	prices := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(prices, []byte("version: v\nprices:\n  - {provider: openai, model: gpt-4.1, from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runCmd(t, env("CONFIG", "../../examples/agents/delegate.yaml", "PRICES", prices), "check")
	if code != exitFailure || !strings.Contains(errs, "the price table has no price for deepseek deepseek-chat") {
		t.Errorf("check with a table that misses the model: %d\n%s%s", code, out, errs)
	}
}

// TestCheckNoAgents: a configuration with no agent passes, and check says
// what the runtime will do with it and how an agent is added; --live has
// nothing to connect and passes too.
func TestCheckNoAgents(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"check"}, {"check", "--live"}} {
		code, out, errs := runCmd(t, env("CONFIG", dir), args...)
		if code != exitOK || !strings.Contains(out, "the configuration passes: 0 agents") ||
			!strings.Contains(out, "no agent is configured: the runtime starts and waits") ||
			strings.Contains(out, "every agent connects") {
			t.Errorf("%v: %d\n%s%s", args, code, out, errs)
		}
	}
}

// TestRunWithNoAgents: with no agent, run starts, says so in its log, is
// healthy, and stops on SIGTERM.
func TestRunWithNoAgents(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childArgs+"=run", "CONFIG="+t.TempDir(), "HTTP_ADDR=127.0.0.1:0",
		"LOG_FORMAT=json", "LOG_LEVEL=info", "SHUTDOWN_GRACE=1s", "DATABASE_URL=", "WORKER_ID=child")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
		}
	})
	var out lines
	done := make(chan struct{})
	go func() { out.read(stderr); close(done) }()

	var started struct {
		Addr   string `json:"addr"`
		Agents int    `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out.wait(t, `"msg":"aishie-runtime started"`)), &started); err != nil || started.Addr == "" || started.Agents != 0 {
		t.Fatalf("the started line: %+v, %v", started, err)
	}
	out.wait(t, `"msg":"no agent is configured: the runtime starts and waits`)
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get("http://" + started.Addr + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status": "ok"`) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz never answered ok:\n%s", out.text())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		stopped = true
		if err != nil {
			t.Errorf("the runtime exited with %v:\n%s", err, out.text())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the runtime did not stop on SIGTERM:\n%s", out.text())
	}
	<-done
}

func TestUSDWithoutPrices(t *testing.T) {
	cfg, err := config.Load("../../examples/runtime.yaml", "../../examples/agents")
	if err != nil {
		t.Fatal(err)
	}
	table, err := pricing.Load("../../examples/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p := config.USDWithoutPrices(cfg, table, time.Now()); len(p) != 0 {
		t.Errorf("the examples' prices leave %v", p)
	}
	if p := config.USDWithoutPrices(cfg, nil, time.Now()); len(p) != 2 {
		t.Errorf("without prices: %v", p)
	}
}

// The school's plan may count dollars only where a price table prices
// every offer; its answers need none.
func TestSchoolUSDWithoutPrices(t *testing.T) {
	cfg, err := config.Load("../../examples/runtime.yaml", "../../examples/agents")
	if err != nil {
		t.Fatal(err)
	}
	table, err := pricing.Load("../../examples/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p := config.USDWithoutPrices(cfg, nil, time.Now()); len(p) != 2 || strings.Contains(strings.Join(p, " "), "runtime.school") {
		t.Errorf("answers need no prices: %v", p)
	}
	usd := 0.5
	cfg.Runtime.School.PerOwnerDay.USD = &usd
	if p := config.USDWithoutPrices(cfg, nil, time.Now()); len(p) == 0 || !strings.Contains(p[0], "runtime.school: it has a quota in dollars, and there is no price table") {
		t.Errorf("dollars and no table: %v", p)
	}
	if p := config.USDWithoutPrices(cfg, table, time.Now()); len(p) != 0 {
		t.Errorf("dollars, every offer priced: %v", p)
	}
	cfg.Runtime.School.Offers = append(cfg.Runtime.School.Offers, config.SchoolOffer{ID: "odd", Label: "Odd", Adapter: "openai_chat",
		Model: "unpriced-model", KeyRef: "secret://school/keys/odd"})
	if p := config.USDWithoutPrices(cfg, table, time.Now()); len(p) != 1 || !strings.Contains(p[0], "no price for offer odd, openai unpriced-model") {
		t.Errorf("an offer not priced: %v", p)
	}
	// The ceiling in dollars holds the YAML agents on the school's key.
	cfg.Runtime.School = config.School{PerDay: config.Quota{USD: &usd}}
	if p := config.USDWithoutPrices(cfg, nil, time.Now()); len(p) != 3 || !strings.Contains(p[0], "runtime.school") {
		t.Errorf("a ceiling in dollars, and no table: %v", p)
	}
}

// fakeCore is the fake Core on loopback.
func fakeCore(t *testing.T) (*fakecore.Core, *httptest.Server) {
	t.Helper()
	fc := fakecore.New(fakecore.Options{})
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	return fc, srv
}

func TestCatalogue(t *testing.T) {
	_, srv := fakeCore(t)
	code, out, errs := runCmd(t, env(), "catalogue", "--core", srv.URL)
	if code != exitOK || !strings.HasPrefix(out, worker.SnapshotCatalogueHash+"  167 tools") {
		t.Fatalf("catalogue: %d\n%s%s", code, out, errs)
	}
	for _, snapshot := range []string{"../../internal/core/testdata/catalogue.json", "../../internal/core/testdata/catalogue.sha256"} {
		if code, out, errs := runCmd(t, env(), "catalogue", "--core", srv.URL, "--check", snapshot); code != exitOK {
			t.Errorf("--check %s: %d\n%s%s", snapshot, code, out, errs)
		}
	}
	written := filepath.Join(t.TempDir(), "catalogue.json")
	if code, out, errs := runCmd(t, env(), "catalogue", "--core", srv.URL, "--write", written); code != exitOK {
		t.Fatalf("--write: %d\n%s%s", code, out, errs)
	}
	if h, err := snapshotHash(written); err != nil || h != worker.SnapshotCatalogueHash {
		t.Errorf("the catalogue written hashes %s, %v", h, err)
	}
	other := filepath.Join(t.TempDir(), "other.sha256")
	if err := os.WriteFile(other, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs = runCmd(t, env(), "catalogue", "--core", srv.URL, "--check", other)
	if code != exitFailure || !strings.Contains(errs, "Core's catalogue has changed") {
		t.Errorf("--check a changed catalogue: %d %s", code, errs)
	}
}

// liveWorld is a course in the fake Core with a student's own agent and a
// course tutor, both runtime agents, the runtime's own credential in Core
// in its secrets (core/agent_runtime), and an OpenAI-compatible model
// server, as check --live and run meet them.
type liveWorld struct {
	fc      *fakecore.Core
	co      fakecore.Course
	yuki    fakecore.Member
	own     fakecore.Member
	ownA    string
	tutor   fakecore.Member
	tutorA  string
	svc     fakecore.Token
	config  string
	secrets string
	llm     *fakellm.Server
	coreURL string
}

// answersInSite waits for the runtime to be issued Yuki's agent's token by
// its id, which it runs the agent with.
func (w *liveWorld) answersInSite(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for w.fc.RuntimeIssues(w.ownA) < 2 || !w.fc.SiteChat(w.ownA) {
		if time.Now().After(deadline) {
			t.Fatal("the runtime was never issued Yuki's agent's token")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeSecret writes v as the secret path under the world's SECRETS_DIR.
func (w *liveWorld) writeSecret(t *testing.T, path, v string) {
	t.Helper()
	p := filepath.Join(w.secrets, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newLiveWorld(t *testing.T) *liveWorld {
	t.Helper()
	fc, srv := fakeCore(t)
	model := fakellm.New(fakellm.DefaultResponder).Start()
	t.Cleanup(model.Close)
	w := &liveWorld{fc: fc, co: fc.AddCourse("CS101"), llm: model, coreURL: srv.URL}
	must := func(m fakecore.Member, err error) fakecore.Member {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	sato, yuki := fc.AddPerson("Sato"), fc.AddPerson("Yuki")
	satoSeat := must(fc.Seat(sato.ID, w.co.ID, fakecore.SeatOptions{Preset: "instructor"}))
	w.yuki = must(fc.Seat(yuki.ID, w.co.ID, fakecore.SeatOptions{Preset: "student"}))
	ownA, err := fc.AddAgent("Yuki's helper", yuki.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.own = must(fc.Seat(ownA.ID, w.co.ID, fakecore.SeatOptions{Preset: "delegate", Principal: w.yuki.ID}))
	w.ownA = ownA.ID
	tutorA, err := fc.AddAgent("CS101 Tutor", sato.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.tutor = must(fc.Seat(tutorA.ID, w.co.ID, fakecore.SeatOptions{Preset: "course_tutor", Principal: satoSeat.ID}))
	w.tutorA = tutorA.ID

	dir := t.TempDir()
	w.secrets = filepath.Join(dir, "secrets")
	w.svc = fc.IssueRuntimeServiceToken("runtime")
	w.writeSecret(t, "core/agent_runtime", w.svc.Token)
	w.config = filepath.Join(dir, "agents")
	if err := os.MkdirAll(w.config, 0o700); err != nil {
		t.Fatal(err)
	}
	polling := "{inbox_hot_s: 0.02, inbox_idle_s: 0.05, inbox_max_s: 0.1, events_s: 0.1, memberships_s: 1, assumed_core_rate_per_min: 600000}"
	for _, a := range []struct{ id, name, agentID string }{{"own", "Yuki's helper", ownA.ID}, {"tutor", "CS101 Tutor", tutorA.ID}} {
		yaml := fmt.Sprintf(`agent:
  id: %s
  display_name: %q
  core: {base_url: %q, agent_id: %q}
  model: {adapter: openai_chat, model: fake-model, base_url: %q}
  polling: %s
`, a.id, a.name, srv.URL, a.agentID, model.URL(), polling)
		if err := os.WriteFile(filepath.Join(w.config, a.id+".yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// TestCheckLive: check --live reads each agent as Core hosts it, with the
// runtime's own credential, and is issued nothing: with no token held
// here (no store), it says so, and tries each model's key. A credential
// Core refuses, or none at all, fails the check, saying so and never
// showing it; so does an agent the runtime may not host.
func TestCheckLive(t *testing.T) {
	w := newLiveWorld(t)
	getenv := env("CONFIG", w.config, "SECRETS_DIR", w.secrets)
	code, out, errs := runCmd(t, getenv, "check", "--live")
	if code != exitOK {
		t.Fatalf("check --live: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"catalogue " + worker.SnapshotCatalogueHash,
		`agent own: in Core "Yuki's helper" (` + w.ownA + "), hosted runtime, active; 1 live seats; asked in the site",
		`agent tutor: in Core "CS101 Tutor" (` + w.tutorA + "), hosted runtime, active; 1 live seats; asked in the site",
		"token: none held here yet; the worker is issued one by the agent's id as it starts it",
		"model openai_chat fake-model (openai_compatible): the key works",
		"every agent connects",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check --live does not say %q:\n%s", want, out)
		}
	}
	if len(w.llm.Requests()) != 2 {
		t.Errorf("the model was tried %d times", len(w.llm.Requests()))
	}
	if n := w.fc.RuntimeIssues(w.ownA); n != 1 {
		t.Errorf("check --live was issued a token (%d issued)", n)
	}

	// A credential Core refuses, and none.
	revoked := w.fc.IssueRuntimeServiceToken("revoked")
	if err := w.fc.RevokeServiceToken(revoked.CredentialID); err != nil {
		t.Fatal(err)
	}
	w.writeSecret(t, "core/agent_runtime", revoked.Token)
	code, out, errs = runCmd(t, getenv, "check", "--live")
	if code != exitFailure || !strings.Contains(out, "agent own: FAILED: the runtime's own credential (CORE_SERVICE_CREDENTIAL): refused by Core") ||
		strings.Contains(out+errs, revoked.Token[:20]) {
		t.Errorf("check --live with a credential Core refuses: %d\n%s%s", code, out, errs)
	}
	if err := os.Remove(filepath.Join(w.secrets, "core/agent_runtime")); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runCmd(t, getenv, "check", "--live")
	if code != exitFailure || !strings.Contains(out, "agent own: FAILED: the runtime's own credential (CORE_SERVICE_CREDENTIAL): ") {
		t.Errorf("check --live with no credential: %d\n%s%s", code, out, errs)
	}

	// An mcp agent.
	w.writeSecret(t, "core/agent_runtime", w.svc.Token)
	tools, err := w.fc.AddMCPAgent("Yuki's tools", w.yuki.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("agent:\n  id: tools\n  display_name: Tools\n  core: {base_url: %q, agent_id: %q}\n  model: {adapter: openai_chat, model: fake-model, base_url: %q}\n",
		w.coreURL, tools.ID, w.llm.URL())
	if err := os.WriteFile(filepath.Join(w.config, "tools.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runCmd(t, getenv, "check", "--live")
	if code != exitFailure || !strings.Contains(out, "agent tools: FAILED: hosting: an mcp agent") {
		t.Errorf("check --live of an mcp agent: %d\n%s%s", code, out, errs)
	}
}

// lines collects a child's standard error, line by line.
type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		l.mu.Lock()
		l.all = append(l.all, sc.Text())
		l.mu.Unlock()
	}
}

// wait waits for a line holding s, and returns it.
func (l *lines) wait(t *testing.T, s string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, line := range l.all {
			if strings.Contains(line, s) {
				l.mu.Unlock()
				return line
			}
		}
		l.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Fatalf("no line says %q:\n%s", s, strings.Join(l.all, "\n"))
	return ""
}

func (l *lines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.all, "\n")
}

// TestRunServesAndStops runs the binary as a process of its own: it starts,
// serves /healthz and /metrics, answers a question end to end with the
// fake model, reads its configuration again on SIGHUP, and stops on
// SIGTERM, exiting 0.
func TestRunServesAndStops(t *testing.T) {
	w := newLiveWorld(t)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childArgs+"=run", "CONFIG="+w.config, "SECRETS_DIR="+w.secrets, "HTTP_ADDR=127.0.0.1:0",
		"LOG_FORMAT=json", "LOG_LEVEL=info", "SHUTDOWN_GRACE=5s", "DATABASE_URL=", "WORKER_ID=child")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var out lines
	done := make(chan struct{})
	go func() { out.read(stderr); close(done) }()
	exited := make(chan error, 1)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
		}
	})

	var started struct {
		Addr string `json:"addr"`
	}
	if err := json.Unmarshal([]byte(out.wait(t, `"msg":"aishie-runtime started"`)), &started); err != nil || started.Addr == "" {
		t.Fatalf("the started line: %v", err)
	}
	if !strings.Contains(out.text(), "DATABASE_URL is not set") {
		t.Error("no warning that state is kept in memory")
	}
	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + started.Addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, body := get("/healthz")
		if code == http.StatusOK && strings.Contains(body, `"status": "ok"`) && strings.Contains(body, `"version": "`+version.Version+`"`) &&
			strings.Contains(body, `"commit": "`+version.Commit+`"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}

	w.answersInSite(t)
	conv, _, err := w.fc.Ask(w.co.ID, w.yuki.ID, w.own.ID, "When is HW1 due?")
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for len(w.fc.Answers(conv.ID)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no answer:\n%s", out.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a := w.fc.Answers(conv.ID)[0]; !strings.HasPrefix(a.Body, "Answer: When is HW1 due?") {
		t.Errorf("the answer: %q", a.Body)
	}
	// The answer is counted once Core's reply is back, which may be just
	// after the fake Core shows it: wait for the count.
	wants := []string{"go_goroutines", "process_cpu_seconds_total", `answers_total{outcome="posted"} 1`, "inbox_polls_total{"}
	deadline = time.Now().Add(10 * time.Second)
	for {
		code, metrics := get("/metrics")
		var missing []string
		for _, want := range wants {
			if code != http.StatusOK || !strings.Contains(metrics, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("/metrics lacks %s", strings.Join(missing, ", "))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	out.wait(t, "SIGHUP: the configuration was read again")

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		stopped = true
		if err != nil {
			t.Errorf("the runtime exited with %v:\n%s", err, out.text())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the runtime did not stop on SIGTERM:\n%s", out.text())
	}
	<-done
	for _, want := range []string{`"msg":"stopping"`, `"msg":"aishie-runtime stopped"`} {
		if !strings.Contains(out.text(), want) {
			t.Errorf("no line says %s", want)
		}
	}
	if strings.Contains(out.text(), "ais_") {
		t.Error("a log line holds a token")
	}
}

func TestMigrate(t *testing.T) {
	dbURL := scratchDatabase(t)
	getenv := env("DATABASE_URL", dbURL)
	_, latest, _, err := pgstore.SchemaVersion(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runCmd(t, getenv, "migrate", "up"); code != exitOK || !strings.Contains(out, fmt.Sprintf("schema version %d;", latest)) {
		t.Fatalf("migrate up: %d\n%s%s", code, out, errs)
	}
	if code, out, errs := runCmd(t, getenv, "migrate", "version"); code != exitOK ||
		!strings.Contains(out, fmt.Sprintf("schema version %d; this binary's newest is %d", latest, latest)) {
		t.Errorf("migrate version: %d\n%s%s", code, out, errs)
	}
	// down takes no count: no one migration is taken down on its own, and
	// a rollback, which leaves the schema as it is (docs/deploying.md),
	// has nothing to run but the down that takes them all.
	for _, args := range [][]string{{"migrate", "down", "1", "--yes"}, {"migrate", "down", "--yes", "1"}} {
		if code, out, errs := runCmd(t, getenv, args...); code != exitUsage || !strings.Contains(errs, "migrate down: unknown arguments") {
			t.Errorf("%s: %d\n%s%s", strings.Join(args, " "), code, out, errs)
		}
	}
	if code, out, errs := runCmd(t, getenv, "migrate", "version"); code != exitOK || !strings.Contains(out, fmt.Sprintf("schema version %d;", latest)) {
		t.Errorf("migrate version after a down by a count: %d\n%s%s", code, out, errs)
	}
	if code, out, errs := runCmd(t, getenv, "migrate", "down", "--yes"); code != exitOK || !strings.Contains(out, "schema version 0 (older") {
		t.Errorf("migrate down: %d\n%s%s", code, out, errs)
	}
}

// TestCatalogueMeetsWhatIsNotACore: --core must be a Core's base URL; an
// answer that is not a catalogue, or too large to be one, fails without
// letting the server write to the terminal; and --check and --write of one
// file compare with what it held before.
func TestCatalogueMeetsWhatIsNotACore(t *testing.T) {
	for _, u := range []string{"ftp://core.example", "core.example", "https://", "https://u:p@core.example",
		"https://core.example/?x=1", "https://core.example/#f"} {
		if code, out, errs := runCmd(t, env(), "catalogue", "--core", u); code != exitUsage {
			t.Errorf("--core %s: %d\n%s%s", u, code, out, errs)
		}
	}

	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tools":[{"name":"x\u001b[2Jy","kind":"read"},{"name":"x\u001b[2Jy","kind":"read"}]}`)
	}))
	t.Cleanup(hostile.Close)
	code, out, errs := runCmd(t, env(), "catalogue", "--core", hostile.URL)
	if code != exitFailure || strings.ContainsRune(out+errs, 0x1b) || !strings.Contains(errs, "twice") {
		t.Errorf("a hostile catalogue: %d %q %q", code, out, errs)
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tools":[`)
		chunk := strings.Repeat(" ", 1<<20)
		for range maxCatalogueBytes>>20 + 1 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(huge.Close)
	if code, out, errs := runCmd(t, env(), "catalogue", "--core", huge.URL); code != exitFailure || !strings.Contains(errs, "larger than a catalogue") {
		t.Errorf("a huge answer: %d %s%s", code, out, errs)
	}

	_, srv := fakeCore(t)
	same := filepath.Join(t.TempDir(), "catalogue.sha256")
	if err := os.WriteFile(same, []byte(strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runCmd(t, env(), "catalogue", "--core", srv.URL, "--check", same, "--write", same)
	if code != exitFailure || !strings.Contains(errs, "Core's catalogue has changed") {
		t.Errorf("--check and --write of one file: %d\n%s%s", code, out, errs)
	}
	if h, err := snapshotHash(same); err != nil || h != worker.SnapshotCatalogueHash {
		t.Errorf("the file written hashes %s, %v", h, err)
	}
}

// child runs the test binary as the runtime, with args, and collects its
// standard error.
func child(t *testing.T, args string, extraEnv ...string) (*exec.Cmd, *lines, <-chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(append(os.Environ(), childArgs+"="+args), extraEnv...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	out := &lines{}
	exited := make(chan error, 1)
	go func() {
		out.read(stderr)
		exited <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, out, exited
}

// hanging is a server that takes every request and never answers it, and
// says when one arrives.
func hanging(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 16)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	return srv, reached
}

func waitExit(t *testing.T, exited <-chan error, within time.Duration, out *lines) error {
	t.Helper()
	select {
	case err := <-exited:
		return err
	case <-time.After(within):
		t.Fatalf("the runtime did not exit within %s:\n%s", within, out.text())
	}
	return nil
}

// TestInterruptStopsACommand: SIGINT stops a command other than run that
// waits on a server that never answers, as it would a program without
// handlers of its own.
func TestInterruptStopsACommand(t *testing.T) {
	srv, reached := hanging(t)
	cmd, out, exited := child(t, "catalogue --core "+srv.URL)
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the catalogue was never asked for")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := waitExit(t, exited, 5*time.Second, out)
	if code := cmd.ProcessState.ExitCode(); err == nil || code != exitFailure {
		t.Errorf("exit %d (%v):\n%s", code, err, out.text())
	}
}

// TestRunWithoutTheCredential: run whose own credential in Core cannot be
// read says so as it starts, never showing a credential; its agents are
// not run, each one's state saying why, and run once the operator puts the
// credential where CORE_SERVICE_CREDENTIAL points, with no restart.
func TestRunWithoutTheCredential(t *testing.T) {
	w := newLiveWorld(t)
	if err := os.Remove(filepath.Join(w.secrets, "core", "agent_runtime")); err != nil {
		t.Fatal(err)
	}
	cmd, out, exited := child(t, "run", "CONFIG="+w.config, "SECRETS_DIR="+w.secrets, "HTTP_ADDR=127.0.0.1:0", "LOG_FORMAT=json",
		"SHUTDOWN_GRACE=2s", "DATABASE_URL=", "WORKER_ID=child")
	line := out.wait(t, `"msg":"the runtime's own credential in Core cannot be read (CORE_SERVICE_CREDENTIAL)`)
	if !strings.Contains(line, `"ref":"secret://core/agent_runtime"`) {
		t.Errorf("the line: %s", line)
	}
	out.wait(t, `"msg":"aishie-runtime started"`)
	out.wait(t, `"msg":"agent failed","worker":"child","agent":"own","err":"agent_runtime.agent: core: agent_runtime_agent: the agent runtime's credential (CORE_SERVICE_CREDENTIAL)`)
	if n := w.fc.RuntimeIssues(w.ownA); n != 1 {
		t.Errorf("issued %d tokens without the runtime's credential", n)
	}
	w.writeSecret(t, "core/agent_runtime", w.svc.Token)
	w.answersInSite(t)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, exited, 15*time.Second, out); err != nil {
		t.Errorf("exit: %v", err)
	}
	if strings.Contains(out.text(), "aissvc_") || strings.Contains(out.text(), "ais_") {
		t.Error("a log line holds a token")
	}
}

// TestSecondSignalStopsAtOnce: run, stopping, gives an answer in progress
// SHUTDOWN_GRACE; a second SIGTERM stops it at once, exiting 1.
func TestSecondSignalStopsAtOnce(t *testing.T) {
	w := newLiveWorld(t)
	model, reached := hanging(t)
	yaml := fmt.Sprintf(`agent:
  id: own
  display_name: "Yuki's helper"
  core: {base_url: %q, agent_id: %q}
  model: {adapter: openai_chat, model: fake-model, base_url: %q}
  polling: {inbox_hot_s: 0.02, inbox_idle_s: 0.05, inbox_max_s: 0.1, events_s: 0.1, memberships_s: 1, assumed_core_rate_per_min: 600000}
`, w.coreURL, w.ownA, model.URL+"/v1")
	if err := os.WriteFile(filepath.Join(w.config, "own.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.config, "tutor.yaml")); err != nil {
		t.Fatal(err)
	}
	cmd, out, exited := child(t, "run", "CONFIG="+w.config, "SECRETS_DIR="+w.secrets, "HTTP_ADDR=127.0.0.1:0",
		"LOG_FORMAT=json", "SHUTDOWN_GRACE=60s", "DATABASE_URL=", "WORKER_ID=child")
	out.wait(t, `"msg":"aishie-runtime started"`)
	w.answersInSite(t)
	if _, _, err := w.fc.Ask(w.co.ID, w.yuki.ID, w.own.ID, "Will you finish?"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatalf("the model was never called:\n%s", out.text())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	out.wait(t, `"msg":"stopping"`)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := waitExit(t, exited, 5*time.Second, out)
	if code := cmd.ProcessState.ExitCode(); err == nil || code != exitFailure || !strings.Contains(out.text(), "stopping at once") {
		t.Errorf("exit %d (%v):\n%s", code, err, out.text())
	}
}
