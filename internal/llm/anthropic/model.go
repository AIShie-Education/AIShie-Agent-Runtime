package anthropic

import (
	"regexp"
	"strconv"
	"strings"
)

// family is what a model takes for thinking and sampling. Anthropic changed
// both across its model generations, and a parameter one generation needs
// is a 400 on another, so the adapter asks by model rather than sending one
// shape everywhere. As Anthropic documents it (read 2026-09):
//
//	models                     to think            unasked   budget_tokens  temperature, top_p
//	Claude 4 to 4.5 (Haiku     enabled, a budget   none      required       one of the two
//	  4.5 included), others
//	Opus 4.6, Sonnet 4.6       adaptive, effort    none      deprecated     one of the two
//	Opus 4.7, 4.8              adaptive, effort    none      400            400
//	Opus 5 on, Sonnet 5,       adaptive, effort    adaptive  400            400
//	  Fable, Mythos 5 on
//
// The adapter sends only what every model of a row takes: never both
// sampling settings, none while thinking, and never {type: disabled}.
type family struct {
	// adaptive models are asked to think with {type: adaptive} and
	// output_config.effort. budget_tokens is deprecated on the 4.6 models and
	// refused with a 400 from Opus 4.7 on.
	adaptive bool
	// thinksByDefault models think when the request says nothing about
	// thinking (Opus 5 and later, Sonnet 5, Fable, Mythos), and some refuse
	// to be told not to. The adapter never sends {type: disabled}; it leaves
	// thinking out and allows for it in max_tokens.
	thinksByDefault bool
	// noSampling models refuse temperature, top_p and top_k with a 400
	// (Opus 4.7 on).
	noSampling bool
}

// claudeID matches Anthropic's model ids from the Claude 4 generation on:
// claude-{tier}-{major}[-{minor}], then perhaps a date or a version
// (claude-sonnet-4-5-20250929, claude-opus-4-5@20251101, …-v1:0). The minor
// version has at most two digits so that a date is never read as one.
var claudeID = regexp.MustCompile(`^claude-([a-z]+)-([0-9]+)(?:-([0-9]{1,2}))?(?:[-@:].*)?$`)

// ThinksUnasked reports whether model, as Anthropic, OpenRouter
// (anthropic/claude-opus-5) or Bedrock writes its id, is a Claude that
// thinks when a request says nothing of thinking: Opus 5 and later,
// Sonnet 5, Fable, Mythos.
func ThinksUnasked(model string) bool { return familyOf(model).thinksByDefault }

// familyOf names model's family from its id, as Anthropic, OpenRouter
// (anthropic/claude-opus-4.7) or Bedrock (us.anthropic.claude-…) write it.
//
// An id it does not recognise, which is every model served in Anthropic's
// format by someone else (DeepSeek, GLM, a local server) and every Claude 3
// model, gets the older shape: thinking with a budget, and temperature or
// top_p as configured. That shape is what the servers that copy the API
// take. A Claude id newer than the ones below is taken to behave as the
// newest generation does, which is more likely than the reverse.
func familyOf(model string) family {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.LastIndex(id, "anthropic."); i >= 0 {
		id = id[i+len("anthropic."):]
	}
	id = strings.ReplaceAll(id, ".", "-")
	m := claudeID.FindStringSubmatch(id)
	if m == nil {
		return family{}
	}
	tier := m[1]
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return family{}
	}
	minor := 0
	if m[3] != "" {
		minor, _ = strconv.Atoi(m[3])
	}
	switch {
	case tier == "fable" || tier == "mythos" || major >= 5:
		return family{adaptive: true, thinksByDefault: true, noSampling: true}
	case major == 4 && minor >= 7:
		return family{adaptive: true, noSampling: true}
	case major == 4 && minor == 6:
		return family{adaptive: true}
	}
	return family{}
}
