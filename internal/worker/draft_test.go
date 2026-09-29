package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
)

// draftCore is a Caller that takes conversation_draft calls as Core would,
// or as answer says, keeps each, and counts those in flight at once.
type draftCore struct {
	mu       sync.Mutex
	writes   []core.DraftArgs
	ctxs     []context.Context
	answer   func(n int, args core.DraftArgs) (*core.Envelope, error)
	hold     time.Duration
	inFlight atomic.Int32
	most     atomic.Int32
	starts   []time.Time
}

func (d *draftCore) Call(ctx context.Context, tool string, raw json.RawMessage) (*core.Envelope, error) {
	if tool != core.ToolDraft {
		return nil, errors.New("not a draft")
	}
	n := d.inFlight.Add(1)
	defer d.inFlight.Add(-1)
	for {
		if m := d.most.Load(); n <= m || d.most.CompareAndSwap(m, n) {
			break
		}
	}
	var args core.DraftArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.writes = append(d.writes, args)
	d.ctxs = append(d.ctxs, ctx)
	d.starts = append(d.starts, time.Now())
	i := len(d.writes)
	answer := d.answer
	d.mu.Unlock()
	if d.hold > 0 {
		time.Sleep(d.hold)
	}
	if answer != nil {
		return answer(i, args)
	}
	return executed(args), nil
}

func executed(args core.DraftArgs) *core.Envelope {
	res, _ := json.Marshal(map[string]any{"stored": true, "version": args.Version})
	return &core.Envelope{Status: core.StatusExecuted, Result: res}
}

func (d *draftCore) sent() []core.DraftArgs {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.writes)
}

// testDrafter is a drafter of an agent that talks to c, writing at most
// every every.
func testDrafter(t *testing.T, c core.Caller, every time.Duration) (*drafter, *Agent, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	s := &Supervisor{o: Options{Metrics: metrics.New(reg), Timing: Timing{DraftEvery: every}}}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{s: s, id: "tutor", log: slog.New(slog.NewTextHandler(io.Discard, nil)), client: core.NewClient(c), drafts: true, answerCtx: ctx}
	t.Cleanup(func() {
		cancel()
		a.answers.Wait()
	})
	// As a claim takes it: its own place in answers held meanwhile.
	a.answers.Add(1)
	d := a.drafter("course-1", "conv-1")
	a.answers.Done()
	return d, a, reg
}

// lastWrite is the last write sent, if any.
func lastWrite(c *draftCore) (core.DraftArgs, bool) {
	ws := c.sent()
	if len(ws) == 0 {
		return core.DraftArgs{}, false
	}
	return ws[len(ws)-1], true
}

// waitWrites waits for n writes.
func waitWrites(t *testing.T, c *draftCore, n int) []core.DraftArgs {
	t.Helper()
	eventually(t, "draft writes", func() bool { return len(c.sent()) >= n })
	return c.sent()
}

// A draft's first write goes at once; the state then changing many times
// is sent at most every DraftEvery, one write at a time, each the state as
// it stands, whole, with its version rising, and the last the latest.
func TestDrafterCoalesces(t *testing.T) {
	c := &draftCore{hold: 20 * time.Millisecond}
	d, _, reg := testDrafter(t, c, 50*time.Millisecond)
	d.begin()
	d.round()
	waitWrites(t, c, 1)
	words := strings.Fields("Recursion is a function calling itself on a smaller input until it reaches a base case")
	for _, w := range words {
		d.text(w + " ")
		time.Sleep(5 * time.Millisecond)
	}
	want := strings.Join(words, " ") + " "
	eventually(t, "the whole text written", func() bool {
		last, ok := lastWrite(c)
		return ok && last.Text != nil && *last.Text == want
	})
	ws := c.sent()
	if len(ws) >= len(words) {
		t.Errorf("%d writes for %d changes: nothing was coalesced", len(ws), len(words)+1)
	}
	if first := ws[0]; first.Text != nil || len(first.Steps) != 1 || first.Steps[0] != (core.DraftStep{Kind: core.StepThinking, State: core.StepRunning}) {
		t.Errorf("first write %+v", first)
	}
	for i, w := range ws {
		if w.Attempt != ws[0].Attempt || w.Version != int64(i+1) || w.CourseID != "course-1" || w.ConversationID != "conv-1" || w.Done {
			t.Errorf("write %d: %+v", i, w)
		}
		if i > 0 && w.Text != nil && !strings.HasPrefix(want, *w.Text) {
			t.Errorf("write %d's text %q is not the text so far", i, *w.Text)
		}
		if !core.BestEffort(c.ctxs[i]) {
			t.Errorf("write %d is not best effort", i)
		}
	}
	last := ws[len(ws)-1]
	if steps := last.Steps; len(steps) != 2 || steps[0].State != core.StepDone || steps[1] != (core.DraftStep{Kind: core.StepWriting, State: core.StepRunning}) {
		t.Errorf("steps once writing: %+v", steps)
	}
	if c.most.Load() != 1 {
		t.Errorf("%d writes were in flight at once", c.most.Load())
	}
	for i := 1; i < len(c.starts); i++ {
		if gap := c.starts[i].Sub(c.starts[i-1]); gap < 45*time.Millisecond {
			t.Errorf("writes %d and %d went %s apart", i, i+1, gap)
		}
	}
	eventually(t, "every write counted", func() bool {
		return counter(t, reg, "draft_writes_total", map[string]string{"agent": "tutor", "outcome": "sent"}) == float64(len(ws))
	})
}

// A round's tool calls are steps of their kinds, running, then done as Core
// answers each, with a document's title where every member may read it;
// the next round's thinking follows, and its text starts from nothing.
func TestDrafterSteps(t *testing.T) {
	c := &draftCore{}
	d, _, _ := testDrafter(t, c, time.Millisecond)
	d.begin()
	d.round()
	d.text("Let me look.")
	eventually(t, "the preamble", func() bool {
		last, ok := lastWrite(c)
		return ok && last.Text != nil && *last.Text == "Let me look."
	})
	calls := []llm.Part{
		{Type: llm.PartToolCall, ID: "c1", Name: "document_get"},
		{Type: llm.PartToolCall, ID: "c2", Name: "grade_list"},
	}
	d.calls(calls)
	d.seen(calls[0], &core.Envelope{Status: core.StatusExecuted,
		Result: json.RawMessage(`{"id":"d1","kind":"material","title":"HW1.pdf\nnotes","status":"active","published_version_id":"v1","version":{"body_md":"the whole handout"}}`)})
	d.callsDone()
	d.round()
	eventually(t, "the second round", func() bool {
		last, ok := lastWrite(c)
		return ok && len(last.Steps) == 5
	})
	ws := c.sent()
	last := ws[len(ws)-1]
	want := []core.DraftStep{
		{Kind: core.StepThinking, State: core.StepDone},
		{Kind: core.StepWriting, State: core.StepDone},
		{Kind: core.StepReadingDocument, Target: "HW1.pdf notes", State: core.StepDone},
		{Kind: core.StepTool, State: core.StepDone},
		{Kind: core.StepThinking, State: core.StepRunning},
	}
	if !slices.Equal(last.Steps, want) {
		t.Errorf("steps %+v\nwant %+v", last.Steps, want)
	}
	if last.Text == nil || *last.Text != "" {
		t.Errorf("the second round's text is %v; want it cleared", last.Text)
	}
	for _, w := range ws {
		b, _ := json.Marshal(w)
		if strings.Contains(string(b), "handout") {
			t.Errorf("a write carries the tool result's content: %s", b)
		}
	}
}

// The latest twenty steps are kept, as Core takes no more.
func TestDraftStepsAreCapped(t *testing.T) {
	var c attemptDraft
	for i := range 25 {
		c.finish()
		c.add(&core.DraftStep{Kind: core.StepTool, Target: string(rune('a' + i)), State: core.StepRunning})
	}
	if len(c.steps) != maxDraftSteps || c.steps[0].Target != "f" || c.steps[19].Target != "y" || c.steps[19].State != core.StepRunning {
		t.Errorf("%d steps, from %q to %q", len(c.steps), c.steps[0].Target, c.steps[len(c.steps)-1].Target)
	}
}

// An attempt that ends without its answer posted is ended with done, its
// version above the last; one whose answer is posted is left to Core,
// which clears its draft; and one of which nothing was sent needs nothing.
func TestDrafterEnds(t *testing.T) {
	c := &draftCore{}
	d, _, _ := testDrafter(t, c, time.Millisecond)
	d.begin()
	d.round()
	waitWrites(t, c, 1)
	d.end(false)
	ws := waitWrites(t, c, 2)
	if done := ws[1]; !done.Done || done.Attempt != ws[0].Attempt || done.Version <= ws[0].Version || done.Text != nil || done.Steps != nil {
		t.Errorf("the end: %+v after %+v", done, ws[0])
	}

	d.begin()
	d.round()
	ws = waitWrites(t, c, 3)
	if ws[2].Attempt == ws[0].Attempt || ws[2].Version != 1 {
		t.Errorf("a new attempt's first write: %+v", ws[2])
	}
	d.hold()
	d.text("posted meanwhile")
	d.end(true)

	d.begin() // nothing of it is sent
	d.end(false)
	d.close()
	time.Sleep(20 * time.Millisecond)
	if n := len(c.sent()); n != 3 {
		t.Errorf("%d writes; the posted attempt and the empty one needed none", n)
	}
}

// A write Core refuses as too soon is dropped: not sent again, and the
// agent's polling not slowed; the next change is sent as usual.
func TestDrafterDropsTooSoon(t *testing.T) {
	c := &draftCore{answer: func(n int, args core.DraftArgs) (*core.Envelope, error) {
		if n == 1 {
			return nil, &core.RateLimitedError{RetryAfter: time.Second}
		}
		return executed(args), nil
	}}
	d, a, reg := testDrafter(t, c, 5*time.Millisecond)
	d.begin()
	d.round()
	waitWrites(t, c, 1)
	time.Sleep(30 * time.Millisecond)
	if n := len(c.sent()); n != 1 {
		t.Fatalf("%d writes: the one refused as too soon was sent again", n)
	}
	d.text("On Friday.")
	ws := waitWrites(t, c, 2)
	if ws[1].Version <= ws[0].Version || *ws[1].Text != "On Friday." {
		t.Errorf("the next write: %+v", ws[1])
	}
	eventually(t, "the counts", func() bool { return a.draftCounts.sent.Load() == 1 })
	if got := counter(t, reg, "draft_writes_total", map[string]string{"outcome": "dropped"}); got != 1 || a.draftCounts.dropped.Load() != 1 {
		t.Errorf("dropped %v, %d", got, a.draftCounts.dropped.Load())
	}
}

// A write that failed on the way is sent once more, as the state stands
// then; failing again, it is given up, until the state changes.
func TestDrafterRetriesOnce(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c := &draftCore{answer: func(_ int, args core.DraftArgs) (*core.Envelope, error) {
		if fail.Load() {
			return nil, &core.TransientError{Status: 502, Err: errors.New("bad gateway")}
		}
		return executed(args), nil
	}}
	d, a, _ := testDrafter(t, c, 5*time.Millisecond)
	d.begin()
	d.round()
	ws := waitWrites(t, c, 2)
	time.Sleep(40 * time.Millisecond)
	if n := len(c.sent()); n != 2 {
		t.Fatalf("%d writes; want the write and one retry", n)
	}
	if ws[1].Version <= ws[0].Version || ws[1].Attempt != ws[0].Attempt {
		t.Errorf("the retry %+v after %+v", ws[1], ws[0])
	}
	if a.draftCounts.failed.Load() != 1 {
		t.Errorf("failed %d, want 1", a.draftCounts.failed.Load())
	}
	fail.Store(false)
	d.text("On Friday.")
	ws = waitWrites(t, c, 3)
	if *ws[2].Text != "On Friday." {
		t.Errorf("the next write %+v", ws[2])
	}
	// The end of an attempt is retried too.
	fail.Store(true)
	d.end(false)
	ws = waitWrites(t, c, 5)
	if !ws[3].Done || !ws[4].Done || ws[3].Version != ws[4].Version {
		t.Errorf("the end, then its retry: %+v, %+v", ws[3], ws[4])
	}
}

// A draft Core refuses for good (the seat is not the respondent any more,
// or the answer went in) stops the attempt's writes, its end included.
func TestDrafterStopsWhenRefused(t *testing.T) {
	for _, env := range []*core.Envelope{
		{Status: core.StatusError, Error: &core.Error{Code: core.CodeForbidden, Details: map[string]any{"reason": "not_the_respondent"}}},
		{Status: core.StatusError, Error: &core.Error{Code: core.CodeConflict, Details: map[string]any{"reason": "conversation_not_awaiting"}}},
	} {
		c := &draftCore{answer: func(int, core.DraftArgs) (*core.Envelope, error) { return env, nil }}
		d, a, _ := testDrafter(t, c, time.Millisecond)
		d.begin()
		d.round()
		waitWrites(t, c, 1)
		time.Sleep(10 * time.Millisecond)
		d.text("more")
		d.end(false)
		time.Sleep(20 * time.Millisecond)
		if n := len(c.sent()); n != 1 {
			t.Errorf("%s: %d writes after a refusal", env.Reason(), n)
		}
		if a.draftCounts.failed.Load()+a.draftCounts.dropped.Load() != 1 {
			t.Errorf("%s: counted %d failed, %d dropped", env.Reason(), a.draftCounts.failed.Load(), a.draftCounts.dropped.Load())
		}
	}
}

// A closed drafter sends what it has left, then ends; a claim of the same
// conversation takes on one that has not ended, so that one write at most
// is ever in flight for a conversation.
func TestDrafterPerConversation(t *testing.T) {
	c := &draftCore{hold: 30 * time.Millisecond}
	d, a, _ := testDrafter(t, c, time.Millisecond)
	d.begin()
	d.round()
	eventually(t, "the first write in flight", func() bool { return c.inFlight.Load() == 1 })
	d.end(false)
	d.close()
	a.answers.Add(1)
	again := a.drafter("course-1", "conv-1")
	a.answers.Done()
	again.begin()
	again.round()
	again.close()
	ws := waitWrites(t, c, 3)
	if !ws[1].Done || ws[2].Attempt == ws[0].Attempt || c.most.Load() != 1 {
		t.Errorf("writes %+v, %d at once", ws, c.most.Load())
	}
	eventually(t, "the drafter to end", func() bool {
		a.draftMu.Lock()
		defer a.draftMu.Unlock()
		return a.drafters["conv-1"] == nil
	})
	a.answers.Add(1)
	fresh := a.drafter("course-1", "conv-1")
	a.answers.Done()
	if fresh == d {
		t.Error("an ended drafter was taken on")
	}
	fresh.close()
}

// Without drafts, there is no drafter, and every step is nothing.
func TestNoDrafter(t *testing.T) {
	a := &Agent{}
	d := a.drafter("c", "v")
	if d != nil {
		t.Fatal("a drafter against a Core that takes no drafts")
	}
	d.begin()
	d.round()
	d.text("x")
	d.calls([]llm.Part{{Type: llm.PartToolCall, ID: "1", Name: "document_get"}})
	d.seen(llm.Part{ID: "1"}, nil)
	d.callsDone()
	d.hold()
	d.end(false)
	d.close()
}

func TestDraftTarget(t *testing.T) {
	env := func(result string) *core.Envelope {
		return &core.Envelope{Status: core.StatusExecuted, Result: json.RawMessage(result)}
	}
	long := strings.Repeat("長", 130)
	for _, c := range []struct {
		tool string
		env  *core.Envelope
		want string
	}{
		{"document_get", env(`{"kind":"material","title":"Syllabus","status":"active","published_version_id":"v"}`), "Syllabus"},
		{"document_get", env(`{"kind":"material","title":"  Week 3\tslides\u0007 ","published_version_id":"v"}`), "Week 3 slides"},
		{"document_get", env(`{"kind":"material","title":"` + long + `","published_version_id":"v"}`), strings.Repeat("長", 119) + "…"},
		// Not every member may read these: no title.
		{"document_get", env(`{"kind":"material","title":"Draft notes","published_version_id":null}`), ""},
		{"document_get", env(`{"kind":"material","title":"Old","status":"archived","published_version_id":"v"}`), ""},
		{"document_get", env(`{"kind":"rubric","title":"HW3 rubric","published_version_id":"v"}`), ""},
		{"document_get", env(`{"kind":"submission","title":"Ken's HW3","published_version_id":"v"}`), ""},
		{"document_get", env(`{"kind":"instructions","title":"Midterm questions","published_version_id":"v"}`), ""},
		{"assignment_get", env(`{"title":"HW3","published_at":"2026-09-01T00:00:00Z"}`), "HW3"},
		{"assignment_get", env(`{"title":"Midterm","published_at":null}`), ""},
		{"grade_list", env(`{"title":"x"}`), ""},
		{"document_get", &core.Envelope{Status: core.StatusDenied}, ""},
		{"document_get", nil, ""},
	} {
		if got := draftTarget(c.tool, c.env); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.tool, c.env, got, c.want)
		}
	}
}

func TestStepKind(t *testing.T) {
	for tool, want := range map[string]string{
		"document_get": core.StepReadingDocument, "document_list": core.StepListingDocuments, "assignment_get": core.StepReadingAssignment,
		"submission_get": core.StepReadingSubmission, "memory_search": core.StepSearchingMemory, "grade_list": core.StepTool,
		"document_versions": core.StepTool,
	} {
		if got := stepKind(tool); got != want {
			t.Errorf("%s: %s, want %s", tool, got, want)
		}
	}
}

func TestDraftTextIsCut(t *testing.T) {
	c := &draftCore{}
	d, _, _ := testDrafter(t, c, time.Millisecond)
	d.begin()
	d.round()
	d.text(strings.Repeat("é", maxDraftChars+10))
	eventually(t, "the text", func() bool {
		last, ok := lastWrite(c)
		return ok && last.Text != nil
	})
	ws := c.sent()
	if n := len([]rune(*ws[len(ws)-1].Text)); n != maxDraftChars {
		t.Errorf("%d characters sent", n)
	}
}
