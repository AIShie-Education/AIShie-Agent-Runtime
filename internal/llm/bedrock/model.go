package bedrock

import (
	"regexp"
	"strconv"
	"strings"
)

// family is what the model behind a Bedrock model id takes, where Converse
// passes a field through to it or leaves it to the model to accept.
// Anthropic changed thinking and sampling across its generations, and a
// field one generation needs is a 400 on another, so the adapter asks by
// model rather than sending one shape to every Claude.
type family struct {
	// anthropic: the id names one of Anthropic's models.
	anthropic bool
	// thinks: the model can be asked to think. Claude 3.7 and every later
	// Claude can; Claude 3, 3.5 and older refuse the thinking field.
	thinks bool
	// adaptive models are asked with {type: adaptive} and
	// output_config.effort: budget_tokens is deprecated on the 4.6 models
	// and refused from Opus 4.7 on.
	adaptive bool
	// thinksByDefault models think when the request says nothing about
	// thinking (Opus 5 and later, Sonnet 5, Fable, Mythos), as the
	// anthropic adapter knows them.
	thinksByDefault bool
	// noSampling models refuse temperature and top_p (Opus 4.7 on).
	noSampling bool
	// toolStatus: the model takes toolResult.status. AWS documents the
	// field for Anthropic's and Amazon's Nova models only, and some others
	// refuse a request that carries it; without it, Core's envelope still
	// begins with its own status.
	toolStatus bool
}

// claudeID matches Anthropic's ids from the Claude 4 generation on, once
// Bedrock's prefixes are gone: claude-{tier}-{major}[-{minor}], then a
// date or a version (claude-sonnet-4-5-20250929-v1:0). The minor version
// has at most two digits so that a date is never read as one.
var claudeID = regexp.MustCompile(`^claude-([a-z]+)-([0-9]+)(?:-([0-9]{1,2}))?(?:[-@:].*)?$`)

// oldClaude matches the Claude ids from before thinking: Claude 3 and 3.5,
// Claude 2 and Instant.
var oldClaude = regexp.MustCompile(`^claude-(?:instant|v\d|2|3-(?:haiku|sonnet|opus)|3-5-)`)

// familyOf names what a Bedrock model id is: a foundation model
// (anthropic.claude-…), a cross-region inference profile
// (us.anthropic.claude-…, global.…) or an ARN holding either. An
// application inference profile's ARN does not say which model it serves;
// it is treated as no known family, which sends none of the fields a model
// might refuse.
func familyOf(model string) family {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if strings.Contains(id, "amazon.nova") {
		return family{toolStatus: true}
	}
	if !strings.Contains(id, "anthropic.") && !strings.Contains(id, "claude") {
		return family{}
	}
	f := family{anthropic: true, thinks: true, toolStatus: true}
	if i := strings.LastIndex(id, "anthropic."); i >= 0 {
		id = id[i+len("anthropic."):]
	}
	if oldClaude.MatchString(id) {
		f.thinks = false
		return f
	}
	m := claudeID.FindStringSubmatch(strings.ReplaceAll(id, ".", "-"))
	if m == nil {
		// Claude 3.7, or an id of a shape not known here: the older
		// thinking shape, which Claude 3.7 and 4 take.
		return f
	}
	tier := m[1]
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return f
	}
	minor := 0
	if m[3] != "" {
		minor, _ = strconv.Atoi(m[3])
	}
	switch {
	case tier == "fable" || tier == "mythos" || major >= 5:
		f.adaptive, f.thinksByDefault, f.noSampling = true, true, true
	case major == 4 && minor >= 7:
		f.adaptive, f.noSampling = true, true
	case major == 4 && minor == 6:
		f.adaptive = true
	}
	return f
}
