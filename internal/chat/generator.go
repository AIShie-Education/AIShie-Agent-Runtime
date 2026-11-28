package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/safety"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// The /run request, model, prompt parts, limits and NDJSON event/done types
// are internal/runner's single source of truth. CoreToken and Model.APIKey
// exist only in that in-memory request, never in chat persistence or logs.
type RunRequest = runner.RunRequest
type RunSeat = runner.Seat
type RunMessage = prompt.Message
type RunPart = prompt.Part
type RunModel = runner.Model
type RunLimits = runner.Limits
type RunEvent = runner.Event
type RunDone = runner.Done
type RunWrites = runner.WriteCounts

// RunError is safe by construction: provider/Runner message text is dropped.
type RunError struct {
	Kind       string
	Retryable  bool
	HTTPStatus int
}

func (e *RunError) Error() string {
	if e.Kind == "" {
		return "chat: runner failed"
	}
	return "chat: runner: " + e.Kind
}

// Runner is exactly internal/runner.Client.Run's contract, including NDJSON
// consumption and cancellation. *runner.Client satisfies it directly.
type Runner interface {
	Run(ctx context.Context, request runner.RunRequest, event func(runner.Event) error) (*runner.Done, error)
}

type RunnerFunc func(context.Context, runner.RunRequest, func(runner.Event) error) (*runner.Done, error)

func (f RunnerFunc) Run(ctx context.Context, r runner.RunRequest, event func(runner.Event) error) (*runner.Done, error) {
	return f(ctx, r, event)
}

// GenerationInput is one persisted outbox row and its current history. Access
// is freshly revalidated before this is built.
type GenerationInput struct {
	Conversation  store.ChatConversation
	Messages      []store.ChatMessage
	Generation    store.ChatGeneration
	Authorization *Authorization
}

// PreparedRun is a primary or fallback request plus non-secret ledger facts.
type PreparedRun struct {
	Request   runner.RunRequest
	TenantID  string
	AgentID   string
	KeySource string
	Adapter   string
	Provider  string
	Model     string
}

// Preparer owns model/catalogue/prompt/tool policy, Vault/Core-token opening,
// attachment encoding, quotas and primary/fallback selection. It is the small
// public wiring boundary to existing config/registry/secrets facilities.
type Preparer interface {
	Prepare(context.Context, GenerationInput) ([]PreparedRun, error)
}

type PreparerFunc func(context.Context, GenerationInput) ([]PreparedRun, error)

func (f PreparerFunc) Prepare(ctx context.Context, in GenerationInput) ([]PreparedRun, error) {
	return f(ctx, in)
}

// UsageRecorder records each model call through existing pricing/ledger
// facilities. Implementations must not record request text or secrets.
type UsageRecorder interface {
	RecordModelCall(context.Context, GenerationInput, PreparedRun, *runner.Done, string, time.Duration)
}

// DraftSink receives safe whole-draft states.
type DraftSink interface {
	Set(text string, steps []store.ChatStep)
}

// Generator produces one safe final result for a queued user message.
type Generator interface {
	Generate(context.Context, GenerationInput, DraftSink) (store.ChatResult, error)
}

type GeneratorFunc func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error)

func (f GeneratorFunc) Generate(ctx context.Context, in GenerationInput, sink DraftSink) (store.ChatResult, error) {
	return f(ctx, in, sink)
}

// RunnerGenerator adapts internal/runner.Client to Generator, including
// retryable fallback, draft events, safety.Body and usage hooks.
type RunnerGenerator struct {
	Preparer Preparer
	Runner   Runner
	Usage    UsageRecorder
}

func generationError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return &RunError{Kind: "aborted"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &RunError{Kind: "timeout", Retryable: true}
	}
	var re *RunError
	if errors.As(err, &re) {
		return &RunError{Kind: re.Kind, Retryable: re.Retryable, HTTPStatus: re.HTTPStatus}
	}
	var runnerErr *runner.Error
	if errors.As(err, &runnerErr) {
		return &RunError{Kind: runnerErr.Kind, Retryable: runnerErr.Retryable, HTTPStatus: runnerErr.HTTPStatus}
	}
	// Deliberately discard arbitrary/provider error text.
	return &RunError{Kind: "provider_failed"}
}

func runnerStepKind(tool string) string {
	switch tool {
	case "document_get", "document_text":
		return "reading_document"
	case "document_list", "course_materials_search":
		return "listing_documents"
	case "assignment_get":
		return "reading_assignment"
	case "submission_get":
		return "reading_submission"
	case "memory_search":
		return "searching_memory"
	default:
		return "tool"
	}
}

func finishSteps(steps []store.ChatStep) {
	for i := range steps {
		if steps[i].State == "running" {
			steps[i].State = "done"
		}
	}
}

func safeErrorKind(err error) string {
	var re *RunError
	if errors.As(generationError(err), &re) && re.Kind != "" {
		return re.Kind
	}
	return "provider_failed"
}

func (g *RunnerGenerator) Generate(ctx context.Context, in GenerationInput, sink DraftSink) (store.ChatResult, error) {
	if g == nil || g.Preparer == nil || g.Runner == nil {
		return store.ChatResult{}, &RunError{Kind: "generation_unavailable"}
	}
	runs, err := g.Preparer.Prepare(ctx, in)
	if err != nil {
		return store.ChatResult{}, generationError(err)
	}
	if len(runs) == 0 {
		return store.ChatResult{}, &RunError{Kind: "generation_unavailable"}
	}
	var last error
	for i, prepared := range runs {
		req := prepared.Request
		req.CourseID, req.ConversationID = in.Conversation.CourseID, in.Conversation.ID
		req.MessageID, req.Attempt = in.Generation.UserMessageID, in.Generation.Attempt
		var mu sync.Mutex
		var text strings.Builder
		steps := []store.ChatStep{{Kind: "thinking", State: "running"}}
		calls := map[string]int{}
		if sink != nil {
			sink.Set("", append([]store.ChatStep(nil), steps...))
		}
		started := time.Now()
		done, runErr := g.Runner.Run(ctx, req, func(ev runner.Event) error {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Type {
			case "step":
				finishSteps(steps)
				kind := ev.Kind
				if kind == "" {
					kind = "thinking"
				}
				steps = append(steps, store.ChatStep{Kind: kind, State: "running"})
			case "tool_start":
				finishSteps(steps)
				steps = append(steps, store.ChatStep{Kind: runnerStepKind(ev.Name), State: "running"})
				if ev.ID != "" {
					calls[ev.ID] = len(steps) - 1
				}
			case "tool_end":
				if at, ok := calls[ev.ID]; ok && at < len(steps) {
					steps[at].State = "done"
					steps[at].Target = ev.Target
				}
			case "text_delta":
				if ev.Delta == "" {
					return nil
				}
				if text.Len() == 0 {
					finishSteps(steps)
					steps = append(steps, store.ChatStep{Kind: "writing", State: "running"})
				}
				text.WriteString(ev.Delta)
			default:
				return nil
			}
			// DraftWriter.Set only swaps cloned in-memory state; it performs no
			// blocking I/O while the Runner callback lock is held.
			if sink != nil {
				sink.Set(text.String(), append([]store.ChatStep(nil), steps...))
			}
			return nil
		})
		if g.Usage != nil {
			kind := ""
			if runErr != nil {
				kind = safeErrorKind(runErr)
			}
			g.Usage.RecordModelCall(ctx, in, prepared, done, kind, time.Since(started))
		}
		if runErr != nil {
			last = generationError(runErr)
			var re *RunError
			if errors.As(last, &re) && re.Retryable && i+1 < len(runs) && ctx.Err() == nil {
				continue
			}
			return store.ChatResult{}, last
		}
		if done == nil {
			return store.ChatResult{}, &RunError{Kind: "provider_failed"}
		}
		body := done.Text
		if body == "" {
			body = text.String()
		}
		body, report := safety.Body(body, safety.CoreMaxChars)
		if report.Empty {
			return store.ChatResult{}, &RunError{Kind: "content_filter"}
		}
		stated := done.SourcesStated
		usage := store.ChatUsage{Input: done.Usage.Input, Output: done.Usage.Output, Reasoning: done.Usage.Reasoning,
			CacheRead: done.Usage.CacheRead, CacheWrite: done.Usage.CacheWrite}
		result := store.ChatResult{Body: body, SourcesStated: &stated, Usage: &usage, FinishReason: done.FinishReason,
			TenantID: prepared.TenantID, AgentID: prepared.AgentID, KeySource: prepared.KeySource, Adapter: prepared.Adapter,
			Provider: prepared.Provider, Model: prepared.Model, Turns: done.Steps, ToolCalls: done.ToolCalls,
			Writes: store.WriteCounts{Sent: done.Writes.Executed + done.Writes.Proposed + done.Writes.Denied + done.Writes.Failed + done.Writes.Refused + done.Writes.Guarded,
				Executed: done.Writes.Executed, Proposed: done.Writes.Proposed, Denied: done.Writes.Denied, Failed: done.Writes.Failed}}
		if in.Authorization != nil {
			result.MemberID = in.Authorization.Grant.SelfMemberID
		}
		if stated {
			result.Sources = make([]store.ChatSource, 0, len(done.Sources))
			for _, source := range done.Sources {
				result.Sources = append(result.Sources, store.ChatSource{DocumentID: source.DocumentID, VersionID: source.VersionID,
					FileID: source.FileID, Page: source.Page, Slide: source.Slide, Part: source.Part})
			}
		}
		return result, nil
	}
	if last == nil {
		last = fmt.Errorf("no runner result")
	}
	return store.ChatResult{}, generationError(last)
}
