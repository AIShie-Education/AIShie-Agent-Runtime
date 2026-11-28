package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

type chatStageCall struct {
	actor, course, filename, mediaType, key string
	data                                    []byte
}

type fakeChatService struct {
	mu sync.Mutex

	hub                        *chat.Hub
	owner, reviewer            string
	snap                       chat.Snapshot
	reviewerSnapshot           chat.Snapshot
	agents                     []chat.Agent
	list                       []chat.Conversation
	next                       string
	attachment                 chat.Attachment
	approvalAttachment         chat.Attachment
	approvalAttachmentApproval string

	checkErr error
	checks   int
	reads    int
	onRead   func(int)

	lastAfter                                                string
	lastLimit                                                int
	lastBefore                                               int64
	lastCreate                                               chat.CreateInput
	lastAppend                                               chat.AppendInput
	lastMessage                                              string
	approvals                                                []chat.Approval
	approvalNext                                             string
	lastApprovalCourse, lastApprovalState, lastApprovalAfter string
	lastDecision                                             chat.DecideInput
	lastReview                                               chat.ReviewInput
	decisionErr, reviewErr                                   error
	stageCalls                                               []chatStageCall
	staged                                                   map[string]chatStageCall
	stageStores                                              int

	reconcile, heartbeat, writeTimeout time.Duration
}

func newFakeChatService() *fakeChatService {
	conversation := chat.Conversation{ID: "conv-1", CourseID: "course-1", AgentID: "agent-1", Title: "Question",
		State: chat.Answered, Revision: 2, CreatedAt: at, UpdatedAt: at, LastActivityAt: at}
	message := chat.Message{ID: "msg-1", ConversationID: conversation.ID, Seq: 1, Role: "user",
		Status: "posted", Body: "What is the answer?", CreatedAt: at}
	return &fakeChatService{hub: chat.NewHub(), owner: yuki,
		snap: chat.Snapshot{Conversation: conversation, Messages: []chat.Message{message}, More: false,
			Submission: &store.ChatSubmission{Status: store.ChatSubmissionExecuted, ReviewState: nil}},
		agents: []chat.Agent{{AgentID: "agent-1", DisplayName: "Tutor", AnswersCourse: true, AnswerLevel: "autonomous"}},
		list:   []chat.Conversation{conversation}, next: "cursor-2",
		approvals: []chat.Approval{{ID: "approval-1", CourseID: "course-1", ConversationID: "conv-1", Subject: "question",
			MessageID: "msg-1", State: "pending", CreatedAt: at}}, approvalNext: "approval-cursor-2",
		attachment: chat.Attachment{ID: "att-1", Filename: "notes.txt", MediaType: "text/plain", Size: 4,
			CreatedAt: at, Bytes: []byte("note")}, staged: map[string]chatStageCall{},
		approvalAttachment: chat.Attachment{ID: "att-review", Filename: "../../review\"\r\nX-Evil: yes.txt", MediaType: "text/plain",
			Size: 12, CreatedAt: at, Bytes: []byte("review bytes")}, approvalAttachmentApproval: "approval-1",
		reconcile: time.Hour, heartbeat: time.Hour, writeTimeout: time.Second}
}

func (f *fakeChatService) chatStreamIntervals() (time.Duration, time.Duration, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reconcile, f.heartbeat, f.writeTimeout
}

func (f *fakeChatService) owns(actor string) bool { return f.owner == "" || f.owner == actor }

func (f *fakeChatService) canReview(actor string) bool {
	if f.reviewer != "" {
		return actor == f.reviewer
	}
	return f.owns(actor)
}

func (f *fakeChatService) Agents(_ context.Context, actor, _ string) ([]chat.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.owns(actor) {
		return []chat.Agent{}, nil
	}
	return append([]chat.Agent(nil), f.agents...), nil
}

func (f *fakeChatService) ListConversations(_ context.Context, actor, _, after string, limit int) ([]chat.Conversation, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAfter, f.lastLimit = after, limit
	if !f.owns(actor) {
		return []chat.Conversation{}, "", nil
	}
	return append([]chat.Conversation(nil), f.list...), f.next, nil
}

func (f *fakeChatService) Create(_ context.Context, actor, _ string, in chat.CreateInput) (chat.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreate = in
	if !f.owns(actor) {
		return chat.CreateResponse{}, chat.ErrNotFound
	}
	return chat.CreateResponse{Snapshot: f.snap, Message: f.snap.Messages[0]}, nil
}

func (f *fakeChatService) Get(_ context.Context, actor, _, _ string) (chat.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.owns(actor) {
		return chat.Conversation{}, chat.ErrNotFound
	}
	return f.snap.Conversation, nil
}

func (f *fakeChatService) Messages(_ context.Context, actor, _, _ string, limit int, before int64) (chat.Snapshot, error) {
	f.mu.Lock()
	if !f.owns(actor) {
		f.mu.Unlock()
		return chat.Snapshot{}, chat.ErrNotFound
	}
	f.reads++
	call := f.reads
	f.lastLimit, f.lastBefore = limit, before
	snap := f.snap
	snap.Messages = append([]chat.Message(nil), f.snap.Messages...)
	if f.snap.Draft != nil {
		d := *f.snap.Draft
		snap.Draft = &d
	}
	onRead := f.onRead
	f.mu.Unlock()
	if onRead != nil {
		onRead(call)
	}
	return snap, nil
}

func (f *fakeChatService) Append(_ context.Context, actor, _, _ string, in chat.AppendInput) (chat.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAppend = in
	if !f.owns(actor) {
		return chat.Snapshot{}, chat.ErrNotFound
	}
	return f.snap, nil
}

func (f *fakeChatService) Withdraw(_ context.Context, actor, _, _, message string) (chat.Withdrawal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMessage = message
	if !f.owns(actor) {
		return chat.Withdrawal{}, chat.ErrNotFound
	}
	retracted := f.snap.Messages[0]
	retracted.Status, retracted.Body = "retracted", ""
	return chat.Withdrawal{Snapshot: f.snap, Retracted: retracted}, nil
}

func (f *fakeChatService) CloseConversation(_ context.Context, actor, _, _ string) (chat.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.owns(actor) {
		return chat.Conversation{}, chat.ErrNotFound
	}
	c := f.snap.Conversation
	c.State, c.CloseReason = chat.Closed, "closed_by_opener"
	return c, nil
}

func chatStageFingerprint(call chatStageCall) string {
	h := sha256.New()
	_, _ = h.Write([]byte(call.filename))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(call.mediaType))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(call.data)
	return hex.EncodeToString(h.Sum(nil))
}

func (f *fakeChatService) StageAttachment(_ context.Context, actor, course, filename, mediaType string, data []byte, key string) (chat.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := chatStageCall{actor: actor, course: course, filename: filename, mediaType: mediaType, data: append([]byte(nil), data...), key: key}
	f.stageCalls = append(f.stageCalls, call)
	if !f.owns(actor) {
		return chat.Attachment{}, chat.ErrNotFound
	}
	mapKey := actor + "\x00" + course + "\x00" + key
	if old, ok := f.staged[mapKey]; ok {
		if chatStageFingerprint(old) != chatStageFingerprint(call) {
			return chat.Attachment{}, &chat.Error{Code: CodeConflict, Reason: "idempotency_conflict", Message: "this idempotency key was used for different attachment bytes"}
		}
		a := f.attachment
		a.Size = int64(len(old.data))
		a.Bytes = append([]byte(nil), old.data...)
		return a, nil
	}
	f.staged[mapKey] = call
	f.stageStores++
	a := f.attachment
	a.Filename, a.MediaType = chat.SafeFilename(filename), mediaType
	a.Size, a.Bytes = int64(len(data)), append([]byte(nil), data...)
	a.CreatedAt = at
	return a, nil
}

func (f *fakeChatService) Attachment(_ context.Context, actor, _, id string) (chat.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.owns(actor) || id != f.attachment.ID {
		return chat.Attachment{}, chat.ErrNotFound
	}
	a := f.attachment
	a.Bytes = append([]byte(nil), f.attachment.Bytes...)
	return a, nil
}

func (f *fakeChatService) CheckAccess(_ context.Context, actor, _, _ string) (*chat.Authorization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	if !f.owns(actor) {
		return nil, chat.ErrDenied
	}
	return &chat.Authorization{}, nil
}

func (f *fakeChatService) ListApprovals(_ context.Context, actor, course, state, after string, limit int) ([]chat.Approval, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastApprovalCourse, f.lastApprovalState, f.lastApprovalAfter, f.lastLimit = course, state, after, limit
	if !f.canReview(actor) {
		return []chat.Approval{}, "", nil
	}
	return append([]chat.Approval(nil), f.approvals...), f.approvalNext, nil
}

func (f *fakeChatService) GetApproval(_ context.Context, actor, id string) (chat.ApprovalDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.canReview(actor) || len(f.approvals) == 0 || f.approvals[0].ID != id {
		return chat.ApprovalDetail{}, chat.ErrNotFound
	}
	snap := f.snap
	if f.reviewerSnapshot.Conversation.ID != "" {
		snap = f.reviewerSnapshot
	}
	return chat.ApprovalDetail{Approval: f.approvals[0], Snapshot: snap}, nil
}

func (f *fakeChatService) ApprovalAttachment(_ context.Context, actor, approvalID, attachmentID string) (chat.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.canReview(actor) || approvalID != f.approvalAttachmentApproval || attachmentID != f.approvalAttachment.ID {
		return chat.Attachment{}, chat.ErrNotFound
	}
	a := f.approvalAttachment
	a.Bytes = append([]byte(nil), a.Bytes...)
	return a, nil
}

func (f *fakeChatService) Decide(_ context.Context, actor, id string, in chat.DecideInput) (chat.DecisionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDecision = in
	if !f.canReview(actor) || len(f.approvals) == 0 || f.approvals[0].ID != id {
		return chat.DecisionResponse{}, chat.ErrNotFound
	}
	if f.decisionErr != nil {
		return chat.DecisionResponse{}, f.decisionErr
	}
	a := f.approvals[0]
	switch in.Decision {
	case "approve":
		a.State = "approved"
	case "reject":
		a.State = "rejected"
	case "changes_requested":
		a.State = "changes_requested"
	}
	a.Reason = in.Reason
	return chat.DecisionResponse{Approval: a, Snapshot: f.snap}, nil
}

func (f *fakeChatService) Review(_ context.Context, actor, id string, in chat.ReviewInput) (chat.DecisionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReview = in
	if !f.canReview(actor) || len(f.approvals) == 0 || f.approvals[0].ID != id {
		return chat.DecisionResponse{}, chat.ErrNotFound
	}
	if f.reviewErr != nil {
		return chat.DecisionResponse{}, f.reviewErr
	}
	a := f.approvals[0]
	a.State = "reviewed"
	return chat.DecisionResponse{Approval: a, Snapshot: f.snap}, nil
}

func (f *fakeChatService) Hub() *chat.Hub { return f.hub }

func (f *fakeChatService) setCheckErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkErr = err
}

func (f *fakeChatService) counts() (checks, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.reads
}

func newChatFixture(t *testing.T, svc ChatService) (*fixture, string) {
	t.Helper()
	f := newFixture(t, func(o *Options) { o.Chat = svc })
	return f, f.core.assert(t, claims(yuki, "", nil))
}

func chatCoursePath(suffix string) string { return Prefix + "courses/course-1/" + suffix }

func TestChatRouteSuccessShapes(t *testing.T) {
	svc := newFakeChatService()
	f, token := newChatFixture(t, svc)

	a := f.get(chatCoursePath("chat/agents"), token)
	if a.code != http.StatusOK || !strings.Contains(a.body, `"agents":[{"agent_id":"agent-1","display_name":"Tutor","answers_course":true,"answer_level":"autonomous","is_my_delegate":false}]`) {
		t.Fatalf("agents: %d %s", a.code, a.body)
	}
	a = f.get(chatCoursePath("conversations?limit=1&after=cursor-1"), token)
	if a.code != http.StatusOK || !strings.Contains(a.body, `"next":"cursor-2"`) || svc.lastAfter != "cursor-1" || svc.lastLimit != 1 {
		t.Fatalf("list: %d %s after=%q limit=%d", a.code, a.body, svc.lastAfter, svc.lastLimit)
	}
	createBody := `{"agent_id":"agent-1","title":"Question","body":"What is the answer?","attachment_ids":["att-1"],"idempotency_key":"create-1"}`
	a = f.send(req{method: http.MethodPost, path: chatCoursePath("conversations"), token: token, body: strings.NewReader(createBody), headers: []string{"Content-Type", "application/json"}})
	if a.code != http.StatusOK || !strings.Contains(a.body, `"message":{"id":"msg-1"`) || !strings.Contains(a.body, `"messages":[{"id":"msg-1"`) || svc.lastCreate.IdempotencyKey != "create-1" {
		t.Fatalf("create: %d %s input=%+v", a.code, a.body, svc.lastCreate)
	}
	var created map[string]json.RawMessage
	a.decode(t, &created)
	for _, key := range []string{"conversation", "messages", "more", "draft", "message", "submission"} {
		if _, ok := created[key]; !ok {
			t.Errorf("create lacks %q: %s", key, a.body)
		}
	}
	if string(created["draft"]) != "null" {
		t.Errorf("create draft is not null: %s", created["draft"])
	}
	if string(created["submission"]) != `{"status":"executed","review_state":null}` {
		t.Errorf("autonomous create submission: %s", created["submission"])
	}

	a = f.get(chatCoursePath("conversations/conv-1"), token)
	if a.code != http.StatusOK || !strings.Contains(a.body, `"conversation":{"id":"conv-1","course_id":"course-1","agent_id":"agent-1"`) {
		t.Fatalf("get conversation: %d %s", a.code, a.body)
	}
	a = f.get(chatCoursePath("conversations/conv-1/messages?limit=2&before_seq=9"), token)
	if a.code != http.StatusOK || svc.lastLimit != 2 || svc.lastBefore != 9 || !strings.Contains(a.body, `"more":false,"draft":null`) {
		t.Fatalf("messages: %d %s limit=%d before=%d", a.code, a.body, svc.lastLimit, svc.lastBefore)
	}
	appendBody := `{"body":"Another question","attachment_ids":[],"idempotency_key":"ask-1"}`
	a = f.send(req{method: http.MethodPost, path: chatCoursePath("conversations/conv-1/messages"), token: token, body: strings.NewReader(appendBody), headers: []string{"Content-Type", "application/json"}})
	if a.code != http.StatusOK || svc.lastAppend.IdempotencyKey != "ask-1" || !strings.Contains(a.body, `"messages":[`) {
		t.Fatalf("append: %d %s input=%+v", a.code, a.body, svc.lastAppend)
	}
	a = f.send(req{method: http.MethodPost, path: chatCoursePath("conversations/conv-1/withdraw"), token: token, body: strings.NewReader(`{"message_id":"msg-1"}`), headers: []string{"Content-Type", "application/json"}})
	if a.code != http.StatusOK || svc.lastMessage != "msg-1" || !strings.Contains(a.body, `"retracted":{"id":"msg-1"`) || !strings.Contains(a.body, `"status":"retracted","body":""`) {
		t.Fatalf("withdraw: %d %s", a.code, a.body)
	}
	a = f.send(req{method: http.MethodPost, path: chatCoursePath("conversations/conv-1/close"), token: token})
	if a.code != http.StatusOK || !strings.Contains(a.body, `"state":"closed"`) || !strings.Contains(a.body, `"draft":null`) {
		t.Fatalf("close: %d %s", a.code, a.body)
	}
}

func TestChatProposedMutationsAreAcceptedWithoutExecution(t *testing.T) {
	for _, tc := range []struct {
		name, submission, review string
		status                   int
	}{
		{"confirm required is proposed", store.ChatSubmissionProposed, "pending", http.StatusAccepted},
		{"post review executed", store.ChatSubmissionExecuted, "pending", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeChatService()
			svc.snap.Submission = &store.ChatSubmission{Status: tc.submission, ApprovalID: "approval-1", ReviewState: &tc.review}
			f, token := newChatFixture(t, svc)
			createBody := `{"agent_id":"agent-1","body":"pending question","idempotency_key":"pending-create"}`
			create := req{method: http.MethodPost, path: chatCoursePath("conversations"), token: token,
				headers: []string{"Content-Type", "application/json"}}
			for range 2 { // replay returns the original accepted proposal, not a 4xx
				create.body = strings.NewReader(createBody)
				a := f.send(create)
				if a.code != tc.status || !strings.Contains(a.body, `"submission":{"status":"`+tc.submission+`","approval_id":"approval-1","review_state":"pending"}`) {
					t.Fatalf("proposed create: %d %s", a.code, a.body)
				}
			}
			appendBody := `{"body":"another pending question","idempotency_key":"pending-append"}`
			a := f.send(req{method: http.MethodPost, path: chatCoursePath("conversations/conv-1/messages"), token: token,
				body: strings.NewReader(appendBody), headers: []string{"Content-Type", "application/json"}})
			if a.code != tc.status || !strings.Contains(a.body, `"submission":{"status":"`+tc.submission+`","approval_id":"approval-1","review_state":"pending"}`) {
				t.Fatalf("proposed append: %d %s", a.code, a.body)
			}
		})
	}
}

func requireCanonicalChatApprovalJSON(t *testing.T, raw json.RawMessage, wantState string) {
	t.Helper()
	var approval map[string]json.RawMessage
	if err := json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "course_id", "conversation_id", "subject", "message_id", "state", "created_at"} {
		if _, ok := approval[key]; !ok {
			t.Errorf("approval lacks canonical %q: %s", key, raw)
		}
	}
	allowed := map[string]bool{"id": true, "course_id": true, "conversation_id": true, "subject": true, "message_id": true,
		"state": true, "reason": true, "created_at": true, "decided_at": true}
	for key := range approval {
		if !allowed[key] {
			t.Errorf("approval exposes non-canonical %q: %s", key, raw)
		}
	}
	var subject, state string
	_ = json.Unmarshal(approval["subject"], &subject)
	_ = json.Unmarshal(approval["state"], &state)
	if (subject != "question" && subject != "answer") || state != wantState {
		t.Errorf("approval subject/state = %q/%q, want question|answer / %q: %s", subject, state, wantState, raw)
	}
}

func TestChatApprovalRoutesUseCanonicalJSON(t *testing.T) {
	svc := newFakeChatService()
	f, token := newChatFixture(t, svc)
	listPath := Prefix + "chat/approvals?course_id=course-1&state=pending&limit=1&after=approval-cursor-1"
	a := f.get(listPath, token)
	if a.code != http.StatusOK || svc.lastApprovalCourse != "course-1" || svc.lastApprovalState != "pending" ||
		svc.lastApprovalAfter != "approval-cursor-1" || svc.lastLimit != 1 {
		t.Fatalf("approval list: %d %s %+v", a.code, a.body, svc)
	}
	var listed struct {
		Approvals []json.RawMessage `json:"approvals"`
		Next      *string           `json:"next"`
	}
	a.decode(t, &listed)
	if len(listed.Approvals) != 1 || listed.Next == nil || *listed.Next != "approval-cursor-2" {
		t.Fatalf("approval list envelope: %s", a.body)
	}
	requireCanonicalChatApprovalJSON(t, listed.Approvals[0], "pending")

	a = f.get(Prefix+"chat/approvals/approval-1", token)
	var got struct {
		Approval json.RawMessage `json:"approval"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	a.decode(t, &got)
	if a.code != http.StatusOK || len(got.Approval) == 0 || len(got.Snapshot) == 0 {
		t.Fatalf("approval get detail: %d %s", a.code, a.body)
	}
	requireCanonicalChatApprovalJSON(t, got.Approval, "pending")

	for _, tc := range []struct{ decision, state string }{
		{"approve", "approved"}, {"reject", "rejected"}, {"changes_requested", "changes_requested"},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			body := `{"decision":"` + tc.decision + `","reason":"reviewed carefully","idempotency_key":"decision-` + tc.decision + `"}`
			sendDecision := func() answer {
				return f.send(req{method: http.MethodPost, path: Prefix + "chat/approvals/approval-1/decision", token: token,
					body: strings.NewReader(body), headers: []string{"Content-Type", "application/json"}})
			}
			for range 2 { // decision replay is a successful response too
				a = sendDecision()
				if a.code != http.StatusOK {
					t.Fatalf("decision: %d %s", a.code, a.body)
				}
				var out struct {
					Approval json.RawMessage `json:"approval"`
					Snapshot json.RawMessage `json:"snapshot"`
				}
				a.decode(t, &out)
				requireCanonicalChatApprovalJSON(t, out.Approval, tc.state)
				if len(out.Snapshot) == 0 || svc.lastDecision.IdempotencyKey != "decision-"+tc.decision {
					t.Errorf("decision response/input: %s %+v", a.body, svc.lastDecision)
				}
			}
		})
	}
	a = f.send(req{method: http.MethodPost, path: Prefix + "chat/approvals/approval-1/review", token: token,
		body: strings.NewReader(`{"idempotency_key":"review-1"}`), headers: []string{"Content-Type", "application/json"}})
	var reviewed struct {
		Approval json.RawMessage `json:"approval"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	a.decode(t, &reviewed)
	if a.code != http.StatusOK || svc.lastReview.IdempotencyKey != "review-1" || len(reviewed.Snapshot) == 0 {
		t.Fatalf("review: %d %s input=%+v", a.code, a.body, svc.lastReview)
	}
	requireCanonicalChatApprovalJSON(t, reviewed.Approval, "reviewed")

	badReview := f.send(req{method: http.MethodPost, path: Prefix + "chat/approvals/approval-1/review", token: token,
		body: strings.NewReader(`{"idempotency_key":"review-2","outcome":"reviewed"}`), headers: []string{"Content-Type", "application/json"}})
	wantRefused(t, badReview, http.StatusBadRequest, CodeInvalidArgument, ReasonUnknownField)
	missingCourse := f.get(Prefix+"chat/approvals?state=pending", token)
	wantRefused(t, missingCourse, http.StatusBadRequest, CodeInvalidArgument, ReasonMissingField)
	internalState := f.get(Prefix+"chat/approvals?course_id=course-1&state=pending_review", token)
	wantRefused(t, internalState, http.StatusBadRequest, CodeInvalidArgument, ReasonInvalidField)
	invalidDecision := f.send(req{method: http.MethodPost, path: Prefix + "chat/approvals/approval-1/decision", token: token,
		body: strings.NewReader(`{"decision":"executed","idempotency_key":"bad-decision"}`), headers: []string{"Content-Type", "application/json"}})
	wantRefused(t, invalidDecision, http.StatusBadRequest, CodeInvalidArgument, ReasonInvalidField)

	other := f.core.assert(t, claims(ken, "", nil))
	unowned := f.get(Prefix+"chat/approvals/approval-1", other)
	wantRefused(t, unowned, http.StatusNotFound, CodeNotFound, "not_found")
	unauthenticated := f.get(Prefix+"chat/approvals?course_id=course-1", "")
	wantRefused(t, unauthenticated, http.StatusUnauthorized, CodeUnauthenticated, "assertion_missing")
}

func TestChatApprovalDetailReviewerBodyAndOpenerScrubbing(t *testing.T) {
	svc := newFakeChatService()
	svc.owner, svc.reviewer = ken, yuki
	svc.snap.Conversation.State = chat.ReplyPendingApproval
	svc.approvals[0].Subject, svc.approvals[0].MessageID = "answer", "msg-2"
	svc.snap.Messages = []chat.Message{
		{ID: "msg-1", ConversationID: "conv-1", Seq: 1, Role: "user", Status: "posted", Body: "question body", CreatedAt: at,
			Attachments: []chat.Attachment{{ID: "att-q", Filename: "question.png", MediaType: "image/png", Size: 3}}},
		{ID: "msg-2", ConversationID: "conv-1", Seq: 2, Role: "assistant", Status: "pending", Body: "", CreatedAt: at},
	}
	svc.reviewerSnapshot = svc.snap
	svc.reviewerSnapshot.Messages = append([]chat.Message(nil), svc.snap.Messages...)
	svc.reviewerSnapshot.Messages[1].Body = "pending answer body for reviewers"
	svc.reviewerSnapshot.Messages[1].Sources = []chat.Source{{DocumentID: "document-review-1"}}
	svc.reviewerSnapshot.Messages[1].Usage = &chat.Usage{Input: 10, Output: 5}
	f, reviewer := newChatFixture(t, svc)
	opener := f.core.assert(t, claims(ken, "", nil))

	detail := f.get(Prefix+"chat/approvals/approval-1", reviewer)
	if detail.code != http.StatusOK || !strings.Contains(detail.body, `"snapshot":{"conversation":`) ||
		!strings.Contains(detail.body, `"id":"msg-1"`) || !strings.Contains(detail.body, `"body":"question body"`) ||
		!strings.Contains(detail.body, `"id":"msg-2"`) || !strings.Contains(detail.body, `"status":"pending","body":"pending answer body for reviewers"`) ||
		!strings.Contains(detail.body, `"document_id":"document-review-1"`) ||
		!strings.Contains(detail.body, `"filename":"question.png"`) {
		t.Fatalf("reviewer detail lacks authorized snapshot content: %d %s", detail.code, detail.body)
	}
	denied := f.get(Prefix+"chat/approvals/approval-1", opener)
	wantRefused(t, denied, http.StatusNotFound, CodeNotFound, "not_found")
	if strings.Contains(denied.body, "pending answer body") {
		t.Fatalf("opener detail denial leaked pending body: %s", denied.body)
	}

	messages := f.get(chatCoursePath("conversations/conv-1/messages"), opener)
	if messages.code != http.StatusOK || !strings.Contains(messages.body, `"id":"msg-2"`) || !strings.Contains(messages.body, `"body":""`) {
		t.Fatalf("opener scrubbed messages: %d %s", messages.code, messages.body)
	}
	for _, hidden := range []string{"pending answer body for reviewers", "document-review-1", `"usage"`, `"sources"`} {
		if strings.Contains(messages.body, hidden) {
			t.Errorf("opener snapshot contains %s: %s", hidden, messages.body)
		}
	}
	listed := f.get(Prefix+"chat/approvals?course_id=course-1", opener)
	if listed.code != http.StatusOK || !strings.Contains(listed.body, `"approvals":[]`) || !strings.Contains(listed.body, `"next":null`) {
		t.Errorf("opener with no reviewer scope should see an empty list: %d %s", listed.code, listed.body)
	}

	// A post-review item is distinguishable without exposing the internal mode:
	// its already-published target message is posted, not pending.
	svc.mu.Lock()
	svc.reviewerSnapshot.Messages[1].Status = "posted"
	svc.mu.Unlock()
	postReview := f.get(Prefix+"chat/approvals/approval-1", reviewer)
	if postReview.code != http.StatusOK || !strings.Contains(postReview.body, `"status":"posted","body":"pending answer body for reviewers"`) ||
		strings.Contains(postReview.body, `"mode"`) {
		t.Errorf("post-review target/mode contract: %d %s", postReview.code, postReview.body)
	}
}

func TestChatApprovalAttachmentReviewerOnlyAndSafeDownload(t *testing.T) {
	svc := newFakeChatService()
	svc.owner, svc.reviewer = ken, yuki
	f, reviewer := newChatFixture(t, svc)
	opener := f.core.assert(t, claims(ken, "", nil))
	path := Prefix + "chat/approvals/approval-1/attachments/att-review"

	a := f.get(path, reviewer)
	if a.code != http.StatusOK || a.body != "review bytes" {
		t.Fatalf("reviewer attachment: %d %s", a.code, a.body)
	}
	for header, want := range map[string]string{
		"Content-Type": "text/plain", "Content-Length": "12", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
	} {
		if got := a.header.Get(header); got != want {
			t.Errorf("%s=%q, want %q", header, got, want)
		}
	}
	disposition := a.header.Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || strings.ContainsAny(disposition, "\r\n") || strings.Contains(disposition, "../") ||
		a.header.Get("X-Evil") != "" {
		t.Errorf("unsafe reviewer Content-Disposition/headers: %q %+v", disposition, a.header)
	}

	for _, tc := range []struct {
		name, path, token string
	}{
		{"not a reviewer", path, opener},
		{"cross approval", Prefix + "chat/approvals/approval-2/attachments/att-review", reviewer},
		{"cross attachment", Prefix + "chat/approvals/approval-1/attachments/att-1", reviewer},
		{"reviewer cannot use owner route", chatCoursePath("attachments/att-review"), reviewer},
		{"review attachment is not in owner lookup", chatCoursePath("attachments/att-review"), opener},
	} {
		t.Run(tc.name, func(t *testing.T) {
			denied := f.get(tc.path, tc.token)
			wantRefused(t, denied, http.StatusNotFound, CodeNotFound, "not_found")
			if strings.Contains(denied.body, "review bytes") {
				t.Errorf("denial leaked attachment bytes: %s", denied.body)
			}
		})
	}
	ownerStillWorks := f.get(chatCoursePath("attachments/att-1"), opener)
	if ownerStillWorks.code != http.StatusOK || ownerStillWorks.body != "note" {
		t.Errorf("ordinary owner attachment route changed: %d %s", ownerStillWorks.code, ownerStillWorks.body)
	}
	unauthenticated := f.get(path, "")
	wantRefused(t, unauthenticated, http.StatusUnauthorized, CodeUnauthenticated, "assertion_missing")
}

func TestChatReviewerConfirmationRequiredDoesNotExecuteOrBlockProposals(t *testing.T) {
	svc := newFakeChatService()
	svc.decisionErr = &chat.Error{Code: CodeFailedPrecondition, Reason: "reviewer_confirmation_required",
		Message: "this reviewer's decision requires a higher-level approval and was not executed"}
	svc.reviewErr = &chat.Error{Code: CodeFailedPrecondition, Reason: "reviewer_confirmation_required",
		Message: "this reviewer's review requires a higher-level approval and was not recorded"}
	f, token := newChatFixture(t, svc)
	decision := f.send(req{method: http.MethodPost, path: Prefix + "chat/approvals/approval-1/decision", token: token,
		body: strings.NewReader(`{"decision":"approve","idempotency_key":"reviewer-level-decision"}`), headers: []string{"Content-Type", "application/json"}})
	wantRefused(t, decision, http.StatusUnprocessableEntity, CodeFailedPrecondition, "reviewer_confirmation_required")
	review := f.send(req{method: http.MethodPost, path: Prefix + "chat/approvals/approval-1/review", token: token,
		body: strings.NewReader(`{"idempotency_key":"reviewer-level-review"}`), headers: []string{"Content-Type", "application/json"}})
	wantRefused(t, review, http.StatusUnprocessableEntity, CodeFailedPrecondition, "reviewer_confirmation_required")
	detail := f.get(Prefix+"chat/approvals/approval-1", token)
	var stillPending struct {
		Approval json.RawMessage `json:"approval"`
	}
	detail.decode(t, &stillPending)
	requireCanonicalChatApprovalJSON(t, stillPending.Approval, "pending")

	pending := "pending"
	svc.snap.Submission = &store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "approval-1", ReviewState: &pending}
	proposal := f.send(req{method: http.MethodPost, path: chatCoursePath("conversations"), token: token,
		body:    strings.NewReader(`{"agent_id":"agent-1","body":"still accepted","idempotency_key":"proposal-still-accepted"}`),
		headers: []string{"Content-Type", "application/json"}})
	if proposal.code != http.StatusAccepted || !strings.Contains(proposal.body, `"status":"proposed"`) {
		t.Errorf("reviewer level refusal changed proposal acceptance: %d %s", proposal.code, proposal.body)
	}
}

func TestChatAuthenticationOwnershipAndSafeErrors(t *testing.T) {
	svc := newFakeChatService()
	f, token := newChatFixture(t, svc)
	other := f.core.assert(t, claims(ken, "", nil))
	paths := []struct{ method, path, body string }{
		{http.MethodGet, chatCoursePath("chat/agents"), ""},
		{http.MethodGet, chatCoursePath("conversations"), ""},
		{http.MethodPost, chatCoursePath("conversations"), `{}`},
		{http.MethodGet, chatCoursePath("conversations/conv-1"), ""},
		{http.MethodGet, chatCoursePath("conversations/conv-1/messages"), ""},
		{http.MethodPost, chatCoursePath("conversations/conv-1/messages"), `{}`},
		{http.MethodPost, chatCoursePath("conversations/conv-1/withdraw"), `{"message_id":"msg-1"}`},
		{http.MethodPost, chatCoursePath("conversations/conv-1/close"), ""},
		{http.MethodGet, chatCoursePath("attachments/att-1"), ""},
		{http.MethodGet, chatCoursePath("conversations/conv-1/stream"), ""},
	}
	for _, tc := range paths {
		a := f.send(req{method: tc.method, path: tc.path, body: strings.NewReader(tc.body), headers: []string{"Content-Type", "application/json"}})
		wantRefused(t, a, http.StatusUnauthorized, CodeUnauthenticated, "assertion_missing")
	}
	for _, tc := range paths[3:] {
		a := f.send(req{method: tc.method, path: tc.path, token: other, body: strings.NewReader(tc.body), headers: []string{"Content-Type", "application/json"}})
		wantRefused(t, a, http.StatusNotFound, CodeNotFound, "not_found")
		if strings.Contains(a.body, "agent-1") || strings.Contains(a.body, "conv-1") {
			t.Errorf("unowned response reveals ids: %s", a.body)
		}
	}
	a := f.send(req{method: http.MethodPost, path: chatCoursePath("conversations"), token: token,
		body:    strings.NewReader(`{"agent_id":"agent-1","body":"q","idempotency_key":"k"}`),
		headers: []string{"Content-Type", "application/json", "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"}})
	wantRefused(t, a, http.StatusForbidden, CodeForbidden, ReasonCrossOrigin)

	svc.setCheckErr(errors.New("provider failed: sk-live-secret and private upstream text"))
	a = f.get(chatCoursePath("conversations/conv-1/stream"), token)
	wantRefused(t, a, http.StatusServiceUnavailable, CodeUnavailable, ReasonStoreUnavailable)
	if strings.Contains(a.body, "sk-live-secret") || strings.Contains(a.body, "private upstream") {
		t.Fatalf("provider error leaked: %s", a.body)
	}
}

func TestChatPaginationRefusals(t *testing.T) {
	f, token := newChatFixture(t, newFakeChatService())
	for _, query := range []string{"limit=0", "limit=201", "limit=01", "limit=nope"} {
		a := f.get(chatCoursePath("conversations?"+query), token)
		wantRefused(t, a, http.StatusBadRequest, CodeInvalidArgument, ReasonInvalidField)
	}
	for _, query := range []string{"before_seq=0", "before_seq=-1", "before_seq=01", "before_seq=nope"} {
		a := f.get(chatCoursePath("conversations/conv-1/messages?"+query), token)
		wantRefused(t, a, http.StatusBadRequest, CodeInvalidArgument, ReasonInvalidField)
	}
}

func TestChatRawAttachmentStageReplayLimitsAndDownload(t *testing.T) {
	svc := newFakeChatService()
	f, token := newChatFixture(t, svc)
	path := chatCoursePath("attachments?filename=notes.txt")
	send := func(body []byte, mediaType string, headers ...string) answer {
		h := []string{"Content-Type", mediaType, "Idempotency-Key", "upload-1"}
		h = append(h, headers...)
		return f.send(req{method: http.MethodPost, path: path, token: token, body: bytes.NewReader(body), headers: h})
	}
	first := send([]byte("note"), "text/plain; charset=utf-8")
	if first.code != http.StatusOK || !strings.Contains(first.body, `"attachment":{"id":"att-1","filename":"notes.txt","media_type":"text/plain","size":4`) {
		t.Fatalf("stage: %d %s", first.code, first.body)
	}
	again := send([]byte("note"), "text/plain")
	if again.code != http.StatusOK || svc.stageStores != 1 {
		t.Fatalf("idempotent replay: %d %s stores=%d", again.code, again.body, svc.stageStores)
	}
	conflict := send([]byte("different"), "text/plain")
	wantRefused(t, conflict, http.StatusConflict, CodeConflict, "idempotency_conflict")

	queryKey := f.send(req{method: http.MethodPost, path: chatCoursePath("attachments?key=upload-2"), token: token,
		body: strings.NewReader("abc"), headers: []string{"Content-Type", "text/plain", "X-Filename", "header.txt"}})
	if queryKey.code != http.StatusOK || !strings.Contains(queryKey.body, `"filename":"header.txt"`) {
		t.Fatalf("query key/header filename: %d %s", queryKey.code, queryKey.body)
	}
	exactText := f.send(req{method: http.MethodPost, path: path, token: token, body: io.LimitReader(&zeroReader{}, chatTextMaxBytes),
		headers: []string{"Content-Type", "text/plain", "Idempotency-Key", "exact-text"}})
	if exactText.code != http.StatusOK || !strings.Contains(exactText.body, `"size":32768`) {
		t.Fatalf("exact text limit: %d %s", exactText.code, exactText.body)
	}
	exactImage := f.send(req{method: http.MethodPost, path: path, token: token, body: io.LimitReader(&zeroReader{}, chatImageMaxBytes),
		headers: []string{"Content-Type", "image/png", "Idempotency-Key", "exact-image"}})
	if exactImage.code != http.StatusOK || !strings.Contains(exactImage.body, `"size":5242880`) {
		t.Fatalf("exact image limit: %d %s", exactImage.code, exactImage.body)
	}
	tooBig := send(bytes.Repeat([]byte{'x'}, chatTextMaxBytes+1), "text/plain")
	wantRefused(t, tooBig, http.StatusBadRequest, CodeInvalidArgument, ReasonBodyTooLarge)
	tooBigImage := f.send(req{method: http.MethodPost, path: path, token: token, body: io.LimitReader(&zeroReader{}, chatImageMaxBytes+1),
		headers: []string{"Content-Type", "image/png", "Idempotency-Key", "large-image"}})
	wantRefused(t, tooBigImage, http.StatusBadRequest, CodeInvalidArgument, ReasonBodyTooLarge)
	unsupported := send([]byte(`{"body":"not JSON decoded"}`), "application/json")
	wantRefused(t, unsupported, http.StatusBadRequest, CodeInvalidArgument, ReasonInvalidField)
	missingKey := f.send(req{method: http.MethodPost, path: path, token: token, body: strings.NewReader("x"), headers: []string{"Content-Type", "text/plain"}})
	wantRefused(t, missingKey, http.StatusBadRequest, CodeInvalidArgument, ReasonInvalidField)
	missingName := f.send(req{method: http.MethodPost, path: chatCoursePath("attachments"), token: token, body: strings.NewReader("x"),
		headers: []string{"Content-Type", "text/plain", "Idempotency-Key", "upload-3"}})
	wantRefused(t, missingName, http.StatusBadRequest, CodeInvalidArgument, ReasonMissingField)

	svc.mu.Lock()
	svc.attachment.Filename = "../../evil\"\r\nSet-Cookie: secret.txt"
	svc.attachment.MediaType = "text/plain"
	svc.attachment.Bytes = []byte("note")
	svc.attachment.Size = 4
	svc.mu.Unlock()
	download := f.get(chatCoursePath("attachments/att-1"), token)
	if download.code != http.StatusOK || download.body != "note" || download.header.Get("Content-Type") != "text/plain" || download.header.Get("Content-Length") != "4" {
		t.Fatalf("download: %d %+v %q", download.code, download.header, download.body)
	}
	disposition := download.header.Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || strings.ContainsAny(disposition, "\r\n") || download.header.Get("Set-Cookie") != "" {
		t.Errorf("unsafe Content-Disposition or injected header: %q %+v", disposition, download.header)
	}
}

type zeroReader struct{}

func (*zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestChatNilServiceIsUnavailable(t *testing.T) {
	f, token := newChatFixture(t, nil)
	a := f.get(chatCoursePath("chat/agents"), token)
	wantRefused(t, a, http.StatusServiceUnavailable, CodeUnavailable, ReasonStoreUnavailable)
}
