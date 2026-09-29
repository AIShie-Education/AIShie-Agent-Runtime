package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// The drafts of answers, against the fake Core as the pinned one, which
// takes them: what the asker sees while the tutor works.

// newDraftWorld is a world whose Core takes drafts, as the pinned one does.
func newDraftWorld(t *testing.T) *world {
	t.Helper()
	return newWorld(t)
}

// draftsEvery writes drafts every d, for tests.
func draftsEvery(d time.Duration) workerOpts {
	return workerOpts{edit: func(o *Options) { o.Timing.DraftEvery = d }}
}

// TestDraftShowsStepsAndStreamedText: the model reads the syllabus, then
// streams its answer. The draft says it thought, then read the syllabus
// (by its title), then shows the answer's text growing, one attempt with
// its versions rising; the posted answer takes its place, and nothing of
// the tool's result or the system prompt was ever in it.
func TestDraftShowsStepsAndStreamedText(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	answer := []string{"The syllabus", " says there are", " weekly lectures,", " one assignment a fortnight", " and a final exam."}
	model := scripted.New(
		slowly(scripted.CallTool("document_get", `{"document_id":"`+w.co.SyllabusID+`"}`)),
		scripted.Streamed(60*time.Millisecond, answer...),
	)
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(20*time.Millisecond))
	wk.waitState("yuki-helper", store.AgentRunning)

	conv, _ := w.ask(0, own, "What does the syllabus say about the exam?")
	posted := w.waitAnswers(conv, 1)[0]
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	full := strings.Join(answer, "")
	if posted.Body != full {
		t.Errorf("posted %q", posted.Body)
	}
	if _, ok := w.fc.Draft(conv); ok {
		t.Error("the draft is still there once the answer is posted")
	}

	writes := w.fc.DraftWrites(conv)
	if len(writes) < 4 {
		t.Fatalf("%d drafts written", len(writes))
	}
	first := writes[0]
	if first.Text != nil || !slices.Equal(first.Steps, []fakecore.DraftStep{{Kind: "thinking", State: "running"}}) {
		t.Errorf("the first draft: %+v", first)
	}
	read, texts := false, []string{}
	for i, d := range writes {
		if !d.Stored || d.Attempt != first.Attempt || d.Version != int64(i+1) || d.Done || d.At.After(posted.CreatedAt) {
			t.Errorf("draft %d: %+v", i, d)
		}
		for _, s := range d.Steps {
			if s.Kind == "reading_document" && s.Target != nil && *s.Target == "Syllabus" && s.State == "done" {
				read = true
			}
		}
		if d.Text != nil && *d.Text != "" {
			texts = append(texts, *d.Text)
		}
		b, _ := json.Marshal(d)
		if strings.Contains(string(b), "Weekly lectures, one assignment") || strings.Contains(string(b), "Messages and tool results") {
			t.Errorf("draft %d carries the tool's result or the system prompt: %s", i, b)
		}
	}
	if !read {
		t.Errorf("no draft says the syllabus was read: %+v", writes)
	}
	if len(texts) < 2 {
		t.Errorf("the text did not grow: %q", texts)
	}
	for i, text := range texts {
		if !strings.HasPrefix(full, text) || (i > 0 && !strings.HasPrefix(text, texts[i-1])) {
			t.Errorf("text %d %q is not the answer so far", i, text)
		}
	}
	last := writes[len(writes)-1].Steps
	if n := len(last); n < 4 || last[n-1].Kind != "writing" || last[n-1].State != "running" {
		t.Errorf("the last draft's steps: %+v", last)
	}
	calls := w.calls(own.actor.ID, core.ToolDraft)
	for i, c := range calls {
		// The last may have been on its way as the answer went in, and
		// found the conversation no longer waiting for it.
		late := i == len(calls)-1 && c.Code == "conflict"
		if (c.Status != "executed" && !late) || strings.Contains(string(c.Args), "idempotency_key") {
			t.Errorf("a draft call %+v", c)
		}
	}
	eventually(t, "the draft writes counted", func() bool {
		return counter(t, wk.reg, "draft_writes_total", map[string]string{"agent": "yuki-helper", "outcome": "sent"}) == float64(len(writes))
	})
	st := agentStatus(t, wk, "yuki-helper")
	if !st.Drafts || st.DraftWrites == nil || st.DraftWrites.Sent != int64(len(writes)) || st.DraftWrites.Failed != 0 {
		t.Errorf("status: drafts %v, %+v", st.Drafts, st.DraftWrites)
	}
}

// adapter answers every agent's model with ad, writing drafts every
// every when it is not zero.
func adapter(ad llm.Adapter, every time.Duration) workerOpts {
	return workerOpts{edit: func(o *Options) {
		o.NewAdapter = func(llm.Config) (llm.Adapter, error) { return ad, nil }
		if every > 0 {
			o.Timing.DraftEvery = every
		}
	}}
}

// agentStatus is the worker's status of agent id.
func agentStatus(t *testing.T, wk *worker, id string) AgentStatus {
	t.Helper()
	for _, st := range wk.sup.Status() {
		if st.AgentID == id {
			return st
		}
	}
	t.Fatalf("no status for %s", id)
	return AgentStatus{}
}

// TestNoDraftsWithoutTheTool: against a Core whose catalogue has no
// conversation_draft (one from before drafts), not one draft is written,
// and the model's call is not streamed.
func TestNoDraftsWithoutTheTool(t *testing.T) {
	w := newWorldWith(t, fakecore.Options{WithoutDraft: true})
	own := w.ownAgent("yuki-helper", 0)
	told := false
	model := scripted.New(func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		scripted.TextOf(ctx)("x")
		return scripted.Reply("On Friday.")(ctx, req)
	})
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), nil, adapter(streamSpy{model, &told}, 0))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	w.waitAnswers(conv, 1)
	if calls := w.calls(own.actor.ID, core.ToolDraft); len(calls) != 0 {
		t.Errorf("%d drafts written to a Core without the tool", len(calls))
	}
	if told {
		t.Error("the model's call was streamed, with nothing to show it")
	}
	if st := agentStatus(t, wk, "yuki-helper"); st.Drafts || st.DraftWrites != nil {
		t.Errorf("status: drafts %v, %+v", st.Drafts, st.DraftWrites)
	}
}

// streamSpy is an adapter that notes whether it was asked to stream.
type streamSpy struct {
	*scripted.Adapter
	streamed *bool
}

func (s streamSpy) Stream(ctx context.Context, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	*s.streamed = true
	return s.Adapter.Stream(ctx, req, onText)
}

// TestDraftEndsWhenTheProvidersFail: the model's provider fails every try,
// part way through streaming; the text of each try starts from nothing,
// and the attempt, given up, ends its draft with done.
func TestDraftEndsWhenTheProvidersFail(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	cut := &llm.Error{Kind: llm.ErrNetwork, Message: "the stream ended before the answer did"}
	var steps []scripted.Step
	for range modelTries {
		steps = append(steps, scripted.StreamedThenFail(40*time.Millisecond, cut, "HW3 is", " due"))
	}
	model := scripted.New(steps...)
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	eventually(t, "the draft ended", func() bool {
		ws := w.fc.DraftWrites(conv)
		return len(ws) > 0 && ws[len(ws)-1].Done
	})
	ws := w.fc.DraftWrites(conv)
	attempt := ws[0].Attempt
	var texts []string
	for i, d := range ws {
		if d.Attempt != attempt {
			break
		}
		if i > 0 && d.Version <= ws[i-1].Version {
			t.Errorf("draft %d's version %d after %d", i, d.Version, ws[i-1].Version)
		}
		if d.Text != nil {
			texts = append(texts, *d.Text)
		}
	}
	// Each try's text started again: a text shown after another is not
	// the other written on.
	again := false
	for i := 1; i < len(texts); i++ {
		if !strings.HasPrefix(texts[i], texts[i-1]) {
			again = true
		}
	}
	if !again {
		t.Errorf("the text never started again: %q", texts)
	}
	if len(w.answers(conv)) != 0 {
		t.Error("an answer was posted")
	}
	if _, ok := w.fc.Draft(conv); ok {
		t.Error("the draft of the attempt given up is still there")
	}
}

// TestDraftTextStartsAgainAfterABrokenStream: a stream cut off is tried
// again, in the same attempt; what the draft shows at the end is the text
// of the try that answered.
func TestDraftTextStartsAgainAfterABrokenStream(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	cut := &llm.Error{Kind: llm.ErrNetwork, Message: "the stream ended before the answer did"}
	model := scripted.New(
		scripted.StreamedThenFail(50*time.Millisecond, cut, "The dead", "line is"),
		scripted.Streamed(50*time.Millisecond, "HW3 is due", " on Friday", " at noon."),
	)
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	if got := w.waitAnswers(conv, 1)[0].Body; got != "HW3 is due on Friday at noon." {
		t.Errorf("posted %q", got)
	}
	ws := w.fc.DraftWrites(conv)
	broken, lastText := false, ""
	for _, d := range ws {
		if d.Attempt != ws[0].Attempt {
			t.Errorf("a try again is a new attempt: %+v", d)
		}
		if d.Text != nil {
			if strings.HasPrefix(*d.Text, "The dead") {
				broken = true
			}
			lastText = *d.Text
		}
	}
	if !broken {
		t.Errorf("the broken try's text was never shown: %+v", ws)
	}
	if !strings.HasPrefix("HW3 is due on Friday at noon.", lastText) || lastText == "" {
		t.Errorf("the last text shown is %q", lastText)
	}
}

// TestDraftWritesNeverHoldTheAnswer: Core takes two seconds over each
// draft; the answer is posted as soon as the model has written it, and
// the drafts are sent one at a time meanwhile.
func TestDraftWritesNeverHoldTheAnswer(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Streamed(20*time.Millisecond, "On", " Friday."))
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == core.ToolDraft {
			return &fakecore.Injection{Delay: 2 * time.Second}
		}
		return nil
	})
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	w.answersInSite(own)
	asked := time.Now()
	conv, _ := w.ask(0, own, "When is HW3 due?")
	w.waitAnswers(conv, 1)
	if took := time.Since(asked); took > 1500*time.Millisecond {
		t.Errorf("the answer took %s, behind its drafts", took)
	}
	eventually(t, "the draft in flight answered", func() bool { return len(w.calls(own.actor.ID, core.ToolDraft)) >= 1 })
	if n := len(w.calls(own.actor.ID, core.ToolDraft)); n > 2 {
		t.Errorf("%d drafts sent while one took two seconds", n)
	}
}

// TestDraftsTooSoonAreDropped: Core refuses every draft as too soon (its
// 429); each is dropped, never sent again, and neither the answer nor the
// agent's polling slows for it.
func TestDraftsTooSoonAreDropped(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Streamed(40*time.Millisecond, "HW3", " is due", " on Friday."))
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == core.ToolDraft {
			return &fakecore.Injection{Status: http.StatusTooManyRequests, RetryAfter: time.Second}
		}
		return nil
	})
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	w.waitAnswers(conv, 1)
	calls := w.calls(own.actor.ID, core.ToolDraft)
	if len(calls) == 0 {
		t.Fatal("no draft was sent")
	}
	versions := map[int64]bool{}
	for _, c := range calls {
		var args core.DraftArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatal(err)
		}
		if versions[args.Version] {
			t.Errorf("version %d was sent again after a 429", args.Version)
		}
		versions[args.Version] = true
	}
	eventually(t, "the drops counted", func() bool {
		return counter(t, wk.reg, "draft_writes_total", map[string]string{"outcome": "dropped"}) == float64(len(calls))
	})
	if st := agentStatus(t, wk, "yuki-helper"); st.SlowUntil != nil || st.DraftWrites.Failed != 0 || st.DraftWrites.Sent != 0 {
		t.Errorf("status: slow until %v, %+v", st.SlowUntil, st.DraftWrites)
	}
}

// TestDraftsFailingOnTheWayAreSentOnceMore: Core cannot be reached for
// drafts (a 503 each time); each is sent once more, then given up, and the
// answer goes in regardless.
func TestDraftsFailingOnTheWayAreSentOnceMore(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Streamed(40*time.Millisecond, "HW3", " is due", " on Friday."))
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == core.ToolDraft {
			return &fakecore.Injection{Status: http.StatusServiceUnavailable}
		}
		return nil
	})
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	w.waitAnswers(conv, 1)
	eventually(t, "a draft given up", func() bool { return agentStatus(t, wk, "yuki-helper").DraftWrites.Failed >= 1 })
	wk.stop() // its drafts sent, and answered
	failed := int(counter(t, wk.reg, "draft_writes_total", map[string]string{"outcome": "failed"}))
	n := len(w.calls(own.actor.ID, core.ToolDraft))
	// Each given up after two sends; the last, perhaps, sent once, its
	// retry passed over for the answer that took its place.
	if n != 2*failed && n != 2*failed+1 {
		t.Errorf("%d drafts sent, %d given up", n, failed)
	}
}

// TestDraftWithoutStreaming: a model whose adapter does not stream shows
// its steps alone.
func TestDraftWithoutStreaming(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	// Slow enough, each, for a draft to go out between them.
	model := scripted.New(slowly(scripted.CallTool("assignment_get", `{"assignment_id":"`+w.co.AssignmentID+`"}`)),
		slowly(scripted.Reply("Friday.")))
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "assignment_get" {
			return &fakecore.Injection{Delay: 100 * time.Millisecond}
		}
		return nil
	})
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), nil, adapter(callOnly{model}, 5*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW1 due?")
	w.waitAnswers(conv, 1)
	ws := w.fc.DraftWrites(conv)
	sawAssignment := false
	for _, d := range ws {
		if d.Text != nil {
			t.Errorf("a draft without streaming carries text: %+v", d)
		}
		for _, s := range d.Steps {
			if s.Kind == "reading_assignment" && s.Target != nil && *s.Target == "HW1" {
				sawAssignment = true
			}
		}
	}
	if !sawAssignment {
		t.Errorf("no draft says HW1 was read: %+v", ws)
	}
}

// slowly is s, answering a tenth of a second late.
func slowly(s scripted.Step) scripted.Step {
	return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		time.Sleep(100 * time.Millisecond)
		return s(ctx, req)
	}
}

// callOnly is an adapter that does not stream.
type callOnly struct{ llm.Adapter }
