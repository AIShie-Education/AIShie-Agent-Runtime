package worker

import (
	"bytes"
	"encoding/json"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// What an answer relied on, as the worker posts it (design §5.3, What the
// answer relied on; toolset.Sources for what its loop gave the model).

// reposts is how many of a message's attempts, atts in order, post again
// the answer of the attempt before them without a source Core refused
// (withoutSource): no model wrote them, and they count toward no
// max_attempts, which bounds the attempts at answering. Each drops a
// source, so an answer is posted again at most core.MaxSources times.
func reposts(atts []store.Attempt) int {
	n := 0
	for i := 1; i < len(atts); i++ {
		if repostOf(atts[i-1], atts[i]) {
			n++
		}
	}
	return n
}

// repostOf reports whether at posts again prev's answer without a source
// Core refused: the next attempt after one Core refused as invalid (as it
// refuses a source), the same answer under its own key, naming one source
// fewer than prev, or saying nothing of those prev named (an empty list
// among them, which a Core older than its catalogue refuses).
func repostOf(prev, at store.Attempt) bool {
	if at.Tool != toolAnswer || prev.Tool != toolAnswer || at.No != prev.No+1 || at.Kind != prev.Kind ||
		prev.ErrorCode != core.CodeInvalidArgument {
		return false
	}
	a, had, okA := answerApart(prev.Args)
	b, has, okB := answerApart(at.Args)
	return okA && okB && had >= 0 && (has == had-1 || has < 0) && bytes.Equal(a, b)
}

// answerApart is an answer's arguments, args, without its sources and its
// idempotency key, as JSON, and how many sources it names: -1 where it
// says nothing of them.
func answerApart(args []byte) (rest []byte, sources int, ok bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return nil, 0, false
	}
	sources = -1
	if raw, named := m["sources"]; named {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			return nil, 0, false
		}
		sources = len(list)
	}
	delete(m, "sources")
	delete(m, "idempotency_key")
	rest, err := json.Marshal(m)
	return rest, sources, err == nil
}

// saidOf is what an answer to question, whose model was given the
// conversation read up to it, posts as what it relied on, from sources,
// what its own loop gave it of the course's materials
// (toolset.Sources.List). An answer given none of them would say it relied
// on none; but one given, in the conversation, an earlier answer that
// relied on some, or did not say (earlierRelied), may rest on what that
// answer read ("repeat question 2 of HW1"), which it may not name: Core
// takes what an answer read for its own question, not another's. So may
// one that writes again an answer a person sent back for changes, whose
// bytes written ahead are redone (passResult.redone, nil for none), where
// that answer relied on some or did not say (relied): "make it shorter"
// keeps what that answer read for its own attempt, which this one may
// not name either. It says nothing of its sources. A notice of the
// agent's (notice reports whether a body of its own is one; nil for
// none), which says nothing of sources either, is no answer, and read
// nothing.
func saidOf(sources []core.Source, read *core.Messages, self, question string, redone []byte, notice func(body string) bool) []core.Source {
	if sources == nil || len(sources) > 0 {
		return sources
	}
	if relied(redone) || earlierRelied(read.Messages, self, question, notice) {
		return nil
	}
	return sources
}

// relied reports whether the answer whose bytes written ahead are args
// names course materials it relied on, or says nothing of them, or cannot
// be read; nil args are no answer.
func relied(args []byte) bool {
	if args == nil {
		return false
	}
	_, n, ok := answerApart(args)
	return !ok || n != 0
}

// earlierRelied reports whether msgs, up to question, hold an answer
// that names course materials it relied on, or one of the agent's own
// (self) that says nothing of them and is no notice; a retracted message
// gives the model nothing.
func earlierRelied(msgs []core.Message, self, question string, notice func(body string) bool) bool {
	for _, m := range msgs {
		if m.ID == question {
			return false
		}
		if m.Retracted != nil {
			continue
		}
		if len(m.Sources) > 0 {
			return true
		}
		if m.Sources == nil && m.AuthorMemberID == self && (notice == nil || m.Body == nil || !notice(*m.Body)) {
			return true
		}
	}
	return false
}
