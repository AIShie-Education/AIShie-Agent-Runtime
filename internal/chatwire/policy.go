package chatwire

import (
	"slices"
	"sort"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
)

// gate is the smallest extraction of the old worker's tool permission table.
// Core still authorizes every call; this table only prevents the model being
// offered a call its current seat could never make.
type gate struct {
	any []string
	all []string
}

func (g gate) allowed(perms map[string]string) bool {
	if len(g.any)+len(g.all) == 0 {
		return false
	}
	allowed := func(name string) bool {
		switch perms[name] {
		case core.LevelConfirmRequired, core.LevelPendingReview, core.LevelAutonomous:
			return true
		default:
			return false
		}
	}
	return (len(g.any) == 0 || slices.ContainsFunc(g.any, allowed)) &&
		!slices.ContainsFunc(g.all, func(name string) bool { return !allowed(name) })
}

var readGates = map[string]gate{
	"course_get":                 {any: []string{"document_read"}},
	"course_materials_search":    {any: []string{"document_read", "rubric_read"}},
	"document_text":              {any: []string{"document_read", "rubric_read", "submission_read", "grade_read"}},
	"assignment_list":            {any: []string{"document_read"}},
	"assignment_get":             {any: []string{"document_read"}},
	"document_list":              {any: []string{"document_read", "rubric_read"}},
	"document_get":               {any: []string{"document_read", "rubric_read", "submission_read", "grade_read"}},
	"document_versions":          {any: []string{"document_read_draft"}},
	"submission_list":            {any: []string{"submission_read"}},
	"submission_get":             {any: []string{"submission_read"}},
	"submission_roster":          {any: []string{"submission_read"}},
	"grade_list":                 {any: []string{"grade_read"}},
	"grade_get":                  {any: []string{"grade_read"}},
	"component_tree":             {any: []string{"grade_read"}},
	"gradebook_get":              {any: []string{"grade_read"}},
	"member_list":                {any: []string{"member_read"}},
	"member_get":                 {any: []string{"member_read"}},
	"member_lookup_actor":        {any: []string{"member_manage"}},
	"action_list_proposed":       {any: []string{"action_decide"}},
	"action_list_pending_review": {any: []string{"action_decide"}},
	"action_get":                 {any: []string{"action_decide"}},
	"course_join_link_list":      {any: []string{"member_invite"}},
	"assignment_delete_preview":  {any: []string{"assignment_write"}},
	"group_set_list":             {any: []string{"document_read"}},
	"group_set_get":              {any: []string{"document_read"}},
	"peer_form_get":              {any: []string{"document_read"}},
	"peer_review_results":        {any: []string{"grade_submit", "grade_post"}},
}

var writeGates = map[string]gate{
	"assignment_create":           {any: []string{"assignment_write"}},
	"assignment_update":           {any: []string{"assignment_write"}},
	"assignment_publish":          {any: []string{"assignment_write"}},
	"assignment_unpublish":        {any: []string{"assignment_write"}},
	"assignment_delete":           {any: []string{"assignment_write"}},
	"component_create":            {any: []string{"assignment_write"}},
	"component_update":            {any: []string{"assignment_write"}},
	"component_move":              {any: []string{"assignment_write"}},
	"document_create":             {any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_add_version":        {any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_publish":            {any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_archive":            {any: []string{"document_write", "submission_write", "grade_submit"}},
	"group_set_create":            {any: []string{"assignment_write"}},
	"group_set_update":            {any: []string{"assignment_write"}},
	"group_create":                {any: []string{"assignment_write"}},
	"group_update":                {any: []string{"assignment_write"}},
	"group_set_members":           {any: []string{"assignment_write"}},
	"group_split":                 {any: []string{"assignment_write"}},
	"peer_form_set":               {any: []string{"assignment_write"}},
	"group_sign_up":               {any: []string{"submission_write"}},
	"document_update":             {any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_unarchive":          {any: []string{"document_write", "submission_write", "grade_submit"}},
	"grade_submit":                {any: []string{"grade_submit"}},
	"grade_post":                  {any: []string{"grade_post"}},
	"grade_regrade":               {all: []string{"grade_submit", "grade_post"}},
	"grade_adjust":                {any: []string{"grade_submit", "grade_post"}},
	"grade_apply_peer":            {all: []string{"grade_submit", "grade_post"}},
	"grade_override_total":        {all: []string{"grade_submit", "grade_post"}},
	"grade_clear_override":        {all: []string{"grade_submit", "grade_post"}},
	"grade_comment_total":         {all: []string{"grade_submit", "grade_post"}},
	"grade_undo_ungraded_as_zero": {any: []string{"grade_post"}},
	"submission_create":           {any: []string{"submission_write"}},
	"submission_update_draft":     {any: []string{"submission_write"}},
	"submission_submit":           {any: []string{"submission_write"}},
	"submission_set_lateness":     {any: []string{"grade_submit"}},
	"submission_record_missing":   {any: []string{"grade_submit"}},
	"member_add":                  {any: []string{"member_manage"}},
	"member_update_perms":         {any: []string{"member_manage"}},
	"member_update_perms_bulk":    {any: []string{"member_manage"}},
	"member_rescope":              {any: []string{"member_manage"}},
	"member_pause":                {any: []string{"member_manage"}},
	"member_resume":               {any: []string{"member_manage"}},
	"member_remove":               {any: []string{"member_manage"}},
	"submission_set_members":      {any: []string{"grade_submit"}},
	"member_set_role":             {any: []string{"member_manage"}},
	"course_update_details":       {any: []string{"member_manage"}},
	"course_join_link_revoke":     {any: []string{"member_invite"}},
	"action_decide":               {any: []string{"action_decide"}},
	"action_review":               {any: []string{"action_decide"}},
}

var defaultReads = []string{
	"course_get", "course_materials_search", "document_list", "document_get", "document_text", "document_versions",
	"assignment_list", "assignment_get", "assignment_delete_preview", "submission_list", "submission_get", "submission_roster",
	"grade_list", "grade_get", "component_tree", "gradebook_get", "member_list", "member_get", "member_lookup_actor",
	"action_list_proposed", "action_list_pending_review", "action_get", "course_join_link_list", "group_set_list",
	"group_set_get", "peer_form_get", "peer_review_results",
}

// This mirrors the Runner's independent deny guard. It is checked here too so
// a broad configured allow can never put one of these names in AllowedTools.
var builtinDeny = []string{
	"conversation_*", "memory_*", "event_list", "action_list_mine", "action_withdraw",
	"actor_*", "agent_*", "credential_*", "me_*", "service_*", "agent_runtime_*", "sso_*",
	"preset_*", "term_*", "department_*", "document_upload_url", "document_file", "document_purge",
	"document_rendition_retry", "document_text_*", "course_create", "course_update", "course_activate",
	"course_archive", "course_move", "course_seat_instructor", "course_list", "course_join_link_create",
	"member_add_delegate", "member_delegate_defaults", "member_reset_password", "peer_review_submit",
}

func deniedBy(name string, patterns []string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			return strings.HasPrefix(name, prefix)
		}
		return name == pattern
	})
}

func defaultAllow() []string {
	out := slices.Clone(defaultReads)
	for name := range writeGates {
		out = append(out, name)
	}
	sort.Strings(out[len(defaultReads):])
	return out
}

type toolPolicy struct {
	allowed []string
	denied  []string
	writes  bool
	reads   []string
	changes []string
	seat    runner.Seat
}

func policyFor(eff *config.Effective, membership core.Membership, grant chat.Grant, hosted *config.Hosted) toolPolicy {
	p := toolPolicy{
		denied: slices.Clone(eff.Tools.Deny),
		seat: runner.Seat{
			SelfMemberID: grant.SelfMemberID, PrincipalMemberID: grant.PrincipalMemberID,
			OpenerMemberID: grant.OpenerMemberID, ActionDecideLevel: grant.ActionDecideLevel,
		},
	}
	if eff.Tools.Writes && eff.Tools.Mode != config.ToolsNone {
		p.writes = grant.PrincipalMemberID != "" && grant.OpenerMemberID == grant.PrincipalMemberID ||
			grant.PrincipalMemberID == "" && hosted == nil
	}
	if eff.Tools.Mode == config.ToolsNone {
		return p
	}
	allow := eff.Tools.Allow
	if len(allow) == 0 {
		allow = defaultAllow()
	}
	seen := make(map[string]bool, len(allow))
	for _, name := range allow {
		if seen[name] {
			continue
		}
		seen[name] = true
		g, isRead := readGates[name]
		kindWrite := false
		if !isRead {
			g, kindWrite = writeGates[name]
		}
		if (!isRead && !kindWrite) || !g.allowed(membership.Perms) || deniedBy(name, eff.Tools.Deny) ||
			deniedBy(name, builtinDeny) || kindWrite && !p.writes {
			continue
		}
		p.allowed = append(p.allowed, name)
		if kindWrite {
			p.changes = append(p.changes, name)
		} else {
			p.reads = append(p.reads, name)
		}
	}
	sort.Strings(p.allowed)
	sort.Strings(p.reads)
	sort.Strings(p.changes)
	return p
}
