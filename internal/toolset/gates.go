// Package toolset decides which of Core's tools a seat's model is offered,
// and runs the calls the model makes (Core's docs/agent-runtime.md §4, §6.1,
// §3.1 rules 1, 5 and 6).
//
// A seat is offered
//
//	toolset(seat) = { t in the catalogue | t's gate is allowed by the seat's perms,
//	                  t in allow, not in deny, not in the built-in deny list, and t is a read }
//
// and every call the model makes is checked again before it reaches Core:
// the tool must be one offered, its arguments a JSON object that Core's own
// schema takes once the runtime has set course_id, and the tool not denied.
// Core's permissions stand over all of it; the runtime only narrows them.
package toolset

import (
	"slices"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
)

// Gate is the permissions that let a seat be offered a tool: allowed when
// any one of them is not denied in the seat's perms. A single-permission
// gate is an Any of one.
type Gate struct {
	Any []string
}

// Allowed reports whether perms, a seat's permission-to-level map from
// me_memberships, lets the gate through. A permission missing from perms is
// denied, and so is a level the runtime does not know: the runtime never
// reads more into a seat than Core wrote there.
func (g Gate) Allowed(perms map[string]string) bool {
	return slices.ContainsFunc(g.Any, func(p string) bool {
		switch perms[p] {
		case core.LevelConfirmRequired, core.LevelPendingReview, core.LevelAutonomous:
			return true
		}
		return false
	})
}

// Gates are the permission gates of the read tools a model may be offered,
// kept by hand because GET /v1/tools does not name them (§4, §10 item 2).
// CheckCatalogue holds them to the catalogue whenever its hash changes. A
// tool with no gate here is never offered: event_list and action_list_mine,
// which §4 gates on document_read, are left out with BuiltinDeny's reason.
var Gates = map[string]Gate{
	"course_get":      {Any: []string{"document_read"}},
	"assignment_list": {Any: []string{"document_read"}},
	"assignment_get":  {Any: []string{"document_read"}},
	"document_list":   {Any: []string{"document_read", "rubric_read"}},
	"document_get":    {Any: []string{"document_read", "rubric_read", "submission_read", "grade_read"}},
	"submission_list": {Any: []string{"submission_read"}},
	"submission_get":  {Any: []string{"submission_read"}},
	"grade_list":      {Any: []string{"grade_read"}},
	"grade_get":       {Any: []string{"grade_read"}},
	"component_tree":  {Any: []string{"grade_read"}},
	"gradebook_get":   {Any: []string{"grade_read"}},
}

// DefaultAllow is the allowlist when an agent's configuration names none:
// the read tools of §2.3.
var DefaultAllow = []string{
	"course_get", "document_list", "document_get", "assignment_list", "assignment_get",
	"submission_list", "submission_get", "grade_list", "grade_get", "component_tree", "gradebook_get",
}

// BuiltinDeny is never offered to a model in M1 and M2, whatever the
// configuration says (§6.1): a name ending in * covers every tool it
// begins. Every write is denied besides, by the catalogue's kind.
//
// Beside the handout's list, event_list and action_list_mine: the runtime
// calls them itself, and action_list_mine returns the agent's own actions,
// the answers it wrote in other people's conversations among them, which a
// tutor answering one conversation must never read (§6.1).
var BuiltinDeny = []string{
	"agent_*", "credential_*", "actor_*", "member_*",
	"action_decide", "action_review", "action_withdraw", "action_list_mine", "event_list",
	"conversation_*", "preset_*", "course_create", "course_update",
	"term_*", "department_*", "document_upload_url",
}

// BuiltinDenied reports whether name is on the built-in deny list.
func BuiltinDenied(name string) bool { return denied(name, BuiltinDeny) }

// denied reports whether a deny list covers name: an entry ending in *
// covers every name it begins, any other only itself. Core's names never
// hold a *, so an entry cannot mean both. A configured list is read the same
// way as the built-in one: an operator's grade_* must take the grade tools
// away, not match nothing and leave them offered.
func denied(name string, list []string) bool {
	return slices.ContainsFunc(list, func(pattern string) bool {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			return strings.HasPrefix(name, prefix)
		}
		return name == pattern
	})
}

// Bound are the arguments the runtime sets and the model never sees: the
// course is always the conversation's, and keys are the runtime's (§2.2,
// §3.8 step 1).
var Bound = []string{"course_id", "idempotency_key"}

// Kinds of tool, as the catalogue names them.
const (
	KindRead  = "read"
	KindWrite = "write"
)
