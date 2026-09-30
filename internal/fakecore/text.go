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

// Text versions (AIShie-Core #43): a version of a course's material,
// instructions or rubric that has a file has a text version, queued
// (pending) as the version is added; the site's transcription service
// claims it from Core's queue with a credential of its own (kind service),
// renews its claim while it works, and completes it: done with the
// Markdown, or failed or skipped with why. Staff may write it themselves
// (EditText), which no transcription writes over, or have it transcribed
// again (Retranscribe). document_get shows it beside the version,
// document_text reads it in parts, and the course's feed says when one is
// done or discarded (document.text_updated, …), never with the text.
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

// textEvent files the course's news of doc's text: its type by what the
// document is and whether the version is published.
func (c *Core) textEvent(doc *document, status string) {
	typ := "document.text_updated"
	switch {
	case doc.draft:
		typ = "document.draft_text_updated"
	case doc.kind == kindRubric:
		typ = "document.rubric_text_updated"
	}
	tv := doc.text
	payload := map[string]any{"kind": doc.kind, "version_id": doc.versionID, "status": status, "revision": tv.revision}
	if status == textDone {
		payload["source"] = tv.source
	}
	id := newID()
	c.flush([]*event{{typ: typ, course: doc.course, actionID: &id, subjectType: "document", subjectID: &doc.id, payload: mustJSON(payload)}})
}

// textDocument is the document whose version is versionID, nil for none.
// Called with the lock held.
func (c *Core) textDocument(versionID string) *document {
	for _, co := range c.courses {
		for _, d := range co.documents {
			if d.versionID == versionID && d.text != nil {
				return d
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
		}
		_ = json.Unmarshal(raw, &in)
		doc, e := c.claimed(in.VersionID, in.LeaseID, now, false)
		if e != nil {
			return errorOutcome(e)
		}
		return executed(c.fileOf(doc, base, now))
	case "document_text.renew":
		var in struct {
			VersionID string `json:"version_id"`
			LeaseID   string `json:"lease_id"`
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
		doc, e := c.claimed(in.VersionID, in.LeaseID, now, true)
		if e != nil {
			return errorOutcome(e)
		}
		doc.text.claim.expires = now.Add(time.Duration(lease) * time.Second)
		doc.text.updatedAt = now
		return executed(map[string]any{"lease_expires_at": doc.text.claim.expires})
	}
	return c.complete(caller, t, raw, key, now)
}

// executed is an executed read's or ephemeral write's outcome.
func executed(v any) outcome {
	return outcome{Status: actExecuted, Result: mustJSON(v)}
}

// claim claims up to n versions waiting for cred, for lease: uploads
// first, those waiting longest first, then the backfill, newest first; a
// claim that lapsed is waiting again, and one that lapsed
// maxTextAttempts times fails. Nothing of an archived course is handed
// out. Called with the lock held.
func (c *Core) claim(cred *credential, n int, lease time.Duration, now time.Time, base string) []map[string]any {
	var waiting []*document
	for _, co := range c.courses {
		if co.status == statusArchived {
			continue
		}
		for _, d := range co.documents {
			tv := d.text
			if tv == nil {
				continue
			}
			lapsed := tv.status == textWorking && tv.claim != nil && (!tv.claim.expires.After(now) || tv.claim.cred.revoked())
			if lapsed && tv.attempts >= maxTextAttempts {
				tv.status, tv.reason, tv.claim, tv.updatedAt = textFailed, "attempts_exhausted", nil, now
				continue
			}
			if tv.status == textPending || lapsed {
				waiting = append(waiting, d)
			}
		}
	}
	slices.SortStableFunc(waiting, func(a, b *document) int {
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
	for _, d := range waiting[:min(n, len(waiting))] {
		tv := d.text
		tv.attempts++
		tv.status, tv.updatedAt = textWorking, now
		tv.claim = &textClaim{leaseID: uuid.NewString(), expires: now.Add(lease), cred: cred}
		ct := ""
		if d.contentType != nil {
			ct = *d.contentType
		}
		out = append(out, map[string]any{
			"version_id": d.versionID, "document_id": d.id, "course_id": d.course.id, "lease_id": tv.claim.leaseID,
			"lease_expires_at": tv.claim.expires, "attempt": tv.attempts, "backfill": tv.backfill, "content_type": ct,
			"byte_size": len(d.file), "download_url": base + blobPath + d.fileToken, "download_expires_at": now.Add(downloadTTL),
		})
	}
	return out
}

// claimed is the document whose version versionID is claimed under
// leaseID: not found when there is none; for a renewal, edited_by_staff
// when staff wrote the text meanwhile; lease_lost when the claim does not
// hold; course_archived when its course was archived. Called with the lock
// held.
func (c *Core) claimed(versionID, leaseID string, now time.Time, renew bool) (*document, *apiError) {
	doc := c.textDocument(versionID)
	switch {
	case doc == nil:
		return nil, missing("no such text version")
	case renew && doc.text.source == sourceStaff && doc.text.claim != nil && doc.text.claim.leaseID == leaseID:
		return nil, conflicts("staff have written the text; it is theirs, and is not written over").with("reason", "edited_by_staff")
	case !doc.text.holds(leaseID, now):
		return nil, conflicts("the claim no longer holds: the version was claimed again, or sent back to the queue").with("reason", "lease_lost")
	case doc.course.status == statusArchived:
		return nil, forbid("the course is archived").with("reason", "course_archived")
	}
	return doc, nil
}

// fileOf is document_text.file's result for doc's claim.
func (c *Core) fileOf(doc *document, base string, now time.Time) map[string]any {
	ct := ""
	if doc.contentType != nil {
		ct = *doc.contentType
	}
	return map[string]any{"version_id": doc.versionID, "content_type": ct, "byte_size": len(doc.file),
		"download_url": base + blobPath + doc.fileToken, "download_expires_at": now.Add(downloadTTL), "lease_expires_at": doc.text.claim.expires}
}

// completion is document_text.complete's input.
type completion struct {
	VersionID string  `json:"version_id"`
	LeaseID   string  `json:"lease_id"`
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
	doc, e := c.completeText(in, now)
	if doc == nil && e != nil && e.Code == codeNotFound {
		// A version not there is a call never attempted, as Core's is.
		return errorOutcome(e)
	}
	c.recordAction(act)
	if e != nil {
		act.status, act.result = actFailed, errorResult(e)
		return outcome{Status: actFailed, ActionID: act.id, ReviewState: reviewNone, Error: e}
	}
	res := mustJSON(map[string]any{"version_id": in.VersionID, "status": in.Status, "revision": doc.text.revision})
	act.executedAt, act.result = &now, res
	return outcome{Status: actExecuted, ActionID: act.id, ReviewState: reviewNone, Result: res}
}

// completeText writes a completion into the version's text, or says why
// not. Called with the lock held.
func (c *Core) completeText(in completion, now time.Time) (*document, *apiError) {
	doc := c.textDocument(in.VersionID)
	if doc == nil {
		return nil, missing("no such text version")
	}
	tv := doc.text
	if e := checkCompletion(in); e != nil {
		return doc, e
	}
	switch {
	case tv.source == sourceStaff && tv.claim != nil && tv.claim.leaseID == in.LeaseID:
		return doc, conflicts("staff have written the text; it is theirs, and is not written over").with("reason", "edited_by_staff")
	case !tv.holds(in.LeaseID, now):
		return doc, conflicts("the claim no longer holds: the version was claimed again, or sent back to the queue").with("reason", "lease_lost")
	case doc.course.status == statusArchived:
		return doc, forbid("the course is archived").with("reason", "course_archived")
	}
	tv.claim, tv.updatedAt = nil, now
	if in.Status != textDone {
		tv.status, tv.reason = in.Status, *in.Reason
		return doc, nil
	}
	tv.status, tv.source, tv.body, tv.pages, tv.model, tv.reason = textDone, sourceAI, *in.Body, *in.Pages, *in.Model, ""
	tv.producedAt, tv.editedBy, tv.editedAt = &now, nil, nil
	tv.revision++
	c.textEvent(doc, textDone)
	return doc, nil
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
			if tv := d.text; tv != nil && tv.status == textWorking && tv.claim != nil && tv.claim.cred == cr {
				tv.status, tv.claim, tv.updatedAt = textPending, nil, now
				released = true
			}
		}
	}
	if released {
		c.queued()
	}
	return nil
}

// TextRecord is a version's text as the fake holds it, for assertions.
type TextRecord struct {
	Status, Source, Model, Reason, Body string
	Pages, Revision, Attempts           int
	Backfill                            bool
	// Claimed is whether a claim holds it now.
	Claimed bool
}

// Text is the text of the document's version, and whether it has one.
func (c *Core) Text(documentID string) (TextRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc := c.documentByID(documentID)
	if doc == nil || doc.text == nil {
		return TextRecord{}, false
	}
	tv := doc.text
	return TextRecord{Status: tv.status, Source: tv.source, Model: tv.model, Reason: tv.reason, Body: tv.body, Pages: tv.pages,
		Revision: tv.revision, Attempts: tv.attempts, Backfill: tv.backfill,
		Claimed: tv.claim != nil && tv.claim.expires.After(c.now()) && !tv.claim.cred.revoked()}, true
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

// EditText is a member of staff, the course's instructor by the seat
// memberID, writing the text of the document's version, as
// document.text_update does: done, theirs (source staff), whatever it was,
// a transcription under way refused when it finishes.
func (c *Core) EditText(documentID, memberID, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc := c.documentByID(documentID)
	if doc == nil || doc.text == nil {
		return errNoText
	}
	m := c.members[memberID]
	if m == nil {
		return fmt.Errorf("fakecore: EditText: no member %s", memberID)
	}
	now := c.now()
	tv := doc.text
	tv.status, tv.source, tv.body, tv.reason, tv.editedBy, tv.editedAt, tv.updatedAt = textDone, sourceStaff, body, "", m, &now, now
	tv.revision++
	c.textEvent(doc, textDone)
	return nil
}

// Retranscribe is staff asking for the text of the document's version
// again, as document.text_retranscribe with discard_edit does: pending,
// its attempts from none, its text and any claim gone.
func (c *Core) Retranscribe(documentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc := c.documentByID(documentID)
	if doc == nil || doc.text == nil {
		return errNoText
	}
	now := c.now()
	tv := doc.text
	had := tv.body != ""
	*tv = textVersion{status: textPending, revision: tv.revision + 1, updatedAt: now, queuedAt: now}
	if had {
		c.textEvent(doc, textPending)
	}
	c.queued()
	return nil
}

// LapseTextClaim makes the claim on the document's version lapse now, as
// its lease running out would: the next claim takes it, and the old
// lease's calls are lease_lost.
func (c *Core) LapseTextClaim(documentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc := c.documentByID(documentID)
	if doc == nil || doc.text == nil || doc.text.claim == nil {
		return errors.New("fakecore: LapseTextClaim: no claim")
	}
	doc.text.claim.expires = c.now().Add(-time.Second)
	c.queued()
	return nil
}

type documentTextIn struct {
	inCourse
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty"`
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

// documentText is Core's document.text: a version's text, a part at a
// time, to whoever may read the version.
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
			if doc.text == nil {
				return nil, missing("the version has no text version").with("reason", "no_text")
			}
			tv := doc.text
			out := struct {
				DocumentID string    `json:"document_id"`
				VersionID  string    `json:"version_id"`
				Seq        int       `json:"seq"`
				Published  bool      `json:"published"`
				Text       *textView `json:"text"`
				Part       *int      `json:"part,omitempty"`
				Parts      int       `json:"parts"`
			}{DocumentID: doc.id, VersionID: doc.versionID, Seq: 1, Published: !doc.draft, Text: tv.view(false)}
			if tv.status != textDone {
				return out, nil
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
			return out, nil
		},
	})
}
