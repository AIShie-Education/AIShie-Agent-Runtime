package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The transcriber (package transcribe, docs/design.md §12) keeps three
// things here beside the site's setting of it (SettingTranscription): the
// service credential Core issued for it, sealed, which the site's
// administrators hand over through the API; its record of what it did with
// each version it claimed, which the API lists; and the cost of its model
// calls, in the ledger as calls of their own kind (CallTranscription).

// Kinds of a model call in the ledger (LLMCall.Kind).
const (
	// CallAnswer is a model call of an agent's answer: "" is one too, as a
	// release before the transcriber wrote every call.
	CallAnswer = "model_calls"
	// CallTranscription is a model call of the transcriber's, on the
	// school's key: of no agent, tenant, course or asker, and counted
	// against the plan's whole key (SpendScope{KeySource}) alone.
	CallTranscription = "transcription"
)

// KindOf is a call's kind as the ledger keeps it: CallAnswer for "".
func (c LLMCall) KindOf() string {
	if c.Kind == "" {
		return CallAnswer
	}
	return c.Kind
}

// checkCall refuses a ledger row a store must not keep: without its id, of
// no known kind, or an answer's without its agent. A transcription's is of
// no agent, tenant, course or asker.
func checkCall(c LLMCall) error {
	switch {
	case c.ID == "":
		return errors.New("store: llm call: id required")
	case c.KindOf() == CallAnswer && c.AgentID == "":
		return fmt.Errorf("store: llm call %s: agent_id required", c.ID)
	case c.KindOf() == CallTranscription && (c.AgentID != "" || c.TenantID != "" || c.CourseID != "" || c.OpenerMemberID != ""):
		return fmt.Errorf("store: llm call %s: a transcription's is no agent's, tenant's, course's or asker's", c.ID)
	case c.KindOf() != CallAnswer && c.KindOf() != CallTranscription:
		return fmt.Errorf("store: llm call %s: kind %q is neither %s nor %s", c.ID, c.Kind, CallAnswer, CallTranscription)
	}
	return nil
}

// CheckCall is checkCall, for the stores.
func CheckCall(c LLMCall) error { return checkCall(c) }

// SiteTenantID is the tenant of the site's own secrets, the transcriber's
// service credential: no owner's (ten_…), nor the school's keys'.
const SiteTenantID = "site"

// TranscriptionCredential is the service credential the transcriber uses
// with Core (a Core token of the service's, aissvc_…), sealed: a
// core_token of SiteTenantID. There is one at most.
type TranscriptionCredential struct {
	// SecretID is the sealed token, and Hint what may be shown of it: the
	// token's public prefix.
	SecretID string `json:"secret_id"`
	Hint     string `json:"hint"`
	// CredentialID is Core's id of it, when the front end gave it; "".
	CredentialID string `json:"credential_id"`
	// Tested is whether Core took it when it was given (a call that
	// claims nothing), false when the test was skipped.
	Tested bool `json:"tested"`
	// SetBy is the Core actor who gave it, and SetAt when.
	SetBy string    `json:"set_by"`
	SetAt time.Time `json:"set_at"`
	// LastOKAt is when Core last took it from the transcriber, nil for
	// never; RejectedAt when Core last refused it (401), nil for never
	// since it was given, and LastError why, in English.
	LastOKAt   *time.Time `json:"last_ok_at"`
	RejectedAt *time.Time `json:"rejected_at"`
	LastError  string     `json:"last_error"`
}

// Where a transcription job stands (TranscriptionJob.Status).
const (
	JobWorking = "working"
	JobDone    = "done"
	JobFailed  = "failed"
	JobSkipped = "skipped"
	// JobDropped is a claim given up: Core took it back (lease_lost), its
	// staff wrote the text meanwhile (edited_by_staff), the document was
	// archived or purged, or the worker stopped part way (interrupted).
	JobDropped = "dropped"
)

// JobStatusKnown reports whether s is one of the statuses above.
func JobStatusKnown(s string) bool {
	switch s {
	case JobWorking, JobDone, JobFailed, JobSkipped, JobDropped:
		return true
	}
	return false
}

// TranscriptionJob is the transcriber's record of one version it claimed
// and what became of it: ids, sizes, counts and costs, never the file's
// name or text.
type TranscriptionJob struct {
	// ID is trj_…, and Seq its place among the jobs, which lists are
	// ordered and paged by: the store's, given when it is first kept.
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
	// VersionID, DocumentID and CourseID are Core's; LeaseID the claim's.
	VersionID  string `json:"version_id"`
	DocumentID string `json:"document_id"`
	CourseID   string `json:"course_id"`
	LeaseID    string `json:"lease_id"`
	Status     string `json:"status"`
	// Reason is why it failed, was skipped or dropped, as a code Core is
	// told (too_many_pages, model_error, …) or a short sentence; "".
	Reason   string `json:"reason"`
	Backfill bool   `json:"backfill"`
	// Attempt is Core's count of the version's claims, this one's.
	Attempt     int    `json:"attempt"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	// Pages is the file's, nil until they are counted; PagesSent those
	// sent to the model, which the daily quota counts.
	Pages     *int `json:"pages"`
	PagesSent int  `json:"pages_sent"`
	// Offer is the plan's offer that transcribed it and Model its model;
	// "" before one was called.
	Offer string `json:"offer"`
	Model string `json:"model"`
	// ModelCalls are the calls made; CostPUSD their cost, nil when a call
	// had no price; the tokens are theirs, in and out.
	ModelCalls   int    `json:"model_calls"`
	CostPUSD     *int64 `json:"cost_pusd"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	// Worker is the process that worked on it.
	Worker string `json:"worker"`
	// StartedAt is when it was claimed (zero, the store's now);
	// HeartbeatAt when its claim was last held, which a job left working
	// by a worker gone is found by; FinishedAt when it ended, nil while
	// it works.
	StartedAt   time.Time  `json:"started_at"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// ReasonInterrupted is the reason of a job left working by a worker that
// stopped before it ended: its claim lapsed in Core, which gives the
// version to the next claim.
const ReasonInterrupted = "interrupted"

// maxJobReason bounds a job's reason, as Core bounds it.
const maxJobReason = 500

// CheckTranscriptionJob refuses a job a store must not keep: without its
// id or version, of a status not known, with negative counts, or a reason
// past what Core keeps.
func CheckTranscriptionJob(j TranscriptionJob) error {
	var bad []string
	if !strings.HasPrefix(j.ID, "trj_") || len(j.ID) > 64 {
		bad = append(bad, "id (trj_…)")
	}
	if j.VersionID == "" {
		bad = append(bad, "version_id")
	}
	if !JobStatusKnown(j.Status) {
		bad = append(bad, "a status of working, done, failed, skipped or dropped")
	}
	if j.ByteSize < 0 || j.PagesSent < 0 || j.ModelCalls < 0 || j.InputTokens < 0 || j.OutputTokens < 0 ||
		j.Pages != nil && *j.Pages < 0 || j.CostPUSD != nil && *j.CostPUSD < 0 {
		bad = append(bad, "counts of zero or more")
	}
	if utf8.RuneCountInString(j.Reason) > maxJobReason {
		bad = append(bad, "a reason of at most 500 characters")
	}
	if len(bad) > 0 {
		return fmt.Errorf("store: transcription job: %s required", strings.Join(bad, ", "))
	}
	return nil
}

// JobQuery pages TranscriptionJobs: those of Status alone when it is set,
// newest first, those before the job of seq Before when it is not 0, at
// most Limit.
type JobQuery struct {
	Status string
	Before int64
	Limit  int
}

// MaxJobPage bounds JobQuery.Limit.
const MaxJobPage = 500

// TranscriptionDay is what the transcriber did since a time: the pages it
// sent to the model, the documents it transcribed, those that failed and
// those it skipped, and what its model calls cost (those it could price).
type TranscriptionDay struct {
	Pages     int   `json:"pages"`
	Documents int   `json:"documents"`
	Failed    int   `json:"failed"`
	Skipped   int   `json:"skipped"`
	CostPUSD  int64 `json:"cost_pusd"`
}

// Transcription is what the transcriber keeps: its credential, and its
// jobs.
type Transcription interface {
	// TranscriptionCredential is the credential; ErrNotFound when none is
	// kept.
	TranscriptionCredential(ctx context.Context) (*TranscriptionCredential, error)
	// PutTranscriptionCredential keeps c with its token, which must be
	// the secret it refers to, in place of the one before, whose secret is
	// destroyed in the same transaction. A zero SetAt is the store's now.
	// It moves the registry's revision on, so that every worker's
	// transcriber takes it.
	PutTranscriptionCredential(ctx context.Context, c TranscriptionCredential, token Secret) error
	// DeleteTranscriptionCredential forgets the credential and destroys
	// its secret; none is nothing. It moves the registry's revision on.
	DeleteTranscriptionCredential(ctx context.Context) error
	// NoteTranscriptionCredential records what Core made of the
	// credential secretID at at, if it is still the one kept: taken (ok),
	// or refused, with why; ErrNotFound when it is not. It moves nothing
	// on: a note is no change of the site's.
	NoteTranscriptionCredential(ctx context.Context, secretID string, ok bool, at time.Time, why string) error

	// PutTranscriptionJob keeps j, in place of the job of its id, whose
	// place (Seq) it keeps; a new one takes the next. A zero StartedAt or
	// HeartbeatAt is the store's now.
	PutTranscriptionJob(ctx context.Context, j TranscriptionJob) (*TranscriptionJob, error)
	// TranscriptionJobs lists the jobs as q says, newest first.
	TranscriptionJobs(ctx context.Context, q JobQuery) ([]TranscriptionJob, error)
	// TranscriptionDay sums the jobs begun since since.
	TranscriptionDay(ctx context.Context, since time.Time) (TranscriptionDay, error)
	// InterruptTranscriptionJobs ends the jobs still working whose claim
	// was last held before before: dropped, ReasonInterrupted, finished
	// at at. It says how many.
	InterruptTranscriptionJobs(ctx context.Context, before, at time.Time) (int64, error)
	// PruneTranscriptionJobs destroys the jobs begun before before, and
	// says how many.
	PruneTranscriptionJobs(ctx context.Context, before time.Time) (int64, error)
}

// CheckTranscriptionCredential refuses a credential a store must not keep:
// without its secret, which must be the token given, a core_token of
// SiteTenantID, or its setter.
func CheckTranscriptionCredential(c TranscriptionCredential, token Secret) error {
	if err := CheckSecret(token); err != nil {
		return err
	}
	switch {
	case !IsSecretID(c.SecretID) || token.ID != c.SecretID:
		return errors.New("store: transcription credential: its secret must be the token given")
	case token.Kind != SecretCoreToken || token.TenantID != SiteTenantID:
		return fmt.Errorf("store: transcription credential: its token must be a %s of tenant %s", SecretCoreToken, SiteTenantID)
	case c.SetBy == "":
		return errors.New("store: transcription credential: set_by required")
	}
	return nil
}

// CheckJobQuery refuses a page of jobs of a status not known, or of a
// limit not from 1 to MaxJobPage.
func CheckJobQuery(q JobQuery) error {
	if q.Status != "" && !JobStatusKnown(q.Status) {
		return fmt.Errorf("store: jobs of status %q", q.Status)
	}
	if q.Limit < 1 || q.Limit > MaxJobPage {
		return fmt.Errorf("store: a page of 1 to %d jobs", MaxJobPage)
	}
	return nil
}
