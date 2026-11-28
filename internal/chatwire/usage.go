package chatwire

import (
	"context"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// RecordModelCall records every primary/fallback attempt, including failures,
// with numeric usage and safe classifications only. It intentionally does not
// inspect PreparedRun.Request, whose credentials and prompt never reach the
// ledger. AnswerRecord remains chat.Service's final-answer responsibility.
func (w *Wiring) RecordModelCall(ctx context.Context, in chat.GenerationInput, prepared chat.PreparedRun,
	done *runner.Done, failureKind string, latency time.Duration) {
	now := w.now()
	call := store.LLMCall{
		ID: w.opts.NewID(), Kind: store.CallAnswer, At: now,
		TenantID: prepared.TenantID, AgentID: prepared.AgentID, CourseID: in.Conversation.CourseID,
		ConversationID: in.Conversation.ID, MessageID: in.Generation.UserMessageID,
		Adapter: prepared.Adapter, Provider: prepared.Provider, Model: prepared.Model,
		Stop: failureKind, KeySource: prepared.KeySource, LatencyMS: max(latency.Milliseconds(), 0),
	}
	if in.Authorization != nil {
		call.MemberID = in.Authorization.Grant.SelfMemberID
		call.OpenerMemberID = in.Authorization.Grant.OpenerMemberID
	}
	if done != nil {
		u := done.Usage
		call.Input, call.Output, call.Reasoning = u.Input, u.Output, u.Reasoning
		call.CacheRead, call.CacheWrite, call.Estimated = u.CacheRead, u.CacheWrite, u.Estimated
		call.RawUsage = runner.RawUsage(u)
		if failureKind == "" {
			call.Stop, call.RawStop = done.FinishReason, done.FinishReason
		}
		prices := w.priceTable()
		if price, ok := prices.LookupIn(prepared.Provider, prepared.Model, now, w.opts.Location); ok {
			call.PriceVersion, call.CostPUSD = price.Version, price.Cost(u)
		}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.opts.LedgerTimeout)
	defer cancel()
	if err := w.opts.Store.RecordLLMCall(writeCtx, call); err != nil {
		// Store error text can contain driver details. Log ids only; neither it
		// nor any provider/request value is needed to diagnose this hook.
		w.opts.Log.Error("chat model call was not recorded", "generation", in.Generation.ID,
			"agent", prepared.AgentID, "provider", prepared.Provider, "model", prepared.Model, "failure_kind", failureKind)
	}
}

func (w *Wiring) priceTable() *pricing.Table {
	if w.opts.PriceTable != nil {
		return w.opts.PriceTable()
	}
	cfg := w.opts.Config()
	if cfg == nil {
		return w.opts.Prices
	}
	return cfg.Runtime.Site.PriceTable(w.opts.Prices)
}
