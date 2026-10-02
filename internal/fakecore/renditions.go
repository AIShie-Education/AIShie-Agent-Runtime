package fakecore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PDF renditions (AIShie-Core's migration 0026, its docs/schema.md §2.4
// Renditions): every Office or OpenDocument file Core keeps, of a
// document's version of any kind or carried by a message, is queued for a
// PDF rendition as it is recorded, which the site's agent runtime makes
// once. The runtime claims what waits (agent_runtime.rendition_claim, a
// long poll), gets the file, renews its claim, asks for somewhere to
// upload the PDF (rendition_upload_url), PUTs it to the fake itself, and
// completes it: done, naming the upload and its pages, which Core holds to
// being a PDF no larger than renditionMaxBytes; or failed or skipped, with
// one of the runtime's reasons. A claim holds while it is the rendition's
// latest, lapsed or not; a lapsed one is claimed again, and one claimed
// renditionAttempts times and not finished fails (attempts_exhausted). A
// revoked credential's claims go back to the queue, uncounted.
//
// Whoever may read the file reads its rendition: document_get and
// document_file give each file's, with a URL that shows the PDF once it is
// done; document_versions says where each stands; conversation_attachment
// gives a message's file's with its URL, conversation_messages where each
// stands. Sending one back (document.rendition_retry,
// conversation.rendition_retry) is not carried out here.

// Bounds of the renditions, as Core has them.
const (
	renditionAttempts = 5
	renditionMaxBytes = 100 << 20
	renditionMaxPages = 100000
	pdfMagic          = "%PDF-"
)

// renditionReasons are why the runtime fails or skips a rendition.
var renditionReasons = []string{"password_protected", "timeout", "conversion_failed", "too_large", "unsupported"}

// Where a rendition stands.
const (
	renditionQueued  = "queued"
	renditionClaimed = "claimed"
	renditionDone    = "done"
	renditionFailed  = "failed"
	renditionSkipped = "skipped"
)

// rendition is a file's PDF rendition: of a version's file or a message's.
type rendition struct {
	id     string
	course *course
	file   *versionFile
	att    *attachment
	status string
	reason string
	// attempts are the claims since it was queued; backfill says the
	// migration queued it, and queuedAt when it was.
	attempts int
	backfill bool
	queuedAt time.Time
	// claim is the latest claim, nil once it is finished or sent back.
	claim *textClaim
	// pdf is the PDF, done, and pages its pages; token serves it.
	pdf   []byte
	pages int
	token string
}

// renditionUpload is an upload URL handed out for a claim's PDF.
type renditionUpload struct {
	token, putToken string
	rend            *rendition
	leaseID         string
	expires         time.Time
	// data is what was PUT, nil before, and again once Core refused it;
	// put says a PUT was taken, which the URL takes once.
	data []byte
	put  bool
}

// renditionView is a rendition as its file's readers are shown it.
type renditionView struct {
	State             string     `json:"state"`
	PageCount         *int       `json:"page_count,omitempty"`
	ByteSize          *int64     `json:"byte_size,omitempty"`
	Reason            *string    `json:"reason,omitempty"`
	DownloadURL       *string    `json:"download_url,omitempty"`
	DownloadExpiresAt *time.Time `json:"download_expires_at,omitempty"`
}

// renditionExts and renditionTypes are Core's table of the files
// converted (file_rendition_convertible): a name ending in one of the
// extensions, and a declared type that is one of the types or says nothing
// in particular of the bytes.
var (
	renditionExts = []string{"doc", "dot", "docx", "docm", "dotx", "xls", "xlt", "xlsx", "xlsm", "xltx", "ppt", "pps", "pot", "pptx",
		"pptm", "ppsx", "potx", "odt", "ods", "odp", "odg", "rtf"}
	renditionTypes = []string{"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.ms-word.document.macroenabled.12",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.template",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-excel.sheet.macroenabled.12",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.template",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.ms-powerpoint.presentation.macroenabled.12",
		"application/vnd.openxmlformats-officedocument.presentationml.slideshow",
		"application/vnd.openxmlformats-officedocument.presentationml.template",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.oasis.opendocument.spreadsheet",
		"application/vnd.oasis.opendocument.presentation",
		"application/vnd.oasis.opendocument.graphics",
		"application/rtf", "text/rtf",
		"application/octet-stream", "application/zip", "application/x-zip-compressed", "application/vnd.ms-office"}
)

// convertible reports whether Core converts a file of that name and
// declared type, as file_rendition_convertible has it.
func convertible(filename, contentType string) bool {
	i := strings.LastIndexByte(filename, '.')
	if i < 0 {
		return false
	}
	ext := filename[i+1:]
	if ext == "" || strings.ContainsAny(ext, `/\`) {
		return false
	}
	mt, _, _ := strings.Cut(contentType, ";")
	return slices.Contains(renditionExts, strings.ToLower(ext)) && slices.Contains(renditionTypes, strings.ToLower(strings.Trim(mt, " ")))
}

// withoutRenditions is the catalogue raw without the renditions' tools, as
// a Core from before them serves it (Options.WithoutRenditions): the
// agent runtime's five and the two that send one back.
func withoutRenditions(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	kept := tools[:0]
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		name, _ := tool["name"].(string)
		if !strings.HasPrefix(name, scopeAgentRuntime+".rendition_") && name != "document.rendition_retry" &&
			name != "conversation.rendition_retry" {
			kept = append(kept, t)
		}
	}
	if len(tools)-len(kept) != 7 {
		return nil, errors.New("fakecore: the catalogue has not the renditions' seven tools to take out")
	}
	doc["tools"] = kept
	return json.Marshal(doc)
}

// renditions reports whether this Core makes renditions: one from before
// them, or before an agent's hosting, does not.
func (c *Core) renditions() bool { return !c.opts.WithoutRenditions && !c.opts.WithoutHosting }

// queueRendition queues a rendition of a version's file f, or of a
// message's file a, in co, when Core converts it. Called with the lock
// held; the caller wakes the claims that wait (queued).
func (c *Core) queueRendition(co *course, f *versionFile, a *attachment, now time.Time) bool {
	name, ct := "", ""
	switch {
	case f != nil:
		name, ct = f.filename, f.contentType
	case a != nil:
		name, ct = a.filename, a.contentType
	}
	if !c.renditions() || !convertible(name, ct) {
		return false
	}
	r := &rendition{id: newID(), course: co, file: f, att: a, status: renditionQueued, queuedAt: now}
	if f != nil {
		f.rend = r
	} else {
		a.rend = r
	}
	c.rends[r.id] = r
	return true
}

// view is r as its file's readers are shown it: where it stands, its pages
// and size done, why failed or skipped; withURL, a URL at base that shows
// the PDF, done.
func (r *rendition) view(withURL bool, base string, now time.Time) *renditionView {
	if r == nil {
		return nil
	}
	v := &renditionView{State: r.status}
	switch r.status {
	case renditionDone:
		pages, size := r.pages, int64(len(r.pdf))
		v.PageCount, v.ByteSize = &pages, &size
		if withURL {
			u, expires := base+blobPath+r.token, now.Add(downloadTTL)
			v.DownloadURL, v.DownloadExpiresAt = &u, &expires
		}
	case renditionFailed, renditionSkipped:
		v.Reason = &r.reason
	}
	return v
}

// stateOf is r as a list of versions shows it: where it stands alone.
func (r *rendition) stateOf() *renditionView {
	if r == nil {
		return nil
	}
	return &renditionView{State: r.status}
}

// filename, contentType and data are r's file's.
func (r *rendition) filename() string {
	if r.file != nil {
		return r.file.filename
	}
	return r.att.filename
}

func (r *rendition) contentType() string {
	if r.file != nil {
		return r.file.contentType
	}
	return r.att.contentType
}

func (r *rendition) data() []byte {
	if r.file != nil {
		return r.file.data
	}
	return r.att.data
}

func (r *rendition) checksum() string {
	if r.file != nil {
		return r.file.checksum()
	}
	return r.att.checksum
}

// fileURL is a short-lived URL at base for r's file.
func (c *Core) fileURL(r *rendition, base string, now time.Time) string {
	if r.file != nil {
		return base + blobPath + r.file.token
	}
	token := fileToken()
	c.downloads[token] = download{a: r.att, expires: now.Add(downloadTTL)}
	return base + blobPath + token
}

// holdsRendition reports whether leaseID is r's latest claim, which holds
// until another claims r or it is finished, lapsed or not.
func (r *rendition) holds(leaseID string) bool {
	return r.status == renditionClaimed && r.claim != nil && r.claim.leaseID == leaseID
}

// The renditions' refusals, as Core words them.
var (
	errNoRendition = missing("no such rendition")
	errRendLost    = conflicts("the claim no longer holds: the file was claimed again, or sent back to the queue").
			with("reason", "lease_lost")
	errNotPDF = precondition("what was uploaded is not a PDF: it does not begin %q; upload the PDF to a new URL", pdfMagic).
			with("reason", "not_a_pdf")
	errNotYourRenditionUpload = forbid("that upload was issued for another claim").with("reason", "not_your_upload")
)

// leaseOf is lease_s as a duration: Core's default for none, and a
// refusal outside 60 to 3600 seconds.
func leaseOf(s *int) (time.Duration, *apiError) {
	if s == nil || *s == 0 {
		return defaultLeaseS * time.Second, nil
	}
	if *s < 60 || *s > 3600 {
		return 0, invalid("lease_s is 60 to 3600")
	}
	return time.Duration(*s) * time.Second, nil
}

// renditionIn is what the renditions' calls about one rendition name.
type renditionIn struct {
	RenditionID string  `json:"rendition_id"`
	LeaseID     string  `json:"lease_id"`
	LeaseS      *int    `json:"lease_s"`
	Max         *int    `json:"max"`
	Status      string  `json:"status"`
	UploadToken *string `json:"upload_token"`
	PageCount   *int    `json:"page_count"`
	Reason      *string `json:"reason"`
}

// invokeRendition carries out one of the agent runtime's renditions'
// tools, called by the service caller with the credential cred. Called
// with the lock held.
func (c *Core) invokeRendition(caller *actor, cred *credential, t *toolDef, raw []byte, key, base string, now time.Time) outcome {
	var in renditionIn
	_ = json.Unmarshal(raw, &in)
	if t.Name == "agent_runtime.rendition_claim" {
		n := 1
		if in.Max != nil && *in.Max != 0 {
			n = *in.Max
		}
		if n < 1 || n > 10 {
			return errorOutcome(invalid("max is 1 to 10"))
		}
		lease, e := leaseOf(in.LeaseS)
		if e != nil {
			return errorOutcome(e)
		}
		return executed(map[string]any{"claimed": c.claimRenditions(cred, n, lease, now, base)})
	}
	if t.write {
		return c.completeRendition(caller, t, raw, key, in, now)
	}
	r := c.rends[strings.ToLower(in.RenditionID)]
	if r == nil {
		return errorOutcome(errNoRendition)
	}
	switch t.Name {
	case "agent_runtime.rendition_renew":
		lease, e := leaseOf(in.LeaseS)
		switch {
		case e != nil:
			return errorOutcome(e)
		case !r.holds(in.LeaseID):
			return errorOutcome(errRendLost)
		}
		r.claim.expires = now.Add(lease)
		return executed(map[string]any{"lease_expires_at": r.claim.expires})
	case "agent_runtime.rendition_file":
		if !r.holds(in.LeaseID) {
			return errorOutcome(errRendLost)
		}
		out := map[string]any{"rendition_id": r.id, "filename": r.filename(), "content_type": r.contentType(),
			"byte_size": len(r.data()), "checksum": r.checksum(), "download_url": c.fileURL(r, base, now),
			"download_expires_at": now.Add(downloadTTL), "lease_expires_at": r.claim.expires}
		return executed(out)
	case "agent_runtime.rendition_upload_url":
		if !r.holds(in.LeaseID) {
			return errorOutcome(errRendLost)
		}
		up := &renditionUpload{token: "fakerendition." + fileToken(), putToken: fileToken(), rend: r, leaseID: in.LeaseID,
			expires: now.Add(uploadWindow)}
		c.rendUploads[up.token] = up
		c.rendPuts[up.putToken] = up
		return executed(map[string]any{"upload_url": base + blobPath + up.putToken, "headers": map[string]string{"Content-Type": "application/pdf"},
			"upload_token": up.token, "expires_at": up.expires, "max_bytes": renditionMaxBytes})
	}
	return errorOutcome(missing("no tool %s here", t.Name))
}

// claimRenditions claims up to n renditions waiting for cred, for lease:
// what was queued as its file was recorded first, the oldest first, then
// the backfill, the newest first; a claim that lapsed is waiting again,
// and one claimed renditionAttempts times and not finished fails. Archived
// courses' files are claimed too. Called with the lock held.
func (c *Core) claimRenditions(cred *credential, n int, lease time.Duration, now time.Time, base string) []map[string]any {
	var waiting []*rendition
	for _, r := range c.rends {
		lapsed := r.status == renditionClaimed && r.claim != nil && !r.claim.expires.After(now)
		if r.attempts >= renditionAttempts && (r.status == renditionQueued || lapsed) {
			r.status, r.reason, r.claim = renditionFailed, "attempts_exhausted", nil
			continue
		}
		if r.status == renditionQueued || lapsed {
			waiting = append(waiting, r)
		}
	}
	slices.SortFunc(waiting, func(a, b *rendition) int {
		switch {
		case a.backfill != b.backfill:
			if a.backfill {
				return 1
			}
			return -1
		case !a.queuedAt.Equal(b.queuedAt):
			if a.backfill {
				return b.queuedAt.Compare(a.queuedAt)
			}
			return a.queuedAt.Compare(b.queuedAt)
		}
		return strings.Compare(a.id, b.id)
	})
	out := []map[string]any{}
	for _, r := range waiting[:min(n, len(waiting))] {
		r.attempts++
		r.status = renditionClaimed
		r.claim = &textClaim{leaseID: uuid.NewString(), expires: now.Add(lease), cred: cred}
		claimed := map[string]any{"rendition_id": r.id, "lease_id": r.claim.leaseID, "lease_expires_at": r.claim.expires,
			"attempt": r.attempts, "backfill": r.backfill, "course_id": r.course.id, "filename": r.filename(),
			"content_type": r.contentType(), "byte_size": len(r.data()), "checksum": r.checksum(),
			"download_url": c.fileURL(r, base, now), "download_expires_at": now.Add(downloadTTL), "max_bytes": renditionMaxBytes}
		if r.file != nil {
			claimed["source"], claimed["file_id"] = "document_file", r.file.id
		} else {
			claimed["source"], claimed["attachment_id"] = "attachment", r.att.id
		}
		out = append(out, claimed)
	}
	return out
}

// checkRenditionCompletion holds a completion to its shape, as Core's
// check does: invokeService asks it before anything else.
func checkRenditionCompletion(in renditionIn) *apiError {
	switch in.Status {
	case renditionDone:
		switch {
		case in.UploadToken == nil || *in.UploadToken == "":
			return invalid("done needs upload_token")
		case in.PageCount == nil || *in.PageCount < 1 || *in.PageCount > renditionMaxPages:
			return invalid("done needs page_count, 1 to %d", renditionMaxPages)
		case in.Reason != nil:
			return invalid("done gives no reason")
		}
		return nil
	case renditionFailed, renditionSkipped:
		if in.UploadToken != nil || in.PageCount != nil {
			return invalid("%s gives no upload_token or page_count", in.Status)
		}
		if in.Reason == nil || !slices.Contains(renditionReasons, *in.Reason) {
			return invalid("%s needs a reason: %s", in.Status, strings.Join(renditionReasons, ", ")).with("field", "reason")
		}
		return nil
	}
	return invalid("status must be done, failed or skipped")
}

// completeRendition is agent_runtime.rendition_complete: a write of the
// service's, recorded and replayed under its key, the upload token kept out
// of its record; one about no rendition is never attempted.
func (c *Core) completeRendition(caller *actor, t *toolDef, raw []byte, key string, in renditionIn, now time.Time) outcome {
	if err := checkKey(t, key); err != nil {
		return c.failure(err)
	}
	canonical, err := canonicalize(raw)
	if err != nil {
		return errorOutcome(invalid("%v", err))
	}
	hash := payloadHash(t.Name, canonical)
	if existing := c.keys[actorKey{caller.id, key}]; existing != nil {
		out, err := replay(existing, hash, "")
		if err != nil {
			return c.failure(err)
		}
		return out
	}
	r := c.rends[strings.ToLower(in.RenditionID)]
	if r == nil {
		return errorOutcome(errNoRendition)
	}
	act := &action{id: newID(), actor: caller, actionType: t.Name, targetType: "file_rendition", targetID: &r.id,
		payload: withoutSecret(canonical), hash: hash, key: key, authz: autonomous, status: actExecuted, reviewState: reviewNone, createdAt: now}
	c.recordAction(act)
	failed := func(e *apiError) outcome {
		act.status, act.result = actFailed, errorResult(e)
		return outcome{Status: actFailed, ActionID: act.id, ReviewState: reviewNone, Error: e}
	}
	if !r.holds(in.LeaseID) {
		return failed(errRendLost)
	}
	res := map[string]any{"rendition_id": r.id, "state": in.Status}
	if in.Status != renditionDone {
		r.status, r.reason, r.claim = in.Status, *in.Reason, nil
	} else {
		pdf, e := c.takePDF(r, in.LeaseID, *in.UploadToken)
		if e != nil {
			return failed(e)
		}
		sum := sha256.Sum256(pdf)
		r.status, r.reason, r.claim, r.pdf, r.pages, r.token = renditionDone, "", nil, pdf, *in.PageCount, fileToken()
		c.rendPDFs[r.token] = r
		res["byte_size"], res["checksum"] = len(pdf), "sha256:"+hex.EncodeToString(sum[:])
	}
	out := mustJSON(res)
	act.executedAt, act.result = &now, out
	return outcome{Status: actExecuted, ActionID: act.id, ReviewState: reviewNone, Result: out}
}

// withoutSecret is a completion's canonical arguments without its upload
// token, which Core keeps out of the action's record.
func withoutSecret(canonical []byte) []byte {
	var m map[string]any
	if json.Unmarshal(canonical, &m) != nil {
		return canonical
	}
	delete(m, "upload_token")
	b, err := canonicalize(mustJSON(m))
	if err != nil {
		return canonical
	}
	return b
}

// takePDF is the PDF the upload token names, PUT for this claim of r: one
// that is not a PDF, or larger than Core takes, is deleted and refused,
// the claim held. Called with the lock held.
func (c *Core) takePDF(r *rendition, leaseID, token string) ([]byte, *apiError) {
	up := c.rendUploads[token]
	switch {
	case up == nil:
		return nil, errBadUploadToken
	case up.rend != r || up.leaseID != leaseID:
		return nil, errNotYourRenditionUpload
	case up.data == nil:
		return nil, errNotUploaded
	case len(up.data) > renditionMaxBytes:
		size := len(up.data)
		up.data = nil
		return nil, precondition("the PDF is %d bytes; the largest taken is %d: complete it skipped, too_large", size, renditionMaxBytes).
			with("reason", "rendition_too_large").with("byte_size", size).with("max_bytes", renditionMaxBytes)
	case !bytes.HasPrefix(up.data, []byte(pdfMagic)):
		up.data = nil
		return nil, errNotPDF
	}
	pdf := up.data
	delete(c.rendUploads, token)
	return pdf, nil
}

// putRendition takes a rendition's PDF at its upload URL, once, as Core's
// own disk does: Content-Type application/pdf, at most renditionMaxBytes.
// false when token names no rendition's upload.
func (c *Core) putRendition(w http.ResponseWriter, r *http.Request, token string) bool {
	c.mu.Lock()
	up := c.rendPuts[token]
	now := c.now()
	c.mu.Unlock()
	if up == nil {
		return false
	}
	if now.After(up.expires) {
		writeError(w, forbid("the upload URL is not valid, or has expired"))
		return true
	}
	if got := r.Header.Get("Content-Type"); got != "application/pdf" {
		writeError(w, invalid("this URL takes Content-Type %q, not %q", "application/pdf", got))
		return true
	}
	data, err := readUpTo(r, renditionMaxBytes)
	if err != nil {
		writeError(w, err)
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if up.put {
		writeError(w, conflicts("this URL has been uploaded to already; a file is written once"))
		return true
	}
	up.put, up.data = true, data
	sum := sha256.Sum256(data)
	writeJSON(w, http.StatusOK, map[string]any{"byte_size": len(data), "checksum": "sha256:" + hex.EncodeToString(sum[:])})
	return true
}

// servePDF serves a done rendition's PDF, to be shown where it is opened,
// named as its file with .pdf; false when token names none.
func (c *Core) servePDF(w http.ResponseWriter, token string) bool {
	c.mu.Lock()
	r := c.rendPDFs[token]
	c.mu.Unlock()
	if r == nil {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	h.Set("Content-Length", strconv.Itoa(len(r.pdf)))
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": pdfName(r.filename())}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(r.pdf)
	return true
}

// pdfName is what a file's rendition is called: its name with .pdf in
// place of its extension.
func pdfName(filename string) string {
	if i := strings.LastIndexByte(filename, '.'); i > 0 {
		filename = filename[:i]
	}
	return filename + ".pdf"
}

// releaseRenditions puts back in the queue what the credential cr had
// claimed, those claims not counted, as revoking it does. Called with the
// lock held; it reports whether any went back.
func (c *Core) releaseRenditions(cr *credential) bool {
	released := false
	for _, r := range c.rends {
		if r.status == renditionClaimed && r.claim != nil && r.claim.cred == cr {
			r.status, r.claim, r.attempts = renditionQueued, nil, max(r.attempts-1, 0)
			released = true
		}
	}
	return released
}

// ---------------------------------------------------------------------------
// Test controls
// ---------------------------------------------------------------------------

// RenditionRecord is a file's rendition as the fake holds it, for
// assertions.
type RenditionRecord struct {
	ID, State, Reason string
	Attempts, Pages   int
	Backfill          bool
	// PDF is the PDF, done.
	PDF []byte
	// Claimed is whether a claim holds it now, its lease not run out.
	Claimed bool
}

// Rendition is the rendition of the file fileID, a version's or a
// message's, and whether it has one.
func (c *Core) Rendition(fileID string) (RenditionRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.renditionOf(fileID)
	if r == nil {
		return RenditionRecord{}, false
	}
	return RenditionRecord{ID: r.id, State: r.status, Reason: r.reason, Attempts: r.attempts, Pages: r.pages, Backfill: r.backfill,
		PDF: bytes.Clone(r.pdf), Claimed: r.status == renditionClaimed && r.claim != nil && r.claim.expires.After(c.now())}, true
}

// renditionOf is the rendition of the file fileID, a version's or a
// message's; nil for none. Called with the lock held.
func (c *Core) renditionOf(fileID string) *rendition {
	for _, r := range c.rends {
		if r.file != nil && r.file.id == fileID || r.att != nil && r.att.id == fileID {
			return r
		}
	}
	return nil
}

// LapseRenditionClaim makes the claim on the file fileID's rendition lapse
// now, as its lease running out would: the next claim takes it, which the
// old lease's calls are then refused for (lease_lost).
func (c *Core) LapseRenditionClaim(fileID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.renditionOf(fileID)
	if r == nil || r.claim == nil {
		return errors.New("fakecore: LapseRenditionClaim: no claim")
	}
	r.claim.expires = c.now().Add(-time.Second)
	c.queued()
	return nil
}

// Backfill marks the file fileID's rendition as the migration's backfill
// queued it, at the time it was queued: the queue takes it after every
// file recorded since, the newest of the backfill first.
func (c *Core) Backfill(fileID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.renditionOf(fileID)
	if r == nil {
		return errors.New("fakecore: Backfill: no rendition")
	}
	r.backfill = true
	return nil
}

// RenderFile makes the file fileID's rendition done with pdf, of pages, as
// the runtime completing a claim of it would, whatever it stood at: its
// readers are shown it, and its URL serves the PDF.
func (c *Core) RenderFile(fileID string, pdf []byte, pages int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.renditionOf(fileID)
	if r == nil {
		return errors.New("fakecore: RenderFile: no rendition")
	}
	if r.token != "" {
		delete(c.rendPDFs, r.token)
	}
	r.status, r.reason, r.claim, r.pdf, r.pages, r.token = renditionDone, "", nil, bytes.Clone(pdf), pages, fileToken()
	c.rendPDFs[r.token] = r
	return nil
}
