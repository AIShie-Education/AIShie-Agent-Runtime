package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
)

const (
	chatDefaultPageSize = 50
	chatMaxPageSize     = 200
	chatImageMaxBytes   = 5 << 20
	chatTextMaxBytes    = 32 << 10
	chatMaxIdempotency  = 200
)

// ChatService is the API's narrow face of AR-owned browser chat. The Hub is
// only a local wake-up mechanism: stream correctness comes from Messages
// reading a fresh store snapshot after every wake and polling tick.
type ChatService interface {
	Agents(ctx context.Context, actor, course string) ([]chat.Agent, error)
	ListConversations(ctx context.Context, actor, course, after string, limit int) ([]chat.Conversation, string, error)
	Create(ctx context.Context, actor, course string, in chat.CreateInput) (chat.CreateResponse, error)
	Get(ctx context.Context, actor, course, conversation string) (chat.Conversation, error)
	Messages(ctx context.Context, actor, course, conversation string, limit int, before int64) (chat.Snapshot, error)
	Append(ctx context.Context, actor, course, conversation string, in chat.AppendInput) (chat.Snapshot, error)
	Withdraw(ctx context.Context, actor, course, conversation, message string) (chat.Withdrawal, error)
	CloseConversation(ctx context.Context, actor, course, conversation string) (chat.Conversation, error)
	StageAttachment(ctx context.Context, actor, course, filename, mediaType string, data []byte, idempotencyKey string) (chat.Attachment, error)
	Attachment(ctx context.Context, actor, course, id string) (chat.Attachment, error)
	CheckAccess(ctx context.Context, actor, course, agent string) (*chat.Authorization, error)
	ListApprovals(ctx context.Context, actor, course, state, after string, limit int) ([]chat.Approval, string, error)
	GetApproval(ctx context.Context, actor, id string) (chat.ApprovalDetail, error)
	ApprovalAttachment(ctx context.Context, actor, approvalID, attachmentID string) (chat.Attachment, error)
	Decide(ctx context.Context, actor, id string, in chat.DecideInput) (chat.DecisionResponse, error)
	Review(ctx context.Context, actor, id string, in chat.ReviewInput) (chat.DecisionResponse, error)
	Hub() *chat.Hub
}

var _ ChatService = (*chat.Service)(nil)

func (s *Server) chatService(w http.ResponseWriter) ChatService {
	if s.o.Chat == nil {
		WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonStoreUnavailable,
			Message: "AR chat is not configured on this runtime"})
		return nil
	}
	return s.o.Chat
}

// chatAPIError converts only chat's deliberately safe errors. Arbitrary
// implementation, Core and provider errors never become response text.
func chatAPIError(err error) Error {
	var ce *chat.Error
	if errors.As(err, &ce) {
		code, reason, message := ce.Code, ce.Reason, ce.Message
		if _, ok := codeStatus[code]; !ok {
			code, reason, message = CodeInternal, ReasonInternal, "the chat request could not be completed"
		}
		if message == "" {
			message = "the chat request could not be completed"
		}
		details := make(map[string]any, len(ce.Details))
		for k, v := range ce.Details {
			details[k] = v
		}
		return Error{Code: code, Reason: reason, Message: message, Details: details}
	}
	return Error{Code: CodeUnavailable, Reason: ReasonStoreUnavailable,
		Message: "the runtime's chat service cannot complete the request"}
}

func writeChatError(w http.ResponseWriter, err error) {
	WriteError(w, chatAPIError(err))
}

func normalizeChatSnapshot(snap chat.Snapshot) chat.Snapshot {
	if snap.Messages == nil {
		snap.Messages = []chat.Message{}
	}
	return snap
}

func chatMutationStatus(snap chat.Snapshot) int {
	if snap.Submission != nil && snap.Submission.Status == "proposed" {
		return http.StatusAccepted
	}
	return http.StatusOK
}

func chatLimit(raw string) (int, bool) {
	if raw == "" {
		return chatDefaultPageSize, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 1 && n <= chatMaxPageSize && strconv.Itoa(n) == raw
}

func (s *Server) chatAgents(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	agents, err := svc.Agents(r.Context(), c.ActorID, r.PathValue("course_id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	if agents == nil {
		agents = []chat.Agent{}
	}
	writeJSON(w, http.StatusOK, struct {
		Agents []chat.Agent `json:"agents"`
	}{agents})
}

func (s *Server) chatListConversations(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	q, ok := queryOf(w, r, "limit", "after")
	if !ok {
		return
	}
	limit, ok := chatLimit(q["limit"])
	if !ok {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "limit", "limit is a decimal integer from 1 to 200"))
		return
	}
	rows, next, err := svc.ListConversations(r.Context(), c.ActorID, r.PathValue("course_id"), q["after"], limit)
	if err != nil {
		writeChatError(w, err)
		return
	}
	if rows == nil {
		rows = []chat.Conversation{}
	}
	writeJSON(w, http.StatusOK, struct {
		Conversations []chat.Conversation `json:"conversations"`
		Next          *string             `json:"next"`
	}{rows, chatNext(next)})
}

func chatNext(next string) *string {
	if next == "" {
		return nil
	}
	return &next
}

func (s *Server) chatCreateConversation(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	var in chat.CreateInput
	if !readBody(w, r, &in) {
		return
	}
	created, err := svc.Create(r.Context(), c.ActorID, r.PathValue("course_id"), in)
	if err != nil {
		writeChatError(w, err)
		return
	}
	created.Snapshot = normalizeChatSnapshot(created.Snapshot)
	writeJSON(w, chatMutationStatus(created.Snapshot), created)
}

func (s *Server) chatGetConversation(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	conversation, err := svc.Get(r.Context(), c.ActorID, r.PathValue("course_id"), r.PathValue("conversation_id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Conversation chat.Conversation `json:"conversation"`
	}{conversation})
}

func (s *Server) chatMessages(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	q, ok := queryOf(w, r, "limit", "before_seq")
	if !ok {
		return
	}
	limit, ok := chatLimit(q["limit"])
	if !ok {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "limit", "limit is a decimal integer from 1 to 200"))
		return
	}
	var before int64
	if raw := q["before_seq"]; raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != raw {
			WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "before_seq", "before_seq is a positive decimal sequence number"))
			return
		}
		before = n
	}
	snap, err := svc.Messages(r.Context(), c.ActorID, r.PathValue("course_id"), r.PathValue("conversation_id"), limit, before)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeChatSnapshot(snap))
}

func (s *Server) chatAppendMessage(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	var in chat.AppendInput
	if !readBody(w, r, &in) {
		return
	}
	snap, err := svc.Append(r.Context(), c.ActorID, r.PathValue("course_id"), r.PathValue("conversation_id"), in)
	if err != nil {
		writeChatError(w, err)
		return
	}
	snap = normalizeChatSnapshot(snap)
	writeJSON(w, chatMutationStatus(snap), snap)
}

func (s *Server) chatWithdrawMessage(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	var in struct {
		MessageID string `json:"message_id"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.MessageID == "" {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonMissingField, "message_id", "message_id is required"))
		return
	}
	withdrawn, err := svc.Withdraw(r.Context(), c.ActorID, r.PathValue("course_id"), r.PathValue("conversation_id"), in.MessageID)
	if err != nil {
		writeChatError(w, err)
		return
	}
	withdrawn.Snapshot = normalizeChatSnapshot(withdrawn.Snapshot)
	writeJSON(w, http.StatusOK, withdrawn)
}

func (s *Server) chatCloseConversation(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	conversation, err := svc.CloseConversation(r.Context(), c.ActorID, r.PathValue("course_id"), r.PathValue("conversation_id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Conversation chat.Conversation `json:"conversation"`
		Draft        *chat.Draft       `json:"draft"`
	}{Conversation: conversation, Draft: nil})
}

func validChatApprovalState(state string) bool {
	switch state {
	case "", "pending", "approved", "rejected", "changes_requested", "reviewed", "cancelled":
		return true
	default:
		return false
	}
}

func (s *Server) chatListApprovals(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	q, ok := queryOf(w, r, "course_id", "state", "limit", "after")
	if !ok {
		return
	}
	course := q["course_id"]
	if course == "" {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonMissingField, "course_id", "course_id is required"))
		return
	}
	if !validChatApprovalState(q["state"]) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "state", "state is not a canonical chat approval state"))
		return
	}
	limit, ok := chatLimit(q["limit"])
	if !ok {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "limit", "limit is a decimal integer from 1 to 200"))
		return
	}
	approvals, next, err := svc.ListApprovals(r.Context(), c.ActorID, course, q["state"], q["after"], limit)
	if err != nil {
		writeChatError(w, err)
		return
	}
	if approvals == nil {
		approvals = []chat.Approval{}
	}
	writeJSON(w, http.StatusOK, struct {
		Approvals []chat.Approval `json:"approvals"`
		Next      *string         `json:"next"`
	}{approvals, chatNext(next)})
}

func (s *Server) chatGetApproval(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	detail, err := svc.GetApproval(r.Context(), c.ActorID, r.PathValue("approval_id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	detail.Snapshot = normalizeChatSnapshot(detail.Snapshot)
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) chatApprovalAttachment(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	attachment, err := svc.ApprovalAttachment(r.Context(), c.ActorID, r.PathValue("approval_id"), r.PathValue("attachment_id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeChatAttachmentBytes(w, attachment)
}

func validChatDecision(decision string) bool {
	return decision == "approve" || decision == "reject" || decision == "changes_requested"
}

func (s *Server) chatDecideApproval(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	var in chat.DecideInput
	if !readBody(w, r, &in) {
		return
	}
	if !validChatDecision(in.Decision) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "decision", "decision is approve, reject or changes_requested"))
		return
	}
	if !validChatIdempotencyKey(in.IdempotencyKey) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "idempotency_key", "idempotency_key is required and at most 200 bytes"))
		return
	}
	decided, err := svc.Decide(r.Context(), c.ActorID, r.PathValue("approval_id"), in)
	if err != nil {
		writeChatError(w, err)
		return
	}
	decided.Snapshot = normalizeChatSnapshot(decided.Snapshot)
	writeJSON(w, http.StatusOK, decided)
}

func (s *Server) chatReviewApproval(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	var in chat.ReviewInput
	if !readBody(w, r, &in) {
		return
	}
	if !validChatIdempotencyKey(in.IdempotencyKey) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "idempotency_key", "idempotency_key is required and at most 200 bytes"))
		return
	}
	reviewed, err := svc.Review(r.Context(), c.ActorID, r.PathValue("approval_id"), in)
	if err != nil {
		writeChatError(w, err)
		return
	}
	reviewed.Snapshot = normalizeChatSnapshot(reviewed.Snapshot)
	writeJSON(w, http.StatusOK, reviewed)
}

// chatUploadQuery is deliberately separate from queryOf: queryOf also invokes
// the bounded JSON decoder, while this route's body is raw image/text bytes.
func chatUploadQuery(w http.ResponseWriter, r *http.Request) (filename, key string, ok bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || strings.Contains(r.URL.RawQuery, ";") {
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter,
			Message: "the query cannot be read", Details: map[string]any{"field": ""}})
		return "", "", false
	}
	for name, values := range q {
		if name != "filename" && name != "key" {
			WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter,
				Message: "this route takes filename and key alone", Details: map[string]any{"field": name}})
			return "", "", false
		}
		if len(values) != 1 {
			WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, name, "a parameter is given once"))
			return "", "", false
		}
	}
	filename = q.Get("filename")
	for _, header := range []string{"X-Filename", "Filename"} {
		values := r.Header.Values(header)
		if len(values) > 1 {
			WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "filename", "filename is given once"))
			return "", "", false
		}
		if len(values) == 1 {
			if filename != "" && filename != values[0] {
				WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "filename", "filename header and query disagree"))
				return "", "", false
			}
			filename = values[0]
		}
	}
	if strings.TrimSpace(filename) == "" {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonMissingField, "filename", "a filename header or query is required"))
		return "", "", false
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) > 1 {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "idempotency_key", "Idempotency-Key is given once"))
		return "", "", false
	}
	if len(keys) == 1 {
		key = keys[0]
	}
	if queryKey := q.Get("key"); queryKey != "" {
		if key != "" && key != queryKey {
			WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "idempotency_key", "idempotency header and query disagree"))
			return "", "", false
		}
		key = queryKey
	}
	if !validChatIdempotencyKey(key) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "idempotency_key", "an Idempotency-Key header or key query of at most 200 bytes is required"))
		return "", "", false
	}
	return filename, key, true
}

func validChatIdempotencyKey(key string) bool {
	return key != "" && len(key) <= chatMaxIdempotency && strings.TrimSpace(key) == key &&
		!strings.ContainsFunc(key, unicode.IsControl)
}

func chatAttachmentMediaType(contentType string) (string, int64, bool) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", 0, false
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case strings.HasPrefix(mediaType, "image/") && len(mediaType) > len("image/"):
		return mediaType, chatImageMaxBytes, true
	case strings.HasPrefix(mediaType, "text/") && len(mediaType) > len("text/"):
		return mediaType, chatTextMaxBytes, true
	default:
		return "", 0, false
	}
}

func (s *Server) chatStageAttachment(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	filename, key, ok := chatUploadQuery(w, r)
	if !ok {
		return
	}
	mediaType, limit, ok := chatAttachmentMediaType(r.Header.Get("Content-Type"))
	if !ok {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "Content-Type", "chat attachments accept image/* or text/* alone"))
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonBodyTooLarge,
			Message: "the attachment exceeds its media size limit", Details: map[string]any{"max_bytes": limit}})
		return
	case err != nil:
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonInvalidField,
			Message: "the attachment bytes could not be read", Details: map[string]any{"field": "body"}})
		return
	}
	attachment, err := svc.StageAttachment(r.Context(), c.ActorID, r.PathValue("course_id"), filename, mediaType, data, key)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Attachment chat.Attachment `json:"attachment"`
	}{attachment})
}

func (s *Server) chatGetAttachment(w http.ResponseWriter, r *http.Request, c *Caller) {
	svc := s.chatService(w)
	if svc == nil {
		return
	}
	attachment, err := svc.Attachment(r.Context(), c.ActorID, r.PathValue("course_id"), r.PathValue("attachment_id"))
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeChatAttachmentBytes(w, attachment)
}

// writeChatAttachmentBytes is only the safe representation shared by owner
// and reviewer downloads. Authorization and ownership/binding lookup remain
// strictly separate in Attachment and ApprovalAttachment.
func writeChatAttachmentBytes(w http.ResponseWriter, attachment chat.Attachment) {
	mediaType, _, ok := chatAttachmentMediaType(attachment.MediaType)
	if !ok || attachment.Size != int64(len(attachment.Bytes)) || attachment.Size < 0 {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the stored attachment metadata is invalid"})
		return
	}
	filename := chat.SafeFilename(attachment.Filename)
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = `attachment; filename="attachment"`
	}
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Content-Disposition", disposition)
	h.Set("Content-Length", strconv.FormatInt(attachment.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(attachment.Bytes)
}
