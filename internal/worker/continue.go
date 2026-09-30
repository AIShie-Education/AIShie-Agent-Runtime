package worker

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/safety"
)

// An answer the output cap of one model call cut off (docs/design.md §5.3
// step 7). A turn that stops at max_tokens with text is not written again:
// the loop continues it. The model is given the answer so far as its own
// message, then the runtime's word to go on from exactly where it stops
// (prompt.Continue), with no tools (ToolMode none); what it writes is
// joined to the answer, and so on while it stops at the cap and there is
// room. Every adapter takes that, as it takes the conversation's own
// turns. An assistant prefill, where a provider has one, would be neater,
// but the models that think refuse it, and the rest of the APIs have none.
//
// A continuation is not a turn: turns bound the rounds of tool calls before
// the answer, and a turn cut off at the cap is the same turn, written
// further. It is a model call of its own in the ledger, within the answer's
// other budgets: it writes within what is left of the output tokens, starts
// only before the wall clock is spent and while the input tokens are not,
// and adds to an answer held to answer.max_body_chars, with room kept for
// on_truncated_text. Its room is the least of what is left of each, the
// wall clock's and the body's at the pace, and the characters a token,
// the answer has been written at so far. One whose room cannot hold a whole
// cap, or whose input spends the input tokens, is the last: the model is
// told its room, to bring the answer to a close within it, and, if it
// cannot, to end by saying that the answer was cut short and that the asker
// can reply "continue" for the rest. Like a forced turn, it is given
// min(wall clock / 6, 15 s) if the wall clock runs out while it writes.
//
// An answer the cap still cuts off with no room left, or whose continuation
// fails, is posted as the model's, cut to leave room for on_truncated_text,
// with the note after it: an answer never ends mid-sentence unexplained.

const (
	// minContinuation is the least room a continuation is made with, in
	// output tokens: less cannot finish a sentence and say the answer was
	// cut short.
	minContinuation = 100
	// minOverlap and maxOverlap bound, in bytes, what joinPiece takes for a
	// continuation beginning again with the end of the answer so far: two
	// Chinese characters, or six letters.
	minOverlap = 6
	maxOverlap = 2000
)

// Why an answer was cut short, beside the budgets' names.
const (
	cutBody   = "body"
	cutFailed = "failed"
)

// budgetTruncated is budget_exhausted_total's label for an answer posted
// cut short, with on_truncated_text after it.
const budgetTruncated = "truncated"

// continuation is an answer being continued.
type continuation struct {
	// text is the answer so far: its pieces, joined.
	text string
	// room is what the next continuation may write, in output tokens;
	// last, that it is the last, and is told room; why, what makes it so.
	room int
	last bool
	why  string
	// tokens and took are the output tokens and the time of the calls
	// that wrote the pieces, and textTokens their tokens of text: the
	// pace the answer is written at.
	tokens, textTokens int64
	took               time.Duration
}

// wrote counts a piece's call in the pace.
func (c *continuation) wrote(took time.Duration, u llm.Usage) {
	c.tokens += u.Output
	c.textTokens += u.Output - min(max(u.Reasoning, 0), u.Output)
	c.took += took
}

// continueAnswer continues the answer whose turn the output cap cut off
// after text, a piece at a time, until the model ends it or there is no
// room for more, and ends the loop with it.
func (l *loop) continueAnswer(ctx context.Context, text string) loopEnd {
	c := &continuation{text: text}
	c.wrote(l.lastTook, l.lastUsage)
	for {
		if why := l.roomFor(c); why != "" {
			return l.cutShort(c.text, why)
		}
		l.c.s.log.Info("the model's output cap cut the answer off: it is continued", "conversation", l.c.conv,
			"continuation", l.stats.Continuations+1, "room", c.room, "last", c.last)
		l.d.continues(c.text)
		l.cont = c
		resp, err := l.call(ctx, true)
		l.cont = nil
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return loopEnd{fatal: ctx.Err()}
			case kindOf(err) == llm.ErrContentFilter:
				return l.refused()
			}
			l.c.s.log.Warn("a continuation of the answer failed", "conversation", l.c.conv, "err_kind", kindOf(err))
			return l.cutShort(c.text, cutFailed)
		}
		l.stats.Continuations++
		c.wrote(l.lastTook, resp.Usage)
		piece := resp.Text()
		if strings.TrimSpace(piece) != "" {
			c.text = joinPiece(c.text, piece)
		}
		switch resp.Stop {
		case llm.StopEnd:
			return l.body(strings.TrimSpace(c.text))
		case llm.StopToolCalls:
			// Told not to call tools, it did: the answer ends with what it
			// wrote before them, if anything.
			if strings.TrimSpace(piece) != "" {
				return l.body(strings.TrimSpace(c.text))
			}
			return l.cutShort(c.text, string(resp.Stop))
		case llm.StopMaxTokens:
		case llm.StopContentFilter, llm.StopRefusal:
			return l.refused()
		default:
			return l.cutShort(c.text, string(resp.Stop))
		}
	}
}

// roomFor sets the room of c's next continuation, and whether it is the
// last; or it says why none is made: c's last was, the input tokens or the
// wall clock are spent, or what is left of the output tokens, the wall
// clock or the body cannot hold minContinuation.
func (l *loop) roomFor(c *continuation) string {
	if c.last {
		return c.why
	}
	now := l.c.a.now()
	switch {
	case l.stats.In >= l.b.InputTokens:
		return "input_tokens"
	case !now.Before(l.deadline):
		return "wall_clock"
	}
	room, why := l.outLeft(), "output_tokens"
	if c.tokens > 0 && c.took > 0 {
		if byTime := int64(float64(c.tokens) * float64(l.deadline.Sub(now)) / float64(c.took)); byTime < room {
			room, why = byTime, "wall_clock"
		}
	}
	chars := int64(utf8.RuneCountInString(c.text))
	left := int64(l.c.eff.Answer.MaxBodyChars-utf8.RuneCountInString(l.c.eff.Prompt.OnTruncatedText)-len(noteBreak)) - chars
	if left <= 0 {
		return cutBody
	}
	if c.textTokens > 0 && chars > 0 {
		if byBody := left * c.textTokens / chars; byBody < room {
			room, why = byBody, cutBody
		}
	}
	if room < int64(min(minContinuation, l.cap)) {
		return why
	}
	c.room = int(min(room, int64(l.cap)))
	switch {
	case room < int64(l.cap):
		c.last, c.why = true, why
	case l.stats.In+l.lastUsage.Input >= l.b.InputTokens:
		c.last, c.why = true, "input_tokens"
	}
	return ""
}

// cutShort ends the loop with an answer cut short for why: its text, cut
// to leave room for on_truncated_text, and the note after it.
func (l *loop) cutShort(text, why string) loopEnd {
	l.stats.Truncated = true
	l.c.a.s.o.Metrics.BudgetExhausted.WithLabelValues(budgetTruncated).Inc()
	l.c.s.log.Info("the answer was cut short at its length: it is posted with on_truncated_text after it", "conversation", l.c.conv,
		"continuations", l.stats.Continuations, "why", why)
	return l.body(withNote(strings.TrimSpace(text), l.c.eff.Prompt.OnTruncatedText, l.c.eff.Answer.MaxBodyChars))
}

// noteBreak comes between an answer cut short and on_truncated_text.
const noteBreak = "\n\n"

// withNote is body with note after it, on a paragraph of its own: body made
// safe and cut (safety.Body) so that the two hold at most maxChars, and a
// code block it leaves open closed, lest the note be taken for code.
func withNote(body, note string, maxChars int) string {
	note = strings.TrimSpace(note)
	limit := maxChars - utf8.RuneCountInString(note) - len(noteBreak)
	if limit < 1 {
		return note
	}
	safe, _ := safety.Body(body, limit)
	for reserve := 0; reserve < limit; {
		fence := openFence(safe)
		if fence == "" {
			break
		}
		if len(fence)+1 <= reserve {
			safe += "\n" + fence
			break
		}
		reserve = len(fence) + 1
		safe, _ = safety.Body(body, limit-reserve)
	}
	if safe == "" {
		return note
	}
	return safe + noteBreak + note
}

// openFence is the fence that closes the fenced code block s leaves open,
// or "": a line of three or more ` or ~ opens one, and a line of as many or
// more of the same, and nothing else, closes it.
func openFence(s string) string {
	fence := ""
	for line := range strings.SplitSeq(s, "\n") {
		t := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(t, "```") && !strings.HasPrefix(t, "~~~") {
			continue
		}
		n := 0
		for n < len(t) && t[n] == t[0] {
			n++
		}
		switch {
		case fence == "" && (t[0] != '`' || !strings.Contains(t[n:], "`")):
			fence = t[:n]
		case fence != "" && t[0] == fence[0] && n >= len(fence) && strings.TrimSpace(t[n:]) == "":
			fence = ""
		}
	}
	return fence
}

// joinPiece is text with piece, a continuation of it, after it; where piece
// begins by writing again the end of text (the line or sentence it broke
// off in), as models asked to go on sometimes do, that is left out. An
// overlap of fewer than minOverlap bytes is taken for chance, and kept.
func joinPiece(text, piece string) string {
	for _, p := range []string{piece, strings.TrimLeft(piece, " \t\n")} {
		for k := min(len(text), len(p), maxOverlap); k >= minOverlap; k-- {
			if (k == len(p) || utf8.RuneStart(p[k])) && strings.HasSuffix(text, p[:k]) {
				return text + p[k:]
			}
		}
	}
	return text + piece
}
