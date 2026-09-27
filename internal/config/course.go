package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// ForCourse is the agent's configuration for one course: its settings with
// courses[courseID] merged over them, decoded and checked. The course's
// enabled (true unless it says false) and prompt_append_ref are set apart
// in Effective. A course with no entry is the agent as it is, enabled.
// Course ids match without regard to case, as UUIDs do.
//
// The checks are those that need nothing but the agent: Validate, at Load,
// has also checked each course against the runtime's tenants and model
// lists.
func (a *Agent) ForCourse(courseID string) (*Effective, error) {
	e, list, err := a.forCourse(courseID)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		return nil, errors.Join(problems(list, a.File, a.ID)...)
	}
	return e, nil
}

// forCourse builds the course's Effective and lists what is wrong with it.
func (a *Agent) forCourse(courseID string) (*Effective, []issue, error) {
	key, over, ok := a.course(courseID)
	if !ok {
		return &Effective{Agent: *a, Enabled: true}, nil, nil
	}
	e := &Effective{Enabled: true}
	rest := make(map[string]any, len(over))
	for k, v := range over {
		switch k {
		case "enabled":
			if b, ok := v.(bool); ok {
				e.Enabled = b
			}
		case "prompt_append_ref":
			if s, ok := v.(string); ok {
				e.PromptAppendRef = s
			}
		default:
			rest[k] = v
		}
	}
	base, err := a.settings()
	if err != nil {
		return nil, nil, &Problem{File: a.File, Agent: a.ID, Path: "agent", Msg: err.Error()}
	}
	merged := merge(base, rest)
	b, err := decodeAgent(merged)
	if err != nil {
		return nil, nil, &Problem{File: a.File, Agent: a.ID, Path: "courses." + key, Msg: err.Error()}
	}
	b.merged, b.Courses, b.Dir, b.File = merged, a.Courses, a.Dir, a.File
	e.Agent = *b
	is := &issues{prefix: "courses." + key + "."}
	validateAgent(&e.Agent, nil, nil, is)
	if e.PromptAppendRef != "" {
		checkFileRef(is, "prompt_append_ref", e.PromptAppendRef, a.Dir)
	}
	return e, is.list, nil
}

// course finds courseID's settings: by its key as written, or else by the
// same UUID in another case.
func (a *Agent) course(courseID string) (string, map[string]any, bool) {
	if over, ok := a.Courses[courseID]; ok {
		return courseID, over, true
	}
	for k, over := range a.Courses {
		if strings.EqualFold(k, courseID) {
			return k, over, true
		}
	}
	return "", nil, false
}

// settings is the agent's settings as generic YAML: as Load merged them,
// or, for an Agent built in code, as its fields say.
func (a *Agent) settings() (map[string]any, error) {
	if a.merged != nil {
		return a.merged, nil
	}
	var n yaml.Node
	if err := n.Encode(a); err != nil {
		return nil, err
	}
	var m map[string]any
	if err := n.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// Path resolves a file reference of the agent's configuration (system_ref,
// prompt_append_ref): relative to the agent's file, or absolute.
func (a *Agent) Path(ref string) string {
	if ref == "" || filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(a.Dir, ref)
}

// EffectiveProvider is who serves the model: Provider when set, else what
// llm.DetectProvider finds from the adapter and the base URL. Prices, the
// school's model lists and an adapter's defaults are keyed on it.
func (m Model) EffectiveProvider() string {
	if m.Provider != "" {
		return m.Provider
	}
	return llm.DetectProvider(m.Adapter, m.BaseURL)
}

// PricesPath is the price table runtime.prices_ref names, relative to the
// runtime's file; "" when it names none. PRICES, when set, is used instead.
func (c *Config) PricesPath() string {
	if c.Runtime.PricesRef == "" || filepath.IsAbs(c.Runtime.PricesRef) {
		return c.Runtime.PricesRef
	}
	return filepath.Join(c.Dir, c.Runtime.PricesRef)
}

// checkFileRef checks that a file the configuration names is there.
func checkFileRef(is *issues, path, ref, dir string) {
	p := ref
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	info, err := os.Stat(p)
	switch {
	case err != nil:
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		is.add(path, "%s cannot be read: %v", ref, err)
	case info.IsDir():
		is.add(path, "%s is a directory", ref)
	}
}
