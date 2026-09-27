package toolset

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// CatalogueTool is one tool of Core's catalogue (GET /v1/tools), as the
// toolset reads it.
type CatalogueTool struct {
	// Name is the MCP name: the registry's, with the dot an underscore.
	Name        string
	Description string
	// Kind is KindRead or KindWrite.
	Kind        string
	InputSchema json.RawMessage
}

// Catalogue is Core's catalogue as the toolset reads it: every tool by MCP
// name, and the hash that names the whole, under which sanitised schemas
// are cached. FromCore makes one from core.Catalogue.
type Catalogue struct {
	Hash  string
	Tools map[string]CatalogueTool
}

// Check holds the hand-kept gates to the catalogue (§4): every gated tool
// must be in it and be a read, and its schema must sanitise under every
// dialect and compile for validation, so that no seat's Build or Run can
// fail on it later. The runtime checks at start and whenever the
// catalogue's hash changes, and refuses a catalogue that fails: a gate
// kept by hand is only safe while it still describes Core.
func (c *Catalogue) Check() error {
	var errs []error
	for _, name := range sortedKeys(Gates) {
		t, ok := c.Tools[name]
		if !ok {
			errs = append(errs, fmt.Errorf("toolset: the gated tool %s is not in Core's catalogue", name))
			continue
		}
		if t.Kind != KindRead {
			errs = append(errs, fmt.Errorf("toolset: the gated tool %s is no longer a read (kind %q)", name, t.Kind))
			continue
		}
		for _, d := range toolschema.Dialects {
			if _, err := toolschema.Sanitise(t.InputSchema, d, Bound); err != nil {
				errs = append(errs, fmt.Errorf("toolset: %s under %s: %w", name, d, err))
			}
		}
		// Any error but the model's (a required property missing from {})
		// is the schema's own.
		if err := toolschema.Validate(t.InputSchema, json.RawMessage(`{}`)); err != nil && !errors.As(err, new(*toolschema.ArgumentError)) {
			errs = append(errs, fmt.Errorf("toolset: %s: %w", name, err))
		}
	}
	for _, name := range DefaultAllow {
		if _, ok := Gates[name]; !ok {
			errs = append(errs, fmt.Errorf("toolset: %s is allowed by default but has no gate", name))
		}
	}
	return errors.Join(errs...)
}

// Build is the toolset of a seat (§4): the tools of allow (DefaultAllow
// when cfg.Allow is empty) whose gate perms allow, that cfg.Deny and the
// built-in list do not deny, and that the catalogue has as reads. Mode none
// offers nothing; an empty toolset is fine, the model then answers from the
// conversation alone. Each tool is declared with Core's description and its
// schema sanitised for dialect, with course_id and idempotency_key bound,
// made once per catalogue through cache (which may be nil).
func (c *Catalogue) Build(perms map[string]string, cfg config.Tools, dialect toolschema.Dialect, cache *toolschema.Cache) (*Set, error) {
	if !dialect.Valid() {
		return nil, fmt.Errorf("toolset: unknown schema dialect %q", string(dialect))
	}
	s := &Set{tools: map[string]*offered{}}
	switch cfg.Mode {
	case "", "derived":
	case "none":
		return s, nil
	default:
		return nil, fmt.Errorf("toolset: unknown tools mode %q (derived or none)", cfg.Mode)
	}
	allow := cfg.Allow
	if len(allow) == 0 {
		allow = DefaultAllow
	}
	for _, name := range allow {
		if _, dup := s.tools[name]; dup || !offerable(name, perms, cfg.Deny) {
			continue
		}
		t, ok := c.Tools[name]
		if !ok || t.Kind != KindRead {
			continue
		}
		schema, err := cache.Sanitise(c.Hash, name, t.InputSchema, dialect, Bound)
		if err != nil {
			return nil, fmt.Errorf("toolset: %s: %w", name, err)
		}
		s.tools[name] = &offered{
			decl:  llm.Tool{Name: name, Description: t.Description, Schema: schema},
			input: t.InputSchema,
		}
	}
	s.names = sortedKeys(s.tools)
	return s, nil
}

// offerable is the part of §4's formula that needs no catalogue: a gate
// perms allow, not denied by configuration, not denied by the runtime.
func offerable(name string, perms map[string]string, deny []string) bool {
	gate, ok := Gates[name]
	return ok && gate.Allowed(perms) && !slices.Contains(deny, name) && !BuiltinDenied(name)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
