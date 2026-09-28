package api

import (
	"time"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// A hosted agent's status, as its owner reads it (the API contract, §6.1).
const (
	StatusNeedsModel = "needs_model"
	StatusStarting   = "starting"
	StatusRunning    = "running"
	StatusPaused     = "paused"
	StatusNeedsToken = "needs_token"
	StatusError      = "error"
	StatusStopped    = "stopped"
)

// problemReasons are the reasons a problem may give (§4's ProblemReason):
// a state's reason outside them is said as failing.
var problemReasons = map[string]bool{
	store.ReasonTokenRefused: true, store.ReasonSettingsRejected: true, store.ReasonRuntimeMisconfigured: true,
	store.ReasonOperatorAgent: true, store.ReasonActorInUse: true, store.ReasonTokenOtherAgent: true,
	store.ReasonTokenNotAgent: true, store.ReasonOwnerChanged: true, store.ReasonCoreTooOld: true,
	store.ReasonAgentSuspended: true, store.ReasonFailing: true,
}

// maxDetail bounds a problem's detail, in characters.
const maxDetail = 500

// Problem is why a hosted agent needs a token or is in error: a reason of
// the contract's list, the worker's detail (English, redacted as it was
// written), and since when.
type Problem struct {
	Reason string    `json:"reason"`
	Detail string    `json:"detail"`
	Since  time.Time `json:"since"`
}

// statusOf works a hosted agent's status out of its row, whether it has a
// model, and the worker's state for it, st (nil for none), as the
// contract's §6.2 says: paused, then no model, then a state written for an
// older version of the row (or none) is a change not yet in force; then
// the state itself. A problem is given exactly for needs_token and error.
func statusOf(row *store.HostedAgent, hasModel bool, st *store.AgentState) (string, *Problem) {
	switch {
	case row.Paused:
		return StatusPaused, nil
	case !hasModel:
		return StatusNeedsModel, nil
	case st == nil || st.ConfigVersion < row.Version:
		return StatusStarting, nil
	}
	problem := func(reason string) *Problem {
		if !problemReasons[reason] {
			reason = store.ReasonFailing
		}
		return &Problem{Reason: reason, Detail: clipRunes(st.Detail, maxDetail), Since: st.UpdatedAt.UTC()}
	}
	switch st.State {
	case store.AgentRunning:
		return StatusRunning, nil
	case store.AgentUnauthorized:
		return StatusNeedsToken, problem(store.ReasonTokenRefused)
	case store.AgentOwnerChanged:
		return StatusError, problem(store.ReasonOwnerChanged)
	case store.AgentError:
		return StatusError, problem(st.Reason)
	case store.AgentStopped:
		return StatusStopped, nil
	}
	// starting, and a paused state the worker has not written over since
	// the agent was resumed.
	return StatusStarting, nil
}

// clipRunes is s cut to at most n characters.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
