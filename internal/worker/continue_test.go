package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// An answer the output cap cuts off (continue.go), against the fake Core,
// with the scripted model: its cap is 500 tokens a call (agentDoc).

// cutOff runs the own agent with over merged into its settings and the
// scripted model, asks one question and waits for its answer, its ledger
// row and its log line, which it returns with the answer's body.
func cutOff(t *testing.T, model *scripted.Adapter, over map[string]any) (string, *worker, map[string]any) {
	t.Helper()
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{})
	conv, _ := w.ask(0, own, "Tell me all about HW1.")
	body := w.waitAnswers(conv, 1)[0].Body
	eventually(t, "the ledger's answer row", func() bool { return len(wk.st.outcomes(conv)) > 0 })
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	eventually(t, "the answer's log line", func() bool {
		line = logLine(w, "answer", conv)
		return line != nil
	})
	return body, wk, line
}

// logLine is the first JSON log line of w whose msg is msg, about conv.
func logLine(w *world, msg, conv string) map[string]any {
	for l := range strings.SplitSeq(w.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == msg && m["conversation"] == conv {
			return m
		}
	}
	return nil
}

// continuedWith is the answer so far and the word to go on that a
// continuation's request ends with.
func continuedWith(t *testing.T, req *llm.Request) (string, string) {
	t.Helper()
	n := len(req.Messages)
	if n < 2 || req.Messages[n-2].Role != llm.RoleAssistant || req.Messages[n-1].Role != llm.RoleUser {
		t.Fatalf("a continuation's request ends with %+v", req.Messages[max(n-2, 0):])
	}
	return req.Messages[n-2].Parts[0].Text, req.Messages[n-1].Parts[0].Text
}

// TestContinuesAnAnswerCutOff: a turn the output cap cuts off is not
// written again but continued, twice here, with no tools: each
// continuation is given the answer so far as the model's and told to go
// on where it stops, and the pieces are joined in order, nothing twice.
// The answer is the model's, of one turn and two continuations.
func TestContinuesAnAnswerCutOff(t *testing.T) {
	pieces := []string{"Three things matter here. First, the deadline, which is", " Friday at noon. Second, the format", ": a PDF of ten pages at most."}
	model := scripted.New(scripted.Stop(llm.StopMaxTokens, pieces[0]), scripted.Stop(llm.StopMaxTokens, pieces[1]), scripted.Reply(pieces[2]))
	body, wk, line := cutOff(t, model, nil)
	if want := strings.Join(pieces, ""); body != want {
		t.Errorf("body %q, want %q", body, want)
	}
	reqs := model.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d calls", len(reqs))
	}
	for i, req := range reqs[1:] {
		sofar, word := continuedWith(t, req)
		if want := strings.Join(pieces[:i+1], ""); sofar != want {
			t.Errorf("continuation %d is given %q as the answer so far, want %q", i+1, sofar, want)
		}
		if word != prompt.Continue(false, 0) {
			t.Errorf("continuation %d is told %q", i+1, word)
		}
		if req.ToolMode != llm.ToolNone || len(req.Tools) != len(reqs[0].Tools) || req.Limits.MaxOutputTokens != 500 ||
			len(req.Messages) != len(reqs[0].Messages)+2 || req.System != reqs[0].System {
			t.Errorf("continuation %d: mode %s, %d tools, cap %d, %d messages", i+1, req.ToolMode, len(req.Tools), req.Limits.MaxOutputTokens, len(req.Messages))
		}
	}
	calls, recs := wk.st.ledger()
	if len(calls) != 3 || len(recs) != 1 || recs[0].Outcome != store.OutcomePosted || !recs[0].Billable || recs[0].Turns != 1 {
		t.Errorf("ledger: %d calls, answers %+v", len(calls), recs)
	}
	if line["continuations"] != 2.0 || line["truncated"] != false || line["kind"] != kindModel {
		t.Errorf("the answer's log line: %v", line)
	}
	if n := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": budgetTruncated}); n != 0 {
		t.Errorf("budget_exhausted_total{truncated} = %v", n)
	}
}

// TestContinuationWritingAgainIsJoinedOnce: a continuation that begins
// again with the row or the words the answer broke off in is joined to it
// without them.
func TestContinuationWritingAgainIsJoinedOnce(t *testing.T) {
	for _, c := range []struct{ name, first, then, want string }{
		{"a table row", "| Week | Topic |\n|---|---|\n| 1 | Loops |\n| 2 | Recur", "| 2 | Recursion |\n| 3 | Trees |",
			"| Week | Topic |\n|---|---|\n| 1 | Loops |\n| 2 | Recursion |\n| 3 | Trees |"},
		{"a sentence, on a new line", "The lab opens at nine. Bring your", "\nBring your laptop and charger.", "The lab opens at nine. Bring your laptop and charger."},
		{"Chinese", "作業一的截止日期是星期五，請記得", "請記得上傳PDF檔。", "作業一的截止日期是星期五，請記得上傳PDF檔。"},
	} {
		t.Run(c.name, func(t *testing.T) {
			model := scripted.New(scripted.Stop(llm.StopMaxTokens, c.first), scripted.Reply(c.then))
			if body, _, _ := cutOff(t, model, nil); body != c.want {
				t.Errorf("body %q, want %q", body, c.want)
			}
		})
	}
}

// TestDraftGrowsAcrossAContinuation: the draft shows the answer growing
// through its continuation, never starting again from nothing, in one
// attempt, until the posted answer takes its place.
func TestDraftGrowsAcrossAContinuation(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	first, then := []string{"HW3 is due", " on Friday"}, []string{" at noon,", " in room 101,", " with your", " lab partner."}
	model := scripted.New(scripted.StreamedStop(100*time.Millisecond, llm.StopMaxTokens, first...), scripted.Streamed(100*time.Millisecond, then...))
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	full := strings.Join(append(first, then...), "")
	if got := w.waitAnswers(conv, 1)[0].Body; got != full {
		t.Errorf("posted %q", got)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	ws := w.fc.DraftWrites(conv)
	prev, grew := "", false
	for i, d := range ws {
		if d.Attempt != ws[0].Attempt || d.Done {
			t.Errorf("draft %d: %+v", i, d)
		}
		if d.Text == nil {
			if prev != "" {
				t.Errorf("draft %d has no text after %q", i, prev)
			}
			continue
		}
		if !strings.HasPrefix(*d.Text, prev) || !strings.HasPrefix(full, *d.Text) {
			t.Errorf("draft %d's text %q after %q: the answer did not grow", i, *d.Text, prev)
		}
		prev = *d.Text
		grew = grew || len(prev) > len(strings.Join(first, ""))
	}
	if !grew {
		t.Errorf("no draft showed the continuation: %+v", ws)
	}
}

// TestLastContinuation: a continuation whose room is less than a whole cap
// is the last: it is told its room, and to close the answer within it.
// Should the cap still cut it off, the answer is posted as the model's with
// on_truncated_text after it, counted and logged as cut short.
func TestLastContinuation(t *testing.T) {
	first := scripted.WithUsage(scripted.Stop(llm.StopMaxTokens, "A long answer that"), llm.Usage{Input: 100, Output: 700})
	t.Run("it closes the answer", func(t *testing.T) {
		model := scripted.New(first, scripted.Reply(" ends here."))
		body, wk, line := cutOff(t, model, perAnswer("output_tokens", 1000))
		if body != "A long answer that ends here." {
			t.Errorf("body %q", body)
		}
		req := lastRequest(t, model)
		if _, word := continuedWith(t, req); word != prompt.Continue(true, 300) || req.Limits.MaxOutputTokens != 300 {
			t.Errorf("the last continuation, capped at %d, is told %q", req.Limits.MaxOutputTokens, word)
		}
		if line["truncated"] != false || counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": budgetTruncated}) != 0 {
			t.Errorf("an answer closed in time is counted as cut short: %v", line)
		}
	})
	t.Run("the body's room, at the characters a token the answer is written at", func(t *testing.T) {
		text := strings.Repeat("Each week has a lab. ", 7)[:147] + " so"
		model := scripted.New(scripted.WithUsage(scripted.Stop(llm.StopMaxTokens, text), llm.Usage{Input: 100, Output: 50}), scripted.Reply(" on."))
		body, _, _ := cutOff(t, model, map[string]any{"answer": map[string]any{"max_body_chars": 1000}})
		room := (1000 - utf8.RuneCountInString(config.DefaultTruncatedText) - 2 - len(text)) * 50 / len(text)
		req := lastRequest(t, model)
		if _, word := continuedWith(t, req); word != prompt.Continue(true, room) || req.Limits.MaxOutputTokens != room || body != text+" on." {
			t.Errorf("the continuation, capped at %d, is told %q; body %q", req.Limits.MaxOutputTokens, word, body)
		}
	})
	t.Run("the wall clock's room, at the pace the answer is written at", func(t *testing.T) {
		slow := func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
			time.Sleep(1200 * time.Millisecond)
			return scripted.WithUsage(scripted.Stop(llm.StopMaxTokens, "Slowly, the answer"), llm.Usage{Input: 100, Output: 500})(ctx, req)
		}
		model := scripted.New(slow, scripted.Reply(" ends."))
		body, _, _ := cutOff(t, model, perAnswer("wall_clock_s", 2))
		req := lastRequest(t, model)
		room := req.Limits.MaxOutputTokens
		if _, word := continuedWith(t, req); word != prompt.Continue(true, room) || room < minContinuation || room >= 500 || body != "Slowly, the answer ends." {
			t.Errorf("the continuation, capped at %d, is told %q; body %q", room, word, body)
		}
	})
	t.Run("still cut off, it is posted with on_truncated_text", func(t *testing.T) {
		model := scripted.New(first, scripted.WithUsage(scripted.Stop(llm.StopMaxTokens, " goes on and on"), llm.Usage{Input: 100, Output: 300}))
		body, wk, line := cutOff(t, model, perAnswer("output_tokens", 1000))
		if want := "A long answer that goes on and on\n\n" + config.DefaultTruncatedText; body != want {
			t.Errorf("body %q, want %q", body, want)
		}
		if n := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": budgetTruncated}); n != 1 {
			t.Errorf("budget_exhausted_total{truncated} = %v", n)
		}
		if line["truncated"] != true || line["continuations"] != 1.0 || line["outcome"] != store.OutcomePosted || line["kind"] != kindModel {
			t.Errorf("the answer's log line: %v", line)
		}
	})
}

// TestForcedTurnCutOff: a last turn forced by a spent budget that the cap
// cuts off is continued as any other; one forced by the wall clock has no
// time left to continue in, and is posted with on_truncated_text.
func TestForcedTurnCutOff(t *testing.T) {
	t.Run("turns: continued", func(t *testing.T) {
		model := scripted.New(scripted.CallTool("course_get", `{}`), scripted.Stop(llm.StopMaxTokens, "With what the course says,"),
			scripted.Reply(" HW1 is due on Friday."))
		body, _, line := cutOff(t, model, perAnswer("turns", 2))
		reqs := model.Requests()
		if body != "With what the course says, HW1 is due on Friday." || reqs[1].ToolMode != llm.ToolNone || reqs[2].ToolMode != llm.ToolNone {
			t.Errorf("body %q", body)
		}
		if sofar, _ := continuedWith(t, reqs[2]); sofar != "With what the course says," || line["continuations"] != 1.0 || line["turns"] != 2.0 {
			t.Errorf("the continuation of %q; log line %v", sofar, line)
		}
	})
	t.Run("wall clock: cut short", func(t *testing.T) {
		slow := func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
			time.Sleep(400 * time.Millisecond)
			return scripted.CallTool("course_get", `{}`)(ctx, req)
		}
		model := scripted.New(slow, scripted.Stop(llm.StopMaxTokens, "Late, and"))
		body, _, line := cutOff(t, model, perAnswer("wall_clock_s", 0.3))
		if body != "Late, and\n\n"+config.DefaultTruncatedText || lastRequest(t, model).ToolMode != llm.ToolNone || line["truncated"] != true {
			t.Errorf("body %q, log line %v", body, line)
		}
	})
}

// TestContinuationsKeepToTheBudgets: no continuation is made past the
// output tokens, the wall clock, the input tokens, or the body's room;
// the answer is posted cut short instead, within answer.max_body_chars.
func TestContinuationsKeepToTheBudgets(t *testing.T) {
	cut := func(text string, u llm.Usage) scripted.Step {
		return scripted.WithUsage(scripted.Stop(llm.StopMaxTokens, text), u)
	}
	long := strings.Repeat("The rubric weighs each part. ", 12)
	for _, c := range []struct {
		name string
		step scripted.Step
		over map[string]any
	}{
		{"output tokens", cut("All the output", llm.Usage{Input: 100, Output: 500}), perAnswer("output_tokens", 500)},
		{"input tokens", cut("All the input", llm.Usage{Input: 200, Output: 20}), perAnswer("input_tokens", 150)},
		{"wall clock", func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
			time.Sleep(400 * time.Millisecond)
			return scripted.Stop(llm.StopMaxTokens, "All the time")(ctx, req)
		}, perAnswer("wall_clock_s", 0.3)},
		{"the body", cut(long, llm.Usage{Input: 100, Output: 80}), map[string]any{"answer": map[string]any{"max_body_chars": 200}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// One step: a continuation would be a script error.
			model := scripted.New(c.step)
			body, wk, line := cutOff(t, model, c.over)
			if !strings.HasSuffix(body, "\n\n"+config.DefaultTruncatedText) || line["truncated"] != true || line["continuations"] != 0.0 {
				t.Errorf("body %q, log line %v", body, line)
			}
			if n := utf8.RuneCountInString(body); n > bodyChars(c.over) {
				t.Errorf("the body is %d characters", n)
			}
			if _, recs := wk.st.ledger(); recs[0].Outcome != store.OutcomePosted || !recs[0].Billable {
				t.Errorf("the answer's row: %+v", recs[0])
			}
		})
	}
}

// bodyChars is answer.max_body_chars as over sets it, or its default.
func bodyChars(over map[string]any) int {
	if a, ok := over["answer"].(map[string]any); ok {
		if n, ok := a["max_body_chars"].(int); ok {
			return n
		}
	}
	return 19000
}

// TestContinuationEnds: what a continuation stops for ends the answer: a
// refusal as any refusal, a failure with what was written and
// on_truncated_text.
func TestContinuationEnds(t *testing.T) {
	for _, c := range []struct {
		name string
		then scripted.Step
		want string
	}{
		{"refused", scripted.Stop(llm.StopRefusal, ""), config.DefaultRefusalText},
		{"filtered", scripted.Fail(&llm.Error{Kind: llm.ErrContentFilter}), config.DefaultRefusalText},
		{"failed", scripted.Fail(&llm.Error{Kind: llm.ErrBadRequest, Status: 400}), "The answer so far\n\n" + config.DefaultTruncatedText},
		{"a tool called, and nothing written", scripted.CallTool("course_get", `{}`), "The answer so far\n\n" + config.DefaultTruncatedText},
		{"a tool called after the rest", scripted.Respond(llm.Response{Stop: llm.StopToolCalls, Parts: []llm.Part{llm.Text(", whole."),
			{Type: llm.PartToolCall, Name: "course_get", Args: json.RawMessage(`{}`)}}}), "The answer so far, whole."},
		{"nothing more to say", scripted.Reply(""), "The answer so far"},
	} {
		t.Run(c.name, func(t *testing.T) {
			model := scripted.New(scripted.Stop(llm.StopMaxTokens, "The answer so far"), c.then)
			if body, _, _ := cutOff(t, model, nil); body != c.want {
				t.Errorf("body %q, want %q", body, c.want)
			}
		})
	}
}

// TestAnswerTooLongToPostSaysSo: an answer the model ended, longer than
// answer.max_body_chars, is cut to fit, and says so as an answer cut short
// at the cap does.
func TestAnswerTooLongToPostSaysSo(t *testing.T) {
	long := strings.Repeat("Each lab builds on the one before. ", 20)
	model := scripted.New(scripted.Reply(long))
	body, wk, line := cutOff(t, model, map[string]any{"answer": map[string]any{"max_body_chars": 300}})
	if n := utf8.RuneCountInString(body); n > 300 || !strings.HasPrefix(body, "Each lab") || !strings.HasSuffix(body, "…\n\n"+config.DefaultTruncatedText) {
		t.Errorf("%d characters: %q", n, body)
	}
	if line["truncated"] != true || line["outcome"] != store.OutcomePosted ||
		counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": budgetTruncated}) != 1 {
		t.Errorf("the answer's log line: %v", line)
	}
}

func TestJoinPiece(t *testing.T) {
	for _, c := range []struct{ text, piece, want string }{
		{"It is due on Fri", "day.", "It is due on Friday."},
		{"Say that that", " is all.", "Say that that is all."},
		{"| 2 | Recur", "| 2 | Recursion |", "| 2 | Recursion |"},
		{"First line.\n| 2 | Recur", "\n| 2 | Recursion |", "First line.\n| 2 | Recursion |"},
		{"請記得", "請記得上傳", "請記得上傳"},
		{"ha ha", " ha ha", "ha ha ha ha"},
		{"", "Anything.", "Anything."},
	} {
		if got := joinPiece(c.text, c.piece); got != c.want {
			t.Errorf("joinPiece(%q, %q) = %q, want %q", c.text, c.piece, got, c.want)
		}
	}
}

func TestOpenFence(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"No code.", ""},
		{"```go\nfunc f() {", "```"},
		{"```go\nfunc f() {}\n```\nDone.", ""},
		{"  ~~~~\ncode\n~~~", "~~~~"},
		{"````\n```\nstill code", "````"},
		{"Inline ```x``` is no fence.", ""},
		{"- a list:\n    ```python\n    print(1)", "```"},
	} {
		if got := openFence(c.text); got != c.want {
			t.Errorf("openFence(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// withNote leaves the note room, cutting the answer rather than the note,
// and closes a code block the answer leaves open.
func TestWithNote(t *testing.T) {
	note := "(Cut short.)"
	if got := withNote("Some text", note, 1000); got != "Some text\n\n"+note {
		t.Errorf("%q", got)
	}
	if got := withNote("```go\nfunc f() {", note, 1000); got != "```go\nfunc f() {\n```\n\n"+note {
		t.Errorf("%q", got)
	}
	long := strings.Repeat("A sentence of the answer. ", 20)
	got := withNote(long, note, 100)
	if n := utf8.RuneCountInString(got); n > 100 || !strings.HasSuffix(got, "\n\n"+note) || !strings.HasPrefix(got, "A sentence") {
		t.Errorf("%d characters: %q", n, got)
	}
	code := "```\n" + strings.Repeat("x := 1\n", 30)
	got = withNote(code, note, 100)
	if n := utf8.RuneCountInString(got); n > 100 || !strings.HasSuffix(got, "\n```\n\n"+note) {
		t.Errorf("%d characters: %q", n, got)
	}
	if got := withNote("Anything", note, 10); got != note {
		t.Errorf("no room: %q", got)
	}
}
