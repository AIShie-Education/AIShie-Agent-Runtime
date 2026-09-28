package core

import (
	"context"
	"encoding/json"
	"fmt"
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
// 0 is Core's default (20); at most 100.
func (c *Client) Inbox(ctx context.Context, courseID string, limit int) ([]Conversation, error) {
	var r Inbox
	err := c.read(ctx, "conversation_inbox", struct {
		CourseID string `json:"course_id"`
		Limit    int    `json:"limit,omitempty"`
	}{courseID, limit}, &r)
	return r.Conversations, err
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
// default (100); at most 500.
func (c *Client) Events(ctx context.Context, courseID string, sinceSeq int64, limit int) (*Events, error) {
	var r Events
	err := c.read(ctx, "event_list", struct {
		CourseID string `json:"course_id"`
		SinceSeq int64  `json:"since_seq"`
		Limit    int    `json:"limit,omitempty"`
	}{courseID, sinceSeq, limit}, &r)
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
