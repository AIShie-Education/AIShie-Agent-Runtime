package config

import (
	"strings"
	"unicode"
)

// The runtime's notices: the texts it posts in place of an answer when a
// budget is spent or every provider is down (on_budget_text), when the
// model refuses (on_refusal_text), and when a quota is spent
// (on_quota_text, and the school plan's own). An agent's own text, when
// its configuration sets one, is posted as it is. Otherwise the built-in
// one is, in the asker's language where the runtime can tell it
// (NoticeLangOf): English, or Chinese in either script; else in English
// and Traditional Chinese, the school's two.

// Lang is a language the runtime writes its built-in notices in.
type Lang int

const (
	// LangUnknown is a language the runtime cannot tell, or has no notice
	// in: the notice is in English and in Traditional Chinese.
	LangUnknown Lang = iota
	LangEn
	LangZhHant
	LangZhHans
)

// String is the language's BCP 47 tag, for logs; "" for LangUnknown.
func (l Lang) String() string {
	switch l {
	case LangEn:
		return "en"
	case LangZhHant:
		return "zh-Hant"
	case LangZhHans:
		return "zh-Hans"
	}
	return ""
}

// Notice is a built-in notice in each language the runtime writes.
type Notice struct {
	En, ZhHant, ZhHans string
}

// In is the notice in l: in English and Traditional Chinese, a paragraph
// each, for LangUnknown.
func (n Notice) In(l Lang) string {
	switch l {
	case LangEn:
		return n.En
	case LangZhHant:
		return n.ZhHant
	case LangZhHans:
		return n.ZhHans
	}
	return n.En + "\n\n" + n.ZhHant
}

// All is every text the notice is ever posted as: in each language, and
// in English and Traditional Chinese together.
func (n Notice) All() []string {
	return []string{n.En, n.ZhHant, n.ZhHans, n.In(LangUnknown)}
}

// The built-in notices. Each English one is the text the runtime posted
// before it wrote any other language (DefaultBudgetText, …).
var (
	BudgetNotice = Notice{
		En:     DefaultBudgetText,
		ZhHant: "這題我未能完成，請試試問得具體一點。",
		ZhHans: "这题我没能完成，请试着问得具体一点。",
	}
	RefusalNotice = Notice{
		En:     DefaultRefusalText,
		ZhHant: "這個我無法在這裡協助，請向你的老師查詢。",
		ZhHans: "这个我无法在这里帮忙，请向你的老师询问。",
	}
	QuotaNotice = Notice{
		En:     DefaultQuotaText,
		ZhHant: "今天我能回答的問題已達上限。請明天再試，或向你的老師查詢。",
		ZhHans: "今天我能回答的问题已达上限。请明天再试，或向你的老师询问。",
	}
	SchoolQuotaNotice = Notice{En: SchoolQuotaTextEn, ZhHant: SchoolQuotaTextZhHant, ZhHans: SchoolQuotaTextZhHans}
)

// NoticeText is a notice's text: configured, the agent's own, as it is
// when it holds any text; else the built-in notice n in l.
func NoticeText(configured string, n Notice, l Lang) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return n.In(l)
}

// NoticeLangOf is the language of a notice to question, for an agent
// whose prompt.answer_language is answerLanguage: the language it fixes,
// where that is English or Chinese; otherwise, where answers are in the
// asker's language, the question's (QuestionLang); else LangUnknown. A
// notice posted before the question is read (the quota's) is given "".
func NoticeLangOf(answerLanguage, question string) Lang {
	tag, fixed := strings.CutPrefix(answerLanguage, LanguageFixed)
	if !fixed {
		return QuestionLang(question)
	}
	lang, rest, _ := strings.Cut(strings.ToLower(tag), "-")
	switch lang {
	case "en":
		return LangEn
	case "zh":
		// Simplified where the tag says so, or names a region that
		// writes it; Traditional otherwise.
		if strings.HasPrefix(rest, "hans") || rest == "cn" || rest == "sg" || rest == "my" {
			return LangZhHans
		}
		return LangZhHant
	}
	return LangUnknown
}

// QuestionLang tells the language of question from its script, as far as
// the runtime's notices need it: Chinese where it holds Han characters
// and no Japanese kana nor Korean hangul, Simplified where more of them
// are of the characters only Simplified Chinese writes than of those only
// Traditional writes (simplifiedOnly, traditionalOnly, and Zhuyin as
// Traditional), Traditional otherwise; English where it holds Latin letters and no other script
// (any language in Latin script gets the English notice); LangUnknown for
// anything else, and for no letters at all.
func QuestionLang(question string) Lang {
	var han, latin, other, simp, trad int
	for _, r := range question {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
			switch {
			case strings.ContainsRune(simplifiedOnly, r):
				simp++
			case strings.ContainsRune(traditionalOnly, r):
				trad++
			}
		case unicode.Is(unicode.Bopomofo, r):
			// Zhuyin, which Taiwan writes Traditional Chinese with.
			han++
			trad++
		case unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			other++
		case unicode.Is(unicode.Latin, r):
			latin++
		case unicode.IsLetter(r):
			other++
		}
	}
	switch {
	case other > 0:
		return LangUnknown
	case han > 0 && simp > trad:
		return LangZhHans
	case han > 0:
		return LangZhHant
	case latin > 0:
		return LangEn
	}
	return LangUnknown
}

// simplifiedOnly and traditionalOnly are common characters that one
// script of Chinese writes and the other does not, each the other's form
// of the same character, in the same order: 这 for 這, 么 for 麼, 问 for
// 問. Characters both scripts write, though one also stands for another
// in Simplified (后 for 後, 里 for 裡, 发 for 髮 aside, 干, 台, 只, 面,
// 系, 云, 几, 才, 着, 准, 余, 冲, 范, 万), are left out: they say
// nothing of the script. So is anything either script writes rarely
// enough not to matter here.
const (
	simplifiedOnly  = "这个们来时为说会对学么问题吗没过还进动发经现实话让认识课书写读记讲请谢门开关间长东车见觉机电网页头习练试验应该样种点从两当与业帮难简单杂给节历数计论设处务区产线级组织结构极广华国语汉词义释选择确错误谁师视观听买卖钱银铁贵费资质变达运远边无尔气热爱体图报场块坏声备将尽层岁岛带张弹归录态总战担换损际阶随险隐须顺领风飞饭马鱼鸟龙齐齿证评诉详调谈购赛转输办迟遗钟链闻阅队阳阴陆预频类显养驱参号复于"
	traditionalOnly = "這個們來時為說會對學麼問題嗎沒過還進動發經現實話讓認識課書寫讀記講請謝門開關間長東車見覺機電網頁頭習練試驗應該樣種點從兩當與業幫難簡單雜給節歷數計論設處務區產線級組織結構極廣華國語漢詞義釋選擇確錯誤誰師視觀聽買賣錢銀鐵貴費資質變達運遠邊無爾氣熱愛體圖報場塊壞聲備將盡層歲島帶張彈歸錄態總戰擔換損際階隨險隱須順領風飛飯馬魚鳥龍齊齒證評訴詳調談購賽轉輸辦遲遺鐘鏈聞閱隊陽陰陸預頻類顯養驅參號復於後裡臺隻麵髮係"
)
