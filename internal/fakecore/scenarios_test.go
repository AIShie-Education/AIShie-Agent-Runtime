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
	// tutorToken is the tutor's live token.
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
	// expire waits for, or makes, the proposal's expiry; false when this
	// world cannot.
	expire(actionID string) bool
	revokeTutorToken()

	// Yuki's own agent, seated as her delegate (preset delegate): its MCP
	// client, and Yuki asking it.
	ownAgent() *mcpClient
	askOwn(body string) (conversationID, messageID string)
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
	a, err := w.agent().call(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %s: %v", name, tool, err)
	}
	s.tool(name, tool, args, a)
	return a
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
	{name: "denied_level", about: "§2.4 denied: conversation_answer lowered to denied", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is there a lecture on Monday?")
		w.setTutorLevel("denied")
		call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Yes.", 1))
		call(t, w, s, "inbox", "conversation_inbox", inCourseArgs(w))
		call(t, w, s, "memberships", "me_memberships", map[string]any{})
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w))
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
		next, _ := page.Structured["next_seq"].(json.Number)
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
	{name: "retract_close", about: "conversation_retract by the author, conversation_close and their refusals", run: func(t *testing.T, w world, s *steps) {
		conv, m1 := w.ask(0, "Is the lab open late?")
		a := call(t, w, s, "answer", "conversation_answer", answer(w, conv, m1, "Until nine.", 1))
		wantStatus(t, a, "executed")
		mine := a.str("result", "message_id")
		call(t, w, s, "retract_theirs", "conversation_retract", inCourseArgs(w, "message_id", m1, "idempotency_key", "retract:"+m1))
		call(t, w, s, "retract_mine", "conversation_retract", inCourseArgs(w, "message_id", mine, "reason", "Wrong hours", "idempotency_key", "retract:"+mine))
		call(t, w, s, "retract_again", "conversation_retract", inCourseArgs(w, "message_id", mine, "idempotency_key", "retract:"+mine+":2"))
		call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		call(t, w, s, "close_reserved_reason", "conversation_close", inCourseArgs(w, "conversation_id", conv, "reason", "seat_removed", "idempotency_key", "close:"+conv+":0"))
		call(t, w, s, "close", "conversation_close", inCourseArgs(w, "conversation_id", conv, "reason", "Answered as far as I can.", "idempotency_key", "close:"+conv))
		call(t, w, s, "close_again", "conversation_close", inCourseArgs(w, "conversation_id", conv, "idempotency_key", "close:"+conv+":2"))
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
	{name: "rate_limited", about: "429 with Retry-After and details.retry_after_seconds, at Core's default limit (600 a minute, bursts of 100)", rateLimited: true,
		run: func(t *testing.T, w world, s *steps) {
			ctx := context.Background()
			for i := range 300 {
				a, err := w.agent().call(ctx, "me_get", map[string]any{})
				if err != nil {
					t.Fatal(err)
				}
				if a.Status == http.StatusTooManyRequests {
					s.http("limited", a.httpAnswer)
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
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
