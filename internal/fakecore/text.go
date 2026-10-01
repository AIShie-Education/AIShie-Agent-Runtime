package fakecore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Text versions (AIShie-Core #43, a file's own since #49): each file of a
// version of a course's material, instructions or rubric has a text
// version, queued (pending) as the version is added; the site's
// transcription service claims files from Core's queue with a credential
// of its own (kind service), renews its claim while it works, and
// completes it: done with the Markdown, or failed or skipped with why.
// Every call of the service's names the file (file_id); one that does not
// is about the file its lease is of, as a runtime of before sends it.
// Staff may write it themselves (EditText), which no transcription writes
// over, or have it transcribed again (Retranscribe). document_get shows it
// beside each file, document_text reads a file's in parts, and the
// course's feed says when one is done or discarded (document.text_updated,
// …, with the file's id), never with the text.
//
// The service's four tools are REST's alone. A service credential calls
// nothing else (403 not_for_services), and nobody else calls them (403
// service_only); a revoked one is 401, and its claims go back to the queue.

// kindService is the service's actor's kind.
const kindService = "service"

// Bounds of the text versions, as Core has them.
const (
	maxTextBytes    = 2 << 20
	textPartBytes   = 65536
	maxTextAttempts = 5
	defaultLeaseS   = 600
	downloadTTL     = 15 * time.Minute
)

// textVersion is a version's text version.
type textVersion struct {
	status, source, model, reason string
	pages                         int
	producedAt, editedAt          *time.Time
	editedBy                      *member
	revision                      int
	updatedAt                     time.Time
	body                          string
	// attempts are the claims since it was queued; backfill says it was
	// queued by the backfill, and queuedAt when.
	attempts int
	backfill bool
	queuedAt time.Time
	// claim is the service's hold on it, nil for none.
	claim *textClaim
}

type textClaim struct {
	leaseID string
	expires time.Time
	cred    *credential
}

// textView is a text version as Core shows it.
type textView struct {
	Status           string     `json:"status"`
	Source           *string    `json:"source,omitempty"`
	Model            *string    `json:"model,omitempty"`
	Pages            *int       `json:"pages,omitempty"`
	Reason           *string    `json:"reason,omitempty"`
	ProducedAt       *time.Time `json:"produced_at,omitempty"`
	EditedByMemberID *string    `json:"edited_by_member_id,omitempty"`
	EditedByName     *string    `json:"edited_by_name,omitempty"`
	EditedAt         *time.Time `json:"edited_at,omitempty"`
	Revision         int        `json:"revision"`
	UpdatedAt        time.Time  `json:"updated_at"`
	Bytes            int        `json:"bytes"`
	Body             *string    `json:"body,omitempty"`
}

// view is tv as Core shows it; withBody gives its body where document_get
// does: done, and no longer than one part.
func (tv *textVersion) view(withBody bool) *textView {
	v := &textView{Status: tv.status, Model: strOrNil(tv.model), Reason: strOrNil(tv.reason), ProducedAt: tv.producedAt,
		EditedAt: tv.editedAt, Revision: tv.revision, UpdatedAt: tv.updatedAt, Bytes: len(tv.body)}
	if tv.status == textDone {
		v.Source = strOrNil(tv.source)
	}
	if tv.pages > 0 {
		v.Pages = &tv.pages
	}
	if tv.editedBy != nil {
		v.EditedByMemberID, v.EditedByName = &tv.editedBy.id, &tv.editedBy.actor.name
	}
	if withBody && tv.status == textDone && len(tv.body) <= textPartBytes {
		b := tv.body
		v.Body = &b
	}
	return v
}

// viewIf is tv's view, nil where there is no text version.
func (tv *textVersion) viewIf(withBody bool) *textView {
	if tv == nil {
		return nil
	}
	return tv.view(withBody)
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Where a text version stands, and whose it is.
const (
	textPending = "pending"
	textWorking = "working"
	textDone    = "done"
	textFailed  = "failed"
	textSkipped = "skipped"
	sourceAI    = "ai"
	sourceStaff = "staff"
)

// newText is a text version queued at now. Called with the lock held.
func (c *Core) newText(now time.Time, backfill bool) *textVersion {
	return &textVersion{status: textPending, revision: 1, updatedAt: now, backfill: backfill, queuedAt: now}
}

// queued wakes the service's calls that wait for a version to claim.
// Called with the lock held.
func (c *Core) queued() {
	if c.textNews != nil {
		close(c.textNews)
	}
	c.textNews = make(chan struct{})
}

// holds reports whether the claim on tv is the lease's and holds at now.
func (tv *textVersion) holds(leaseID string, now time.Time) bool {
	cl := tv.claim
	return cl != nil && cl.leaseID == leaseID && cl.expires.After(now) && !cl.cred.revoked()
}

// textEvent files the course's news of f's text: its type by what the
// document is and whether the version is published.
func (c *Core) textEvent(f *versionFile, status string) {
	doc := f.doc
	typ := "document.text_updated"
	switch {
	case doc.draft:
		typ = "document.draft_text_updated"
	case doc.kind == kindRubric:
		typ = "document.rubric_text_updated"
	}
	tv := f.text
	payload := map[string]any{"kind": doc.kind, "version_id": doc.versionID, "file_id": f.id, "status": status, "revision": tv.revision}
	if c.opts.WithoutFiles {
		delete(payload, "file_id")
	}
	if status == textDone {
		payload["source"] = tv.source
	}
	id := newID()
	c.flush([]*event{{typ: typ, course: doc.course, actionID: &id, subjectType: "document", subjectID: &doc.id, payload: mustJSON(payload)}})
}

// textFile is the file of the version versionID, with a text version,
// that fileID names, or, for none, the version's one file, or of several
// the one the lease leaseID holds; nil for none. Called with the lock
// held.
func (c *Core) textFile(versionID, fileID, leaseID string) *versionFile {
	for _, co := range c.courses {
		for _, d := range co.documents {
			if d.versionID != versionID {
				continue
			}
			var texts []*versionFile
			for _, f := range d.files {
				if f.text != nil {
					texts = append(texts, f)
				}
			}
			if fileID == "" && len(texts) == 1 {
				// A version of one file: that file, as before.
				return texts[0]
			}
			for _, f := range texts {
				if fileID != "" && f.id == fileID || fileID == "" && f.text.claim != nil && f.text.claim.leaseID == leaseID {
					return f
				}
			}
		}
	}
	return nil
}

// serviceRefusal is what a service's call that is not the service's, or a
// call of the service's elsewhere, is told, or nil.
func serviceRefusal(caller *actor, t *toolDef) *apiError {
	switch {
	case caller.kind == kindService && !t.restOnly:
		return denial("not_for_services")
	case t.restOnly && caller.kind != kindService:
		return denial("service_only")
	}
	return nil
}

// invokeService carries out one of the service's four tools, called with
// the credential cred. The lock is held.
func (c *Core) invokeService(caller *actor, cred *credential, t *toolDef, raw []byte, key, base string) outcome {
	if _, err := t.decodeArgs(raw); err != nil {
		return c.failure(err)
	}
	now := c.now()
	switch t.Name {
	case "document_text.queue":
		var in struct {
			Max    *int `json:"max"`
			LeaseS *int `json:"lease_s"`
		}
		_ = json.Unmarshal(raw, &in)
		n, lease := 1, defaultLeaseS
		if in.Max != nil {
			n = *in.Max
		}
		if in.LeaseS != nil {
			lease = *in.LeaseS
		}
		if n < 1 || n > 10 {
			return errorOutcome(invalid("max is from 1 to 10"))
		}
		if lease < 60 || lease > 3600 {
			return errorOutcome(invalid("lease_s is from 60 to 3600"))
		}
		claimed := c.claim(cred, n, time.Duration(lease)*time.Second, now, base)
		return executed(map[string]any{"claimed": claimed})
	case "document_text.file":
		var in struct {
			VersionID string `json:"version_id"`
			LeaseID   string `json:"lease_id"`
			FileID    string `json:"file_id"`
		}
		_ = json.Unmarshal(raw, &in)
		f, e := c.claimed(in.VersionID, in.FileID, in.LeaseID, now, false)
		if e != nil {
			return errorOutcome(e)
		}
		return executed(c.fileOf(f, base, now))
	case "document_text.renew":
		var in struct {
			VersionID string `json:"version_id"`
			LeaseID   string `json:"lease_id"`
			FileID    string `json:"file_id"`
			LeaseS    *int   `json:"lease_s"`
		}
		_ = json.Unmarshal(raw, &in)
		lease := defaultLeaseS
		if in.LeaseS != nil {
			lease = *in.LeaseS
		}
		if lease < 60 || lease > 3600 {
			return errorOutcome(invalid("lease_s is from 60 to 3600"))
		}
		f, e := c.claimed(in.VersionID, in.FileID, in.LeaseID, now, true)
		if e != nil {
			return errorOutcome(e)
		}
		f.text.claim.expires = now.Add(time.Duration(lease) * time.Second)
		f.text.updatedAt = now
		return executed(map[string]any{"lease_expires_at": f.text.claim.expires})
	}
	return c.complete(caller, t, raw, key, now)
}

// executed is an executed read's or ephemeral write's outcome.
func executed(v any) outcome {
	return outcome{Status: actExecuted, Result: mustJSON(v)}
}

// claim claims up to n files waiting for cred, for lease: uploads first,
// those waiting longest first, then the backfill, newest first; a
// version's files in order. A claim that lapsed is waiting again, and one
// that lapsed maxTextAttempts times fails. Nothing of an archived course
// is handed out. Called with the lock held.
func (c *Core) claim(cred *credential, n int, lease time.Duration, now time.Time, base string) []map[string]any {
	var waiting []*versionFile
	for _, co := range c.courses {
		if co.status == statusArchived {
			continue
		}
		for _, d := range co.documents {
			for _, f := range d.files {
				tv := f.text
				if tv == nil {
					continue
				}
				lapsed := tv.status == textWorking && tv.claim != nil && (!tv.claim.expires.After(now) || tv.claim.cred.revoked())
				if lapsed && tv.attempts >= maxTextAttempts {
					tv.status, tv.reason, tv.claim, tv.updatedAt = textFailed, "attempts_exhausted", nil, now
					continue
				}
				if tv.status == textPending || lapsed {
					waiting = append(waiting, f)
				}
			}
		}
	}
	slices.SortStableFunc(waiting, func(a, b *versionFile) int {
		ta, tb := a.text, b.text
		switch {
		case ta.backfill != tb.backfill:
			if ta.backfill {
				return 1
			}
			return -1
		case ta.backfill:
			return tb.queuedAt.Compare(ta.queuedAt)
		}
		return ta.queuedAt.Compare(tb.queuedAt)
	})
	out := []map[string]any{}
	for _, f := range waiting[:min(n, len(waiting))] {
		tv, d := f.text, f.doc
		tv.attempts++
		tv.status, tv.updatedAt = textWorking, now
		tv.claim = &textClaim{leaseID: uuid.NewString(), expires: now.Add(lease), cred: cred}
		claimed := map[string]any{
			"version_id": d.versionID, "file_id": f.id, "position": f.position, "filename": f.filename, "document_id": d.id,
			"course_id": d.course.id, "lease_id": tv.claim.leaseID, "lease_expires_at": tv.claim.expires, "attempt": tv.attempts,
			"backfill": tv.backfill, "content_type": f.contentType, "byte_size": len(f.data), "checksum": f.checksum(),
			"download_url": base + blobPath + f.token, "download_expires_at": now.Add(downloadTTL),
		}
		if c.opts.WithoutFiles {
			delete(claimed, "file_id")
			delete(claimed, "position")
			delete(claimed, "filename")
		}
		out = append(out, claimed)
	}
	return out
}

// claimed is the file of the version versionID claimed under leaseID,
// fileID or, for none, the lease's: not found when there is none; for a
// renewal, edited_by_staff when staff wrote the text meanwhile; lease_lost
// when the claim does not hold (and, naming no file, when no file of the
// version holds the lease any more); course_archived when its course was
// archived. Called with the lock held.
func (c *Core) claimed(versionID, fileID, leaseID string, now time.Time, renew bool) (*versionFile, *apiError) {
	f := c.textFile(versionID, fileID, leaseID)
	switch {
	case f == nil && fileID == "" && c.hasVersion(versionID):
		return nil, conflicts("the claim no longer holds: the version was claimed again, or sent back to the queue").with("reason", "lease_lost")
	case f == nil:
		return nil, missing("no such text version")
	case renew && f.text.source == sourceStaff && f.text.claim != nil && f.text.claim.leaseID == leaseID:
		return nil, conflicts("staff have written the text; it is theirs, and is not written over").with("reason", "edited_by_staff")
	case !f.text.holds(leaseID, now):
		return nil, conflicts("the claim no longer holds: the version was claimed again, or sent back to the queue").with("reason", "lease_lost")
	case f.doc.course.status == statusArchived:
		return nil, forbid("the course is archived").with("reason", "course_archived")
	}
	return f, nil
}

// hasVersion reports whether a version of versionID has a file with a text
// version. Called with the lock held.
func (c *Core) hasVersion(versionID string) bool {
	for _, co := range c.courses {
		for _, d := range co.documents {
			if d.versionID == versionID {
				return slices.ContainsFunc(d.files, func(f *versionFile) bool { return f.text != nil })
			}
		}
	}
	return false
}

// fileOf is document_text.file's result for f's claim.
func (c *Core) fileOf(f *versionFile, base string, now time.Time) any {
	return c.older(map[string]any{"version_id": f.doc.versionID, "file_id": f.id, "position": f.position, "filename": f.filename,
		"content_type": f.contentType, "byte_size": len(f.data), "checksum": f.checksum(), "download_url": base + blobPath + f.token,
		"download_expires_at": now.Add(downloadTTL), "lease_expires_at": f.text.claim.expires}, "file_id", "position", "filename")
}

// completion is document_text.complete's input.
type completion struct {
	VersionID string  `json:"version_id"`
	LeaseID   string  `json:"lease_id"`
	FileID    string  `json:"file_id"`
	Status    string  `json:"status"`
	Body      *string `json:"body"`
	Pages     *int    `json:"pages"`
	Model     *string `json:"model"`
	Reason    *string `json:"reason"`
}

// complete is document_text.complete: a write, recorded, and replayed
// under its key; refused, nothing is written.
func (c *Core) complete(caller *actor, t *toolDef, raw []byte, key string, now time.Time) outcome {
	if err := checkKey(t, key); err != nil {
		return c.failure(err)
	}
	canonical, err := canonicalize(raw)
	if err != nil {
		return errorOutcome(invalid("%v", err))
	}
	hash := payloadHash(t.Name, canonical)
	if existing := c.keys[actorKey{caller.id, key}]; existing != nil {
		out, err := replay(existing, hash)
		if err != nil {
			return c.failure(err)
		}
		return out
	}
	var in completion
	_ = json.Unmarshal(raw, &in)
	act := &action{id: newID(), actor: caller, actionType: t.Name, targetType: "document_version", targetID: &in.VersionID,
		payload: canonical, hash: hash, key: key, authz: autonomous, status: actExecuted, reviewState: reviewNone, createdAt: now}
	f, e := c.completeText(in, now)
	if f == nil && e != nil && e.Code == codeNotFound {
		// A version not there is a call never attempted, as Core's is.
		return errorOutcome(e)
	}
	c.recordAction(act)
	if e != nil {
		act.status, act.result = actFailed, errorResult(e)
		return outcome{Status: actFailed, ActionID: act.id, ReviewState: reviewNone, Error: e}
	}
	res := mustJSON(c.older(map[string]any{"version_id": in.VersionID, "file_id": f.id, "status": in.Status, "revision": f.text.revision},
		"file_id"))
	act.executedAt, act.result = &now, res
	return outcome{Status: actExecuted, ActionID: act.id, ReviewState: reviewNone, Result: res}
}

// completeText writes a completion into the file's text, or says why not.
// A refusal after the file was found comes with it. Called with the lock
// held.
func (c *Core) completeText(in completion, now time.Time) (*versionFile, *apiError) {
	f := c.textFile(in.VersionID, in.FileID, in.LeaseID)
	switch {
	case f == nil && in.FileID == "" && c.hasVersion(in.VersionID):
		// A version of several files none of which holds the lease: the
		// call was attempted, and refused.
		return &versionFile{}, conflicts("the claim no longer holds: the version was claimed again, or sent back to the queue").with("reason", "lease_lost")
	case f == nil:
		return nil, missing("no such text version")
	}
	if e := checkCompletion(in); e != nil {
		return f, e
	}
	tv := f.text
	switch {
	case tv.source == sourceStaff && tv.claim != nil && tv.claim.leaseID == in.LeaseID:
		return f, conflicts("staff have written the text; it is theirs, and is not written over").with("reason", "edited_by_staff")
	case !tv.holds(in.LeaseID, now):
		return f, conflicts("the claim no longer holds: the version was claimed again, or sent back to the queue").with("reason", "lease_lost")
	case f.doc.course.status == statusArchived:
		return f, forbid("the course is archived").with("reason", "course_archived")
	}
	tv.claim, tv.updatedAt = nil, now
	if in.Status != textDone {
		tv.status, tv.reason = in.Status, *in.Reason
		return f, nil
	}
	tv.status, tv.source, tv.body, tv.pages, tv.model, tv.reason = textDone, sourceAI, *in.Body, *in.Pages, *in.Model, ""
	tv.producedAt, tv.editedBy, tv.editedAt = &now, nil, nil
	tv.revision++
	c.textEvent(f, textDone)
	return f, nil
}

// checkCompletion holds a completion to its shape, as Core does.
func checkCompletion(in completion) *apiError {
	switch in.Status {
	case textDone:
		switch {
		case in.Body == nil || *in.Body == "":
			return invalid("done needs body")
		case len(*in.Body) > maxTextBytes:
			return invalid("the text is longer than 2 MiB").with("reason", "text_too_long")
		case !utf8.ValidString(*in.Body):
			return invalid("the text is not UTF-8")
		case in.Pages == nil || *in.Pages < 1 || *in.Pages > 100000:
			return invalid("done needs pages, from 1 to 100000")
		case in.Model == nil || *in.Model == "" || utf8.RuneCountInString(*in.Model) > 200:
			return invalid("done needs model, 1 to 200 characters")
		case in.Reason != nil:
			return invalid("done takes no reason")
		}
	case textFailed, textSkipped:
		switch {
		case in.Reason == nil || *in.Reason == "" || utf8.RuneCountInString(*in.Reason) > 500:
			return invalid("%s needs reason, 1 to 500 characters", in.Status)
		case in.Body != nil || in.Pages != nil || in.Model != nil:
			return invalid("%s takes no body, pages or model", in.Status)
		}
	default:
		return invalid("status is done, failed or skipped")
	}
	return nil
}

// waitForQueue is what a call of the queue that asked to wait (wait_s),
// and claimed nothing, does: it waits without the lock for a version to be
// queued, and claims again (again) each time one is, until it claims
// something or its time is up. The lock is held on entry and on return;
// ctx is the request's.
func (c *Core) waitForQueue(ctx context.Context, t *toolDef, args []byte, first outcome, again func() outcome) outcome {
	if t.Name != "document_text.queue" || first.Status != actExecuted || ctx == nil {
		return first
	}
	var in struct {
		WaitS int `json:"wait_s"`
	}
	var res struct {
		Claimed []json.RawMessage `json:"claimed"`
	}
	if json.Unmarshal(args, &in) != nil || in.WaitS <= 0 || json.Unmarshal(first.Result, &res) != nil || len(res.Claimed) > 0 {
		return first
	}
	until := time.Now().Add(time.Duration(in.WaitS) * time.Second)
	last := first
	for {
		if c.textNews == nil {
			c.textNews = make(chan struct{})
		}
		news, shutdown := c.textNews, c.shutdown
		c.mu.Unlock()
		stop := waitNews(ctx, news, shutdown, until)
		c.mu.Lock()
		if stop == cancelled {
			return last
		}
		last = again()
		if last.Status != actExecuted || json.Unmarshal(last.Result, &res) != nil || len(res.Claimed) > 0 || stop != woken {
			return last
		}
	}
}

// serviceToken is a new token of the service's, as Core makes them.
func serviceToken() (token, prefix string) {
	var p [8]byte
	var secret [32]byte
	_, _ = rand.Read(p[:])
	_, _ = rand.Read(secret[:])
	prefix = strings.ToLower(prefixEncoding.EncodeToString(p[:]))[:12]
	return "aissvc_" + prefix + "_" + base64.RawURLEncoding.EncodeToString(secret[:]), prefix
}

// IssueServiceToken issues the site's transcription service a credential,
// as a platform administrator's service.issue_credential does; the
// service's actor is made by the first.
func (c *Core) IssueServiceToken(label string) Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.service == nil {
		c.service = &actor{id: newID(), kind: kindService, name: "document_text", status: statusActive}
		c.actors[c.service.id] = c.service
	}
	token, prefix := serviceToken()
	cr := &credential{id: newID(), token: token, prefix: prefix, actor: c.service, issuer: c.system, label: label, createdAt: c.now()}
	c.tokens[token] = cr
	c.serviceCreds[cr.id] = cr
	return Token{Token: token, CredentialID: cr.id, Prefix: prefix}
}

// RevokeServiceToken revokes one of the service's credentials, as
// service.revoke_credential does: its next call is a 401, and what it had
// claimed is back in the queue.
func (c *Core) RevokeServiceToken(credentialID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cr := c.serviceCreds[credentialID]
	if cr == nil {
		return fmt.Errorf("fakecore: RevokeServiceToken: no credential %s", credentialID)
	}
	now := c.now()
	if cr.revokedAt == nil {
		cr.revokedAt = &now
	}
	released := false
	for _, co := range c.courses {
		for _, d := range co.documents {
			for _, f := range d.files {
				if tv := f.text; tv != nil && tv.status == textWorking && tv.claim != nil && tv.claim.cred == cr {
					tv.status, tv.claim, tv.updatedAt = textPending, nil, now
					released = true
				}
			}
		}
	}
	if released {
		c.queued()
	}
	return nil
}

// TextRecord is a file's text as the fake holds it, for assertions.
type TextRecord struct {
	Status, Source, Model, Reason, Body string
	Pages, Revision, Attempts           int
	Backfill                            bool
	// Claimed is whether a claim holds it now.
	Claimed bool
}

// Text is the text of the first file of the document's version, and
// whether it has one.
func (c *Core) Text(documentID string) (TextRecord, bool) { return c.FileText(documentID, "") }

// FileText is the text of the file fileID of the document's version (its
// first for ""), and whether it has one.
func (c *Core) FileText(documentID, fileID string) (TextRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.textOf(documentID, fileID)
	if f == nil {
		return TextRecord{}, false
	}
	tv := f.text
	return TextRecord{Status: tv.status, Source: tv.source, Model: tv.model, Reason: tv.reason, Body: tv.body, Pages: tv.pages,
		Revision: tv.revision, Attempts: tv.attempts, Backfill: tv.backfill,
		Claimed: tv.claim != nil && tv.claim.expires.After(c.now()) && !tv.claim.cred.revoked()}, true
}

// textOf is the file fileID (the first for "") of the document's version,
// when it has a text version; nil otherwise. Called with the lock held.
func (c *Core) textOf(documentID, fileID string) *versionFile {
	doc := c.documentByID(documentID)
	if doc == nil {
		return nil
	}
	f := doc.first()
	if fileID != "" {
		f = doc.file(fileID)
	}
	if f == nil || f.text == nil {
		return nil
	}
	return f
}

// documentByID is the document of id in any course, nil for none. Called
// with the lock held.
func (c *Core) documentByID(id string) *document {
	for _, co := range c.courses {
		for _, d := range co.documents {
			if d.id == id {
				return d
			}
		}
	}
	return nil
}

// errNoText is a document with no text version.
var errNoText = errors.New("fakecore: the document's version has no text version")

// EditText is EditFileText of the version's first file.
func (c *Core) EditText(documentID, memberID, body string) error {
	return c.EditFileText(documentID, "", memberID, body)
}

// EditFileText is a member of staff, the course's instructor by the seat
// memberID, writing the text of the file fileID of the document's version
// (its first for ""), as document.text_update does: done, theirs (source
// staff), whatever it was, a transcription under way refused when it
// finishes.
func (c *Core) EditFileText(documentID, fileID, memberID, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.textOf(documentID, fileID)
	if f == nil {
		return errNoText
	}
	m := c.members[memberID]
	if m == nil {
		return fmt.Errorf("fakecore: EditText: no member %s", memberID)
	}
	now := c.now()
	tv := f.text
	tv.status, tv.source, tv.body, tv.reason, tv.editedBy, tv.editedAt, tv.updatedAt = textDone, sourceStaff, body, "", m, &now, now
	tv.revision++
	c.textEvent(f, textDone)
	return nil
}

// Retranscribe is staff asking for the text of the first file of the
// document's version again, as document.text_retranscribe with
// discard_edit does: pending, its attempts from none, its text and any
// claim gone.
func (c *Core) Retranscribe(documentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.textOf(documentID, "")
	if f == nil {
		return errNoText
	}
	now := c.now()
	tv := f.text
	had := tv.body != ""
	*tv = textVersion{status: textPending, revision: tv.revision + 1, updatedAt: now, queuedAt: now}
	if had {
		c.textEvent(f, textPending)
	}
	c.queued()
	return nil
}

// LapseTextClaim is LapseFileTextClaim of the version's first file.
func (c *Core) LapseTextClaim(documentID string) error { return c.LapseFileTextClaim(documentID, "") }

// LapseFileTextClaim makes the claim on the file fileID of the document's
// version (its first for "") lapse now, as its lease running out would:
// the next claim takes it, and the old lease's calls are lease_lost.
func (c *Core) LapseFileTextClaim(documentID, fileID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.textOf(documentID, fileID)
	if f == nil || f.text.claim == nil {
		return errors.New("fakecore: LapseTextClaim: no claim")
	}
	f.text.claim.expires = c.now().Add(-time.Second)
	c.queued()
	return nil
}

type documentTextIn struct {
	inCourse
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty"`
	FileID     *uuid.UUID `json:"file_id,omitempty"`
	Part       *int       `json:"part,omitempty"`
}

// textParts cuts a text into parts of at most textPartBytes, as Core
// does: before a line that begins "## " (a page's heading) where that
// leaves the part at least half full, else after a whole line, else on a
// character's boundary.
func textParts(s string) []string {
	var out []string
	for len(s) > textPartBytes {
		cut := -1
		for i := textPartBytes; i >= textPartBytes/2; i-- {
			if strings.HasPrefix(s[i:], "## ") && s[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut < 0 {
			if i := strings.LastIndexByte(s[:textPartBytes], '\n'); i > 0 {
				cut = i + 1
			}
		}
		if cut < 0 {
			cut = textPartBytes
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

// documentText is Core's document.text: the text of a file of a version
// (file_id; its first for none), a part at a time, to whoever may read the
// version.
func documentText() *impl {
	return define(spec[documentTextIn]{
		gate: gate{any: true, perms: []string{permDocumentRead, permRubricRead}},
		resolve: func(c *Core, co *course, in documentTextIn) (target, error) {
			doc := c.findDocument(co, in.DocumentID)
			if doc == nil {
				return target{}, missing("no such document in this course")
			}
			return target{typ: "document", id: &doc.id, scope: ownerScope(doc), perms: []string{readPerm(doc.kind)}}, nil
		},
		query: func(c *Core, rc *readCtx, in documentTextIn) (any, error) {
			doc := c.findDocument(rc.course, in.DocumentID)
			if hiddenDraft(doc, rc.member) || withheld(doc, rc.member) || in.VersionID != nil && in.VersionID.String() != doc.versionID {
				return nil, missing("no such version of this document")
			}
			f := doc.first()
			if in.FileID != nil {
				if f = doc.file(in.FileID.String()); f == nil {
					return nil, missing("no such file of this version")
				}
			}
			if f == nil || f.text == nil {
				return nil, missing("the version has no text version").with("reason", "no_text")
			}
			tv := f.text
			out := struct {
				DocumentID string    `json:"document_id"`
				VersionID  string    `json:"version_id"`
				Seq        int       `json:"seq"`
				Published  bool      `json:"published"`
				FileID     string    `json:"file_id"`
				Position   int       `json:"position"`
				Filename   string    `json:"filename"`
				Text       *textView `json:"text"`
				Part       *int      `json:"part,omitempty"`
				Parts      int       `json:"parts"`
			}{DocumentID: doc.id, VersionID: doc.versionID, Seq: 1, Published: !doc.draft, FileID: f.id, Position: f.position,
				Filename: f.filename, Text: tv.view(false)}
			if tv.status != textDone {
				return c.older(out, "file_id", "position", "filename"), nil
			}
			parts := textParts(tv.body)
			k := 1
			if in.Part != nil {
				k = *in.Part
			}
			if k < 1 || k > len(parts) {
				return nil, invalid("part %d is past the last, %d", k, len(parts)).with("parts", len(parts))
			}
			body := parts[k-1]
			out.Text.Body, out.Part, out.Parts = &body, &k, len(parts)
			return c.older(out, "file_id", "position", "filename"), nil
		},
	})
}
