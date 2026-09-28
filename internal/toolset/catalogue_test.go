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
			name: "a course tutor reads the material and nobody's work: no roster of submissions, no drafts, no proposals", perms: tutorPerms,
			want: []string{"assignment_get", "assignment_list", "course_get", "document_get", "document_list"},
		},
		{
			name: "a student's own agent reads its principal's work too, and where they stand on an assignment", perms: delegatePerms,
			want: []string{"assignment_get", "assignment_list", "component_tree", "course_get", "document_get",
				"document_list", "grade_get", "grade_list", "gradebook_get", "submission_get", "submission_list", "submission_roster"},
		},
		{
			name: "document_read_draft reads a document's versions", perms: map[string]string{"document_read": "autonomous", "document_read_draft": "autonomous"},
			want: []string{"assignment_get", "assignment_list", "course_get", "document_get", "document_list", "document_versions"},
		},
		{
			name:  "action_decide reads the queues of proposals and one in full, and decides none where writes are not",
			perms: map[string]string{"action_decide": "confirm_required"},
			want:  []string{"action_get", "action_list_pending_review", "action_list_proposed"},
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
				"submission_get", "submission_list", "submission_roster"},
		},
		{
			name:  "member_read reads the roster, and member_manage alone looks up whom to seat",
			perms: map[string]string{"member_read": "autonomous", "member_manage": "denied"},
			want:  []string{"member_get", "member_list"},
		},
		{
			name:  "member_manage alone looks up whom to seat, and offers no write where writes are not",
			perms: map[string]string{"member_manage": "confirm_required"},
			want:  []string{"member_lookup_actor"},
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
				"document_list", "grade_list", "gradebook_get", "submission_get", "submission_list", "submission_roster"},
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
			name: "writes, the built-in deny list and tools the seat's perms do not open are never offered, even allowed",
			perms: map[string]string{"document_read": "autonomous", "grade_submit": "autonomous",
				"conversation_answer": "autonomous", "member_manage": "autonomous"},
			cfg: config.Tools{Allow: []string{"conversation_answer", "conversation_messages", "member_add", "member_add_delegate",
				"member_delegate_defaults", "member_list", "grade_submit", "document_upload_url", "agent_get", "action_decide", "action_withdraw",
				"action_get", "action_list_mine", "course_update", "course_list", "me_get", "document_versions", "submission_roster", "course_get"}},
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
			s, err := cat.Build(tc.perms, tc.cfg, ReadOnly, toolschema.OpenAI, nil)
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
// as a mistaken edit of Gates or WriteGates might, and checks they are
// still refused: a write gated as a read is refused for its kind, a read
// gated as a write likewise, and the built-in list in either.
func TestBuildNeverOffersDenied(t *testing.T) {
	savedReads, savedWrites := maps.Clone(Gates), maps.Clone(WriteGates)
	t.Cleanup(func() { Gates, WriteGates = savedReads, savedWrites })
	Gates, WriteGates = maps.Clone(savedReads), maps.Clone(savedWrites)
	for _, name := range []string{"document_upload_url", "conversation_messages", "member_delegate_defaults", "agent_list", "grade_submit",
		"document_create"} {
		Gates[name] = Gate{Any: []string{"document_read"}}
	}
	for _, name := range []string{"conversation_answer", "member_add_delegate", "action_withdraw", "agent_create", "credential_issue_token",
		"course_archive", "course_get"} {
		WriteGates[name] = Gate{Any: []string{"document_read"}}
	}
	allow := []string{
		"document_upload_url", "conversation_messages", "member_delegate_defaults", "agent_list", "grade_submit", "document_create",
		"course_get", "conversation_answer", "member_add_delegate", "action_withdraw", "agent_create", "credential_issue_token",
		"course_archive",
	}
	for _, access := range []Access{ReadOnly, ReadWrite} {
		s, err := snapshot(t).Build(delegatePerms, config.Tools{Allow: allow, Writes: true}, access, toolschema.OpenAI, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Names(); !slices.Equal(got, []string{"course_get"}) {
			t.Fatalf("access %d: offered %v, want only course_get", access, got)
		}
	}
	if err := snapshot(t).Check(); err == nil || !strings.Contains(err.Error(), "member_add_delegate has a gate, and the built-in list denies it") {
		t.Errorf("Check: %v; want the gates on denied tools refused", err)
	}
}

// Seats' perms for writes: an instructor's own agent, whose document_write
// the instructor set at confirm_required, whose decisions on proposals are
// proposals of its own (action_decide at confirm_required, as Core holds an
// agent's), and which grades nothing and, being a delegate, manages no
// members (Core never gives a delegate member_manage); and an agent an
// instructor seated to manage the course's members, whose member_manage is
// at confirm_required.
var (
	ownerPerms = map[string]string{
		"conversation_answer": "autonomous", "document_read": "autonomous", "document_read_draft": "autonomous",
		"document_write": "confirm_required", "grade_submit": "denied", "grade_post": "autonomous",
		"assignment_write": "denied", "submission_write": "denied", "member_manage": "denied", "action_decide": "confirm_required",
	}
	registrarPerms = map[string]string{
		"conversation_answer": "autonomous", "document_read": "autonomous", "member_read": "autonomous",
		"member_manage": "confirm_required", "agent_delegate": "autonomous",
	}
)

// memberWrites are the member writes a seat that manages members is
// offered, sorted.
var memberWrites = []string{"member_add", "member_pause", "member_remove", "member_rescope", "member_resume",
	"member_update_perms", "member_update_perms_bulk"}

// TestBuildWrites is §4 with writes: the writes the seat's perms allow,
// only in a conversation that may have them (ReadWrite) and only with
// tools.writes on; allow, deny and the built-in list apply to them as to
// reads.
func TestBuildWrites(t *testing.T) {
	cat := snapshot(t)
	reads := []string{"assignment_get", "assignment_list", "course_get", "document_get", "document_list"}
	// ownerReads are the instructor's own agent's: it reads drafts, and
	// the queues of proposals.
	ownerReads := append(slices.Clone(reads), "action_get", "action_list_pending_review", "action_list_proposed", "document_versions")
	slices.Sort(ownerReads)
	docWrites := []string{"document_add_version", "document_archive", "document_create", "document_publish"}
	decides := []string{"action_decide", "action_review"}
	withReads := func(writes ...string) []string {
		out := append(slices.Clone(reads), writes...)
		slices.Sort(out)
		return out
	}
	withOwnerReads := func(writes ...string) []string {
		out := append(slices.Clone(ownerReads), writes...)
		slices.Sort(out)
		return out
	}
	tests := []struct {
		name       string
		perms      map[string]string
		cfg        config.Tools
		access     Access
		want       []string
		wantWrites []string
	}{
		{
			name: "its owner's conversation is offered the writes the seat's perms allow", perms: ownerPerms,
			cfg: config.Tools{Writes: true}, access: ReadWrite,
			want:       withOwnerReads(append(append([]string{"grade_post"}, docWrites...), decides...)...),
			wantWrites: append(append(slices.Clone(docWrites), "grade_post"), decides...),
		},
		{
			name: "anyone else's conversation is offered none", perms: ownerPerms,
			cfg: config.Tools{Writes: true}, access: ReadOnly, want: ownerReads,
		},
		{
			name: "writes off in the configuration, none even for its owner", perms: ownerPerms,
			cfg: config.Tools{}, access: ReadWrite, want: ownerReads,
		},
		{
			name: "a denied permission offers none of its writes, and a gate of both needs both",
			perms: map[string]string{"grade_submit": "denied", "grade_post": "autonomous", "assignment_write": "denied",
				"submission_write": "denied", "document_write": "denied"},
			cfg: config.Tools{Writes: true}, access: ReadWrite, want: []string{"grade_post"}, wantWrites: []string{"grade_post"},
		},
		{
			name:  "every level but denied allows a write",
			perms: map[string]string{"grade_submit": "confirm_required", "grade_post": "pending_review", "assignment_write": "autonomous"},
			cfg:   config.Tools{Writes: true}, access: ReadWrite,
			want: []string{"assignment_create", "assignment_publish", "assignment_unpublish", "assignment_update",
				"component_create", "component_move", "component_update", "document_add_version", "document_archive",
				"document_create", "document_publish", "grade_post", "grade_regrade", "grade_submit",
				"submission_record_missing", "submission_set_lateness"},
		},
		{
			name: "a level the runtime does not know allows no write", perms: map[string]string{"document_write": "always"},
			cfg: config.Tools{Writes: true}, access: ReadWrite, want: nil,
		},
		{
			name: "allow names writes exactly, as it does reads", perms: ownerPerms,
			cfg: config.Tools{Writes: true, Allow: []string{"course_get", "document_create", "document_*", "grade_submit"}}, access: ReadWrite,
			want: []string{"course_get", "document_create"}, wantWrites: []string{"document_create"},
		},
		{
			name: "deny takes writes away, by name or beginning", perms: ownerPerms,
			cfg: config.Tools{Writes: true, Deny: []string{"document_archive", "grade_*", "action_*"}}, access: ReadWrite,
			want: withReads("document_add_version", "document_create", "document_publish", "document_versions"),
		},
		{
			name: "mode none offers no write either", perms: ownerPerms,
			cfg: config.Tools{Writes: true, Mode: "none"}, access: ReadWrite, want: nil,
		},
		{
			name: "a seat that manages members is offered their writes, never the seating of agents", perms: registrarPerms,
			cfg: config.Tools{Writes: true}, access: ReadWrite,
			want:       withReads(append([]string{"member_get", "member_list", "member_lookup_actor"}, memberWrites...)...),
			wantWrites: slices.Clone(memberWrites),
		},
		{
			name: "and anywhere writes are not, reads the roster alone", perms: registrarPerms,
			cfg: config.Tools{Writes: true}, access: ReadOnly,
			want: withReads("member_get", "member_list", "member_lookup_actor"),
		},
		{
			name: "deny takes member writes away as it does others", perms: registrarPerms,
			cfg: config.Tools{Writes: true, Deny: []string{"member_remove", "member_update_*"}}, access: ReadWrite,
			want: withReads("member_add", "member_get", "member_list", "member_lookup_actor", "member_pause", "member_rescope", "member_resume"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := cat.Build(tc.perms, tc.cfg, tc.access, toolschema.OpenAI, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Names(); !slices.Equal(got, tc.want) {
				t.Errorf("offered %v\nwant    %v", got, tc.want)
			}
			if tc.wantWrites != nil {
				slices.Sort(tc.wantWrites)
				if got := s.Writes(); !slices.Equal(got, tc.wantWrites) {
					t.Errorf("writes %v, want %v", got, tc.wantWrites)
				}
			}
			for _, name := range s.Writes() {
				if _, ok := WriteGates[name]; !ok || s.tools[name].kind != KindWrite {
					t.Errorf("%s is offered as a write without a write gate", name)
				}
			}
			if len(s.Reads())+len(s.Writes()) != s.Len() {
				t.Errorf("reads %v and writes %v are not the set %v", s.Reads(), s.Writes(), s.Names())
			}
		})
	}
}

// TestBuiltinDenyNeverOffered is the built-in list against every tool of
// the catalogue: a seat holding every permission at autonomous, in its
// owner's conversation with writes on and every tool allowed, is offered
// exactly the gated tools, none the list denies; and every tool, read or
// write, is one or the other.
func TestBuiltinDenyNeverOffered(t *testing.T) {
	cat := snapshot(t)
	all := map[string]string{}
	for _, p := range []string{"document_read", "document_read_draft", "document_write", "rubric_read", "assignment_write",
		"submission_read", "submission_write", "grade_read", "grade_submit", "grade_post", "member_read", "member_manage",
		"action_decide", "agent_delegate", "conversation_ask", "conversation_answer"} {
		all[p] = "autonomous"
	}
	s, err := cat.Build(all, config.Tools{Writes: true, Allow: sortedKeys(cat.Tools)}, ReadWrite, toolschema.OpenAI, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := append(sortedKeys(Gates), sortedKeys(WriteGates)...)
	slices.Sort(want)
	if got := s.Names(); !slices.Equal(got, want) {
		t.Errorf("offered %v\nwant    %v", got, want)
	}
	denied := 0
	for name, tool := range cat.Tools {
		if BuiltinDenied(name) {
			denied++
			if s.Has(name) {
				t.Errorf("%s is offered, and the built-in list denies it", name)
			}
			continue
		}
		if !s.Has(name) {
			t.Errorf("the %s %s is neither offered nor denied: give it a gate, or a line in BuiltinDeny", tool.Kind, name)
		}
	}
	if denied == 0 {
		t.Error("the built-in list denies nothing of the catalogue")
	}
}

func TestBuildRefuses(t *testing.T) {
	cat := snapshot(t)
	if _, err := cat.Build(delegatePerms, config.Tools{Mode: "everything"}, ReadOnly, toolschema.OpenAI, nil); err == nil {
		t.Error("an unknown mode was taken")
	}
	if _, err := cat.Build(delegatePerms, config.Tools{}, ReadOnly, "xml", nil); err == nil {
		t.Error("an unknown dialect was taken")
	}
	broken := &Catalogue{Hash: "h", Tools: maps.Clone(cat.Tools)}
	ct := broken.Tools["course_get"]
	ct.InputSchema = json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"#"}}}`)
	broken.Tools["course_get"] = ct
	if _, err := broken.Build(delegatePerms, config.Tools{}, ReadOnly, toolschema.OpenAI, nil); err == nil || !strings.Contains(err.Error(), "course_get") {
		t.Errorf("err %v, want one naming course_get", err)
	}
}

func TestBuildMissingTool(t *testing.T) {
	cat := snapshot(t)
	delete(cat.Tools, "grade_get")
	s, err := cat.Build(delegatePerms, config.Tools{}, ReadOnly, toolschema.OpenAI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Has("grade_get") || s.Len() != 11 {
		t.Errorf("offered %v, want the defaults but grade_get", s.Names())
	}
}

func TestDeclarations(t *testing.T) {
	cat := snapshot(t)
	cache := toolschema.NewCache()
	for _, d := range toolschema.Dialects {
		s, err := cat.Build(delegatePerms, config.Tools{}, ReadOnly, d, cache)
		if err != nil {
			t.Fatal(err)
		}
		decls := s.Declarations()
		if len(decls) != 12 {
			t.Fatalf("%s: %d declarations, want 12", d, len(decls))
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
	if cache.Len() != 12*len(toolschema.Dialects) {
		t.Errorf("the cache holds %d schemas, want one per tool and dialect", cache.Len())
	}
}

func TestNilCatalogue(t *testing.T) {
	var c *Catalogue
	if err := c.Check(); err == nil {
		t.Error("a nil catalogue passes Check")
	}
	s, err := c.Build(delegatePerms, config.Tools{}, ReadOnly, toolschema.OpenAI, nil)
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
		{"a gated write now a read", func(c *Catalogue) {
			ct := c.Tools["document_create"]
			ct.Kind = KindRead
			c.Tools["document_create"] = ct
		}, "document_create is no longer a write"},
		{"a gated write gone", func(c *Catalogue) { delete(c.Tools, "grade_submit") }, "grade_submit is not in Core's catalogue"},
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
		if _, _, ok := gateOf(name); !ok {
			t.Errorf("%s is allowed by default and has no gate", name)
		}
		if BuiltinDenied(name) {
			t.Errorf("%s is allowed by default and denied", name)
		}
	}
	for _, gates := range []map[string]Gate{Gates, WriteGates} {
		for name := range gates {
			if BuiltinDenied(name) {
				t.Errorf("%s has a gate and is denied", name)
			}
			if !slices.Contains(DefaultAllow, name) {
				t.Errorf("%s has a gate and is not allowed by default", name)
			}
		}
	}
	for name := range WriteGates {
		if _, ok := Gates[name]; ok {
			t.Errorf("%s is gated as a read and as a write", name)
		}
	}
}

func TestGateAllowed(t *testing.T) {
	for _, tc := range []struct {
		gate  Gate
		perms map[string]string
		want  bool
	}{
		{Gate{}, map[string]string{"document_read": "autonomous"}, false},
		{Gate{Any: []string{"a", "b"}}, map[string]string{"b": "confirm_required"}, true},
		{Gate{Any: []string{"a", "b"}}, map[string]string{"a": "denied", "b": "no"}, false},
		{Gate{All: []string{"a", "b"}}, map[string]string{"a": "autonomous", "b": "pending_review"}, true},
		{Gate{All: []string{"a", "b"}}, map[string]string{"a": "autonomous"}, false},
		{Gate{Any: []string{"a"}, All: []string{"b"}}, map[string]string{"a": "autonomous", "b": "denied"}, false},
	} {
		if got := tc.gate.Allowed(tc.perms); got != tc.want {
			t.Errorf("%+v with %v: %v, want %v", tc.gate, tc.perms, got, tc.want)
		}
	}
}

func TestBuiltinDenied(t *testing.T) {
	denied := []string{"agent_create", "agent_issue_token", "agent_withdraw", "credential_list", "credential_issue_token",
		"credential_set_password", "actor_get", "actor_register", "actor_set_owner", "me_get", "me_memberships", "me_site_chat",
		"member_add_delegate", "member_delegate_defaults",
		"action_withdraw", "conversation_answer", "conversation_open", "conversation_retract",
		"conversation_messages", "conversation_inbox", "preset_create", "course_create", "course_update", "course_archive",
		"course_activate", "course_seat_instructor", "course_list", "term_list", "department_list", "document_upload_url",
		"action_list_mine", "event_list"}
	for _, name := range denied {
		if !BuiltinDenied(name) {
			t.Errorf("%s is not denied", name)
		}
	}
	for _, name := range []string{"course_get", "document_get", "action_get", "courses_get", "agentx", "document_create",
		"grade_submit", "submission_submit", "assignment_update", "memberx", "member_add", "member_update_perms",
		"member_update_perms_bulk", "member_rescope", "member_pause", "member_resume", "member_remove", "member_list", "member_get",
		"member_lookup_actor", "member_add_delegates", "action_decide", "action_review", "action_get", "action_list_proposed"} {
		if BuiltinDenied(name) {
			t.Errorf("%s is denied", name)
		}
	}
}
