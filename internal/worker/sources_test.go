package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

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
