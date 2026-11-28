// Package chat implements AR-owned browser conversations. Core is consulted
// only for authorization and, by the TS Runner, course tools; no Core
// conversation, message, attachment, or draft APIs are used here.
package chat

import "github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"

// Browser DTO aliases keep the canonical JSON in one place (internal/store).
type Conversation = store.ChatConversation
type Message = store.ChatMessage
type Snapshot = store.ChatSnapshot
type Draft = store.ChatDraft
type Step = store.ChatStep
type Source = store.ChatSource
type Usage = store.ChatUsage
type Attachment = store.ChatAttachment
type Generation = store.ChatGeneration
type Result = store.ChatResult
type Submission = store.ChatSubmission
type Approval = store.ChatApproval

type ApprovalDetail struct {
	Approval Approval `json:"approval"`
	Snapshot Snapshot `json:"snapshot"`
}

const (
	Queued               = store.ChatQueued
	Answering            = store.ChatAnswering
	Answered             = store.ChatAnswered
	ReplyPendingApproval = store.ChatReplyPendingApproval
	Failed               = store.ChatFailed
	Closed               = store.ChatClosed

	ChatRoleUser      = store.ChatRoleUser
	ChatRoleAssistant = store.ChatRoleAssistant
	ChatPosted        = store.ChatPosted
	ChatPending       = store.ChatPending
	ChatRetracted     = store.ChatRetracted
	ChatMsgFailed     = store.ChatMsgFailed
)
