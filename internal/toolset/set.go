package toolset

import (
	"encoding/json"
	"slices"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// Set is the tools one seat's model is offered. It is immutable once built
// and safe for concurrent use; a nil *Set offers nothing.
type Set struct {
	tools map[string]*offered
	// names are the tools' names, sorted: declarations go to the model in
	// this order, the same every time, which keeps prompt caches warm.
	names []string
	// decide is the seat's level of action_decide, which Run holds a
	// decision or a review to (Decides).
	decide string
}

// offered is one tool in a set.
type offered struct {
	decl llm.Tool
	// input is Core's own input schema: calls are reversed and validated
	// against it, not against what the model was shown.
	input json.RawMessage
	// kind is the catalogue's. Run refuses a write unless the answer
	// gives it keys and a budget (Runner.Writes), whatever built the set.
	kind string
}

// Names are the offered tools' names, sorted.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.names)
}

// Reads are the offered reads' names, sorted.
func (s *Set) Reads() []string { return s.namesOf(KindRead) }

// Writes are the offered writes' names, sorted: none but in a set built
// ReadWrite.
func (s *Set) Writes() []string { return s.namesOf(KindWrite) }

func (s *Set) namesOf(kind string) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, name := range s.names {
		if s.tools[name].kind == kind {
			out = append(out, name)
		}
	}
	return out
}

// Len is how many tools are offered.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.names)
}

// Has reports whether name is offered.
func (s *Set) Has(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.tools[name]
	return ok
}

// Declarations are the tools as the model is given them: name, Core's
// description, and the schema sanitised for the adapter's dialect with the
// bound arguments taken out. The caller may change what it is given.
func (s *Set) Declarations() []llm.Tool {
	if s == nil || len(s.names) == 0 {
		return nil
	}
	out := make([]llm.Tool, 0, len(s.names))
	for _, name := range s.names {
		d := s.tools[name].decl
		d.Schema = slices.Clone(d.Schema)
		out = append(out, d)
	}
	return out
}
