package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

const (
	// answerWait bounds the wait for anything the runtime does in Core: an
	// answer, a proposal, a seat stopped. On a slow runner under -race it
	// takes seconds; the deadline is for a runtime that never does it.
	answerWait = 60 * time.Second
	// latencyTarget is the handout's p95 from a question to its answer
	// while the agent is idle (§7.2: 13 s to notice), and latencyTargetCI
	// what a shared CI runner under -race is allowed.
	latencyTarget   = 13 * time.Second
	latencyTargetCI = 30 * time.Second
)

// answerKey is the idempotency key of an answer to msg in conv (§2.2),
// written out here rather than taken from the runtime, which it checks.
func answerKey(conv, msg string, attempt int) string {
	return fmt.Sprintf("answer:%s:%s:%d", conv, msg, attempt)
}

// attemptState says where an attempt stands, for a failure message: its
// state, error and reason, and the action and message it made; never its
// bytes.
func attemptState(at *store.Attempt) string {
	if at == nil {
		return "missing"
	}
	return fmt.Sprintf("%s (error %q, reason %q, action %q, message %q)", at.State, at.ErrorCode, at.Reason, at.ActionID, at.PostedMessageID)
}

// question is the newest user turn of a model request: the question the
// runtime asks the model to answer. Messages the opener wrote one after
// another come as one turn, the newest last.
func question(req fakellm.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Text()
		}
	}
	return ""
}

// system is a model request's system prompt.
func system(req fakellm.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			return m.Text()
		}
	}
	return ""
}

// waitAnswer waits until p reads a message the member author wrote in
// conv, and returns the first.
func (w *world) waitAnswer(t *testing.T, p person, conv, author string) message {
	t.Helper()
	var got message
	eventually(t, answerWait, p.name+" reading an answer", func() bool {
		as := w.answers(t, p, conv, author)
		if len(as) == 0 {
			return false
		}
		got = as[0]
		return true
	})
	return got
}

// waitPending waits until conv, as p sees it, holds an answer waiting for
// approval other than not, and returns that proposal's action.
func (w *world) waitPending(t *testing.T, p person, conv, not string) string {
	t.Helper()
	var id string
	eventually(t, answerWait, "an answer to "+p.name+" waiting for approval", func() bool {
		c := w.conversation(t, p, conv)
		id = c.pending()
		return c.State == "reply_pending_approval" && id != "" && id != not
	})
	return id
}

// settle waits until the agent's seat has polled its inbox n more times:
// long enough that anything more it meant to post about what it found
// there, it would have.
func (rt *instance) settle(agent string, n int) {
	rt.t.Helper()
	from := rt.inboxPolls(agent)
	eventually(rt.t, answerWait, fmt.Sprintf("%d more inbox polls of %s", n, agent), func() bool {
		return rt.inboxPolls(agent) >= from+float64(n)
	})
}

// ownAgentAnswers is M1's "done when": a student's own agent answers her,
// end to end. Yuki asks her agent why she lost marks on HW3; the model
// looks at the course's assignments (assignment_list, through Core) and
// answers; Yuki reads the answer in Core within the handout's latency
// target; and it was posted under answer:{x}:{m1}:1. Core does not show an
// action's key, so the key is shown by a replay: the agent sending the same
// body under it again gets the same action back, replayed.
func ownAgentAnswers(t *testing.T, w *world) {
	m := newModel(t, fakellm.DefaultResponder)
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own}}})
	rt.waitPolling("yuki-helper")

	const q = "Why did I lose marks on HW3?"
	asked := time.Now()
	conv, m1 := w.ask(t, w.yuki, w.own.member, q)
	answer := w.waitAnswer(t, w.yuki, conv, w.own.member)
	took := time.Since(asked)
	limit := latencyTarget
	if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
		limit = latencyTargetCI
	}
	t.Logf("Yuki read her agent's answer %s after asking", took.Round(10*time.Millisecond))
	if took > limit {
		t.Errorf("Yuki read the answer %s after asking; the target here is %s", took.Round(10*time.Millisecond), limit)
	}
	if want := "Answer: " + q + " (from 1 tool result)"; answer.text() != want || answer.replyTo() != m1 {
		t.Errorf("the answer is %q in reply to %s; want %q in reply to the question %s", answer.text(), answer.replyTo(), want, m1)
	}

	// The model called assignment_list, the runtime called Core, and the
	// model had this course's HW3 back.
	sawResult := false
	for _, req := range m.Requests() {
		for _, msg := range req.Messages {
			if msg.Role == "tool" && strings.Contains(msg.Text(), w.hw3) {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Error("no model request holds assignment_list's result with this course's HW3")
	}
	if n := rt.metric("core_calls_total", map[string]string{"tool": "assignment_list", "status": "executed"}); n < 1 {
		t.Errorf("assignment_list was called %v times as executed; want at least 1", n)
	}

	// The action log, as the agent reads it, holds the answer; the same
	// body under answer:{x}:{m1}:1 replays that action.
	var posted string
	for _, a := range w.actionsMine(t, w.own) {
		var res struct {
			MessageID string `json:"message_id"`
		}
		if a.ActionType == "conversation.answer" && a.Status == "executed" && json.Unmarshal(a.Result, &res) == nil && res.MessageID == answer.ID {
			posted = a.ID
		}
	}
	if posted == "" {
		t.Fatal("the agent's action log holds no executed conversation.answer that made the answer")
	}
	r := w.answerAs(t, w.own, conv, m1, answer.text(), answerKey(conv, m1, 1))
	res := decodeMessage(t, r)
	if r.HTTP != http.StatusOK || r.Status != "executed" || !r.Replayed || r.ActionID != posted || res != answer.ID {
		t.Errorf("the answer sent again under answer:{x}:{m1}:1: %s; want the action %s replayed, executed, making %s", r, posted, answer.ID)
	}
	if len(w.answers(t, w.yuki, conv, w.own.member)) != 1 {
		t.Error("the replay posted a second answer")
	}
}

// decodeMessage is the message an answer's reply names, "" when none.
func decodeMessage(t *testing.T, r reply) string {
	t.Helper()
	var res struct {
		MessageID string `json:"message_id"`
	}
	if len(r.Result) > 0 && string(r.Result) != "null" {
		if err := json.Unmarshal(r.Result, &res); err != nil {
			t.Fatalf("the answer's result: %v; %s", err, r)
		}
	}
	return res.MessageID
}

// tutorKeepsAskersApart is §2.5 and §6.1: a course tutor answers Yuki and
// Ken, each in their own conversation, and never carries one asker's words
// into another's prompt. Yuki is answered first, so that what she wrote is
// in the runtime's memory and ledger when Ken asks; then nothing of hers
// may be in any model request made for Ken's answer, nor his in hers.
func tutorKeepsAskersApart(t *testing.T, w *world) {
	m := newModel(t, fakellm.DefaultResponder)
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor}}})
	rt.waitPolling("tutor")

	yukiMark, kenMark := "yuki-"+randomHex(6), "ken-"+randomHex(6)
	yukiQ := "What does HW3 ask for? My study group is " + yukiMark + "."
	kenQ := "When is HW3 due? My study group is " + kenMark + "."
	convY, mY := w.ask(t, w.yuki, w.tutor.member, yukiQ)
	answerY := w.waitAnswer(t, w.yuki, convY, w.tutor.member)
	convK, mK := w.ask(t, w.ken, w.tutor.member, kenQ)
	answerK := w.waitAnswer(t, w.ken, convK, w.tutor.member)
	if convY == convK {
		t.Fatal("Yuki and Ken are in one conversation")
	}
	for _, c := range []struct {
		who     string
		a       message
		q, body string
	}{{"Yuki", answerY, mY, yukiQ}, {"Ken", answerK, mK, kenQ}} {
		if c.a.replyTo() != c.q || !strings.Contains(c.a.text(), c.body) {
			t.Errorf("%s's answer is %q in reply to %s; want an answer to their question %s", c.who, c.a.text(), c.a.replyTo(), c.q)
		}
	}

	forYuki, forKen := 0, 0
	for i, req := range m.Requests() {
		raw := string(req.Raw)
		switch q := question(req); {
		case strings.Contains(q, kenMark):
			forKen++
			if strings.Contains(raw, yukiMark) {
				t.Errorf("model request %d, made for Ken's answer, holds what Yuki wrote", i+1)
			}
		case strings.Contains(q, yukiMark):
			forYuki++
			if strings.Contains(raw, kenMark) {
				t.Errorf("model request %d, made for Yuki's answer, holds what Ken wrote", i+1)
			}
		}
	}
	if forYuki == 0 || forKen == 0 {
		t.Errorf("%d model requests were for Yuki's answer and %d for Ken's; want some for each", forYuki, forKen)
	}
}

// movedOn is the moved-on row of §2.4 (§2.6's last line): Yuki writes
// again while the model is still answering her first question (the
// model's responder posts her follow-up through Core before it replies).
// Core refuses the answer to the first question as moved_on, and the
// runtime answers the follow-up instead, under answer:{x}:{m2}:1: one
// answer in all, to the follow-up.
func movedOn(t *testing.T, w *world) {
	const (
		first       = "Is question 2 of the lab about recursion?"
		second      = "Sorry, I meant question 3 of the lab."
		firstReply  = "Question 2 is about loops."
		secondReply = "Question 3 is about recursion."
	)
	conv, m1 := w.ask(t, w.yuki, w.own.member, first)
	followed := make(chan string, 1)
	var once sync.Once
	m := newModel(t, func(req fakellm.ChatRequest) fakellm.ChatResponse {
		switch q := question(req); {
		case strings.HasSuffix(q, second):
			return fakellm.Reply(secondReply)
		case strings.HasSuffix(q, first):
			once.Do(func() {
				ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
				defer cancel()
				m2, err := w.followUp(ctx, w.yuki, conv, second)
				if err != nil {
					t.Errorf("Yuki's follow-up: %v", err)
				}
				followed <- m2
			})
			return fakellm.Reply(firstReply)
		}
		return fakellm.Reply("I cannot tell what was asked.")
	})
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own}}})

	var m2 string
	select {
	case m2 = <-followed:
	case <-time.After(answerWait):
		t.Fatal("the model was never asked the first question")
	}
	if m2 == "" {
		t.FailNow()
	}
	answer := w.waitAnswer(t, w.yuki, conv, w.own.member)
	if answer.replyTo() != m2 || answer.text() != secondReply {
		t.Errorf("the answer is %q in reply to %s; want %q in reply to the follow-up %s", answer.text(), answer.replyTo(), secondReply, m2)
	}
	rt.settle("yuki-helper", 3)
	if n := len(w.answers(t, w.yuki, conv, w.own.member)); n != 1 {
		t.Errorf("%d answers in the conversation; want 1", n)
	}

	// Core holds both keys: the first answer's, refused as moved_on, and
	// the follow-up's, posted. Each replays as it stands.
	r := w.answerAs(t, w.own, conv, m2, secondReply, answerKey(conv, m2, 1))
	if r.HTTP != http.StatusOK || !r.Replayed || r.Status != "executed" || decodeMessage(t, r) != answer.ID {
		t.Errorf("the follow-up's answer sent again under answer:{x}:{m2}:1: %s; want a replay making %s", r, answer.ID)
	}
	r = w.answerAs(t, w.own, conv, m1, firstReply, answerKey(conv, m1, 1))
	if !r.Replayed || r.Status != "failed" || r.Error == nil || r.Error.Details["reason"] != "moved_on" {
		t.Errorf("the first answer sent again under answer:{x}:{m1}:1: %s; want the moved_on refusal replayed", r)
	}
	if at := rt.attempt("yuki-helper", answerKey(conv, m1, 1)); at == nil || at.State != store.AttemptFailed {
		t.Errorf("the runtime recorded the first attempt as %s; want it failed", attemptState(at))
	}
}

// duplicates is §7.4: two workers that share no store (memstore each, as
// two workers without Postgres would be) both run Yuki's agent, and both
// take her one question. The model holds each back until both have asked
// it, so that both post under answer:{x}:{m1}:1, each with its own body.
// Core posts one: the other is an idempotency_conflict, which the runtime
// never sends again under that key, and since the conversation is answered
// it leaves it. Exactly one answer is posted.
func duplicates(t *testing.T, w *world) {
	const q = "What should I read before the lab?"
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
		both = make(chan struct{})
	)
	m := newModel(t, func(req fakellm.ChatRequest) fakellm.ChatResponse {
		tag := req.Header.Get(tagHeader)
		mu.Lock()
		if !seen[tag] {
			seen[tag] = true
			if len(seen) == 2 {
				close(both)
			}
		}
		mu.Unlock()
		wait := time.NewTimer(answerWait / 2)
		defer wait.Stop()
		select {
		case <-both:
		case <-wait.C:
		case <-t.Context().Done():
		}
		return fakellm.Reply("Worker " + tag + " says: read chapter 2.")
	})
	conv, m1 := w.ask(t, w.yuki, w.own.member, q)
	agents := []agentConf{{id: "yuki-helper", seat: w.own}}
	workers := []*instance{
		w.startRuntime(t, m, runtimeConf{workerID: "w1", tag: "w1", agents: agents}),
		w.startRuntime(t, m, runtimeConf{workerID: "w2", tag: "w2", agents: agents}),
	}
	answer := w.waitAnswer(t, w.yuki, conv, w.own.member)

	key := answerKey(conv, m1, 1)
	settled := func(rt *instance) *store.Attempt {
		at := rt.attempt("yuki-helper", key)
		if at == nil || at.State == store.AttemptSending {
			return nil
		}
		return at
	}
	eventually(t, answerWait, "both workers settling their attempt", func() bool {
		return settled(workers[0]) != nil && settled(workers[1]) != nil
	})
	mu.Lock()
	if !seen["w1"] || !seen["w2"] {
		t.Errorf("the model was asked by %v; want both workers", seen)
	}
	mu.Unlock()
	for _, rt := range workers {
		rt.settle("yuki-helper", 3)
	}
	if n := len(w.answers(t, w.yuki, conv, w.own.member)); n != 1 {
		t.Fatalf("%d answers in the conversation; want 1", n)
	}

	executed, conflicted := 0, 0
	for i, rt := range workers {
		at := settled(rt)
		switch {
		case at.State == store.AttemptExecuted && at.PostedMessageID == answer.ID:
			executed++
			if want := fmt.Sprintf("Worker w%d says: read chapter 2.", i+1); answer.text() != want {
				t.Errorf("worker w%d posted, but the answer is %q", i+1, answer.text())
			}
		case at.State == store.AttemptError && at.ErrorCode == "idempotency_conflict":
			conflicted++
		default:
			t.Errorf("worker w%d recorded its attempt as %s", i+1, attemptState(at))
		}
	}
	if executed != 1 || conflicted != 1 {
		t.Errorf("%d workers posted and %d met an idempotency_conflict; want one each", executed, conflicted)
	}
}

// denied is the denied row of §2.4 and M1's denied path. Sato sets his
// tutor's conversation_answer to denied while it is answering Yuki: Core
// refuses the answer, and the runtime stops polling the course's inbox
// (its seat no longer answers) while the answer stays unposted. Once Sato
// restores the level, the runtime polls again and answers, under the next
// attempt number.
func denied(t *testing.T, w *world) {
	const q = "Is the lab on Friday open to everyone?"
	generating, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := newModel(t, func(fakellm.ChatRequest) fakellm.ChatResponse {
		if calls.Add(1) == 1 {
			close(generating)
			select {
			case <-release:
			case <-t.Context().Done():
			}
		}
		return fakellm.Reply("Yes, the lab is open to everyone.")
	})
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor}}})
	rt.waitPolling("tutor")
	conv, m1 := w.ask(t, w.yuki, w.tutor.member, q)
	select {
	case <-generating:
	case <-time.After(answerWait):
		t.Fatal("the model was never asked")
	}
	w.setAnswerLevel(t, "denied")
	close(release)

	eventually(t, answerWait, "the first attempt recorded as denied", func() bool {
		at := rt.attempt("tutor", answerKey(conv, m1, 1))
		return at != nil && at.State == store.AttemptDenied
	})
	eventually(t, answerWait, "the tutor's seat stopping", func() bool {
		_, running := rt.seat("tutor", w.course)
		return !running
	})
	// The seat's pollers have ended once the agent has read its seats
	// again after stopping it: it does both in one loop.
	seats := rt.coreCalls("me_memberships")
	eventually(t, answerWait, "a read of the tutor's seats", func() bool { return rt.coreCalls("me_memberships") > seats })
	polls, seats := rt.inboxPolls("tutor"), rt.coreCalls("me_memberships")
	eventually(t, answerWait, "three more reads of the tutor's seats", func() bool { return rt.coreCalls("me_memberships") >= seats+3 })
	if now := rt.inboxPolls("tutor"); now != polls {
		t.Errorf("the tutor's inbox was polled %v times while it was denied; want none", now-polls)
	}
	if n := len(w.answers(t, w.yuki, conv, w.tutor.member)); n != 0 {
		t.Fatalf("%d answers while the tutor was denied; want none", n)
	}

	w.setAnswerLevel(t, "autonomous")
	answer := w.waitAnswer(t, w.yuki, conv, w.tutor.member)
	if answer.replyTo() != m1 {
		t.Errorf("the answer replies to %s; want the question %s", answer.replyTo(), m1)
	}
	if rt.inboxPolls("tutor") <= polls {
		t.Error("the answer came without an inbox poll")
	}
	if at := rt.attempt("tutor", answerKey(conv, m1, 2)); at == nil || at.State != store.AttemptExecuted || at.PostedMessageID != answer.ID {
		t.Errorf("the second attempt is recorded as %s; want it executed, making %s", attemptState(at), answer.ID)
	}
	rt.settle("tutor", 3)
	if n := len(w.answers(t, w.yuki, conv, w.tutor.member)); n != 1 {
		t.Errorf("%d answers in the conversation; want 1", n)
	}
}

// proposals is §2.4's proposed rows and §5.4 of the design, as M2 has them:
// the tutor at confirm_required, whose answers wait for a person. Mori, the
// second instructor, approves one: the runtime sees action.approved and
// records the answer posted. He rejects another with a reason: the
// runtime's next attempt (key :2) has the reason in the model's system
// prompt, and is proposed again.
func proposals(t *testing.T, w *world) {
	const reason = "Point to the page of the syllabus that says so."
	w.setAnswerLevel(t, "confirm_required")
	m := newModel(t, func(req fakellm.ChatRequest) fakellm.ChatResponse {
		return fakellm.Reply("Answer: " + question(req))
	})
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor}}})
	rt.waitPolling("tutor")

	// Approved.
	const qY = "Can I hand in HW3 a day late?"
	convY, mY := w.ask(t, w.yuki, w.tutor.member, qY)
	proposed := w.waitPending(t, w.yuki, convY, "")
	keyY := answerKey(convY, mY, 1)
	eventually(t, answerWait, "the runtime recording the proposal", func() bool {
		at := rt.attempt("tutor", keyY)
		return at != nil && at.State == store.AttemptProposed && at.ActionID == proposed
	})
	if out := w.decide(t, proposed, "approve", ""); out != "executed" {
		t.Fatalf("Mori approved the answer, and it came to %s", out)
	}
	answer := w.waitAnswer(t, w.yuki, convY, w.tutor.member)
	if answer.replyTo() != mY || answer.text() != "Answer: "+qY {
		t.Errorf("the approved answer is %q in reply to %s", answer.text(), answer.replyTo())
	}
	eventually(t, answerWait, "the runtime recording the approved answer as posted", func() bool {
		at := rt.attempt("tutor", keyY)
		return at != nil && at.State == store.AttemptExecuted && at.PostedMessageID == answer.ID
	})

	// Rejected, then proposed again with the reason in mind.
	const qK = "Is the final exam open book?"
	convK, mK := w.ask(t, w.ken, w.tutor.member, qK)
	rejected := w.waitPending(t, w.ken, convK, "")
	if out := w.decide(t, rejected, "reject", reason); out != "rejected" {
		t.Fatalf("Mori rejected the answer, and it came to %s", out)
	}
	again := w.waitPending(t, w.ken, convK, rejected)
	key1, key2 := answerKey(convK, mK, 1), answerKey(convK, mK, 2)
	eventually(t, answerWait, "the runtime recording the second proposal", func() bool {
		at := rt.attempt("tutor", key2)
		return at != nil && at.State == store.AttemptProposed && at.ActionID == again
	})
	if at := rt.attempt("tutor", key1); at == nil || at.State != store.AttemptRejected || at.Reason != reason {
		t.Errorf("the first attempt is recorded as %s; want it rejected, with Mori's reason", attemptState(at))
	}
	var first, withReason int
	for _, req := range m.Requests() {
		if question(req) != qK {
			continue
		}
		if strings.Contains(system(req), reason) {
			withReason++
		} else if withReason == 0 {
			first++
		}
	}
	if first == 0 || withReason == 0 {
		t.Errorf("of the model requests for Ken's answer, %d came before the rejection and %d had its reason in the system prompt; want some of each",
			first, withReason)
	}

	// Core holds the second proposal under answer:{x}:{m}:2: the runtime's
	// stored bytes, sent again, replay it.
	at := rt.attempt("tutor", key2)
	var args struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(at.Args, &args); err != nil {
		t.Fatal(err)
	}
	r := w.answerAs(t, w.tutor, convK, mK, args.Body, key2)
	if r.HTTP != http.StatusAccepted || r.Status != "proposed" || !r.Replayed || r.ActionID != again {
		t.Errorf("the second attempt sent again under answer:{x}:{m}:2: %s; want the proposal %s replayed", r, again)
	}
	if n := len(w.answers(t, w.ken, convK, w.tutor.member)); n != 0 {
		t.Errorf("%d answers to Ken before anyone approved one; want none", n)
	}
}
