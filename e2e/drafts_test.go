package e2e

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// draftView is a conversation's draft as a reader of it sees it.
type draftView struct {
	Attempt    string           `json:"attempt"`
	Version    int64            `json:"version"`
	Steps      []core.DraftStep `json:"steps"`
	Text       *string          `json:"text"`
	TextHidden bool             `json:"text_hidden"`
}

// watched is what p saw of conv while watching it: each draft, as it
// changed, and the answer the member author posted, which ended the watch.
type watched struct {
	drafts []draftView
	answer message
}

// watch reads conv as p until the member author has answered in it, as
// the site's conversation view does: long polls of conversation_messages
// that wait for the next message, or for the draft to change
// (seen_draft_version).
func (w *world) watch(t *testing.T, p person, conv, author string) watched {
	t.Helper()
	var out watched
	var seen int64
	deadline := time.Now().Add(answerWait)
	for time.Now().Before(deadline) {
		path := w.path(fmt.Sprintf("/conversations/%s/messages?limit=50&wait_s=5&seen_draft_version=%d", conv, seen))
		r, err := w.api.send(context.Background(), p.token, http.MethodGet, path, nil, "")
		if err != nil || r.HTTP != http.StatusOK {
			t.Fatalf("%s reading the conversation: %v %s", p.name, err, r)
		}
		read := decode[struct {
			Messages []message  `json:"messages"`
			Draft    *draftView `json:"draft"`
		}](t, r)
		for _, m := range read.Messages {
			if m.AuthorMemberID == author {
				out.answer = m
				if read.Draft != nil {
					t.Errorf("%s read the answer and a draft beside it: %+v", p.name, *read.Draft)
				}
				return out
			}
		}
		seen = 0
		if d := read.Draft; d != nil {
			seen = d.Version
			if n := len(out.drafts); n == 0 || out.drafts[n-1].Attempt != d.Attempt || out.drafts[n-1].Version != d.Version {
				out.drafts = append(out.drafts, *d)
			}
		}
	}
	t.Fatalf("%s saw no answer in %s", p.name, answerWait)
	return out
}

// draftsShown: where Core takes the drafts of answers (conversation.draft),
// Yuki watches her agent's answer come. The runtime streams the model's
// answer (the model's server sends it a few words every 300 ms), and Yuki,
// long-polling the conversation for the draft's next version, sees what the
// agent does (thinking, a tool, writing) and the answer's text growing,
// each a prefix of the next; then the posted answer, whole, in its place,
// and no draft beside it. At confirm_required, the course tutor's steps are
// shown to Ken, who asked, without their text, which Mori, who decides the
// tutor's answers, reads.
func draftsShown(t *testing.T, w *world) {
	if !w.api.takesDrafts(t) {
		t.Skip("this Core's catalogue has no conversation.draft: it shows no drafts")
	}
	m := newModel(t, fakellm.DefaultResponder).StreamEvery(300 * time.Millisecond)
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own}, {id: "tutor", seat: w.tutor}}})
	rt.waitPolling("yuki-helper")

	question := "About the homework: how should I structure the loop in question two so that it ends when the list is empty?"
	conv, _ := w.ask(t, w.yuki, w.own.member, question)
	got := w.watch(t, w.yuki, conv, w.own.member)
	want := "Answer: " + question + " (from 1 tool result)"
	if got.answer.text() != want {
		t.Errorf("Yuki's answer %q, want %q", got.answer.text(), want)
	}
	var texts []string
	var kinds []string
	for _, d := range got.drafts {
		if d.TextHidden {
			t.Errorf("Yuki's own agent's answers post without confirmation, and its draft's text is hidden from her: %+v", d)
		}
		if d.Text != nil && *d.Text != "" {
			texts = append(texts, *d.Text)
		}
		for _, s := range d.Steps {
			if !slices.Contains(kinds, s.Kind) {
				kinds = append(kinds, s.Kind)
			}
		}
	}
	t.Logf("Yuki saw %d drafts, %d with text, of steps %v", len(got.drafts), len(texts), kinds)
	if len(texts) < 3 {
		t.Errorf("Yuki saw the answer's text %d times as it came; want it growing: %q", len(texts), texts)
	}
	for i, text := range texts {
		if !strings.HasPrefix(want, text) || (i > 0 && (!strings.HasPrefix(text, texts[i-1]) || text == texts[i-1])) {
			t.Errorf("text %d %q is not the answer so far, after %q", i, text, texts[max(i-1, 0)])
		}
	}
	for _, kind := range []string{"thinking", "tool", "writing"} {
		if !slices.Contains(kinds, kind) {
			t.Errorf("no draft showed a %s step: %v", kind, kinds)
		}
	}
	if n := rt.metric("draft_writes_total", map[string]string{"agent": "yuki-helper", "outcome": "sent"}); n < float64(len(got.drafts)) {
		t.Errorf("draft_writes_total{sent} = %v; Yuki saw %d drafts", n, len(got.drafts))
	}

	// The tutor at confirm_required: Ken sees its steps alone; Mori, who
	// would approve its answer, the text as well.
	w.setAnswerLevel(t, "confirm_required")
	question = "When is homework three due, and does the deadline move if the lab closes early that week?"
	conv, _ = w.ask(t, w.ken, w.tutor.member, question)
	hidden, shown := false, false
	eventually(t, answerWait, "the tutor's draft read by Ken and by Mori", func() bool {
		for _, look := range []struct {
			p   person
			see *bool
		}{{w.ken, &hidden}, {w.mori, &shown}} {
			d := w.conversationDraft(t, look.p, conv)
			switch {
			case d == nil || len(d.Steps) == 0:
			case look.p.name == "Ken":
				if d.Text != nil {
					t.Fatalf("Ken read the text of an answer waiting to be approved: %+v", *d)
				}
				*look.see = *look.see || d.TextHidden
			default:
				*look.see = *look.see || (d.Text != nil && *d.Text != "")
			}
		}
		return hidden && shown
	})
	w.waitPending(t, w.ken, conv, "")
	if d := w.conversationDraft(t, w.ken, conv); d != nil {
		t.Errorf("a draft is left beside the answer proposed: %+v", *d)
	}
}

// conversationDraft is conv's draft as p reads it with conversation_get.
func (w *world) conversationDraft(t *testing.T, p person, conv string) *draftView {
	t.Helper()
	return result[struct {
		Draft *draftView `json:"draft"`
	}](t, w.api, p.token, "GET", w.path("/conversations/"+conv), nil).Draft
}

// takesDrafts reports whether the Core under test takes the drafts of
// answers: its catalogue has conversation.draft.
func (c *coreAPI) takesDrafts(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	cat, err := core.FetchCatalogue(ctx, c.hc, c.base)
	if err != nil {
		t.Fatal(err)
	}
	return cat.Drafts()
}
