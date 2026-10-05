package fakecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The scenarios the fixtures are recorded from: every row of the handout's
// §2.4, and the reads the runtime makes, each in a world of its own. One set
// of steps runs against a real Core (record_test.go) and against the fake
// (conformance_test.go) through the world interface, so the two are held to
// the same script.

// world is one course as a scenario needs it: an instructor, Sato, who owns
// the course's tutor agent; a second instructor, Mori, who decides the
// tutor's proposals (nobody decides their own agent's); and two students,
// Yuki (0) and Ken (1). Its methods fail the test on any error.
type world interface {
	course() string
	tutorSeat() string
	studentSeat(i int) string
	// agent is the tutor over MCP, initialized; rest is the tutor over REST.
	agent() *mcpClient
	rest() *restClient
	// base is the server's URL, for requests made by hand.
	base() string
	// tutorToken is the tutor's live token: the one the site's runtime was
	// issued for it.
	tutorToken() string

	// The people.
	ask(student int, body string) (conversationID, messageID string)
	followUp(conversationID, body string) string
	retract(messageID, reason string)
	closeAsOpener(conversationID, reason string)
	setTutorLevel(level string)
	pauseTutor()
	pauseStudent(i int)
	removeStudent(i int)
	removeTutor()
	archiveCourse()
	approve(actionID string)
	reject(actionID, reason string)
	// requestChanges is Mori sending a proposal back for changes, with a
	// note of what to change.
	requestChanges(actionID, note string)
	// expire waits for, or makes, the proposal's expiry; false when this
	// world cannot.
	expire(actionID string) bool
	// revokeTutorToken is the site's runtime revoking the tutor's token
	// (agent_runtime.revoke_token).
	revokeTutorToken()

	// Yuki's own agent, seated as her delegate (preset delegate): its MCP
	// client, its seat, and Yuki asking it.
	ownAgent() *mcpClient
	ownSeat() string
	askOwn(body string) (conversationID, messageID string)

	// as is a person over MCP, initialized: "sato", "mori", "yuki" or "ken".
	// A real Core's recording seats the same Mori, Yuki and Ken in every
	// world, and a key is unique per actor, so a key of theirs must name
	// something of this world's.
	as(who string) *mcpClient
	// listedTutor seats a second agent of Sato's with the tutor preset,
	// listed for the one student and answering the course, as an
	// instructor's tutor for some students is seated: its seat and client.
	listedTutor(student int) (seat string, c *mcpClient)
	// pausePrincipal pauses Sato's seat, which the tutor is the delegate of.
	pausePrincipal()
	// suspendTutor and reactivateTutor are Sato suspending the tutor, and
	// lifting it (agent.suspend, agent.reactivate).
	suspendTutor()
	reactivateTutor()
	// runtimeService is the site's agent runtime over REST, with the agent_runtime
	// service's credential: the tutor, and every agent of the world but
	// mcpAgent's, is a runtime agent whose token it was issued.
	runtimeService() *restClient
	// textService is the site's transcription service over REST, with a
	// credential of the document_text service's.
	textService() *restClient
	// mcpAgent is an agent of Sato's hosted mcp, with a token he issued
	// it, seated as the course's tutor: its actor id, its seat and its
	// client.
	mcpAgent() (agentID, seat string, c *mcpClient)
	// ownerAgent seats an agent of Sato's as his delegate (preset
	// delegate), with perms over the preset's, as an instructor seats an
	// assistant of their own: its seat and its client.
	ownerAgent(perms map[string]string) (seat string, c *mcpClient)
	// setLevel is Sato setting one permission of seat (member.update_perms).
	setLevel(seat, perm, level string)
	// registrar is an agent nobody owns, which the administrator registers
	// and Sato seats with member.add (preset ta), with perms over the
	// preset's: its seat and its client.
	registrar(perms map[string]string) (seat string, c *mcpClient)
	// newcomer registers a person, seated nowhere: their actor id.
	newcomer(name string) string
	// actorOf is the actor id of "yuki", "sato" or "tutor".
	actorOf(who string) string
	// assignment is a published assignment (HW1), material a published
	// material of text (the syllabus), and submit a student handing in
	// work on the assignment: its submission's id.
	assignment() string
	material() string
	submit(student int) string
	// officeMaterial is Sato putting up a published material of files, in
	// order, each under its name: its id and its files' ids.
	officeMaterial(title string, files ...namedFile) (documentID string, fileIDs []string)
	// quiz is Sato putting up an assignment worth 10 points, published or
	// not, in the component the course's HW1 (assignment, published and
	// worth 100) counts toward: its id.
	quiz(title string, published bool) string
	// handIn is a student handing in work on the assignment, which nobody
	// grades: its submission's id.
	handIn(student int, assignmentID string) string
	// gradeAndPost is Sato grading the work and posting the grade, which
	// writes the student's totals down.
	gradeAndPost(submissionID, score string)
	// assistant is an agent of Sato's seated as his delegate (preset
	// delegate) reaching every student, with perms over the preset's, as
	// an instructor seats an assistant who works with the whole class: its
	// seat and its client.
	assistant(perms map[string]string) (seat string, c *mcpClient)
	// groupSet is Sato making a group set of the groups named, in order,
	// and placing the students named in each: its id, and its groups' ids
	// in order.
	groupSet(name string, groups ...groupSpec) (setID string, groupIDs []string)
	// groupAssignment is Sato putting up a published group assignment
	// worth 20 points that uses the set, in the component HW1 counts
	// toward: its id.
	groupAssignment(title, setID string) string
	// groupHandIn is a student starting their group's work on the
	// assignment and handing it in: its submission's id.
	groupHandIn(student int, assignmentID string) string
	// peerForm is Sato giving the group assignment a peer form: shares of
	// 100, open to each group once it has handed in, closing a week from
	// now, counting at 20%.
	peerForm(assignmentID string)
	// post is Sato posting grades.
	post(gradeIDs ...string)
}

// steps is what a scenario recorded, in order.
type steps struct {
	list []map[string]any
}

// tool records a tool call over MCP: what was sent and the envelope that
// came back, and whether content[0].text says the same as
// structuredContent.
func (s *steps) tool(name, tool string, args any, a toolAnswer) {
	step := map[string]any{"step": name, "tool": tool, "args": args, "http_status": a.Status}
	if a.RPCError != nil {
		step["rpc_error"] = a.RPCError
	} else if a.Status == http.StatusOK {
		var text map[string]any
		same := decodeNumbers([]byte(a.Text), &text) == nil && reflect.DeepEqual(text, a.Structured)
		step["is_error"], step["envelope"], step["text_is_envelope"] = a.IsError, a.Structured, same
	} else {
		httpStep(step, a.httpAnswer)
	}
	s.list = append(s.list, step)
}

// http records an answer given at the HTTP level.
func (s *steps) http(name string, a httpAnswer) {
	step := map[string]any{"step": name}
	httpStep(step, a)
	s.list = append(s.list, step)
}

// rest records a REST call.
func (s *steps) rest(name, method, path string, body any, a httpAnswer) {
	step := map[string]any{"step": name, "method": method, "path": path}
	if body != nil {
		step["request"] = body
	}
	httpStep(step, a)
	if a.Header.Get("Idempotency-Replayed") != "" {
		step["replayed_header"] = a.Header.Get("Idempotency-Replayed")
	}
	s.list = append(s.list, step)
}

func httpStep(step map[string]any, a httpAnswer) {
	step["http_status"] = a.Status
	step["content_type"] = a.Header.Get("Content-Type")
	if ra := a.Header.Get("Retry-After"); ra != "" {
		step["retry_after"] = ra
	}
	if wa := a.Header.Get("WWW-Authenticate"); wa != "" {
		step["www_authenticate"] = wa
	}
	var body any
	if strings.HasPrefix(a.Header.Get("Content-Type"), "application/json") && decodeNumbers(a.Body, &body) == nil {
		step["body"] = body
	} else {
		step["body"] = string(a.Body)
	}
}

// fixture is one scenario's file.
func (s *steps) fixture(name string) (any, error) {
	return normalize(map[string]any{"scenario": name, "steps": s.list})
}

// scenario is one script.
type scenario struct {
	name string
	// about says what it holds the fake to.
	about string
	// rateLimited scenarios need Core's limit on (RATE_LIMIT_PER_MINUTE 600).
	rateLimited bool
	run         func(t *testing.T, w world, s *steps)
}

// answer is conversation_answer's arguments under the handout's key.
func answer(w world, conv, msg, body string, attempt int) map[string]any {
	return map[string]any{"course_id": w.course(), "conversation_id": conv, "in_reply_to_message_id": msg, "body": body,
		"idempotency_key": fmt.Sprintf("answer:%s:%s:%d", conv, msg, attempt)}
}

// call makes a tool call as the tutor and records it.
func call(t *testing.T, w world, s *steps, name, tool string, args map[string]any) toolAnswer {
	t.Helper()
	return callAs(t, w.agent(), s, name, tool, args)
}

// callAs makes a tool call as whoever c is, and records it.
func callAs(t *testing.T, c *mcpClient, s *steps, name, tool string, args map[string]any) toolAnswer {
	t.Helper()
	a, err := c.call(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %s: %v", name, tool, err)
	}
	s.tool(name, tool, args, a)
	return a
}

// callShape makes a call whose result the fake answers from canned
// material, and records it without what the result holds: the envelope,
// with the names of the result's fields in place of the result. Who may
// read it, and what comes back when they may not, are Core's; the material
// is the fake's.
func callShape(t *testing.T, c *mcpClient, s *steps, name, tool string, args map[string]any) toolAnswer {
	t.Helper()
	a := callAs(t, c, s, name, tool, args)
	step := s.list[len(s.list)-1]
	if env, ok := step["envelope"].(map[string]any); ok {
		if res, ok := env["result"].(map[string]any); ok {
			keys := make([]string, 0, len(res))
			for k := range res {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			shaped := make(map[string]any, len(env))
			for k, v := range env {
				shaped[k] = v
			}
			delete(shaped, "result")
			shaped["result_fields"] = keys
			step["envelope"] = shaped
		}
	}
	return a
}

// resultOf is a field of an envelope's result.
func resultOf(a toolAnswer, field string) any {
	res, _ := a.Structured["result"].(map[string]any)
	return res[field]
}

func inCourseArgs(w world, more ...any) map[string]any {
	m := map[string]any{"course_id": w.course()}
	for i := 0; i+1 < len(more); i += 2 {
		m[more[i].(string)] = more[i+1]
	}
	return m
}

func wantStatus(t *testing.T, a toolAnswer, status string) {
	t.Helper()
	if a.status() != status {
		t.Fatalf("status %q, want %q: %s", a.status(), status, a.Body)
	}
}

var scenarios = []scenario{
	{name: "initialize", about: "initialize at the pinned revision: capabilities, server info and Core's instructions", run: func(t *testing.T, w world, s *steps) {
		c := newMCPClient(w.base(), w.tutorToken(), nil)
		a, err := c.post(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fakecore-test", "version": "1"}}})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := decodeNumbers(a.Body, &body); err != nil {
			t.Fatalf("%v: %s", err, a.Body)
		}
		if res, ok := body["result"].(map[string]any); ok {
			if info, ok := res["serverInfo"].(map[string]any); ok {
				info["version"] = "<version>"
			}
		}
		s.list = append(s.list, map[string]any{"step": "initialize", "http_status": a.Status, "content_type": a.Header.Get("Content-Type"), "body": body})
	}},
	{name: "protocol_revisions", about: "initialize at other revisions: an older one answered as asked, a newer one with 2025-11-25", run: func(t *testing.T, w world, s *steps) {
		for _, rev := range []string{"2024-11-05", "2025-06-18", "2026-07-28"} {
			a, err := newMCPClient(w.base(), w.tutorToken(), nil).post(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1,
				"method": "initialize", "params": map[string]any{"protocolVersion": rev, "capabilities": map[string]any{},
					"clientInfo": map[string]any{"name": "fakecore-test", "version": "1"}}})
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			if err := json.Unmarshal(a.Body, &body); err != nil {
				t.Fatalf("%v: %s", err, a.Body)
			}
			s.list = append(s.list, map[string]any{"step": "initialize " + rev, "http_status": a.Status, "protocol_version": body.Result.ProtocolVersion})
		}
	}},
	{name: "screened", about: "what Core refuses before the SDK: a batch, an id over 256 bytes, subscriptions/listen", run: func(t *testing.T, w world, s *steps) {
		for _, c := range []struct{ name, body string }{
			{"batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`},
			{"long_id", `{"jsonrpc":"2.0","id":"` + strings.Repeat("7", 300) + `","method":"tools/list"}`},
			{"listen", `{"jsonrpc":"2.0","id":9,"method":"subscriptions/listen","params":{}}`},
			{"not_json", `{"jsonrpc":"2.0",`},
		} {
			a, err := w.agent().postRaw(context.Background(), []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			s.http(c.name, a)
		}
	}},
	{name: "events_cursor", about: "event_list's next_seq: the last event returned, or since_seq when none is, over events the caller may not see", run: func(t *testing.T, w world, s *steps) {
		first := call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		next := first.Structured["result"].(map[string]any)["next_seq"]
		w.pauseStudent(1)
		call(t, w, s, "after_an_event_it_may_not_see", "event_list", inCourseArgs(w, "since_seq", next))
		call(t, w, s, "limit_over_500", "event_list", inCourseArgs(w, "since_seq", 0, "limit", 501))
	}},
	{name: "tools_list", about: "tools/list: every tool's name, annotations, and a hash of its description and schemas", run: func(t *testing.T, w world, s *steps) {
		a, err := w.agent().post(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Result struct {
				Tools []map[string]any `json:"tools"`
			} `json:"result"`
		}
		if err := decodeNumbers(a.Body, &body); err != nil {
			t.Fatalf("%v: %s", err, a.Body)
		}
		var tools []any
		for _, tl := range body.Result.Tools {
			tools = append(tools, map[string]any{"name": tl["name"], "annotations": tl["annotations"],
				"description_sha256": digest(tl["description"]), "input_schema_sha256": digest(tl["inputSchema"]),
				"output_schema_sha256": digest(tl["outputSchema"])})
		}
		sort.Slice(tools, func(i, j int) bool {
			return tools[i].(map[string]any)["name"].(string) < tools[j].(map[string]any)["name"].(string)
		})
		s.list = append(s.list, map[string]any{"step": "tools_list", "http_status": a.Status, "count": len(tools), "tools": tools})
	}},
	{name: "me", about: "me_get and me_memberships: a course tutor's seat, perms capped by its principal, answers_course", run: func(t *testing.T, w world, s *steps) {
		call(t, w, s, "me_get", "me_get", map[string]any{})
		call(t, w, s, "me_memberships", "me_memberships", map[string]any{})
		call(t, w, s, "course_get", "course_get", inCourseArgs(w))
		call(t, w, s, "submissions_not_its_to_read", "submission_list", inCourseArgs(w))
		call(t, w, s, "gradebook_not_its_to_read", "gradebook_get", inCourseArgs(w, "student_member_id", w.studentSeat(0)))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}},
	{name: "own_agent", about: "a student's own agent (§2.6): its seat, the conversation its principal opens, its answer, the reads it may make", run: func(t *testing.T, w world, s *steps) {
		own := w.ownAgent()
		ask := func(name, tool string, args map[string]any) toolAnswer {
			t.Helper()
			a, err := own.call(context.Background(), tool, args)
			if err != nil {
				t.Fatal(err)
			}
			s.tool(name, tool, args, a)
			return a
		}
		ask("memberships", "me_memberships", map[string]any{})
		conv, m1 := w.askOwn("Why did I lose marks on HW3?")
		ask("inbox", "conversation_inbox", inCourseArgs(w))
		ask("get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		wantStatus(t, ask("answer", "conversation_answer", answer(w, conv, m1, "Let me look at your grades.", 1)), "executed")
		ask("submissions", "submission_list", inCourseArgs(w))
		ask("grades", "grade_list", inCourseArgs(w))
		ask("gradebook_other_student", "gradebook_get", inCourseArgs(w, "student_member_id", w.studentSeat(1)))
		ask("events", "event_list", inCourseArgs(w, "since_seq", 0))
	}},
	{name: "opener_removed", about: "§2.4 failed conflict closed: closed by the removal of the opener's seat (closed_reason seat_removed)", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is the exam cumulative?")
		w.removeStudent(0)
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes.", 1))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
	}},
	{name: "tutor_removed", about: "the agent's seat removed: its proposals cancelled (member_removed), its seat gone, its calls denied", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Can I have an extension?")
		args := answer(w, conv, m1, "Ask your instructor.", 1)
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", args), "proposed")
		w.removeTutor()
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "answer_again", "conversation_answer", answer(w, conv, m1, "Ask your instructor.", 2))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
	}},
	{name: "archived", about: "the course archived: every write denied (course_archived), reads still answered", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Will the course run next year?")
		w.archiveCourse()
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes.", 1))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
		call(t, w, s, "close", "conversation_close", inCourseArgs(w, "conversation_id", conv, "idempotency_key", "close:"+conv))
	}},
	{name: "inbox_messages", about: "the inbox's rules and order, and conversation_messages' paging and retractions", run: func(t *testing.T, w world, s *steps) {
		conv, q1 := w.ask(0, "Q1: what is covered in week 3?")
		w.followUp(conv, "Q2: and is the quiz on it?")
		q3 := w.followUp(conv, "Q3: sorry, one more: when is it?")
		w.retract(q1, "Wrong course")
		kconv, _ := w.ask(1, "K1: where are the slides?")
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "inbox_limit_1", "conversation_inbox", inCourseArgs(w, "limit", 1))
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "messages_after_1", "conversation_messages", inCourseArgs(w, "conversation_id", conv, "after_seq", 1))
		call(t, w, s, "messages_before_3_limit_1", "conversation_messages", inCourseArgs(w, "conversation_id", conv, "before_seq", 3, "limit", 1))
		call(t, w, s, "messages_both_cursors", "conversation_messages", inCourseArgs(w, "conversation_id", conv, "after_seq", 1, "before_seq", 3))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		w.retract(q3, "")
		call(t, w, s, "inbox_latest_retracted", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get_other", "conversation_get", inCourseArgs(w, "conversation_id", kconv))
	}},
	{name: "executed_none", about: "§2.4 executed, review_state none: posted, and the conversation leaves the inbox", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Why did I lose marks on HW3?")
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Because the proof skipped a step.", 1)), "executed")
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "inbox_after", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
	}},
	{name: "replay", about: "§2.4 any, replayed: the same key and arguments give the stored outcome", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is HW1 due on Friday?")
		args := answer(w, conv, m1, "Yes, at noon.", 1)
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", args), "executed")
		call(t, w, s, "replay", "conversation_answer", args)
	}},
	{name: "idempotency_conflict", about: "§2.4 error idempotency_conflict: the same key with another body, nothing recorded", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is HW1 due on Friday?")
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes, at noon.", 1)), "executed")
		call(t, w, s, "conflict", "conversation_answer", answer(w, conv, m1, "Yes, at five.", 1))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}},
	{name: "already_answered", about: "§2.4 failed conflict already_answered: a second answer to one message", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is HW1 due on Friday?")
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes, at noon.", 1)), "executed")
		call(t, w, s, "second", "conversation_answer", answer(w, conv, m1, "Yes, at noon, I said.", 2))
	}},
	{name: "executed_pending", about: "§2.4 executed, review_state pending: pending_review posts and a person reviews after", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("pending_review")
		conv, m1 := w.ask(0, "What does question 2 ask?")
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "It asks for a proof by induction.", 1)), "executed")
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
	}},
	{name: "proposed", about: "§2.4 proposed: confirm_required queues the answer; the inbox leaves it out; the view says so", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Can I have an extension?")
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Ask your instructor; I cannot grant one.", 1)), "proposed")
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}},
	{name: "answer_pending", about: "§2.4 failed conflict answer_pending: a second proposal while one waits", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Can I have an extension?")
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Ask your instructor.", 1)), "proposed")
		call(t, w, s, "second", "conversation_answer", answer(w, conv, m1, "Ask your instructor, please.", 2))
	}},
	{name: "denied_level", about: "§2.4 denied: conversation_answer lowered to denied; a target is not looked up for a denied caller; a denial replays as it was", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is there a lecture on Monday?")
		w.setTutorLevel("denied")
		args := answer(w, conv, m1, "Yes.", 1)
		call(t, w, s, "answer", "conversation_answer", args)
		call(t, w, s, "no_such_conversation", "conversation_answer", answer(w, uuid.NewString(), m1, "Yes.", 1))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		w.setTutorLevel("autonomous")
		call(t, w, s, "replay_after_restored", "conversation_answer", args)
		wantStatus(t, call(t, w, s, "next_attempt", "conversation_answer", answer(w, conv, m1, "Yes.", 2)), "executed")
	}},
	{name: "denied_paused", about: "§2.4 denied: the seat paused, every call denied and every perm shown denied", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is there a lecture on Monday?")
		w.pauseTutor()
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes.", 1))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "me", "me_get", map[string]any{})
	}},
	{name: "moved_on", about: "§2.4 failed conflict moved_on: the opener wrote again; answer details.latest_opener_message_id", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "What is on the midterm?")
		m2 := w.followUp(conv, "Actually, is it open book?")
		a := call(t, w, s, "answer_old", "conversation_answer", answer(w, conv, m1, "Chapters 1 to 4.", 1))
		if got := a.str("error", "details", "latest_opener_message_id"); got != m2 {
			t.Fatalf("latest_opener_message_id %q, want %q", got, m2)
		}
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		wantStatus(t, call(t, w, s, "answer_new", "conversation_answer", answer(w, conv, m2, "No, closed book.", 1)), "executed")
	}},
	{name: "closed", about: "§2.4 failed conflict closed: closed by its opener", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Where is room 101?")
		w.closeAsOpener(conv, "Found it myself")
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "In the east wing.", 1))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
	}},
	{name: "not_addressable", about: "§2.4 failed forbidden not_addressable: the opener may no longer address the agent", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Where is room 101?")
		w.pauseStudent(0)
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "In the east wing.", 1))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
	}},
	{name: "invalid_argument", about: "§2.4 failed invalid_argument (empty, over 20000 characters) and the errors of calls never attempted", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Summarise chapter 2, please.")
		call(t, w, s, "empty", "conversation_answer", answer(w, conv, m1, "", 1))
		call(t, w, s, "blank", "conversation_answer", answer(w, conv, m1, " \n\t ", 2))
		call(t, w, s, "too_long", "conversation_answer", answer(w, conv, m1, strings.Repeat("あ", maxMessageChars+1), 3))
		noKey := answer(w, conv, m1, "Chapter 2 is about sets.", 4)
		delete(noKey, "idempotency_key")
		call(t, w, s, "no_key", "conversation_answer", noKey)
		longKey := answer(w, conv, m1, "Chapter 2 is about sets.", 4)
		longKey["idempotency_key"] = strings.Repeat("k", maxKeyChars+1)
		call(t, w, s, "key_too_long", "conversation_answer", longKey)
		badID := answer(w, conv, m1, "Chapter 2 is about sets.", 4)
		badID["conversation_id"] = "not-a-uuid"
		call(t, w, s, "bad_uuid", "conversation_answer", badID)
		extra := answer(w, conv, m1, "Chapter 2 is about sets.", 4)
		extra["mood"] = "cheerful"
		call(t, w, s, "unknown_argument", "conversation_answer", extra)
		call(t, w, s, "not_the_opener", "conversation_answer", map[string]any{"course_id": w.course(), "conversation_id": conv,
			"in_reply_to_message_id": uuid.NewString(), "body": "Chapter 2 is about sets.", "idempotency_key": "answer:other:1"})
		wantStatus(t, call(t, w, s, "at_the_limit", "conversation_answer", answer(w, conv, m1, strings.Repeat("あ", maxMessageChars), 5)), "executed")
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w, "exclude_types", []string{"conversation.ask"}))
	}},
	{name: "not_found", about: "§2.4 error not_found: no such conversation, or course", run: func(t *testing.T, w world, s *steps) {
		_, m1 := w.ask(0, "Hello?")
		ghost := uuid.NewString()
		call(t, w, s, "answer", "conversation_answer", answer(w, ghost, m1, "Hello.", 1))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", ghost))
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", ghost))
		other := uuid.NewString()
		call(t, w, s, "inbox_other_course", "conversation_inbox", map[string]any{"course_id": other})
		call(t, w, s, "retract_nothing", "conversation_retract", inCourseArgs(w, "message_id", ghost, "idempotency_key", "retract:"+ghost))
	}},
	{name: "replayed_rejected", about: "§2.4 replayed rejected: a person rejected the proposal, with a reason action_list_mine shows", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "What is the thesis of chapter 3?")
		args := answer(w, conv, m1, "It is about sets.", 1)
		a := call(t, w, s, "answer", "conversation_answer", args)
		wantStatus(t, a, "proposed")
		w.reject(a.str("action_id"), "Too terse; explain the thesis.")
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		wantStatus(t, call(t, w, s, "next_attempt", "conversation_answer", answer(w, conv, m1, "Chapter 3 argues that every set has a size.", 2)), "proposed")
	}},
	{name: "replayed_changes_requested", about: "§2.4 replayed changes_requested: a person sent the proposal back for changes, with a note action_list_mine shows trimmed; the conversation back in the inbox; the next attempt naming it in revises, proposed and replayed, and its key without revises a conflict", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "What does chapter 4 cover?")
		args := answer(w, conv, m1, "Graphs.", 1)
		a := call(t, w, s, "answer", "conversation_answer", args)
		wantStatus(t, a, "proposed")
		w.requestChanges(a.str("action_id"), "  Say which kinds of graphs, and where the chapter starts.  ")
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		const body = "Chapter 4 covers directed and undirected graphs, from page 61."
		revision := answer(w, conv, m1, body, 2)
		revision["revises"] = a.str("action_id")
		wantStatus(t, call(t, w, s, "revision", "conversation_answer", revision), "proposed")
		call(t, w, s, "revision_replay", "conversation_answer", revision)
		call(t, w, s, "revision_without_revises", "conversation_answer", answer(w, conv, m1, body, 2))
		call(t, w, s, "events_after", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine_after", "action_list_mine", inCourseArgs(w))
	}},
	{name: "revises", about: "revises, the proposal sent back that a call proposes again: refused not_revisable, nothing recorded, naming one still waiting, one rejected, another's action, no action, or given to a read over REST; null and empty naming nothing; a revision sent back and revised in turn, then approved; over REST, the Revises header, its replay, a conflict without it, and one that is no id", run: func(t *testing.T, w world, s *steps) {
		ctx := context.Background()
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "When is the midterm?")
		first := call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Soon.", 1))
		wantStatus(t, first, "proposed")
		a1 := first.str("action_id")
		revising := func(name string, attempt int, body string, revises any) toolAnswer {
			t.Helper()
			args := answer(w, conv, m1, body, attempt)
			args["revises"] = revises
			return call(t, w, s, name, "conversation_answer", args)
		}
		revising("revises_waiting", 2, "In week 7.", a1)
		revising("revises_no_action", 2, "In week 7.", uuid.NewString())

		w.requestChanges(a1, "Give the date.")
		mine := call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		var decision string
		if acts, ok := resultOf(mine, "actions").([]any); ok {
			for _, x := range acts {
				if act, ok := x.(map[string]any); ok && act["id"] == a1 {
					res, _ := act["result"].(map[string]any)
					d, _ := res["decision"].(map[string]any)
					decision, _ = d["by_action_id"].(string)
				}
			}
		}
		if decision == "" {
			t.Fatalf("no decision on %s in action_list_mine: %s", a1, mine.Body)
		}
		revising("revises_others", 2, "On 14 March.", decision)
		second := revising("revision", 2, "On 14 March.", a1)
		wantStatus(t, second, "proposed")
		a2 := second.str("action_id")
		w.requestChanges(a2, "And the room.")
		third := revising("revision_of_revision", 3, "On 14 March, in room B12.", a2)
		wantStatus(t, third, "proposed")
		w.approve(third.str("action_id"))
		revising("revision_of_revision_replay", 3, "On 14 March, in room B12.", a2)
		call(t, w, s, "chain", "action_list_mine", inCourseArgs(w))

		kconv, k1 := w.ask(1, "Is there a resit?")
		null, empty := answer(w, kconv, k1, "Yes.", 1), answer(w, kconv, k1, "Yes.", 1)
		null["revises"], empty["revises"] = nil, ""
		wantStatus(t, call(t, w, s, "revises_null", "conversation_answer", null), "proposed")
		rejected := call(t, w, s, "revises_empty", "conversation_answer", empty)
		w.reject(rejected.str("action_id"), "Say when.")
		kagain := answer(w, kconv, k1, "Yes, in August.", 2)
		kagain["revises"] = rejected.str("action_id")
		call(t, w, s, "revises_rejected", "conversation_answer", kagain)

		r := w.rest()
		do := func(name, method, path string, body any, key, revises string) httpAnswer {
			t.Helper()
			a, err := r.doRevising(ctx, method, path, body, key, revises)
			if err != nil {
				t.Fatal(err)
			}
			s.rest(name, method, path, body, a)
			s.list[len(s.list)-1]["revises_header"] = revises
			return a
		}
		c := "/v1/courses/" + w.course()
		do("rest_read", "GET", c+"/conversations/"+kconv, nil, "", a1)
		rconv, r1 := w.ask(1, "Where do I collect my marked work?")
		path := c + "/conversations/" + rconv + "/answer"
		body := map[string]any{"in_reply_to_message_id": r1, "body": "At the office."}
		key := "answer:" + rconv + ":" + r1 + ":1"
		a := do("rest_answer", "POST", path, body, key, "")
		var prop struct {
			ActionID string `json:"action_id"`
		}
		if err := json.Unmarshal(a.Body, &prop); err != nil || prop.ActionID == "" {
			t.Fatalf("no action_id in %s", a.Body)
		}
		w.requestChanges(prop.ActionID, "Which office, and when it is open.")
		do("rest_replayed_changes_requested", "POST", path, body, key, "")
		rbody := map[string]any{"in_reply_to_message_id": r1, "body": "At the department office, room A3, 9 to 5."}
		rkey := "answer:" + rconv + ":" + r1 + ":2"
		do("rest_revision", "POST", path, rbody, rkey, prop.ActionID)
		do("rest_revision_replay", "POST", path, rbody, rkey, prop.ActionID)
		do("rest_revision_without_revises", "POST", path, rbody, rkey, "")
		do("rest_revises_no_id", "POST", path, rbody, "answer:"+rconv+":"+r1+":3", "the one before")
	}},
	{name: "replayed_cancelled", about: "§2.4 replayed cancelled: the proposal expired (payload.reason proposal_expired)", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Can I resubmit HW1?")
		args := answer(w, conv, m1, "Ask your instructor.", 1)
		a := call(t, w, s, "answer", "conversation_answer", args)
		wantStatus(t, a, "proposed")
		if !w.expire(a.str("action_id")) {
			t.Skip("this world cannot make a proposal expire")
		}
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
	}},
	{name: "approved", about: "a proposal approved: action.approved executed, the answer posted under the proposal, a replay executed; event_list paging", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Is attendance compulsory?")
		args := answer(w, conv, m1, "Yes, for the labs.", 1)
		a := call(t, w, s, "answer", "conversation_answer", args)
		wantStatus(t, a, "proposed")
		w.approve(a.str("action_id"))
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		page := call(t, w, s, "events_page_1", "event_list", inCourseArgs(w, "since_seq", 0, "limit", 2))
		next, ok := resultOf(page, "next_seq").(json.Number)
		if !ok || next == "0" {
			t.Fatalf("events_page_1 gave no next_seq: %s", page.Body)
		}
		call(t, w, s, "events_page_2", "event_list", inCourseArgs(w, "since_seq", next, "limit", 2))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
	}},
	{name: "approved_failed", about: "a proposal approved after the conversation moved on: action.approved failed, a replay failed moved_on", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "When is the exam?")
		args := answer(w, conv, m1, "On the 12th.", 1)
		a := call(t, w, s, "answer", "conversation_answer", args)
		wantStatus(t, a, "proposed")
		w.followUp(conv, "And where?")
		call(t, w, s, "inbox_before", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get_before", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		w.approve(a.str("action_id"))
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}},
	{name: "retract_close", about: "conversation_retract by the author, conversation_close and their refusals; at confirm_required, a retraction of a message retracted already and a close of a closed conversation failed at once, never proposed (AIShie-Core #60), and the reasons the system writes refused at any level", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is the lab open late?")
		a := call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Until nine.", 1))
		wantStatus(t, a, "executed")
		mine := a.str("result", "message_id")
		call(t, w, s, "retract_theirs", "conversation_retract", inCourseArgs(w, "message_id", m1, "idempotency_key", "retract:"+m1))
		call(t, w, s, "retract_mine", "conversation_retract", inCourseArgs(w, "message_id", mine, "reason", "Wrong hours", "idempotency_key", "retract:"+mine))
		call(t, w, s, "retract_again", "conversation_retract", inCourseArgs(w, "message_id", mine, "idempotency_key", "retract:"+mine+":2"))
		// Retracting and closing are document_read's: a person confirms them.
		w.setLevel(w.tutorSeat(), permDocumentRead, "confirm_required")
		call(t, w, s, "retract_again_proposed", "conversation_retract", inCourseArgs(w, "message_id", mine, "idempotency_key", "retract:"+mine+":3"))
		call(t, w, s, "close_reserved_reason", "conversation_close", inCourseArgs(w, "conversation_id", conv, "reason", "seat_removed", "idempotency_key", "close:"+conv+":0"))
		call(t, w, s, "close_reserved_reason_with_agents", "conversation_close", inCourseArgs(w, "conversation_id", conv, "reason", "Conversations_Are_With_Agents",
			"idempotency_key", "close:"+conv+":1"))
		w.setLevel(w.tutorSeat(), permDocumentRead, "autonomous")
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "close", "conversation_close", inCourseArgs(w, "conversation_id", conv, "reason", "Answered as far as I can.", "idempotency_key", "close:"+conv))
		call(t, w, s, "close_again", "conversation_close", inCourseArgs(w, "conversation_id", conv, "idempotency_key", "close:"+conv+":2"))
		w.setLevel(w.tutorSeat(), permDocumentRead, "confirm_required")
		call(t, w, s, "close_again_proposed", "conversation_close", inCourseArgs(w, "conversation_id", conv, "idempotency_key", "close:"+conv+":3"))
		w.setLevel(w.tutorSeat(), permDocumentRead, "autonomous")
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
	}},
	{name: "unauthenticated", about: "401 text/plain: no token, an unknown token, a revoked token", run: func(t *testing.T, w world, s *steps) {
		ctx := context.Background()
		ping := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "me_get", "arguments": map[string]any{}}}
		for _, c := range []struct{ name, token string }{{"no_token", ""}, {"unknown_token", "ais_nosuchtoken0000000000000000"}} {
			a, err := newMCPClient(w.base(), c.token, nil).post(ctx, ping)
			if err != nil {
				t.Fatal(err)
			}
			s.http(c.name, a)
		}
		w.revokeTutorToken()
		a, err := w.agent().post(ctx, ping)
		if err != nil {
			t.Fatal(err)
		}
		s.http("revoked_token", a)
	}},
	{name: "hosting", about: "the site's agent runtime (agent_runtime, a site service): check_owner and agent of a runtime agent, an mcp agent, a suspended one and ids that are no agent's; issue_token replacing the runtime's token, a replay without it, refused for an mcp agent, a suspended one and nobody; revoke_token, and again with none; a question to a runtime agent not hosted and to an mcp agent; me_site_chat, which is no tool any more; the service's credential anywhere else, and an agent's at the service's routes",
		run: func(t *testing.T, w world, s *steps) {
			ctx := context.Background()
			svc := w.runtimeService()
			const base = "/v1/services/agent_runtime"
			do := func(name string, r *restClient, method, path string, body any, key string) httpAnswer {
				t.Helper()
				a, err := r.do(ctx, method, path, body, key)
				if err != nil {
					t.Fatal(err)
				}
				s.rest(name, method, path, body, a)
				return a
			}
			tutor, sato, yuki := w.actorOf("tutor"), w.actorOf("sato"), w.actorOf("yuki")
			const nobody = "0192f3c1-0000-7000-8000-00000000abcd"
			ping := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "me_get", "arguments": map[string]any{}}}
			do("check_owner", svc, "GET", base+"/owners/"+sato+"/agents/"+tutor, nil, "")
			do("check_owner_not_theirs", svc, "GET", base+"/owners/"+yuki+"/agents/"+tutor, nil, "")
			do("check_owner_no_agent", svc, "GET", base+"/owners/"+sato+"/agents/"+nobody, nil, "")
			do("check_owner_a_person", svc, "GET", base+"/owners/"+sato+"/agents/"+yuki, nil, "")
			do("agent", svc, "GET", base+"/agents/"+tutor, nil, "")
			do("agent_no_agent", svc, "GET", base+"/agents/"+nobody, nil, "")
			do("agent_a_person", svc, "GET", base+"/agents/"+yuki, nil, "")
			call(t, w, s, "me_runtime", "me_get", map[string]any{})
			call(t, w, s, "site_chat_gone", "me_site_chat", map[string]any{"on": true, "idempotency_key": "site-chat:on"})
			do("agent_as_an_agent", w.rest(), "GET", base+"/agents/"+tutor, nil, "")
			do("me_as_the_service", svc, "GET", "/v1/me", nil, "")
			a, err := newMCPClient(w.base(), svc.token, nil).post(ctx, ping)
			if err != nil {
				t.Fatal(err)
			}
			s.http("service_over_mcp", a)

			mcpID, mcpSeat, mcp := w.mcpAgent()
			do("agent_mcp", svc, "GET", base+"/agents/"+mcpID, nil, "")
			do("check_owner_mcp", svc, "GET", base+"/owners/"+sato+"/agents/"+mcpID, nil, "")
			do("issue_mcp", svc, "POST", base+"/agents/"+mcpID+"/token", map[string]any{}, "issue:mcp")
			do("revoke_mcp", svc, "POST", base+"/agents/"+mcpID+"/token/revoke", map[string]any{}, "revoke:mcp")
			callAs(t, mcp, s, "me_mcp", "me_get", map[string]any{})
			callAs(t, w.as("ken"), s, "open_mcp", "conversation_open", inCourseArgs(w, "respondent_member_id", mcpSeat,
				"body", "Can you help me?", "idempotency_key", "open:ken:mcp"))

			issue := map[string]any{"label": "agent runtime"}
			issued := do("issue", svc, "POST", base+"/agents/"+tutor+"/token", issue, "issue:1")
			do("issue_replay", svc, "POST", base+"/agents/"+tutor+"/token", issue, "issue:1")
			do("issue_bad_label", svc, "POST", base+"/agents/"+tutor+"/token", map[string]any{"label": " "}, "issue:bad")
			do("issue_no_agent", svc, "POST", base+"/agents/"+nobody+"/token", map[string]any{}, "issue:nobody")
			a, err = w.agent().post(ctx, ping)
			if err != nil {
				t.Fatal(err)
			}
			s.http("replaced_token", a)
			var res struct {
				Result struct {
					Token string `json:"token"`
				} `json:"result"`
			}
			if err := json.Unmarshal(issued.Body, &res); err != nil || res.Result.Token == "" {
				t.Fatalf("issue_token gave no token: %d %s", issued.Status, issued.Body)
			}
			fresh := newMCPClient(w.base(), res.Result.Token, nil)
			if h, err := fresh.initialize(ctx); err != nil || h.Status != http.StatusOK {
				t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
			}
			callAs(t, fresh, s, "me_issued", "me_get", map[string]any{})
			do("agent_issued", svc, "GET", base+"/agents/"+tutor, nil, "")

			do("revoke", svc, "POST", base+"/agents/"+tutor+"/token/revoke", map[string]any{}, "revoke:1")
			do("revoke_replay", svc, "POST", base+"/agents/"+tutor+"/token/revoke", map[string]any{}, "revoke:1")
			do("revoke_none", svc, "POST", base+"/agents/"+tutor+"/token/revoke", map[string]any{}, "revoke:2")
			do("revoke_no_agent", svc, "POST", base+"/agents/"+nobody+"/token/revoke", map[string]any{}, "revoke:nobody")
			a, err = fresh.post(ctx, ping)
			if err != nil {
				t.Fatal(err)
			}
			s.http("revoked_token", a)
			do("agent_not_hosted", svc, "GET", base+"/agents/"+tutor, nil, "")
			callAs(t, w.as("yuki"), s, "open_not_hosted", "conversation_open", inCourseArgs(w, "respondent_member_id", w.tutorSeat(),
				"body", "Are you there?", "idempotency_key", "open:yuki:not-hosted"))

			w.suspendTutor()
			do("agent_suspended", svc, "GET", base+"/agents/"+tutor, nil, "")
			do("issue_suspended", svc, "POST", base+"/agents/"+tutor+"/token", map[string]any{}, "issue:suspended")
			w.reactivateTutor()
			do("issue_reactivated", svc, "POST", base+"/agents/"+tutor+"/token", map[string]any{}, "issue:2")
			do("check_owner_hosted", svc, "GET", base+"/owners/"+sato+"/agents/"+tutor, nil, "")
		}},
	{name: "rate_limited", about: "429 with Retry-After and details.retry_after_seconds, at Core's default limit (600 a minute, bursts of 100); REST shares the allowance", rateLimited: true,
		run: func(t *testing.T, w world, s *steps) {
			ctx := context.Background()
			for i := range 300 {
				a, err := w.agent().call(ctx, "me_get", map[string]any{})
				if err != nil {
					t.Fatal(err)
				}
				if a.Status == http.StatusTooManyRequests {
					s.http("limited", a.httpAnswer)
					r, err := w.rest().do(ctx, "GET", "/v1/me", nil, "")
					if err != nil {
						t.Fatal(err)
					}
					s.rest("limited_over_rest", "GET", "/v1/me", nil, r)
					return
				}
				if i == 299 {
					t.Skipf("no 429 after %d calls: this Core has no rate limit; start it with CORE_RATE_LIMIT_PER_MINUTE=600", i+1)
				}
			}
		}},
	{name: "rest", about: "the REST transport: statuses, Idempotency-Replayed, and error bodies", run: func(t *testing.T, w world, s *steps) {
		ctx := context.Background()
		r := w.rest()
		do := func(name, method, path string, body any, key string) httpAnswer {
			t.Helper()
			a, err := r.do(ctx, method, path, body, key)
			if err != nil {
				t.Fatal(err)
			}
			s.rest(name, method, path, body, a)
			return a
		}
		c := "/v1/courses/" + w.course()
		conv, m1 := w.ask(0, "Where are the notes?")
		do("inbox", "GET", c+"/conversations/inbox", nil, "")
		do("inbox_limit", "GET", c+"/conversations/inbox?limit=1", nil, "")
		do("messages", "GET", c+"/conversations/"+conv+"/messages?after_seq=0", nil, "")
		path := c + "/conversations/" + conv + "/answer"
		body := map[string]any{"in_reply_to_message_id": m1, "body": "On the course page."}
		key := "answer:" + conv + ":" + m1 + ":1"
		do("answer", "POST", path, body, key)
		do("replay", "POST", path, body, key)
		do("conflict", "POST", path, map[string]any{"in_reply_to_message_id": m1, "body": "Elsewhere."}, key)
		do("already_answered", "POST", path, body, "answer:"+conv+":"+m1+":2")
		do("no_key", "POST", path, body, "")
		do("not_found", "GET", c+"/conversations/"+uuid.NewString(), nil, "")
		do("memberships", "GET", "/v1/me/memberships", nil, "")
		w.setTutorLevel("confirm_required")
		kconv, k1 := w.ask(1, "Can I switch lab groups?")
		kpath := c + "/conversations/" + kconv + "/answer"
		kbody := map[string]any{"in_reply_to_message_id": k1, "body": "Ask the lab coordinator."}
		kkey := "answer:" + kconv + ":" + k1 + ":1"
		a := do("proposed", "POST", kpath, kbody, kkey)
		var prop struct {
			ActionID string `json:"action_id"`
		}
		if err := json.Unmarshal(a.Body, &prop); err != nil || prop.ActionID == "" {
			t.Fatalf("no action_id in %s", a.Body)
		}
		do("answer_pending", "POST", kpath, kbody, "answer:"+kconv+":"+k1+":2")
		w.reject(prop.ActionID, "Point them to the form.")
		do("replayed_rejected", "POST", kpath, kbody, kkey)
		w.setTutorLevel("denied")
		do("denied", "POST", kpath, kbody, "answer:"+kconv+":"+k1+":3")
		anon := &restClient{base: w.base()}
		a, err := anon.do(ctx, "GET", "/v1/me", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		s.rest("unauthenticated", "GET", "/v1/me", nil, a)
	}},
	{name: "inbox_order", about: "the inbox over several conversations: longest waiting first by the opener's last message, limits, and a conversation leaving and coming back", run: func(t *testing.T, w world, s *steps) {
		a, _ := w.ask(0, "A1: when is the first lab?")
		b, b1 := w.ask(1, "B1: is there a reading list?")
		w.ask(0, "C1: a separate question: may I audit the course?")
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "inbox_limit_2", "conversation_inbox", inCourseArgs(w, "limit", 2))
		w.followUp(a, "A2: and the second one?")
		call(t, w, s, "inbox_after_a_follow_up", "conversation_inbox", inCourseArgs(w))
		wantStatus(t, call(t, w, s, "answer_b", "conversation_answer", answer(w, b, b1, "Yes, on the course page.", 1)), "executed")
		call(t, w, s, "inbox_after_an_answer", "conversation_inbox", inCourseArgs(w))
		w.followUp(b, "B2: thanks; and is the exam open book?")
		call(t, w, s, "inbox_after_b2", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "inbox_limit_1", "conversation_inbox", inCourseArgs(w, "limit", 1))
		call(t, w, s, "inbox_limit_0", "conversation_inbox", inCourseArgs(w, "limit", 0))
		call(t, w, s, "inbox_limit_negative", "conversation_inbox", inCourseArgs(w, "limit", -1))
		call(t, w, s, "inbox_limit_over_100", "conversation_inbox", inCourseArgs(w, "limit", 101))
	}},
	{name: "mine_paging", about: "action_list_mine: the seat's own actions oldest first, a page of limit with next, after, exclude_types", run: func(t *testing.T, w world, s *steps) {
		c1, m1 := w.ask(0, "Q1: is the lab at nine?")
		c2, m2 := w.ask(1, "Q2: is the lab at ten?")
		wantStatus(t, call(t, w, s, "answer_1", "conversation_answer", answer(w, c1, m1, "Yes.", 1)), "executed")
		wantStatus(t, call(t, w, s, "answer_2", "conversation_answer", answer(w, c2, m2, "No, at nine.", 1)), "executed")
		wantStatus(t, call(t, w, s, "close_1", "conversation_close", inCourseArgs(w, "conversation_id", c1, "idempotency_key", "close:"+c1)), "executed")
		w.setTutorLevel("denied")
		wantStatus(t, call(t, w, s, "denied_2", "conversation_answer", answer(w, c2, m2, "At nine.", 2)), "denied")
		w.setTutorLevel("autonomous")
		all := call(t, w, s, "all", "action_list_mine", inCourseArgs(w))
		page := call(t, w, s, "page_1", "action_list_mine", inCourseArgs(w, "limit", 2))
		next, _ := resultOf(page, "next").(string)
		if next == "" {
			t.Fatalf("a full page without next: %s", page.Body)
		}
		page = call(t, w, s, "page_2", "action_list_mine", inCourseArgs(w, "limit", 2, "after", next))
		if next, _ = resultOf(page, "next").(string); next == "" {
			t.Fatalf("a full page without next: %s", page.Body)
		}
		call(t, w, s, "page_3", "action_list_mine", inCourseArgs(w, "limit", 2, "after", next))
		actions, _ := resultOf(all, "actions").([]any)
		if len(actions) != 4 {
			t.Fatalf("%d actions, want 4: %s", len(actions), all.Body)
		}
		last, _ := actions[3].(map[string]any)["id"].(string)
		call(t, w, s, "after_the_last", "action_list_mine", inCourseArgs(w, "after", last))
		call(t, w, s, "exclude_answers", "action_list_mine", inCourseArgs(w, "exclude_types", []string{"conversation.answer"}))
		call(t, w, s, "limit_over_200", "action_list_mine", inCourseArgs(w, "limit", 201))
	}},
	{name: "approved_after_moving_on", about: "proposals to a question and to its follow-up: the old one refused when proposed again, failed when approved, the new one executed; replays of both", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Is the essay due on Monday?")
		old := answer(w, conv, m1, "Yes, Monday.", 1)
		p1 := call(t, w, s, "propose_old", "conversation_answer", old)
		wantStatus(t, p1, "proposed")
		m2 := w.followUp(conv, "Sorry: the long essay, not the short one?")
		call(t, w, s, "inbox_after_follow_up", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get_after_follow_up", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "propose_old_again", "conversation_answer", answer(w, conv, m1, "Yes, Monday, I said.", 2))
		newer := answer(w, conv, m2, "The long one is due on Friday.", 1)
		p2 := call(t, w, s, "propose_new", "conversation_answer", newer)
		wantStatus(t, p2, "proposed")
		call(t, w, s, "get_with_new_pending", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "inbox_with_new_pending", "conversation_inbox", inCourseArgs(w))
		w.approve(p1.str("action_id"))
		call(t, w, s, "replay_old", "conversation_answer", old)
		w.approve(p2.str("action_id"))
		call(t, w, s, "replay_new", "conversation_answer", newer)
		call(t, w, s, "replay_old_again", "conversation_answer", old)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
	}},
	{name: "closed_pending", about: "a proposal whose conversation its opener closed: the approval fails closed, and so does its replay", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Can I swap my lab slot?")
		args := answer(w, conv, m1, "Yes, through the form.", 1)
		p := call(t, w, s, "propose", "conversation_answer", args)
		wantStatus(t, p, "proposed")
		w.closeAsOpener(conv, "Sorted it out")
		call(t, w, s, "get_closed", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		w.approve(p.str("action_id"))
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
	}},
	{name: "staff_retract", about: "a message retracted by staff who oversee the opener: the question and the agent's answer; an answer to a retracted question, refused; refusals to others", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is HW1 marked on style?")
		a := call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Partly.", 1))
		wantStatus(t, a, "executed")
		mine := a.str("result", "message_id")
		m2 := w.followUp(conv, "And on comments?")
		mori, ken := w.as("mori"), w.as("ken")
		wantStatus(t, callAs(t, mori, s, "staff_retracts_the_question", "conversation_retract",
			inCourseArgs(w, "message_id", m2, "reason", "Posted in the wrong course", "idempotency_key", "retract:"+m2)), "executed")
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		wantStatus(t, callAs(t, mori, s, "staff_retracts_the_answer", "conversation_retract",
			inCourseArgs(w, "message_id", mine, "reason", "Inaccurate", "idempotency_key", "retract:"+mine)), "executed")
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "answer_the_retracted_question", "conversation_answer", answer(w, conv, m2, "Comments count too.", 1))
		callAs(t, ken, s, "another_student_retracts", "conversation_retract", inCourseArgs(w, "message_id", m1, "idempotency_key", "retract:"+m1))
		callAs(t, ken, s, "another_student_reads", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		callAs(t, mori, s, "staff_reads", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
	}},
	{name: "retracted_pending", about: "the question an answer waits on is retracted: the inbox leaves it out, the view waits for nothing and shows no answer waiting, and approving the answer fails, moved_on", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Can I bring notes to the exam?")
		args := answer(w, conv, m1, "One sheet of notes.", 1)
		p := call(t, w, s, "propose", "conversation_answer", args)
		wantStatus(t, p, "proposed")
		w.retract(m1, "Found it in the syllabus")
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		w.approve(p.str("action_id"))
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
	}},
	{name: "events_visibility", about: "event_list for those who do not take part: a student, a decider, the agents; conversations read by others", run: func(t *testing.T, w world, s *steps) {
		x, x1 := w.ask(0, "X: when are office hours?")
		wantStatus(t, call(t, w, s, "answer_x", "conversation_answer", answer(w, x, x1, "Tuesdays at two.", 1)), "executed")
		y, y1 := w.ask(1, "Y: is the lab compulsory?")
		w.setTutorLevel("confirm_required")
		wantStatus(t, call(t, w, s, "propose_y", "conversation_answer", answer(w, y, y1, "Yes.", 1)), "proposed")
		ken, mori := w.as("ken"), w.as("mori")
		callAs(t, ken, s, "student_events", "event_list", inCourseArgs(w, "since_seq", 0))
		callAs(t, mori, s, "decider_events", "event_list", inCourseArgs(w, "since_seq", 0))
		tutor := call(t, w, s, "tutor_events", "event_list", inCourseArgs(w, "since_seq", 0))
		callAs(t, ken, s, "student_reads_another", "conversation_messages", inCourseArgs(w, "conversation_id", x))
		callAs(t, ken, s, "student_inbox", "conversation_inbox", inCourseArgs(w))
		own := w.ownAgent()
		z, _ := w.askOwn("Z: what did I get on HW1?")
		callAs(t, own, s, "own_agent_events", "event_list", inCourseArgs(w, "since_seq", 0))
		callAs(t, own, s, "own_agent_reads_the_tutors", "conversation_get", inCourseArgs(w, "conversation_id", x))
		call(t, w, s, "tutor_events_after", "event_list", inCourseArgs(w, "since_seq", resultOf(tutor, "next_seq")))
		call(t, w, s, "tutor_reads_the_own_agents", "conversation_get", inCourseArgs(w, "conversation_id", z))
	}},
	{name: "delegate_others", about: "a student's own agent asked by someone other than its principal, and each agent at a conversation not addressed to it", run: func(t *testing.T, w world, s *steps) {
		own, seat := w.ownAgent(), w.ownSeat()
		callAs(t, w.as("ken"), s, "another_student_asks_it", "conversation_open",
			inCourseArgs(w, "respondent_member_id", seat, "body", "Hi, can you help me too?", "idempotency_key", "open:"+seat))
		callAs(t, w.as("mori"), s, "an_instructor_asks_it", "conversation_open",
			inCourseArgs(w, "respondent_member_id", seat, "body", "Hello, how is Yuki doing?", "idempotency_key", "open:"+seat))
		x, x1 := w.ask(0, "X: for the tutor")
		callAs(t, own, s, "own_agent_answers_the_tutors", "conversation_answer", answer(w, x, x1, "I will.", 1))
		callAs(t, own, s, "own_agent_inbox", "conversation_inbox", inCourseArgs(w))
		z, z1 := w.askOwn("Z: for my own agent")
		call(t, w, s, "tutor_answers_the_own_agents", "conversation_answer", answer(w, z, z1, "I will.", 1))
		call(t, w, s, "tutor_inbox", "conversation_inbox", inCourseArgs(w))
		callAs(t, own, s, "own_agent_closes_the_tutors", "conversation_close", inCourseArgs(w, "conversation_id", x, "idempotency_key", "close:"+x))
		callAs(t, own, s, "own_agent_in_reply_to_its_own", "conversation_answer", answer(w, z, x1, "Wrong question.", 1))
	}},
	{name: "tutor_scope", about: "an instructor's tutor listed for one student: another student may not address it, the listed one may; what it may read", run: func(t *testing.T, w world, s *steps) {
		seat, lab := w.listedTutor(0)
		callAs(t, w.as("ken"), s, "outside_its_scope", "conversation_open",
			inCourseArgs(w, "respondent_member_id", seat, "body", "Can you check my lab report?", "idempotency_key", "open:"+seat))
		opened := callAs(t, w.as("yuki"), s, "within_its_scope", "conversation_open",
			inCourseArgs(w, "respondent_member_id", seat, "body", "Can you check my lab report?", "idempotency_key", "open:"+seat))
		wantStatus(t, opened, "executed")
		callAs(t, lab, s, "memberships", "me_memberships", map[string]any{})
		callAs(t, lab, s, "inbox", "conversation_inbox", inCourseArgs(w))
		callShape(t, lab, s, "gradebook_in_scope", "gradebook_get", inCourseArgs(w, "student_member_id", w.studentSeat(0)))
		callAs(t, lab, s, "gradebook_out_of_scope", "gradebook_get", inCourseArgs(w, "student_member_id", w.studentSeat(1)))
		callAs(t, lab, s, "submissions", "submission_list", inCourseArgs(w))
	}},
	{name: "principal_paused", about: "§2.4 denied: the agent's principal paused, every call of the seat denied (principal_not_active); its opener may no longer address it", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is the library open on Sunday?")
		w.pausePrincipal()
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes.", 1))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
		call(t, w, s, "me", "me_get", map[string]any{})
		callAs(t, w.as("yuki"), s, "opener_asks_again", "conversation_ask",
			inCourseArgs(w, "conversation_id", conv, "body", "Hello?", "idempotency_key", "ask:"+conv))
	}},
	{name: "uppercase_ids", about: "ids sent in upper case: a proposal is kept as Core pins it, so answer_pending and the inbox still see it; a key reused in lower case is another call", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Is there a seminar this week?")
		args := answer(w, conv, m1, "Yes, on Thursday.", 1)
		args["conversation_id"], args["in_reply_to_message_id"] = strings.ToUpper(conv), strings.ToUpper(m1)
		p := call(t, w, s, "propose", "conversation_answer", args)
		wantStatus(t, p, "proposed")
		call(t, w, s, "same_key_lower_case", "conversation_answer", answer(w, conv, m1, "Yes, on Thursday.", 1))
		call(t, w, s, "second", "conversation_answer", answer(w, conv, m1, "Yes, Thursday.", 2))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "get", "conversation_get", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
		w.approve(p.str("action_id"))
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", strings.ToUpper(conv)))
	}},
	{name: "decide_own_party", about: "an agent's proposal is decided by its owner where they could make it themselves, never by the agent: the agent is refused, at once too where a person confirms its decisions, its owner approves at once, and it is decided for anyone after", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "May I submit HW1 late?")
		args := answer(w, conv, m1, "Ask your instructor.", 1)
		p := call(t, w, s, "propose", "conversation_answer", args)
		wantStatus(t, p, "proposed")
		id := p.str("action_id")
		decide := func(decision string, key string, more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"action_id", id, "decision", decision, "idempotency_key", key}, more...)...)
		}
		sato, mori := w.as("sato"), w.as("mori")
		call(t, w, s, "the_agent_approves", "action_decide", decide("approve", "decide:"+id))
		// Where a person confirms its decisions, the agent's own decision is
		// refused at once, recorded, and not proposed (AIShie-Core #60).
		w.setLevel(w.tutorSeat(), permActionDecide, "confirm_required")
		call(t, w, s, "the_agent_approves_at_confirm_required", "action_decide", decide("approve", "decide:"+id+":agent"))
		w.setLevel(w.tutorSeat(), permActionDecide, "denied")
		callAs(t, mori, s, "not_a_decision", "action_decide", decide("maybe", "decide:"+id+":0"))
		wantStatus(t, callAs(t, sato, s, "its_owner_approves", "action_decide", decide("approve", "decide:"+id)), "executed")
		callAs(t, mori, s, "decided_again", "action_decide", decide("reject", "decide:"+id+":2", "reason", "Say when the deadline is."))
		callAs(t, mori, s, "no_such_action", "action_decide", inCourseArgs(w, "action_id", uuid.NewString(), "decision", "approve",
			"idempotency_key", "decide:"+id+":3"))
		call(t, w, s, "replay", "conversation_answer", args)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
	}},
	{name: "owner_would_be_refused", about: "an agent's proposal that approving now would refuse is not its owner's to decide (AIShie-Core #60): the opener asks again while its answer waits, its owner is refused owner_would_be_refused with the refusal inside, and someone else may still reject it", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "When is the midterm?")
		p := call(t, w, s, "propose", "conversation_answer", answer(w, conv, m1, "In week 8.", 1))
		wantStatus(t, p, "proposed")
		id := p.str("action_id")
		w.followUp(conv, "And where is it held?")
		decide := func(decision, key string, more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"action_id", id, "decision", decision, "idempotency_key", key}, more...)...)
		}
		callAs(t, w.as("sato"), s, "its_owner_approves", "action_decide", decide("approve", "decide:"+id))
		callAs(t, w.as("sato"), s, "its_owner_rejects", "action_decide", decide("reject", "decide:"+id+":1", "reason", "Answer both."))
		wantStatus(t, callAs(t, w.as("mori"), s, "someone_else_rejects", "action_decide",
			decide("reject", "decide:"+id+":2", "reason", "Answer both questions.")), "executed")
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}},
	{name: "reviewed", about: "answers at pending_review reviewed after: escalated, reviewed by the agent's owner, the refusals, and what the agent sees of it", run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("pending_review")
		c1, m1 := w.ask(0, "Is the quiz on Friday?")
		first := answer(w, c1, m1, "Yes, at noon.", 1)
		a1 := call(t, w, s, "answer_1", "conversation_answer", first)
		wantStatus(t, a1, "executed")
		id1 := a1.str("action_id")
		review := func(id, outcome, key string) map[string]any {
			return inCourseArgs(w, "action_id", id, "outcome", outcome, "idempotency_key", key)
		}
		sato, mori := w.as("sato"), w.as("mori")
		call(t, w, s, "the_agent_reviews", "action_review", review(id1, "reviewed", "review:"+id1))
		callAs(t, mori, s, "not_an_outcome", "action_review", review(id1, "fine", "review:"+id1+":0"))
		wantStatus(t, callAs(t, mori, s, "escalated", "action_review", review(id1, "escalated", "review:"+id1)), "executed")
		callAs(t, mori, s, "escalated_again", "action_review", review(id1, "escalated", "review:"+id1+":2"))
		callAs(t, mori, s, "closed_by_who_escalated", "action_review", review(id1, "reviewed", "review:"+id1+":3"))
		call(t, w, s, "replay_1", "conversation_answer", first)
		c2, m2 := w.ask(1, "Is the quiz open book?")
		second := answer(w, c2, m2, "No.", 1)
		a2 := call(t, w, s, "answer_2", "conversation_answer", second)
		wantStatus(t, a2, "executed")
		id2 := a2.str("action_id")
		wantStatus(t, callAs(t, sato, s, "its_owner_reviews", "action_review", review(id2, "reviewed", "review:"+id2)), "executed")
		callAs(t, mori, s, "reviewed_again", "action_review", review(id2, "reviewed", "review:"+id2+":2"))
		call(t, w, s, "replay_2", "conversation_answer", second)
		call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}},
	{name: "malformed", about: "arguments refused before anything is attempted: a key given twice, a number out of range, U+0000, not an object", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is the lab report due today?")
		raw := func(format string, args ...any) json.RawMessage { return json.RawMessage(fmt.Sprintf(format, args...)) }
		rawCall := func(name, tool string, args json.RawMessage) {
			t.Helper()
			a, err := w.agent().call(context.Background(), tool, args)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			s.tool(name, tool, args, a)
		}
		rawCall("write_key_twice", "conversation_answer", raw(`{"course_id":%q,"conversation_id":%q,"in_reply_to_message_id":%q,"body":"Yes.","body":"No.","idempotency_key":"answer:%s:%s:1"}`,
			w.course(), conv, m1, conv, m1))
		rawCall("read_key_twice", "conversation_inbox", raw(`{"course_id":%q,"course_id":%q}`, w.course(), w.course()))
		rawCall("number_out_of_range", "event_list", raw(`{"course_id":%q,"since_seq":1e500}`, w.course()))
		rawCall("write_number_out_of_range", "conversation_close", raw(`{"course_id":%q,"conversation_id":%q,"reason":null,"x":1e-500,"idempotency_key":"close:%s"}`,
			w.course(), conv, conv))
		rawCall("nul", "conversation_answer", raw(`{"course_id":%q,"conversation_id":%q,"in_reply_to_message_id":%q,"body":"Yes.\u0000","idempotency_key":"answer:%s:%s:2"}`,
			w.course(), conv, m1, conv, m1))
		rawCall("write_not_an_object", "conversation_answer", raw(`[1]`))
		rawCall("read_not_an_object", "conversation_inbox", raw(`[1]`))
		rawCall("read_null", "me_get", raw(`null`))
		rawCall("integer_as_fraction", "event_list", raw(`{"course_id":%q,"since_seq":1.5}`, w.course()))
		rawCall("integer_written_as_a_float", "event_list", raw(`{"course_id":%q,"since_seq":1.0,"limit":1e1}`, w.course()))
	}},
	modelWrites,
	memberWrites,
	rosterReads,
	waitForNews,
	waitCap,
	drafts,
	withdrawn,
	attachments,
	renditions,
	transcriptionChecks,
	sources,
	escalations,
	gradeSince,
	assignmentDeletion,
	groupWork,
}

// escalations are reviews of the tutor's answers proposed by an agent
// nobody owns, whose decisions a person confirms: an escalation is for
// someone else to look at than whoever had a hand in it, from any seat
// (Core's EscalatedBy): Mori, who approved the agent's escalation, may not
// close it, nor may the agent; and Mori, who escalated another himself,
// may not approve the agent's review that would close it, though he may
// reject it (Core's closesOwnEscalation).
var escalations = scenario{name: "escalations", about: "reviews of the tutor's answers proposed by an agent nobody owns: Mori approves its escalation, " +
	"and neither he nor the agent may close it, while the tutor's owner does; Mori escalates another himself, and may not approve the agent's " +
	"review that would close it, nor may the tutor's owner, while Mori may reject it; and what the tutor sees of both",
	run: func(t *testing.T, w world, s *steps) {
		w.setTutorLevel("pending_review")
		_, reg := w.registrar(map[string]string{permActionDecide: "confirm_required"})
		sato, mori := w.as("sato"), w.as("mori")
		answered := func(name string, student int, question, body string) string {
			t.Helper()
			conv, m := w.ask(student, question)
			a := call(t, w, s, name, "conversation_answer", answer(w, conv, m, body, 1))
			wantStatus(t, a, "executed")
			return a.str("action_id")
		}
		review := func(id, outcome, key string) map[string]any {
			return inCourseArgs(w, "action_id", id, "outcome", outcome, "idempotency_key", key)
		}
		decide := func(id, decision, key string, more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"action_id", id, "decision", decision, "idempotency_key", key}, more...)...)
		}

		id1 := answered("answer_1", 0, "Is the quiz on Friday?", "Yes, at noon.")
		p1 := callAs(t, reg, s, "escalation_proposed", "action_review", review(id1, "escalated", "review:"+id1))
		wantStatus(t, p1, "proposed")
		wantStatus(t, callAs(t, mori, s, "escalation_approved", "action_decide", decide(p1.str("action_id"), "approve", "decide:"+id1)), "executed")
		callAs(t, mori, s, "closed_by_who_approved_it", "action_review", review(id1, "reviewed", "review:"+id1+":mori"))
		callAs(t, reg, s, "closed_by_who_proposed_it", "action_review", review(id1, "reviewed", "review:"+id1+":2"))
		wantStatus(t, callAs(t, sato, s, "closed_by_its_owner", "action_review", review(id1, "reviewed", "review:"+id1+":sato")), "executed")

		id2 := answered("answer_2", 1, "Is the quiz open book?", "No.")
		wantStatus(t, callAs(t, mori, s, "escalated", "action_review", review(id2, "escalated", "review:"+id2+":mori")), "executed")
		p2 := callAs(t, reg, s, "closing_proposed", "action_review", review(id2, "reviewed", "review:"+id2))
		wantStatus(t, p2, "proposed")
		closing := p2.str("action_id")
		callAs(t, mori, s, "closing_approved_by_who_escalated", "action_decide", decide(closing, "approve", "decide:"+id2))
		callAs(t, sato, s, "closing_approved_by_the_answers_owner", "action_decide", decide(closing, "approve", "decide:"+id2+":sato"))
		wantStatus(t, callAs(t, mori, s, "closing_rejected_by_who_escalated", "action_decide",
			decide(closing, "reject", "decide:"+id2+":2", "reason", "Leave it to Sato.")), "executed")
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
	}}

// gradeSince is grade_submit proposed by an agent of Sato's, and a newer
// draft of the same work entered while it waits, which approving it must
// not pass over (Core's Since): its owner is refused deciding it, with the
// refusal inside, and Mori's approval fails; a proposal made after that
// draft replaces it. And grade_submit's own refusals, of what its
// arguments say and of what the work says.
var gradeSince = scenario{name: "grade_since", about: "grade_submit proposed by a tutor of Sato's listed for Yuki, then a newer draft " +
	"of her work entered by Sato: approving the proposal refused for it, as its owner (owner_would_be_refused, the refusal inside) and as " +
	"Mori (failed), and a proposal made after the draft approved by its owner, replacing it; refused arguments, and grades the work refuses",
	run: func(t *testing.T, w world, s *steps) {
		sub := w.submit(0)
		seat, lab := w.listedTutor(0)
		w.setLevel(seat, permGradeSubmit, "confirm_required")
		sato, mori := w.as("sato"), w.as("mori")
		grade := func(key string, score any, more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"submission_id", sub, "score", score, "idempotency_key", key}, more...)...)
		}
		decide := func(id, decision, key string) map[string]any {
			return inCourseArgs(w, "action_id", id, "decision", decision, "idempotency_key", key)
		}

		p1 := callAs(t, lab, s, "proposed", "grade_submit", grade("tool:x:g:1", "80", "feedback", "Show your working."))
		wantStatus(t, p1, "proposed")
		first := p1.str("action_id")
		wantStatus(t, callAs(t, sato, s, "newer_draft", "grade_submit", grade("grade:"+sub+":1", 85)), "executed")
		callAs(t, sato, s, "its_owner_approves", "action_decide", decide(first, "approve", "decide:"+first))
		wantStatus(t, callAs(t, mori, s, "approved_after_a_newer_draft", "action_decide", decide(first, "approve", "decide:"+first+":1")), "executed")
		p2 := callAs(t, lab, s, "proposed_after_the_draft", "grade_submit", grade("tool:x:g:2", 82.50))
		wantStatus(t, p2, "proposed")
		second := p2.str("action_id")
		wantStatus(t, callAs(t, sato, s, "its_owner_approves_after", "action_decide", decide(second, "approve", "decide:"+second)), "executed")

		callAs(t, sato, s, "negative", "grade_submit", grade("grade:"+sub+":2", "-1"))
		callAs(t, sato, s, "no_subject", "grade_submit", inCourseArgs(w, "score", 1, "idempotency_key", "grade:"+sub+":3"))
		callAs(t, sato, s, "both_rubric_and_none", "grade_submit", grade("grade:"+sub+":4", 1, "no_rubric", true,
			"rubric_version_id", "0192f3c1-0000-7000-8000-00000000abcd"))
		callAs(t, sato, s, "for_missing_on_a_component", "grade_submit", inCourseArgs(w, "component_id", "0192f3c1-0000-7000-8000-00000000abcd",
			"student_member_id", w.studentSeat(0), "for_missing", false, "score", 1, "idempotency_key", "grade:"+sub+":5"))
		callAs(t, sato, s, "no_such_submission", "grade_submit", inCourseArgs(w, "submission_id", "0192f3c1-0000-7000-8000-00000000abcd",
			"score", 1, "idempotency_key", "grade:"+sub+":6"))
		callAs(t, sato, s, "above_the_points", "grade_submit", grade("grade:"+sub+":7", 120))
		callAs(t, sato, s, "out_of_another", "grade_submit", grade("grade:"+sub+":8", "45", "out_of", 50))
		callAs(t, sato, s, "for_nothing_handed_in", "grade_submit", grade("grade:"+sub+":9", 1, "for_missing", true))
		callAs(t, sato, s, "rubric_of_none", "grade_submit", grade("grade:"+sub+":10", 1, "rubric_version_id", "0192f3c1-0000-7000-8000-00000000abcd"))
		wantStatus(t, callAs(t, sato, s, "extra", "grade_submit", grade("grade:"+sub+":11", "100.5", "allow_extra", true, "out_of", "100.00")), "executed")
	}}

// transcriptionChecks is what a transcription's completion says alone
// refused as it is read (AIShie-Core #60, its Check): status error, no
// action_id, before any version, lease or file is looked up; and one that
// says nothing amiss, for a version that is not there, looked up.
var transcriptionChecks = scenario{name: "transcription_checks", about: "document_text.complete refused on what it says alone, before " +
	"anything is looked up or recorded: done without a body, with an empty one, without pages, with a reason or without a model; failed " +
	"or skipped with a body, pages or a model, or without a reason; and a completion of nothing amiss for a version that is not there",
	run: func(t *testing.T, w world, s *steps) {
		ctx := context.Background()
		svc := w.textService()
		version, lease, file := uuid.NewString(), uuid.NewString(), uuid.NewString()
		path := "/v1/services/document_text/versions/" + version + "/complete"
		n := 0
		complete := func(name string, more ...any) {
			t.Helper()
			body := map[string]any{"lease_id": lease, "file_id": file}
			for i := 0; i+1 < len(more); i += 2 {
				body[more[i].(string)] = more[i+1]
			}
			n++
			a, err := svc.do(ctx, http.MethodPost, path, body, fmt.Sprintf("text:%s:%d", version, n))
			if err != nil {
				t.Fatal(err)
			}
			s.rest(name, http.MethodPost, path, body, a)
		}
		complete("done_without_body", "status", "done", "pages", 1, "model", "m")
		complete("done_empty_body", "status", "done", "body", " \n", "pages", 1, "model", "m")
		complete("done_without_pages", "status", "done", "body", "## 第 1 頁\n\nText.", "model", "m")
		complete("done_with_reason", "status", "done", "body", "## 第 1 頁\n\nText.", "pages", 1, "model", "m", "reason", "timeout")
		complete("done_without_model", "status", "done", "body", "## 第 1 頁\n\nText.", "pages", 1)
		complete("failed_with_body", "status", "failed", "reason", "timeout", "body", "x")
		complete("skipped_with_pages", "status", "skipped", "reason", "too_many_pages", "pages", 1)
		complete("failed_without_reason", "status", "failed")
		complete("done_for_no_version", "status", "done", "body", "## 第 1 頁\n\nText.", "pages", 1, "model", "m")
	}}

// modelWrites is a write a model makes through its seat's perms (the
// runtime's docs/design.md §4), document_create under the runtime's keys.
var modelWrites = scenario{name: "model_writes", about: "document_create by an instructor's own agent, as its model makes one: proposed at confirm_required, replayed, a conflict under its key, executed at autonomous, failed and refused arguments, denied, and the draft its principal reads and a student does not",
	run: func(t *testing.T, w world, s *steps) {
		seat, c := w.ownerAgent(map[string]string{"document_write": "confirm_required"})
		create := func(key, title string, more ...any) map[string]any {
			args := inCourseArgs(w, "kind", "material", "title", title, "body_md", "# "+title, "idempotency_key", key)
			for i := 0; i+1 < len(more); i += 2 {
				args[more[i].(string)] = more[i+1]
			}
			return args
		}
		wantStatus(t, callAs(t, c, s, "proposed", "document_create", create("tool:x:m:1:1", "Week 1 notes")), "proposed")
		callAs(t, c, s, "replayed", "document_create", create("tool:x:m:1:1", "Week 1 notes"))
		callAs(t, c, s, "conflict", "document_create", create("tool:x:m:1:1", "Week 2 notes"))
		w.setLevel(seat, "document_write", "autonomous")
		made := callAs(t, c, s, "executed", "document_create", create("tool:x:m:2:1", "Week 3 notes", "sort_order", 3))
		wantStatus(t, made, "executed")
		callAs(t, c, s, "no_text", "document_create", inCourseArgs(w, "kind", "rubric", "title", "Rubric", "idempotency_key", "tool:x:m:2:2"))
		callAs(t, c, s, "untitled", "document_create", create("tool:x:m:2:3", " "))
		callAs(t, c, s, "unknown_kind", "document_create", create("tool:x:m:2:4", "Exam", "kind", "exam"))
		callAs(t, c, s, "submission_without_its_draft", "document_create", create("tool:x:m:2:5", "Mine", "kind", "submission"))
		id, _ := resultOf(made, "document_id").(string)
		callShape(t, w.as("sato"), s, "draft_to_its_principal", "document_get", inCourseArgs(w, "document_id", id))
		callAs(t, w.as("yuki"), s, "draft_to_a_student", "document_get", inCourseArgs(w, "document_id", id))
		w.setLevel(seat, "document_write", "denied")
		callAs(t, c, s, "denied", "document_create", create("tool:x:m:3:1", "Week 4 notes"))
	}}

func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// memberWrites is the roster and seating someone, as a model makes them
// through a seat that manages the course's members (the runtime's
// docs/design.md §4): an agent nobody owns, which Sato seated with
// member_manage.
var memberWrites = scenario{name: "member_writes", about: "member_lookup_actor, member_add, member_get and member_list by an agent nobody owns that manages the course's members: whom an id names, a seat proposed at confirm_required and approved, and the seats the course refuses failing there at once in Core's order (a seat already there, a listed student who is none, an expiry past), executed at autonomous and replayed, refused for a seat already there, for more than the agent holds, for an agent someone owns and for arguments Core refuses; the seats as Core shows them, and the roster denied to a student",
	run: func(t *testing.T, w world, s *steps) {
		// A ta's seat, with the submission_write a student's has, which it
		// could not give otherwise.
		seat, c := w.registrar(map[string]string{"member_manage": "confirm_required", "submission_write": "autonomous"})
		aoi, ren := w.newcomer("Aoi"), w.newcomer("Ren")
		lookup := func(name string, more ...any) toolAnswer {
			return callAs(t, c, s, name, "member_lookup_actor", inCourseArgs(w, more...))
		}
		wantStatus(t, lookup("lookup_newcomer", "actor_id", aoi), "executed")
		lookup("lookup_seated", "actor_id", w.actorOf("yuki"))
		lookup("lookup_owned_agent", "actor_id", w.actorOf("tutor"))
		lookup("lookup_neither")
		lookup("lookup_both", "actor_id", aoi, "email", "aoi@example.invalid")
		lookup("lookup_unknown_email", "email", " nobody@example.invalid ")
		lookup("lookup_unknown_id", "actor_id", "0192f3c1-0000-7000-8000-00000000abcd")
		add := func(name, key string, more ...any) toolAnswer {
			return callAs(t, c, s, name, "member_add", inCourseArgs(w, append([]any{"idempotency_key", key}, more...)...))
		}
		proposed := add("add_proposed", "tool:x:m:1:1", "actor_id", aoi, "preset", "student")
		wantStatus(t, proposed, "proposed")
		// What the course says refuses a seat at once, never proposed
		// (AIShie-Core #60), in Core's order: the lists before a seat
		// already there, and a seat already there before an expiry past.
		add("add_seated_proposed", "tool:x:m:1:2", "actor_id", w.actorOf("yuki"), "preset", "observer")
		add("add_seated_listed_nobody_there", "tool:x:m:1:3", "actor_id", w.actorOf("yuki"), "preset", "observer",
			"student_scope", "listed", "listed_students", []string{w.tutorSeat()})
		add("add_seated_expired", "tool:x:m:1:4", "actor_id", w.actorOf("yuki"), "preset", "observer", "expires_at", "2020-01-01T00:00:00Z")
		add("add_expired_listed_nobody_there", "tool:x:m:1:5", "actor_id", ren, "preset", "observer",
			"student_scope", "listed", "listed_students", []string{w.tutorSeat()}, "expires_at", "2020-01-01T00:00:00Z")
		w.setLevel(seat, "member_manage", "autonomous")
		made := add("add_executed", "tool:x:m:2:1", "actor_id", ren, "preset", "student")
		wantStatus(t, made, "executed")
		add("add_replayed", "tool:x:m:2:1", "actor_id", ren, "preset", "student")
		add("add_seated", "tool:x:m:2:2", "actor_id", ren, "preset", "observer")
		add("add_more_than_it_holds", "tool:x:m:2:3", "actor_id", aoi, "preset", "instructor")
		add("add_a_level_more", "tool:x:m:2:4", "actor_id", aoi, "preset", "student", "perms", map[string]any{"grade_post": "autonomous"})
		add("add_owned_agent", "tool:x:m:2:5", "actor_id", w.actorOf("tutor"), "preset", "observer")
		add("add_unknown_preset", "tool:x:m:2:6", "actor_id", aoi, "preset", "wizard")
		add("add_no_preset", "tool:x:m:2:7", "actor_id", aoi)
		add("add_bad_role", "tool:x:m:2:8", "actor_id", aoi, "preset", "student", "role", "dean")
		add("add_bad_perm", "tool:x:m:2:9", "actor_id", aoi, "preset", "student", "perms", map[string]any{"grade_all": "autonomous", "zzz": "x"})
		add("add_bad_level", "tool:x:m:2:10", "actor_id", aoi, "preset", "student", "perms", map[string]any{"grade_read": "sometimes"})
		add("add_unknown_actor", "tool:x:m:2:11", "actor_id", "0192f3c1-0000-7000-8000-00000000abcd", "preset", "student")
		add("add_listed_nobody_there", "tool:x:m:2:12", "actor_id", aoi, "preset", "observer", "student_scope", "listed",
			"listed_students", []string{w.tutorSeat()})
		add("add_listed_for_all", "tool:x:m:2:13", "actor_id", aoi, "preset", "observer", "listed_students", []string{w.studentSeat(0)})
		id, _ := resultOf(made, "member_id").(string)
		get := func(name, member string) toolAnswer {
			return callAs(t, c, s, name, "member_get", inCourseArgs(w, "member_id", member))
		}
		get("get_added", id)
		get("get_itself", seat)
		get("get_tutor", w.tutorSeat())
		get("get_missing", "0192f3c1-0000-7000-8000-00000000abcd")
		callAs(t, c, s, "list_students", "member_list", inCourseArgs(w, "role", "student"))
		callAs(t, c, s, "list_page", "member_list", inCourseArgs(w, "limit", 2))
		callAs(t, w.as("yuki"), s, "list_as_a_student", "member_list", inCourseArgs(w))
		action, _ := proposed.Structured["action_id"].(string)
		w.approve(action)
		lookup("lookup_after_approval", "actor_id", aoi)
	}}

// rosterReads are where students stand on an assignment and what versions
// a document has, as the seats a model reads through see them (the
// runtime's docs/design.md §4): an instructor sees every student by name,
// those who have not started among them; a student, a student's own agent
// and a tutor listed for one student see that one alone, and no names;
// the course tutor, who reads nobody's work and no drafts, is denied both.
var rosterReads = scenario{name: "roster_reads", about: "submission_roster by an instructor (names, pages), a student, a student's own agent, a listed tutor and the course tutor (denied), for an assignment that is not there too; document_versions by an instructor, a student's own agent and the course tutor (both denied), for a document that is not there too",
	run: func(t *testing.T, w world, s *steps) {
		hw := w.assignment()
		w.submit(0)
		sato := w.as("sato")
		roster := func(more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"assignment_id", hw}, more...)...)
		}
		wantStatus(t, callAs(t, sato, s, "roster_as_an_instructor", "submission_roster", roster()), "executed")
		callAs(t, sato, s, "roster_a_page", "submission_roster", roster("limit", 1))
		callAs(t, w.as("ken"), s, "roster_as_a_student", "submission_roster", roster())
		callAs(t, w.ownAgent(), s, "roster_as_a_students_agent", "submission_roster", roster())
		_, lab := w.listedTutor(0)
		callAs(t, lab, s, "roster_as_a_listed_tutor", "submission_roster", roster())
		call(t, w, s, "roster_as_the_course_tutor", "submission_roster", roster())
		callAs(t, sato, s, "roster_of_no_assignment", "submission_roster", inCourseArgs(w, "assignment_id", "0192f3c1-0000-7000-8000-00000000abcd"))
		doc := w.material()
		versions := func(id string) map[string]any { return inCourseArgs(w, "document_id", id) }
		wantStatus(t, callAs(t, sato, s, "versions_as_an_instructor", "document_versions", versions(doc)), "executed")
		callAs(t, w.ownAgent(), s, "versions_as_a_students_agent", "document_versions", versions(doc))
		call(t, w, s, "versions_as_the_course_tutor", "document_versions", versions(doc))
		callAs(t, sato, s, "versions_of_no_document", "document_versions", versions("0192f3c1-0000-7000-8000-00000000abcd"))
	}}
