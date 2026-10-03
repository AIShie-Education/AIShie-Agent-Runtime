package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// versionOf is the version of the course's document id, as the fake holds
// it.
func (w *world) versionOf(id string) string {
	w.t.Helper()
	for _, d := range w.fc.Documents(w.co.ID) {
		if d.ID == id {
			return d.VersionID
		}
	}
	w.t.Fatalf("no document %s", id)
	return ""
}

// sourcesOf is what an answer said it relied on, as the fake holds it:
// "none" for an empty list, "unsaid" where it said nothing.
func sourcesOf(m fakecore.MessageRecord) string {
	if !m.SourcesStated {
		return "unsaid"
	}
	if len(m.Sources) == 0 {
		return "none"
	}
	var out []string
	for _, s := range m.Sources {
		out = append(out, fmt.Sprintf("%s@%s/%s/%d/%d/%d", s.DocumentID, s.VersionID, s.FileID, s.Page, s.Slide, s.Part))
	}
	return strings.Join(out, ", ")
}

// source is a document's version as sourcesOf names it, with no file.
func (w *world) source(id string) string { return id + "@" + w.versionOf(id) + "//0/0/0" }

func getDoc(id string) scripted.ToolCall {
	return scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + id + `"}`}
}

// The tutor's answer says what it relied on: the syllabus and the HW1
// instructions it read, in the order it read them, and not the material
// it only listed; an answer that read nothing says it relied on none; and
// the notice of a refusal, after reading, relies on none either. Each is
// what the attempt written ahead holds, and was sent.
func TestAnAnswerSaysWhatItReliedOn(t *testing.T) {
	w := newWorld(t)
	tut := w.tutor("tutor")
	m := scripted.New(
		scripted.CallTools(scripted.ToolCall{Name: "document_list", Args: `{}`}, getDoc(w.co.SyllabusID)),
		scripted.CallTools(getDoc(w.co.InstructionsID), getDoc(w.co.SyllabusID)),
		scripted.Reply("Chapter 1, three questions; show your working."),
		scripted.Reply("Hello."),
		scripted.CallTools(getDoc(w.co.SyllabusID)),
		scripted.Stop(llm.StopRefusal, ""),
	)
	wk := w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{})

	conv, msg := w.ask(0, tut, "What is HW1?")
	a := w.waitAnswers(conv, 1)[0]
	if got, want := sourcesOf(a), w.source(w.co.SyllabusID)+", "+w.source(w.co.InstructionsID); got != want {
		t.Errorf("the answer relied on %s; want %s", got, want)
	}
	atts, err := wk.st.AttemptsFor(context.Background(), "tutor", conv, msg)
	w.ok(err)
	var args core.AnswerArgs
	if len(atts) != 1 || json.Unmarshal(atts[0].Args, &args) != nil || len(args.Sources) != 2 ||
		args.Sources[0] != (core.Source{DocumentID: w.co.SyllabusID, VersionID: w.versionOf(w.co.SyllabusID)}) {
		t.Errorf("the attempt written ahead: %+v", atts)
	}

	conv2, _ := w.ask(1, tut, "Hi!")
	if a := w.waitAnswers(conv2, 1)[0]; sourcesOf(a) != "none" || !strings.Contains(string(callArgs(t, w, tut, conv2)), `"sources":[]`) {
		t.Errorf("an answer that read nothing relied on %s", sourcesOf(a))
	}
	conv3, _ := w.ask(0, tut, "Write my essay for me.")
	if a := w.waitAnswers(conv3, 1)[0]; sourcesOf(a) != "none" || a.Body != config.DefaultRefusalText {
		t.Errorf("the refusal's notice: %q, relying on %s", a.Body, sourcesOf(a))
	}
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
}

// callArgs are the arguments of the agent's conversation_answer in conv,
// as the fake took them.
func callArgs(t *testing.T, w *world, ag agent, conv string) json.RawMessage {
	t.Helper()
	for _, c := range w.calls(ag.actor.ID, "conversation_answer") {
		if strings.Contains(string(c.Args), conv) {
			return c.Args
		}
	}
	t.Fatalf("no answer in %s", conv)
	return nil
}

// An answer that only searched the course's materials, reading none of
// its hits, says nothing of its sources: it was given excerpts, which it
// is told to read before it relies on them, so it cannot be said to have
// relied on none.
func TestAnAnswerThatOnlySearchedSaysNothingOfItsSources(t *testing.T) {
	w := newWorld(t)
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "排序的複雜度", Body: []doctexttest.Bullet{{Text: "合併排序：O(n log n)"}}})
	_, err := w.fc.AddFile(w.co.ID, "Week 3 slides", doctexttest.PPTXType, deck)
	w.ok(err)
	tut := w.tutor("tutor")
	m := scripted.New(scripted.CallTool(toolset.SearchTool, `{"query":"合併排序"}`), scripted.Reply("O(n log n)."))
	w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{store: memstore.New()})
	conv, _ := w.ask(0, tut, "合併排序的複雜度？")
	if a := w.waitAnswers(conv, 1)[0]; sourcesOf(a) != "unsaid" || strings.Contains(string(callArgs(t, w, tut, conv)), `"sources"`) {
		t.Errorf("an answer that only searched relied on %s", sourcesOf(a))
	}
	if hits := searchHits(t, m.Requests()); len(hits) == 0 {
		t.Error("the search found nothing")
	}
}

// A source Core refuses as one the tutor may no longer read (the HW1
// instructions, withheld as HW1 is unpublished while the answer is
// written) is dropped, and the answer posted again at once under the next
// attempt's key, its other sources kept: the refused call is recorded
// failed, its key spent, and nothing regenerated. One purged meanwhile,
// the answer's only source, is dropped too, and the answer posted saying
// nothing of its sources: it relied on a material it can no longer name.
func TestARefusedSourceIsDroppedAndTheAnswerPostedAgain(t *testing.T) {
	w := newWorld(t)
	tut := w.tutor("tutor")
	m := scripted.New(
		scripted.CallTools(getDoc(w.co.SyllabusID), getDoc(w.co.InstructionsID)),
		scripted.Reply("Chapter 1, three questions.").Then(func(*llm.Request) {
			w.ok(w.fc.UnpublishAssignment(w.co.ID, w.co.AssignmentID))
		}),
		scripted.CallTools(getDoc(w.co.SyllabusID)),
		scripted.Reply("Lectures weekly.").Then(func(*llm.Request) { w.ok(w.fc.PurgeVersion(w.co.SyllabusID)) }),
	)
	wk := w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{})

	syllabus := w.source(w.co.SyllabusID)
	conv, msg := w.ask(0, tut, "What is HW1?")
	a := w.waitAnswers(conv, 1)[0]
	if a.Body != "Chapter 1, three questions." || sourcesOf(a) != syllabus || a.IdempotencyKey != core.AnswerKey(conv, msg, 2) {
		t.Errorf("the answer %q under %s relied on %s; want %s under attempt 2", a.Body, a.IdempotencyKey, sourcesOf(a), syllabus)
	}
	atts := settledAttempts(t, wk, conv, msg, 2)
	if len(atts) != 2 || atts[0].State != store.AttemptFailed || atts[0].ErrorCode != core.CodeInvalidArgument ||
		atts[0].Reason != reasonSourceUnreadable || atts[1].State != store.AttemptExecuted || atts[1].Kind != kindModel {
		t.Errorf("the attempts: %+v", atts)
	}
	if !strings.Contains(w.logs.String(), "Core refused a source of the answer: it is posted again without it") {
		t.Error("no log line says the source was dropped")
	}

	conv2, msg2 := w.ask(1, tut, "How often are the lectures?")
	a = w.waitAnswers(conv2, 1)[0]
	if a.Body != "Lectures weekly." || sourcesOf(a) != "unsaid" || a.IdempotencyKey != core.AnswerKey(conv2, msg2, 2) {
		t.Errorf("the answer %q under %s relied on %s; want it to say nothing, under attempt 2", a.Body, a.IdempotencyKey, sourcesOf(a))
	}
	atts = settledAttempts(t, wk, conv2, msg2, 2)
	if len(atts) != 2 || atts[0].Reason != reasonSourcePurged || atts[1].State != store.AttemptExecuted {
		t.Errorf("the attempts: %+v", atts)
	}
	eventually(t, "the second answer in the ledger", func() bool { return len(wk.st.outcomes(conv2)) > 0 })
	for _, c := range []string{conv, conv2} {
		if outcomes := wk.st.outcomes(c); len(outcomes) != 1 || outcomes[0] != store.OutcomePosted {
			t.Errorf("the ledger of %s: %v", c, outcomes)
		}
	}
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
}

// settledAttempts are the tutor's attempts at msg in conv once there are n and
// none is still being sent: Core posts an answer before the runtime
// records what came back.
func settledAttempts(t *testing.T, wk *worker, conv, msg string, n int) []store.Attempt {
	t.Helper()
	var atts []store.Attempt
	eventually(t, fmt.Sprintf("%d attempts settled", n), func() bool {
		var err error
		if atts, err = wk.st.AttemptsFor(context.Background(), "tutor", conv, msg); err != nil {
			t.Fatal(err)
		}
		return len(atts) >= n && !slices.ContainsFunc(atts, func(at store.Attempt) bool { return at.State == store.AttemptSending })
	})
	return atts
}

// A Core from before an answer's sources (AIShie-Core #71) is sent none:
// its catalogue's conversation_answer takes none, and an answer that
// read a material says nothing of it, as every answer did before. One
// whose catalogue says it takes them while it does not, an older Core
// behind a newer catalogue, refuses them as its schema refuses any
// argument it does not name: the answer is posted again without them,
// under the next attempt.
func TestAnOlderCoreIsSentNoSources(t *testing.T) {
	snapshot, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		behind bool
		key    int
	}{{"its own catalogue", false, 1}, {"behind the pinned catalogue", true, 2}} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorldWith(t, fakecore.Options{WithoutSources: true})
			if c.behind {
				w.tools.Store(&snapshot)
			}
			tut := w.tutor("tutor")
			m := scripted.New(scripted.CallTools(getDoc(w.co.SyllabusID)), scripted.Reply("Lectures weekly."))
			w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{})
			conv, msg := w.ask(0, tut, "How often are the lectures?")
			a := w.waitAnswers(conv, 1)[0]
			if a.Body != "Lectures weekly." || sourcesOf(a) != "unsaid" || a.IdempotencyKey != core.AnswerKey(conv, msg, c.key) {
				t.Errorf("the answer %q under %s relied on %s; want it to say nothing, under attempt %d", a.Body, a.IdempotencyKey, sourcesOf(a), c.key)
			}
			if err := m.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// sentSources is what an answer's arguments, as written ahead or as the
// fake keeps a proposal's, say it relied on, as sourcesOf names it.
func sentSources(t *testing.T, args json.RawMessage) string {
	t.Helper()
	var a struct {
		Sources *[]core.Source `json:"sources"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		t.Fatal(err)
	}
	m := fakecore.MessageRecord{SourcesStated: a.Sources != nil}
	if a.Sources != nil {
		for _, s := range *a.Sources {
			m.Sources = append(m.Sources, fakecore.SourceRecord{DocumentID: s.DocumentID, VersionID: s.VersionID, FileID: s.FileID,
				Page: s.Page, Slide: s.Slide, Part: s.Part})
		}
	}
	return sourcesOf(m)
}

// A revision says what it relied on, and what it revises, in one call: the
// answer a person sent back for changes named the syllabus it read; the
// revision names the instructions and the syllabus it read in its own
// loop, and the proposal it revises (revises over MCP, the Revises header
// over REST), both in the bytes written ahead. A source of the revision
// Core refuses (the HW1 instructions, withheld as HW1 is unpublished while
// it is written) is dropped, and the revision posted again at once under
// the next attempt's key, still naming the proposal it revises, the model
// not asked again. Approved, the revision is posted with its sources.
func TestARevisionSaysWhatItReliedOn(t *testing.T) {
	for _, c := range []struct {
		name, transport string
		withheld        bool
	}{
		{"over MCP", "mcp", false},
		{"over REST", "rest", false},
		{"a source refused, over MCP", "mcp", true},
		{"a source refused, over REST", "rest", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			syllabus, instructions := w.source(w.co.SyllabusID), w.source(w.co.InstructionsID)
			const note = "Say what HW1 asks, not only where it is."
			const body = "HW1 is three questions on chapter 1; show your working."
			revision := scripted.Reply(body)
			if c.withheld {
				revision = revision.Then(func(*llm.Request) { w.ok(w.fc.UnpublishAssignment(w.co.ID, w.co.AssignmentID)) })
			}
			model := scripted.New(
				scripted.CallTools(getDoc(w.co.SyllabusID)), scripted.Reply("HW1 is in chapter 1."),
				scripted.CallTools(getDoc(w.co.InstructionsID), getDoc(w.co.SyllabusID)), revision,
			)
			tu, wk := confirmedTutor(t, w, model, map[string]any{"core": map[string]any{"transport": c.transport}})
			conv, msg := w.ask(0, tu, "What is HW1?")
			k1 := core.AnswerKey(conv, msg, 1)
			p1 := w.waitProposal(k1)
			if got := sentSources(t, p1.Args); got != syllabus {
				t.Errorf("the first answer relied on %s; want %s", got, syllabus)
			}
			wk.waitAttempt("cs101-tutor", k1, store.AttemptProposed)
			w.ok(w.fc.RequestChanges(p1.ActionID, note))

			n, want := 2, instructions+", "+syllabus
			if c.withheld {
				n, want = 3, syllabus
			}
			key := core.AnswerKey(conv, msg, n)
			p := w.waitProposal(key)
			if got := sentSources(t, p.Args); p.Revises != p1.ActionID || got != want {
				t.Errorf("the revision under %s revises %q, relying on %s; want %s, relying on %s", key, p.Revises, got, p1.ActionID, want)
			}
			at := wk.waitAttempt("cs101-tutor", key, store.AttemptProposed)
			var ahead core.AnswerArgs
			w.ok(json.Unmarshal(at.Args, &ahead))
			if got := sentSources(t, at.Args); ahead.Revises != p1.ActionID || ahead.IdempotencyKey != key || got != want || ahead.Body != body {
				t.Errorf("the revision written ahead: %s", at.Args)
			}
			if c.withheld {
				k2 := core.AnswerKey(conv, msg, 2)
				refused, err := wk.st.Attempt(context.Background(), "cs101-tutor", k2)
				w.ok(err)
				if refused.State != store.AttemptFailed || refused.Reason != reasonSourceUnreadable ||
					!strings.Contains(string(refused.Args), `"revises":"`+p1.ActionID+`"`) || sentSources(t, refused.Args) != instructions+", "+syllabus {
					t.Errorf("the revision Core refused for its source: %+v, %s", refused, refused.Args)
				}
			}
			sent := map[string]int{}
			for _, call := range w.calls(tu.actor.ID, toolAnswer) {
				sent[call.IdempotencyKey]++
				if call.IdempotencyKey == k1 {
					continue
				}
				if call.Revises != p1.ActionID || call.Transport != c.transport || !strings.Contains(string(call.Args), `"sources":[{`) {
					t.Errorf("a revision was sent as %+v: %s", call, call.Args)
				}
			}
			if len(sent) != n || sent[k1] != 1 || sent[key] != 1 {
				t.Errorf("answers sent %v; want each of %d attempts once", sent, n)
			}

			outcome, err := w.fc.Approve(p.ActionID)
			w.ok(err)
			if outcome != "executed" {
				t.Fatalf("approval: %s", outcome)
			}
			if a := w.waitAnswers(conv, 1)[0]; a.Body != body || sourcesOf(a) != want {
				t.Errorf("the revision posted: %q, relying on %s; want %s", a.Body, sourcesOf(a), want)
			}
			reqs := model.Requests()
			if len(reqs) != 4 || !strings.Contains(reqs[2].System, revisionSaid(note)) {
				t.Errorf("%d model calls; want 4, the revision told what to change", len(reqs))
			}
			if err := model.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// An answer left sending by a crash, naming a source Core refuses when
// it is sent again at the seat's start (the HW1 instructions, withheld as
// HW1 was unpublished meanwhile), is posted again at once without that
// source, under the next attempt's key, as a claim posts it: the model is
// not asked, and one row in the ledger says it was posted. The attempt
// that posts it again is dated when it is written, not when the attempt
// Core refused was.
func TestARefusedSourceLeftSendingIsDroppedAtStart(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("tutor")
	conv, msg := w.ask(0, tu, "What is HW1?")
	syllabus := core.Source{DocumentID: w.co.SyllabusID, VersionID: w.versionOf(w.co.SyllabusID)}
	instructions := core.Source{DocumentID: w.co.InstructionsID, VersionID: w.versionOf(w.co.InstructionsID)}
	k1, k2 := core.AnswerKey(conv, msg, 1), core.AnswerKey(conv, msg, 2)
	const body = "Written before the restart: chapter 1, three questions."
	args, err := json.Marshal(core.AnswerArgs{CourseID: w.co.ID, ConversationID: conv, InReplyToMessageID: msg, Body: body,
		Sources: []core.Source{syllabus, instructions}, IdempotencyKey: k1})
	w.ok(err)
	w.ok(w.fc.UnpublishAssignment(w.co.ID, w.co.AssignmentID))
	st := memstore.New()
	written := time.Now().Add(-time.Hour)
	if _, err := st.PutAttempt(context.Background(), store.Attempt{Key: k1, AgentID: "tutor", MemberID: tu.seat.ID, CourseID: w.co.ID,
		ConversationID: conv, MessageID: msg, No: 1, Tool: toolAnswer, Args: args, Kind: kindModel, State: store.AttemptSending,
		CreatedAt: written}); err != nil {
		t.Fatal(err)
	}

	model := scripted.New()
	wk := w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": model}, workerOpts{store: st})
	a := w.waitAnswers(conv, 1)[0]
	if want := w.source(w.co.SyllabusID); a.Body != body || sourcesOf(a) != want || a.IdempotencyKey != k2 {
		t.Errorf("the answer %q under %s relied on %s; want the one written ahead under %s, relying on %s", a.Body, a.IdempotencyKey,
			sourcesOf(a), k2, want)
	}
	atts := settledAttempts(t, wk, conv, msg, 2)
	if len(atts) != 2 || atts[0].State != store.AttemptFailed || atts[0].Reason != reasonSourceUnreadable ||
		atts[1].State != store.AttemptExecuted || atts[1].Kind != kindModel {
		t.Fatalf("the attempts: %+v", atts)
	}
	if !atts[0].CreatedAt.Equal(written.Truncate(time.Microsecond)) || !atts[1].CreatedAt.After(written.Add(time.Minute)) {
		t.Errorf("the attempt posted again is dated %s, the one Core refused %s; want it dated as it was written",
			atts[1].CreatedAt, atts[0].CreatedAt)
	}
	eventually(t, "the answer in the ledger", func() bool { return len(wk.st.outcomes(conv)) > 0 })
	if o := wk.st.outcomes(conv); len(o) != 1 || o[0] != store.OutcomePosted {
		t.Errorf("the ledger: %v", o)
	}
	if n := len(model.Requests()); n != 0 {
		t.Errorf("the model was asked %d times; want none", n)
	}
}

// An answer posted again without a source Core refused is not another
// attempt at answering: it counts toward no max_attempts. With one
// attempt, the answer is posted again under attempt 2. With two, the
// answer posted again under attempt 2 and refused otherwise (its key used
// before, by another body) leaves one attempt at answering, which the
// model writes under attempt 3, rather than the message being skipped
// until the next day.
func TestRepostsCountTowardNoMaxAttempts(t *testing.T) {
	for _, c := range []struct {
		name     string
		attempts int
		spent    bool
	}{{"one attempt", 1, false}, {"two attempts, the answer posted again refused", 2, true}} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			tu := w.tutor("tutor")
			conv, msg := w.ask(0, tu, "What is HW1?")
			k2, k3 := core.AnswerKey(conv, msg, 2), core.AnswerKey(conv, msg, 3)
			if c.spent {
				// Attempt 2's key, used by another body that Core refused.
				args, err := json.Marshal(core.AnswerArgs{CourseID: w.co.ID, ConversationID: conv, InReplyToMessageID: msg, Body: "Another body.",
					Sources: []core.Source{{DocumentID: w.co.SyllabusID, VersionID: "0192f3c1-dead-7b4a-9c3d-2e1f0a9b8c7d"}}, IdempotencyKey: k2})
				w.ok(err)
				caller := core.NewMCPCaller(core.MCPOptions{BaseURL: w.srv.URL, Token: tu.actor.Token, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
				if env, err := caller.Call(context.Background(), toolAnswer, args); err != nil || env.Status != core.StatusFailed {
					t.Fatalf("the answer under attempt 2's key: %+v, %v", env, err)
				}
			}
			m := scripted.New(
				scripted.CallTools(getDoc(w.co.SyllabusID), getDoc(w.co.InstructionsID)),
				scripted.Reply("Chapter 1, three questions.").Then(func(*llm.Request) {
					w.ok(w.fc.UnpublishAssignment(w.co.ID, w.co.AssignmentID))
				}),
				scripted.Reply("Written again: chapter 1."),
			)
			over := map[string]any{"answer": map[string]any{"max_attempts": c.attempts}}
			wk := w.start(w.config(nil, w.agentDoc("tutor", "m", over, nil)), models{"m": m}, workerOpts{})
			a := w.waitAnswers(conv, 1)[0]
			body, key, requests := "Chapter 1, three questions.", k2, 2
			if c.spent {
				body, key, requests = "Written again: chapter 1.", k3, 3
			}
			if a.Body != body || a.IdempotencyKey != key {
				t.Errorf("the answer %q under %s; want %q under %s", a.Body, a.IdempotencyKey, body, key)
			}
			if c.spent {
				at := wk.waitAttempt("tutor", k2, store.AttemptError)
				if at.ErrorCode != core.CodeIdempotencyConflict || sentSources(t, at.Args) != w.source(w.co.SyllabusID) {
					t.Errorf("the answer posted again: %+v, %s", at, at.Args)
				}
			}
			if n := len(m.Requests()); n != requests {
				t.Errorf("the model was asked %d times; want %d", n, requests)
			}
		})
	}
}

// A follow-up drawn from an earlier answer in the conversation, reading
// none of the course's materials itself, says nothing of its sources: the
// earlier answer it was given relied on the syllabus, read for another
// question, which it may rest on and cannot name. One after an answer
// that relied on none says it relies on none.
func TestAFollowUpOnAnAnswerThatReliedOnMaterialsSaysNothing(t *testing.T) {
	w := newWorld(t)
	tut := w.tutor("tutor")
	m := scripted.New(
		scripted.CallTools(getDoc(w.co.SyllabusID)),
		scripted.Reply("Lectures are weekly, on Mondays."),
		scripted.Reply("Again: weekly, on Mondays."),
		scripted.Reply("Hello."),
		scripted.Reply("Hello again."),
	)
	w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{})
	conv, _ := w.ask(0, tut, "How often are the lectures?")
	if a := w.waitAnswers(conv, 1)[0]; sourcesOf(a) != w.source(w.co.SyllabusID) {
		t.Fatalf("the first answer relied on %s", sourcesOf(a))
	}
	_, err := w.fc.FollowUp(conv, "Say that again?")
	w.ok(err)
	if a := w.waitAnswers(conv, 2)[1]; a.Body != "Again: weekly, on Mondays." || sourcesOf(a) != "unsaid" {
		t.Errorf("the follow-up %q relied on %s; want it to say nothing", a.Body, sourcesOf(a))
	}

	conv2, _ := w.ask(1, tut, "Hi!")
	if a := w.waitAnswers(conv2, 1)[0]; sourcesOf(a) != "none" {
		t.Fatalf("the greeting relied on %s", sourcesOf(a))
	}
	_, err = w.fc.FollowUp(conv2, "Hi again!")
	w.ok(err)
	if a := w.waitAnswers(conv2, 2)[1]; a.Body != "Hello again." || sourcesOf(a) != "none" {
		t.Errorf("the follow-up %q relied on %s; want none", a.Body, sourcesOf(a))
	}
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
}

// A revision that reads none of the course's materials itself, of an
// answer that relied on the syllabus, says nothing of its sources: Mori
// sends the answer back asking only that it be shorter, and the revision,
// shown that answer whole as he read it, keeps what rests on the syllabus
// that answer read for its own attempt, which the revision cannot name.
// Written ahead, proposed and approved, it says nothing. A revision of an
// answer that relied on none, shown it too, says it relies on none.
func TestARevisionOfAnAnswerThatReliedOnMaterialsSaysNothing(t *testing.T) {
	w := newWorld(t)
	model := scripted.New(
		scripted.CallTools(getDoc(w.co.SyllabusID)), scripted.Reply("Lectures are weekly, on Mondays at 10, in room 4."),
		scripted.Reply("Weekly, on Mondays."),
		scripted.Reply("Hello."),
		scripted.Reply("Hello, and welcome to CS101."),
	)
	tu, wk := confirmedTutor(t, w, model, nil)
	revision := func(question, body, note string) (first, again fakecore.Proposal, conv string) {
		t.Helper()
		conv, msg := w.ask(0, tu, question)
		k1, k2 := core.AnswerKey(conv, msg, 1), core.AnswerKey(conv, msg, 2)
		first = w.waitProposal(k1)
		wk.waitAttempt("cs101-tutor", k1, store.AttemptProposed)
		w.ok(w.fc.RequestChanges(first.ActionID, note))
		again = w.waitProposal(k2)
		if again.Revises != first.ActionID {
			t.Errorf("the revision revises %q; want %s", again.Revises, first.ActionID)
		}
		if s := lastRequest(t, model).System; !strings.Contains(s, "## The answer you are writing again\n- A member of staff read your last answer "+
			"to this question before it was posted, and "+revisionSaid(note)+"\n"+answerRead(body)) {
			t.Errorf("the revision's prompt does not show the answer sent back, %q, with what was asked:\n%s", body, s)
		}
		if at := wk.waitAttempt("cs101-tutor", k2, store.AttemptProposed); sentSources(t, at.Args) != sentSources(t, again.Args) {
			t.Errorf("the revision written ahead relied on %s, and was proposed relying on %s", sentSources(t, at.Args), sentSources(t, again.Args))
		}
		return first, again, conv
	}

	p1, p2, conv := revision("How often are the lectures?", "Lectures are weekly, on Mondays at 10, in room 4.", "Make it shorter.")
	if got := sentSources(t, p1.Args); got != w.source(w.co.SyllabusID) {
		t.Fatalf("the first answer relied on %s", got)
	}
	if got := sentSources(t, p2.Args); got != "unsaid" {
		t.Errorf("the shorter revision relied on %s; want it to say nothing", got)
	}
	outcome, err := w.fc.Approve(p2.ActionID)
	w.ok(err)
	if outcome != "executed" {
		t.Fatalf("approval: %s", outcome)
	}
	if a := w.waitAnswers(conv, 1)[0]; a.Body != "Weekly, on Mondays." || sourcesOf(a) != "unsaid" {
		t.Errorf("the revision posted: %q, relying on %s; want it to say nothing", a.Body, sourcesOf(a))
	}

	p1, p2, _ = revision("Hi!", "Hello.", "Welcome them to the course.")
	if got := sentSources(t, p1.Args); got != "none" {
		t.Fatalf("the greeting relied on %s", got)
	}
	if got := sentSources(t, p2.Args); got != "none" {
		t.Errorf("the greeting's revision relied on %s; want none", got)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
}

// A revision that names nothing in revises, Core having refused what the
// one before it named (not_revisable: a rollback of Core made the answer
// sent back a rejection), is still told what was asked and shown the
// answer sent back, which relied on the syllabus; reading nothing itself,
// it says nothing of its sources, as does the refused one before it.
func TestARevisionNamingNothingOfAnAnswerThatReliedOnMaterialsSaysNothing(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	conv, msg := w.ask(0, tu, "How often are the lectures?")
	k1, k2, k3 := core.AnswerKey(conv, msg, 1), core.AnswerKey(conv, msg, 2), core.AnswerKey(conv, msg, 3)
	const body, note = "Lectures are weekly, on Mondays at 10, in room 4.", "Make it shorter."
	args, err := json.Marshal(core.AnswerArgs{CourseID: w.co.ID, ConversationID: conv, InReplyToMessageID: msg, Body: body,
		Sources: []core.Source{{DocumentID: w.co.SyllabusID, VersionID: w.versionOf(w.co.SyllabusID)}}, IdempotencyKey: k1})
	w.ok(err)
	caller := core.NewMCPCaller(core.MCPOptions{BaseURL: w.srv.URL, Token: tu.actor.Token, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	env, err := caller.Call(context.Background(), toolAnswer, args)
	if err != nil || env.Status != core.StatusProposed {
		t.Fatalf("the first answer: %+v, %v", env, err)
	}
	w.ok(w.fc.Reject(env.ActionID, "From before the rollback."))
	st := memstore.New()
	if _, err := st.PutAttempt(context.Background(), store.Attempt{Key: k1, AgentID: "cs101-tutor", MemberID: tu.seat.ID, CourseID: w.co.ID,
		ConversationID: conv, MessageID: msg, No: 1, Tool: toolAnswer, Args: args, Kind: kindModel, State: store.AttemptSending}); err != nil {
		t.Fatal(err)
	}
	w.ok(st.FinishAttempt(context.Background(), "cs101-tutor", k1, store.Outcome{State: store.AttemptChangesRequested, ActionID: env.ActionID,
		Reason: note}))
	model := scripted.New(scripted.Reply("Weekly, on Mondays."), scripted.Reply("Weekly, on Mondays at 10."))
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", map[string]any{"memory": map[string]any{"enabled": false}}, nil)),
		models{"m1": model}, workerOpts{store: st})
	if at := wk.waitAttempt("cs101-tutor", k2, store.AttemptError); at.Reason != core.ReasonNotRevisable || sentSources(t, at.Args) != "unsaid" {
		t.Errorf("the second attempt settled %s, %s, relying on %s; want refused, %s, saying nothing", at.State, at.Reason, sentSources(t, at.Args),
			core.ReasonNotRevisable)
	}
	p := w.waitProposal(k3)
	if p.Revises != "" || sentSources(t, p.Args) != "unsaid" {
		t.Errorf("the third attempt revises %q, relying on %s; want none, saying nothing", p.Revises, sentSources(t, p.Args))
	}
	if at := wk.waitAttempt("cs101-tutor", k3, store.AttemptProposed); sentSources(t, at.Args) != "unsaid" {
		t.Errorf("the third attempt was written ahead relying on %s; want it to say nothing", sentSources(t, at.Args))
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls; want 2", len(reqs))
	}
	if s := reqs[1].System; !strings.Contains(s, "- A member of staff read an earlier answer of yours to this question before it was posted, and "+
		revisionSaid(note)+"\n"+answerRead(body)) {
		t.Errorf("the third attempt's prompt does not show the answer sent back with what was asked:\n%s", s)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
}

// saidOf leaves out the sources of an answer that read none of the
// course's materials, where an earlier answer in the conversation, or the
// answer it writes again, relied on some or said nothing of them; one
// that read some names them whatever came before, and one that says
// nothing stays so.
func TestSaidOf(t *testing.T) {
	sent := func(sources []core.Source) []byte {
		args, err := json.Marshal(core.AnswerArgs{CourseID: "c", ConversationID: "x", InReplyToMessageID: "q", Body: "A",
			Sources: sources, IdempotencyKey: core.AnswerKey("x", "q", 1)})
		if err != nil {
			t.Fatal(err)
		}
		return args
	}
	syllabus := core.Source{DocumentID: "s", VersionID: "1"}
	read := func(earlier ...core.Message) *core.Messages {
		return &core.Messages{Messages: append(earlier, core.Message{ID: "q", AuthorMemberID: "student"})}
	}
	ownNone := core.Message{ID: "a", AuthorMemberID: "self", Sources: []core.SourceView{}}
	ownSyllabus := core.Message{ID: "a", AuthorMemberID: "self", Sources: []core.SourceView{{DocumentID: &syllabus.DocumentID}}}
	for _, tc := range []struct {
		name    string
		sources []core.Source
		read    *core.Messages
		redone  []byte
		want    string
	}{
		{"none read, nothing before", []core.Source{}, read(), nil, "none"},
		{"none read, after an answer that relied on none", []core.Source{}, read(ownNone), nil, "none"},
		{"none read, after an answer that relied on the syllabus", []core.Source{}, read(ownSyllabus), nil, "unsaid"},
		{"none read, writing again an answer that relied on the syllabus", []core.Source{}, read(), sent([]core.Source{syllabus}), "unsaid"},
		{"none read, writing again an answer that said nothing", []core.Source{}, read(), sent(nil), "unsaid"},
		{"none read, writing again an answer that relied on none", []core.Source{}, read(ownNone), sent([]core.Source{}), "none"},
		{"none read, writing again bytes that cannot be read", []core.Source{}, read(), []byte("{"), "unsaid"},
		{"the syllabus read, writing again an answer that said nothing", []core.Source{syllabus}, read(), sent(nil), "s@1//0/0/0"},
		{"only searched, writing again an answer that relied on none", nil, read(), sent([]core.Source{}), "unsaid"},
	} {
		got := saidOf(tc.sources, tc.read, "self", "q", tc.redone)
		var said fakecore.MessageRecord
		if said.SourcesStated = got != nil; got != nil {
			for _, s := range got {
				said.Sources = append(said.Sources, fakecore.SourceRecord{DocumentID: s.DocumentID, VersionID: s.VersionID})
			}
		}
		if sourcesOf(said) != tc.want {
			t.Errorf("%s: %s; want %s", tc.name, sourcesOf(said), tc.want)
		}
	}
}

// The runtime's own notices rely on no course material, whatever the
// model read first: on_budget_text, after the syllabus was read and the
// turns ran out; and the quota's notice, with no model call, once the
// asker's one answer of the day, which relied on the syllabus, is posted.
func TestTheRuntimesNoticesRelyOnNone(t *testing.T) {
	w := newWorld(t)
	tut := w.tutor("tutor")
	m := scripted.New(
		scripted.CallTools(getDoc(w.co.SyllabusID)), scripted.Stop(llm.StopEnd, ""),
		scripted.CallTools(getDoc(w.co.SyllabusID)), scripted.Reply("Weekly."),
	)
	over := map[string]any{
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 10, "turns": 2}, "per_asker_day": map[string]any{"answers": 1}},
	}
	wk := w.start(w.config(nil, w.agentDoc("tutor", "m", over, nil)), models{"m": m}, workerOpts{})
	conv, _ := w.ask(0, tut, "How often are the lectures?")
	if a := w.waitAnswers(conv, 1)[0]; a.Body != config.DefaultBudgetText || sourcesOf(a) != "none" {
		t.Errorf("the budget's notice %q relied on %s; want none", a.Body, sourcesOf(a))
	}
	conv2, _ := w.ask(0, tut, "How often, again?")
	if a := w.waitAnswers(conv2, 1)[0]; a.Body != "Weekly." || sourcesOf(a) != w.source(w.co.SyllabusID) {
		t.Fatalf("the answer %q relied on %s", a.Body, sourcesOf(a))
	}
	eventually(t, "the answer in the ledger", func() bool { return len(wk.st.outcomes(conv2)) == 1 })
	conv3, _ := w.ask(0, tut, "And the exam?")
	if a := w.waitAnswers(conv3, 1)[0]; a.Body != config.DefaultQuotaText || sourcesOf(a) != "none" {
		t.Errorf("the quota's notice %q relied on %s; want none", a.Body, sourcesOf(a))
	}
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
}

// reposts counts the attempts that post the answer before them again
// without a source Core refused: the same answer under the next number,
// one source fewer or none said, after one Core refused as invalid. A
// body written again, or one refused otherwise, is another attempt.
func TestReposts(t *testing.T) {
	answer := func(no int, body string, sources []core.Source, code string) store.Attempt {
		args, err := json.Marshal(core.AnswerArgs{CourseID: "c", ConversationID: "x", InReplyToMessageID: "m", Body: body, Sources: sources,
			IdempotencyKey: core.AnswerKey("x", "m", no)})
		if err != nil {
			t.Fatal(err)
		}
		return store.Attempt{No: no, Tool: toolAnswer, Kind: kindModel, Args: args, ErrorCode: code}
	}
	a, b, c := core.Source{DocumentID: "a", VersionID: "1"}, core.Source{DocumentID: "b", VersionID: "1"}, core.Source{DocumentID: "c", VersionID: "1"}
	inv := core.CodeInvalidArgument
	for _, tc := range []struct {
		name string
		atts []store.Attempt
		want int
	}{
		{"one source dropped, then the last", []store.Attempt{answer(1, "A", []core.Source{a, b}, inv), answer(2, "A", []core.Source{a}, inv),
			answer(3, "A", nil, "")}, 2},
		{"every source dropped at once", []store.Attempt{answer(1, "A", []core.Source{a, b, c}, inv), answer(2, "A", nil, "")}, 1},
		{"an empty list dropped", []store.Attempt{answer(1, "A", []core.Source{}, inv), answer(2, "A", nil, "")}, 1},
		{"written again by the model", []store.Attempt{answer(1, "A", []core.Source{a, b}, inv), answer(2, "B", []core.Source{a}, "")}, 0},
		{"refused otherwise", []store.Attempt{answer(1, "A", []core.Source{a, b}, core.CodeIdempotencyConflict), answer(2, "A", []core.Source{a}, "")}, 0},
		{"the same sources", []store.Attempt{answer(1, "A", []core.Source{a, b}, inv), answer(2, "A", []core.Source{a, b}, "")}, 0},
		{"none said before", []store.Attempt{answer(1, "A", nil, inv), answer(2, "A", nil, "")}, 0},
		{"not the next number", []store.Attempt{answer(1, "A", []core.Source{a, b}, inv), answer(3, "A", []core.Source{a}, "")}, 0},
	} {
		if got := reposts(tc.atts); got != tc.want {
			t.Errorf("%s: %d; want %d", tc.name, got, tc.want)
		}
	}
}
