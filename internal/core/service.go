package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The transcription service's side of Core (AIShie-Core #43, its
// docs/schema.md "text versions"): the site's transcriber claims the files
// of documents' versions whose text is waiting (document_text.queue; a
// version's each file on its own since AIShie-Core #49, a Core before it
// claiming versions of one file), fetches them (a fresh URL with
// document_text.file), holds its claims while it works
// (document_text.renew), and writes back the text, or why there is none
// (document_text.complete), every call naming the file where the claim
// did. It calls them over REST alone, with the service's own credential
// (aissvc_…), which works at these four routes and nowhere else; Core
// serves none of them over MCP.

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

// ClaimedText is one file the service claimed: whose it is (its version,
// and the file by its id, place and name, which a Core before #49, whose
// versions had one file, does not give), the claim (its lease, until when
// it holds, how many claims the file has had, this one's included),
// whether it was queued by the backfill rather than as its file was
// added, the file as Core has it, and a short-lived URL that serves it (no
// credential: it is signed).
type ClaimedText struct {
	VersionID         string    `json:"version_id"`
	FileID            string    `json:"file_id,omitempty"`
	Position          int       `json:"position,omitempty"`
	Filename          string    `json:"filename,omitempty"`
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

// TextFile is document_text.file's result: the claimed file again, at a
// fresh URL.
type TextFile struct {
	VersionID         string    `json:"version_id"`
	FileID            string    `json:"file_id,omitempty"`
	Position          int       `json:"position,omitempty"`
	Filename          string    `json:"filename,omitempty"`
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

// Queue claims up to n files waiting (1 to MaxClaims), each for
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

// Claim names one claim in the calls that follow it: the version, the
// file (none from a Core before #49, and none is then sent) and the
// lease.
type Claim struct {
	VersionID, FileID, LeaseID string
}

// ClaimOf is c's claim.
func ClaimOf(c ClaimedText) Claim {
	return Claim{VersionID: c.VersionID, FileID: c.FileID, LeaseID: c.LeaseID}
}

// claimArgs are the arguments that name cl: file_id only where the claim
// named a file, which a Core before #49, whose schema takes no file_id,
// never does.
func claimArgs(cl Claim) map[string]any {
	args := map[string]any{"version_id": cl.VersionID, "lease_id": cl.LeaseID}
	if cl.FileID != "" {
		args["file_id"] = cl.FileID
	}
	return args
}

// File is the claimed file again, at a fresh URL: while the claim holds,
// and ReasonLeaseLost otherwise.
func (s *Service) File(ctx context.Context, cl Claim) (*TextFile, error) {
	var f TextFile
	err := s.call(ctx, ToolTextFile, claimArgs(cl), &f)
	return &f, err
}

// Renew holds the claim for lease from now, and says until when:
// ReasonLeaseLost or ReasonEditedByStaff when the work is to stop,
// ReasonCourseArchived when the course was archived.
func (s *Service) Renew(ctx context.Context, cl Claim, lease time.Duration) (time.Time, error) {
	var r struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	args := claimArgs(cl)
	args["lease_s"] = leaseSeconds(lease)
	err := s.call(ctx, ToolTextRenew, args, &r)
	return r.LeaseExpiresAt, err
}

// CompleteKey is the idempotency key of the completion of a claim: the
// same for every try of it, so that a completion sent again after a
// network error is Core's replay of the first. It names the file where
// the claim does (complete:{file_id}:{lease_id}, as Core suggests), and
// the version otherwise, as before.
func CompleteKey(cl Claim) string {
	if cl.FileID != "" {
		return "complete:" + cl.FileID + ":" + cl.LeaseID
	}
	return "complete:" + cl.VersionID + ":" + cl.LeaseID
}

// Complete writes back what became of the claimed file, under
// CompleteKey, and returns the text's revision. Core refuses it with
// ReasonEditedByStaff, ReasonLeaseLost, ReasonDocumentArchived or
// ReasonCourseArchived, and a version purged meanwhile is not found: in
// each, nothing was written, and the work is dropped.
func (s *Service) Complete(ctx context.Context, cl Claim, c Completion) (int, error) {
	args := claimArgs(cl)
	args["status"], args[idempotencyKeyArg] = c.Status, CompleteKey(cl)
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

// call makes one of the service's calls (serviceCall).
func (s *Service) call(ctx context.Context, tool string, args, out any) error {
	return serviceCall(ctx, s.c, tool, args, out)
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

// TextPart is document_text's result: one part of the text of a file of a
// version (the file by its id, place and name, from a Core since #49).
type TextPart struct {
	DocumentID string   `json:"document_id"`
	VersionID  string   `json:"version_id"`
	Seq        int      `json:"seq"`
	Published  bool     `json:"published"`
	FileID     string   `json:"file_id,omitempty"`
	Position   int      `json:"position,omitempty"`
	Filename   string   `json:"filename,omitempty"`
	Text       TextView `json:"text"`
	Part       int      `json:"part"`
	Parts      int      `json:"parts"`
}

// ToolText is document.text over MCP and REST: a version's text, a part
// at a time.
const ToolText = "document_text"

// TextPart reads part (from 1) of the text of the file fileID of the
// document's version (its first, as before, for ""; a Core before #49
// takes no file_id, and none is sent), with the caller's own token: the
// version's access is the text's.
func (c *Client) TextPart(ctx context.Context, courseID, documentID, versionID, fileID string, part int) (*TextPart, error) {
	var r TextPart
	err := c.read(ctx, ToolText, struct {
		CourseID   string `json:"course_id"`
		DocumentID string `json:"document_id"`
		VersionID  string `json:"version_id,omitempty"`
		FileID     string `json:"file_id,omitempty"`
		Part       int    `json:"part,omitempty"`
	}{courseID, documentID, versionID, fileID, part}, &r)
	return &r, err
}

// ToolDocumentFile is document.file over MCP and REST (AIShie-Core #49):
// one file of a version, with a fresh URL.
const ToolDocumentFile = "document_file"

// DocumentFile is document_file's result: a file of a version, its text
// version without its body, and a short-lived URL that serves it (a
// credential for the file: never a model's).
type DocumentFile struct {
	ID          string    `json:"id"`
	Position    int       `json:"position"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	ByteSize    int64     `json:"byte_size"`
	Checksum    string    `json:"checksum,omitempty"`
	Text        *TextView `json:"text,omitempty"`
	DocumentID  string    `json:"document_id"`
	VersionID   string    `json:"version_id"`
	Seq         int       `json:"seq"`
	Published   bool      `json:"published"`
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// DocumentFile reads the file fileID of the document, with a fresh URL,
// with the caller's own token: whoever may read its version may read it,
// and anyone else is told it is not there.
func (c *Client) DocumentFile(ctx context.Context, courseID, documentID, fileID string) (*DocumentFile, error) {
	var r DocumentFile
	err := c.read(ctx, ToolDocumentFile, struct {
		CourseID   string `json:"course_id"`
		DocumentID string `json:"document_id"`
		FileID     string `json:"file_id"`
	}{courseID, documentID, fileID}, &r)
	return &r, err
}

// Text events, which Core files in the course's feed when a file's text
// becomes done (by the service or staff) or is discarded by a
// retranscription: its payload names the version (version_id), the file
// (file_id, from a Core since #49) and its revision, never the text.
var TextEvents = []string{
	"document.text_updated", "document.rubric_text_updated", "document.draft_text_updated",
	"document.text_updated_unreleased", "document.rubric_text_updated_unreleased", "document.draft_text_updated_unreleased",
}

// IsTextEvent reports whether an event's type is one of TextEvents.
func IsTextEvent(typ string) bool { return slices.Contains(TextEvents, typ) }

// TextEventOf is what a text event's payload names: the version, and the
// file, "" from a Core before #49, whose versions had one file.
type TextEventOf struct {
	VersionID string `json:"version_id"`
	FileID    string `json:"file_id,omitempty"`
}
