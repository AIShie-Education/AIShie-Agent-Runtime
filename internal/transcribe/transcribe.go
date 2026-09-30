// Package transcribe is the runtime's transcriber (docs/design.md §12): a
// module of its own, off unless the site's administrators turn it on, that
// gives every file of a course's documents its text version in Core
// (AIShie-Core #43; each file of a version its own since #49, a Core
// before it claiming versions of one file). It claims the files waiting
// from Core's queue with the site's service credential, each on its own
// and every call about it naming it (file_id), fetches each, converts an
// Office file to PDF (package office), counts its pages, has a model of
// the school's plan (one offer, which the administrators choose, on the
// school's key) transcribe it into Markdown a range of pages at a time
// with a fixed prompt (Prompt), joins the ranges, and writes the text back
// to Core, or why there is none (skipped, failed). It holds each claim
// while it works, and drops the work when Core says the claim was lost or
// staff wrote the text meanwhile.
//
// One worker at a time claims, whatever the number of replicas: the one
// that holds the store's lease "transcriber"; the others stand by, and one
// of them takes over when it lapses. Its model calls are the ledger's, of
// their own kind (store.CallTranscription), on the school's key: they
// count against the plan's ceiling across the whole key (per_day_usd), and
// no owner's or asker's quota. It keeps a record of what it did with each
// claim, a file (store.TranscriptionJob), for the administrators, and counts its
// work (metrics.Transcribe*). Nothing it logs or keeps holds the service's
// token, a key, or any document's text.
//
// The operator's environment is its ceiling (TRANSCRIBE, config.Env): off,
// it never runs; with a Core from before the service (core_too_old), or
// without the store, key and Core it needs, it cannot. Where it may run,
// the site's setting (config.SiteTranscription) turns it on and off, and
// chooses its offer and its limits, put in force by Set at every build of
// the registry, without a restart; so does a service credential given or
// forgotten.
package transcribe

import (
	"strconv"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// Why the transcriber cannot run here (Status.Reason), as the API says it
// (unavailable_reason).
const (
	// ReasonOperatorOff: the operator's environment does not let it run:
	// TRANSCRIBE=off, or what it needs (the store in PostgreSQL, the key
	// that seals secrets, CORE_BASE_URL) is not set.
	ReasonOperatorOff = "operator_off"
	// ReasonCoreTooOld: Core's catalogue has no transcription service
	// (document_text.queue): a Core from before AIShie-Core #43.
	ReasonCoreTooOld = "core_too_old"
)

// Why the transcriber, turned on, claims nothing (Blocked), as the API
// says it (blocked_reason).
const (
	BlockedNoCredential       = "no_credential"       // #nosec G101 -- a reason's code, not a credential.
	BlockedCredentialRejected = "credential_rejected" // #nosec G101 -- a reason's code, not a credential.
	BlockedNoOffer            = "no_offer"
	BlockedOfferUnavailable   = "offer_unavailable"
	BlockedQuotaExhausted     = "quota_exhausted"
)

// Reasons the transcriber gives Core for a version failed or skipped
// (store.TranscriptionJob.Reason too), which the front end words.
const (
	ReasonTooManyPages      = "too_many_pages"
	ReasonQuotaExhausted    = "quota_exhausted"
	ReasonUnsupportedFormat = "unsupported_format"
	ReasonEncrypted         = "encrypted"
	ReasonTooLarge          = "too_large"
	ReasonModelError        = "model_error"
	ReasonConversionFailed  = "conversion_failed"
	ReasonEmpty             = "empty"
)

// LeaseName is the store's lease the transcribing worker holds.
const LeaseName = "transcriber"

// TextFileModel is the model a text file's own text is recorded as made
// by: no model reads it.
const TextFileModel = "text file"

// TooLongLine ends a text cut short at the 2 MiB Core keeps.
const TooLongLine = "[本文過長，其餘頁面未收錄]"

// Input is how a model is given a document's pages.
type Input string

// The inputs.
const (
	// InputPDF: a range of pages as a PDF of its own, a file part.
	InputPDF Input = "pdf"
	// InputImages: each page drawn as a picture (PNG, 150 dpi), a file
	// part each: a model that takes pictures and no PDFs.
	InputImages Input = "images"
	// InputNone: the model takes no files, and cannot transcribe.
	InputNone Input = "none"
)

// InputOf is how the model of an offer is given pages: none where it
// takes no files (its adapter's defaults for its provider, and the offer's
// file_input); a PDF where its API and provider take PDFs (OpenAI's own
// and Azure's, Anthropic's, Gemini's, OpenAI's Responses, Bedrock's
// Converse); and otherwise each page as a picture, which every API that
// takes files takes (a server of the school's own, Qwen's, …).
func InputOf(m config.Model) Input {
	provider := m.EffectiveProvider()
	caps, _ := llm.Defaults(m.Adapter, provider)
	if f := m.Capabilities.FileInput; f != nil {
		caps.FileInput = *f
	}
	if !caps.FileInput {
		return InputNone
	}
	switch m.Adapter {
	case llm.AdapterGemini, llm.AdapterOpenAIResponses, llm.AdapterBedrockConverse:
		return InputPDF
	case llm.AdapterAnthropic:
		if provider == llm.ProviderAnthropic {
			return InputPDF
		}
	case llm.AdapterOpenAIChat:
		if provider == llm.ProviderOpenAI || provider == llm.ProviderAzure {
			return InputPDF
		}
	}
	return InputImages
}

// Prompt is what the model is told, the same for every document and
// every range of its pages (not configurable in this round). The page
// headings are fixed in Chinese whatever the document's language: the
// front end and the answering models' citations find pages by them.
const Prompt = `You transcribe the pages of a course document into Markdown, for a learning platform whose people, and whose other models, read your text in place of the file.

Rules:
- Transcribe faithfully, in the document's own language or languages. Do not translate, summarise, correct or add anything: write only what the pages show.
- Begin each page with a heading line of its own, exactly "## 第 N 頁", or for a slide of a presentation exactly "## 投影片 N", N being the page's or slide's number in the whole document, counting from 1, as the request gives it. Use these words whatever the document's language. Write nothing before the first heading.
- Keep the structure: headings within a page as ### and below, lists as lists, tables as Markdown tables, formulas in LaTeX ($…$ within a line, $$…$$ on lines of their own), code in fenced blocks with its language.
- Describe each picture, diagram, chart or screenshot in one bracketed line of its own, "[圖：…]", saying what it shows: a chart's axes, series and trend; a diagram's parts and how they connect; a screenshot's visible text and what it is of. Nothing it does not show.
- When you are given a slide's speaker notes, write them after the slide's content, as a quote: "> 講者備註：…".
- Mark what you cannot read as [無法辨識]; never guess it.
- No preamble, no closing remarks, and no code fence around the whole answer.`

// heading is page n's heading, a slide's when slides.
func heading(n int, slides bool) string {
	if slides {
		return "## 投影片 " + strconv.Itoa(n)
	}
	return "## 第 " + strconv.Itoa(n) + " 頁"
}

// isHeading reports whether line is a page's or slide's heading, as
// heading writes them.
func isHeading(line string) bool {
	return strings.HasPrefix(line, "## 第 ") && strings.HasSuffix(line, " 頁") || strings.HasPrefix(line, "## 投影片 ")
}
