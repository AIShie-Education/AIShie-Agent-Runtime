package rendition

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
)

const (
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	pptxType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// fakeConverter makes a PDF of pages pages of every file, or fails with
// err; with block, it waits until block is closed or its context ends.
type fakeConverter struct {
	mu      sync.Mutex
	pages   int
	pdf     []byte
	err     error
	block   chan struct{}
	calls   []office.Format
	running int
	most    int
	stopped int
}

func (f *fakeConverter) Describe() string { return "LibreOffice 9.9 fake" }

func (f *fakeConverter) Rendition(ctx context.Context, data []byte, fm office.Format, maxBytes int64) (*office.Output, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fm)
	f.running++
	f.most = max(f.most, f.running)
	block, pdf, pages, err := f.block, f.pdf, f.pages, f.err
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.stopped++
			f.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if pdf == nil {
		pdf = []byte("%PDF-1.7\n% made of " + fm.Ext + "\n")
	}
	if int64(len(pdf)) > maxBytes {
		return nil, office.ErrTooLarge
	}
	return &office.Output{Data: pdf, Pages: pages}, nil
}

func (f *fakeConverter) formats() []office.Format {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]office.Format(nil), f.calls...)
}

// world is a fake Core with a course, the runtime's credential, and a
// worker on it.
type world struct {
	t    *testing.T
	fc   *fakecore.Core
	srv  *httptest.Server
	co   fakecore.Course
	svc  fakecore.Token
	conv *fakeConverter
	m    *metrics.Metrics
	reg  *prometheus.Registry
	logs *syncWriter

	mu   sync.Mutex
	cred string
}

func newWorld(t *testing.T, fo fakecore.Options) *world {
	t.Helper()
	fc := fakecore.New(fo)
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(fc.Shutdown)
	w := &world{t: t, fc: fc, srv: srv, co: fc.AddCourse("CS101"), conv: &fakeConverter{pages: 3}, reg: prometheus.NewRegistry(),
		logs: &syncWriter{b: &bytes.Buffer{}}}
	w.m = metrics.New(w.reg)
	w.svc = fc.IssueRuntimeServiceToken("runtime")
	w.cred = w.svc.Token
	return w
}

func (w *world) setCredential(token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cred = token
}

// options are a worker's on the world, its timings short.
func (w *world) options(cfg Config) Options {
	return Options{Config: cfg, Converter: w.conv, CoreBaseURL: w.srv.URL, CoreHTTP: w.srv.Client(), Metrics: w.m,
		Bucket: ratelimit.New(6000, 100), Log: slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Credential: func(context.Context) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.cred == "" {
				return "", errors.New("secret://core/agent_runtime: no such secret")
			}
			return w.cred, nil
		},
		Timing: Timing{Wait: time.Second, Backoff: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond, Blocked: 50 * time.Millisecond,
			Recheck: 50 * time.Millisecond}}
}

// run runs a worker of o until the test ends.
func (w *world) run(o Options) *Service {
	s := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	w.t.Cleanup(func() { cancel(); <-done })
	return s
}

// material puts up files as a material: their ids.
func (w *world) material(files ...fakecore.File) []string {
	w.t.Helper()
	_, ids, err := w.fc.AddFiles(w.co.ID, "Week", "", files...)
	if err != nil {
		w.t.Fatal(err)
	}
	return ids
}

// wait waits until the file's rendition is in a state other than queued
// or claimed, and returns it.
func (w *world) wait(fileID string) fakecore.RenditionRecord {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := w.fc.Rendition(fileID); ok && r.State != core.RenditionQueued && r.State != core.RenditionClaimed {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := w.fc.Rendition(fileID)
	w.t.Fatalf("the rendition is still %s", r.State)
	return r
}

type syncWriter struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (w *world) logText() string {
	time.Sleep(20 * time.Millisecond)
	w.logs.mu.Lock()
	defer w.logs.mu.Unlock()
	return w.logs.b.String()
}

// TestConverts: the worker claims each Office file of a material, of any
// family, converts it from the format its extension names, uploads its
// PDF and completes it done with the PDF's pages, which Core keeps; a text
// is never claimed; and nothing it logs names a file, a URL or the
// credential.
func TestConverts(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	ids := w.material(
		fakecore.File{Filename: "第四週 handout.docx", ContentType: docxType, Data: doctexttest.DOCX(doctexttest.Doc{})},
		fakecore.File{Filename: "Lecture 4.PPTX", ContentType: "application/octet-stream", Data: []byte("PK\x03\x04 a deck")},
		fakecore.File{Filename: "notes.txt", ContentType: "text/plain", Data: []byte("notes")},
		fakecore.File{Filename: "flow.odg", ContentType: "application/vnd.oasis.opendocument.graphics", Data: []byte("PK\x03\x04 a drawing")})
	s := w.run(w.options(Config{}))
	for _, id := range []string{ids[0], ids[1], ids[3]} {
		r := w.wait(id)
		if r.State != core.RenditionDone || r.Pages != 3 || !bytes.HasPrefix(r.PDF, []byte("%PDF-")) || r.Attempts != 1 {
			t.Errorf("the rendition: %+v", r)
		}
	}
	if _, ok := w.fc.Rendition(ids[2]); ok {
		t.Error("a text has a rendition")
	}
	var exts []string
	for _, f := range w.conv.formats() {
		exts = append(exts, f.Ext+":"+string(f.Family))
	}
	if strings.Join(exts, " ") != "docx:document pptx:slides odg:drawing" {
		t.Errorf("converted %v", exts)
	}
	if st := s.Status(); !st.Available || st.Blocked != "" {
		t.Errorf("the status: %+v", st)
	}
	// The job counts and logs itself just after Core is told.
	waitFor(t, func() bool { return testutil.ToFloat64(w.m.RenditionJobs.WithLabelValues(core.RenditionDone, "")) == 3 })
	logs := w.logText()
	for _, never := range []string{"handout", "Lecture", "http://", "/v1/blobs/", w.svc.Token, "fakerendition."} {
		if strings.Contains(logs, never) {
			t.Errorf("the logs hold %q:\n%s", never, logs)
		}
	}
	if !strings.Contains(logs, `"status":"done"`) || !strings.Contains(logs, `"ext":"docx"`) {
		t.Errorf("the logs say nothing of the renditions:\n%s", logs)
	}
}

// TestReasons: a file protected by a password (an Office Open XML file
// encrypted in its container, an OpenDocument file whose manifest says
// so) is skipped password_protected, and one not held as an Office file
// unsupported, before LibreOffice is run; LibreOffice failing is failed
// conversion_failed, too slow failed timeout, a PDF past Core's bound
// skipped too_large; and a PDF Core refuses as none is failed
// conversion_failed.
func TestReasons(t *testing.T) {
	encryptedOOXML := append(append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 64)...), utf16("EncryptedPackage")...)
	encryptedODF := doctexttest.Zip([2]string{"mimetype", "application/vnd.oasis.opendocument.text"},
		[2]string{"META-INF/manifest.xml", `<manifest:manifest><manifest:file-entry manifest:full-path="content.xml"><manifest:encryption-data/></manifest:file-entry></manifest:manifest>`})
	for _, tc := range []struct {
		name    string
		data    []byte
		err     error
		pdf     []byte
		status  string
		reason  string
		convert bool
	}{
		{"encrypted.docx", encryptedOOXML, nil, nil, core.RenditionSkipped, core.RenditionPasswordProtected, false},
		{"encrypted.odt", encryptedODF, nil, nil, core.RenditionSkipped, core.RenditionPasswordProtected, false},
		{"page.docx", []byte("<html>not a Word file</html>"), nil, nil, core.RenditionSkipped, core.RenditionUnsupported, false},
		{"broken.docx", []byte("PK\x03\x04 broken"), office.ErrMalformed, nil, core.RenditionFailed, core.RenditionConversionFailed, true},
		{"slow.pptx", []byte("PK\x03\x04 slow"), office.ErrTimeout, nil, core.RenditionFailed, core.RenditionTimeout, true},
		{"huge.xlsx", []byte("PK\x03\x04 huge"), office.ErrTooLarge, nil, core.RenditionSkipped, core.RenditionTooLarge, true},
		{"odd.doc", []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, nil, []byte("not a pdf"), core.RenditionFailed, core.RenditionConversionFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, fakecore.Options{})
			w.conv.err, w.conv.pdf = tc.err, tc.pdf
			ids := w.material(fakecore.File{Filename: tc.name, ContentType: "application/octet-stream", Data: tc.data})
			w.run(w.options(Config{}))
			r := w.wait(ids[0])
			if r.State != tc.status || r.Reason != tc.reason {
				t.Errorf("%s %s, want %s %s", r.State, r.Reason, tc.status, tc.reason)
			}
			if converted := len(w.conv.formats()) > 0; converted != tc.convert {
				t.Errorf("converted: %v", converted)
			}
		})
	}
}

func utf16(s string) []byte {
	var b []byte
	for i := 0; i < len(s); i++ {
		b = append(b, s[i], 0)
	}
	return b
}

// TestTooLargeToFetch: a file larger than the worker fetches is skipped
// too_large unread.
func TestTooLargeToFetch(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	ids := w.material(fakecore.File{Filename: "big.pptx", ContentType: pptxType, Data: bytes.Repeat([]byte("PK\x03\x04"), 300)})
	o := w.options(Config{})
	o.MaxFileBytes = 1000
	w.run(o)
	if r := w.wait(ids[0]); r.State != core.RenditionSkipped || r.Reason != core.RenditionTooLarge || len(w.conv.formats()) != 0 {
		t.Errorf("the rendition: %+v", r)
	}
}

// TestLeaseLost: the claim is renewed while the file is converted; once
// Core says it is lost (it lapsed and another claimed the file), the
// conversion stops and nothing is uploaded or completed under it: the
// other claim's work stands.
func TestLeaseLost(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	w.conv.block = make(chan struct{})
	ids := w.material(fakecore.File{Filename: "slow.docx", ContentType: docxType, Data: []byte("PK\x03\x04 slow")})
	o := w.options(Config{})
	o.Timing.Renew = 50 * time.Millisecond
	w.run(o)
	deadline := time.Now().Add(5 * time.Second)
	for len(w.conv.formats()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// It lapses, and another claims it, and holds it: the worker's
	// renewal may come between the two, and hold it again first.
	other := core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: w.srv.URL, HTTPClient: w.srv.Client(), Once: true,
		Credential: func(context.Context) (string, error) { return w.svc.Token, nil }}))
	var claimed []core.ClaimedRendition
	for len(claimed) == 0 && time.Now().Before(deadline) {
		if err := w.fc.LapseRenditionClaim(ids[0]); err != nil {
			t.Fatal(err)
		}
		var err error
		if claimed, err = other.ClaimRenditions(t.Context(), 1, time.Hour, 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(claimed) != 1 || claimed[0].Attempt != 2 {
		t.Fatalf("claimed again: %+v", claimed)
	}
	for time.Now().Before(deadline) {
		w.conv.mu.Lock()
		stopped := w.conv.stopped
		w.conv.mu.Unlock()
		if stopped == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, _ := w.fc.Rendition(ids[0]); r.State != core.RenditionClaimed || r.Attempts != 2 {
		t.Errorf("the rendition: %+v", r)
	}
	waitFor(t, func() bool {
		return testutil.ToFloat64(w.m.RenditionJobs.WithLabelValues("dropped", DroppedLeaseLost)) == 1
	})
}

// TestConcurrency: the worker converts at most RENDITIONS_CONCURRENCY
// files at once, and claims no more than it converts.
func TestConcurrency(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	w.conv.block = make(chan struct{})
	var ids []string
	for _, name := range []string{"a.docx", "b.docx", "c.docx"} {
		ids = append(ids, w.material(fakecore.File{Filename: name, ContentType: docxType, Data: []byte("PK\x03\x04 " + name)})[0])
	}
	s := w.run(w.options(Config{Concurrency: 2}))
	deadline := time.Now().Add(5 * time.Second)
	for s.Status().Inflight < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	claimed := 0
	for _, id := range ids {
		if r, _ := w.fc.Rendition(id); r.State == core.RenditionClaimed {
			claimed++
		}
	}
	if claimed != 2 {
		t.Errorf("%d claimed at once", claimed)
	}
	close(w.conv.block)
	for _, id := range ids {
		if r := w.wait(id); r.State != core.RenditionDone {
			t.Errorf("the rendition: %+v", r)
		}
	}
	w.conv.mu.Lock()
	most := w.conv.most
	w.conv.mu.Unlock()
	if most != 2 {
		t.Errorf("%d converted at once", most)
	}
}

// TestCredential: without the runtime's credential nothing is claimed,
// and the worker says so; given one, it claims; one Core refuses (revoked)
// stops it again, until another is put in its place.
func TestCredential(t *testing.T) {
	w := newWorld(t, fakecore.Options{})
	w.setCredential("")
	ids := w.material(fakecore.File{Filename: "a.docx", ContentType: docxType, Data: []byte("PK\x03\x04 a")})
	s := w.run(w.options(Config{}))
	waitFor(t, func() bool { return s.Status().Blocked == BlockedNoCredential })
	if r, _ := w.fc.Rendition(ids[0]); r.State != core.RenditionQueued {
		t.Errorf("claimed without a credential: %+v", r)
	}
	w.setCredential(w.svc.Token)
	if r := w.wait(ids[0]); r.State != core.RenditionDone {
		t.Errorf("the rendition: %+v", r)
	}
	waitFor(t, func() bool { return s.Status().Blocked == "" })

	if err := w.fc.RevokeServiceToken(w.svc.CredentialID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Status().Blocked == BlockedCredentialRejected })
	ids = w.material(fakecore.File{Filename: "b.docx", ContentType: docxType, Data: []byte("PK\x03\x04 b")})
	fresh := w.fc.IssueRuntimeServiceToken("again")
	w.setCredential(fresh.Token)
	if r := w.wait(ids[0]); r.State != core.RenditionDone {
		t.Errorf("the rendition after a new credential: %+v", r)
	}
	if strings.Contains(w.logText(), fresh.Token) {
		t.Error("the logs hold the credential")
	}
}

// TestUnavailable: RENDITIONS=off, no LibreOffice, or no Core, and the
// worker does not run; a Core from before renditions makes it say so, and
// claim nothing.
func TestUnavailable(t *testing.T) {
	w := newWorld(t, fakecore.Options{WithoutRenditions: true})
	for _, tc := range []struct {
		name string
		edit func(*Options)
		want string
	}{
		{"off", func(o *Options) { o.Config.Mode = ModeOff }, "RENDITIONS=off"},
		{"no converter", func(o *Options) { o.Converter, o.Unavailable = nil, "LibreOffice is not installed" }, "LibreOffice is not installed"},
		{"no core", func(o *Options) { o.CoreBaseURL = "" }, "CORE_BASE_URL"},
	} {
		o := w.options(Config{})
		tc.edit(&o)
		st := New(o).Status()
		if st.Available || st.Reason != ReasonOperatorOff || !strings.Contains(st.Detail, tc.want) {
			t.Errorf("%s: %+v", tc.name, st)
		}
	}
	s := New(w.options(Config{}))
	st, err := s.Check(t.Context())
	if err != nil || st.Available || st.Reason != ReasonCoreTooOld {
		t.Errorf("a Core from before renditions: %+v, %v", st, err)
	}
	if !strings.HasPrefix(s.String(), "off: ") {
		t.Errorf("described as %q", s.String())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestConfigCheck: the settings' bounds.
func TestConfigCheck(t *testing.T) {
	if err := (Config{}).Check(); err != nil {
		t.Errorf("the defaults: %v", err)
	}
	d := Config{}.WithDefaults()
	if d.Mode != ModeAuto || d.Concurrency != 1 || d.Timeout != 5*time.Minute || d.Lease != 10*time.Minute {
		t.Errorf("the defaults: %+v", d)
	}
	for _, c := range []Config{{Mode: "sometimes"}, {Concurrency: 9}, {Timeout: time.Second}, {Timeout: 2 * time.Hour}, {Lease: 30 * time.Second},
		{Lease: 2 * time.Hour}} {
		if c.Check() == nil {
			t.Errorf("%+v passes", c)
		}
	}
}
