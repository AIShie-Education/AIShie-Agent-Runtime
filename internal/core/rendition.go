package core

import (
	"context"
	"fmt"
	"time"
)

// The agent runtime's renditions (AIShie-Core's migration 0026, its
// docs/schema.md §2.4 Renditions and docs/agent-runtime.md §2.0.1): every
// Office or OpenDocument file Core keeps, of a document's version or
// carried by a message, is converted to PDF once, by the site's agent
// runtime. It claims what waits (agent_runtime.rendition_claim, a long
// poll), fetches the file from a short-lived URL (a fresh one with
// rendition_file), holds its claim while it converts (rendition_renew),
// asks for somewhere to put the PDF (rendition_upload_url), PUTs it there,
// and says what became of it (rendition_complete): done, naming the upload
// and its pages, or failed or skipped with a reason. Core serves these over
// REST alone, to the agent_runtime service's credential and nobody else's,
// as it does the service's hosting (runtime.go): RuntimeService makes them
// over a RuntimeCaller.

// The renditions' tools, as the catalogue names them over MCP (Core serves
// none of them over MCP).
const (
	ToolRenditionClaim     = "agent_runtime_rendition_claim"
	ToolRenditionFile      = "agent_runtime_rendition_file"
	ToolRenditionRenew     = "agent_runtime_rendition_renew"
	ToolRenditionUploadURL = "agent_runtime_rendition_upload_url"
	ToolRenditionComplete  = "agent_runtime_rendition_complete"
)

// Where a rendition stands (RenditionCompleted.State, RenditionView.State),
// and what a completion says of it.
const (
	RenditionQueued  = "queued"
	RenditionClaimed = "claimed"
	RenditionDone    = "done"
	RenditionFailed  = "failed"
	RenditionSkipped = "skipped"
)

// Why a rendition failed or was skipped: the runtime's five, which it gives
// Core with either status, and Core's own attempts_exhausted
// (ReasonAttemptsExhausted).
const (
	// RenditionPasswordProtected: the file is protected by a password.
	RenditionPasswordProtected = "password_protected"
	// RenditionUnsupported: the file could not be read as an Office
	// document.
	RenditionUnsupported = "unsupported"
	// RenditionTooLarge: the PDF would be larger than Core takes.
	RenditionTooLarge = "too_large"
	// RenditionConversionFailed: the conversion failed.
	RenditionConversionFailed = "conversion_failed"
	// RenditionTimeout: the conversion took too long.
	RenditionTimeout = "timeout"
)

// Core's refusals of a rendition's calls, in error.details.reason, beside
// ReasonLeaseLost (the claim does not hold: stop, and upload nothing).
const (
	// ReasonBadUploadToken: not a rendition's upload token.
	ReasonBadUploadToken = "bad_upload_token" // #nosec G101 -- a reason's code, not a credential.
	// ReasonNotYourUpload: an upload made for another claim or rendition.
	ReasonNotYourUpload = "not_your_upload"
	// ReasonNotUploaded: nothing was PUT to the upload's URL yet.
	ReasonNotUploaded = "not_uploaded"
	// ReasonNotAPDF: what was PUT does not begin %PDF-; Core deleted it,
	// and the claim holds.
	ReasonNotAPDF = "not_a_pdf"
	// ReasonRenditionTooLarge: what was PUT is larger than max_bytes;
	// Core deleted it, and the claim holds.
	ReasonRenditionTooLarge = "rendition_too_large"
	// ReasonNoFileStorage: the site keeps no files.
	ReasonNoFileStorage = "no_file_storage"
)

// Sources of a rendition's file (ClaimedRendition.Source).
const (
	RenditionOfDocumentFile = "document_file"
	RenditionOfAttachment   = "attachment"
)

// Bounds of the renditions' protocol.
const (
	// MaxRenditionClaims is the most one claim takes.
	MaxRenditionClaims = 10
	// DefaultRenditionMaxBytes is Core's largest PDF where it says
	// nothing else (RENDITION_MAX_BYTES): every claim and upload URL says
	// its own (max_bytes).
	DefaultRenditionMaxBytes = 100 << 20
	// MaxRenditionPages is the most pages Core takes of a PDF.
	MaxRenditionPages = 100000
)

// HasRenditions reports whether cat offers the renditions' five tools: a
// Core since AIShie-Core's migration 0026.
func HasRenditions(cat *Catalogue) bool {
	if cat == nil {
		return false
	}
	for _, name := range []string{ToolRenditionClaim, ToolRenditionFile, ToolRenditionRenew, ToolRenditionUploadURL, ToolRenditionComplete} {
		if _, ok := cat.Tool(name); !ok {
			return false
		}
	}
	return true
}

// ClaimedRendition is one rendition the runtime claimed: the claim (its
// lease, until when it holds, how many claims it has had, this one
// included, and whether the backfill queued it), whose file it is (a
// version's, FileID, or a message's, AttachmentID, in CourseID), the file
// as Core has it, a short-lived URL that serves it (signed: no
// credential, and a credential for the file), and the largest PDF Core
// takes.
type ClaimedRendition struct {
	RenditionID       string    `json:"rendition_id"`
	LeaseID           string    `json:"lease_id"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
	Attempt           int       `json:"attempt"`
	Backfill          bool      `json:"backfill"`
	CourseID          string    `json:"course_id"`
	Source            string    `json:"source"`
	FileID            string    `json:"file_id,omitempty"`
	AttachmentID      string    `json:"attachment_id,omitempty"`
	Filename          string    `json:"filename"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          string    `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
	MaxBytes          int64     `json:"max_bytes"`
}

// RenditionFile is rendition_file's result: the claimed file again, at a
// fresh URL.
type RenditionFile struct {
	RenditionID       string    `json:"rendition_id"`
	Filename          string    `json:"filename"`
	ContentType       string    `json:"content_type"`
	ByteSize          int64     `json:"byte_size"`
	Checksum          string    `json:"checksum,omitempty"`
	DownloadURL       string    `json:"download_url"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
}

// RenditionUpload is rendition_upload_url's result: where to PUT the PDF,
// once, with exactly Headers and no Authorization header, until
// ExpiresAt, and the token that names the upload in the completion. The
// URL and the token are credentials: never logged.
type RenditionUpload struct {
	UploadURL   string            `json:"upload_url"`
	Headers     map[string]string `json:"headers"`
	UploadToken string            `json:"upload_token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	MaxBytes    int64             `json:"max_bytes"`
}

// RenditionCompletion is what the runtime says became of a rendition:
// done, with the upload's token and the PDF's pages; or failed or skipped,
// with one of the runtime's reasons.
type RenditionCompletion struct {
	Status      string
	UploadToken string
	PageCount   int
	Reason      string
}

// RenditionCompleted is rendition_complete's result: where the rendition
// stands, and, done, the PDF as Core keeps it.
type RenditionCompleted struct {
	RenditionID string `json:"rendition_id"`
	State       string `json:"state"`
	ByteSize    int64  `json:"byte_size,omitempty"`
	Checksum    string `json:"checksum,omitempty"`
}

// RenditionClaim names one claim in the calls that follow it.
type RenditionClaim struct {
	RenditionID, LeaseID string
}

// ClaimOfRendition is r's claim.
func ClaimOfRendition(r ClaimedRendition) RenditionClaim {
	return RenditionClaim{RenditionID: r.RenditionID, LeaseID: r.LeaseID}
}

// ClaimRenditions claims up to n renditions waiting (1 to
// MaxRenditionClaims), each for lease, rounded to whole seconds within
// MinLease and MaxLease. wait above zero is wait_s, whole seconds up to
// MaxWait: a call that finds nothing waits that long for a file to be
// queued anywhere, and answers as soon as one is, or with none.
func (s *RuntimeService) ClaimRenditions(ctx context.Context, n int, lease, wait time.Duration) ([]ClaimedRendition, error) {
	ctx, secs := waiting(ctx, wait)
	var r struct {
		Claimed []ClaimedRendition `json:"claimed"`
	}
	err := serviceCall(ctx, s.c, ToolRenditionClaim, struct {
		Max    int `json:"max"`
		LeaseS int `json:"lease_s"`
		WaitS  int `json:"wait_s,omitempty"`
	}{min(max(n, 1), MaxRenditionClaims), leaseSeconds(lease), secs}, &r)
	return r.Claimed, err
}

// claimArgs are the arguments that name a rendition's claim.
func (cl RenditionClaim) args() map[string]any {
	return map[string]any{"rendition_id": cl.RenditionID, "lease_id": cl.LeaseID}
}

// RenditionFile is the claimed rendition's file again, at a fresh URL,
// while the claim holds; ReasonLeaseLost otherwise, and not found for a
// rendition whose file was purged.
func (s *RuntimeService) RenditionFile(ctx context.Context, cl RenditionClaim) (*RenditionFile, error) {
	var f RenditionFile
	if err := serviceCall(ctx, s.c, ToolRenditionFile, cl.args(), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// RenewRendition holds the claim for lease from now, and says until when;
// ReasonLeaseLost once it does not hold: stop, and upload nothing.
func (s *RuntimeService) RenewRendition(ctx context.Context, cl RenditionClaim, lease time.Duration) (time.Time, error) {
	var r struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	args := cl.args()
	args["lease_s"] = leaseSeconds(lease)
	err := serviceCall(ctx, s.c, ToolRenditionRenew, args, &r)
	return r.LeaseExpiresAt, err
}

// RenditionUploadURL is somewhere to PUT the claimed rendition's PDF, a
// new one each call, while the claim holds; ReasonLeaseLost otherwise.
func (s *RuntimeService) RenditionUploadURL(ctx context.Context, cl RenditionClaim) (*RenditionUpload, error) {
	var u RenditionUpload
	if err := serviceCall(ctx, s.c, ToolRenditionUploadURL, cl.args(), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// RenditionKey is the idempotency key of a completion of the claim cl:
// rendition:{rendition_id}:{lease_id}:{n}, as Core suggests, n the same
// for every try of one completion (after a timeout), and the next after a
// refusal that leaves the claim held (a PDF uploaded again).
func RenditionKey(cl RenditionClaim, n int) string {
	return fmt.Sprintf("rendition:%s:%s:%d", cl.RenditionID, cl.LeaseID, n)
}

// CompleteRendition says what became of the claimed rendition, under key
// (RenditionKey). Core refuses it with ReasonLeaseLost (the claim does not
// hold), not found (the file purged), and, for done, ReasonBadUploadToken,
// ReasonNotYourUpload, ReasonNotUploaded, and ReasonNotAPDF or
// ReasonRenditionTooLarge, after which the claim holds and the upload is
// gone. The upload token is a credential: Core keeps it out of the
// action's record, and nothing here logs it.
func (s *RuntimeService) CompleteRendition(ctx context.Context, cl RenditionClaim, key string, c RenditionCompletion) (*RenditionCompleted, error) {
	args := cl.args()
	args["status"], args[idempotencyKeyArg] = c.Status, key
	if c.Status == RenditionDone {
		args["upload_token"], args["page_count"] = c.UploadToken, c.PageCount
	} else {
		args["reason"] = c.Reason
	}
	var r RenditionCompleted
	if err := serviceCall(ctx, s.c, ToolRenditionComplete, args, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// RenditionView is a file's PDF rendition as its readers are shown it
// (document_get's and document_file's files, conversation_attachment):
// where it stands; done, its pages, its size and, where the read gives
// URLs, a short-lived URL that shows the PDF (a credential for it, never
// a model's: the runtime fetches it to give a model reading the file that
// PDF, package toolset); failed or skipped, why. A file that is not
// converted has none.
type RenditionView struct {
	State             string     `json:"state"`
	PageCount         int        `json:"page_count,omitempty"`
	ByteSize          int64      `json:"byte_size,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	DownloadURL       string     `json:"download_url,omitempty"`
	DownloadExpiresAt *time.Time `json:"download_expires_at,omitempty"`
}
