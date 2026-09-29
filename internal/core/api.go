package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Client is the typed face of a Caller: the calls the runtime makes itself
// (§2.3). Reads that come back with any status but executed are
// *EnvelopeError; writes return the envelope as it came.
type Client struct {
	c Caller
}

// NewClient wraps c.
func NewClient(c Caller) *Client { return &Client{c: c} }

// Caller is the Caller underneath.
func (c *Client) Caller() Caller { return c.c }

// Me is me_get: it checks the token and names the agent's actor.
func (c *Client) Me(ctx context.Context) (*Actor, error) {
	var a Actor
	return &a, c.read(ctx, "me_get", struct{}{}, &a)
}

// Memberships is me_memberships: every seat.
func (c *Client) Memberships(ctx context.Context) ([]Membership, error) {
	var r struct {
		Memberships []Membership `json:"memberships"`
	}
	err := c.read(ctx, "me_memberships", struct{}{}, &r)
	return r.Memberships, err
}

// Inbox is conversation_inbox for one course, longest waiting first. limit
// 0 is Core's default (20); at most 100. wait above zero is wait_s, in
// whole seconds up to MaxWait, which only a Core whose catalogue offers it
// takes (Catalogue.MaxWait): a call that finds no question waits up to that
// long for one, and answers as soon as one comes.
func (c *Client) Inbox(ctx context.Context, courseID string, limit int, wait time.Duration) ([]Conversation, error) {
	var r Inbox
	ctx, secs := waiting(ctx, wait)
	err := c.read(ctx, "conversation_inbox", struct {
		CourseID string `json:"course_id"`
		Limit    int    `json:"limit,omitempty"`
		WaitS    int    `json:"wait_s,omitempty"`
	}{courseID, limit, secs}, &r)
	return r.Conversations, err
}

// waiting is ctx marked for a call that waits up to wait (WithWait), and
// wait as wait_s; ctx as it is and 0 for no wait.
func waiting(ctx context.Context, wait time.Duration) (context.Context, int) {
	secs := waitSeconds(wait)
	if secs == 0 {
		return ctx, 0
	}
	return WithWait(ctx, time.Duration(secs)*time.Second), secs
}

// MessagesQuery pages conversation_messages. With neither AfterSeq nor
// BeforeSeq, it gives the newest Limit messages, oldest first.
type MessagesQuery struct {
	AfterSeq  *int64
	BeforeSeq *int64
	// Limit 0 is Core's default (50); at most 200.
	Limit int
}

// Messages is conversation_messages.
func (c *Client) Messages(ctx context.Context, courseID, conversationID string, q MessagesQuery) (*Messages, error) {
	var r Messages
	err := c.read(ctx, "conversation_messages", struct {
		CourseID       string `json:"course_id"`
		ConversationID string `json:"conversation_id"`
		AfterSeq       *int64 `json:"after_seq,omitempty"`
		BeforeSeq      *int64 `json:"before_seq,omitempty"`
		Limit          int    `json:"limit,omitempty"`
	}{courseID, conversationID, q.AfterSeq, q.BeforeSeq, q.Limit}, &r)
	return &r, err
}

// Conversation is conversation_get.
func (c *Client) Conversation(ctx context.Context, courseID, conversationID string) (*Conversation, error) {
	var r Conversation
	err := c.read(ctx, "conversation_get", struct {
		CourseID       string `json:"course_id"`
		ConversationID string `json:"conversation_id"`
	}{courseID, conversationID}, &r)
	return &r, err
}

// Events is event_list from sinceSeq (0 for the start). limit 0 is Core's
// default (100); at most 500. wait above zero is wait_s, as for Inbox: a
// call that finds no event waits up to that long for one.
func (c *Client) Events(ctx context.Context, courseID string, sinceSeq int64, limit int, wait time.Duration) (*Events, error) {
	var r Events
	ctx, secs := waiting(ctx, wait)
	err := c.read(ctx, "event_list", struct {
		CourseID string `json:"course_id"`
		SinceSeq int64  `json:"since_seq"`
		Limit    int    `json:"limit,omitempty"`
		WaitS    int    `json:"wait_s,omitempty"`
	}{courseID, sinceSeq, limit, secs}, &r)
	return &r, err
}

// ActionsMine is action_list_mine after the action id after ("" for the
// start), oldest first. limit 0 is Core's default (50); at most 200.
func (c *Client) ActionsMine(ctx context.Context, courseID, after string, excludeTypes []string, limit int) (*Actions, error) {
	var r Actions
	args := struct {
		CourseID     string   `json:"course_id"`
		ExcludeTypes []string `json:"exclude_types,omitempty"`
		After        string   `json:"after,omitempty"`
		Limit        int      `json:"limit,omitempty"`
	}{courseID, excludeTypes, after, limit}
	err := c.read(ctx, "action_list_mine", args, &r)
	return &r, err
}

// Credentials is credential_list: the caller's own credentials, newest
// first, revoked ones included; never a secret. Core lets any active actor
// list its own (a self gate), an agent among them.
func (c *Client) Credentials(ctx context.Context) ([]Credential, error) {
	var r struct {
		Credentials []Credential `json:"credentials"`
	}
	err := c.read(ctx, "credential_list", struct{}{}, &r)
	return r.Credentials, err
}

// RevokeCredential is credential_revoke of one of the caller's own
// credentials, under RevokeKey(id), and returns the envelope as it came:
// executed when it was revoked (or a replay of that), failed not_found when
// Core knows no live credential of the caller's by that id (someone else's,
// revoked already, or none), denied when the caller may not act (a
// suspended actor). An agent's token can revoke itself: the call is
// answered, and its next use is a 401.
func (c *Client) RevokeCredential(ctx context.Context, id string) (*Envelope, error) {
	raw, err := json.Marshal(struct {
		CredentialID   string `json:"credential_id"`
		IdempotencyKey string `json:"idempotency_key"`
	}{id, RevokeKey(id)})
	if err != nil {
		return nil, fmt.Errorf("core: credential_revoke: %w", err)
	}
	return c.c.Call(ctx, "credential_revoke", raw)
}

// ToolSiteChat is me.site_chat over MCP: the brain running an agent
// declaring, with the agent's token, that it takes conversations in the
// site. Core takes them for the agent only after that, and only while the
// token that declared it is live.
const ToolSiteChat = "me_site_chat"

// SiteChat is me.site_chat {on}, and returns the envelope as it came. A
// write takes an idempotency key, key, which is left out when it is "":
// give each declaration a key of its own, since a replay does nothing, and
// a token's revocation turns the declaration off.
func (c *Client) SiteChat(ctx context.Context, on bool, key string) (*Envelope, error) {
	raw, err := json.Marshal(struct {
		On             bool   `json:"on"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}{on, key})
	if err != nil {
		return nil, fmt.Errorf("core: %s: %w", ToolSiteChat, err)
	}
	return c.c.Call(ctx, ToolSiteChat, raw)
}

// ToolDraft is conversation.draft over MCP: the draft of an answer being
// written, which whoever reads the conversation sees until the answer takes
// its place (Catalogue.Drafts).
const ToolDraft = "conversation_draft"

// The kinds of a draft's steps, and their states.
const (
	StepThinking          = "thinking"
	StepReadingDocument   = "reading_document"
	StepListingDocuments  = "listing_documents"
	StepReadingAssignment = "reading_assignment"
	StepReadingSubmission = "reading_submission"
	StepSearchingMemory   = "searching_memory"
	StepWriting           = "writing"
	StepTool              = "tool"

	StepRunning = "running"
	StepDone    = "done"
)

// DraftStep is one thing the runtime does, or did, towards an answer: its
// kind, its state, and what it is about (a document's title), plain text.
type DraftStep struct {
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	State  string `json:"state"`
}

// DraftArgs are conversation_draft's arguments. Text, when set, is the
// whole answer so far; Steps, when any, every step so far: each replaces
// what Core keeps, and one left out keeps it. Version rises with each
// write of an attempt: Core passes over one not newer than it keeps. Done
// ends the attempt, and Core deletes its draft.
type DraftArgs struct {
	CourseID       string      `json:"course_id"`
	ConversationID string      `json:"conversation_id"`
	Attempt        string      `json:"attempt"`
	Version        int64       `json:"version"`
	Text           *string     `json:"text,omitempty"`
	Steps          []DraftStep `json:"steps,omitempty"`
	Done           bool        `json:"done,omitempty"`
}

// Draft is conversation_draft, an ephemeral write: no idempotency key, and
// nothing recorded. It returns the envelope as it came; it is best effort
// (WithBestEffort), so no layer underneath sends it again or holds it for
// the agent's rate limit, which Core does not count it against.
func (c *Client) Draft(ctx context.Context, args DraftArgs) (*Envelope, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("core: %s: %w", ToolDraft, err)
	}
	return c.c.Call(WithBestEffort(ctx), ToolDraft, raw)
}

// Send sends a write's exact bytes and returns the envelope as it came:
// the bytes written ahead, sent again after a timeout (§2.2).
func (c *Client) Send(ctx context.Context, tool string, args []byte) (*Envelope, error) {
	return c.c.Call(ctx, tool, args)
}

// Call makes any call and returns the envelope as it came: the model's
// read tools go through here.
func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	return c.c.Call(ctx, tool, args)
}

func (c *Client) read(ctx context.Context, tool string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("core: %s: %w", tool, err)
	}
	env, err := c.c.Call(ctx, tool, raw)
	if err != nil {
		return err
	}
	if env.Status != StatusExecuted {
		return &EnvelopeError{Tool: tool, Envelope: env}
	}
	if err := env.Decode(out); err != nil {
		return &ProtocolError{Code: 0, Message: fmt.Sprintf("%s: the result does not decode: %v", tool, err)}
	}
	return nil
}
