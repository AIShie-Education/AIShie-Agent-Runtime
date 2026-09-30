package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// pageModel is a model that takes files and transcribes what it is asked
// for: the heading of each page the request names, and a line of text
// under it. answer, when set, answers instead.
type pageModel struct {
	provider string
	caps     llm.Capabilities
	limits   llm.FileLimits

	mu     sync.Mutex
	reqs   []*llm.Request
	answer func(req *llm.Request, n int) (*llm.Response, error)
}

func (p *pageModel) Name() string                   { return llm.AdapterOpenAIChat }
func (p *pageModel) Provider() string               { return p.provider }
func (p *pageModel) Model() string                  { return "flash-lite" }
func (p *pageModel) Maker() string                  { return "test" }
func (p *pageModel) Dialect() toolschema.Dialect    { return toolschema.OpenAI }
func (p *pageModel) Capabilities() llm.Capabilities { return p.caps }
func (p *pageModel) FileLimits() llm.FileLimits     { return p.limits }

func (p *pageModel) Call(_ context.Context, req *llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	n := len(p.reqs)
	answer := p.answer
	p.mu.Unlock()
	if answer != nil {
		return answer(req, n)
	}
	return transcribed(req), nil
}

func (p *pageModel) requests() []*llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.reqs)
}

// asked is what a request names: its first and last page, and whether
// they are slides.
var askedRe = regexp.MustCompile(`(page|slide)s? (\d+)(?: to (\d+))? of (\d+)`)

func asked(req *llm.Request) (first, last int, slides bool) {
	m := askedRe.FindStringSubmatch(req.Messages[0].Parts[0].Text)
	first, _ = strconv.Atoi(m[2])
	last = first
	if m[3] != "" {
		last, _ = strconv.Atoi(m[3])
	}
	return first, last, m[1] == "slide"
}

// transcribed is each page the request names under its heading.
func transcribed(req *llm.Request) *llm.Response {
	first, last, slides := asked(req)
	var b strings.Builder
	for n := first; n <= last; n++ {
		fmt.Fprintf(&b, "%s\n\nText of %d.\n\n", heading(n, slides), n)
	}
	return &llm.Response{Parts: []llm.Part{llm.Text(b.String())}, Stop: llm.StopEnd,
		Usage: llm.Usage{Input: int64(300 * (last - first + 1)), Output: int64(100 * (last - first + 1))}}
}

// fakeOffice converts every Office file to a PDF of pages pages, cuts a
// range of a PDF as a few bytes naming it, and draws a page as a few
// bytes naming it.
type fakeOffice struct {
	pages    int
	capped   bool
	off      bool
	cuts     bool
	mu       sync.Mutex
	ranges   [][2]int
	drawn    [][2]int
	convert  int
	failWith office.Status
}

func (f *fakeOffice) Available() (bool, string) { return !f.off, "LibreOffice is not installed" }
func (f *fakeOffice) Cuts() bool                { return f.cuts }
func (f *fakeOffice) Draws() bool               { return f.cuts }

func (f *fakeOffice) Convert(_ context.Context, _ string, _ office.Format, to office.Target, _ []byte) office.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.convert++
	if f.failWith != "" {
		return office.State{Status: f.failWith, Why: "it is damaged"}
	}
	if to != office.ToPDF {
		return office.State{Status: office.StatusFailed}
	}
	pages := make([]doctexttest.PDFPage, f.pages)
	for i := range pages {
		pages[i] = doctexttest.PDFPage{Lines: []string{"Slide " + strconv.Itoa(i+1)}}
	}
	return office.State{Status: office.StatusDone, Out: &office.Output{Data: doctexttest.PDF(pages...), Pages: f.pages, Capped: f.capped}}
}

func (f *fakeOffice) Range(_ context.Context, _ string, _ []byte, first, last int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranges = append(f.ranges, [2]int{first, last})
	return fmt.Appendf(nil, "%%PDF-1.4 pages %d-%d", first, last), nil
}

func (f *fakeOffice) Images(_ context.Context, _ []byte, first, last, dpi int) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drawn = append(f.drawn, [2]int{first, last})
	var out [][]byte
	for n := first; n <= last; n++ {
		out = append(out, fmt.Appendf(nil, "PNG page %d at %d dpi", n, dpi))
	}
	return out, nil
}

// secretsOf resolves the references it holds, and none else.
type secretsOf struct {
	mu sync.Mutex
	m  map[string]string
}

func (r *secretsOf) Resolve(_ context.Context, ref, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.m[ref]; ok {
		return v, nil
	}
	return "", errors.New("no such secret")
}

func (r *secretsOf) put(ref, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[ref] = v
}

// rig is a transcriber against a fake Core, on a memstore, with a model
// that transcribes what it is asked for and an Office that converts
// everything.
type rig struct {
	t       *testing.T
	fc      *fakecore.Core
	co      fakecore.Course
	st      *memstore.Store
	model   *pageModel
	office  *fakeOffice
	secrets *secretsOf
	m       *metrics.Metrics
	svc     *Service
	setting Setting
	creds   int
}

// offer is the plan's offer the rig transcribes with.
var offer = config.SchoolOffer{ID: "flash", Label: "Gemini Flash-Lite (test)", Adapter: llm.AdapterOpenAIChat, Provider: llm.ProviderOpenAI,
	Model: "flash-lite", KeyRef: "secret://school/keys/flash"}

// prices price the offer's model: $1 in and $2 out a million tokens.
func prices(t *testing.T) *pricing.Table {
	t.Helper()
	tb, err := pricing.Parse([]byte(`version: "t1"
prices:
  - provider: openai
    model: flash-lite
    from: 2025-01-01
    id: flash
    usd_per_mtok: {input: 1, output: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func newRig(t *testing.T, edit ...func(*rig, *Options)) *rig {
	t.Helper()
	fc := fakecore.New(fakecore.Options{})
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(fc.Shutdown)
	r := &rig{t: t, fc: fc, co: fc.AddCourse("CS101"), st: memstore.New(),
		model:   &pageModel{provider: llm.ProviderOpenAI, caps: llm.Capabilities{FileInput: true}, limits: llm.OpenAIFileLimits},
		office:  &fakeOffice{pages: 3, cuts: true},
		secrets: &secretsOf{m: map[string]string{"secret://school/keys/flash": "sk-test-flash-0123456789"}},
		m:       metrics.New(prometheus.NewRegistry())}
	o := offer
	r.setting = Setting{Site: config.SiteTranscription{Enabled: true, Offer: "flash"}, Offer: &o, Prices: prices(t)}
	opts := Options{Mode: config.TranscribeAuto, Store: r.st, Keeps: true, CoreBaseURL: srv.URL, CoreHTTP: srv.Client(),
		Secrets: r.secrets, NewAdapter: func(llm.Config) (llm.Adapter, error) { return r.model, nil }, Office: r.office,
		Metrics: r.m, Holder: "w1", Timing: Timing{Wait: time.Second, Blocked: 50 * time.Millisecond, Standby: 50 * time.Millisecond,
			Backoff: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond, ModelBackoff: 10 * time.Millisecond}}
	for _, e := range edit {
		e(r, &opts)
	}
	r.svc = New(opts)
	return r
}

// credential gives the transcriber a new service credential of the fake
// Core's, as the API would keep it.
func (r *rig) credential() fakecore.Token {
	r.t.Helper()
	r.creds++
	tok := r.fc.IssueServiceToken("runtime")
	id := "sec_svc_" + strconv.Itoa(r.creds)
	r.secrets.put("sealed://"+id, tok.Token)
	sec := store.Secret{ID: id, TenantID: store.SiteTenantID, Kind: store.SecretCoreToken, KEKID: "k1", WrappedDEK: []byte{1},
		Nonce: []byte{2}, Ciphertext: []byte{3}, Hint: "aissvc_…"}
	if err := r.st.PutTranscriptionCredential(r.t.Context(), store.TranscriptionCredential{SecretID: id, Hint: sec.Hint,
		CredentialID: tok.CredentialID, SetBy: "admin-1"}, sec); err != nil {
		r.t.Fatal(err)
	}
	return tok
}

// run runs the transcriber with the rig's setting until the test ends.
func (r *rig) run() {
	r.t.Helper()
	r.svc.Set(r.setting)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.svc.Run(ctx) }()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			r.t.Error("the transcriber did not stop")
		}
	})
}

// waitText waits for the document's text to leave pending and working,
// and returns it.
func (r *rig) waitText(doc string) fakecore.TextRecord {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, ok := r.fc.Text(doc)
		if !ok {
			r.t.Fatal("the document has no text version")
		}
		if rec.Status != core.TextPending && rec.Status != core.TextWorking {
			return rec
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the text is still %s", rec.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitJob waits for the job of the version of doc to end, and returns it.
func (r *rig) waitJob(status string) store.TranscriptionJob {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		js, err := r.st.TranscriptionJobs(r.t.Context(), store.JobQuery{Status: status, Limit: 10})
		if err != nil {
			r.t.Fatal(err)
		}
		if len(js) > 0 {
			return js[0]
		}
		if time.Now().After(deadline) {
			all, _ := r.st.TranscriptionJobs(r.t.Context(), store.JobQuery{Limit: 10})
			r.t.Fatalf("no job %s: %+v", status, all)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pdfOf(pages int) []byte {
	ps := make([]doctexttest.PDFPage, pages)
	for i := range ps {
		ps[i] = doctexttest.PDFPage{Lines: []string{"Page " + strconv.Itoa(i+1)}}
	}
	return doctexttest.PDF(ps...)
}

func (r *rig) addFile(title, contentType string, data []byte) string {
	r.t.Helper()
	id, err := r.fc.AddFile(r.co.ID, title, contentType, data)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// A PDF of twelve pages is transcribed ten pages a call, each range given
// as a PDF of its own; the text, every page under its heading, is done in
// Core with the offer's label, and the job, the ledger and the metrics say
// so, with the cost at the day's prices, on the school's key and nobody's
// else.
func TestTranscribesAPDF(t *testing.T) {
	r := newRig(t)
	r.credential()
	doc := r.addFile("Week 1", "application/pdf", pdfOf(12))
	r.run()
	rec := r.waitText(doc)
	if rec.Status != core.TextDone || rec.Source != core.SourceAI || rec.Pages != 12 || rec.Model != offer.Label {
		t.Fatalf("the text: %+v", rec)
	}
	for n := 1; n <= 12; n++ {
		if !strings.Contains(rec.Body, "## 第 "+strconv.Itoa(n)+" 頁\n\nText of "+strconv.Itoa(n)+".") {
			t.Errorf("page %d is not in the text:\n%s", n, rec.Body)
		}
	}
	reqs := r.model.requests()
	if len(reqs) != 2 || !slices.Equal(r.office.ranges, [][2]int{{1, 10}, {11, 12}}) {
		t.Fatalf("%d calls, ranges %v", len(reqs), r.office.ranges)
	}
	for _, req := range reqs {
		f := req.Messages[0].Parts[1].File
		if req.System != Prompt || f == nil || f.MIME != "application/pdf" || len(req.Messages[0].Parts) != 2 {
			t.Errorf("a request: %+v", req.Messages[0].Parts)
		}
	}
	if !strings.Contains(reqs[1].Messages[0].Parts[0].Text, "pages 11 to 12 of 12") {
		t.Errorf("the second range is asked for as %q", reqs[1].Messages[0].Parts[0].Text)
	}
	j := r.waitJob(store.JobDone)
	if j.VersionID == "" || j.CourseID != r.co.ID || j.DocumentID != doc || *j.Pages != 12 || j.PagesSent != 12 || j.ModelCalls != 2 ||
		j.Offer != "flash" || j.Model != "flash-lite" || j.CostPUSD == nil || *j.CostPUSD != pricing.PUSD((3600*1+1200*2)/1e6) ||
		j.FinishedAt == nil {
		t.Errorf("the job: %+v", j)
	}
	rows, err := r.st.CostReport(t.Context(), store.CostQuery{Group: store.CostByAgent, Since: store.UTCDay(time.Now()),
		Until: store.UTCDay(time.Now()).AddDate(0, 0, 1), Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Key != store.CostKeyTranscription || rows[0].Transcription.Calls != 2 ||
		rows[0].Transcription.CostPUSD != *j.CostPUSD {
		t.Errorf("the ledger: %+v, %v", rows, err)
	}
	if sp, _ := r.st.Spend(t.Context(), store.SpendScope{KeySource: config.KeySchool}, store.UTCDay(time.Now())); sp.CostPUSD != *j.CostPUSD {
		t.Errorf("the school's key's day: %+v", sp)
	}
	if got := testutil.ToFloat64(r.m.TranscribeJobs.WithLabelValues("done")); got != 1 {
		t.Errorf("transcribe_jobs_total{done} = %v", got)
	}
	if got := testutil.ToFloat64(r.m.TranscribePages); got != 12 {
		t.Errorf("transcribe_pages_total = %v", got)
	}
	if day, _ := r.st.TranscriptionDay(t.Context(), store.UTCDay(time.Now())); day.Pages != 12 || day.Documents != 1 {
		t.Errorf("the day: %+v", day)
	}
	if st := r.svc.Status(); !st.Available || !st.Enabled || !st.Leader || st.Standby {
		t.Errorf("the status: %+v", st)
	}
	if c, _ := r.st.TranscriptionCredential(t.Context()); c.LastOKAt == nil {
		t.Errorf("Core's taking of the credential is not noted: %+v", c)
	}
}

// Off, the transcriber claims nothing, and holds no lease; turned on,
// without a restart, it claims what waits.
func TestOffClaimsNothing(t *testing.T) {
	r := newRig(t)
	r.credential()
	r.setting.Site.Enabled = false
	doc := r.addFile("Week 1", "application/pdf", pdfOf(2))
	r.run()
	time.Sleep(300 * time.Millisecond)
	if rec, _ := r.fc.Text(doc); rec.Status != core.TextPending || rec.Attempts != 0 {
		t.Fatalf("off, the text is %+v", rec)
	}
	if st := r.svc.Status(); st.Leader || st.Standby || st.Enabled {
		t.Errorf("off: %+v", st)
	}
	if ok, err := r.st.AcquireLease(t.Context(), LeaseName, "w2", time.Second); err != nil || !ok {
		t.Errorf("off, the lease is held: %v %v", ok, err)
	}
	if err := r.st.ReleaseLease(t.Context(), LeaseName, "w2"); err != nil {
		t.Fatal(err)
	}
	r.setting.Site.Enabled = true
	r.svc.Set(r.setting)
	if rec := r.waitText(doc); rec.Status != core.TextDone {
		t.Errorf("turned on: %+v", rec)
	}
}

// Two workers on one store: one claims, the other stands by, and takes
// over when the first stops.
func TestOneWorkerClaims(t *testing.T) {
	r := newRig(t)
	r.credential()
	o := r.svc.o
	o.Holder, o.Metrics = "w2", nil
	workers := map[string]*Service{"w1": r.svc, "w2": New(o)}
	stop := map[string]func(){}
	for name, w := range workers {
		w.Set(r.setting)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); w.Run(ctx) }()
		stop[name] = func() { cancel(); <-done }
		t.Cleanup(stop[name])
	}
	leader := func(not string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var leads, stands []string
			for name, w := range workers {
				switch st := w.Status(); {
				case st.Leader:
					leads = append(leads, name)
				case st.Standby:
					stands = append(stands, name)
				}
			}
			if len(leads) == 1 && leads[0] != not && (not != "" || len(stands) == 1) {
				return leads[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("leading %v, standing by %v", leads, stands)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	first := leader("")
	doc := r.addFile("Week 1", "application/pdf", pdfOf(1))
	if rec := r.waitText(doc); rec.Status != core.TextDone {
		t.Errorf("the text: %+v", rec)
	}
	if j := r.waitJob(store.JobDone); j.Worker != first {
		t.Errorf("claimed by %s, not the leader %s", j.Worker, first)
	}
	stop[first]()
	second := leader(first)
	doc = r.addFile("Week 2", "application/pdf", pdfOf(1))
	if rec := r.waitText(doc); rec.Status != core.TextDone {
		t.Errorf("the text after the first stopped: %+v", rec)
	}
	js, err := r.st.TranscriptionJobs(t.Context(), store.JobQuery{Limit: 10})
	if err != nil || len(js) != 2 || js[0].Worker != second {
		t.Errorf("the jobs: %+v, %v", js, err)
	}
}

// What is not transcribed is said so to Core: a PDF of more pages than a
// document may have, one that needs a password, a file of a type not
// transcribed, a workbook, an empty text file; a text file is its own
// text, with no model's call; a document that does not fit in what the
// day's pages leave is skipped, one that does is transcribed, and with the
// day's pages spent nothing more is claimed that day.
func TestSkips(t *testing.T) {
	r := newRig(t)
	r.credential()
	limit := 6
	r.setting.Site.MaxPages, r.setting.Site.PerDayPages, r.setting.Site.Concurrency = 5, &limit, 1
	docs := map[string]string{
		"too many":  r.addFile("Long", "application/pdf", pdfOf(6)),
		"encrypted": r.addFile("Locked", "application/pdf", doctexttest.PDFWith(doctexttest.PDFOptions{Encrypt: "aes256", UserPassword: "secret"}, doctexttest.PDFPage{Lines: []string{"x"}})),
		"zip":       r.addFile("Archive", "application/zip", []byte("PK\x03\x04 not a document")),
		"workbook":  r.addFile("Marks", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", doctexttest.XLSX(doctexttest.Sheet{Name: "A"})),
		"empty":     r.addFile("Blank", "text/plain", []byte("  \n")),
		"text":      r.addFile("Notes", "text/markdown; charset=utf-8", []byte("# Notes\n\nRead chapter 3.")),
		"fits":      r.addFile("Short", "application/pdf", pdfOf(4)),
		"over":      r.addFile("Next", "application/pdf", pdfOf(3)),
		"last":      r.addFile("Last", "application/pdf", pdfOf(2)),
	}
	r.run()
	want := map[string][2]string{
		"too many": {core.TextSkipped, ReasonTooManyPages}, "encrypted": {core.TextSkipped, ReasonEncrypted},
		"zip": {core.TextSkipped, ReasonUnsupportedFormat}, "workbook": {core.TextSkipped, ReasonUnsupportedFormat},
		"empty": {core.TextSkipped, ReasonEmpty}, "text": {core.TextDone, ""}, "fits": {core.TextDone, ""},
		"over": {core.TextSkipped, ReasonQuotaExhausted}, "last": {core.TextDone, ""},
	}
	for name, doc := range docs {
		rec := r.waitText(doc)
		if rec.Status != want[name][0] || rec.Reason != want[name][1] {
			t.Errorf("%s: %+v, want %v", name, rec, want[name])
		}
		if name == "text" && (rec.Body != "# Notes\n\nRead chapter 3." || rec.Model != TextFileModel || rec.Pages != 1) {
			t.Errorf("the text file's text: %+v", rec)
		}
	}
	if n := len(r.model.requests()); n != 2 {
		t.Errorf("%d model calls, want those of the two PDFs that fit", n)
	}
	// The day's six pages spent: the next is not claimed.
	next := r.addFile("Tomorrow's", "application/pdf", pdfOf(1))
	time.Sleep(300 * time.Millisecond)
	if rec, _ := r.fc.Text(next); rec.Status != core.TextPending {
		t.Errorf("claimed with the day's pages spent: %+v", rec)
	}
}

// A presentation is converted to PDF, its slides transcribed under slides'
// headings with their speaker notes given beside them; one of more pages
// than LibreOffice made is too many; one LibreOffice cannot convert
// fails; with no LibreOffice, it is not transcribed.
func TestOfficeFiles(t *testing.T) {
	const pptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "Sorting", Notes: "Ask who has seen quicksort."}, doctexttest.Slide{Title: "Merge"},
		doctexttest.Slide{Title: "End"})
	r := newRig(t)
	r.credential()
	doc := r.addFile("Week 3", pptx, deck)
	r.run()
	rec := r.waitText(doc)
	if rec.Status != core.TextDone || rec.Pages != 3 || !strings.HasPrefix(rec.Body, "## 投影片 1\n\nText of 1.") ||
		!strings.Contains(rec.Body, "## 投影片 3") {
		t.Fatalf("the deck's text: %+v", rec)
	}
	req := r.model.requests()[0].Messages[0].Parts[0].Text
	if !strings.Contains(req, "slides 1 to 3 of 3") || !strings.Contains(req, "Slide 1:\nAsk who has seen quicksort.") {
		t.Errorf("the deck asked for as %q", req)
	}

	r.office.mu.Lock()
	r.office.capped = true
	r.office.mu.Unlock()
	long := r.addFile("Long deck", pptx, deck)
	if rec := r.waitText(long); rec.Status != core.TextSkipped || rec.Reason != ReasonTooManyPages {
		t.Errorf("a deck capped: %+v", rec)
	}
	r.office.mu.Lock()
	r.office.capped, r.office.failWith = false, office.StatusFailed
	r.office.mu.Unlock()
	broken := r.addFile("Broken", pptx, deck)
	if rec := r.waitText(broken); rec.Status != core.TextFailed || rec.Reason != ReasonConversionFailed {
		t.Errorf("a deck LibreOffice could not convert: %+v", rec)
	}
	r.office.mu.Lock()
	r.office.off = true
	r.office.mu.Unlock()
	none := r.addFile("Word", "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "x"}}}))
	if rec := r.waitText(none); rec.Status != core.TextSkipped || rec.Reason != ReasonUnsupportedFormat {
		t.Errorf("with no LibreOffice: %+v", rec)
	}
}

// A model that takes pictures and no PDFs is given each page drawn, five
// a call; an image file is one page.
func TestPicturesForAModelOfPictures(t *testing.T) {
	r := newRig(t)
	r.credential()
	o := offer
	o.Provider = llm.ProviderQwen
	yes := true
	o.Capabilities.FileInput = &yes
	r.setting.Offer = &o
	r.model.provider, r.model.limits = llm.ProviderQwen, llm.FileLimits{}
	doc := r.addFile("Week 1", "application/pdf", pdfOf(7))
	pic := r.addFile("Diagram", "image/png", []byte("\x89PNG not really"))
	r.run()
	if rec := r.waitText(doc); rec.Status != core.TextDone || !strings.Contains(rec.Body, "## 第 7 頁") {
		t.Fatalf("the PDF drawn: %+v", rec)
	}
	if rec := r.waitText(pic); rec.Status != core.TextDone || rec.Pages != 1 || !strings.HasPrefix(rec.Body, "## 第 1 頁") {
		t.Fatalf("the picture: %+v", rec)
	}
	if !slices.Equal(r.office.drawn, [][2]int{{1, 5}, {6, 7}}) || len(r.office.ranges) != 0 {
		t.Errorf("drawn %v, cut %v", r.office.drawn, r.office.ranges)
	}
	for _, req := range r.model.requests() {
		for _, p := range req.Messages[0].Parts[1:] {
			if p.File == nil || !strings.HasPrefix(p.File.MIME, "image/") {
				t.Errorf("a part: %+v", p)
			}
		}
	}
}

// A model's failure that may pass is tried again, three times at most;
// one that will not fails the version (model_error). Text cut off at the
// output bound is asked for again in halves.
func TestModelFailures(t *testing.T) {
	r := newRig(t)
	r.credential()
	r.model.answer = func(req *llm.Request, n int) (*llm.Response, error) {
		first, last, _ := asked(req)
		switch {
		case n <= 2:
			return nil, &llm.Error{Kind: llm.ErrOverloaded, Status: 529}
		case last-first == 3:
			return &llm.Response{Parts: []llm.Part{llm.Text(heading(first, false) + "\n\ncut")}, Stop: llm.StopMaxTokens}, nil
		}
		return transcribed(req), nil
	}
	doc := r.addFile("Week 1", "application/pdf", pdfOf(4))
	r.run()
	rec := r.waitText(doc)
	if rec.Status != core.TextDone || strings.Contains(rec.Body, "cut") || !strings.Contains(rec.Body, "Text of 4.") {
		t.Fatalf("after two overloads and a cut: %+v", rec)
	}
	if !slices.Equal(r.office.ranges, [][2]int{{1, 2}, {3, 4}}) {
		t.Errorf("halves: %v", r.office.ranges)
	}
	r.model.mu.Lock()
	r.model.answer = func(*llm.Request, int) (*llm.Response, error) {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Status: 400, Message: "no such model"}
	}
	r.model.mu.Unlock()
	bad := r.addFile("Week 2", "application/pdf", pdfOf(1))
	if rec := r.waitText(bad); rec.Status != core.TextFailed || rec.Reason != ReasonModelError {
		t.Errorf("a model that refuses the request: %+v", rec)
	}
}

// Staff writing the text while it is transcribed: Core refuses the
// completion, and the job is dropped, the staff's text kept.
func TestEditedByStaff(t *testing.T) {
	r := newRig(t)
	r.credential()
	sato := r.fc.AddPerson("Sato")
	m, err := r.fc.Seat(sato.ID, r.co.ID, fakecore.SeatOptions{Preset: "instructor"})
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	r.model.answer = func(req *llm.Request, _ int) (*llm.Response, error) {
		if err := r.fc.EditText(doc, m.ID, "## 第 1 頁\n\nThe staff's"); err != nil {
			return nil, err
		}
		return transcribed(req), nil
	}
	doc = r.addFile("Week 1", "application/pdf", pdfOf(1))
	r.run()
	j := r.waitJob(store.JobDropped)
	if j.Reason != core.ReasonEditedByStaff {
		t.Errorf("the job: %+v", j)
	}
	if rec, _ := r.fc.Text(doc); rec.Source != core.SourceStaff || rec.Body != "## 第 1 頁\n\nThe staff's" {
		t.Errorf("the staff's text: %+v", rec)
	}
}

// A credential Core refuses (revoked: 401) is noted as refused, and
// nothing is claimed until another is given, which is taken without a
// restart.
func TestCredentialRejected(t *testing.T) {
	r := newRig(t)
	tok := r.credential()
	if err := r.fc.RevokeServiceToken(tok.CredentialID); err != nil {
		t.Fatal(err)
	}
	doc := r.addFile("Week 1", "application/pdf", pdfOf(1))
	r.run()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := r.st.TranscriptionCredential(t.Context())
		if err == nil && c.RejectedAt != nil {
			if !strings.Contains(c.LastError, "401") {
				t.Errorf("why: %q", c.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refusal is not noted: %+v %v", c, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := testutil.ToFloat64(r.m.TranscribeClaimErrors.WithLabelValues("unauthenticated")); got < 1 {
		t.Errorf("transcribe_claim_errors_total{unauthenticated} = %v", got)
	}
	r.credential()
	r.svc.Set(r.setting)
	if rec := r.waitText(doc); rec.Status != core.TextDone {
		t.Errorf("with a new credential: %+v", rec)
	}
}

// With no credential, or one Core refused, no offer, an offer withdrawn,
// or one whose model takes no files, nothing is claimed; so with the
// plan's dollars across the school's key spent; and Blocked says why.
func TestBlocked(t *testing.T) {
	no := false
	for _, c := range []struct {
		name string
		edit func(r *rig)
		why  string
	}{
		{"no credential", func(*rig) {}, BlockedNoCredential},
		{"refused", func(r *rig) {
			r.credential()
			c, _ := r.st.TranscriptionCredential(r.t.Context())
			if err := r.st.NoteTranscriptionCredential(r.t.Context(), c.SecretID, false, time.Now(), "Core answered 401"); err != nil {
				r.t.Fatal(err)
			}
		}, BlockedCredentialRejected},
		{"no offer", func(r *rig) { r.credential(); r.setting.Offer, r.setting.Site.Offer = nil, "" }, BlockedNoOffer},
		{"withdrawn", func(r *rig) { r.credential(); r.setting.Offer = nil }, BlockedOfferUnavailable},
		{"no files", func(r *rig) {
			r.credential()
			o := offer
			o.Capabilities.FileInput = &no
			r.setting.Offer = &o
			r.model.caps.FileInput = false
		}, BlockedOfferUnavailable},
		{"dollars spent", func(r *rig) {
			r.credential()
			one := 0.000001
			r.setting.PerDayUSD = &one
			if err := r.st.RecordLLMCall(r.t.Context(), store.LLMCall{ID: "c1", AgentID: "agt_1", KeySource: config.KeySchool, CostPUSD: 5_000_000}); err != nil {
				r.t.Fatal(err)
			}
		}, BlockedQuotaExhausted},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			c.edit(r)
			doc := r.addFile("Week 1", "application/pdf", pdfOf(1))
			r.run()
			time.Sleep(300 * time.Millisecond)
			if rec, _ := r.fc.Text(doc); rec.Status != core.TextPending || rec.Attempts != 0 {
				t.Errorf("claimed: %+v", rec)
			}
			if why := r.svc.Blocked(t.Context()); why != c.why {
				t.Errorf("blocked by %q, want %q", why, c.why)
			}
		})
	}
	r := newRig(t)
	r.credential()
	r.svc.Set(r.setting)
	if why := r.svc.Blocked(t.Context()); why != "" {
		t.Errorf("nothing stops it, and it is blocked by %q", why)
	}
	r.setting.Site.Enabled = false
	r.svc.Set(r.setting)
	if why := r.svc.Blocked(t.Context()); why != "" {
		t.Errorf("off, and blocked by %q", why)
	}
}

// Where the operator turns it off, or it lacks what it needs, the
// transcriber cannot run, whatever the site says; against a Core without
// the service, Check says the Core is too old.
func TestAvailability(t *testing.T) {
	if st := New(Options{Mode: config.TranscribeOff, Keeps: true, CoreBaseURL: "http://core", Store: memstore.New()}).Status(); st.Available ||
		st.Reason != ReasonOperatorOff || st.Detail != "TRANSCRIBE=off" {
		t.Errorf("TRANSCRIBE=off: %+v", st)
	}
	if st := New(Options{Mode: config.TranscribeOn, CoreBaseURL: "http://core", Store: memstore.New()}).Status(); st.Available ||
		st.Reason != ReasonOperatorOff || !strings.Contains(st.Detail, "KMS_KEY_ID") {
		t.Errorf("without its store and key: %+v", st)
	}
	r := newRig(t)
	st, err := r.svc.Check(t.Context())
	if err != nil || !st.Available {
		t.Errorf("against the fake Core: %+v, %v", st, err)
	}
	// A Core from before the service: its catalogue has none of its tools.
	fc := fakecore.New(fakecore.Options{})
	t.Cleanup(fc.Shutdown)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := httptest.NewRecorder()
		fc.Handler().ServeHTTP(rec, req)
		var cat map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
			t.Error(err)
		}
		cat["tools"] = slices.DeleteFunc(cat["tools"].([]any), func(tool any) bool {
			return strings.HasPrefix(tool.(map[string]any)["name"].(string), "document_text.")
		})
		_ = json.NewEncoder(w).Encode(cat)
	}))
	defer old.Close()
	st, err = New(Options{Store: memstore.New(), Keeps: true, CoreBaseURL: old.URL}).Check(t.Context())
	if err != nil || st.Available || st.Reason != ReasonCoreTooOld {
		t.Errorf("against a Core too old: %+v, %v", st, err)
	}
	if _, err := New(Options{Store: memstore.New(), Keeps: true, CoreBaseURL: "http://127.0.0.1:1"}).Check(t.Context()); err == nil {
		t.Error("a Core that cannot be reached is an answer")
	}
	var nilSvc *Service
	if st := nilSvc.Status(); st.Available {
		t.Errorf("no transcriber: %+v", st)
	}
}

// fit keeps a text within its bound, cut before the last page's heading
// that leaves room for the line saying the rest is left out.
func TestFit(t *testing.T) {
	text := "## 第 1 頁\n\n" + strings.Repeat("a", 40) + "\n\n## 第 2 頁\n\n" + strings.Repeat("b", 40) + "\n\n## 第 3 頁\n\n" + strings.Repeat("c", 40)
	if got := fit(text, len(text)); got != text {
		t.Errorf("a text that fits: %q", got)
	}
	got := fit(text, len(text)-1)
	if !strings.HasSuffix(got, TooLongLine+"\n") || !strings.Contains(got, "## 第 2 頁") || strings.Contains(got, "## 第 3 頁") ||
		len(got) > len(text)-1 {
		t.Errorf("cut: %q", got)
	}
	if got := fit(strings.Repeat("一", 100), 60); len(got) > 60 || !strings.HasSuffix(got, TooLongLine+"\n") {
		t.Errorf("no heading to cut at: %q", got)
	}
}

// clean takes a code fence from around a whole text, and gives the first
// page its heading where the model wrote none.
func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"```markdown\n## 第 3 頁\n\nx\n```": "## 第 3 頁\n\nx",
		"x":                               "## 第 3 頁\n\nx",
		"## 第 3 頁\n\n```go\ncode\n```":    "## 第 3 頁\n\n```go\ncode\n```",
		"  ":                              "",
	} {
		if got := clean(in, 3, false); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clean("x", 2, true); got != "## 投影片 2\n\nx" {
		t.Errorf("a slide: %q", got)
	}
}

// How an offer's model is given pages: a PDF where its API and provider
// take PDFs, pictures where it takes files and not PDFs, and none where it
// takes no files.
func TestInputOf(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		m    config.Model
		want Input
	}{
		{config.Model{Adapter: llm.AdapterGemini}, InputPDF},
		{config.Model{Adapter: llm.AdapterOpenAIChat}, InputPDF},
		{config.Model{Adapter: llm.AdapterOpenAIChat, BaseURL: "https://x.openai.azure.com/openai"}, InputPDF},
		{config.Model{Adapter: llm.AdapterAnthropic}, InputPDF},
		{config.Model{Adapter: llm.AdapterOpenAIResponses}, InputPDF},
		{config.Model{Adapter: llm.AdapterOpenAIChat, BaseURL: "https://api.deepseek.com"}, InputNone},
		{config.Model{Adapter: llm.AdapterOpenAIChat, Provider: llm.ProviderQwen, Capabilities: config.Capabilities{FileInput: &yes}}, InputImages},
		{config.Model{Adapter: llm.AdapterGemini, Capabilities: config.Capabilities{FileInput: &no}}, InputNone},
		{config.Model{Adapter: llm.AdapterBedrockConverse}, InputNone},
	} {
		if got := InputOf(c.m); got != c.want {
			t.Errorf("InputOf(%+v) = %s, want %s", c.m, got, c.want)
		}
	}
}
