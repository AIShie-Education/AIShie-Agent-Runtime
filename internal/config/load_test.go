package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
)

// update rewrites the golden files: go test ./internal/config -update.
var update = flag.Bool("update", false, "rewrite the golden files")

const (
	tutorCourse    = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"
	disabledCourse = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1c"
)

func TestExamples(t *testing.T) {
	cfg, err := Load("../../examples/runtime.yaml", "../../examples/agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate([]string{"https://lms.example.edu"}); err != nil {
		t.Fatalf("the examples' Core is allowed: %v", err)
	}
	err = cfg.Validate([]string{"*.other.edu"})
	if got := len(Problems(err)); got != 2 {
		t.Fatalf("both agents are outside the allowlist; got %d problems: %v", got, err)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[0].ID != "cs101-tutor" || cfg.Agents[1].ID != "yuki-helper" {
		t.Fatalf("agents, by file name: %+v", cfg.Agents)
	}
	if cfg.Runtime.File != "../../examples/runtime.yaml" || cfg.Dir != "../../examples" {
		t.Fatalf("runtime file %q, dir %q", cfg.Runtime.File, cfg.Dir)
	}
	if _, err := pricing.Load(cfg.PricesPath()); err != nil {
		t.Fatalf("prices: %v", err)
	}

	tutor, own := cfg.Agents[0], cfg.Agents[1]
	if tutor.Model.KeySource != KeySchool || tutor.Model.EffectiveProvider() != llm.ProviderAnthropic ||
		*tutor.Budgets.PerAskerDay.Answers != 30 || *tutor.Budgets.PerAskerDay.USD != 0.40 {
		t.Fatalf("tutor: %+v", tutor)
	}
	// The fallback is paid for as the model is, and has its output cap.
	if fb := tutor.Model.Fallback; fb == nil || fb.KeySource != KeySchool || fb.Params.MaxOutputTokens != 1500 || fb.EffectiveProvider() != llm.ProviderDeepSeek {
		t.Fatalf("fallback: %+v", tutor.Model.Fallback)
	}
	// runtime.defaults sit between the built-in defaults and the agent.
	if !strings.HasSuffix(own.Prompt.OnQuotaText, "ask your instructor in class.") || own.Prompt.OnRefusalText != DefaultRefusalText {
		t.Fatalf("prompt: %+v", own.Prompt)
	}
	if own.Model.KeySource != KeyOwn || own.Model.EffectiveProvider() != llm.ProviderDeepSeek || own.Answer.HistoryMessages != 30 ||
		own.Dir != "../../examples/agents" || own.File != "../../examples/agents/delegate.yaml" {
		t.Fatalf("own agent: %+v", own)
	}

	e, err := tutor.ForCourse(tutorCourse)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Enabled || e.PromptAppendRef != "prompts/cs101_style.md" || e.Model.Model != "claude-haiku-4-5" ||
		e.Model.Adapter != "anthropic" || e.Model.KeyRef != tutor.Model.KeyRef ||
		*e.Budgets.PerAskerDay.Answers != 15 || *e.Budgets.PerAskerDay.USD != 0.40 ||
		e.Polling.InboxIdleS != 5 || e.Polling.InboxHotS != 2 || e.ID != "cs101-tutor" {
		t.Fatalf("course: %+v", e)
	}
	if _, err := os.Stat(tutor.Path(e.PromptAppendRef)); err != nil {
		t.Fatalf("the course's prompt: %v", err)
	}
	if e, err := tutor.ForCourse(disabledCourse); err != nil || e.Enabled {
		t.Fatalf("disabled course: %+v, %v", e, err)
	}
	e, err = tutor.ForCourse("0192f3c1-0000-7c3a-9b1f-2a4c6e8f0a1b")
	if err != nil || !e.Enabled || e.Model.Model != "claude-sonnet-4-5" || e.PromptAppendRef != "" {
		t.Fatalf("a course without settings is the agent: %+v, %v", e, err)
	}
	if e, err := tutor.ForCourse(strings.ToUpper(tutorCourse)); err != nil || e.Model.Model != "claude-haiku-4-5" {
		t.Fatalf("course ids match without regard to case: %+v, %v", e, err)
	}
}

// TestDefaultsGolden holds the built-in defaults, as a minimal agent gets
// them, to testdata/minimal.golden.yaml.
func TestDefaultsGolden(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := yaml.Marshal(cfg.Agents[0])
	if err != nil {
		t.Fatal(err)
	}
	golden := "testdata/minimal.golden.yaml"
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("the defaults differ from %s (go test -update to rewrite it):\n%s", golden, got)
	}
	a := cfg.Agents[0]
	// Spot checks against design §9, in case the golden file is rewritten
	// carelessly.
	if a.Core.Transport != "mcp" || a.Core.MCPProtocol != "2025-11-25" || a.Model.KeySource != "own" ||
		a.Model.Params.MaxOutputTokens != 2000 || a.Tools.MaxParallelTools != 4 || a.Answer.MaxBodyChars != 19000 ||
		a.Budgets.PerAnswer.WallClock() != 90*time.Second || a.Budgets.PerAgentDay.set() || a.Budgets.PerAskerDay.set() ||
		a.Polling.MaxRateShare != 0.3 || a.Polling.AssumedCoreBurst != 100 || !a.Memory.Enabled {
		t.Fatalf("defaults: %+v", a)
	}
}

func TestDefaultsAreFresh(t *testing.T) {
	d := Defaults()
	d["core"].(map[string]any)["transport"] = "rest"
	if Defaults()["core"].(map[string]any)["transport"] != "mcp" {
		t.Fatal("Defaults shares its maps")
	}
}

func TestAnchorsAndMergeKeys(t *testing.T) {
	cfg, err := Load("testdata/anchors.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Agents[0].Polling
	if p.InboxIdleS != 6 || p.InboxMaxS != 40 || p.EventsS != 50 || p.InboxHotS != 2 {
		t.Fatalf("polling: %+v", p)
	}
	for _, c := range []string{tutorCourse, disabledCourse} {
		e, err := cfg.Agents[0].ForCourse(c)
		if err != nil || e.Polling.InboxIdleS != 5 || e.Polling.InboxMaxS != 40 {
			t.Fatalf("%s: %+v, %v", c, e, err)
		}
	}
}

// write puts files in a new directory and returns it.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const okAgent = `
agent:
  id: a1
  display_name: A1
  core: {base_url: "https://lms.example.edu", token_ref: "env://A1_TOKEN"}
  model: {adapter: openai_chat, model: gpt-4.1-mini, key_ref: "env://OPENAI_API_KEY"}
`

// problem is what a test expects of one reported problem.
type problem struct {
	file  string // the file's base name
	line  int
	agent string
	path  string
	msg   string // a part of the message
}

func expectProblems(t *testing.T, err error, want []problem) {
	t.Helper()
	if err == nil {
		t.Fatal("accepted")
	}
	got := Problems(err)
	for _, w := range want {
		found := false
		for _, p := range got {
			if (w.file == "" || filepath.Base(p.File) == w.file) && (w.line == 0 || p.Line == w.line) &&
				p.Agent == w.agent && p.Path == w.path && strings.Contains(p.Msg, w.msg) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no problem like %+v in:\n%v", w, err)
		}
	}
}

func TestLoadIsStrict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []problem
	}{
		{
			name: "unknown keys at every depth",
			files: map[string]string{"a.yaml": `
agent:
  id: a1
  display_name: A1
  colour: blue
  core: {base_url: "https://lms.example.edu", token_ref: "env://A1_TOKEN"}
  model:
    adapter: openai_chat
    model: gpt-4.1-mini
    key_ref: env://OPENAI_API_KEY
    params: {max_tokens: 10}
  budgets:
    per_answer: {turn: 3}
courses:
  0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b:
    model: {modelname: x}
`},
			want: []problem{
				{file: "a.yaml", line: 5, agent: "a1", path: "agent.colour", msg: "unknown field"},
				{file: "a.yaml", line: 11, agent: "a1", path: "agent.model.params.max_tokens", msg: "unknown field"},
				{file: "a.yaml", line: 13, agent: "a1", path: "agent.budgets.per_answer.turn", msg: "unknown field"},
				{file: "a.yaml", line: 16, agent: "a1", path: "courses.0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b.model.modelname", msg: "unknown field"},
			},
		},
		{
			name: "wrong kinds of value",
			files: map[string]string{"a.yaml": `
agent:
  id: a1
  display_name: A1
  core: [https://lms.example.edu]
  model: {adapter: openai_chat, model: m, key_ref: "env://K", params: {temperature: hot, max_output_tokens: 1.5}}
  tools: {allow: course_get}
  budgets: {per_answer: {turns: eight}}
  memory: {enabled: maybe}
`},
			want: []problem{
				{line: 5, agent: "a1", path: "agent.core", msg: "must be a mapping"},
				{line: 6, agent: "a1", path: "agent.model.params.temperature", msg: "must be a number"},
				{line: 6, agent: "a1", path: "agent.model.params.max_output_tokens", msg: "must be a whole number"},
				{line: 7, agent: "a1", path: "agent.tools.allow", msg: "must be a list"},
				{line: 8, agent: "a1", path: "agent.budgets.per_answer.turns", msg: "must be a whole number"},
				{line: 9, agent: "a1", path: "agent.memory.enabled", msg: "must be true or false"},
			},
		},
		{
			name: "what a course may not set",
			files: map[string]string{"a.yaml": okAgent + `
courses:
  0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b:
    id: other
    core: {token_ref: "env://OTHER"}
    paused: true
    enabled: sometimes
`},
			want: []problem{
				{agent: "a1", path: "courses.0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b.id", msg: "not allowed here"},
				{agent: "a1", path: "courses.0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b.core", msg: "one token"},
				{agent: "a1", path: "courses.0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b.paused", msg: "enabled: false"},
				{agent: "a1", path: "courses.0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b.enabled", msg: "true or false"},
			},
		},
		{
			name: "the runtime document",
			files: map[string]string{
				"a.yaml": okAgent,
				"r.yaml": `
runtime:
  prices: x
  defaults: {id: shared, polling: {inbox_idle: 3}}
  tenants: {t1: {per_dya: {usd: 1}}}
`,
			},
			want: []problem{
				{file: "r.yaml", line: 3, path: "runtime.prices", msg: "unknown field"},
				{file: "r.yaml", line: 4, path: "runtime.defaults.id", msg: "every agent has its own id"},
				{file: "r.yaml", line: 4, path: "runtime.defaults.polling.inbox_idle", msg: "unknown field"},
				{file: "r.yaml", line: 5, path: "runtime.tenants.t1.per_dya", msg: "unknown field"},
			},
		},
		{
			name: "document shapes",
			files: map[string]string{"a.yaml": okAgent + `
---
runtime: {}
agent: {id: x}
---
courses: {}
---
agnet: {}
---
- a list
---
runtime: {}
---
runtime: {}
`},
			want: []problem{
				{msg: "the runtime's document holds nothing else"},
				{msg: "courses without an agent"},
				{path: "agnet", msg: "unknown field"},
				{msg: "a document is either runtime"},
				{path: "runtime", msg: "a second runtime document"},
			},
		},
		{
			name:  "keys written twice",
			files: map[string]string{"a.yaml": okAgent + "  display_name: again\n"},
			want:  []problem{{agent: "a1", path: "agent.display_name", msg: "written twice"}},
		},
		{
			name:  "a YAML error names its file",
			files: map[string]string{"a.yaml": "agent: {id: [\n", "b.yaml": okAgent},
			want:  []problem{{file: "a.yaml", msg: "yaml:"}},
		},
		{
			name:  "no agent",
			files: map[string]string{"r.yaml": "runtime: {}\n", "empty.yaml": "", "nulls.yaml": "---\n~\n---\n"},
			want:  []problem{{msg: "no agent is configured"}},
		},
		{
			name: "problems of the documents are all reported",
			files: map[string]string{
				"a.yaml": strings.Replace(okAgent, "A1", "A1\n  colour: x", 1),
				"b.yaml": strings.Replace(okAgent, "id: a1", "id: b1\n  shape: y", 1),
			},
			want: []problem{
				{file: "a.yaml", agent: "a1", path: "agent.colour", msg: "unknown field"},
				{file: "b.yaml", agent: "b1", path: "agent.shape", msg: "unknown field"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.files))
			expectProblems(t, err, tc.want)
		})
	}
}

func TestLoadFiles(t *testing.T) {
	dir := write(t, map[string]string{
		"b.yaml":         strings.Replace(okAgent, "a1", "b1", 1),
		"a.yml":          okAgent,
		"notes.txt":      "not configuration",
		".hidden.yaml":   "not: read",
		"sub/deep.yaml":  "not: read either",
		"z/other.yaml":   strings.Replace(okAgent, "a1", "z1", 1),
		"agent.yaml.bak": "not: read",
	})
	if err := os.Symlink(filepath.Join(dir, "z", "other.yaml"), filepath.Join(dir, "c-link.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range cfg.Agents {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "a1,b1,z1" {
		t.Fatalf("agents %v: by name, not recursive, symbolic links followed, hidden files passed over", ids)
	}
	// A file named twice, directly and through its directory, is read once.
	if cfg, err := Load(dir, filepath.Join(dir, "a.yml")); err != nil || len(cfg.Agents) != 3 {
		t.Fatalf("got %v", err)
	}
	// A file named directly is read whatever its name.
	if cfg, err := Load(filepath.Join(dir, "z", "other.yaml")); err != nil || cfg.Agents[0].ID != "z1" {
		t.Fatalf("got %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing path")
	}
	if _, err := Load(); err == nil {
		t.Fatal("no path")
	}
	if _, err := Load(dir, filepath.Join(dir, "z")); err == nil || !strings.Contains(err.Error(), "another agent has this id") {
		t.Fatalf("the same agent twice: %v", err)
	}
}

func TestAliasBomb(t *testing.T) {
	// Each map merges ten of the one before: a billion keys, unless the
	// walk is bounded.
	var b strings.Builder
	b.WriteString("x-anchors:\n  m0: &m0 {a: x}\n")
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&b, "  m%d: &m%d {<<: [", i, i)
		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*m%d", i-1)
		}
		b.WriteString("]}\n")
	}
	// Anchors live within one document, so the agent carries them.
	agent := "agent:\n  id: bomb\n  display_name: B\n  x-anchors:\n" + indent(strings.TrimPrefix(b.String(), "x-anchors:\n"), "  ") +
		"  model:\n    headers: *m9\n"
	_, err := Load(write(t, map[string]string{"a.yaml": agent}))
	expectProblems(t, err, []problem{{agent: "bomb", path: "agent.model.headers", msg: "more than 100000 values"}})
}

func indent(s, by string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return by + strings.Join(lines, "\n"+by) + "\n"
}

func TestForCourseConcurrently(t *testing.T) {
	cfg, err := Load("../../examples/runtime.yaml", "../../examples/agents")
	if err != nil {
		t.Fatal(err)
	}
	tutor := cfg.Agents[0]
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			course := tutorCourse
			if i%2 == 1 {
				course = disabledCourse
			}
			e, err := tutor.ForCourse(course)
			if err != nil || e.Enabled != (i%2 == 0) {
				t.Errorf("%s: %+v, %v", course, e, err)
			}
		})
	}
	wg.Wait()
	// The agent is as it was.
	if tutor.Model.Model != "claude-sonnet-4-5" || *tutor.Budgets.PerAskerDay.Answers != 30 {
		t.Fatalf("ForCourse changed the agent: %+v", tutor)
	}
}

func TestForCourseOfAnAgentBuiltInCode(t *testing.T) {
	answers := 5
	a := &Agent{ID: "coded", DisplayName: "Coded"}
	a.Core = Core{BaseURL: "https://lms.example.edu", Transport: "mcp", MCPProtocol: DefaultMCPProtocol, TokenRef: "env://T"}
	a.Model = Model{Adapter: "openai_chat", Model: "m", KeyRef: "env://K", KeySource: KeyOwn, Params: ModelParams{MaxOutputTokens: 100}}
	a.Prompt = Prompt{AnswerLanguage: "opener", OnRefusalText: "r", OnBudgetText: "b", OnQuotaText: "q", CloseReasonText: "c"}
	a.Tools = Tools{Mode: "derived", MaxParallelTools: 1}
	a.Answer = Answer{MaxAttempts: 1, OnAttemptsExhausted: "close", OnQuotaExhausted: "canned", MaxBodyChars: 1000, HistoryMessages: 10, MaxConcurrent: 1, MaxConcurrentPerCourse: 1}
	a.Budgets = Budgets{PerAnswer: PerAnswer{Turns: 1, ToolCalls: 1, InputTokens: 1, OutputTokens: 1, WallClockS: 1}}
	a.Polling = Polling{InboxHotS: 1, InboxIdleS: 1, InboxMaxS: 1, EventsS: 1, MembershipsS: 1, MaxRateShare: 1, AssumedCoreRatePerMin: 1, AssumedCoreBurst: 1}
	a.Courses = map[string]map[string]any{tutorCourse: {"budgets": map[string]any{"per_asker_day": map[string]any{"answers": answers}}, "enabled": false}}
	e, err := a.ForCourse(tutorCourse)
	if err != nil {
		t.Fatal(err)
	}
	if e.Enabled || e.Budgets.PerAskerDay.Answers == nil || *e.Budgets.PerAskerDay.Answers != 5 || e.Model.Model != "m" {
		t.Fatalf("%+v", e)
	}
	a.Courses[tutorCourse]["answer"] = map[string]any{"max_attempts": 0}
	_, err = a.ForCourse(tutorCourse)
	expectProblems(t, err, []problem{{agent: "coded", path: "courses." + tutorCourse + ".answer.max_attempts", msg: "from 1 to 10"}})
}
