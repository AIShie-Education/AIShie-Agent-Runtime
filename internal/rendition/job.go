package rendition

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// Bounds of one file's work.
const (
	// MaxFileBytes is the largest file fetched to be converted: a larger
	// one is skipped (too_large) unread.
	MaxFileBytes = 100 << 20
	// jobMargin is how much longer than the conversion's timeout a file's
	// work may take in all: its fetch, its upload and Core told.
	jobMargin = 5 * time.Minute
	// completeTries is how many times a completion is sent, under its
	// key, while Core cannot be reached.
	completeTries = 5
	// uploadTries is how many PDFs are uploaded for one claim, each to a
	// URL of its own, while Core says nothing arrived.
	uploadTries = 2
	// manifestBytes bounds the reading of an OpenDocument file's manifest.
	manifestBytes = 1 << 20
)

// Reasons a file's work is dropped, Core told nothing: its claim lapses,
// and Core gives the file out again, or has done with it.
const (
	DroppedLeaseLost   = "lease_lost"
	DroppedGone        = "gone"
	DroppedInterrupted = "interrupted"
	DroppedCoreRefused = "core_refused"
	DroppedFetch       = "fetch_failed"
	DroppedUpload      = "upload_failed"
	DroppedCredential  = "credential_refused" // #nosec G101 -- a reason's code, not a credential.
	outcomeDropped     = "dropped"
)

// job is the work on one claimed rendition.
type job struct {
	s   *Service
	svc *core.RuntimeService
	c   core.ClaimedRendition
	cl  core.RenditionClaim
	// n is the number in the completion's key: the next after a refusal
	// that leaves the claim held.
	n int
	// bytes and pages are the PDF's, made.
	bytes, pages int
}

// result is what became of a file: done, failed or skipped, which Core is
// told, or dropped, which it is not, with why.
type result struct {
	status, reason string
}

func skipped(reason string) result { return result{status: core.RenditionSkipped, reason: reason} }
func failed(reason string) result  { return result{status: core.RenditionFailed, reason: reason} }
func dropped(reason string) result { return result{status: outcomeDropped, reason: reason} }

// dropCause is why a job's work was dropped part way, as the context's
// cause carries it.
type dropCause struct{ reason string }

func (d dropCause) Error() string { return "renditions: dropped: " + d.reason }

// start works on the claimed rendition c in the background, in one of the
// worker's slots.
func (s *Service) start(ctx context.Context, svc *core.RuntimeService, c core.ClaimedRendition) {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
	if s.o.Metrics != nil {
		s.o.Metrics.RenditionInflight.Inc()
	}
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer func() {
			s.mu.Lock()
			s.inflight--
			close(s.wake)
			s.wake = make(chan struct{})
			s.mu.Unlock()
			if s.o.Metrics != nil {
				s.o.Metrics.RenditionInflight.Dec()
			}
		}()
		j := &job{s: s, svc: svc, c: c, cl: core.ClaimOfRendition(c), n: 1}
		j.run(ctx)
	}()
}

// run is the job: the claim held while the file is fetched, converted and
// its PDF uploaded, Core told what became of it, and the metrics and the
// log line of it.
func (j *job) run(ctx context.Context) {
	s := j.s
	start := s.now()
	ctx, cancel := context.WithTimeoutCause(ctx, s.cfg.Timeout+jobMargin, dropCause{DroppedInterrupted})
	defer cancel()
	ctx, drop := context.WithCancelCause(ctx)
	defer drop(nil)
	holding, stopHold := context.WithCancel(ctx)
	held := make(chan struct{})
	go func() { defer close(held); j.hold(holding, drop) }()

	res := j.work(ctx)
	stopHold()
	<-held
	// Work stopped part way is dropped for what stopped it: the claim
	// lost, or the time up; Core told before it stopped stands.
	var dc dropCause
	if res.status == outcomeDropped && errors.As(context.Cause(ctx), &dc) {
		res = dropped(dc.reason)
	}
	j.finish(res, start)
}

// hold renews the claim every half of its lease until ctx ends; Core
// saying the work is no longer the runtime's to do (the claim lost, the
// file purged, the credential refused) drops it.
func (j *job) hold(ctx context.Context, drop context.CancelCauseFunc) {
	s := j.s
	t := time.NewTicker(s.t.Renew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := j.svc.RenewRendition(ctx, j.cl, s.cfg.Lease)
		if ctx.Err() != nil {
			return
		}
		if reason, stop := stopReason(err); stop {
			drop(dropCause{reason})
			return
		}
		if err != nil {
			s.log.Warn("a rendition's claim could not be renewed; it is tried again", "rendition", j.c.RenditionID, "err", errText(err))
		}
	}
}

// stopReason says whether Core's answer err means the work on a rendition
// is to stop, and why: the claim lost (lapsed and claimed again, or the
// credential that made it revoked), the file purged, or the credential
// refused.
func stopReason(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case core.IsReason(err, core.ReasonLeaseLost):
		return DroppedLeaseLost, true
	case core.IsNotFound(err):
		return DroppedGone, true
	case core.CredentialRefused(err):
		return DroppedCredential, true
	}
	return "", false
}

// work is what becomes of the file: fetched, held to what LibreOffice
// converts, converted, and its PDF handed to Core; or why not.
func (j *job) work(ctx context.Context) result {
	f, ok := FormatOf(j.c.Filename)
	if !ok || !Convertible(j.c.Filename, j.c.ContentType) {
		// Core queues nothing else; a Core whose table is wider than the
		// runtime's is told so.
		return j.tell(ctx, skipped(core.RenditionUnsupported))
	}
	data, err := j.fetch(ctx)
	switch {
	case ctx.Err() != nil:
		return dropped(DroppedInterrupted)
	case errors.Is(err, errTooLarge):
		return j.tell(ctx, skipped(core.RenditionTooLarge))
	case err != nil:
		if reason, stop := stopReason(err); stop {
			return dropped(reason)
		}
		j.s.log.Warn("a rendition's file could not be fetched; its claim lapses, and it is claimed again", "rendition", j.c.RenditionID,
			"err", errText(err))
		return dropped(DroppedFetch)
	}
	if why := unconvertible(data); why != "" {
		return j.tell(ctx, skipped(why))
	}
	maxBytes := j.c.MaxBytes
	if maxBytes <= 0 {
		maxBytes = core.DefaultRenditionMaxBytes
	}
	out, err := j.s.o.Converter.Rendition(ctx, data, f, maxBytes)
	switch {
	case ctx.Err() != nil:
		return dropped(DroppedInterrupted)
	case errors.Is(err, office.ErrTooLarge):
		return j.tell(ctx, skipped(core.RenditionTooLarge))
	case errors.Is(err, office.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return j.tell(ctx, failed(core.RenditionTimeout))
	case err != nil:
		j.s.log.Warn("a rendition's file could not be converted", "rendition", j.c.RenditionID, "ext", f.Ext, "err", errText(err))
		return j.tell(ctx, failed(core.RenditionConversionFailed))
	case out.Pages < 1:
		return j.tell(ctx, failed(core.RenditionConversionFailed))
	case out.Pages > core.MaxRenditionPages:
		return j.tell(ctx, skipped(core.RenditionTooLarge))
	}
	j.bytes, j.pages = len(out.Data), out.Pages
	return j.deliver(ctx, out.Data)
}

// unconvertible is why a file is not converted by what it holds, "" when
// it may be: protected by a password (an Office Open XML file encrypted in
// its container, an OpenDocument file whose manifest says it is
// encrypted), or not held as an Office file at all.
func unconvertible(data []byte) string {
	if _, err := doctext.Sniff(data); errors.Is(err, doctext.ErrEncrypted) {
		return core.RenditionPasswordProtected
	}
	if !office.IsContainer(data) {
		return core.RenditionUnsupported
	}
	if odfEncrypted(data) {
		return core.RenditionPasswordProtected
	}
	return ""
}

// odfEncrypted reports whether data is an OpenDocument file whose parts
// are encrypted with a password: its manifest gives them encryption-data.
func odfEncrypted(data []byte) bool {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return false
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name != "META-INF/manifest.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return false
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(io.LimitReader(rc, manifestBytes))
		return bytes.Contains(b, []byte("encryption-data"))
	}
	return false
}

// errTooLarge is a file past MaxFileBytes.
var errTooLarge = errors.New("renditions: the file is larger than the runtime converts")

// fetch reads the claimed file from its signed URL, with no credential;
// from a fresh one (rendition_file) where it has expired or is about to,
// or the file server refuses it.
func (j *job) fetch(ctx context.Context) ([]byte, error) {
	if j.c.ByteSize > j.s.o.MaxFileBytes {
		return nil, errTooLarge
	}
	url := j.c.DownloadURL
	fresh := func() error {
		f, err := j.svc.RenditionFile(ctx, j.cl)
		if err != nil {
			return err
		}
		url = f.DownloadURL
		return nil
	}
	if !j.c.DownloadExpiresAt.IsZero() && j.c.DownloadExpiresAt.Sub(j.s.now()) < time.Minute {
		if err := fresh(); err != nil {
			return nil, err
		}
	}
	data, status, err := j.get(ctx, url)
	if status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone {
		if err := fresh(); err != nil {
			return nil, err
		}
		data, _, err = j.get(ctx, url)
	}
	return data, err
}

// get GETs url, reading at most Options.MaxFileBytes; its errors never
// hold the URL, which is a credential for the file.
func (j *job) get(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, errors.New("renditions: the file's URL does not make a request")
	}
	resp, err := j.s.hc.Do(req)
	if err != nil {
		return nil, 0, errors.New("renditions: the file could not be fetched")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, fmt.Errorf("renditions: the file server answered HTTP %d", resp.StatusCode)
	}
	most := j.s.o.MaxFileBytes
	data, err := io.ReadAll(io.LimitReader(resp.Body, most+1))
	switch {
	case err != nil:
		return nil, resp.StatusCode, errors.New("renditions: the file could not be read")
	case int64(len(data)) > most:
		return nil, resp.StatusCode, errTooLarge
	}
	return data, resp.StatusCode, nil
}

// deliver hands the PDF to Core: somewhere to upload it, the PUT, and the
// completion, done; again to a new URL where Core says nothing arrived;
// failed or skipped where Core refuses the PDF itself.
func (j *job) deliver(ctx context.Context, pdf []byte) result {
	for try := 1; ; try++ {
		up, err := j.svc.RenditionUploadURL(ctx, j.cl)
		if err != nil {
			return j.refused(ctx, err)
		}
		if status, err := j.put(ctx, up, pdf); err != nil {
			if ctx.Err() != nil {
				return dropped(DroppedInterrupted)
			}
			j.s.log.Warn("a rendition's PDF could not be uploaded", "rendition", j.c.RenditionID, "try", try, "err", errText(err))
			switch {
			case try < uploadTries:
				continue
			case status == 0:
				// Nothing answered: the claim lapses, and the file is
				// claimed again.
				return dropped(DroppedUpload)
			}
			// The file server refused the PDF itself.
			return j.tell(ctx, failed(core.RenditionConversionFailed))
		}
		_, err = j.complete(ctx, core.RenditionCompletion{Status: core.RenditionDone, UploadToken: up.UploadToken, PageCount: j.pages})
		switch {
		case err == nil:
			return result{status: core.RenditionDone}
		case core.IsReason(err, core.ReasonNotUploaded) && try < uploadTries:
			j.n++
			continue
		case core.IsReason(err, core.ReasonRenditionTooLarge):
			j.n++
			return j.tell(ctx, skipped(core.RenditionTooLarge))
		case core.IsReason(err, core.ReasonNotAPDF), core.IsReason(err, core.ReasonNotUploaded),
			core.IsReason(err, core.ReasonBadUploadToken), core.IsReason(err, core.ReasonNotYourUpload):
			j.s.log.Warn("Core refused a rendition's PDF", "rendition", j.c.RenditionID, "err", errText(err))
			j.n++
			return j.tell(ctx, failed(core.RenditionConversionFailed))
		}
		return j.refused(ctx, err)
	}
}

// put PUTs the PDF to the upload's URL with exactly its headers and no
// Authorization header, and says what the file server answered (0 for
// nothing). Its errors never hold the URL, which is a credential for the
// upload.
func (j *job) put(ctx context.Context, up *core.RenditionUpload, pdf []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.UploadURL, bytes.NewReader(pdf))
	if err != nil {
		return 0, errors.New("renditions: the upload's URL does not make a request")
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := j.s.hc.Do(req)
	if err != nil {
		return 0, errors.New("renditions: the PDF could not be uploaded")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("renditions: the file server answered the upload HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// tell completes the rendition failed or skipped, as res says.
func (j *job) tell(ctx context.Context, res result) result {
	if _, err := j.complete(ctx, core.RenditionCompletion{Status: res.status, Reason: res.reason}); err != nil {
		return j.refused(ctx, err)
	}
	return res
}

// complete sends a completion under the claim's key, again while Core
// cannot be reached.
func (j *job) complete(ctx context.Context, c core.RenditionCompletion) (*core.RenditionCompleted, error) {
	key := core.RenditionKey(j.cl, j.n)
	backoff := j.s.t.Backoff
	var err error
	for try := range completeTries {
		if try > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, j.s.t.BackoffMax)
		}
		var done *core.RenditionCompleted
		if done, err = j.svc.CompleteRendition(ctx, j.cl, key, c); err == nil {
			return done, nil
		}
		var te *core.TransientError
		var rl *core.RateLimitedError
		if !errors.As(err, &te) && !errors.As(err, &rl) {
			return nil, err
		}
	}
	return nil, err
}

// refused is what a call Core refused, or did not answer, makes of the
// work: dropped, the claim left to lapse, and why.
func (j *job) refused(ctx context.Context, err error) result {
	if ctx.Err() != nil {
		return dropped(DroppedInterrupted)
	}
	if reason, stop := stopReason(err); stop {
		return dropped(reason)
	}
	j.s.log.Warn("Core was not told what became of a rendition; its claim lapses, and it is claimed again", "rendition", j.c.RenditionID,
		"err", errText(err))
	return dropped(DroppedCoreRefused)
}

// finish counts the job and logs it: ids, codes, counts and timings,
// never the file's name, its URL, the upload's or anything it holds.
func (j *job) finish(res result, start time.Time) {
	s := j.s
	took := s.now().Sub(start)
	if s.o.Metrics != nil {
		s.o.Metrics.RenditionJobs.WithLabelValues(res.status, res.reason).Inc()
		s.o.Metrics.RenditionSeconds.Observe(took.Seconds())
	}
	ext := ""
	if f, ok := FormatOf(j.c.Filename); ok {
		ext = f.Ext
	}
	of := j.c.FileID
	if of == "" {
		of = j.c.AttachmentID
	}
	s.log.Info("a file's PDF rendition", "rendition", j.c.RenditionID, "course", j.c.CourseID, "source", j.c.Source, "of", of,
		"attempt", j.c.Attempt, "backfill", j.c.Backfill, "ext", ext, "bytes", j.c.ByteSize, "status", res.status, "reason", res.reason,
		"pages", j.pages, "pdf_bytes", j.bytes, "ms", took.Milliseconds())
}
