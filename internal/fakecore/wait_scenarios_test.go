package fakecore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

// The scenarios of waiting for news (wait.go), recorded from Core 2c1fe1b
// like the rest. When a call answered is recorded as one of:
//
//   - before_the_news: it answered before the news it was to wait for was
//     made;
//   - on_news: after the news, well before its time was up;
//   - when_its_time_was_up: at the end of wait_s, or near it;
//   - at_once: with no news to wait for, well before its time was up.
//
// Times are the only thing a real Core and the fake cannot be held to
// exactly; these four are far enough apart that neither a slow machine nor
// a busy one blurs them.

// answeredWhen says when a call that asked to wait wait answered, having
// taken took; newsAt is when the news was made, from the call's start, or
// below zero when there was none.
func answeredWhen(took, newsAt, wait time.Duration) string {
	switch {
	case newsAt >= 0 && took < newsAt:
		return "before_the_news"
	case wait > 0 && took >= wait*9/10:
		return "when_its_time_was_up"
	case newsAt >= 0:
		return "on_news"
	}
	return "at_once"
}

// waitSeconds is args' wait_s.
func waitSeconds(args map[string]any) time.Duration {
	n, _ := args["wait_s"].(int)
	return time.Duration(n) * time.Second
}

// waitingCall makes a tool call over MCP that may wait for news, as
// whoever c is, and records it with when it answered. news, when given, is
// made after the call has run for after: the news that is to end its wait.
func waitingCall(t *testing.T, c *mcpClient, s *steps, name, tool string, args map[string]any, after time.Duration, news func()) toolAnswer {
	t.Helper()
	var a toolAnswer
	took, newsAt := timed(t, name, after, news, func() error {
		var err error
		a, err = c.call(context.Background(), tool, args)
		return err
	})
	s.tool(name, tool, args, a)
	s.list[len(s.list)-1]["answered"] = answeredWhen(took, newsAt, waitSeconds(args))
	return a
}

// waitingGet is waitingCall over REST: a GET of path as the tutor.
func waitingGet(t *testing.T, w world, s *steps, name, path string, wait, after time.Duration, news func()) httpAnswer {
	t.Helper()
	var a httpAnswer
	took, newsAt := timed(t, name, after, news, func() error {
		var err error
		a, err = w.rest().do(context.Background(), http.MethodGet, path, nil, "")
		return err
	})
	s.rest(name, http.MethodGet, path, nil, a)
	s.list[len(s.list)-1]["answered"] = answeredWhen(took, newsAt, wait)
	return a
}

// timed runs call, and news after after while it runs: how long call took,
// and when the news was made (below zero when there was none).
func timed(t *testing.T, name string, after time.Duration, news func(), call func() error) (took, newsAt time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- call() }()
	newsAt = -1
	var err error
	if news != nil {
		select {
		case err = <-done:
			took = time.Since(start)
			news()
			newsAt = time.Since(start)
		case <-time.After(after):
			newsAt = time.Since(start)
			news()
			err = <-done
			took = time.Since(start)
		}
	} else {
		err = <-done
		took = time.Since(start)
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return took, newsAt
}

// firstConversation is the id of the first conversation an inbox answer
// lists.
func firstConversation(t *testing.T, a toolAnswer) string {
	t.Helper()
	list, _ := resultOf(a, "conversations").([]any)
	if len(list) == 0 {
		t.Fatalf("no conversation in the inbox: %s", a.Body)
	}
	cv, _ := list[0].(map[string]any)
	id, _ := cv["id"].(string)
	return id
}

// lastSeq is the seq of the last message a conversation_messages answer
// holds.
func lastSeq(t *testing.T, a toolAnswer) any {
	t.Helper()
	msgs, _ := resultOf(a, "messages").([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages: %s", a.Body)
	}
	m, _ := msgs[len(msgs)-1].(map[string]any)
	return m["seq"]
}

// news waits a second before it is made: long enough that a call that
// does not wait has answered, short against the five seconds it asks for.
const newsAfter = time.Second

var waitForNews = scenario{name: "wait_for_news", about: "wait_s: conversation_inbox, event_list and conversation_messages wait for news and answer as soon as it comes, " +
	"at once when there is something, and empty when their time is up, over MCP and REST; wait_s out of its bounds, with before_seq, " +
	"and a seen_state that is no state, refused; a seen_state that is not the state now answers at once",
	run: func(t *testing.T, w world, s *steps) {
		wait5 := func(more ...any) map[string]any { return inCourseArgs(w, append([]any{"wait_s", 5}, more...)...) }
		var q1 string
		inbox := waitingCall(t, w.agent(), s, "inbox_waits_for_a_question", "conversation_inbox", wait5(), newsAfter, func() {
			_, q1 = w.ask(0, "Q1: when is the lab report due?")
		})
		conv := firstConversation(t, inbox)
		waitingCall(t, w.agent(), s, "inbox_with_a_question_waiting", "conversation_inbox", wait5(), 0, nil)
		wantStatus(t, call(t, w, s, "answer_q1", "conversation_answer", answer(w, conv, q1, "On Friday.", 1)), "executed")
		waitingCall(t, w.agent(), s, "inbox_empty_when_its_time_is_up", "conversation_inbox", inCourseArgs(w, "wait_s", 1), 0, nil)

		events := call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		waitingCall(t, w.agent(), s, "events_wait_for_a_follow_up", "event_list", wait5("since_seq", resultOf(events, "next_seq")), newsAfter,
			func() { w.followUp(conv, "Q2: and is it typed?") })
		waitingCall(t, w.agent(), s, "events_with_news_waiting", "event_list", wait5("since_seq", resultOf(events, "next_seq")), 0, nil)

		read := call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		seq := lastSeq(t, read)
		waitingCall(t, w.agent(), s, "messages_wait_for_a_follow_up", "conversation_messages", wait5("conversation_id", conv, "after_seq", seq), newsAfter,
			func() { w.followUp(conv, "Q3: typed or handwritten?") })
		read = call(t, w, s, "messages_again", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		seq = lastSeq(t, read)
		waitingCall(t, w.agent(), s, "messages_seen_state_not_the_state", "conversation_messages",
			wait5("conversation_id", conv, "after_seq", seq, "seen_state", "answered"), 0, nil)
		waitingCall(t, w.agent(), s, "messages_seen_state_the_state_time_up", "conversation_messages",
			inCourseArgs(w, "conversation_id", conv, "after_seq", seq, "seen_state", "awaiting_answer", "wait_s", 1), 0, nil)
		call(t, w, s, "messages_wait_with_before_seq", "conversation_messages", wait5("conversation_id", conv, "before_seq", seq))
		call(t, w, s, "messages_seen_state_no_state", "conversation_messages", inCourseArgs(w, "conversation_id", conv, "after_seq", seq, "seen_state", "waiting"))
		call(t, w, s, "wait_s_over_25", "conversation_inbox", inCourseArgs(w, "wait_s", 26))
		call(t, w, s, "wait_s_negative", "event_list", inCourseArgs(w, "wait_s", -1))
		call(t, w, s, "wait_s_a_fraction", "conversation_inbox", inCourseArgs(w, "wait_s", 1.5))

		// Over REST: the inbox, emptied by an answer, waits for Ken's
		// question.
		read = call(t, w, s, "messages_to_answer", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		latest, _ := resultOf(read, "conversation").(map[string]any)["latest_opener_message_id"].(string)
		wantStatus(t, call(t, w, s, "answer_q3", "conversation_answer", answer(w, conv, latest, "Typed.", 1)), "executed")
		path := fmt.Sprintf("/v1/courses/%s/conversations/inbox?wait_s=5", w.course())
		waitingGet(t, w, s, "rest_inbox_waits_for_a_question", path, 5*time.Second, newsAfter, func() { w.ask(1, "K1: is there a lab this week?") })
		waitingGet(t, w, s, "rest_inbox_with_a_question_waiting", path, 5*time.Second, 0, nil)

		// A conversation closed while a reader waits on it: no message, but
		// news all the same.
		read = call(t, w, s, "messages_before_the_close", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		waitingCall(t, w.agent(), s, "messages_wait_for_the_close", "conversation_messages",
			wait5("conversation_id", conv, "after_seq", lastSeq(t, read)), newsAfter, func() { w.closeAsOpener(conv, "Thanks!") })
	}}

var waitCap = scenario{name: "wait_cap", about: "an actor's calls waiting at once, at Core's default of 16 (LONG_POLL_WAITERS_PER_ACTOR): " +
	"past them a call answers at once, as if it had not asked to wait; a question wakes every call waiting",
	run: func(t *testing.T, w world, s *steps) {
		const perActor = 16
		args := inCourseArgs(w, "wait_s", 5)
		type answered struct {
			a    toolAnswer
			took time.Duration
			err  error
		}
		start := time.Now()
		results := make(chan answered, perActor)
		var wg sync.WaitGroup
		for range perActor {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a, err := w.agent().call(context.Background(), "conversation_inbox", args)
				results <- answered{a, time.Since(start), err}
			}()
		}
		// Long enough for all sixteen to be waiting.
		time.Sleep(newsAfter)
		waitingCall(t, w.agent(), s, "past_the_cap", "conversation_inbox", args, 0, nil)
		newsAt := time.Since(start)
		w.ask(0, "Q1: when is the lab report due?")
		wg.Wait()
		close(results)
		when := map[string]int{}
		var first toolAnswer
		for r := range results {
			if r.err != nil {
				t.Fatal(r.err)
			}
			when[answeredWhen(r.took, newsAt, 5*time.Second)]++
			if first.Structured == nil {
				first = r.a
			}
		}
		s.tool("those_waiting", "conversation_inbox", args, first)
		keys := make([]string, 0, len(when))
		for k := range when {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		counted := make([]any, 0, len(keys))
		for _, k := range keys {
			counted = append(counted, map[string]any{"answered": k, "calls": when[k]})
		}
		s.list[len(s.list)-1]["answered"] = counted
		raw, _ := json.Marshal(counted)
		t.Logf("the %d calls waiting answered: %s", perActor, raw)
	}}
