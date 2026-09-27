package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// answerOnce runs the own agent with over merged into its settings and the
// scripted model, asks one question and waits for its answer and ledger
// row. It returns the answer's body and the worker.
func answerOnce(t *testing.T, model *scripted.Adapter, over map[string]any) (string, *worker, string) {
	t.Helper()
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{})
	conv, _ := w.ask(0, own, "Tell me about HW1.")
	got := w.waitAnswers(conv, 1)
	eventually(t, "the ledger's answer row", func() bool { return len(wk.st.outcomes(conv)) > 0 })
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	return got[0].Body, wk, conv
}

func perAnswer(kv ...any) map[string]any {
	pa := map[string]any{"wall_clock_s": 10}
	for i := 0; i+1 < len(kv); i += 2 {
		pa[kv[i].(string)] = kv[i+1]
	}
	return map[string]any{"budgets": map[string]any{"per_answer": pa}}
}

// TestBudgets: a model that keeps calling tools gets a last turn with
// ToolMode none (ForceAnswer), and its text is posted; with no text,
// on_budget_text is; as it is when the wall clock is spent.
func TestBudgets(t *testing.T) {
	t.Run("turns: the last turn is forced, and its text posted", func(t *testing.T) {
		model := scripted.New(scripted.CallTool("course_get", `{}`), scripted.CallTool("course_get", `{}`), scripted.Reply("Forced to answer."))
		body, wk, conv := answerOnce(t, model, perAnswer("turns", 3))
		reqs := model.Requests()
		if body != "Forced to answer." || reqs[0].ToolMode != llm.ToolAuto || reqs[2].ToolMode != llm.ToolNone {
			t.Errorf("body %q, tool modes %s, %s", body, reqs[0].ToolMode, reqs[2].ToolMode)
		}
		if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "turns"}); got != 1 {
			t.Errorf("budget_exhausted_total{turns} = %v", got)
		}
		if o := wk.st.outcomes(conv); o[0] != store.OutcomePosted {
			t.Errorf("outcome %v", o)
		}
	})
	t.Run("turns: no text on the last turn posts on_budget_text", func(t *testing.T) {
		model := scripted.New(scripted.CallTool("course_get", `{}`), scripted.Stop(llm.StopEnd, ""))
		body, wk, conv := answerOnce(t, model, perAnswer("turns", 2))
		if body != config.DefaultBudgetText || lastRequest(t, model).ToolMode != llm.ToolNone {
			t.Errorf("body %q", body)
		}
		if o := wk.st.outcomes(conv); o[0] != store.OutcomeBudget {
			t.Errorf("outcome %v", o)
		}
		_, recs := wk.st.ledger()
		if recs[0].Billable {
			t.Error("the budget text is billable")
		}
	})
	t.Run("tool calls: those past the budget reach nobody, and the next turn is forced", func(t *testing.T) {
		model := scripted.New(
			scripted.CallTools(scripted.ToolCall{Name: "course_get", Args: `{}`}, scripted.ToolCall{Name: "assignment_list", Args: `{}`},
				scripted.ToolCall{Name: "document_list", Args: `{}`}),
			scripted.Reply("With two of three."),
		)
		body, _, _ := answerOnce(t, model, perAnswer("tool_calls", 2))
		req := lastRequest(t, model)
		res := req.Messages[len(req.Messages)-1].Parts
		if body != "With two of three." || req.ToolMode != llm.ToolNone || len(res) != 3 || res[0].IsError || res[1].IsError ||
			!res[2].IsError || !strings.Contains(res[2].Content, "spent") {
			t.Errorf("body %q, mode %s, results %+v", body, req.ToolMode, res)
		}
	})
	t.Run("wall clock: spent, the last turn is forced", func(t *testing.T) {
		slow := func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
			time.Sleep(400 * time.Millisecond)
			return scripted.CallTool("course_get", `{}`)(ctx, req)
		}
		model := scripted.New(slow, scripted.Reply("Late, but an answer."))
		body, wk, _ := answerOnce(t, model, perAnswer("wall_clock_s", 0.3))
		if body != "Late, but an answer." || lastRequest(t, model).ToolMode != llm.ToolNone {
			t.Errorf("body %q", body)
		}
		if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "wall_clock"}); got != 1 {
			t.Errorf("budget_exhausted_total{wall_clock} = %v", got)
		}
	})
	t.Run("output tokens: the next turn is forced within what is left", func(t *testing.T) {
		model := scripted.New(
			scripted.WithUsage(scripted.CallTool("course_get", `{}`), llm.Usage{Input: 10, Output: 450}),
			scripted.Reply("Short."),
		)
		body, _, _ := answerOnce(t, model, perAnswer("output_tokens", 500))
		req := lastRequest(t, model)
		if body != "Short." || req.ToolMode != llm.ToolNone || req.Limits.MaxOutputTokens != 50 {
			t.Errorf("body %q, mode %s, cap %d", body, req.ToolMode, req.Limits.MaxOutputTokens)
		}
	})
}

// TestStops: every stop reason of §3.4 as the loop meets it.
func TestStops(t *testing.T) {
	for _, c := range []struct {
		name  string
		steps []scripted.Step
		want  string
		check func(t *testing.T, reqs []*llm.Request)
	}{
		{name: "refusal", steps: []scripted.Step{scripted.Stop(llm.StopRefusal, "")}, want: config.DefaultRefusalText},
		{name: "content_filter", steps: []scripted.Step{scripted.Stop(llm.StopContentFilter, "partly")}, want: config.DefaultRefusalText},
		{name: "content_filter as an error", steps: []scripted.Step{scripted.Fail(&llm.Error{Kind: llm.ErrContentFilter})}, want: config.DefaultRefusalText},
		{
			name:  "max_tokens with partial text: once more with twice the cap",
			steps: []scripted.Step{scripted.Stop(llm.StopMaxTokens, "Part"), scripted.Stop(llm.StopMaxTokens, "Partly longer")},
			want:  "Partly longer",
			check: func(t *testing.T, reqs []*llm.Request) {
				if reqs[1].Limits.MaxOutputTokens != 2*reqs[0].Limits.MaxOutputTokens {
					t.Errorf("caps %d then %d", reqs[0].Limits.MaxOutputTokens, reqs[1].Limits.MaxOutputTokens)
				}
			},
		},
		{
			name:  "max_tokens with no text: once more, then the answer",
			steps: []scripted.Step{scripted.Stop(llm.StopMaxTokens, ""), scripted.Reply("Whole.")},
			want:  "Whole.",
		},
		{
			name:  "max_tokens with no text twice: on_budget_text",
			steps: []scripted.Step{scripted.Stop(llm.StopMaxTokens, ""), scripted.Stop(llm.StopMaxTokens, "")},
			want:  config.DefaultBudgetText,
		},
		{
			name:  "end with no text: once more",
			steps: []scripted.Step{scripted.Stop(llm.StopEnd, "  "), scripted.Reply("Second time.")},
			want:  "Second time.",
		},
		{
			name:  "end with no text twice: on_budget_text",
			steps: []scripted.Step{scripted.Stop(llm.StopEnd, ""), scripted.Stop(llm.StopEnd, "")},
			want:  config.DefaultBudgetText,
		},
		{
			name:  "tool_error: the turn once more",
			steps: []scripted.Step{scripted.Stop(llm.StopToolError, ""), scripted.Reply("Fixed.")},
			want:  "Fixed.",
		},
		{
			name:  "context_overflow with nothing to take out: on_budget_text",
			steps: []scripted.Step{scripted.Stop(llm.StopContextOverflow, "")},
			want:  config.DefaultBudgetText,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			model := scripted.New(c.steps...)
			body, _, _ := answerOnce(t, model, nil)
			if body != c.want {
				t.Errorf("body %q, want %q", body, c.want)
			}
			if c.check != nil {
				c.check(t, model.Requests())
			}
		})
	}
}

// TestContextOverflowHalvesTheHistory: the older half of the conversation
// goes, the question stays, and the model is asked once more.
func TestContextOverflowHalvesTheHistory(t *testing.T) {
	for _, asError := range []bool{false, true} {
		overflow := scripted.Stop(llm.StopContextOverflow, "")
		if asError {
			overflow = scripted.Fail(&llm.Error{Kind: llm.ErrContextOverflow, Status: 400})
		}
		w := newWorld(t)
		own := w.ownAgent("yuki-helper", 0)
		model := scripted.New(scripted.Reply("One."), scripted.Reply("Two."), overflow, scripted.Reply("Three, shorter."))
		w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
		conv, _ := w.ask(0, own, "Question one.")
		w.waitAnswers(conv, 1)
		_, err := w.fc.FollowUp(conv, "Question two.")
		w.ok(err)
		w.waitAnswers(conv, 2)
		_, err = w.fc.FollowUp(conv, "Question three.")
		w.ok(err)
		got := w.waitAnswers(conv, 3)
		if got[2].Body != "Three, shorter." {
			t.Errorf("body %q", got[2].Body)
		}
		reqs := model.Requests()
		full, halved := reqs[2].Messages, reqs[3].Messages
		if len(full) != 5 || len(halved) >= len(full) {
			t.Fatalf("history of %d messages, then %d", len(full), len(halved))
		}
		first, last := halved[0], halved[len(halved)-1]
		if first.Role != llm.RoleUser || first.Parts[0].Text != prompt.Omitted || last.Role != llm.RoleUser ||
			last.Parts[len(last.Parts)-1].Text != "Question three." {
			t.Errorf("the halved history: %+v", halved)
		}
		if strings.Contains(requestText(reqs[3]), "Question one.") {
			t.Error("the oldest question is still there")
		}
	}
}

// TestProvidersDown: nothing is posted while the model's provider cannot
// be reached; the conversation is held back and answered when it is back;
// a fallback model answers when there is one, given the history as text;
// and after five failures on one message, on_budget_text is posted.
func TestProvidersDown(t *testing.T) {
	down := func(n int) []scripted.Step {
		var s []scripted.Step
		for range n {
			s = append(s, scripted.Fail(&llm.Error{Kind: llm.ErrOverloaded, Status: 529}))
		}
		return s
	}
	t.Run("held back, then answered", func(t *testing.T) {
		model := scripted.New(append(down(modelTries), scripted.Reply("Back up."))...)
		body, wk, conv := answerOnce(t, model, nil)
		if body != "Back up." {
			t.Errorf("body %q", body)
		}
		eventually(t, "two ledger rows", func() bool { return len(wk.st.outcomes(conv)) == 2 })
		if o := wk.st.outcomes(conv); o[0] != store.OutcomeError || o[1] != store.OutcomePosted {
			t.Errorf("outcomes %v", o)
		}
	})
	t.Run("the fallback answers, given the history as text", func(t *testing.T) {
		w := newWorld(t)
		own := w.ownAgent("yuki-helper", 0)
		primary := scripted.New(append([]scripted.Step{scripted.CallTool("course_get", `{}`)}, down(modelTries)...)...)
		fallback := scripted.New(scripted.Reply("From the fallback.")).WithName("other").WithMaker("other-maker")
		over := map[string]any{"model": map[string]any{"fallback": map[string]any{"adapter": "openai_chat", "model": "fb", "key_ref": "env://MODEL_KEY"}}}
		w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": primary, "fb": fallback}, workerOpts{})
		conv, _ := w.ask(0, own, "Anyone there?")
		got := w.waitAnswers(conv, 1)
		if got[0].Body != "From the fallback." {
			t.Errorf("body %q", got[0].Body)
		}
		req := lastRequest(t, fallback)
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if p.Type == llm.PartToolCall || p.Type == llm.PartToolResult {
					t.Errorf("the fallback was given a %s part", p.Type)
				}
			}
		}
		if !strings.Contains(requestText(req), "[called course_get") {
			t.Errorf("the fallback was not given the tool history as text:\n%s", requestText(req))
		}
		if err := primary.Err(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the fifth failure posts on_budget_text", func(t *testing.T) {
		model := scripted.New(down(modelTries * maxProviderFailures)...)
		body, wk, conv := answerOnce(t, model, nil)
		if body != config.DefaultBudgetText {
			t.Errorf("body %q", body)
		}
		eventually(t, "every row", func() bool { return len(wk.st.outcomes(conv)) == maxProviderFailures })
		o := wk.st.outcomes(conv)
		if o[len(o)-1] != store.OutcomeBudget {
			t.Errorf("outcomes %v", o)
		}
	})
}

// TestFixRegeneratesShorter: Core refuses a body the runtime had to cut or
// strip; it is written again, shorter, under the next attempt.
func TestFixRegeneratesShorter(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	long := strings.Repeat("This sentence goes on. ", 20)
	model := scripted.New(scripted.Reply(long), scripted.Reply("Short now."))
	var refused bool
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", map[string]any{"answer": map[string]any{"max_body_chars": 100}}, nil)),
		models{"m1": model}, workerOpts{edit: wrapCaller(func(next core.Caller) core.Caller {
			return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
				if tool == toolAnswer && !refused {
					refused = true
					return &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: core.CodeInvalidArgument, Message: "too long"}}, nil
				}
				return next.Call(ctx, tool, args)
			})
		})})
	conv, msg := w.ask(0, own, "Write me an essay.")
	got := w.waitAnswers(conv, 1)
	if got[0].Body != "Short now." || got[0].IdempotencyKey != core.AnswerKey(conv, msg, 2) {
		t.Errorf("answer %+v", got[0])
	}
	if !strings.Contains(lastRequest(t, model).System, "shorter") {
		t.Error("the second attempt was not asked to be shorter")
	}
	eventually(t, "two rows", func() bool { return len(wk.st.outcomes(conv)) == 2 })
}
