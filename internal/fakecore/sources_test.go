package fakecore

import (
	"fmt"
	"strings"
	"testing"
)

// sourcesRead are the sources of the newest message of conv as c reads
// them, each as "title", "restricted" or "other_version": "unsaid" where
// the message says nothing of them.
func sourcesRead(t *testing.T, w *fakeWorld, c *mcpClient, conv string) string {
	t.Helper()
	msgs := list(mustCall(t, c, "conversation_messages", inCourseArgs(w, "conversation_id", conv)), "messages")
	last, _ := msgs[len(msgs)-1].(map[string]any)
	raw, said := last["sources"].([]any)
	if !said {
		return "unsaid"
	}
	out := []string{}
	for _, s := range raw {
		v, _ := s.(map[string]any)
		switch {
		case v["restricted"] == true:
			out = append(out, "restricted")
		case v["other_version"] == true:
			out = append(out, "other_version")
		default:
			out = append(out, fmt.Sprint(v["title"]))
		}
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// versionOf is a document's version, as the fake holds it.
func (w *fakeWorld) versionOf(id string) string {
	for _, d := range w.fc.Documents(w.co.ID) {
		if d.ID == id {
			return d.VersionID
		}
	}
	w.t.Fatalf("no document %s", id)
	return ""
}

// An answer's sources are kept with it, and shown each reader as they may
// read the documents now: the HW1 instructions, once HW1 is unpublished,
// are restricted to the student who asked and whole to Sato, who writes
// assignments; the syllabus, purged, is restricted to both. A retracted
// answer's sources are withheld with it. The records keep them as named.
func TestAnAnswersSourcesAreReadAsTheReaderMay(t *testing.T) {
	w := newFakeWorld(t, Options{})
	syl := map[string]any{"document_id": w.co.SyllabusID, "version_id": w.versionOf(w.co.SyllabusID)}
	instr := map[string]any{"document_id": w.co.InstructionsID, "version_id": w.versionOf(w.co.InstructionsID)}
	conv, m1 := w.ask(0, "What is HW1?")
	args := answer(w, conv, m1, "Chapter 1.", 1)
	args["sources"] = []map[string]any{syl, instr}
	wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", args), "executed", "", "")
	yuki, sato := w.as("yuki"), w.as("sato")
	for who, c := range map[string]*mcpClient{"Yuki": yuki, "Sato": sato} {
		if got := sourcesRead(t, w, c, conv); got != "[Syllabus, HW1 instructions]" {
			t.Errorf("%s reads %s", who, got)
		}
	}
	rec := w.fc.Answers(conv)[0]
	if !rec.SourcesStated || len(rec.Sources) != 2 || rec.Sources[1] != (SourceRecord{DocumentID: w.co.InstructionsID, VersionID: w.versionOf(w.co.InstructionsID)}) {
		t.Errorf("the record: %+v", rec)
	}

	w.ok(w.fc.UnpublishAssignment(w.co.ID, w.co.AssignmentID))
	if got := sourcesRead(t, w, yuki, conv); got != "[Syllabus, restricted]" {
		t.Errorf("Yuki reads %s once HW1 is unpublished", got)
	}
	if got := sourcesRead(t, w, sato, conv); got != "[Syllabus, HW1 instructions]" {
		t.Errorf("Sato reads %s once HW1 is unpublished", got)
	}
	w.ok(w.fc.PurgeVersion(w.co.SyllabusID))
	if got := sourcesRead(t, w, yuki, conv); got != "[restricted, restricted]" {
		t.Errorf("Yuki reads %s once the syllabus is purged", got)
	}
	if got := sourcesRead(t, w, sato, conv); got != "[restricted, HW1 instructions]" {
		t.Errorf("Sato reads %s once the syllabus is purged", got)
	}

	w.ok(w.fc.Retract(rec.ID, w.tutorM.ID, "wrong"))
	if got := sourcesRead(t, w, yuki, conv); got != "unsaid" {
		t.Errorf("a retracted answer's sources: %s", got)
	}
}

// A source is checked before the answer is proposed, and again when the
// proposal is approved, as the tutor's: the HW1 instructions, which it
// may no longer read once HW1 is unpublished, fail the approval; a purged
// syllabus is refused as purged; and a file of none of the version's as
// unreadable.
func TestSourcesAreCheckedWhenAProposalIsApproved(t *testing.T) {
	w := newFakeWorld(t, Options{})
	w.setTutorLevel("confirm_required")
	instr := map[string]any{"document_id": w.co.InstructionsID, "version_id": w.versionOf(w.co.InstructionsID)}
	conv, m1 := w.ask(0, "What is HW1?")
	args := answer(w, conv, m1, "Chapter 1.", 1)
	args["sources"] = []map[string]any{instr}
	a := mustCall(t, w.agentC, "conversation_answer", args)
	wantEnvelope(t, a, "proposed", "", "")
	w.ok(w.fc.UnpublishAssignment(w.co.ID, w.co.AssignmentID))
	if out, err := w.fc.Approve(a.str("action_id")); err != nil || out != "failed" {
		t.Errorf("the approval: %s, %v", out, err)
	}
	if n := len(w.fc.Answers(conv)); n != 0 {
		t.Errorf("%d answers posted", n)
	}

	w.setTutorLevel("autonomous")
	slides := map[string]any{"document_id": w.co.SlidesID, "version_id": w.versionOf(w.co.SlidesID), "file_id": w.co.SyllabusID}
	args = answer(w, conv, m1, "Chapter 1.", 2)
	args["sources"] = []map[string]any{slides}
	a = mustCall(t, w.agentC, "conversation_answer", args)
	wantEnvelope(t, a, "failed", codeInvalidArgument, reasonSourceUnreadable)
	if a.str("error", "details", "field") != "sources[0]" || a.str("action_id") == "" {
		t.Errorf("a file of none of the version's: %s", a.Text)
	}
	w.ok(w.fc.PurgeVersion(w.co.SyllabusID))
	args = answer(w, conv, m1, "Chapter 1.", 3)
	args["sources"] = []map[string]any{{"document_id": w.co.SyllabusID, "version_id": w.versionOf(w.co.SyllabusID)}}
	wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", args), "failed", codeInvalidArgument, reasonSourcePurged)
}

// A Core from before an answer's sources (WithoutSources) lists none in
// its catalogue, and refuses an answer that gives them, an empty list
// too, as its schema refuses any argument it does not name; one that
// gives none it posts.
func TestACoreFromBeforeSources(t *testing.T) {
	w := newFakeWorld(t, Options{WithoutSources: true})
	answerTool := w.fc.cat.byName[toolConversationAnswer]
	if strings.Contains(string(answerTool.InputSchema), `"sources"`) {
		t.Fatal("the catalogue still has conversation.answer take sources")
	}
	conv, m1 := w.ask(0, "What is HW1?")
	args := answer(w, conv, m1, "Chapter 1.", 1)
	args["sources"] = []map[string]any{}
	a := mustCall(t, w.agentC, "conversation_answer", args)
	wantEnvelope(t, a, "error", codeInvalidArgument, "")
	if !strings.Contains(a.str("error", "message"), `"sources"`) {
		t.Errorf("the refusal: %s", a.Text)
	}
	wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "Chapter 1.", 2)), "executed", "", "")
	if got := sourcesRead(t, w, w.as("yuki"), conv); got != "unsaid" {
		t.Errorf("Yuki reads %s", got)
	}
}
