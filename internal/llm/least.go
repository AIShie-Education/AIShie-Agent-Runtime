package llm

import (
	"regexp"
	"strings"
)

// A call asking for the least reasoning (Request.LeastReasoning) must never
// make a model think more than an ordinary call would. Each adapter asks
// for the lowest setting the model takes, as its provider documents it for
// the model's family: off where it can be switched off, the lowest effort
// where it cannot, and nothing at all where the model already thinks least
// unasked (its default is no reasoning, or the lowest effort it takes). A
// model whose family is not documented here is sent no setting it was not
// configured with: a configured effort no higher than low (LeastEffort),
// else nothing, since an effort sent to a model that does not think unasked
// would make it think.

// OpenAILeastEffort is the reasoning effort that asks model, as OpenAI
// names it (an Azure deployment named for one, an id of OpenRouter's after
// openai/), to think least: the lowest effort its model page lists
// (developers.openai.com/api/docs/models and the reasoning guide, read
// 2026-10). It is "" where that lowest is what the model does unasked, or
// where the model does not reason: nothing need be sent. known is false
// for a model not listed here, whose efforts are not known.
//
//	models                                 efforts               unasked  least
//	o1, o1-pro, o3, o3-mini, o3-pro,       low, medium, high     medium   low
//	  o4-mini ("earlier reasoning models
//	  like o3 supported only low, medium,
//	  and high")
//	gpt-5, gpt-5-mini, gpt-5-nano          minimal … high        medium   minimal
//	gpt-5-pro                              high                  high     -
//	gpt-5.1                                none … high           none     -
//	gpt-5.2, gpt-5.4, gpt-5.4-mini,        none … xhigh          none     -
//	  gpt-5.4-nano
//	gpt-5.4-pro                            medium … xhigh        medium   -
//	gpt-5.2-pro, gpt-5.5-pro               medium … xhigh                 medium
//	gpt-5.2-codex, gpt-5.3-codex           low … xhigh                    low
//	gpt-5.5, gpt-5.6-luna, gpt-5.6-sol,    none … xhigh (max)    medium   none
//	  gpt-5.6-terra, gpt-6-sol, gpt-6-luna
//	gpt-6-astra, gpt-6.1-sol               low … max             medium   low
//	gpt-oss-120b, gpt-oss-20b              low, medium, high              low
//	gpt-5-chat-latest, gpt-5.x-chat-latest, (no reasoning)                -
//	  chat-latest
//
// GPT-5.1 and later default to none only where the table says so: GPT-5.5,
// 5.6 and 6 reason at medium unasked, and are asked for none. A model not
// listed (gpt-5-codex, gpt-5.1-codex, gpt-5.6-cyber, a model newer than
// these) is not guessed at.
func OpenAILeastEffort(model string) (effort string, known bool) {
	id := openAIBaseID(model)
	if id == "chat-latest" || strings.Contains(id, "-chat") {
		return "", true
	}
	effort, known = openAILeast[id]
	return effort, known
}

// openAILeast is OpenAILeastEffort's table, by base model id.
var openAILeast = map[string]string{
	// The lowest effort each takes, below what it does unasked.
	"o1": "low", "o1-pro": "low", "o3": "low", "o3-mini": "low", "o3-pro": "low", "o4-mini": "low",
	"gpt-5": "minimal", "gpt-5-mini": "minimal", "gpt-5-nano": "minimal",
	"gpt-5.2-pro": "medium", "gpt-5.5-pro": "medium",
	"gpt-5.2-codex": "low", "gpt-5.3-codex": "low",
	"gpt-5.5": "none", "gpt-5.6-luna": "none", "gpt-5.6-sol": "none", "gpt-5.6-terra": "none",
	"gpt-6-sol": "none", "gpt-6-luna": "none",
	"gpt-6-astra": "low", "gpt-6.1-sol": "low",
	"gpt-oss-120b": "low", "gpt-oss-20b": "low",
	// Least unasked already: nothing to send.
	"gpt-5-pro": "", "gpt-5.4-pro": "",
	"gpt-5.1": "", "gpt-5.2": "", "gpt-5.4": "", "gpt-5.4-mini": "", "gpt-5.4-nano": "",
}

// snapshotDate is a dated snapshot's suffix: gpt-5.1-2025-11-13.
var snapshotDate = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

// openAIBaseID is model as its family names it: lower case, a fine-tuned
// model as its base (ft:o4-mini-2025-04-16:org::id), without a router's
// variant (:batch) or a snapshot's date.
func openAIBaseID(model string) string {
	id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "ft:")
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id = id[:i]
	}
	return snapshotDate.ReplaceAllString(id, "")
}
