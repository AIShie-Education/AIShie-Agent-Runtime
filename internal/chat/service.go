package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/provider"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/safety"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Error is mapped to the existing API's Core-style envelope. No Error carries
// provider text, a Core token, an API key, or a signed URL.
type Error struct {
	Code    string
	Reason  string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

func serviceError(code, reason, message string) *Error {
	return &Error{Code: code, Reason: reason, Message: message}
}

var (
	ErrNotFound = serviceError("not_found", "not_found", "no such conversation or attachment")
	ErrDenied   = serviceError("not_found", "not_found", "no such agent or conversation")
	ErrCore     = serviceError("unavailable", "core_unavailable", "Core could not be reached to validate chat access")
	ErrGrant    = serviceError("failed_precondition", "authorization_grant_incomplete", "Core did not provide the complete chat authorization grant required by this runtime")
)

// Options configure an AR chat Service.
type Options struct {
	Store         store.Store
	Access        Access
	Decisions     DecisionAccess
	Generator     Generator
	Hub           *Hub
	Log           *slog.Logger
	Now           func() time.Time
	NewID         func() string
	WorkerID      string
	DraftEvery    time.Duration
	RecoveryEvery time.Duration
	PriceTable    func() *pricing.Table
	Location      func() *time.Location
}

// Service owns browser chat state and its local dispatcher.
type Service struct {
	store         store.Store
	access        Access
	decisions     DecisionAccess
	generator     Generator
	hub           *Hub
	log           *slog.Logger
	now           func() time.Time
	newID         func() string
	workerID      string
	draftEvery    time.Duration
	recoveryEvery time.Duration
	priceTable    func() *pricing.Table
	location      func() *time.Location

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	dispatcher *Dispatcher
	runOnce    sync.Once
	closeOnce  sync.Once
}

func NewService(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("chat: store required")
	}
	if o.Access == nil {
		return nil, errors.New("chat: access required")
	}
	if o.Decisions == nil {
		o.Decisions, _ = o.Access.(DecisionAccess)
	}
	if o.Hub == nil {
		o.Hub = NewHub()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.WorkerID == "" {
		o.WorkerID = "chat-" + o.NewID()
	}
	if o.DraftEvery <= 0 {
		o.DraftEvery = DefaultDraftEvery
	}
	if o.RecoveryEvery <= 0 {
		o.RecoveryEvery = 5 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{store: o.Store, access: o.Access, decisions: o.Decisions, generator: o.Generator, hub: o.Hub, log: o.Log,
		now: o.Now, newID: o.NewID, workerID: o.WorkerID, draftEvery: o.DraftEvery, recoveryEvery: o.RecoveryEvery,
		priceTable: o.PriceTable, location: o.Location,
		ctx: ctx, cancel: cancel}
	s.dispatcher = NewDispatcher(s)
	return s, nil
}

func (s *Service) Hub() *Hub               { return s.hub }
func (s *Service) Dispatcher() *Dispatcher { return s.dispatcher }

// Run starts recovery sweeps. Immediate dispatch works before Run too; Run is
// what resumes persisted queued/interrupted rows after a restart.
func (s *Service) Run(ctx context.Context) {
	s.runOnce.Do(func() {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.dispatcher.recoverLoop(ctx) }()
	})
}

// Close aborts local Runner calls and waits for dispatcher goroutines. Rows
// left running are recovered by this or another process using the lease.
func (s *Service) Close() {
	s.closeOnce.Do(func() { s.cancel(); s.dispatcher.abortAll() })
	s.wg.Wait()
}

func normalizeActor(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validKey(key string) bool {
	return len(key) > 0 && len(key) <= 200 && strings.TrimSpace(key) == key && !strings.ContainsFunc(key, unicode.IsControl)
}

func validBody(body string) bool {
	n := utf8.RuneCountInString(body)
	return n > 0 && n <= safety.CoreMaxChars && strings.TrimSpace(body) != "" && utf8.ValidString(body)
}

func chatTitle(title, body string) (string, error) {
	if title == "" {
		title = strings.Join(strings.FieldsFunc(body, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
		title = cutRunes(title, 80)
	}
	if title == "" || utf8.RuneCountInString(title) > 200 || strings.ContainsFunc(title, unicode.IsControl) {
		return "", serviceError("invalid_argument", "invalid_field", "title is one line of at most 200 characters")
	}
	return title, nil
}

func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Service) authorize(ctx context.Context, actor, course, agent string) (*Authorization, error) {
	auth, err := s.access.Check(ctx, actor, course, agent)
	if errors.Is(err, ErrAccessDenied) {
		return nil, ErrDenied
	}
	if err != nil {
		return nil, ErrCore
	}
	if auth == nil || !auth.GrantPresent || auth.Grant.OpenerMemberID == "" || auth.Grant.SelfMemberID == "" ||
		!slices.Contains([]string{"denied", "autonomous", "pending_review", "confirm_required"}, auth.Grant.ActionDecideLevel) || !auth.Grant.AnswersCourseKnown {
		return nil, ErrGrant
	}
	if !slices.Contains([]string{"autonomous", "pending_review", "confirm_required"}, auth.Grant.ConversationAskLevel) ||
		!slices.Contains([]string{"autonomous", "pending_review", "confirm_required"}, auth.Grant.ConversationAnswerLevel) {
		return nil, ErrGrant
	}
	return auth, nil
}

// CheckAccess is used by the SSE handler on connect, events and heartbeat.
func (s *Service) CheckAccess(ctx context.Context, actor, course, agent string) (*Authorization, error) {
	return s.authorize(ctx, normalizeActor(actor), course, agent)
}

func (s *Service) Agents(ctx context.Context, actor, course string) ([]Agent, error) {
	agents, err := s.access.List(ctx, normalizeActor(actor), course)
	if errors.Is(err, ErrAccessDenied) {
		return []Agent{}, nil
	}
	if err != nil {
		return nil, ErrCore
	}
	if agents == nil {
		agents = []Agent{}
	}
	return agents, nil
}

func (s *Service) ListConversations(ctx context.Context, actor, course, after string, limit int) ([]Conversation, string, error) {
	rows, next, err := s.store.ListChatConversations(ctx, course, normalizeActor(actor), after, limit)
	if err != nil {
		return nil, "", serviceError("invalid_argument", "invalid_field", "after is not a valid conversation cursor")
	}
	return rows, next, nil
}

// CreateInput is POST conversations' JSON body.
type CreateInput struct {
	AgentID        string   `json:"agent_id"`
	Title          string   `json:"title,omitempty"`
	Body           string   `json:"body"`
	AttachmentIDs  []string `json:"attachment_ids,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
}

// AppendInput is POST messages' JSON body.
type AppendInput struct {
	Body           string   `json:"body"`
	AttachmentIDs  []string `json:"attachment_ids,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
}

// CreateResponse is ChatSnapshot plus the duplicated first message required
// by the canonical create-contract.
type CreateResponse struct {
	Snapshot
	Message Message `json:"message"`
}

func validateMutation(body, key string, ids []string) error {
	if !validBody(body) {
		return serviceError("invalid_argument", "invalid_field", "body is non-empty UTF-8 of at most 20000 characters")
	}
	if !validKey(key) {
		return serviceError("invalid_argument", "invalid_field", "idempotency_key is required and at most 200 bytes")
	}
	if len(ids) > 100 {
		return serviceError("invalid_argument", "invalid_field", "too many attachment ids")
	}
	return nil
}

func questionSubmission(level, approvalID string) (store.ChatSubmission, *store.ChatApproval) {
	sub := store.ChatSubmission{Status: store.ChatSubmissionExecuted}
	if level == "autonomous" {
		return sub, nil
	}
	pending := store.ChatApprovalPending
	sub.ApprovalID, sub.ReviewState = approvalID, &pending
	approval := &store.ChatApproval{ID: approvalID, Subject: store.ChatSubjectQuestion, State: pending, Mode: store.ChatApprovalModeDecision}
	if level == "confirm_required" {
		sub.Status = store.ChatSubmissionProposed
	}
	if level == "pending_review" {
		approval.Mode = store.ChatApprovalModeReview
	}
	return sub, approval
}

func (s *Service) Create(ctx context.Context, actor, course string, in CreateInput) (CreateResponse, error) {
	actor = normalizeActor(actor)
	if err := validateMutation(in.Body, in.IdempotencyKey, in.AttachmentIDs); err != nil {
		return CreateResponse{}, err
	}
	title, err := chatTitle(in.Title, in.Body)
	if err != nil {
		return CreateResponse{}, err
	}
	agentID := normalizeActor(in.AgentID)
	if agentID == "" || course == "" {
		return CreateResponse{}, ErrDenied
	}
	auth, err := s.authorize(ctx, actor, course, agentID)
	if err != nil {
		return CreateResponse{}, err
	}
	fp := fingerprint(struct {
		Agent, Title, Body string
		IDs                []string
	}{agentID, title, in.Body, in.AttachmentIDs})
	convID, msgID, genID, answerID := s.newID(), s.newID(), s.newID(), s.newID()
	submission, approval := questionSubmission(auth.Grant.ConversationAskLevel, s.newID())
	snap, replay, err := s.store.CreateChatConversation(ctx, store.ChatCreateParams{
		Conversation: store.ChatConversation{ID: convID, CourseID: course, AgentID: agentID, Title: title, OpenerActorID: actor},
		Message:      store.ChatMessage{ID: msgID, Body: in.Body}, GenerationID: genID, AssistantMsgID: answerID,
		AttachmentIDs: in.AttachmentIDs, IdempotencyKey: in.IdempotencyKey, RequestFingerprint: fp,
		Submission: submission, Approval: approval, AnswerLevel: auth.Grant.ConversationAnswerLevel})
	if err != nil {
		return CreateResponse{}, mapStoreError(err)
	}
	if !replay && submission.Status != store.ChatSubmissionProposed {
		s.dispatcher.schedule(genID)
	}
	s.hub.Publish(snap.Conversation.ID)
	for _, m := range snap.Messages {
		if m.Role == store.ChatRoleUser && m.Seq == 1 {
			return CreateResponse{Snapshot: snap, Message: m}, nil
		}
	}
	return CreateResponse{}, serviceError("internal", "internal", "the first chat message is missing")
}

func (s *Service) Get(ctx context.Context, actor, course, conversation string) (Conversation, error) {
	c, err := s.store.ChatConversation(ctx, course, conversation, normalizeActor(actor))
	if err != nil {
		return Conversation{}, mapStoreError(err)
	}
	return c, nil
}

func (s *Service) Messages(ctx context.Context, actor, course, conversation string, limit int, before int64) (Snapshot, error) {
	snap, err := s.store.ChatSnapshot(ctx, course, conversation, normalizeActor(actor), limit, before)
	if err != nil {
		return Snapshot{}, mapStoreError(err)
	}
	return snap, nil
}

func (s *Service) Append(ctx context.Context, actor, course, conversation string, in AppendInput) (Snapshot, error) {
	actor = normalizeActor(actor)
	if err := validateMutation(in.Body, in.IdempotencyKey, in.AttachmentIDs); err != nil {
		return Snapshot{}, err
	}
	c, err := s.Get(ctx, actor, course, conversation)
	if err != nil {
		return Snapshot{}, err
	}
	auth, err := s.authorize(ctx, actor, course, c.AgentID)
	if err != nil {
		return Snapshot{}, err
	}
	fp := fingerprint(struct {
		Conversation, Body string
		IDs                []string
	}{conversation, in.Body, in.AttachmentIDs})
	msgID, genID, answerID := s.newID(), s.newID(), s.newID()
	submission, approval := questionSubmission(auth.Grant.ConversationAskLevel, s.newID())
	snap, replay, err := s.store.AppendChatMessage(ctx, store.ChatAppendParams{CourseID: course, ConversationID: conversation, ActorID: actor,
		Message: store.ChatMessage{ID: msgID, Body: in.Body}, GenerationID: genID, AssistantMsgID: answerID,
		AttachmentIDs: in.AttachmentIDs, IdempotencyKey: in.IdempotencyKey, RequestFingerprint: fp,
		Submission: submission, Approval: approval, AnswerLevel: auth.Grant.ConversationAnswerLevel})
	if err != nil {
		return Snapshot{}, mapStoreError(err)
	}
	if !replay && submission.Status != store.ChatSubmissionProposed {
		s.dispatcher.schedule(genID)
	}
	s.hub.Publish(conversation)
	return snap, nil
}

// Withdrawal is ChatSnapshot plus the canonical retracted message copy.
type Withdrawal struct {
	Snapshot
	Retracted Message `json:"retracted"`
}

func (s *Service) Withdraw(ctx context.Context, actor, course, conversation, message string) (Withdrawal, error) {
	snap, err := s.store.WithdrawChatMessage(ctx, course, conversation, normalizeActor(actor), message, "withdrawn")
	if err != nil {
		return Withdrawal{}, mapStoreError(err)
	}
	s.dispatcher.abort(conversation)
	s.hub.Publish(conversation)
	for _, m := range snap.Messages {
		if m.ID == message {
			return Withdrawal{Snapshot: snap, Retracted: m}, nil
		}
	}
	return Withdrawal{}, serviceError("internal", "internal", "the retracted chat message is missing")
}

func (s *Service) CloseConversation(ctx context.Context, actor, course, conversation string) (Conversation, error) {
	c, err := s.store.CloseChatConversation(ctx, course, conversation, normalizeActor(actor), "closed_by_opener")
	if err != nil {
		return Conversation{}, mapStoreError(err)
	}
	s.dispatcher.abort(conversation)
	s.hub.Publish(conversation)
	return c, nil
}

func SafeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if at := strings.LastIndex(name, "/"); at >= 0 {
		name = name[at+1:]
	}
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == ';' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return cutRunes(name, 128)
}

func (s *Service) StageAttachment(ctx context.Context, actor, course, filename, mediaType string, data []byte, idempotencyKey string) (Attachment, error) {
	if !validKey(idempotencyKey) {
		return Attachment{}, serviceError("invalid_argument", "invalid_field", "Idempotency-Key is required and at most 200 bytes")
	}
	filename = SafeFilename(filename)
	sum := sha256.Sum256(data)
	fp := fingerprint(struct{ Filename, MediaType, SHA256 string }{filename, mediaType, hex.EncodeToString(sum[:])})
	a := store.ChatAttachment{ID: s.newID(), CourseID: course, OwnerActorID: normalizeActor(actor), Filename: filename,
		MediaType: mediaType, Size: int64(len(data)), Bytes: data, CreatedAt: s.now()}
	a, _, err := s.store.StageChatAttachmentIdempotent(ctx, a, idempotencyKey, fp)
	if errors.Is(err, store.ErrChatIdempotency) {
		return Attachment{}, mapStoreError(err)
	}
	if err != nil {
		return Attachment{}, serviceError("invalid_argument", "invalid_field", "attachment exceeds its media limit or has an unsupported type")
	}
	return a, nil
}

func (s *Service) Attachment(ctx context.Context, actor, course, id string) (Attachment, error) {
	a, err := s.store.ChatAttachment(ctx, course, id, normalizeActor(actor))
	if err != nil {
		return Attachment{}, mapStoreError(err)
	}
	return a, nil
}

func mapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrChatIdempotency):
		return serviceError("conflict", "idempotency_conflict", "this idempotency key was used for a different request")
	case errors.Is(err, store.ErrChatAttachments):
		return serviceError("invalid_argument", "invalid_attachment", "an attachment is duplicated, already bound, or exceeds the four-image limit")
	case errors.Is(err, store.ErrChatNotAwaiting):
		return serviceError("failed_precondition", "conversation_not_awaiting", "this conversation is closed or still waiting for its answer")
	case errors.Is(err, store.ErrChatWithdraw):
		return serviceError("failed_precondition", "cannot_withdraw", "only the latest live user question can be withdrawn")
	case errors.Is(err, store.ErrChatApproval):
		return serviceError("failed_precondition", "approval_stale", "this approval is withdrawn, closed, superseded or already decided")
	default:
		return serviceError("unavailable", "store_unavailable", "the runtime's chat store cannot be reached")
	}
}

func safeRunError(err error) (string, bool) {
	var re *RunError
	if errors.As(err, &re) {
		kind := re.Kind
		if kind == "" {
			kind = "provider_failed"
		}
		return kind, re.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", true
	}
	if errors.Is(err, context.Canceled) {
		return "aborted", false
	}
	return "provider_failed", false
}

func (s *Service) recordAnswer(gen store.ChatGeneration, c store.ChatConversation, result store.ChatResult, grant *Grant) {
	agentID := result.AgentID
	if agentID == "" {
		agentID = c.RuntimeAgentID
	}
	if agentID == "" {
		agentID = c.AgentID
	}
	a := store.AnswerRecord{ID: gen.ID, At: s.now(), TenantID: result.TenantID, AgentID: agentID, CourseID: c.CourseID,
		ConversationID: c.ID, MessageID: gen.UserMessageID, Outcome: store.OutcomePosted, Billable: true,
		MemberID: result.MemberID, KeySource: result.KeySource, Turns: result.Turns, ToolCalls: result.ToolCalls, Writes: result.Writes}
	if grant != nil {
		a.OpenerMemberID = grant.OpenerMemberID
		if a.MemberID == "" {
			a.MemberID = grant.SelfMemberID
		}
	}
	if result.Usage != nil {
		a.InputTokens, a.OutputTokens = result.Usage.Input, result.Usage.Output
		if s.priceTable != nil && result.Provider != "" && result.Model != "" {
			table := s.priceTable()
			if table != nil {
				loc := time.UTC
				if s.location != nil && s.location() != nil {
					loc = s.location()
				}
				u := provider.Usage{Input: result.Usage.Input, Output: result.Usage.Output, Reasoning: result.Usage.Reasoning, CacheRead: result.Usage.CacheRead, CacheWrite: result.Usage.CacheWrite}
				if price, ok := table.LookupIn(result.Provider, result.Model, a.At, loc); ok {
					a.CostPUSD = price.Cost(u)
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.RecordAnswer(ctx, a); err != nil {
		s.log.Warn("chat answer ledger write failed", "generation", gen.ID)
	}
}

func (s *Service) String() string { return fmt.Sprintf("AR chat service %s", s.workerID) }
