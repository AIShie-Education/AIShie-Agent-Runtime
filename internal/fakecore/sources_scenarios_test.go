package fakecore

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// sources is what an answer relied on (AIShie-Core #71): the tutor's
// answer refused for its sources as they are read (one named twice, a
// page of no file, more than 20), and before anything is posted for one
// it may not read (a document that is none, a file of none of the
// version's), recorded as failed; then posted with the syllabus and a
// page of a lecture's file, which its opener reads whole; an answer that
// relied on none, read as an empty list; and one that does not say, read
// with no sources.
var sources = scenario{name: "sources", about: "an answer's sources: refused as they are read (named twice, a page of no file, " +
	"more than 20), refused before anything is posted naming the one the tutor may not read (a document that is none, a file of " +
	"none of the version's), recorded as failed; posted with a version and a page of a file, which the opener reads whole; an " +
	"empty list, read as one; and none, read with no sources",
	run: func(t *testing.T, w world, s *steps) {
		syllabus := w.material()
		lecture, files := w.officeMaterial("Lecture 2", namedFile{"week2.txt", "text/plain", []byte("Week 2: variables and types.\n")})
		// The versions, as the tutor reads them: not recorded, as what
		// the fake's canned syllabus says is the fake's.
		version := func(doc string) string {
			t.Helper()
			a, err := w.agent().call(context.Background(), "document_get", inCourseArgs(w, "document_id", doc))
			if err != nil {
				t.Fatal(err)
			}
			wantStatus(t, a, "executed")
			return a.str("result", "version", "id")
		}
		sylV, lecV := version(syllabus), version(lecture)
		syl := map[string]any{"document_id": syllabus, "version_id": sylV}
		page := func(n int) map[string]any {
			return map[string]any{"document_id": lecture, "version_id": lecV, "file_id": files[0], "page": n}
		}
		with := func(args map[string]any, sources ...map[string]any) map[string]any {
			args["sources"] = append([]map[string]any{}, sources...)
			return args
		}

		conv, m1 := w.ask(0, "What does week 2 cover?")
		const body = "Variables and types (Syllabus; Lecture 2, page 1)."
		call(t, w, s, "named_twice", "conversation_answer", with(answer(w, conv, m1, body, 1), syl, page(1), syl))
		call(t, w, s, "page_of_no_file", "conversation_answer", with(answer(w, conv, m1, body, 1),
			map[string]any{"document_id": lecture, "version_id": lecV, "page": 1}))
		many := make([]map[string]any, 21)
		for i := range many {
			many[i] = page(i + 1)
		}
		call(t, w, s, "too_many", "conversation_answer", with(answer(w, conv, m1, body, 1), many...))
		call(t, w, s, "not_a_document", "conversation_answer", with(answer(w, conv, m1, body, 1), syl,
			map[string]any{"document_id": uuid.NewString(), "version_id": uuid.NewString()}))
		call(t, w, s, "file_of_no_version", "conversation_answer", with(answer(w, conv, m1, body, 2),
			map[string]any{"document_id": lecture, "version_id": lecV, "file_id": uuid.NewString()}))
		wantStatus(t, call(t, w, s, "posted", "conversation_answer", with(answer(w, conv, m1, body, 3), syl, page(1))), "executed")
		yuki := w.as("yuki")
		callAs(t, yuki, s, "read", "conversation_messages", inCourseArgs(w, "conversation_id", conv))

		none, m2 := w.ask(0, "Thank you!")
		wantStatus(t, call(t, w, s, "relied_on_none", "conversation_answer", with(answer(w, none, m2, "You are welcome.", 1))), "executed")
		callAs(t, yuki, s, "read_none", "conversation_messages", inCourseArgs(w, "conversation_id", none))
		silent, m3 := w.ask(0, "Hello?")
		wantStatus(t, call(t, w, s, "did_not_say", "conversation_answer", answer(w, silent, m3, "Hello.", 1)), "executed")
		callAs(t, yuki, s, "read_silent", "conversation_messages", inCourseArgs(w, "conversation_id", silent))
		call(t, w, s, "mine", "action_list_mine", inCourseArgs(w, "exclude_types", []string{"conversation.ask", "conversation.open"}))
	}}
