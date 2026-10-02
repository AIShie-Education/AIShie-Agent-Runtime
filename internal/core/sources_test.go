package core

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// An answer's sources go as conversation_answer takes them (§2.10): an
// empty list says it relied on none, and none at all says nothing; a
// source names its file, page, slide and part only where it has them.
// What the runtime sends is what the pinned catalogue's schema takes.
func TestAnswerArgsSayWhatTheAnswerReliedOn(t *testing.T) {
	raw, err := os.ReadFile("testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	answer, ok := cat.Tool("conversation_answer")
	if !ok {
		t.Fatal("no conversation_answer in the catalogue")
	}
	const (
		course = "0192f3c1-7d2e-7b4a-9c3d-2e1f0a9b8c7d"
		doc    = "0192f3c1-aaaa-7b4a-9c3d-2e1f0a9b8c7d"
	)
	for _, c := range []struct {
		name    string
		sources []Source
		want    string
	}{
		{"unsaid", nil, `"body":"b","idempotency_key":"k"}`},
		{"none", []Source{}, `"body":"b","sources":[],"idempotency_key":"k"}`},
		{"a version and a page", []Source{{DocumentID: doc, VersionID: doc}, {DocumentID: doc, VersionID: doc, FileID: doc, Page: 3, Part: 2}},
			`"sources":[{"document_id":"` + doc + `","version_id":"` + doc + `"},{"document_id":"` + doc + `","version_id":"` + doc +
				`","file_id":"` + doc + `","page":3,"part":2}],"idempotency_key":"k"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(AnswerArgs{CourseID: course, ConversationID: course, InReplyToMessageID: course, Body: "b",
				Sources: c.sources, IdempotencyKey: "k"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(b), c.want) {
				t.Errorf("%s; want it to end %s", b, c.want)
			}
			// The key goes beside the tool's own arguments (§2.2).
			var args map[string]any
			_ = json.Unmarshal(b, &args)
			delete(args, "idempotency_key")
			own, _ := json.Marshal(args)
			if err := toolschema.Validate(answer.InputSchema, own); err != nil {
				t.Errorf("Core's schema refuses %s: %v", own, err)
			}
		})
	}
}
