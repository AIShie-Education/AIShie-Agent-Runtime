package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The transcription service's side of Core (AIShie-Core #43, its
// docs/schema.md "text versions"): the site's transcriber claims the
// versions of documents whose text is waiting (document_text.queue),
// fetches their files (a fresh URL with document_text.file), holds its
// claims while it works (document_text.renew), and writes back the text,
// or why there is none (document_text.complete). It calls them over REST
// alone, with the service's own credential (aissvc_…), which works at
// these four routes and nowhere else; Core serves none of them over MCP.

// The service's tools, as the catalogue names them.
const (
	ToolTextQueue    = "document_text_queue"
	ToolTextFile     = "document_text_file"
	ToolTextRenew    = "document_text_renew"
	ToolTextComplete = "document_text_complete"
)

// Reasons Core gives in error.details.reason to the service (the queue
// protocol, §4.2), and around a text version.
const (
	// ReasonLeaseLost: the claim does not hold (lapsed and claimed again,
	// sent back to the queue by a retranscription, or the credential
	// that made it revoked). Stop, and drop the work.
	ReasonLeaseLost = "lease_lost"
	// ReasonEditedByStaff: staff wrote the text meanwhile, which no
	// transcription writes over. Stop, and drop the work.
	ReasonEditedByStaff = "edited_by_staff"
	// ReasonCourseArchived: the course is archived; the version waits
	// for it to open again.
	ReasonCourseArchived = "course_archived"
	// ReasonDocumentArchived: the document is archived.
	ReasonDocumentArchived = "document_archived"
	// ReasonServiceOnly: the route is the service's, and the credential
	// is not; ReasonNotForServices: the other way round.
	ReasonServiceOnly    = "service_only"
	ReasonNotForServices = "not_for_services"
	// ReasonTextTooLong: a body past the 2 MiB Core keeps.
	ReasonTextTooLong = "text_too_long"
	// ReasonAttemptsExhausted: Core's own, for a version whose claims
	// lapsed five times.
	ReasonAttemptsExhausted = "attempts_exhausted"
)

// ServiceTokenPrefix begins the service's tokens, as Core makes them:
// aissvc_, a public prefix of 12 characters, _ and the secret.
const ServiceTokenPrefix = "aissvc_"

// MaxTextBytes is the most text Core keeps of a version: 2 MiB of
// Markdown.
const MaxTextBytes = 2 << 20

// Bounds of the queue protocol.
const (
	// MaxClaims is the most one call to the queue claims.
	MaxClaims = 10
	// MinLease and MaxLease bound a claim's lease, and DefaultLease is
	// Core's when none is asked for.
	MinLease     = 60 * time.Second
	MaxLease     = time.Hour
	DefaultLease = 10 * time.Minute
)

// ClaimedText is one version the service claimed: whose it is, the claim
// (its lease, until when it holds, how many claims the version has had,
// this one's included), whether it was queued by the backfill rather than
// as its file was added, the file as Core has it, and a short-lived URL
// that serves it (no credential: it is signed).
type ClaimedText struct {
	VersionID         string    `json:"version_id"`
	DocumentID        string    `json:"document_id"`
	CourseID          string    `json:"course_id"`
	LeaseID           string    `json:"lease_id"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
	Attempt           int       `json:"attempt"`
	Backfill          bool      `json:"backfill"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          string    `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
}

// TextFile is document_text.file's result: the claimed version's file
// again, at a fresh URL.
type TextFile struct {
	VersionID         string    `json:"version_id"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          string    `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
}

// Where a version's text stands (TextView.Status), and whose it is
// (TextView.Source).
const (
	TextPending = "pending"
	TextWorking = "working"
	TextDone    = "done"
	TextFailed  = "failed"
	TextSkipped = "skipped"

	SourceAI    = "ai"
	SourceStaff = "staff"
)

// Completion is what the service writes back for a version: done, with
// the text, the pages it found and the model that made it, as staff are
// to be shown it; or failed or skipped, with why.
type Completion struct {
	Status string
	Body   string
	Pages  int
	Model  string
	Reason string
}

// ServiceError is a call of the service's that Core refused, or never
// attempted: its tool, the envelope's status, and the error Core gave.
// Code and Reason say what to do (ReasonLeaseLost, …); a 401 is
// ErrUnauthenticated, never this.
type ServiceError struct {
	Tool    string
	Status  Status
	Code    string
	Reason  string
	Message string
}

func (e *ServiceError) Error() string {
	s := fmt.Sprintf("core: %s: %s", e.Tool, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Reason != "" {
		s += " (" + e.Reason + ")"
	}
	return s
}

// IsReason reports whether err is Core's refusal for reason.
func IsReason(err error, reason string) bool {
	var se *ServiceError
	return errors.As(err, &se) && se.Reason == reason
}

// IsNotFound reports whether err is Core's not_found: the version is not
// there (purged), or is none the service may hold.
func IsNotFound(err error) bool {
	var se *ServiceError
	return errors.As(err, &se) && se.Code == CodeNotFound
}

// Service is the transcription service's typed client: its four calls,
// over a Caller that carries the service's credential (RESTCaller: the
// service's tools are REST's alone).
type Service struct {
	c Caller
}

// NewService is the service's client over c.
func NewService(c Caller) *Service { return &Service{c: c} }

// HasService reports whether cat offers the transcription service's
// tools: a Core since #43.
func HasService(cat *Catalogue) bool {
	if cat == nil {
		return false
	}
	for _, name := range []string{ToolTextQueue, ToolTextFile, ToolTextRenew, ToolTextComplete} {
		if _, ok := cat.Tool(name); !ok {
			return false
		}
	}
	return true
}

// Queue claims up to n versions waiting (1 to MaxClaims), each for
// lease, rounded to whole seconds within MinLease and MaxLease. wait above
// zero is wait_s, whole seconds up to MaxWait: a call that finds nothing
// waiting waits that long for a version to be queued, and answers as soon
// as one is, or with none.
func (s *Service) Queue(ctx context.Context, n int, lease, wait time.Duration) ([]ClaimedText, error) {
	ctx, secs := waiting(ctx, wait)
	var r struct {
		Claimed []ClaimedText `json:"claimed"`
	}
	err := s.call(ctx, ToolTextQueue, struct {
		Max    int `json:"max"`
		LeaseS int `json:"lease_s"`
		WaitS  int `json:"wait_s,omitempty"`
	}{min(max(n, 1), MaxClaims), leaseSeconds(lease), secs}, &r)
	return r.Claimed, err
}

// File is the claimed version's file again, at a fresh URL: while the
// claim holds, and ReasonLeaseLost otherwise.
func (s *Service) File(ctx context.Context, versionID, leaseID string) (*TextFile, error) {
	var f TextFile
	err := s.call(ctx, ToolTextFile, struct {
		VersionID string `json:"version_id"`
		LeaseID   string `json:"lease_id"`
	}{versionID, leaseID}, &f)
	return &f, err
}

// Renew holds the claim for lease from now, and says until when:
// ReasonLeaseLost or ReasonEditedByStaff when the work is to stop,
// ReasonCourseArchived when the course was archived.
func (s *Service) Renew(ctx context.Context, versionID, leaseID string, lease time.Duration) (time.Time, error) {
	var r struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	err := s.call(ctx, ToolTextRenew, struct {
		VersionID string `json:"version_id"`
		LeaseID   string `json:"lease_id"`
		LeaseS    int    `json:"lease_s"`
	}{versionID, leaseID, leaseSeconds(lease)}, &r)
	return r.LeaseExpiresAt, err
}

// CompleteKey is the idempotency key of the completion of a claim: the
// same for every try of it, so that a completion sent again after a
// network error is Core's replay of the first.
func CompleteKey(versionID, leaseID string) string {
	return "complete:" + versionID + ":" + leaseID
}

// Complete writes back what became of the claimed version, under
// CompleteKey, and returns the text's revision. Core refuses it with
// ReasonEditedByStaff, ReasonLeaseLost, ReasonDocumentArchived or
// ReasonCourseArchived, and a version purged meanwhile is not found: in
// each, nothing was written, and the work is dropped.
func (s *Service) Complete(ctx context.Context, versionID, leaseID string, c Completion) (int, error) {
	args := map[string]any{"version_id": versionID, "lease_id": leaseID, "status": c.Status,
		idempotencyKeyArg: CompleteKey(versionID, leaseID)}
	if c.Status == TextDone {
		args["body"], args["pages"], args["model"] = c.Body, c.Pages, c.Model
	} else {
		args["reason"] = c.Reason
	}
	var r struct {
		Revision int `json:"revision"`
	}
	err := s.call(ctx, ToolTextComplete, args, &r)
	return r.Revision, err
}

// leaseSeconds is d as lease_s: whole seconds within MinLease and MaxLease,
// DefaultLease for none.
func leaseSeconds(d time.Duration) int {
	if d <= 0 {
		d = DefaultLease
	}
	return int(min(max(d, MinLease), MaxLease) / time.Second)
}

// call makes one of the service's calls: executed decodes into out, and
// anything else is a *ServiceError (a 401, ErrUnauthenticated; a 429, a
// *RateLimitedError; a 5xx or the network, a *TransientError).
func (s *Service) call(ctx context.Context, tool string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("core: %s: %w", tool, err)
	}
	env, err := s.c.Call(ctx, tool, raw)
	if err != nil {
		return err
	}
	if env.Status != StatusExecuted {
		se := &ServiceError{Tool: tool, Status: env.Status, Code: env.Code(), Reason: env.Reason()}
		if env.Error != nil {
			se.Message = env.Error.Message
		}
		return se
	}
	if err := env.Decode(out); err != nil {
		return &ProtocolError{Message: fmt.Sprintf("%s: the result does not decode: %v", tool, err)}
	}
	return nil
}

// TextView is a version's text version as Core shows it beside the
// version (document_get's version.text, document_text's text): where it
// stands, whose it is, the model that made it, its pages, why there is
// none, who last wrote it, its revision, its length, and, when it is done
// and short, its body; in document_text, the body is the part read.
type TextView struct {
	Status           string     `json:"status"`
	Source           string     `json:"source,omitempty"`
	Model            string     `json:"model,omitempty"`
	Pages            int        `json:"pages,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	ProducedAt       *time.Time `json:"produced_at,omitempty"`
	EditedByMemberID string     `json:"edited_by_member_id,omitempty"`
	EditedByName     string     `json:"edited_by_name,omitempty"`
	EditedAt         *time.Time `json:"edited_at,omitempty"`
	Revision         int        `json:"revision"`
	UpdatedAt        time.Time  `json:"updated_at"`
	Bytes            int        `json:"bytes"`
	Body             *string    `json:"body,omitempty"`
}

// TextPart is document_text's result: one part of a version's text.
type TextPart struct {
	DocumentID string   `json:"document_id"`
	VersionID  string   `json:"version_id"`
	Seq        int      `json:"seq"`
	Published  bool     `json:"published"`
	Text       TextView `json:"text"`
	Part       int      `json:"part"`
	Parts      int      `json:"parts"`
}

// ToolText is document.text over MCP and REST: a version's text, a part
// at a time.
const ToolText = "document_text"

// TextPart reads part (from 1) of the text of the document's version,
// with the caller's own token: the version's access is the text's.
func (c *Client) TextPart(ctx context.Context, courseID, documentID, versionID string, part int) (*TextPart, error) {
	var r TextPart
	err := c.read(ctx, ToolText, struct {
		CourseID   string `json:"course_id"`
		DocumentID string `json:"document_id"`
		VersionID  string `json:"version_id,omitempty"`
		Part       int    `json:"part,omitempty"`
	}{courseID, documentID, versionID, part}, &r)
	return &r, err
}

// Text events, which Core files in the course's feed when a version's
// text becomes done (by the service or staff) or is discarded by a
// retranscription: its payload names the version (version_id) and its
// revision, never the text.
var TextEvents = []string{
	"document.text_updated", "document.rubric_text_updated", "document.draft_text_updated",
	"document.text_updated_unreleased", "document.rubric_text_updated_unreleased", "document.draft_text_updated_unreleased",
}

// IsTextEvent reports whether an event's type is one of TextEvents.
func IsTextEvent(typ string) bool { return slices.Contains(TextEvents, typ) }
