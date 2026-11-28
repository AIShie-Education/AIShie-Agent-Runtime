package chat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/safety"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

const chatLeaseTTL = 5 * time.Minute

type activeGeneration struct {
	conversation string
	cancel       context.CancelFunc
}

// Dispatcher schedules immediately in-process and recovers durable outbox
// rows. Existing AR leases provide cross-process per-conversation exclusion.
type Dispatcher struct {
	s    *Service
	mu   sync.Mutex
	runs map[string]*activeGeneration
}

func NewDispatcher(s *Service) *Dispatcher {
	return &Dispatcher{s: s, runs: map[string]*activeGeneration{}}
}

func (d *Dispatcher) schedule(generationID string) {
	if generationID == "" {
		return
	}
	d.mu.Lock()
	if _, ok := d.runs[generationID]; d.s.ctx.Err() != nil || ok {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(d.s.ctx)
	run := &activeGeneration{cancel: cancel}
	d.runs[generationID] = run
	d.s.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.s.wg.Done()
		defer func() { d.mu.Lock(); delete(d.runs, generationID); d.mu.Unlock(); cancel() }()
		d.run(ctx, generationID, run)
	}()
}

func (d *Dispatcher) abort(conversationID string) {
	d.mu.Lock()
	for _, run := range d.runs {
		if run.conversation == conversationID {
			run.cancel()
		}
	}
	d.mu.Unlock()
}

func (d *Dispatcher) abortAll() {
	d.mu.Lock()
	for _, run := range d.runs {
		run.cancel()
	}
	d.mu.Unlock()
}

func (d *Dispatcher) recoverLoop(ctx context.Context) {
	d.recover(ctx)
	ticker := time.NewTicker(d.s.recoveryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.s.cancel()
			d.abortAll()
			return
		case <-d.s.ctx.Done():
			return
		case <-ticker.C:
			d.recover(ctx)
		}
	}
}

func (d *Dispatcher) recover(ctx context.Context) {
	rows, err := d.s.store.RecoverChatGenerations(ctx, 200)
	if err != nil {
		if ctx.Err() == nil {
			d.s.log.Warn("chat generation recovery could not read the store")
		}
		return
	}
	for _, row := range rows {
		d.schedule(row.ID)
	}
}

func (d *Dispatcher) heartbeat(ctx context.Context, gen store.ChatGeneration, lease string, active *activeGeneration, done <-chan struct{}) {
	ticker := time.NewTicker(chatLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ok, err := d.s.store.HeartbeatChatGeneration(callCtx, gen.ID, gen.Holder, gen.Fence, chatLeaseTTL)
			if err == nil && ok {
				ok, err = d.s.store.AcquireLease(callCtx, lease, d.s.workerID, chatLeaseTTL)
			}
			cancel()
			if err != nil || !ok {
				active.cancel()
				return
			}
		}
	}
}

func (d *Dispatcher) fail(gen store.ChatGeneration, kind string, retryable bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conv, changed, err := d.s.store.FailChatGeneration(ctx, gen.ID, gen.Holder, gen.Fence, kind, retryable)
	if err != nil {
		d.s.log.Warn("chat generation failure could not be persisted", "generation", gen.ID)
		return
	}
	if changed {
		d.s.hub.Publish(conv.ID)
	}
}

func (d *Dispatcher) run(ctx context.Context, generationID string, active *activeGeneration) {
	gen, err := d.s.store.ChatGeneration(ctx, generationID)
	if err != nil {
		return
	}
	d.mu.Lock()
	active.conversation = gen.ConversationID
	d.mu.Unlock()
	lease := "chat:conv:" + gen.ConversationID
	held, err := d.s.store.AcquireLease(ctx, lease, d.s.workerID, chatLeaseTTL)
	if err != nil || !held {
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.s.store.ReleaseLease(ctx, lease, d.s.workerID)
		cancel()
	}()
	gen, conv, begin, err := d.s.store.BeginChatGeneration(ctx, generationID, d.s.workerID, chatLeaseTTL)
	if err != nil {
		return
	}
	if !begin {
		return
	}
	heartbeatDone := make(chan struct{})
	go d.heartbeat(ctx, gen, lease, active, heartbeatDone)
	defer close(heartbeatDone)
	d.s.hub.Publish(conv.ID)

	auth, err := d.s.authorize(ctx, conv.OpenerActorID, conv.CourseID, conv.AgentID)
	if err != nil {
		if errors.Is(err, ErrDenied) {
			d.fail(gen, "permission_denied", false)
		} else {
			d.fail(gen, "authorization_unavailable", true)
		}
		return
	}
	// A result written ahead before a crash is finalized/proposed from its
	// persisted answer_level without invoking a model again.
	if gen.Result != nil {
		d.finalize(gen, auth)
		return
	}
	snap, err := d.s.store.ChatSnapshot(ctx, conv.CourseID, conv.ID, conv.OpenerActorID, 100, 0)
	if err != nil {
		d.fail(gen, "store_unavailable", true)
		return
	}
	if d.s.generator == nil {
		d.fail(gen, "generation_unavailable", false)
		return
	}

	draft := NewDraftWriter(d.s.store, d.s.hub, generationID, gen.Holder, gen.Fence, d.s.draftEvery, auth.Grant.ConversationAnswerLevel != "autonomous")
	defer draft.Stop()
	result, err := d.s.generator.Generate(ctx, GenerationInput{Conversation: conv, Messages: snap.Messages, Generation: gen, Authorization: auth}, draft)
	if err != nil {
		// Service shutdown leaves the durable running row recoverable. A
		// withdrawal/close has already cancelled it atomically in the store.
		if d.s.ctx.Err() != nil {
			return
		}
		kind, retryable := safeRunError(err)
		var runErr *RunError
		if errors.As(err, &runErr) {
			// Kind and numeric status are safe by construction. Never log the
			// provider's message, request, response body or credentials.
			d.s.log.Warn("chat runner failed", "generation", gen.ID, "kind", runErr.Kind, "http_status", runErr.HTTPStatus)
		}
		d.fail(gen, kind, retryable)
		return
	}
	body, report := safety.Body(result.Body, safety.CoreMaxChars)
	if report.Empty {
		d.fail(gen, "content_filter", false)
		return
	}
	result.Body = body
	written, err := d.s.store.WriteChatResult(ctx, generationID, gen.Holder, gen.Fence, result)
	if err != nil {
		d.fail(gen, "store_unavailable", true)
		return
	}
	if !written {
		return
	}
	d.finalize(gen, auth)
}

func safeStoreFailureCode(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return state.SQLState()
	}
	switch {
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "unknown"
	}
}

func (d *Dispatcher) finalize(gen store.ChatGeneration, auth *Authorization) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	level := gen.AnswerLevel
	if level == "" && auth != nil {
		level = auth.Grant.ConversationAnswerLevel
	}
	var snap store.ChatSnapshot
	var changed bool
	var err error
	switch level {
	case "confirm_required":
		approval := store.ChatApproval{ID: d.s.newID(), Subject: store.ChatSubjectAnswer,
			GenerationID: gen.ID, Mode: store.ChatApprovalModeDecision, State: store.ChatApprovalPending}
		snap, changed, err = d.s.store.ProposeChatGeneration(ctx, gen.ID, gen.Holder, gen.Fence, approval)
	case "pending_review":
		approval := store.ChatApproval{ID: d.s.newID(), Subject: store.ChatSubjectAnswer, GenerationID: gen.ID, Mode: store.ChatApprovalModeReview, State: store.ChatApprovalPending}
		snap, changed, err = d.s.store.FinalizeReviewedChatGeneration(ctx, gen.ID, gen.Holder, gen.Fence, approval)
	default:
		snap, changed, err = d.s.store.FinalizeChatGeneration(ctx, gen.ID, gen.Holder, gen.Fence)
	}
	if err != nil {
		attrs := []any{"generation", gen.ID, "store_code", safeStoreFailureCode(err)}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			attrs = append(attrs, "db_code", pgErr.Code, "constraint", pgErr.ConstraintName)
		} else {
			attrs = append(attrs, "error_type", fmt.Sprintf("%T", err))
		}
		d.s.log.Warn("chat final message could not be persisted", attrs...)
		return
	}
	if changed {
		if level != "confirm_required" {
			var result *store.ChatResult
			if gen.Result != nil {
				result = gen.Result
			} else if len(snap.Messages) > 0 && snap.Messages[len(snap.Messages)-1].ID == gen.AssistantMsgID {
				m := snap.Messages[len(snap.Messages)-1]
				result = &store.ChatResult{Body: m.Body, Sources: m.Sources, SourcesStated: m.SourcesStated, Usage: m.Usage, FinishReason: m.FinishReason}
			}
			var grant *Grant
			if auth != nil {
				grant = &auth.Grant
			}
			if result != nil {
				d.s.recordAnswer(gen, snap.Conversation, *result, grant)
			}
		}
		d.s.hub.Publish(snap.Conversation.ID)
	}
}
