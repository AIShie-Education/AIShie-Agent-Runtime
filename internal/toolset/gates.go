// Package toolset decides which of Core's tools a seat's model is offered,
// and runs the calls the model makes (Core's docs/agent-runtime.md §4, §6.1,
// §3.1 rules 1, 5 and 6; docs/design.md §4).
//
// A seat is offered
//
//	toolset(seat) = { t in the catalogue | t's gate is allowed by the seat's perms,
//	                  t in allow, not in deny, not in the built-in deny list,
//	                  and t is a read, or a write in a conversation that may have them }
//
// Which conversations may have writes is the worker's to say (ReadWrite):
// only one the agent's owner opened, and only while tools.writes is on.
// Every call the model makes is checked again before it reaches Core: the
// tool must be one offered, its arguments a JSON object that Core's own
// schema takes once the runtime has set course_id, and the tool not denied;
// a member write must leave alone the agent's own seat and those of the
// people it acts for (SeatGuard); a write is bound to the idempotency key
// the runtime gives it, and counted against the answer's writes. Core's
// permissions stand over all of it: a write at confirm_required comes back
// proposed and waits for a person, at autonomous it is executed, and denied
// is refused. The runtime only narrows what Core allows.
package toolset

import (
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// Gate is the permissions that let a seat be offered a tool, as Core's own
// declaration of the tool checks them (its tool.Gate): allowed when any one
// of Any is not denied in the seat's perms, and every one of All is not
// either. A single-permission gate is an Any of one; Core's gates of
// several permissions without Any (grade.regrade's) are All.
type Gate struct {
	Any []string
	All []string
}

// Allowed reports whether perms, a seat's permission-to-level map from
// me_memberships, lets the gate through. A permission missing from perms is
// denied, and so is a level the runtime does not know: the runtime never
// reads more into a seat than Core wrote there. A gate that names no
// permission lets nothing through.
func (g Gate) Allowed(perms map[string]string) bool {
	if len(g.Any)+len(g.All) == 0 {
		return false
	}
	allowed := func(p string) bool {
		switch perms[p] {
		case core.LevelConfirmRequired, core.LevelPendingReview, core.LevelAutonomous:
			return true
		}
		return false
	}
	return (len(g.Any) == 0 || slices.ContainsFunc(g.Any, allowed)) && !slices.ContainsFunc(g.All, func(p string) bool { return !allowed(p) })
}

// Gates are the permission gates of the read tools a model may be offered,
// kept by hand because GET /v1/tools does not name them (§4, §10 item 2):
// each is the permissions Core's declaration of the tool checks
// (internal/tools in AIShie-Core). CheckCatalogue holds them to the
// catalogue whenever its hash changes. A tool with no gate here or in
// WriteGates is never offered: event_list and action_list_mine, which §4
// gates on document_read, are left out with BuiltinDeny's reason.
var Gates = map[string]Gate{
	"course_get":      {Any: []string{"document_read"}},
	"assignment_list": {Any: []string{"document_read"}},
	"assignment_get":  {Any: []string{"document_read"}},
	"document_list":   {Any: []string{"document_read", "rubric_read"}},
	"document_get":    {Any: []string{"document_read", "rubric_read", "submission_read", "grade_read"}},
	// Every version of a document, for whoever reads drafts; Core then
	// holds the caller to the document's kind as document_get does.
	"document_versions": {Any: []string{"document_read_draft"}},
	"submission_list":   {Any: []string{"submission_read"}},
	"submission_get":    {Any: []string{"submission_read"}},
	// Where every student in the caller's scope stands on an assignment,
	// those who have not started among them; their names only for a seat
	// that reads the roster.
	"submission_roster": {Any: []string{"submission_read"}},
	"grade_list":        {Any: []string{"grade_read"}},
	"grade_get":         {Any: []string{"grade_read"}},
	"component_tree":    {Any: []string{"grade_read"}},
	"gradebook_get":     {Any: []string{"grade_read"}},
	// The roster, and whom an email or an actor id names, which Core gates
	// on member_manage: it is for whoever seats people (member_add takes
	// the actor it finds).
	"member_list":         {Any: []string{"member_read"}},
	"member_get":          {Any: []string{"member_read"}},
	"member_lookup_actor": {Any: []string{"member_manage"}},
	// The approval and review queues, and one action in full: what a seat
	// that decides proposals reads before it recommends a decision.
	"action_list_proposed":       {Any: []string{"action_decide"}},
	"action_list_pending_review": {Any: []string{"action_decide"}},
	"action_get":                 {Any: []string{"action_decide"}},
	// The course's join links as their makers and revokers see them, never
	// a token, on member_invite, as Core gates them: what a seat that may
	// revoke one reads first.
	"course_join_link_list": {Any: []string{"member_invite"}},
	// What deleting an assignment for good would take with it, counted
	// (never a person named), and what Core would refuse the seat now: on
	// assignment_write, as Core gates it and the deletion. A model reads
	// it before assignment_delete, which takes its counts back.
	"assignment_delete_preview": {Any: []string{"assignment_write"}},
	// A course's group sets, their groups and sign-up, and one set with
	// who is in no group, each group's work and its history: on
	// document_read, as Core gates them. Core names the members only to a
	// seat that reads the roster, within its student scope, and to a
	// student (or a student's own agent) their own group's.
	"group_set_list": {Any: []string{"document_read"}},
	"group_set_get":  {Any: []string{"document_read"}},
	// A group assignment's peer form, and to a student in a group of its
	// set (or their own agent) their own task: whom they evaluate, the
	// window, their own sheet. On document_read, as Core gates it.
	"peer_form_get": {Any: []string{"document_read"}},
	// Every sheet of a group, raters named, and what each member received:
	// evidence for grading, on either grade permission, as Core gates it,
	// of the groups wholly within the seat's student scope.
	"peer_review_results": {Any: []string{"grade_submit", "grade_post"}},
}

// WriteGates are the gates of the writes a model may be offered, where the
// seat's perms allow them and only in a conversation that may have writes
// (ReadWrite): the course's work that a person may ask their own agent to
// do for them, and nothing of the runtime's own or of the platform's. Each
// is the permissions Core's declaration of the tool checks (internal/tools
// in AIShie-Core): a document's write is any of the three kinds' (the
// document's kind then names the one that governs, as reading does), and
// grade_regrade takes both grade permissions, at the lower of their levels.
// The course's members are managed on member_manage, which only someone who
// manages them may give a seat, and Core holds every grant to what the
// granter holds; Run keeps each such write off the seats the model must
// never change (SeatGuard). A proposal is decided or reviewed on
// action_decide, which Core holds an agent's at confirm_required, so that
// the model's decision is itself a proposal a person confirms; Run refuses
// either at any other level (Decides). CheckCatalogue holds them to
// the catalogue as it holds the reads, each still a write. Core decides
// every call again at its own level: these only keep from the model what
// it could never do.
var WriteGates = map[string]Gate{
	"assignment_create":    {Any: []string{"assignment_write"}},
	"assignment_update":    {Any: []string{"assignment_write"}},
	"assignment_publish":   {Any: []string{"assignment_write"}},
	"assignment_unpublish": {Any: []string{"assignment_write"}},
	// An assignment deleted for good, with its work and grades, given back
	// the counts assignment_delete_preview showed: Core refuses an agent
	// one anybody has started on (people_only), so a model deletes only an
	// assignment with no work and no grade, and a person the rest.
	"assignment_delete":    {Any: []string{"assignment_write"}},
	"component_create":     {Any: []string{"assignment_write"}},
	"component_update":     {Any: []string{"assignment_write"}},
	"component_move":       {Any: []string{"assignment_write"}},
	"document_create":      {Any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_add_version": {Any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_publish":     {Any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_archive":     {Any: []string{"document_write", "submission_write", "grade_submit"}},
	// Forming groups: sets and groups made, changed and archived, students
	// placed and split among them, on assignment_write, as Core gates them
	// (groups exist for group work and grant nothing). Core holds a split
	// to a seat that reaches every student, and a placement that touches a
	// group's work to a call that says so (affects_work).
	"group_set_create":  {Any: []string{"assignment_write"}},
	"group_set_update":  {Any: []string{"assignment_write"}},
	"group_create":      {Any: []string{"assignment_write"}},
	"group_update":      {Any: []string{"assignment_write"}},
	"group_set_members": {Any: []string{"assignment_write"}},
	"group_split":       {Any: []string{"assignment_write"}},
	// A group assignment's peer form, part of setting the work.
	"peer_form_set": {Any: []string{"assignment_write"}},
	// A student signing up to a group, switching or leaving it: on
	// submission_write, scoped to the student, as Core gates it; a
	// student's own agent proposes it, and the student confirms.
	"group_sign_up": {Any: []string{"submission_write"}},
	// Renaming a document or moving it in the list, and bringing back one
	// archived: whoever may archive it, as document_archive.
	"document_update":    {Any: []string{"document_write", "submission_write", "grade_submit"}},
	"document_unarchive": {Any: []string{"document_write", "submission_write", "grade_submit"}},
	"grade_submit":       {Any: []string{"grade_submit"}},
	"grade_post":         {Any: []string{"grade_post"}},
	"grade_regrade":      {All: []string{"grade_submit", "grade_post"}},
	// One member's adjustment of a grade given from a group's: a draft's
	// on grade_submit, a posted grade's as a regrade, at the lower of the
	// two, which Core decides by the grade it names (its gate is either).
	"grade_adjust": {Any: []string{"grade_submit", "grade_post"}},
	// Peer evaluation counted in each member's grade at the form's weight:
	// it writes drafts and regrades posted grades, gated as a regrade, on
	// both grade permissions at the lower of their levels.
	"grade_apply_peer": {All: []string{"grade_submit", "grade_post"}},
	// A total overridden, its override cleared, or its feedback written:
	// each writes what a student is shown at once, and Core gates them as
	// a regrade, on both grade permissions at the lower of their levels.
	"grade_override_total": {All: []string{"grade_submit", "grade_post"}},
	"grade_clear_override": {All: []string{"grade_submit", "grade_post"}},
	"grade_comment_total":  {All: []string{"grade_submit", "grade_post"}},
	// Ungraded work no longer counted as zero: gated as posting as final
	// is, on grade_post.
	"grade_undo_ungraded_as_zero": {Any: []string{"grade_post"}},
	"submission_create":           {Any: []string{"submission_write"}},
	"submission_update_draft":     {Any: []string{"submission_write"}},
	"submission_submit":           {Any: []string{"submission_write"}},
	"submission_set_lateness":     {Any: []string{"grade_submit"}},
	"submission_record_missing":   {Any: []string{"grade_submit"}},
	"member_add":                  {Any: []string{"member_manage"}},
	"member_update_perms":         {Any: []string{"member_manage"}},
	"member_update_perms_bulk":    {Any: []string{"member_manage"}},
	"member_rescope":              {Any: []string{"member_manage"}},
	"member_pause":                {Any: []string{"member_manage"}},
	"member_resume":               {Any: []string{"member_manage"}},
	"member_remove":               {Any: []string{"member_manage"}},
	// Whose work a group's submission is, corrected: gated as correcting
	// lateness is, since with whom a student handed work in is not theirs
	// to declare.
	"submission_set_members": {Any: []string{"grade_submit"}},
	// A seat's roster role, a fact of the roster that grants nothing, is
	// changed as any other member write, and kept off the same seats.
	"member_set_role": {Any: []string{"member_manage"}},
	// The course's title and description, which Core lets whoever manages
	// its members change from their seat: a course write, not the
	// platform's administration of courses that course_update is.
	"course_update_details": {Any: []string{"member_manage"}},
	// A join link revoked before its ten minutes are up: it seats nobody
	// after. (Making one is denied: BuiltinDeny.)
	"course_join_link_revoke": {Any: []string{"member_invite"}},
	"action_decide":           {Any: []string{"action_decide"}},
	"action_review":           {Any: []string{"action_decide"}},
}

// DefaultAllow is the allowlist when an agent's configuration names none:
// the read tools of §2.3, and every gated write, which only a conversation
// that may have writes is offered.
var DefaultAllow = append(slices.Clone(defaultReads), sortedKeys(WriteGates)...)

// defaultReads are the read tools of §2.3, the versions of a document and
// where students stand on an assignment, the roster's, the queues of
// proposals, the course's join links, what deleting an assignment would
// take, the course's groups, and a group assignment's peer form and its
// results.
var defaultReads = []string{
	"course_get", "document_list", "document_get", "document_versions", "assignment_list", "assignment_get", "assignment_delete_preview",
	"submission_list", "submission_get", "submission_roster", "grade_list", "grade_get", "component_tree", "gradebook_get",
	"member_list", "member_get", "member_lookup_actor",
	"action_list_proposed", "action_list_pending_review", "action_get",
	"course_join_link_list",
	"group_set_list", "group_set_get", "peer_form_get", "peer_review_results",
}

// BuiltinDeny is never offered to a model, whatever the configuration or
// the seat's perms say (§6.1, design §4): a name ending in * covers every
// tool it begins. The gates above offer nothing beyond them; this list is
// the second lock, checked again by Build and by Run whatever built the
// set, and a gate for a tool on it fails Check. Each entry, and why:
var BuiltinDeny = []string{
	// The runtime reads and answers conversations itself, from the one
	// conversation it is answering (§6.1): a conversation tool would let
	// the model read other people's conversations (a tutor's token reads
	// every one addressed to it), or open, ask in, answer, close or
	// retract one in someone else's name. conversation_draft among them:
	// the runtime writes an answer's draft itself, and a model writing one
	// would show the asker whatever it liked as the answer to come. So are
	// a message's files (conversation_attachment, Core's attachments): a
	// tutor's token reads the files of every conversation addressed to it,
	// and the attachment's URL is a credential; the runtime fetches them
	// itself, for the conversation it answers alone. conversation_upload_url
	// is a signed URL for bytes a model cannot send.
	"conversation_*",
	// The runtime follows the course's events itself (design §5.4), and
	// action_list_mine returns the agent's own actions, among them the
	// answers it wrote in other people's conversations, which a worker
	// answering one conversation must never read.
	"event_list", "action_list_mine",
	// Withdrawing acts on any proposal of the agent's, the answers the
	// runtime follows in other conversations among them. (Deciding and
	// reviewing are gated: the model's decision is a proposal a person
	// confirms, and Run refuses one that would not be.)
	"action_withdraw",
	// Accounts, agents and credentials: platform administration (actor.),
	// the owner's own management of their agents, their tokens and seats
	// (agent.), the caller's own tokens and password (credential.), and
	// who the caller is and where it sits (me.), which the runtime reads
	// itself. A model issuing a token would put a credential in text;
	// revoking, suspending or withdrawing would stop the agent; registering
	// or re-owning actors is no one's to do through a model.
	"actor_*", "agent_*", "credential_*", "me_*",
	// Seating agents: member_add_delegate brings in an agent of the
	// caller's own, on agent_delegate, and member_delegate_defaults
	// previews what it would give one. An agent never seats agents (Core
	// refuses a delegate's, and no model is to multiply itself); the rest
	// of member_* is gated on the seat's perms, and kept off the seats the
	// model must never change (SeatGuard).
	"member_add_delegate", "member_delegate_defaults",
	// A temporary password for a member who has forgotten theirs, shown
	// once: a credential in the model's text. Core refuses an agent's
	// call too (people_only).
	"member_reset_password",
	// The platform's own administration, which Core gates on a platform
	// or department administrator's role and no course permission grants:
	// courses made, changed, activated, archived, moved between
	// departments, listed across the platform, their instructors seated;
	// presets, terms and departments.
	"course_create", "course_update", "course_activate", "course_archive", "course_move", "course_seat_instructor",
	"course_list", "preset_*", "term_*", "department_*",
	// A signed upload URL is for bytes, which the model cannot send, and
	// is a credential for the upload besides.
	"document_upload_url",
	// One file of a version with a fresh URL, which is a credential for
	// the file: document_get gives the model every file of a version, and
	// one of them by the runtime's own file_id, fetched by the runtime
	// itself (design §4, Files), which asks document_file for a fresh URL
	// when one has lapsed.
	"document_file",
	// Purging a document or a version, uploaded by mistake, deletes its
	// text and file for good: an administrator's tool, which no course
	// permission grants.
	"document_purge",
	// A join link seats whoever opens it as a student, at once and by no
	// proposal, and its token, returned once, is a credential that would
	// be in the model's text. Revoking one, and listing them, are gated
	// on member_invite.
	"course_join_link_create",
	// Core's memory of the agent: entries about its owner, about each
	// asker, and the course's, reached by the conversation the call names.
	// The runtime keeps each conversation's memory itself and answers one
	// conversation from that conversation alone (design §6); a memory
	// tool would let the model read what it keeps about other askers by
	// naming their conversations, and write about people.
	"memory_*",
	// A document's text version (Core #43): document_get gives it, in
	// parts that fit a result, and the runtime reads document_text itself
	// for a text longer than document_get holds (design §4, Text
	// versions). Writing one, or asking for one to be transcribed again,
	// is the course's staff's, in the front end: a model's text would
	// stand in place of the file for every reader after. The rest,
	// document_text_queue, _file, _renew and _complete, are the
	// transcription service's, which the runtime's transcriber calls with
	// the service's own credential, and Core refuses to any other.
	"document_text", "document_text_*",
	// The site's service credentials (the transcriber's, the agent
	// runtime's): issued, listed and revoked by the platform's
	// administrators alone, and a token issued is a credential in the
	// model's text.
	"service_*",
	// The site's agent runtime's own service (AIShie-Core #52): who
	// owns an agent and how it is hosted, and its one token issued and
	// revoked by its id, which the runtime calls with the agent_runtime
	// service's credential, over REST, as it hosts an agent, and Core
	// refuses to any other (service_only). agent_* covers them; they are
	// named for what they are. So are its renditions (agent_runtime.
	// rendition_claim, _file, _renew, _upload_url and _complete), the PDFs
	// it makes of Office files with the same credential: their URLs and
	// upload tokens are credentials for a course's files.
	"agent_runtime_*",
	// Sending a failed PDF rendition of a file back to be converted again
	// (AIShie-Core's renditions): the conversion is the site's plumbing,
	// which staff send back from the front end where it failed; a model
	// has nothing to judge it by, and its PDF is read as Core gives it.
	// conversation_rendition_retry, a message's file's, is
	// conversation_*'s.
	"document_rendition_retry",
	// Exporting conversations for audit (AIShie-Core #51): every
	// conversation of the site, a department or a course, retracted
	// messages with their text, as files whose URLs are credentials for
	// them, by the site's and the departments' administrators alone. Core
	// refuses an agent's export whatever role it holds (people_only): an
	// export is a person's, who answers for taking it away; and gives an
	// export's files again (conversation_export_file) to its maker alone.
	// conversation_* covers them; they are named for what they are.
	"conversation_export", "conversation_export_*",
	// The site's identity providers for single sign-on, set up, changed,
	// switched, removed and tested by the platform's administrators
	// alone: a provider's client secret is a credential, and a change
	// decides who signs in.
	"sso_*",
	// A peer evaluation is a person's judgment of their classmates' part
	// in their group's work: a student writes their own sheet, and Core
	// refuses an agent's whatever it holds (people_only). Reading the form,
	// and the results for those who grade, is gated.
	"peer_review_submit",
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

// gateOf is name's gate and kind: a read's from Gates, a write's from
// WriteGates.
func gateOf(name string) (Gate, string, bool) {
	if g, ok := Gates[name]; ok {
		return g, KindRead, true
	}
	if g, ok := WriteGates[name]; ok {
		return g, KindWrite, true
	}
	return Gate{}, "", false
}

// Bound are the arguments the runtime sets and the model never sees: the
// course is always the conversation's, keys are the runtime's, and so is
// what a write revises, which the runtime names for its own answers alone
// and never for a model's write (§2.2, §3.8 step 1).
var Bound = []string{"course_id", "idempotency_key", "revises"}

// Kinds of tool, as the catalogue names them.
const (
	KindRead  = "read"
	KindWrite = "write"
)
