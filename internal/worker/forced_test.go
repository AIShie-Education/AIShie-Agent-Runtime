package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// A model that thinks before it writes, as DeepSeek's deepseek-flash does
// by default, its thinking counted in its completion tokens: a turn whose
// thinking runs past its max_tokens stops at length with
// reasoning_content and no content, every token of it output, all of it
// reasoning. Told to think least (its thinking switched off,
// thinking {type: disabled}), it writes.

// readingTurn calls tools, having thought for out tokens.
func readingTurn(out int64, calls ...scripted.ToolCall) scripted.Step {
	return scripted.WithUsage(scripted.CallTools(calls...), llm.Usage{Input: 20000, Output: out, Reasoning: out - 50})
}

// thinksToTheCap thinks until the call's cap stops it, writing nothing.
func thinksToTheCap() scripted.Step {
	return func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		n := int64(req.Limits.MaxOutputTokens)
		return &llm.Response{
			Parts: []llm.Part{{Type: llm.PartReasoning, Text: "Let me weigh every document again…"}},
			Stop:  llm.StopMaxTokens, RawStop: "length",
			Usage: llm.Usage{Input: 30000, Output: n, Reasoning: n},
		}, nil
	}
}

// deepseekLike writes text when told to think least, and thinks to the cap
// otherwise.
func deepseekLike(text string) scripted.Step {
	return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		if !req.LeastReasoning {
			return thinksToTheCap()(ctx, req)
		}
		return &llm.Response{Parts: []llm.Part{llm.Text(text)}, Stop: llm.StopEnd, RawStop: "stop",
			Usage: llm.Usage{Input: 30000, Output: 400}}, nil
	}
}

// incident is the agent of the answers on test.aishie.app: 4,000 output
// tokens a call, the default budgets of an answer, with over.
func incident(over map[string]any) map[string]any {
	return mergeMaps(map[string]any{
		"model":   map[string]any{"params": map[string]any{"max_output_tokens": 4000}},
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 10, "output_tokens": 12000}},
	}, over)
}

func getDocCall(id string) scripted.ToolCall {
	return scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + id + `"}`}
}

// TestAThinkingModelCutOffBeforeItWritesIsForcedToAnswer is the answer of
// 2026-10-04 03:54 UTC: three turns of reading (4,784 output tokens,
// nearly all thinking), then a turn whose thinking took its whole cap of
// 4,000 and wrote nothing. What was left of the answer's 12,000 output
// tokens (3,216) could not hold that turn again with twice the cap, and
// the loop gave on_budget_text with no forced last turn: none was logged,
// and budget_exhausted_total counted none. The last turn is forced now,
// within what is left, telling the model to think least, and its answer is
// posted.
func TestAThinkingModelCutOffBeforeItWritesIsForcedToAnswer(t *testing.T) {
	model := scripted.New(
		readingTurn(1500, getDocCall("d1"), getDocCall("d2")),
		readingTurn(1600, getDocCall("d3"), getDocCall("d4")),
		readingTurn(1684, getDocCall("d5"), getDocCall("d6")),
		thinksToTheCap(),
		deepseekLike("Three things to watch in this course: …"),
	)
	body, wk, line := cutOff(t, model, incident(nil))
	if body != "Three things to watch in this course: …" {
		t.Fatalf("body %q (%d calls)", body, len(model.Requests()))
	}
	reqs := model.Requests()
	for i, req := range reqs[:4] {
		if req.ToolMode != llm.ToolAuto || req.LeastReasoning || req.Limits.MaxOutputTokens != 4000 {
			t.Errorf("turn %d: %s, least %v, cap %d", i+1, req.ToolMode, req.LeastReasoning, req.Limits.MaxOutputTokens)
		}
	}
	if f := reqs[4]; f.ToolMode != llm.ToolNone || !f.LeastReasoning || f.Limits.MaxOutputTokens != 12000-8784 {
		t.Errorf("the forced turn: %s, least %v, cap %d", f.ToolMode, f.LeastReasoning, f.Limits.MaxOutputTokens)
	}
	if n := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "output_tokens"}); n != 1 {
		t.Errorf("budget_exhausted_total{output_tokens} = %v", n)
	}
	if line["outcome"] != store.OutcomePosted || line["turns"] != 5.0 || line["tool_calls"] != 6.0 || line["output_tokens"] != 9184.0 ||
		line["asked_again"] != false {
		t.Errorf("the answer's line: %v", line)
	}
	if !strings.Contains(wk.w.logs.String(), `"msg":"a budget of the answer is spent: the last turn is forced"`) {
		t.Error("the forced turn was not logged")
	}
}

// The turns before a forced last turn leave it a reserve of the output
// tokens (forcedFloor): a turn that is not forced is not started once what
// is left, less the reserve, cannot hold a whole cap. After 6,500 of
// 12,000, the next turn is forced at once, at the whole cap of 4,000,
// where before it would have taken that cap thinking and left the forced
// turn 1,500.
func TestTheForcedTurnIsLeftAReserve(t *testing.T) {
	model := scripted.New(
		readingTurn(3000, getDocCall("d1")),
		readingTurn(3500, getDocCall("d2")),
		deepseekLike("From what I read: …"),
	)
	body, wk, _ := cutOff(t, model, incident(nil))
	reqs := model.Requests()
	if f := reqs[2]; body != "From what I read: …" || f.ToolMode != llm.ToolNone || !f.LeastReasoning || f.Limits.MaxOutputTokens != 4000 {
		t.Errorf("body %q; the third turn: %s, least %v, cap %d", body, f.ToolMode, f.LeastReasoning, f.Limits.MaxOutputTokens)
	}
	if n := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "output_tokens"}); n != 1 {
		t.Errorf("budget_exhausted_total{output_tokens} = %v", n)
	}
	// The first turn of a small budget leaves the reserve too: a quarter of
	// it, as the cap is the most a forced turn may write.
	small := scripted.New(scripted.Reply("Short."))
	answerOnce(t, small, incident(perAnswer("output_tokens", 4000)))
	if got := small.Requests()[0].Limits.MaxOutputTokens; got != 3000 {
		t.Errorf("the first turn of 4,000 output tokens was capped at %d, want 3,000", got)
	}
}

// A model whose thinking cannot be switched off thinks through the forced
// turn too. That turn is asked once more, telling it to answer now,
// briefly, within what is left (prompt.AnswerNow); it is not counted as a
// turn, and the answer's line says it was asked again.
func TestAForcedTurnThatWritesNothingIsAskedOnceMore(t *testing.T) {
	answersWhenAsked := func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if !strings.HasPrefix(last.Parts[len(last.Parts)-1].Text, "[You cannot look anything more up") {
			return thinksToTheCap()(ctx, req)
		}
		return &llm.Response{Parts: []llm.Part{llm.Text("In short: …")}, Stop: llm.StopEnd, Usage: llm.Usage{Input: 30000, Output: 900, Reasoning: 800}}, nil
	}
	model := scripted.New(
		readingTurn(1500, getDocCall("d1")),
		readingTurn(1600, getDocCall("d2")),
		thinksToTheCap(),
		thinksToTheCap(),
		answersWhenAsked,
	)
	body, _, line := cutOff(t, model, incident(nil))
	if body != "In short: …" {
		t.Fatalf("body %q", body)
	}
	reqs := model.Requests()
	forced, again := reqs[3], reqs[4]
	room := 12000 - 1500 - 1600 - 4000 - forced.Limits.MaxOutputTokens
	if forced.ToolMode != llm.ToolNone || again.ToolMode != llm.ToolNone || !again.LeastReasoning || again.Limits.MaxOutputTokens != room ||
		len(again.Messages) != len(forced.Messages) {
		t.Errorf("forced: %s, cap %d; asked again: %s, least %v, cap %d, %d messages after %d", forced.ToolMode, forced.Limits.MaxOutputTokens,
			again.ToolMode, again.LeastReasoning, again.Limits.MaxOutputTokens, len(again.Messages), len(forced.Messages))
	}
	last := again.Messages[len(again.Messages)-1]
	if want := prompt.AnswerNow(room); last.Role != llm.RoleTool || last.Parts[len(last.Parts)-1].Text != want {
		t.Errorf("asked again with %+v, want %q", last, want)
	}
	if line["turns"] != 4.0 || line["asked_again"] != true || line["outcome"] != store.OutcomePosted || line["output_tokens"].(float64) > 12000 {
		t.Errorf("the answer's line: %v", line)
	}
}

// A forced turn's one try more starts only while the input tokens are not
// spent, as a continuation does: forced by them, a model that writes
// nothing gets the budget's notice after the forced turn.
func TestNoTryMoreOnceTheInputTokensAreSpent(t *testing.T) {
	model := scripted.New(
		scripted.WithUsage(scripted.CallTool("course_get", `{}`), llm.Usage{Input: 9000, Output: 100}),
		thinksToTheCap(),
	)
	body, wk, line := cutOff(t, model, incident(perAnswer("input_tokens", 9000)))
	if body != config.DefaultBudgetText || line["asked_again"] != false {
		t.Errorf("body %q; the answer's line: %v", body, line)
	}
	if n := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "input_tokens"}); n != 1 {
		t.Errorf("budget_exhausted_total{input_tokens} = %v", n)
	}
}

// Nor does it start once the time the forced turn was given is over: a
// turn forced by a spent wall clock is given its grace (a sixth of the
// wall clock), and the try more no more than what is left of it.
func TestNoTryMoreOnceTheForcedTurnsTimeIsOver(t *testing.T) {
	slow := func(d time.Duration, s scripted.Step) scripted.Step {
		return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
			time.Sleep(d)
			return s(ctx, req)
		}
	}
	model := scripted.New(slow(400*time.Millisecond, scripted.CallTool("course_get", `{}`)), slow(100*time.Millisecond, scripted.Stop(llm.StopEnd, "")))
	body, _, line := cutOff(t, model, incident(perAnswer("wall_clock_s", 0.3)))
	if body != config.DefaultBudgetText || line["asked_again"] != false {
		t.Errorf("body %q; the answer's line: %v", body, line)
	}
}

// A model's API that refuses to be asked to think least (a 400) gets the
// forced turn again as the model is configured, and the rest of the
// answer's calls so too.
func TestAnAPIThatRefusesToThinkLeastIsAskedAsConfigured(t *testing.T) {
	refusesLeast := func(text string) scripted.Step {
		return func(_ context.Context, req *llm.Request) (*llm.Response, error) {
			if req.LeastReasoning {
				return nil, &llm.Error{Kind: llm.ErrBadRequest, Status: 400, Code: "invalid_request_error", Message: "unknown parameter: thinking"}
			}
			return &llm.Response{Parts: []llm.Part{llm.Text(text)}, Stop: llm.StopEnd}, nil
		}
	}
	model := scripted.New(scripted.CallTool("course_get", `{}`), refusesLeast(""), refusesLeast("Answered as configured."))
	body, wk, _ := cutOff(t, model, perAnswer("turns", 2))
	reqs := model.Requests()
	if body != "Answered as configured." || len(reqs) != 3 || !reqs[1].LeastReasoning || reqs[2].LeastReasoning || reqs[2].ToolMode != llm.ToolNone {
		t.Errorf("body %q, %d calls", body, len(reqs))
	}
	if !strings.Contains(wk.w.logs.String(), "refused a call asking it to think least") {
		t.Error("not logged")
	}
}
