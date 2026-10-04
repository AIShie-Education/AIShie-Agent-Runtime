package worker

import (
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// The runtime's notices, which it posts in place of an answer (design
// §5.3): of a spent budget or every provider down (kindBudget), of a
// refusal (kindRefusal), of a spent quota (kindQuota). Each is the
// agent's own text where its configuration sets one, posted as it is;
// else the built-in one (config.BudgetNotice, …) in the asker's language
// as far as the runtime can tell it: the language prompt.answer_language
// fixes, else the asker's question's (config.QuestionLang); else in
// English and Traditional Chinese.

// notice is the text of the notice of kind, in the claim's language
// (c.lang).
func (c *claim) notice(kind string) string {
	p := c.eff.Prompt
	switch kind {
	case kindRefusal:
		return config.NoticeText(p.OnRefusalText, config.RefusalNotice, c.lang)
	case kindQuota:
		return config.NoticeText(p.OnQuotaText, config.QuotaNotice, c.lang)
	}
	return config.NoticeText(p.OnBudgetText, config.BudgetNotice, c.lang)
}

// askerLang is the language of a notice to question in the conversation
// read: the one answer_language fixes, else the question's; where the
// question gives none (a file, "?"), the asker's newest message before it
// that does.
func (c *claim) askerLang(read *core.Messages, question string) config.Lang {
	fixed := config.NoticeLangOf(c.eff.Prompt.AnswerLanguage, "")
	if fixed != config.LangUnknown || strings.HasPrefix(c.eff.Prompt.AnswerLanguage, config.LanguageFixed) {
		return fixed
	}
	upTo := len(read.Messages)
	for i, m := range read.Messages {
		if m.ID == question {
			upTo = i + 1
			break
		}
	}
	opener := c.openerOf(read)
	for i := upTo - 1; i >= 0; i-- {
		m := read.Messages[i]
		if m.AuthorMemberID != opener || m.Retracted != nil || m.Body == nil {
			continue
		}
		if l := config.QuestionLang(*m.Body); l != config.LangUnknown {
			return l
		}
	}
	return config.LangUnknown
}
