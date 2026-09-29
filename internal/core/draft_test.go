package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCatalogueDrafts: a Core takes drafts when its catalogue offers
// conversation_draft as an ephemeral write; the pinned one's does not.
func TestCatalogueDrafts(t *testing.T) {
	if testCatalogue(t).Drafts() || (*Catalogue)(nil).Drafts() {
		t.Error("the snapshot, or no catalogue, takes drafts")
	}
	for kind, want := range map[string]bool{KindEphemeral: true, KindWrite: false, KindRead: false} {
		raw := `{"tools":[{"name":"conversation.draft","kind":"` + kind + `","method":"POST","path":"/v1/courses/{course_id}/conversations/{conversation_id}/draft","input_schema":{},"output_schema":{}}]}`
		cat, err := ParseCatalogue([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := cat.Drafts(); got != want {
			t.Errorf("conversation.draft of kind %s: drafts %v", kind, got)
		}
	}
}

// TestDraftCall: Draft sends conversation_draft best effort, with no
// idempotency key, text and steps left out when unset; over REST, at its
// route with no Idempotency-Key.
func TestDraftCall(t *testing.T) {
	var args json.RawMessage
	var best bool
	c := NewClient(callerFunc(func(ctx context.Context, tool string, a json.RawMessage) (*Envelope, error) {
		args, best = a, BestEffort(ctx)
		return &Envelope{Status: StatusExecuted}, nil
	}))
	if _, err := c.Draft(context.Background(), DraftArgs{CourseID: "c", ConversationID: "v", Attempt: "a", Version: 2, Done: true}); err != nil {
		t.Fatal(err)
	}
	if string(args) != `{"course_id":"c","conversation_id":"v","attempt":"a","version":2,"done":true}` || !best {
		t.Errorf("sent %s, best effort %v", args, best)
	}

	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"executed","result":{"stored":true,"version":1}}`)
	}))
	defer srv.Close()
	cat, err := ParseCatalogue([]byte(`{"tools":[{"name":"conversation.draft","kind":"ephemeral","method":"POST",` +
		`"path":"/v1/courses/{course_id}/conversations/{conversation_id}/draft","input_schema":{},"output_schema":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewClient(NewRESTCaller(RESTOptions{BaseURL: srv.URL, Token: "tok", Catalogue: cat}))
	text := "HW1 is due"
	env, err := rc.Draft(context.Background(), DraftArgs{CourseID: "c1", ConversationID: "v1", Attempt: "a", Version: 1, Text: &text,
		Steps: []DraftStep{{Kind: StepThinking, State: StepRunning}}})
	if err != nil || !env.OK() {
		t.Fatalf("%v %v", env, err)
	}
	if got.URL.Path != "/v1/courses/c1/conversations/v1/draft" || got.Header.Get("Idempotency-Key") != "" ||
		string(body) != `{"attempt":"a","version":1,"text":"HW1 is due","steps":[{"kind":"thinking","state":"running"}]}` {
		t.Errorf("%s %s %q: %s", got.Method, got.URL.Path, got.Header.Get("Idempotency-Key"), body)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
