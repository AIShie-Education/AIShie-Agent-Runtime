package transcribe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Bounds of one version's work.
const (
	// MaxFileBytes is the largest file transcribed: a longer one is
	// skipped (too_large).
	MaxFileBytes = 64 << 20
	// jobTimeout bounds one version's work, its claim renewed meanwhile.
	jobTimeout = 2 * time.Hour
	// imagePages is the most pages one call is given as pictures.
	imagePages = 5
	// imageDPI is the resolution pages are drawn at for a model that
	// takes pictures.
	imageDPI = office.MaxImageDPI
	// completeTries is how many times a completion is sent, under its
	// key, while Core cannot be reached.
	completeTries = 5
)

// job is the work on one claimed version.
type job struct {
	s   *Service
	svc *core.Service
	m   *model
	st  Setting
	c   core.ClaimedText

	mu  sync.Mutex
	rec store.TranscriptionJob
	// cost is what its model calls cost; upTo the last page sent to the
	// model; held the pages it holds of the day's quota, on heldOn.
	cost   spent
	upTo   int
	held   int
	heldOn time.Time
}

// result is what became of a version: done, failed or skipped, which Core
// is told, or dropped, which it is not (the claim lost, staff wrote the
// text, the version gone), with why; and a text done's body, pages and
// model.
type result struct {
	status, reason string
	body           string
	pages          int
	model          string
}

func done(body string, pages int, model string) result {
	return result{status: core.TextDone, body: body, pages: pages, model: model}
}
func skipped(reason string) result { return result{status: core.TextSkipped, reason: reason} }
func failed(reason string) result  { return result{status: core.TextFailed, reason: reason} }
func dropped(reason string) result { return result{status: store.JobDropped, reason: reason} }

// dropCause is why a job's work was dropped part way, as the context's
// cause carries it.
type dropCause struct{ reason string }

func (d dropCause) Error() string { return "transcribe: dropped: " + d.reason }

// start works on the claimed version c in the background, as one of the
// jobs in progress.
func (s *Service) start(ctx context.Context, svc *core.Service, m *model, st Setting, c core.ClaimedText) {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
	if s.o.Metrics != nil {
		s.o.Metrics.TranscribeInflight.Inc()
	}
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer func() {
			s.mu.Lock()
			s.inflight--
			s.poke()
			s.mu.Unlock()
			if s.o.Metrics != nil {
				s.o.Metrics.TranscribeInflight.Dec()
			}
		}()
		j := &job{s: s, svc: svc, m: m, st: st, c: c}
		j.run(ctx)
	}()
}

// run is the job: recorded as working, the claim held while the file is
// transcribed, Core told what became of it, and the record, the metrics
// and the log line of it.
func (j *job) run(ctx context.Context) {
	s := j.s
	start := s.now()
	j.rec = store.TranscriptionJob{ID: "trj_" + uuid.NewString(), VersionID: j.c.VersionID, DocumentID: j.c.DocumentID, CourseID: j.c.CourseID,
		LeaseID: j.c.LeaseID, Status: store.JobWorking, Backfill: j.c.Backfill, Attempt: j.c.Attempt, ContentType: j.c.ContentType,
		ByteSize: j.c.ByteSize, Offer: j.m.offer.ID, Model: j.m.offer.Model, Worker: s.o.Holder, StartedAt: start, HeartbeatAt: start}
	j.save(ctx)
	ctx, cancel := context.WithTimeoutCause(ctx, jobTimeout, dropCause{"the work took longer than " + jobTimeout.String()})
	defer cancel()
	ctx, drop := context.WithCancelCause(ctx)
	defer drop(nil)
	holding, stopHold := context.WithCancel(ctx)
	held := make(chan struct{})
	go func() { defer close(held); j.hold(holding, drop) }()

	res := j.transcribe(ctx)
	stopHold()
	<-held
	var dc dropCause
	switch cause := context.Cause(ctx); {
	case errors.As(cause, &dc):
		res = dropped(dc.reason)
	case ctx.Err() != nil:
		res = dropped(store.ReasonInterrupted)
	case res.status != store.JobDropped:
		res = j.tell(ctx, res)
	}
	j.finish(res, start)
}

// save keeps the job's record as it stands.
func (j *job) save(ctx context.Context) {
	j.mu.Lock()
	rec := j.rec
	j.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := j.s.o.Store.PutTranscriptionJob(ctx, rec); err != nil {
		j.s.log.Warn("a transcription's record could not be kept", "job", rec.ID, "err", errText(err))
	}
}

// hold renews the claim every third of its lease until ctx ends, the job
// recorded as held each time; Core saying the work is no longer to be
// done (the claim lost, staff wrote the text, the course or document
// archived, the version gone, the credential refused) drops it.
func (j *job) hold(ctx context.Context, drop context.CancelCauseFunc) {
	s := j.s
	t := time.NewTicker(s.t.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := j.svc.Renew(ctx, core.ClaimOf(j.c), s.t.Lease)
		if ctx.Err() != nil {
			return
		}
		if reason, stop := stopReason(ctx, s, err); stop {
			drop(dropCause{reason})
			return
		}
		if err != nil {
			s.log.Warn("a transcription's claim could not be renewed; it is tried again", "version", j.c.VersionID, "err", errText(err))
			continue
		}
		j.mu.Lock()
		j.rec.HeartbeatAt = s.now()
		j.mu.Unlock()
		j.save(ctx)
	}
}

// stopReason says whether Core's answer err means the work on a version
// is to stop, and why: the claim lost, staff wrote the text, the course
// or the document archived, the version gone, or the credential refused
// (which is then not tried again).
func stopReason(ctx context.Context, s *Service, err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, core.ErrUnauthenticated):
		s.rejectCredential(ctx, "Core answered 401: the credential was revoked or has expired")
		return BlockedCredentialRejected, true
	case core.IsNotFound(err):
		return "gone", true
	}
	for _, r := range []string{core.ReasonLeaseLost, core.ReasonEditedByStaff, core.ReasonCourseArchived, core.ReasonDocumentArchived} {
		if core.IsReason(err, r) {
			return r, true
		}
	}
	return "", false
}

// tell completes the version in Core as res says, under the claim's key,
// again while Core cannot be reached; Core refusing it drops it.
func (j *job) tell(ctx context.Context, res result) result {
	s := j.s
	c := core.Completion{Status: res.status, Body: res.body, Pages: res.pages, Model: res.model, Reason: res.reason}
	backoff := s.t.Backoff
	var err error
	for try := range completeTries {
		if try > 0 {
			select {
			case <-ctx.Done():
				return dropped(store.ReasonInterrupted)
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, s.t.BackoffMax)
		}
		if _, err = j.svc.Complete(ctx, core.ClaimOf(j.c), c); err == nil {
			return res
		}
		if reason, stop := stopReason(ctx, s, err); stop {
			return dropped(reason)
		}
		var te *core.TransientError
		var rl *core.RateLimitedError
		if !errors.As(err, &te) && !errors.As(err, &rl) {
			break
		}
	}
	s.log.Warn("Core was not told what became of a transcription; its claim lapses, and it is claimed again", "version", j.c.VersionID,
		"err", errText(err))
	return dropped("core_refused")
}

// finish records what became of the job, counts it and logs it: ids,
// counts and codes, never the text.
func (j *job) finish(res result, start time.Time) {
	s := j.s
	now := s.now()
	j.mu.Lock()
	j.rec.Status, j.rec.Reason, j.rec.FinishedAt = res.status, clip(res.reason), &now
	j.rec.PagesSent, j.rec.ModelCalls, j.rec.InputTokens, j.rec.OutputTokens = j.upTo, j.cost.calls, j.cost.input, j.cost.output
	if j.cost.calls > 0 && !j.cost.unpriced {
		c := j.cost.cost
		j.rec.CostPUSD = &c
	}
	if res.status == core.TextDone && res.model == TextFileModel {
		j.rec.Offer, j.rec.Model = "", ""
	}
	rec := j.rec
	j.mu.Unlock()
	j.save(context.Background())
	// The pages it held count in the store now, as the pages it sent.
	s.unreserve(j.heldOn, j.held)
	if s.o.Metrics != nil {
		s.o.Metrics.TranscribeJobs.WithLabelValues(res.status).Inc()
		s.o.Metrics.TranscribePages.Add(float64(j.upTo))
	}
	pages := 0
	if rec.Pages != nil {
		pages = *rec.Pages
	}
	s.log.Info("a version transcribed", "job", rec.ID, "version", rec.VersionID, "course", rec.CourseID, "status", res.status,
		"reason", rec.Reason, "pages", pages, "pages_sent", j.upTo, "calls", j.cost.calls, "ms", now.Sub(start).Milliseconds())
}

// clip is a reason within the 500 characters Core and the store keep.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= 500 {
		return s
	}
	r := []rune(s)
	return string(r[:499]) + "…"
}

// kinds of file, by what the transcriber does with it.
type fileKind int

const (
	kindOther fileKind = iota
	kindText
	kindImage
	kindPDF
	kindOffice
)

// kindOf is what a file of media type mt is.
func kindOf(mt string) fileKind {
	switch mt {
	case "text/plain", "text/markdown", "text/x-markdown", "text/csv":
		return kindText
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return kindImage
	case "application/pdf":
		return kindPDF
	}
	if _, ok := office.FormatOf(mt); ok {
		return kindOffice
	}
	return kindOther
}

// mediaType is a content type's type/subtype, lower case; "" for none.
func mediaType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

// sniff is what a file of no telling type is, by what it holds: a PDF or
// an Office Open XML file (an encrypted one, errEncrypted), an older or
// OpenDocument Office file or RTF, and else by Go's sniffing.
func sniff(data []byte) (string, error) {
	f, err := doctext.Sniff(data)
	switch {
	case errors.Is(err, doctext.ErrEncrypted):
		return "", errEncrypted
	case f != "":
		return f.MediaType(), nil
	}
	if mt := office.Sniff(data); mt != "" {
		return mt, nil
	}
	return mediaType(http.DetectContentType(data)), nil
}

var errEncrypted = errors.New("transcribe: the file is password-protected")

// transcribe is what becomes of the version: its file fetched and known by
// what it is; a text file's own text; an image, one page; a PDF, or an
// Office file's PDF, held to the pages a document may have and the day's
// quota, then transcribed a range of pages at a time.
func (j *job) transcribe(ctx context.Context) result {
	data, err := j.fetch(ctx)
	switch {
	case ctx.Err() != nil:
		return dropped(store.ReasonInterrupted)
	case errors.Is(err, errTooLarge):
		return skipped(ReasonTooLarge)
	case err != nil:
		j.s.log.Warn("a transcription's file could not be fetched", "version", j.c.VersionID, "err", errText(err))
		return failed("the file could not be fetched")
	}
	mt := mediaType(j.c.ContentType)
	if k := kindOf(mt); k == kindOther || mt == "application/octet-stream" || mt == "" {
		if mt, err = sniff(data); errors.Is(err, errEncrypted) {
			return skipped(ReasonEncrypted)
		}
	}
	switch kindOf(mt) {
	case kindText:
		return j.textFile(data)
	case kindImage:
		return j.image(ctx, data, mt)
	case kindPDF:
		return j.pdf(ctx, data, nil, false, nil)
	case kindOffice:
		return j.office(ctx, data, mt)
	}
	return skipped(ReasonUnsupportedFormat)
}

// errTooLarge is a file past MaxFileBytes.
var errTooLarge = errors.New("transcribe: the file is larger than the transcriber reads")

// fetch reads the claimed version's file from its signed URL, with no
// credential; from a fresh one (document_text.file) where it has expired,
// or the file server refuses it.
func (j *job) fetch(ctx context.Context) ([]byte, error) {
	if j.c.ByteSize > MaxFileBytes {
		return nil, errTooLarge
	}
	url := j.c.DownloadURL
	fresh := func() error {
		f, err := j.svc.File(ctx, core.ClaimOf(j.c))
		if err != nil {
			return err
		}
		url = f.DownloadURL
		return nil
	}
	if !j.c.DownloadExpiresAt.IsZero() && time.Until(j.c.DownloadExpiresAt) < time.Minute {
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

// get GETs url, reading at most MaxFileBytes; its errors never hold the
// URL, which is a credential for the file.
func (j *job) get(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, errors.New("transcribe: the file's URL does not make a request")
	}
	resp, err := j.s.o.CoreHTTP.Do(req)
	if err != nil {
		return nil, 0, errors.New("transcribe: the file could not be fetched")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, fmt.Errorf("transcribe: the file server answered HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes+1))
	switch {
	case err != nil:
		return nil, resp.StatusCode, errors.New("transcribe: the file could not be read")
	case len(data) > MaxFileBytes:
		return nil, resp.StatusCode, errTooLarge
	}
	return data, resp.StatusCode, nil
}

// textFile is a text file's own text, as it is: no model reads it.
func (j *job) textFile(data []byte) result {
	text := strings.ToValidUTF8(string(data), "�")
	if strings.TrimSpace(text) == "" {
		return skipped(ReasonEmpty)
	}
	j.pages(1)
	return done(fit(text, core.MaxTextBytes), 1, TextFileModel)
}

// pages records the file's pages.
func (j *job) pages(n int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.rec.Pages = &n
}

// image is a picture: one page, given to the model as it is.
func (j *job) image(ctx context.Context, data []byte, mt string) result {
	j.pages(1)
	if r, ok := j.hold1(ctx, 1); !ok {
		return r
	}
	text, err := j.ask(ctx, pageSpan{first: 1, last: 1, total: 1}, []*llm.File{{Name: "page-1" + extOf(mt), MIME: mt, Data: data}})
	if err != nil {
		return j.modelFailed(ctx, err)
	}
	return done(fit(text, core.MaxTextBytes), 1, j.m.label)
}

func extOf(mt string) string {
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// hold1 holds pages of the day's quota for the version: the result to
// give, skipped (quota_exhausted), when they do not fit.
func (j *job) hold1(ctx context.Context, pages int) (result, bool) {
	ok, err := j.s.reserve(ctx, j.st, pages)
	if err != nil {
		j.s.log.Warn("the day's pages could not be read", "err", errText(err))
		return failed("the transcriber's records could not be read"), false
	}
	if !ok {
		return skipped(ReasonQuotaExhausted), false
	}
	j.held, j.heldOn = pages, store.UTCDay(j.s.now())
	return result{}, true
}

// office is an Office file: a presentation or a document converted to PDF
// by LibreOffice (package office), its slides' speaker notes read beside
// it for a deck of PowerPoint's; a workbook is not transcribed.
func (j *job) office(ctx context.Context, data []byte, mt string) result {
	f, _ := office.FormatOf(mt)
	if f.Family == office.Workbook {
		return skipped(ReasonUnsupportedFormat)
	}
	if _, err := doctext.Sniff(data); errors.Is(err, doctext.ErrEncrypted) {
		return skipped(ReasonEncrypted)
	}
	if j.s.o.Office == nil {
		return skipped(ReasonUnsupportedFormat)
	}
	if ok, _ := j.s.o.Office.Available(); !ok {
		return skipped(ReasonUnsupportedFormat)
	}
	sum := checksum(data)
	var out *office.Output
	for out == nil {
		st := j.s.o.Office.Convert(ctx, sum, f, office.ToPDF, data)
		switch st.Status {
		case office.StatusDone:
			out = st.Out
		case office.StatusFailed:
			j.s.log.Warn("a transcription's file could not be converted", "version", j.c.VersionID, "why", st.Why)
			return failed(ReasonConversionFailed)
		case office.StatusOff:
			return skipped(ReasonUnsupportedFormat)
		default: // pending, or the queue full: again, in a while
			select {
			case <-ctx.Done():
				return dropped(store.ReasonInterrupted)
			case <-time.After(2 * time.Second):
			}
		}
	}
	if out.Capped {
		return skipped(ReasonTooManyPages)
	}
	var notes map[int]string
	if f.Family == office.Slides && f.OOXML {
		notes = j.notes(ctx, data)
	}
	return j.pdf(ctx, out.Data, &out.Pages, f.Family == office.Slides, notes)
}

// notes are a deck's speaker notes, by slide, as the runtime reads them:
// none where it cannot.
func (j *job) notes(ctx context.Context, data []byte) map[int]string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := doctext.Extract(ctx, data, doctext.PPTX, j.s.o.DocLimits)
	if err != nil {
		return nil
	}
	out := map[int]string{}
	for _, sl := range res.Slides {
		if sl.Notes != "" {
			out[sl.N] = sl.Notes
		}
	}
	return out
}

// checksum is data's sha256, as "sha256:<hex>".
func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pdf is a PDF transcribed, of pages pages when they are known (an Office
// file's, as LibreOffice made it) and counted otherwise: one that needs a
// password skipped, as is one of more pages than a document may have, or
// than the day has left; then a range of its pages at a time, joined, and
// cut to what Core keeps.
func (j *job) pdf(ctx context.Context, pdf []byte, pages *int, slides bool, notes map[int]string) result {
	var n int
	if pages != nil {
		n = *pages
	} else {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		var err error
		n, err = doctext.PDFPages(pctx, pdf, j.s.o.DocLimits)
		cancel()
		switch {
		case errors.Is(err, doctext.ErrEncrypted):
			return skipped(ReasonEncrypted)
		case err != nil || n < 1:
			return failed("the PDF could not be read")
		}
	}
	j.pages(n)
	if n > j.st.Site.Pages() {
		return skipped(ReasonTooManyPages)
	}
	if r, ok := j.hold1(ctx, n); !ok {
		return r
	}
	d := doc{pdf: pdf, sum: checksum(pdf), pages: n, slides: slides, notes: notes}
	per := j.s.o.PartPages
	if j.m.input == InputImages {
		per = min(per, imagePages)
	}
	if j.m.pdfPages > 0 {
		per = min(per, j.m.pdfPages)
	}
	whole := j.m.input == InputPDF && !j.cuts()
	if whole {
		if j.m.pdfPages > 0 && n > j.m.pdfPages {
			return failed("the PDF has more pages than the model takes in one file, and its pages cannot be cut here")
		}
		per = n
	}
	if j.m.input == InputImages && !j.draws() {
		return failed("its pages cannot be drawn as pictures here")
	}
	var b strings.Builder
	for first := 1; first <= n; first += per {
		text, err := j.span(ctx, d, first, min(first+per-1, n), whole)
		if err != nil {
			return j.modelFailed(ctx, err)
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return done(fit(b.String(), core.MaxTextBytes), n, j.m.label)
}

func (j *job) cuts() bool  { return j.s.o.Office != nil && j.s.o.Office.Cuts() }
func (j *job) draws() bool { return j.s.o.Office != nil && j.s.o.Office.Draws() }

// doc is a PDF being transcribed.
type doc struct {
	pdf    []byte
	sum    string
	pages  int
	slides bool
	notes  map[int]string
}

// span is pages first to last of d transcribed: given as their own PDF,
// or drawn as pictures, in one call; in halves where the model's text was
// cut off at its output bound, the request was too long for it, or the
// pages are more bytes than it takes in a file. whole gives the whole PDF
// as it is.
func (j *job) span(ctx context.Context, d doc, first, last int, whole bool) (string, error) {
	halves := func() (string, error) {
		mid := (first + last) / 2
		a, err := j.span(ctx, d, first, mid, false)
		if err != nil {
			return "", err
		}
		b, err := j.span(ctx, d, mid+1, last, false)
		if err != nil {
			return "", err
		}
		return a + "\n\n" + b, nil
	}
	files, err := j.files(ctx, d, first, last, whole)
	if err != nil {
		return "", err
	}
	if j.m.input == InputPDF && j.m.pdfBytes > 0 && int64(len(files[0].Data)) > j.m.pdfBytes {
		if last > first && j.cuts() {
			return halves()
		}
		return "", fmt.Errorf("transcribe: %d bytes of pages, more than the model takes in a file", len(files[0].Data))
	}
	text, err := j.ask(ctx, pageSpan{first: first, last: last, total: d.pages, slides: d.slides, notes: d.notes}, files)
	var le *llm.Error
	switch {
	case (errors.Is(err, errTruncated) || errors.As(err, &le) && le.Kind == llm.ErrContextOverflow) && last > first && !whole:
		return halves()
	case errors.Is(err, errTruncated):
		return text, nil
	case err != nil:
		return "", err
	}
	return text, nil
}

// files are pages first to last of d as the model is given them: a PDF of
// their own (cut by pdftocairo, or d's whole), or a picture each.
func (j *job) files(ctx context.Context, d doc, first, last int, whole bool) ([]*llm.File, error) {
	name := fmt.Sprintf("pages-%d-%d", first, last)
	if j.m.input == InputImages {
		pics, err := j.s.o.Office.Images(ctx, d.pdf, first, last, imageDPI)
		if err != nil {
			return nil, fmt.Errorf("transcribe: drawing pages %d to %d: %w", first, last, err)
		}
		out := make([]*llm.File, len(pics))
		for i, p := range pics {
			out[i] = &llm.File{Name: fmt.Sprintf("page-%d.png", first+i), MIME: "image/png", Data: p}
		}
		return out, nil
	}
	if whole || first == 1 && last == d.pages {
		return []*llm.File{{Name: name + ".pdf", MIME: "application/pdf", Data: d.pdf}}, nil
	}
	part, err := j.s.o.Office.Range(ctx, d.sum, d.pdf, first, last)
	if err != nil {
		return nil, fmt.Errorf("transcribe: cutting pages %d to %d: %w", first, last, err)
	}
	return []*llm.File{{Name: name + ".pdf", MIME: "application/pdf", Data: part}}, nil
}

// pageSpan is pages first to last of a document of total, its slides'
// when slides, with the speaker notes of its slides.
type pageSpan struct {
	first, last, total int
	slides             bool
	notes              map[int]string
}

// ask is the model's text of the pages s, sent as files: the pages it was
// given count as sent, and the text is cleaned. errTruncated comes with
// the text as far as the output bound cut it.
func (j *job) ask(ctx context.Context, s pageSpan, files []*llm.File) (string, error) {
	req := request(j.m, s.first, s.last, s.total, s.slides, files, s.notes)
	j.mu.Lock()
	j.upTo = max(j.upTo, s.last)
	j.mu.Unlock()
	text, err := j.s.call(ctx, j.m, req, j.st.Prices, &j.cost)
	if err != nil && !errors.Is(err, errTruncated) {
		return "", err
	}
	return clean(text, s.first, s.slides), err
}

// modelFailed is what a model call that failed makes of the version:
// dropped when the work was stopped meanwhile, and otherwise failed.
func (j *job) modelFailed(ctx context.Context, err error) result {
	if ctx.Err() != nil {
		return dropped(store.ReasonInterrupted)
	}
	j.s.log.Warn("a transcription's model call failed", "version", j.c.VersionID, "offer", j.m.offer.ID, "err", errText(err))
	if strings.HasPrefix(err.Error(), "transcribe: cutting") || strings.HasPrefix(err.Error(), "transcribe: drawing") {
		return failed(ReasonConversionFailed)
	}
	return failed(ReasonModelError)
}
