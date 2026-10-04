package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The script heuristic: Chinese by its Han characters, Simplified or
// Traditional by the characters only one of them writes; English by Latin
// letters alone; anything else, or nothing to go by, unknown. A formula's
// Greek letters, and letters of no one script (µ, ℓ, ℝ), say nothing.
func TestQuestionLang(t *testing.T) {
	for _, c := range []struct {
		question string
		want     Lang
	}{
		// The question of the answers of 2026-10-04 on test.aishie.app.
		{"有什么问题吗?", LangZhHans},
		{"有什麼問題嗎？", LangZhHant},
		{"这个作业怎么做", LangZhHans},
		{"這個作業怎麼做", LangZhHant},
		{"HW1 第二题怎么写？", LangZhHans},
		{"HW1 第二題怎麼寫？", LangZhHant},
		{"請問 deadline 是幾時?", LangZhHant},
		// Characters both scripts write say nothing: Traditional, the
		// school's.
		{"你好", LangZhHant},
		{"作业", LangZhHans},
		{"ㄅㄆㄇ是什麼", LangZhHant},
		// More of one script's characters than of the other's decides.
		{"这这这個", LangZhHans},
		{"這這這个", LangZhHant},
		{"这個", LangZhHant},
		{"What is due on Friday?", LangEn},
		{"¿Qué hay que entregar el viernes?", LangEn},
		{"Ｗｈａｔ？", LangEn},
		{"宿題はいつまでですか", LangUnknown},
		{"숙제는 언제까지예요?", LangUnknown},
		{"Когда сдавать?", LangUnknown},
		{"HW1 课题 и задача", LangUnknown},
		// A formula's letters say nothing, in Chinese or in English.
		{"λ 演算是什么？请解释一下", LangZhHans},
		{"这道题里的 θ 怎么求", LangZhHans},
		{"如何计算 Δx？", LangZhHans},
		{"这道题的答案是 α+β 吗", LangZhHans},
		{"这个公式里的 σ 是什么意思", LangZhHans},
		{"Σ 和 π 這兩個符號是什麼意思？", LangZhHant},
		{"ℝ 上的 ℓ² 空間是什麼", LangZhHant},
		{"求 θ 的值", LangZhHant},
		{"What is λ calculus?", LangEn},
		{"What does µ mean here?", LangEn},
		{"Is ℏ the same as h/2π?", LangEn},
		{"θ = ?", LangUnknown},
		// A question in Greek is in no language the runtime writes, and
		// with Latin letters in it is taken for English.
		{"Τι σημαίνει αυτό;", LangUnknown},
		{"Τι είναι το API;", LangEn},
		{"?", LangUnknown},
		{"123 👍", LangUnknown},
		{"", LangUnknown},
	} {
		if got := QuestionLang(c.question); got != c.want {
			t.Errorf("QuestionLang(%q) = %q, want %q", c.question, got, c.want)
		}
	}
}

// simplifiedOnly and traditionalOnly are pairs, each character the other
// script's form of the one beside it, and none in both.
func TestScriptListsArePairs(t *testing.T) {
	simp, trad := []rune(simplifiedOnly), []rune(traditionalOnly)
	if len(simp) > len(trad) {
		t.Fatalf("%d Simplified, %d Traditional", len(simp), len(trad))
	}
	seen := map[rune]string{}
	for i, r := range simp {
		if r == trad[i] {
			t.Errorf("%c is in both", r)
		}
		seen[r] = "simplified"
	}
	for _, r := range trad {
		if seen[r] != "" {
			t.Errorf("%c is in both", r)
		}
		seen[r] = "traditional"
	}
	if n := utf8.RuneCountInString(simplifiedOnly); n < 150 {
		t.Errorf("only %d characters to tell the scripts by", n)
	}
}

// The language a notice is written in: the one answer_language fixes,
// where the runtime writes it; else the question's; else unknown.
func TestNoticeLangOf(t *testing.T) {
	for _, c := range []struct {
		answerLanguage, question string
		want                     Lang
	}{
		{LanguageOpener, "有什么问题吗?", LangZhHans},
		{LanguageOpener, "Any problems?", LangEn},
		{LanguageOpener, "", LangUnknown},
		{"fixed:en", "有什么问题吗?", LangEn},
		{"fixed:en-GB", "", LangEn},
		{"fixed:zh-Hant", "有什么问题吗?", LangZhHant},
		{"fixed:zh-TW", "", LangZhHant},
		{"fixed:zh", "", LangZhHant},
		{"fixed:zh-Hans", "Any problems?", LangZhHans},
		{"fixed:zh-hans-CN", "", LangZhHans},
		{"fixed:zh-CN", "", LangZhHans},
		{"fixed:zh-SG", "", LangZhHans},
		{"fixed:ja", "Any problems?", LangUnknown},
	} {
		if got := NoticeLangOf(c.answerLanguage, c.question); got != c.want {
			t.Errorf("NoticeLangOf(%q, %q) = %q, want %q", c.answerLanguage, c.question, got, c.want)
		}
	}
}

// A built-in notice in each language, in English and Traditional Chinese
// where the language is unknown; an agent's own text as it is.
func TestNoticeText(t *testing.T) {
	for _, n := range []Notice{BudgetNotice, RefusalNotice, QuotaNotice, SchoolQuotaNotice} {
		if n.En == "" || n.ZhHant == "" || n.ZhHans == "" || n.ZhHant == n.ZhHans {
			t.Errorf("a notice missing a language: %+v", n)
		}
		if got := NoticeText("", n, LangUnknown); got != n.En+"\n\n"+n.ZhHant {
			t.Errorf("unknown: %q", got)
		}
		for l, want := range map[Lang]string{LangEn: n.En, LangZhHant: n.ZhHant, LangZhHans: n.ZhHans} {
			if got := NoticeText("  ", n, l); got != want {
				t.Errorf("%s: %q, want %q", l, got, want)
			}
			if got := NoticeText("Ask Ms Sato.", n, l); got != "Ask Ms Sato." {
				t.Errorf("%s: the agent's own text became %q", l, got)
			}
		}
		// Each Chinese notice is in its script.
		if QuestionLang(n.ZhHans) != LangZhHans || QuestionLang(n.ZhHant) != LangZhHant || QuestionLang(n.En) != LangEn {
			t.Errorf("%+v is not in the scripts it says", n)
		}
	}
	if BudgetNotice.En != "I couldn't finish this one. Try a narrower question." {
		t.Errorf("the English budget notice changed: %q", BudgetNotice.En)
	}
	if !strings.Contains(SchoolQuotaText("", "fixed:zh-Hans"), "额度") || SchoolQuotaText("", LanguageOpener) != SchoolQuotaTextEn+"\n\n"+SchoolQuotaTextZhHant {
		t.Error("the school's quota notice")
	}
}
