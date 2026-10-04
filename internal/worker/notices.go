package worker

import (
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/safety"
)

// The runtime's notices, which it posts in place of an answer (design
// §5.3): of a spent budget or every provider down (kindBudget), of a
// refusal (kindRefusal), of a spent quota (kindQuota). Each is the
// agent's own text where its configuration sets one, posted as it is;
// else the built-in one (config.BudgetNotice, …) in the asker's language
// as far as the runtime can tell it: the language prompt.answer_language
// fixes, else the asker's question's (config.QuestionLang); else in
// English and Traditional Chinese. A notice is no answer: it is posted
// saying nothing of sources (claim.post), and an earlier one in the
// conversation is not taken for an answer that may rest on materials
// (saidOf).

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

// isNotice reports whether body, a message of the agent's own, is one of
// its notices: the agent's own texts, or a built-in notice in any of its
// languages, as written or as made safe to post.
func (c *claim) isNotice(body string) bool {
	if c.notices == nil {
		p := c.eff.Prompt
		texts := []string{p.OnBudgetText, p.OnRefusalText, p.OnQuotaText, c.a.s.school().OnQuotaText}
		for _, n := range []config.Notice{config.BudgetNotice, config.RefusalNotice, config.QuotaNotice, config.SchoolQuotaNotice} {
			texts = append(texts, n.All()...)
		}
		c.notices = map[string]bool{}
		for _, t := range texts {
			if t = strings.TrimSpace(t); t != "" {
				safe, _ := safety.Body(t, c.eff.Answer.MaxBodyChars)
				c.notices[t], c.notices[strings.TrimSpace(safe)] = true, true
			}
		}
	}
	body = strings.TrimSpace(body)
	return body != "" && c.notices[body]
}
