package gemini

import (
	"strings"
)

// What the adapter knows of Gemini's models, by their ids: how each is
// asked to think, whether it thinks unasked, and whether it refuses a
// function call without its thought signature. An id that names no version
// (gemini-flash-latest, a tuned model) is asked as a 2.5 model is, which
// every later model also takes: a thinking budget, and nothing added for
// thinking it was not asked to do.

// budgets are thinkingBudget by effort, for Gemini 2.5 and every model not
// known to take thinkingLevel. Each lies within what every 2.5 model takes
// (Pro 128 to 32768, Flash 0 to 24576, Flash-Lite 512 to 24576). minimal is
// 512, not 0: 0 turns thinking off on Flash, but Pro refuses it with a 400.
var budgets = map[string]int{"minimal": 512, "low": 1024, "medium": 8192, "high": 24576}

// levels are thinkingLevel by effort, for Gemini 3 and later, which take a
// level in place of a budget. Every Gemini 3 model takes LOW, MEDIUM and
// HIGH but the first Gemini 3 Pro, which took LOW and HIGH only (see
// thinking); MINIMAL is not taken by Pro nor by the newest Flash models,
// so minimal is LOW. Which levels a model newer than those documented
// takes is [UNVERIFIED].
var levels = map[string]string{"minimal": "LOW", "low": "LOW", "medium": "MEDIUM", "high": "HIGH"}

// defaultThinkingAllowance is what maxOutputTokens allows for thinking
// beyond the answer's cap when no budget bounds it: a model thinking at a
// level, or unasked at its own default. It is high's budget: the most 2.5
// Flash ever thinks, and three quarters of the most 2.5 Pro does.
const defaultThinkingAllowance = 24576

// maxOutputCeiling is the largest maxOutputTokens the thinking models take
// (65536 for 2.5 and 3); an allowance never raises the cap above it.
const maxOutputCeiling = 65536

// skipSignature stands in for the thought signature of a function call the
// model did not make: Google documents it for a history carried over from
// another model, which Gemini 3 would otherwise refuse with a 400.
const skipSignature = "skip_thought_signature_validator"

// version is a model id's version: 2.5 in gemini-2.5-flash, 3.1 in
// models/gemini-3.1-pro-preview.
type version struct {
	major, minor int
	// known is false for an id that names no version.
	known bool
	// variant is what follows the version: -flash, -pro-preview, -flash-lite.
	variant string
}

// versionOf reads the version in a model id.
func versionOf(model string) version {
	id := model[strings.LastIndex(model, "/")+1:]
	rest, ok := strings.CutPrefix(id, "gemini-")
	if !ok {
		return version{}
	}
	major, rest, ok := number(rest)
	if !ok {
		return version{}
	}
	minor := 0
	if after, dot := strings.CutPrefix(rest, "."); dot {
		if minor, rest, ok = number(after); !ok {
			return version{}
		}
	}
	if rest != "" && rest[0] != '-' {
		return version{}
	}
	return version{major: major, minor: minor, known: true, variant: rest}
}

// number reads one to three digits at the start of s.
func number(s string) (n int, rest string, ok bool) {
	i := 0
	for i < len(s) && i < 4 && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == 0 || i > 3 {
		return 0, s, false
	}
	return n, s[i:], true
}

// takesLevel reports whether a model takes thinkingLevel: Gemini 3 and
// later. Any other id (2.5, an alias such as gemini-flash-latest, a tuned
// model) gets thinkingBudget: a level is documented for Gemini 3 only,
// while Gemini 3 still takes a budget, so a budget is the choice that
// cannot break a call.
func takesLevel(model string) bool {
	v := versionOf(model)
	return v.known && v.major >= 3
}

// checksSignatures reports whether a model refuses, with a 400, a step of
// the current turn whose first functionCall carries no thought signature:
// Gemini 3 and later. Gemini 2.5 takes calls without one.
func checksSignatures(model string) bool { return takesLevel(model) }

// thinksUnasked reports whether a model thinks when not told to: every
// Gemini 3, and 2.5 Pro and Flash. 2.5 Flash-Lite does not, nor do the
// image and speech models, nor anything older.
func thinksUnasked(model string) bool {
	v := versionOf(model)
	switch {
	case !v.known:
		return false
	case v.major >= 3:
		return true
	case v.major == 2 && v.minor == 5:
		for _, s := range []string{"-lite", "-image", "-tts"} {
			if strings.Contains(v.variant, s) {
				return false
			}
		}
		return true
	}
	return false
}

// thinking is thinkingConfig for a reasoning effort, or nil for none: the
// model's own default then stands.
func thinking(model, effort string) *wireThinking {
	if effort == "" {
		return nil
	}
	if takesLevel(model) {
		level := levels[effort]
		// The first Gemini 3 Pro refused MEDIUM; HIGH is its default.
		if v := versionOf(model); level == "MEDIUM" && v.major == 3 && v.minor == 0 && strings.HasPrefix(v.variant, "-pro") {
			level = "HIGH"
		}
		return &wireThinking{ThinkingLevel: level}
	}
	budget := budgets[effort]
	return &wireThinking{ThinkingBudget: &budget}
}

// thinkingAllowance is what maxOutputTokens allows beyond the answer's cap
// for thinking, which counts against maxOutputTokens: without it, a model
// that thinks past the cap stops at MAX_TOKENS with nothing written. It is
// the budget where one bounds thinking, defaultThinkingAllowance where a
// level or the model's default does, and 0 for a model that does not
// think.
func thinkingAllowance(model, effort string) int {
	switch {
	case effort != "" && !takesLevel(model):
		return budgets[effort]
	case effort != "" || thinksUnasked(model):
		return defaultThinkingAllowance
	}
	return 0
}

// outputCap is maxOutputTokens for an answer's cap and a thinking
// allowance: the cap and the allowance, but never above maxOutputCeiling
// unless the cap itself is.
func outputCap(limit, allowance int) int {
	if limit <= 0 || allowance <= 0 {
		return limit
	}
	return max(limit, min(limit+allowance, maxOutputCeiling))
}
