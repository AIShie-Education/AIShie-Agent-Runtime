package config

import (
	"slices"
)

// Deprecated settings are those the runtime takes from a configuration
// written before they went, but no longer does as they say. They are
// warnings, not problems: Load and Validate pass them, so that every
// runtime.yaml and every hosted agent's settings written before still
// load; the runtime logs them as it puts the configuration in force, and
// check shows them.
//
//   - answer.on_attempts_exhausted: close. The runtime closes no
//     conversation: a message whose attempts are spent is held back until
//     the next day, as skip does (decodeAgent).
//   - prompt.close_reason_text, for the same reason: unused.

// Deprecated lists the deprecated settings of the runtime's document: those
// runtime.defaults gives every agent.
func (c *Config) Deprecated() []*Problem {
	var out []*Problem
	for _, is := range deprecatedIn(c.Runtime.Defaults) {
		out = append(out, &Problem{File: c.Runtime.File, Path: "runtime.defaults." + is.path, Msg: is.msg})
	}
	return out
}

// Deprecated lists the deprecated settings of a: those its document sets,
// and each of its courses'. Those it has from runtime.defaults are the
// Config's.
func (a *Agent) Deprecated() []*Problem {
	var out []*Problem
	add := func(prefix string, m map[string]any) {
		for _, is := range deprecatedIn(m) {
			out = append(out, &Problem{File: a.File, Agent: a.ID, Path: prefix + is.path, Msg: is.msg})
		}
	}
	own := a.own
	if own == nil {
		own, _ = a.settings()
	}
	add("agent.", own)
	keys := make([]string, 0, len(a.Courses))
	for k := range a.Courses {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		add(join("courses", k)+".", a.Courses[k])
	}
	return out
}

// deprecatedIn lists the deprecated settings that settings, an agent's, a
// course's or runtime.defaults', hold.
func deprecatedIn(settings map[string]any) []issue {
	var out []issue
	if ans, ok := settings["answer"].(map[string]any); ok && ans["on_attempts_exhausted"] == OnExhaustedClose {
		out = append(out, issue{path: "answer.on_attempts_exhausted",
			msg: "close is deprecated, and done as skip: the runtime closes no conversation, and holds a message whose attempts are spent back until the next day; write skip, or leave it out"})
	}
	if p, ok := settings["prompt"].(map[string]any); ok {
		if _, set := p["close_reason_text"]; set {
			out = append(out, issue{path: "prompt.close_reason_text",
				msg: "is deprecated, and unused: the runtime closes no conversation; leave it out"})
		}
	}
	return out
}
