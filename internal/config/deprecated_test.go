package config

import (
	"strings"
	"testing"
)

// A configuration written before the runtime stopped closing conversations
// still loads: on_attempts_exhausted close, in runtime.defaults, an agent
// or a course, is taken as skip, and close_reason_text is taken and
// unused. Each is listed as deprecated where it is set, and nothing else
// is.
func TestDeprecatedSettingsLoad(t *testing.T) {
	cfg, err := Load(write(t, map[string]string{
		"runtime.yaml": `
runtime:
  defaults:
    answer: {on_attempts_exhausted: close}
`,
		"a1.yaml": okAgent + `  prompt: {close_reason_text: "I've closed this conversation."}
courses:
  "` + tutorCourse + `":
    answer: {on_attempts_exhausted: close}
    prompt: {close_reason_text: "Closed."}
`,
		"a2.yaml": strings.ReplaceAll(okAgent, "a1", "a2") + "  answer: {on_attempts_exhausted: skip}\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a1, a2 := cfg.Agents[0], cfg.Agents[1]
	e, err := a1.ForCourse(tutorCourse)
	if err != nil {
		t.Fatal(err)
	}
	if a1.Answer.OnAttemptsExhausted != OnExhaustedSkip || e.Answer.OnAttemptsExhausted != OnExhaustedSkip || a2.Answer.OnAttemptsExhausted != OnExhaustedSkip {
		t.Errorf("on_attempts_exhausted %q, in the course %q, and %q", a1.Answer.OnAttemptsExhausted, e.Answer.OnAttemptsExhausted, a2.Answer.OnAttemptsExhausted)
	}
	var got []string
	for _, p := range a1.Deprecated() {
		if p.Agent != "a1" || !strings.HasSuffix(p.File, "a1.yaml") || !strings.Contains(p.Msg, "deprecated") {
			t.Errorf("%+v", p)
		}
		got = append(got, p.Path)
	}
	want := []string{"agent.prompt.close_reason_text", "courses." + tutorCourse + ".answer.on_attempts_exhausted",
		"courses." + tutorCourse + ".prompt.close_reason_text"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("a1's deprecated settings: %q, want %q", got, want)
	}
	if d := a2.Deprecated(); len(d) != 0 {
		t.Errorf("a2 has deprecated settings: %v", d)
	}
	// runtime.defaults' are the runtime document's, named once.
	rt := cfg.Deprecated()
	if len(rt) != 1 || rt[0].Path != "runtime.defaults.answer.on_attempts_exhausted" || rt[0].Agent != "" ||
		!strings.HasSuffix(rt[0].File, "runtime.yaml") || !strings.Contains(rt[0].Msg, "done as skip") {
		t.Errorf("the runtime document's deprecated settings: %v", rt)
	}
}

// A hosted agent's settings from before, in the registry, load the same.
func TestDeprecatedSettingsOfAHostedAgent(t *testing.T) {
	a := ownAgent("agt_old")
	a["answer"] = map[string]any{"on_attempts_exhausted": "close"}
	a["prompt"] = map[string]any{"close_reason_text": "Closed after three tries."}
	agents, rejected := LoadDocuments(nil, nil, Source{Name: "registry:agt_old", Data: jsonAgent(t, a, nil)})
	if len(rejected) != 0 || len(agents) != 1 {
		t.Fatalf("rejected: %v", rejected)
	}
	if agents[0].Answer.OnAttemptsExhausted != OnExhaustedSkip || len(agents[0].Deprecated()) != 2 || agents[0].Deprecated()[0].File != "registry:agt_old" {
		t.Errorf("%+v, deprecated %v", agents[0].Answer, agents[0].Deprecated())
	}
}

// Nothing the defaults give is deprecated.
func TestDefaultsAreNotDeprecated(t *testing.T) {
	cfg, err := Load(write(t, map[string]string{"a1.yaml": okAgent}))
	if err != nil {
		t.Fatal(err)
	}
	if d := append(cfg.Deprecated(), cfg.Agents[0].Deprecated()...); len(d) != 0 {
		t.Errorf("%v", d)
	}
}
