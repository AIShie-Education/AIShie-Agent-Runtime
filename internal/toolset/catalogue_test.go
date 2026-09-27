package toolset

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

func TestBuild(t *testing.T) {
	cat := snapshot(t)
	tests := []struct {
		name  string
		perms map[string]string
		cfg   config.Tools
		want  []string
	}{
		{
			name: "a course tutor reads the material and nobody's work", perms: tutorPerms,
			want: []string{"assignment_get", "assignment_list", "course_get", "document_get", "document_list"},
		},
		{
			name: "a student's own agent reads its principal's work too", perms: delegatePerms,
			want: []string{"assignment_get", "assignment_list", "component_tree", "course_get", "document_get",
				"document_list", "grade_get", "grade_list", "gradebook_get", "submission_get", "submission_list"},
		},
		{
			name: "rubric_read alone opens the any-gates", perms: map[string]string{"rubric_read": "autonomous"},
			want: []string{"document_get", "document_list"},
		},
		{
			name: "grade_read alone", perms: map[string]string{"grade_read": "autonomous"},
			want: []string{"component_tree", "document_get", "grade_get", "grade_list", "gradebook_get"},
		},
		{
			name: "every level but denied allows a read",
			perms: map[string]string{"document_read": "confirm_required", "submission_read": "pending_review",
				"grade_read": "denied"},
			want: []string{"assignment_get", "assignment_list", "course_get", "document_get", "document_list",
				"submission_get", "submission_list"},
		},
		{
			name: "a level the runtime does not know allows nothing", perms: map[string]string{"document_read": "sometimes"},
			want: nil,
		},
		{
			name: "no perms, nothing", perms: nil, want: nil,
		},
		{
			name: "deny takes tools away", perms: delegatePerms,
			cfg: config.Tools{Deny: []string{"document_get", "grade_get", "not_a_tool"}},
			want: []string{"assignment_get", "assignment_list", "component_tree", "course_get",
				"document_list", "grade_list", "gradebook_get", "submission_get", "submission_list"},
		},
		{
			name: "a deny entry ending in * takes every tool it begins", perms: delegatePerms,
			cfg: config.Tools{Deny: []string{"grade_*", "submission_*", "gradebook"}},
			want: []string{"assignment_get", "assignment_list", "component_tree", "course_get", "document_get",
				"document_list", "gradebook_get"},
		},
		{
			name: "allow names tools exactly", perms: delegatePerms,
			cfg:  config.Tools{Allow: []string{"grade_*", "course_get"}},
			want: []string{"course_get"},
		},
		{
			name: "allow narrows, and never offers the feeds the runtime reads itself", perms: tutorPerms,
			cfg:  config.Tools{Mode: "derived", Allow: []string{"course_get", "event_list", "action_list_mine", "grade_list", "course_get"}},
			want: []string{"course_get"},
		},
		{
			name: "writes, the built-in deny list and ungated tools are never offered, even allowed",
			perms: map[string]string{"document_read": "autonomous", "grade_submit": "autonomous",
				"conversation_answer": "autonomous", "member_manage": "autonomous"},
			cfg: config.Tools{Allow: []string{"conversation_answer", "conversation_messages", "member_add",
				"member_list", "grade_submit", "document_upload_url", "agent_get", "action_decide", "course_update",
				"course_list", "me_get", "document_versions", "course_get"}},
			want: []string{"course_get"},
		},
		{
			name: "mode none offers nothing", perms: delegatePerms,
			cfg:  config.Tools{Mode: "none", Allow: []string{"course_get"}},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := cat.Build(tc.perms, tc.cfg, toolschema.OpenAI, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Names(); !slices.Equal(got, tc.want) {
				t.Errorf("offered %v\nwant    %v", got, tc.want)
			}
			if s.Len() != len(tc.want) {
				t.Errorf("Len %d, want %d", s.Len(), len(tc.want))
			}
			for _, name := range s.Names() {
				if k := s.tools[name].kind; k != KindRead {
					t.Errorf("%s is held as kind %q, which Run refuses", name, k)
				}
			}
		})
	}
}

// TestBuildNeverOffersDenied gives writes and built-in denied tools gates,
// as a mistaken edit of Gates might, and checks they are still refused.
func TestBuildNeverOffersDenied(t *testing.T) {
	saved := maps.Clone(Gates)
	t.Cleanup(func() { Gates = saved })
	Gates = maps.Clone(saved)
	for _, name := range []string{"document_upload_url", "conversation_messages", "member_list", "agent_list", "grade_submit", "document_create"} {
		Gates[name] = Gate{Any: []string{"document_read"}}
	}
	s, err := snapshot(t).Build(delegatePerms, config.Tools{Allow: []string{
		"document_upload_url", "conversation_messages", "member_list", "agent_list", "grade_submit", "document_create", "course_get",
	}}, toolschema.OpenAI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); !slices.Equal(got, []string{"course_get"}) {
		t.Fatalf("offered %v, want only course_get", got)
	}
}

func TestBuildRefuses(t *testing.T) {
	cat := snapshot(t)
	if _, err := cat.Build(delegatePerms, config.Tools{Mode: "everything"}, toolschema.OpenAI, nil); err == nil {
		t.Error("an unknown mode was taken")
	}
	if _, err := cat.Build(delegatePerms, config.Tools{}, "xml", nil); err == nil {
		t.Error("an unknown dialect was taken")
	}
	broken := &Catalogue{Hash: "h", Tools: maps.Clone(cat.Tools)}
	ct := broken.Tools["course_get"]
	ct.InputSchema = json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"#"}}}`)
	broken.Tools["course_get"] = ct
	if _, err := broken.Build(delegatePerms, config.Tools{}, toolschema.OpenAI, nil); err == nil || !strings.Contains(err.Error(), "course_get") {
		t.Errorf("err %v, want one naming course_get", err)
	}
}

func TestBuildMissingTool(t *testing.T) {
	cat := snapshot(t)
	delete(cat.Tools, "grade_get")
	s, err := cat.Build(delegatePerms, config.Tools{}, toolschema.OpenAI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Has("grade_get") || s.Len() != 10 {
		t.Errorf("offered %v, want the defaults but grade_get", s.Names())
	}
}

func TestDeclarations(t *testing.T) {
	cat := snapshot(t)
	cache := toolschema.NewCache()
	for _, d := range toolschema.Dialects {
		s, err := cat.Build(delegatePerms, config.Tools{}, d, cache)
		if err != nil {
			t.Fatal(err)
		}
		decls := s.Declarations()
		if len(decls) != 11 {
			t.Fatalf("%s: %d declarations, want 11", d, len(decls))
		}
		for i, decl := range decls {
			if decl.Name != s.Names()[i] {
				t.Errorf("%s: declaration %d is %s, want %s: declarations go in name order", d, i, decl.Name, s.Names()[i])
			}
			if decl.Description != cat.Tools[decl.Name].Description {
				t.Errorf("%s: %s's description is not Core's", d, decl.Name)
			}
			want, err := toolschema.Sanitise(cat.Tools[decl.Name].InputSchema, d, Bound)
			if err != nil {
				t.Fatal(err)
			}
			if string(decl.Schema) != string(want) {
				t.Errorf("%s: %s's schema is not the sanitised one", d, decl.Name)
			}
			if strings.Contains(string(decl.Schema), "course_id") {
				t.Errorf("%s: %s's schema offers course_id", d, decl.Name)
			}
		}
		// What the caller does with the declarations is its own business.
		decls[0].Schema[0] = 'X'
		if s.Declarations()[0].Schema[0] == 'X' {
			t.Errorf("%s: a caller's change reached the set", d)
		}
	}
	if cache.Len() != 11*len(toolschema.Dialects) {
		t.Errorf("the cache holds %d schemas, want one per tool and dialect", cache.Len())
	}
}

func TestNilCatalogue(t *testing.T) {
	var c *Catalogue
	if err := c.Check(); err == nil {
		t.Error("a nil catalogue passes Check")
	}
	s, err := c.Build(delegatePerms, config.Tools{}, toolschema.OpenAI, nil)
	if err != nil || s.Len() != 0 {
		t.Errorf("a nil catalogue offers %v, %v", s.Names(), err)
	}
}

func TestNilSet(t *testing.T) {
	var s *Set
	if s.Names() != nil || s.Len() != 0 || s.Has("course_get") || s.Declarations() != nil {
		t.Error("a nil set offers something")
	}
}

func TestCheck(t *testing.T) {
	if err := snapshot(t).Check(); err != nil {
		t.Fatalf("the snapshot fails: %v", err)
	}
	tests := []struct {
		name   string
		doctor func(c *Catalogue)
		want   string
	}{
		{"a gated tool gone", func(c *Catalogue) { delete(c.Tools, "course_get") }, "course_get is not in Core's catalogue"},
		{"a gated tool now a write", func(c *Catalogue) {
			ct := c.Tools["grade_list"]
			ct.Kind = KindWrite
			c.Tools["grade_list"] = ct
		}, "grade_list is no longer a read"},
		{"a gated tool's schema recursive", func(c *Catalogue) {
			ct := c.Tools["document_get"]
			ct.InputSchema = json.RawMessage(`{"type":"object","$defs":{"N":{"type":"object","properties":{"n":{"$ref":"#/$defs/N"}}}},"properties":{"n":{"$ref":"#/$defs/N"}}}`)
			c.Tools["document_get"] = ct
		}, "recursive"},
		{"a gated tool's schema that does not compile", func(c *Catalogue) {
			ct := c.Tools["assignment_list"]
			ct.InputSchema = json.RawMessage(`{"type":"object","properties":{"s":{"type":"string","pattern":"("}}}`)
			c.Tools["assignment_list"] = ct
		}, "assignment_list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cat := snapshot(t)
			tc.doctor(cat)
			err := cat.Check()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one saying %q", err, tc.want)
			}
		})
	}
}

func TestDefaultAllowIsGated(t *testing.T) {
	for _, name := range DefaultAllow {
		if _, ok := Gates[name]; !ok {
			t.Errorf("%s is allowed by default and has no gate", name)
		}
		if BuiltinDenied(name) {
			t.Errorf("%s is allowed by default and denied", name)
		}
	}
	for name := range Gates {
		if BuiltinDenied(name) {
			t.Errorf("%s has a gate and is denied", name)
		}
	}
}

func TestBuiltinDenied(t *testing.T) {
	denied := []string{"agent_create", "agent_issue_token", "credential_list", "actor_get", "member_add",
		"member_add_delegate", "action_decide", "action_review", "action_withdraw", "conversation_answer",
		"conversation_messages", "conversation_inbox", "preset_create", "course_create", "course_update",
		"term_list", "department_list", "document_upload_url", "action_list_mine", "event_list"}
	for _, name := range denied {
		if !BuiltinDenied(name) {
			t.Errorf("%s is not denied", name)
		}
	}
	for _, name := range []string{"course_get", "document_get", "action_get", "courses_get", "agentx"} {
		if BuiltinDenied(name) {
			t.Errorf("%s is denied", name)
		}
	}
}
