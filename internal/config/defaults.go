package config

import "strings"

// Texts the runtime posts or gives when the model's answer cannot be used.
const (
	DefaultRefusalText = "I can't help with that here. Please ask your instructor."
	DefaultBudgetText  = "I couldn't finish this one. Try a narrower question."
	DefaultQuotaText   = "I've answered as many questions as I can today. Please try again tomorrow, or ask your instructor."
	DefaultCloseReason = "I couldn't produce an answer to this after several tries, so I've closed this conversation. Please ask again, or ask your instructor."
)

// The notice posted when a quota of the school plan's is spent and the
// agent has no key of its owner's to go on with (runtime.school), in the
// language answers are fixed to, when they are; otherwise in English and
// in Traditional Chinese, the school's two, since the runtime has not read
// the question yet.
const (
	SchoolQuotaTextEn     = "Today's school AI allowance is used up. Please try again tomorrow."
	SchoolQuotaTextZhHant = "今天的學校 AI 額度已用完，請明天再試。"
	SchoolQuotaTextZhHans = "今天的学校 AI 额度已用完，请明天再试。"
)

// SchoolQuotaText is the school plan's notice for an agent whose
// prompt.answer_language is answerLanguage: text when the plan sets its
// own, else the built-in one in the language answers are fixed to (English,
// or Chinese in either script), else in English and Traditional Chinese.
func SchoolQuotaText(text, answerLanguage string) string {
	if strings.TrimSpace(text) != "" {
		return text
	}
	tag, fixed := strings.CutPrefix(answerLanguage, LanguageFixed)
	if !fixed {
		return SchoolQuotaTextEn + "\n\n" + SchoolQuotaTextZhHant
	}
	lang, rest, _ := strings.Cut(strings.ToLower(tag), "-")
	switch lang {
	case "en":
		return SchoolQuotaTextEn
	case "zh":
		// Simplified where the tag says so, or names a region that
		// writes it; Traditional otherwise.
		if strings.HasPrefix(rest, "hans") || rest == "cn" || rest == "sg" || rest == "my" {
			return SchoolQuotaTextZhHans
		}
		return SchoolQuotaTextZhHant
	}
	return SchoolQuotaTextEn + "\n\n" + SchoolQuotaTextZhHant
}

// The MCP revisions Core answers (§1.2), and the one the runtime pins.
var mcpRevisions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

// DefaultMCPProtocol is the pinned revision.
const DefaultMCPProtocol = "2025-11-25"

// Defaults are the built-in settings every agent starts from (design §9,
// which follows §4's example): MCP at the pinned revision; the owner's own
// key and 2,000 output tokens; the answer in the asker's language; tools
// derived from the seat, with no allow list (tools.allow unset or empty
// means the default list: the read tools of §2.3 and the gated writes),
// writes off (a hosted agent's registry document turns them on) and four
// calls at once; three attempts, then close; the canned notice when out of
// quota; 19,000 characters; the newest 30 messages; eight answers at once
// per agent and four per course; per answer 8 turns, 12 tool calls of which
// at most 10 writes, 150,000 input and 4,000 output tokens and 90 s; no
// daily quotas (a school key must set
// them); polling 2 s hot for 120 s, 10 s idle growing to 30 s, events every
// 45 s, seats every 300 s, ±25 %, 30 % of Core's 600 calls a minute (burst
// 100), and where Core offers wait_s each inbox waiting 25 s for a
// question, twelve calls at most waiting at once; memory on, purged 30
// days after a seat goes.
//
// Each call returns a new map, as generic YAML: maps, lists and scalars.
func Defaults() map[string]any {
	return map[string]any{
		"core": map[string]any{
			"transport":    "mcp",
			"mcp_protocol": DefaultMCPProtocol,
		},
		"model": map[string]any{
			"key_source": KeyOwn,
			"params":     map[string]any{"max_output_tokens": 2000},
		},
		"prompt": map[string]any{
			"answer_language":   LanguageOpener,
			"on_refusal_text":   DefaultRefusalText,
			"on_budget_text":    DefaultBudgetText,
			"on_quota_text":     DefaultQuotaText,
			"close_reason_text": DefaultCloseReason,
		},
		"tools": map[string]any{
			"mode":               ToolsDerived,
			"writes":             false,
			"max_parallel_tools": 4,
		},
		"answer": map[string]any{
			"max_attempts":              3,
			"on_attempts_exhausted":     OnExhaustedClose,
			"on_quota_exhausted":        OnQuotaCanned,
			"max_body_chars":            19000,
			"history_messages":          30,
			"max_concurrent":            8,
			"max_concurrent_per_course": 4,
		},
		"budgets": map[string]any{
			"per_answer": map[string]any{
				"turns":         8,
				"tool_calls":    12,
				"max_writes":    10,
				"input_tokens":  150000,
				"output_tokens": 4000,
				"wall_clock_s":  90,
			},
		},
		"polling": map[string]any{
			"inbox_hot_s":               2,
			"hot_window_s":              120,
			"inbox_idle_s":              10,
			"inbox_max_s":               30,
			"events_s":                  45,
			"memberships_s":             300,
			"jitter":                    0.25,
			"max_rate_share":            0.3,
			"assumed_core_rate_per_min": 600,
			"assumed_core_burst":        100,
			"long_poll_wait_s":          25,
			"long_poll_max":             12,
		},
		"memory": map[string]any{
			"enabled":                      true,
			"retention_days_after_removal": 30,
		},
	}
}

// Values the configuration names.
const (
	TransportMCP  = "mcp"
	TransportREST = "rest"

	KeySchool = "school"
	KeyOwn    = "own"

	LanguageOpener = "opener"
	// LanguageFixed begins answer_language when it names a language:
	// fixed:<bcp47>.
	LanguageFixed = "fixed:"

	ToolsDerived = "derived"
	ToolsNone    = "none"

	OnExhaustedClose = "close"
	OnExhaustedSkip  = "skip"

	OnQuotaCanned = "canned"
	OnQuotaSilent = "silent"
)
