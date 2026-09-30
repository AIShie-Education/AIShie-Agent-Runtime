package core

import (
	"context"
	"encoding/json"
	"errors"
)

// A message of a conversation may carry files (Core's conversation
// attachments; handout §2.9): conversation_messages lists each message's,
// in order, and conversation_attachment gives one's download URL, good for
// about fifteen minutes, to whoever may read the conversation. A retracted
// message's files are withheld as its text is: not listed, and
// conversation_attachment answers not_found, reason retracted. Both are the
// runtime's calls, made with the agent's own token; no model is offered
// either (toolset.BuiltinDeny), and no model sees a URL.

// Attachment is a file a message carries, as conversation_messages lists
// it: never where it is kept, nor a URL.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	// Checksum is sha256:<hex> where Core's store worked it out from the
	// bytes, etag:<value> where all it has is an object store's tag, and
	// nil where it has neither.
	Checksum  *string `json:"checksum,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// ToolAttachment is conversation.attachment over MCP.
const ToolAttachment = "conversation_attachment"

// ReasonRetracted is error.details.reason of conversation_attachment's
// not_found for a file whose message was retracted.
const ReasonRetracted = "retracted"

// AttachmentFile is conversation_attachment's result: the file, the
// message that carries it, and a URL that serves it, GET as it is with no
// Authorization header before ExpiresAt. The URL is a credential: it goes
// to no model and into no log.
type AttachmentFile struct {
	Attachment
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	MessageSeq     int64  `json:"message_seq"`
	AuthorMemberID string `json:"author_member_id"`
	DownloadURL    string `json:"download_url"`
	ExpiresAt      string `json:"expires_at"`
}

// Attachment is conversation_attachment: the file attachmentID of the
// course, which Core gives only to whoever may read its conversation (any
// other caller is told not_found, as for a file that does not exist); one
// whose message was retracted is not_found with reason retracted
// (IsRetracted).
func (c *Client) Attachment(ctx context.Context, courseID, attachmentID string) (*AttachmentFile, error) {
	var r AttachmentFile
	err := c.read(ctx, ToolAttachment, struct {
		CourseID     string `json:"course_id"`
		AttachmentID string `json:"attachment_id"`
	}{courseID, attachmentID}, &r)
	return &r, err
}

// IsRetracted reports whether err is conversation_attachment's refusal of
// a file whose message was retracted: its files are gone, with its text.
func IsRetracted(err error) bool {
	var ee *EnvelopeError
	return errors.As(err, &ee) && ee.Envelope.Code() == CodeNotFound && ee.Envelope.Reason() == ReasonRetracted
}

// IsMissing reports whether err is a read Core refused as not_found, for
// any reason: a retracted message's file among them (IsRetracted).
func IsMissing(err error) bool {
	var ee *EnvelopeError
	return errors.As(err, &ee) && ee.Envelope.Code() == CodeNotFound
}

// PostedAttachments are the files a conversation.message_posted event says
// its message carries, by id, name, type and size (never a URL); none for
// a message that carries none, or a Core from before attachments.
func PostedAttachments(e Event) []Attachment {
	if e.Type != EventConversationMessagePosted || len(e.Payload) == 0 {
		return nil
	}
	var p struct {
		Attachments []Attachment `json:"attachments"`
	}
	if json.Unmarshal(e.Payload, &p) != nil {
		return nil
	}
	return p.Attachments
}
