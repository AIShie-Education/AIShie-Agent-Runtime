package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/transcribe"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// TestTranscriptionAgainstCore is the transcriber against the real Core
// (AIShie-Core #43), in a world of its own, run alone once the scenarios
// are over: Core's transcription service is one for the whole site, and
// its queue hands out every course's versions, so a transcriber running
// beside the scenarios would give their documents text versions under
// them. It claims the other worlds' versions too, which is no matter now.
func TestTranscriptionAgainstCore(t *testing.T) {
	c, root := liveCore(t)
	w := newWorld(t, c, root, "transcription")
	t.Run("transcription", func(t *testing.T) { transcription(t, w) })
	t.Run("no-token-in-any-log", func(t *testing.T) { noTokenInAnyLog(t, []*world{w}) })
}

// The transcriber's offer, and its model at the scripted server.
const (
	transcriberOffer = "transcriber"
	transcriberLabel = "E2E Transcriber"
	transcriberModel = "e2e-transcribe-model"
)

// pagesAskedRe is what a transcriber's request names of the pages it
// gives: "pages 1 to 3 of 3", "slide 2 of 2".
var pagesAskedRe = regexp.MustCompile(`(page|slide)s? (\d+)(?: to (\d+))? of (\d+)`)

// notesRe are the speaker notes a transcriber's request gives of a slide.
var notesRe = regexp.MustCompile(`(?m)^Slide (\d+):\n(.+)$`)

// transcribing is the transcriber's model: a request of the transcriber's
// (its system prompt transcribe.Prompt) it answers with each page or slide
// the request names under its heading, a line naming it, a picture
// described, and a slide's speaker notes where it was given them; any other
// request, as textReader does.
func transcribing(req fakellm.ChatRequest) fakellm.ChatResponse {
	if system(req) != transcribe.Prompt {
		return textReader(req)
	}
	q := question(req)
	m := pagesAskedRe.FindStringSubmatch(q)
	if m == nil {
		return fakellm.Reply("[無法辨識]")
	}
	first, _ := strconv.Atoi(m[2])
	last := first
	if m[3] != "" {
		last, _ = strconv.Atoi(m[3])
	}
	notes := map[string]string{}
	for _, n := range notesRe.FindAllStringSubmatch(q, -1) {
		notes[n[1]] = n[2]
	}
	var b strings.Builder
	for n := first; n <= last; n++ {
		heading, unit := fmt.Sprintf("## 第 %d 頁", n), "page"
		if m[1] == "slide" {
			heading, unit = fmt.Sprintf("## 投影片 %d", n), "slide"
		}
		fmt.Fprintf(&b, "%s\n\nE2E transcription of %s %d.\n\n[圖：a diagram on %s %d]\n\n", heading, unit, n, unit, n)
		if note := notes[strconv.Itoa(n)]; note != "" {
			fmt.Fprintf(&b, "> 講者備註：%s\n\n", note)
		}
	}
	r := fakellm.Reply(strings.TrimSpace(b.String()))
	pages := last - first + 1
	r.Usage = &fakellm.Usage{PromptTokens: 1000 * pages, CompletionTokens: 200 * pages, TotalTokens: 1200 * pages}
	return r
}

// textQuestion asks Yuki's agent what a document's text says:
// "What does the text of <title> say?".
const textQuestionPrefix = "What does the text of "

// textReader is the answering model: to textQuestion it lists the course's
// documents, reads the one of the title asked for, and answers with whose
// text it was given and the text's first page; any other question as
// DefaultResponder does.
func textReader(req fakellm.ChatRequest) fakellm.ChatResponse {
	title, ok := strings.CutPrefix(question(req), textQuestionPrefix)
	if !ok {
		return fakellm.DefaultResponder(req)
	}
	title = strings.TrimSuffix(title, " say?")
	var results []string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results = append(results, m.Text())
		}
	}
	switch len(results) {
	case 0:
		return fakellm.CallTools(fakellm.FunctionCall{Name: "document_list", Arguments: `{}`})
	case 1:
		var env struct {
			Result struct {
				Documents []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"documents"`
			} `json:"result"`
		}
		_ = json.Unmarshal([]byte(results[0]), &env)
		for _, d := range env.Result.Documents {
			if d.Title == title {
				return fakellm.CallTools(fakellm.FunctionCall{Name: "document_get", Arguments: fmt.Sprintf(`{"document_id":%q}`, d.ID)})
			}
		}
		return fakellm.Reply("There is no document titled " + title + ".")
	}
	var got struct {
		File struct {
			GivenAs    string `json:"given_as"`
			TextSource string `json:"text_source"`
		} `json:"file"`
		FileText string `json:"file_text"`
	}
	_ = json.Unmarshal([]byte(results[len(results)-1]), &got)
	page, _, _ := strings.Cut(got.FileText, "\n\n[圖")
	return fakellm.Reply(fmt.Sprintf("Given as %s, %s: %s", got.File.GivenAs, got.File.TextSource, page))
}

// transcription: the site's administrator turns the transcriber on through
// the API, against the real Core. While it is off, a PDF Sato puts in the
// course waits in Core's queue, and nothing is claimed. Root issues the
// service's credential in Core, which the administrator hands to the
// runtime (tried with Core, sealed); the administrator prices the offer's
// model and turns the transcriber on with an offer of the school's plan
// whose model is the scripted server. The PDF, and a deck Sato puts in the
// course then, are transcribed: their text versions are done in Core, the
// scripted model's Markdown under each page's heading, a slide's speaker
// notes with it, and the offer's label as the model. The jobs list shows
// them, the cost report has a transcription line, and GET /info says it
// runs. Yuki's own agent, reading the PDF, is given its text version
// first, marked as the AI transcription it is. Turned off, a document put
// in the course then is claimed no more. No log, answer or row holds the
// service's token.
func transcription(t *testing.T, w *world) {
	audience := os.Getenv("E2E_RUNTIME_AUDIENCE")
	if audience == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_RUNTIME_AUDIENCE is not set, and CI is: start Core with scripts/ci-core.sh")
		}
		t.Skip("E2E_RUNTIME_AUDIENCE is not set: this Core makes no assertion for a runtime (scripts/ci-core.sh sets it)")
	}
	cat, err := core.FetchCatalogue(t.Context(), w.api.hc, w.api.base)
	if err != nil {
		t.Fatal(err)
	}
	if !core.HasService(cat) {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("the Core under test has no transcription service (document_text.queue): it is older than AIShie-Core #43")
		}
		t.Skip("the Core under test has no transcription service: it is older than AIShie-Core #43")
	}
	st, dbURL := runtimeStore(t)
	v, kek := keyring(t)
	w.addSecret("the key that seals the transcriber's runtime's secrets", kek)
	w.addSecret("the school's key", schoolKey)
	w.secretsDir = t.TempDir()
	keyFile := filepath.Join(w.secretsDir, "school", "keys", "e2e")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(schoolKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conv := officeOrNone(t)
	m := newModel(t, transcribing)
	yes := true
	yaml := &config.Config{Runtime: config.Runtime{School: config.School{Offers: []config.SchoolOffer{{
		ID: transcriberOffer, Label: transcriberLabel, Adapter: "openai_chat", Provider: "openai", Model: transcriberModel, BaseURL: m.URL(),
		KeyRef: "secret://school/keys/e2e", Capabilities: config.Capabilities{FileInput: &yes},
	}}}}}

	// Yuki's own agent, whose model takes files, reads the course's
	// documents: a runtime of its own, as another worker would be.
	takesFiles := map[string]any{"model": map[string]any{"capabilities": map[string]any{"file_input": true}}}
	reader := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own, over: takesFiles}}})
	reader.waitPolling("yuki-helper")
	tr := w.startTranscriber(t, st, v, yaml, conv)
	a := w.startAPI(t, st, audience, func(o *api.Options) {
		o.Vault, o.Hosting, o.Transcriber = v, staticHosting{yaml: yaml}, tr
	})
	type answer struct {
		code int
		body []byte
	}
	asAdmin := func(method, path, body string) answer {
		t.Helper()
		code, _, raw := a.do(t, method, "/runtime/api/v1/"+path, w.assertion(t, w.admin.token, "", audience), body)
		return answer{code, raw}
	}
	decodeAs := func(an answer, want int, v any) {
		t.Helper()
		if an.code != want {
			t.Fatalf("%d, want %d: %s", an.code, want, an.body)
		}
		if err := json.Unmarshal(an.body, v); err != nil {
			t.Fatalf("%v: %s", err, an.body)
		}
	}
	textOf := func(doc string) core.TextView {
		t.Helper()
		return result[struct {
			Text core.TextView `json:"text"`
		}](t, w.api, w.sato.token, "GET", w.path("/documents/"+doc+"/text"), nil).Text
	}
	transcriptions := func() int {
		n := 0
		for _, r := range m.Requests() {
			if system(r) == transcribe.Prompt {
				n++
			}
		}
		return n
	}

	// Off: available, and nothing claimed.
	var settings api.Settings
	decodeAs(asAdmin("GET", "admin/settings", ""), 200, &settings)
	if s := settings.Transcription; !s.Available || s.Enabled || s.State != api.TranscriptionOff || s.Credential.Status != api.CredentialNone {
		t.Fatalf("the transcriber's settings: %+v", s)
	}
	const handout = "Week 5 handout"
	pdfDoc := w.upload(t, w.sato, handout, "application/pdf", doctexttest.PDF(
		doctexttest.PDFPage{Lines: []string{"Week 5: hashing"}}, doctexttest.PDFPage{Lines: []string{"Open addressing"}}))
	time.Sleep(3 * time.Second)
	if tv := textOf(pdfDoc); tv.Status != core.TextPending {
		t.Fatalf("off, the handout's text is %+v", tv)
	}
	if n := transcriptions(); n != 0 {
		t.Fatalf("off, the transcriber called its model %d times", n)
	}

	// Root issues the service's credential in Core; the administrator
	// hands it to the runtime, prices the offer's model, and turns the
	// transcriber on.
	issued := result[struct {
		CredentialID string `json:"credential_id"`
		Token        string `json:"token"`
	}](t, w.api, w.root, "POST", "/v1/services/document_text/credentials", map[string]any{"label": "runtime e2e", "replace": true})
	w.addSecret("the transcription service's token", issued.Token)
	put, _ := json.Marshal(map[string]string{"token": issued.Token, "credential_id": issued.CredentialID})
	var ts api.TranscriptionSettings
	decodeAs(asAdmin("PUT", "admin/transcription/credential", string(put)), 200, &ts)
	if c := ts.Credential; c.Status != api.CredentialOK || c.Hint == nil || !strings.HasPrefix(*c.Hint, "aissvc_") || c.CredentialID == nil ||
		*c.CredentialID != issued.CredentialID {
		t.Fatalf("the credential handed over: %+v", c)
	}
	var row api.PriceRow
	decodeAs(asAdmin("POST", "admin/prices", `{"id":"e2e-transcribe","provider":"openai","model":"`+transcriberModel+
		`","from":"2025-01-01","usd_per_mtok":{"input":1,"output":4}}`), http.StatusCreated, &row)
	decodeAs(asAdmin("PATCH", "admin/settings", `{"transcription":{"enabled":true,"offer":"`+transcriberOffer+`"}}`), 200, &settings)
	if s := settings.Transcription; !s.Enabled || s.OfferStatus == nil || *s.OfferStatus != api.OfferStatusOK || s.State != api.TranscriptionRunning {
		t.Fatalf("turned on: %+v", s)
	}

	// The handout, and a deck put in the course now, are transcribed.
	deckDoc := ""
	if conv != nil {
		deckDoc = w.upload(t, w.sato, "Week 5 slides", doctexttest.PPTXType, doctexttest.PPTX(
			doctexttest.Slide{Title: "Hashing", Body: []doctexttest.Bullet{{Text: "Buckets"}}, Notes: "Ask about collisions."},
			doctexttest.Slide{Title: "Probing", Body: []doctexttest.Bullet{{Text: "Linear"}}}))
	}
	var done core.TextView
	eventually(t, answerWait, "the handout's text version done", func() bool {
		done = textOf(pdfDoc)
		return done.Status == core.TextDone
	})
	if done.Source != core.SourceAI || done.Model != transcriberLabel || done.Pages != 2 {
		t.Errorf("the handout's text version: %+v", done)
	}
	full := result[struct {
		Text core.TextView `json:"text"`
	}](t, w.api, w.sato.token, "GET", w.path("/documents/"+pdfDoc+"/text"), nil).Text
	if full.Body == nil || *full.Body != "## 第 1 頁\n\nE2E transcription of page 1.\n\n[圖：a diagram on page 1]\n\n"+
		"## 第 2 頁\n\nE2E transcription of page 2.\n\n[圖：a diagram on page 2]" {
		t.Errorf("the handout's text: %v", full.Body)
	}
	if deckDoc != "" {
		var deck core.TextView
		eventually(t, answerWait, "the deck's text version done", func() bool {
			deck = textOf(deckDoc)
			return deck.Status == core.TextDone
		})
		if deck.Body == nil || !strings.HasPrefix(*deck.Body, "## 投影片 1\n\nE2E transcription of slide 1.") ||
			!strings.Contains(*deck.Body, "> 講者備註：Ask about collisions.") || !strings.Contains(*deck.Body, "## 投影片 2") {
			t.Errorf("the deck's text: %v", deck.Body)
		}
	}

	// The jobs, the costs, and GET /info say so.
	var jobs api.JobList
	eventually(t, answerWait, "the handout's job in the list", func() bool {
		an := asAdmin("GET", "admin/transcription/jobs?status=done", "")
		if an.code != 200 || json.Unmarshal(an.body, &jobs) != nil {
			return false
		}
		for _, j := range jobs.Jobs {
			if j.DocumentID == pdfDoc {
				return j.Pages != nil && *j.Pages == 2 && j.Offer != nil && *j.Offer == transcriberOffer && j.CostUSD != nil && j.CourseID == w.course
			}
		}
		return false
	})
	var costs api.CostReport
	decodeAs(asAdmin("GET", "admin/costs?group=agent&key_source=school", ""), 200, &costs)
	var line *api.CostLine
	for _, g := range costs.Rows {
		if g.Key == store.CostKeyTranscription && g.AgentID == nil && len(g.Lines) == 1 {
			line = &g.Lines[0]
		}
	}
	if line == nil || line.Kind != api.CostKindTranscription || line.Calls < 1 || line.UnpricedCalls != 0 || line.CostUSD == "0.000000" {
		t.Errorf("the cost report's transcription line: %+v", costs.Rows)
	}
	var info api.Info
	if err := json.Unmarshal(mustGet(t, a, "/runtime/api/v1/info"), &info); err != nil || !info.Features.Transcription {
		t.Errorf("GET /info: %+v, %v", info.Features, err)
	}

	// Yuki's agent reads the handout's text version first.
	asked, msg := w.ask(t, w.yuki, w.own.member, textQuestionPrefix+handout+" say?")
	ans := w.waitAnswer(t, w.yuki, asked, w.own.member)
	want := "Given as text, AI transcription (" + transcriberLabel + "): ## 第 1 頁\n\nE2E transcription of page 1."
	if ans.text() != want || ans.replyTo() != msg {
		t.Errorf("Yuki's answer is %q; want %q", ans.text(), want)
	}

	// Off again: a document put in the course is claimed no more.
	decodeAs(asAdmin("PATCH", "admin/settings", `{"transcription":{"enabled":false}}`), 200, &settings)
	if settings.Transcription.State != api.TranscriptionOff {
		t.Errorf("turned off: %+v", settings.Transcription)
	}
	eventually(t, answerWait, "the transcriber standing down", func() bool { return !tr.Status().Leader })
	later := w.upload(t, w.sato, "Week 6 handout", "application/pdf", doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Week 6"}}))
	time.Sleep(3 * time.Second)
	if tv := textOf(later); tv.Status != core.TextPending {
		t.Errorf("turned off, a new document's text is %+v", tv)
	}

	// The credential forgotten, and revoked in Core, as the front end does.
	decodeAs(asAdmin("DELETE", "admin/transcription/credential", ""), 200, &ts)
	if ts.Credential.Status != api.CredentialNone {
		t.Errorf("the credential forgotten: %+v", ts.Credential)
	}
	w.api.call(t, http.StatusOK, w.root, "POST", "/v1/services/document_text/credentials/"+issued.CredentialID+"/revoke", nil)

	for what, text := range map[string]string{"the runtime's database": dumpDatabase(t, dbURL), "the API's answers": a.answers.String()} {
		if strings.Contains(text, issued.Token) || strings.Contains(text, issued.Token[len("aissvc_")+13:]) {
			t.Errorf("%s hold the service's token", what)
		}
	}
}

// officeOrNone is the worker's conversion of Office files on the
// LibreOffice and poppler installed here, or nil where they are not (a
// deck is then not transcribed), but with OFFICE_PDF_REQUIRED=1.
func officeOrNone(t *testing.T) *office.Service {
	t.Helper()
	cfg := office.Config{TempDir: t.TempDir()}
	c, err := office.NewConverter(t.Context(), cfg)
	var p *office.Pager
	if err == nil {
		p, err = office.NewPager(cfg)
	}
	if errors.Is(err, office.ErrUnavailable) {
		if os.Getenv("OFFICE_PDF_REQUIRED") == "1" {
			t.Fatalf("OFFICE_PDF_REQUIRED is set, and the conversion is not available: %v", err)
		}
		t.Logf("LibreOffice or poppler is not installed, so no deck is transcribed: %v", err)
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := office.NewService(ctx, office.ServiceOptions{Converter: c, Pager: p, Config: cfg})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// startTranscriber runs the transcriber as run does: its state in st, its
// sealed credential opened with v, the site's setting in force from the
// registry's builds of yaml ∪ the site's settings, which a watcher puts in
// force at each change (as run's hosting does), Office files converted by
// conv (none where nil). Its log is the world's.
func (w *world) startTranscriber(t *testing.T, st *pgstore.Store, v *vault.Vault, yaml *config.Config, conv *office.Service) *transcribe.Service {
	t.Helper()
	raw, redacted := &logBuffer{}, &logBuffer{}
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	logger := slog.New(teeHandler{redact.NewHandler(slog.NewJSONHandler(redacted, opts), nil), slog.NewJSONHandler(raw, opts)})
	w.addLog(t.Name()+" (the transcriber)", redacted.String)
	w.addLog(t.Name()+" (the transcriber, before redaction)", raw.String)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	o := transcribe.Options{
		Mode: config.TranscribeAuto, Store: st, Keeps: true, CoreBaseURL: w.api.base, CoreHTTP: &http.Client{Transport: tr},
		Secrets: secrets.Resolver{Dir: w.secretsDir, Sealed: vault.Opener{Vault: v, Store: st}},
		// runtime.yaml's offer, at an endpoint of the operator's: the
		// runtime's own client, as run gives it.
		ModelHTTP: &http.Client{Transport: tr}, Metrics: metrics.New(prometheus.NewRegistry()), Log: logger, Holder: "transcriber-w1",
		Timing: transcribe.Timing{Standby: time.Second, Blocked: time.Second, Backoff: 100 * time.Millisecond, BackoffMax: time.Second,
			ModelBackoff: 100 * time.Millisecond},
	}
	if conv != nil {
		o.Office = conv
	}
	svc := transcribe.New(o)
	ro := registry.Options{CoreBaseURL: w.api.base}
	build := func(ctx context.Context) (int64, error) {
		cfg, rev, err := registry.Build(ctx, yaml, st, ro)
		if err != nil {
			return 0, err
		}
		svc.Set(transcribe.SettingOf(cfg.Runtime, "", cfg.Runtime.Site.PriceTable(nil)))
		return rev, nil
	}
	rev, err := build(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	watcher := &registry.Watcher{Rev: st.RegistryRev, Listen: st.ListenRegistry, Log: logger, Poll: time.Hour, Rebuild: build}
	ctx, cancel := context.WithCancel(context.Background())
	runDone, watchDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(runDone); svc.Run(ctx) }()
	go func() { defer close(watchDone); watcher.Run(ctx, rev) }()
	t.Cleanup(func() {
		cancel()
		for _, done := range []chan struct{}{runDone, watchDone} {
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the transcriber did not stop within 30 s")
			}
		}
		if t.Failed() {
			t.Logf("the transcriber's log (redacted):\n%s", tail(redacted.String(), 200))
		}
	})
	return svc
}
