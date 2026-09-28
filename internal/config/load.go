package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
)

// Load reads the configuration from files and directories (a directory's
// *.yaml and *.yml files, by name, not its subdirectories; hidden files are
// passed over), and validates it (Validate, with no allowlist).
//
// A file holds YAML documents, each either the runtime's settings,
//
//	runtime: {defaults: …, tenants: …, prices_ref: …, allowed_models: …, denied_models: …}
//
// at most one in all, or one agent, as §4 has it:
//
//	agent: {id: …, display_name: …, core: …, model: …, …}
//	courses: {<course_id>: {enabled: …, prompt_append_ref: …, <agent settings>}}
//
// Every key must be known, and every problem is reported at once, each
// naming its file, line, agent and path. An agent's settings are the
// built-in Defaults, then runtime.defaults, then its document, merged as
// maps key by key, anything else replaced whole; a course's are those, then
// courses[course_id] (ForCourse).
//
// A configuration with no agent loads: the runtime then starts and waits
// for agents to be added, and the commands say so.
func Load(paths ...string) (*Config, error) {
	files, err := expand(paths)
	if err != nil {
		return nil, err
	}
	var (
		errs   []error
		rtDoc  *document
		agents []*document
	)
	for _, f := range files {
		docs := readFile(f, &errs)
		for _, d := range docs {
			switch {
			case d.runtime == nil:
				agents = append(agents, d)
			case rtDoc != nil:
				errs = append(errs, &Problem{File: d.file, Line: d.line, Path: "runtime", Msg: "a second runtime document; the first is in " + rtDoc.file})
			default:
				rtDoc = d
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	cfg := &Config{}
	base := Defaults()
	if rtDoc != nil {
		rt, err := decodeRuntime(rtDoc.runtime)
		if err != nil {
			return nil, &Problem{File: rtDoc.file, Path: "runtime", Msg: err.Error()}
		}
		rt.File = rtDoc.file
		cfg.Runtime, cfg.Dir = *rt, filepath.Dir(rtDoc.file)
		if d, ok := rtDoc.runtime["defaults"].(map[string]any); ok {
			base = merge(base, d)
		}
	}
	for _, d := range agents {
		merged := merge(base, d.agent)
		a, err := decodeAgent(merged)
		if err != nil {
			errs = append(errs, &Problem{File: d.file, Line: d.line, Agent: d.id, Path: "agent", Msg: err.Error()})
			continue
		}
		a.merged, a.Courses, a.Dir, a.File = merged, d.courses, filepath.Dir(d.file), d.file
		cfg.Agents = append(cfg.Agents, a)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := cfg.Validate(nil); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expand lists the files paths name: files as given, and each directory's
// YAML files by name. A file named twice is read once.
func expand(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("config: no configuration: give files or directories (CONFIG)")
	}
	var files []string
	seen := map[string]bool{}
	add := func(f string) {
		abs, err := filepath.Abs(f)
		if err != nil {
			abs = filepath.Clean(f)
		}
		if !seen[abs] {
			seen[abs] = true
			files = append(files, f)
		}
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		for _, e := range entries { // ReadDir sorts them by name
			name := e.Name()
			if strings.HasPrefix(name, ".") || (filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml") {
				continue
			}
			f := filepath.Join(p, name)
			// Follow a symbolic link, as a mounted ConfigMap's files are.
			if fi, err := os.Stat(f); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			add(f)
		}
	}
	return files, nil
}

// document is one YAML document of a file, walked.
type document struct {
	file string
	line int
	// runtime is set for the runtime document.
	runtime map[string]any
	// agent, courses and id are set for an agent's.
	agent   map[string]any
	courses map[string]map[string]any
	id      string
}

// readFile reads and walks the documents of file f.
func readFile(f string, errs *[]error) []*document {
	b, err := os.ReadFile(f) // #nosec G304 -- the operator names the configuration files.
	if err != nil {
		*errs = append(*errs, &Problem{File: f, Msg: err.Error()})
		return nil
	}
	w := &walker{file: f, errs: errs, budget: maxValues}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var docs []*document
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if !errors.Is(err, io.EOF) {
				*errs = append(*errs, &Problem{File: f, Msg: redact.String(err.Error())})
			}
			return docs
		}
		root := &n
		if root.Kind == yaml.DocumentNode {
			if len(root.Content) == 0 {
				continue
			}
			root = root.Content[0]
		}
		if root = resolve(root); isNull(root) {
			continue
		}
		if d := w.document(root); d != nil {
			docs = append(docs, d)
		}
	}
}

// document walks one document: runtime, or agent and courses.
func (w *walker) document(root *yaml.Node) *document {
	const shapes = "a document is either runtime: {…} or agent: {…} with courses: {…}"
	if root.Kind != yaml.MappingNode {
		w.problem(root, "", shapes)
		return nil
	}
	w.agent = ""
	top := map[string]*yaml.Node{}
	for _, kv := range w.pairs(root, "") {
		switch kv.key {
		case "runtime", "agent", "courses":
			top[kv.key] = kv.value
		default:
			w.problem(kv.keyNode, join("", kv.key), "unknown field; %s", shapes)
		}
	}
	d := &document{file: w.file, line: root.Line}
	if rn, ok := top["runtime"]; ok {
		if len(top) > 1 {
			w.problem(root, "", "the runtime's document holds nothing else; %s", shapes)
			return nil
		}
		d.runtime = w.mapping(rn, runtimeType, "runtime", shape{special: map[string]func(*yaml.Node, string) any{
			"defaults": func(n *yaml.Node, path string) any { return w.mapping(n, agentType, path, defaultsShape) },
		}})
		if d.runtime == nil {
			d.runtime = map[string]any{}
		}
		return d
	}
	an, ok := top["agent"]
	if !ok {
		w.problem(root, "", "courses without an agent; %s", shapes)
		return nil
	}
	d.id = agentID(an)
	w.agent = d.id
	d.agent = w.mapping(an, agentType, "agent", shape{})
	if d.agent == nil {
		return nil
	}
	if cn, ok := top["courses"]; ok && !isNull(resolve(cn)) {
		d.courses = w.courses(resolve(cn))
	}
	return d
}

// courses walks the courses mapping: each course's settings, as the agent's
// less those that are the agent's alone, with enabled and
// prompt_append_ref.
func (w *walker) courses(n *yaml.Node) map[string]map[string]any {
	if n.Kind != yaml.MappingNode {
		w.problem(n, "courses", "must be a mapping of course ids to settings")
		return nil
	}
	out := map[string]map[string]any{}
	for _, kv := range w.pairs(n, "courses") {
		p := join("courses", kv.key)
		if isNull(resolve(kv.value)) {
			out[kv.key] = map[string]any{}
			continue
		}
		if m := w.mapping(kv.value, agentType, p, courseShape); m != nil {
			out[kv.key] = m
		}
	}
	return out
}

// agentID is the id an agent document gives, for naming its problems
// before it is decoded.
func agentID(n *yaml.Node) string {
	n = resolve(n)
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "id" {
			if v := resolve(n.Content[i+1]); v.Kind == yaml.ScalarNode {
				return redact.String(v.Value)
			}
		}
	}
	return ""
}

// merge returns base with over merged in: maps key by key, anything else,
// null included, replaced whole. Neither is changed, and the result shares
// nothing with them.
func merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = clone(v)
	}
	for k, v := range over {
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = merge(bm, om)
				continue
			}
		}
		out[k] = clone(v)
	}
	return out
}

func clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return merge(nil, t)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = clone(x)
		}
		return out
	}
	return v
}

// decodeAgent turns merged generic settings into an Agent.
func decodeAgent(m map[string]any) (*Agent, error) {
	a := &Agent{}
	if err := decodeInto(m, a); err != nil {
		return nil, err
	}
	a.Model.inheritInto(a.Model.Fallback)
	return a, nil
}

func decodeRuntime(m map[string]any) (*Runtime, error) {
	rt := &Runtime{}
	if err := decodeInto(m, rt); err != nil {
		return nil, err
	}
	if d, ok := m["defaults"].(map[string]any); ok {
		rt.Defaults = merge(nil, d)
	}
	return rt, nil
}

func decodeInto(m map[string]any, v any) error {
	var n yaml.Node
	if err := n.Encode(m); err != nil {
		return fmt.Errorf("the settings do not encode: %w", err)
	}
	if err := n.Decode(v); err != nil {
		return fmt.Errorf("the settings do not decode: %s", redact.String(err.Error()))
	}
	return nil
}

// inheritInto gives a fallback model what it leaves unset of the model it
// stands in for: the key source (a fallback is paid for as the model is)
// and the output cap.
func (m *Model) inheritInto(fb *Model) {
	if fb == nil {
		return
	}
	if fb.KeySource == "" {
		fb.KeySource = m.KeySource
	}
	if fb.Params.MaxOutputTokens == 0 {
		fb.Params.MaxOutputTokens = m.Params.MaxOutputTokens
	}
}
