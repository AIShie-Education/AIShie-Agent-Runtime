package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolset"
)

const (
	// maxCallTimeout bounds one model call (§7.1: "timeout = min(60 s, the
	// wall clock left)").
	maxCallTimeout = 60 * time.Second
	// modelTries is how often one adapter is called for one turn while its
	// errors are retryable.
	modelTries = 3
	// maxGrace bounds the time a last turn forced by a spent wall clock is
	// given.
	maxGrace = 15 * time.Second
)

// loop is one answer's model loop (§7.1; design §5.3 step 7): turns of the
// model and its tool calls, bounded by budgets.per_answer, until it writes
// the answer's body or a budget or a refusal decides it.
type loop struct {
	c   *claim
	msg string
	b   config.PerAnswer

	start    time.Time
	deadline time.Time
	system   string
	// history is the conversation as plain turns, ending with the
	// question; turns are this loop's own, which are never edited.
	history []llm.Message
	turns   []llm.Message

	m        *model
	fallback *model
	// access is whether the model is offered the seat's writes, and
	// writes the answer's account of them: nil when it is not. guard is
	// the seats its member writes never change.
	access toolset.Access
	writes *toolset.Writes
	guard  toolset.SeatGuard
	set    *toolset.Set
	decls  []llm.Tool
	// cap is the output tokens asked for on a turn.
	cap int

	stats     loopStats
	exhausted string
}

// loopStats are what the loop spent.
type loopStats struct {
	Turns     int
	ToolCalls int
	In, Out   int64
	// Cost is in pico-dollars.
	Cost int64
	// Writes are the writes the model made, as Core answered them;
	// WritesRefused those refused before Core, the answer's writes being
	// spent; and WritesGuarded the member writes refused before Core for
	// the seats they would have changed (toolset.SeatGuard).
	Writes        store.WriteCounts
	WritesRefused int
	WritesGuarded int
}

// loopEnd is how a loop ended: a body to post and what wrote it; or every
// provider failed, and nothing is posted; or the answer cannot go on
// (Core refused the token, the claim's context ended).
type loopEnd struct {
	body   string
	kind   string
	failed bool
	fatal  error
}

// newLoop is the loop of attempt at answering msg. With access ReadWrite
// the model is offered the seat's writes, each bound to the key of its
// number in this attempt (core.ToolKey), at most per_answer.max_writes,
// and its member writes kept off the seats guard names.
func newLoop(c *claim, msg string, attempt int, access toolset.Access, guard toolset.SeatGuard, system string, history []llm.Message) (*loop, error) {
	l := &loop{
		c: c, msg: msg, b: c.eff.Budgets.PerAnswer, start: c.a.now(), system: system, history: history,
		m: c.s.primary, fallback: c.s.fallback, access: access, guard: guard,
	}
	if access == toolset.ReadWrite {
		conv := c.conv
		l.writes = &toolset.Writes{Max: l.b.MaxWrites, Key: func(n int) string { return core.ToolKey(conv, msg, attempt, n) }}
	}
	l.deadline = l.start.Add(l.b.WallClock())
	if err := l.use(l.m); err != nil {
		return nil, err
	}
	return l, nil
}

// use makes m the loop's model: its toolset's declarations in its dialect,
// and its output cap.
func (l *loop) use(m *model) error {
	set, err := l.c.s.toolsFor(m.ad.Dialect(), l.access)
	if err != nil {
		return err
	}
	l.m, l.set, l.decls = m, set, set.Declarations()
	l.cap = max(m.maxOut, 1)
	return nil
}

// retries are the turns tried again once each.
type retries struct {
	emptyEnd, maxTokens, overflow, toolError bool
}

// run runs the loop. A spent budget takes one last turn with ToolMode none
// (ForceAnswer) and gives on_budget_text if that has no text.
func (l *loop) run(ctx context.Context) loopEnd {
	var tried retries
	forced := false
	partial := ""
	for {
		if l.stats.Turns >= l.b.Turns {
			return l.spent(partial)
		}
		if !forced {
			switch why := l.spentOn(); {
			case why != "":
				forced = l.exhaust(why)
			case l.stats.Turns == l.b.Turns-1:
				forced = l.exhaust("turns")
			}
		}
		if l.outLeft() <= 0 {
			return l.spent(partial)
		}
		resp, err := l.call(ctx, forced)
		if err != nil {
			switch kindOf(err) {
			case llm.ErrContextOverflow:
				resp = &llm.Response{Stop: llm.StopContextOverflow}
			case llm.ErrContentFilter:
				resp = &llm.Response{Stop: llm.StopContentFilter}
			default:
				if ctx.Err() != nil {
					return loopEnd{fatal: ctx.Err()}
				}
				if forced {
					return l.spent(partial)
				}
				l.c.s.log.Warn("the model's providers could not be reached", "conversation", l.c.conv, "err_kind", kindOf(err))
				return loopEnd{failed: true}
			}
		}
		text := strings.TrimSpace(resp.Text())
		switch resp.Stop {
		case llm.StopToolCalls:
			if forced {
				// Told not to call tools, it did: its text, if any, is the
				// answer.
				if text != "" {
					return l.body(text)
				}
				return l.spent(partial)
			}
			if err := l.runTools(ctx, resp); err != nil {
				return loopEnd{fatal: err}
			}
		case llm.StopEnd:
			if text != "" {
				return l.body(text)
			}
			// No text is no answer: the turn once more, then the budget.
			if forced || tried.emptyEnd {
				return l.spent(partial)
			}
			tried.emptyEnd = true
		case llm.StopMaxTokens:
			if text != "" {
				partial = text
			}
			if !forced && !tried.maxTokens {
				if c := min(2*l.cap, int(min(l.outLeft(), int64(maxInt)))); c > l.cap {
					tried.maxTokens, l.cap = true, c
					continue
				}
			}
			return l.spent(partial)
		case llm.StopContentFilter, llm.StopRefusal:
			// A refusal's tool calls, if any, were removed, and none is run.
			return loopEnd{body: l.c.eff.Prompt.OnRefusalText, kind: kindRefusal}
		case llm.StopContextOverflow:
			if !tried.overflow && l.halve() {
				tried.overflow = true
				continue
			}
			return l.spent(partial)
		case llm.StopToolError:
			if forced || tried.toolError {
				return l.spent(partial)
			}
			tried.toolError = true
		default:
			return l.spent(partial)
		}
	}
}

const maxInt = int(^uint(0) >> 1)

// body is the model's own text as the answer.
func (l *loop) body(text string) loopEnd { return loopEnd{body: text, kind: kindModel} }

// spent ends a loop whose budget is spent: the model's partial text, if
// it wrote any, else on_budget_text.
func (l *loop) spent(partial string) loopEnd {
	if partial != "" {
		return l.body(partial)
	}
	return loopEnd{body: l.c.eff.Prompt.OnBudgetText, kind: kindBudget}
}

// spentOn names the budget spent, or "". The output tokens count as spent
// once what is left cannot hold a whole turn: the last turn is forced then,
// within what is left, so that the cap holds and the model still writes.
func (l *loop) spentOn() string {
	switch {
	case !l.c.a.now().Before(l.deadline):
		return "wall_clock"
	case l.stats.ToolCalls >= l.b.ToolCalls:
		return "tool_calls"
	case l.stats.In >= l.b.InputTokens:
		return "input_tokens"
	case l.stats.Turns > 0 && l.outLeft() < int64(l.cap):
		return "output_tokens"
	}
	return ""
}

// exhaust records that a budget was spent, and forces the last turn.
func (l *loop) exhaust(why string) bool {
	l.exhausted = why
	l.c.a.s.o.Metrics.BudgetExhausted.WithLabelValues(why).Inc()
	l.c.s.log.Info("a budget of the answer is spent: the last turn is forced", "conversation", l.c.conv, "budget", why)
	return true
}

// outLeft is the output tokens the answer may still use.
func (l *loop) outLeft() int64 { return l.b.OutputTokens - l.stats.Out }

// request is the next turn's request.
func (l *loop) request(forced bool) *llm.Request {
	msgs := make([]llm.Message, 0, len(l.history)+len(l.turns))
	msgs = append(msgs, l.history...)
	msgs = append(msgs, l.turns...)
	mode := llm.ToolAuto
	if forced {
		mode = llm.ToolNone
	}
	return &llm.Request{
		System: l.system, Messages: msgs, Tools: l.decls, ToolMode: mode,
		Limits: llm.Limits{MaxOutputTokens: int(min(int64(l.cap), l.outLeft()))},
	}
}

// call makes the turn's model call: the model, retried with backoff within
// the wall clock while its errors are retryable, then its fallback, once,
// for the rest of the answer. An error that is a stop in disguise
// (context_overflow, content_filter) comes back at once.
func (l *loop) call(ctx context.Context, forced bool) (*llm.Response, error) {
	for {
		resp, err := l.callModel(ctx, forced)
		if err == nil || ctx.Err() != nil || isStop(err) || l.fallback == nil {
			return resp, err
		}
		fb := l.fallback
		l.fallback = nil
		l.c.s.log.Warn("the model's provider failed: its fallback answers", "conversation", l.c.conv,
			"err_kind", kindOf(err), "fallback", fb.ad.Name(), "fallback_model", fb.ad.Model())
		if fb.ad.Maker() != l.m.ad.Maker() {
			// Another maker takes no reasoning or signatures of this one's,
			// and some refuse tool calls in history without their own.
			l.turns = llm.FlattenToolHistory(l.turns)
		}
		if err := l.use(fb); err != nil {
			return nil, err
		}
	}
}

// callModel calls the loop's model, up to modelTries times while its
// errors are retryable and the wall clock allows the backoff.
func (l *loop) callModel(ctx context.Context, forced bool) (*llm.Response, error) {
	t := l.c.a.s.o.Timing
	var last error
	for try := 0; try < modelTries; try++ {
		timeout := l.timeout(ctx, forced)
		if timeout <= 0 {
			if last == nil {
				last = &llm.Error{Kind: llm.ErrTimeout, Message: "the answer's wall clock is spent"}
			}
			return nil, last
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		began := time.Now()
		resp, err := l.m.ad.Call(cctx, l.request(forced))
		cancel()
		l.account(resp, err, time.Since(began))
		if err == nil && resp.Stop != llm.StopError {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			// A call that completed stopped for an error of the provider's.
			err = &llm.Error{Kind: llm.ErrServer, Code: "stop_" + resp.RawStop}
		}
		last = err
		var le *llm.Error
		if !errors.As(err, &le) || !le.Retryable() {
			return nil, err
		}
		if le.Kind == llm.ErrTimeout && l.fallback != nil {
			// A provider that hung once is likely to hang again: the
			// fallback gets what time is left.
			return nil, err
		}
		wait := Backoff(try, t.ModelBackoff, t.ModelBackoffMax, l.c.a.rand())
		wait = max(wait, le.RetryAfter)
		if try == modelTries-1 || !l.c.a.now().Add(wait).Before(l.deadline) {
			return nil, err
		}
		if !sleep(ctx, wait) {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

// timeout is a model call's: min(60 s, the wall clock left). A last turn
// forced by a spent wall clock is given a grace of its own, a sixth of the
// wall clock and at most 15 s, within the claim's own deadline. While a
// fallback remains, the model gets two thirds of what is left, so that a
// provider that hangs leaves its fallback time to answer.
func (l *loop) timeout(ctx context.Context, forced bool) time.Duration {
	left := l.deadline.Sub(l.c.a.now())
	if forced {
		grace := min(l.b.WallClock()/6, maxGrace)
		left = max(left, grace)
	}
	if dl, ok := ctx.Deadline(); ok {
		left = min(left, time.Until(dl))
	}
	if l.fallback != nil {
		left = left * 2 / 3
	}
	return min(left, maxCallTimeout)
}

// otherModel names, in the metrics, a hosted agent's model that the price
// table does not price.
const otherModel = "other"

// modelLabel is the model a call's metrics name: a YAML agent's as its
// operator wrote it; a hosted agent's, which its owner chose and may be
// any text, as the price table names it (pricing.Table.PricedAs: the
// model, or the glob that prices it), and otherModel when no row prices
// it, so that no owner's text becomes a label, and the labels are no more
// than the table's rows.
func modelLabel(cfg *config.Agent, prices *pricing.Table, ad llm.Adapter, at time.Time) string {
	if cfg.Hosted == nil {
		return ad.Model()
	}
	if name, ok := prices.PricedAs(ad.Provider(), ad.Model(), at); ok {
		return name
	}
	return otherModel
}

// account counts a model call: its turn and tokens, its cost at the day's
// prices, a ledger row with ids and numbers, and the metrics.
func (l *loop) account(resp *llm.Response, err error, took time.Duration) {
	a, ad := l.c.a, l.m.ad
	prices, now := a.s.priceTable(), a.now()
	var cost int64
	var version string
	if resp != nil {
		if price, ok := prices.Lookup(ad.Provider(), ad.Model(), now); ok {
			cost, version = price.Cost(resp.Usage), price.Version
		}
	}
	a.s.o.Metrics.ObserveLLM(ad.Name(), modelLabel(a.cfg, prices, ad, now), resp, err, float64(cost)/1e12, l.m.keySource)
	if resp == nil {
		return
	}
	l.stats.Turns++
	l.stats.In += resp.Usage.Input
	l.stats.Out += resp.Usage.Output
	l.stats.Cost += cost
	u := resp.Usage
	rec := store.LLMCall{
		ID: uuid.NewString(), At: a.now(), TenantID: a.cfg.TenantID, AgentID: a.id, CourseID: l.c.s.course, MemberID: l.c.s.id,
		ConversationID: l.c.conv, MessageID: l.msg, OpenerMemberID: l.c.opener,
		Adapter: ad.Name(), Provider: ad.Provider(), Model: ad.Model(), Stop: string(resp.Stop), RawStop: resp.RawStop,
		Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning,
		Estimated: u.Estimated, RawUsage: u.Raw, PriceVersion: version, CostPUSD: cost, KeySource: l.m.keySource,
		LatencyMS: took.Milliseconds(),
	}
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := a.store().RecordLLMCall(ctx, rec); err != nil {
		l.c.s.log.Error("model call not recorded in the ledger", "conversation", l.c.conv, "err", err)
	}
}

// runTools runs a turn's tool calls through the seat's toolset, at most
// what is left of the answer's tool_calls budget: calls past it are
// answered with an error and reach nobody. The model's turn and the
// results are added to the loop's turns. Its error is fatal to the answer:
// Core refused the token, or the claim's context ended.
func (l *loop) runTools(ctx context.Context, resp *llm.Response) error {
	calls := resp.ToolCalls()
	l.turns = append(l.turns, llm.Message{Role: llm.RoleAssistant, Parts: resp.Parts})
	room := max(l.b.ToolCalls-l.stats.ToolCalls, 0)
	run, over := calls, []llm.Part(nil)
	if len(calls) > room {
		run, over = calls[:room], calls[room:]
	}
	l.stats.ToolCalls += len(calls)
	eff := l.c.eff
	parts, err := l.set.Run(ctx, toolset.Runner{
		Client: l.c.a.client, Files: l.c.a.s.files, MaxParallel: eff.Tools.MaxParallelTools,
		FileInput: l.m.ad.Capabilities().FileInput, Writes: l.writes, Guard: l.guard,
	}, l.c.s.course, run)
	l.accountWrites()
	if err != nil {
		return err
	}
	var results, files []llm.Part
	for _, p := range parts {
		if p.Type == llm.PartToolResult {
			results = append(results, p)
		} else {
			files = append(files, p)
		}
	}
	for _, c := range over {
		results = append(results, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name, IsError: true,
			Content: `{"status":"error","error":{"code":"failed_precondition","message":"the tool calls this answer may make are spent; answer with what you have"}}`})
	}
	l.turns = append(l.turns, llm.Message{Role: llm.RoleTool, Parts: append(results, files...)})
	return nil
}

// accountWrites counts the writes a turn made, as Core answered them:
// in the answer's stats for the ledger, in tool_writes_total by tool and
// outcome, and in one log line each, of ids and codes (the audit of what
// the agent did, beside Core's own action log). An executed or proposed
// write, not a replay, is noted in the conversation's memory, so that a
// later attempt, whose keys are new, knows what is done already.
func (l *loop) accountWrites() {
	w := l.writes
	if w == nil {
		return
	}
	m := l.c.a.s.o.Metrics
	for _, rec := range w.Records[l.stats.Writes.Sent:] {
		l.stats.Writes.Sent++
		switch rec.Status {
		case string(core.StatusExecuted):
			l.stats.Writes.Executed++
		case string(core.StatusProposed):
			l.stats.Writes.Proposed++
		case string(core.StatusDenied):
			l.stats.Writes.Denied++
		case string(core.StatusFailed):
			l.stats.Writes.Failed++
		}
		m.ToolWrites.WithLabelValues(rec.Tool, writeOutcome(rec.Status)).Inc()
		l.c.s.log.Info("a write the model made", "conversation", l.c.conv, "message", l.msg, "tool", rec.Tool, "n", rec.N,
			"key", rec.Key, "status", rec.Status, "code", rec.Code, "reason", rec.Reason, "action", rec.ActionID, "replayed", rec.Replayed)
		if !rec.Replayed && (rec.Status == string(core.StatusExecuted) || rec.Status == string(core.StatusProposed)) {
			addNote(l.c.a, l.c.eff, store.Note{AgentID: l.c.a.id, MemberID: l.c.s.id, ConversationID: l.c.conv,
				Kind: store.NoteWrote, Text: wroteNote(rec)})
		}
	}
	if refused := w.Refused[l.stats.WritesRefused:]; len(refused) > 0 {
		l.stats.WritesRefused = len(w.Refused)
		for _, tool := range refused {
			m.ToolWrites.WithLabelValues(tool, writeRefused).Inc()
		}
		m.BudgetExhausted.WithLabelValues("writes").Inc()
		l.c.s.log.Info("the writes of the answer are spent: writes were refused", "conversation", l.c.conv, "refused", len(refused))
	}
	for _, tool := range w.Guarded[l.stats.WritesGuarded:] {
		l.stats.WritesGuarded++
		m.ToolWrites.WithLabelValues(tool, writeRefused).Inc()
		l.c.s.log.Warn("a write the model made was refused before Core: it would have changed a seat the model never changes",
			"conversation", l.c.conv, "message", l.msg, "tool", tool)
	}
}

// writeRefused is tool_writes_total's outcome for a write refused before
// Core: the answer's writes being spent, or a member write that would have
// changed a seat the model never changes (toolset.SeatGuard).
const writeRefused = "refused"

// writeOutcome is tool_writes_total's outcome for a write's status: Core's
// executed, proposed, denied and failed as they are; error for Core's
// error (an idempotency_conflict, an argument Core refused); unreachable
// when Core did not answer.
func writeOutcome(status string) string {
	switch status {
	case string(core.StatusExecuted), string(core.StatusProposed), string(core.StatusDenied), string(core.StatusFailed),
		toolset.StatusUnreachable:
		return status
	}
	return "error"
}

// wroteNote is a write's note for the conversation's memory: the tool,
// what came of it, the action and the ids it made; never its arguments.
func wroteNote(rec toolset.WriteRecord) string {
	var b strings.Builder
	b.WriteString(rec.Tool + ": " + rec.Status)
	if rec.Status == string(core.StatusProposed) {
		b.WriteString(", waiting for a person's approval")
	}
	var ids []string
	if rec.ActionID != "" {
		ids = append(ids, "action "+rec.ActionID)
	}
	keys := make([]string, 0, len(rec.IDs))
	for k := range rec.IDs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		ids = append(ids, k+" "+rec.IDs[k])
	}
	if len(ids) > 0 {
		b.WriteString(" (" + strings.Join(ids, ", ") + ")")
	}
	return b.String()
}

// halve shortens the history after a context overflow: the older half of
// the conversation before the question goes, as whole turns, and the
// question stays. The loop's own turns, which may carry reasoning bound to
// them, are left as they are. It reports false when there is nothing
// left to take out.
func (l *loop) halve() bool {
	if len(l.history) < 2 {
		return false
	}
	question := l.history[len(l.history)-1]
	prior := l.history[:len(l.history)-1]
	if len(prior) > 0 && isOmitted(prior[0]) {
		prior = prior[1:]
	}
	if len(prior) == 0 {
		return false
	}
	keep := prior[(len(prior)+1)/2:]
	out := []llm.Message{llm.UserText(prompt.Omitted)}
	for _, m := range append(append([]llm.Message(nil), keep...), question) {
		if last := &out[len(out)-1]; last.Role == m.Role {
			last.Parts = append(append([]llm.Part(nil), last.Parts...), m.Parts...)
			continue
		}
		out = append(out, m)
	}
	l.history = out
	l.c.s.log.Info("the model's context overflowed: the history is halved", "conversation", l.c.conv,
		"turns_before", len(prior)+1, "turns_after", len(out))
	return true
}

func isOmitted(m llm.Message) bool {
	return m.Role == llm.RoleUser && len(m.Parts) == 1 && m.Parts[0].Text == prompt.Omitted
}

// kindOf is an error's llm.ErrorKind, or "".
func kindOf(err error) llm.ErrorKind {
	var le *llm.Error
	if errors.As(err, &le) {
		return le.Kind
	}
	return ""
}

// isStop reports whether err is a stop reason that came as an error: the
// loop meets it as that stop, on the same model.
func isStop(err error) bool {
	k := kindOf(err)
	return k == llm.ErrContextOverflow || k == llm.ErrContentFilter
}
