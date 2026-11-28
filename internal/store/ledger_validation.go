package store

import (
	"errors"
	"fmt"
)

// Kinds of model call retained in the ledger. CallTranscription remains only
// so historical rows and cost reports stay readable after Media was removed.
const (
	CallAnswer        = "model_calls"
	CallTranscription = "transcription"
)

// KindOf is a call's kind as the ledger keeps it: CallAnswer for "".
func (c LLMCall) KindOf() string {
	if c.Kind == "" {
		return CallAnswer
	}
	return c.Kind
}

// CheckCall refuses a malformed or unknown ledger row.
func CheckCall(c LLMCall) error {
	switch {
	case c.ID == "":
		return errors.New("store: llm call: id required")
	case c.KindOf() == CallAnswer && c.AgentID == "":
		return fmt.Errorf("store: llm call %s: agent_id required", c.ID)
	case c.KindOf() == CallTranscription && (c.AgentID != "" || c.TenantID != "" || c.CourseID != "" || c.OpenerMemberID != ""):
		return fmt.Errorf("store: llm call %s: a transcription's is no agent's, tenant's, course's or asker's", c.ID)
	case c.KindOf() != CallAnswer && c.KindOf() != CallTranscription:
		return fmt.Errorf("store: llm call %s: kind %q is neither %s nor %s", c.ID, c.Kind, CallAnswer, CallTranscription)
	}
	return nil
}
