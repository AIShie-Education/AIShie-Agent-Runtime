package worker

import (
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
)

// The runtime's notices are in the asker's language: the one
// answer_language fixes, where it fixes English or Chinese; else the
// question's; else, where neither tells it, in English and Traditional
// Chinese. An agent's own text is posted as it is.
func TestNoticesAreInTheAskersLanguage(t *testing.T) {
	refuses := func() *scripted.Adapter { return scripted.New(scripted.Stop(llm.StopRefusal, "")) }
	for _, c := range []struct {
		name, question string
		over           map[string]any
		model          *scripted.Adapter
		want           string
	}{
		{"a refusal, asked in Traditional Chinese", "幫我寫這篇論文。", nil, refuses(), config.RefusalNotice.ZhHant},
		{"a refusal, asked in Simplified Chinese", "帮我写这篇论文。", nil, refuses(), config.RefusalNotice.ZhHans},
		{"a refusal, asked in English", "Write my essay.", nil, refuses(), config.RefusalNotice.En},
		{"a refusal, asked in Simplified Chinese with a Greek letter", "λ 演算是什么？请解释一下", nil, refuses(), config.RefusalNotice.ZhHans},
		{"a refusal, asked in English with a Greek letter", "What is λ calculus?", nil, refuses(), config.RefusalNotice.En},
		{"a refusal, asked in Japanese", "作文を書いてください。", nil, refuses(), config.RefusalNotice.In(config.LangUnknown)},
		{"a refusal, answers fixed to Simplified Chinese", "Write my essay.", map[string]any{"prompt": map[string]any{"answer_language": "fixed:zh-Hans"}},
			refuses(), config.RefusalNotice.ZhHans},
		{"a refusal, answers fixed to Japanese", "Write my essay.", map[string]any{"prompt": map[string]any{"answer_language": "fixed:ja"}},
			refuses(), config.RefusalNotice.In(config.LangUnknown)},
		{"the agent's own refusal text", "幫我寫這篇論文。", map[string]any{"prompt": map[string]any{"on_refusal_text": "Ask Ms Sato."}},
			refuses(), "Ask Ms Sato."},
		{"a spent budget, asked in Simplified Chinese", "有什么问题吗?", perAnswer("turns", 1), scripted.New(scripted.Stop(llm.StopEnd, ""),
			scripted.Stop(llm.StopEnd, "")), config.BudgetNotice.ZhHans},
		{"the agent's own budget text", "有什么问题吗?", mergeMaps(perAnswer("turns", 1), map[string]any{"prompt": map[string]any{"on_budget_text": "Too broad."}}),
			scripted.New(scripted.Stop(llm.StopEnd, ""), scripted.Stop(llm.StopEnd, "")), "Too broad."},
	} {
		t.Run(c.name, func(t *testing.T) {
			if body, _, _ := answerTo(t, c.question, c.model, c.over); body != c.want {
				t.Errorf("body %q, want %q", body, c.want)
			}
		})
	}
}

// A question that tells no language ("?", a file) gets the notice in the
// language of the asker's message before it.
func TestANoticeToAQuestionOfNoLanguageIsInTheAskersEarlierOne(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("交到 Moodle。"), scripted.Stop(llm.StopRefusal, ""))
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	conv, _ := w.ask(0, own, "這份作業怎麼交？")
	w.waitAnswers(conv, 1)
	_, err := w.fc.FollowUp(conv, "？")
	w.ok(err)
	if got := w.waitAnswers(conv, 2)[1].Body; got != config.RefusalNotice.ZhHant {
		t.Errorf("body %q, want %q", got, config.RefusalNotice.ZhHant)
	}
}

// The notice posted after every provider failed five times on a message
// is the budget's, in the asker's language.
func TestTheProvidersDownNoticeIsInTheAskersLanguage(t *testing.T) {
	var down []scripted.Step
	for range modelTries * maxProviderFailures {
		down = append(down, scripted.Fail(&llm.Error{Kind: llm.ErrOverloaded, Status: 529}))
	}
	if body, _, _ := answerTo(t, "这周的作业是什么？", scripted.New(down...), nil); body != config.BudgetNotice.ZhHans {
		t.Errorf("body %q, want %q", body, config.BudgetNotice.ZhHans)
	}
}

// The quota's notice, posted before the question is read, is in the
// language answers are fixed to.
func TestTheQuotaNoticeIsInTheFixedLanguage(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	model := scripted.New(scripted.Reply("Your one answer today."))
	over := map[string]any{"budgets": map[string]any{"per_asker_day": map[string]any{"answers": 1}},
		"prompt": map[string]any{"answer_language": "fixed:zh-Hans"}}
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", over, nil)), models{"m1": model}, workerOpts{})
	c1, _ := w.ask(0, tu, "First question.")
	w.waitAnswers(c1, 1)
	eventually(t, "the first answer in the ledger", func() bool { return len(wk.st.outcomes(c1)) == 1 })
	c2, _ := w.ask(0, tu, "Second question.")
	if got := w.waitAnswers(c2, 1)[0].Body; got != config.QuotaNotice.ZhHans {
		t.Errorf("body %q, want %q", got, config.QuotaNotice.ZhHans)
	}
}
