package transcribe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/providers"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// model is the offer's model, as the transcriber calls it.
type model struct {
	ad    llm.Adapter
	offer config.SchoolOffer
	input Input
	// label is what Core is told made the text: the offer's label.
	label string
	// maxOut is each call's output bound; pdfPages and pdfBytes what the
	// provider takes in one file (0 for no bound known).
	maxOut, pdfPages int
	pdfBytes         int64
	// site is an offer the site made, whose model its administrators
	// named: metrics name it as the price table does.
	site bool
}

// Output bounds of a call, where the offer sets none: ten dense pages
// are some six thousand tokens of Markdown.
const defaultMaxOutput = 8192

// modelOf is the offer's model, made once for the offer as it stands:
// its key resolved (a file of runtime.yaml's, or the site's, sealed), over
// the runtime's client for an offer of runtime.yaml's, at an endpoint of
// the operator's, and over the hosted-model client for one the site made.
func (s *Service) modelOf(ctx context.Context, st Setting) (*model, error) {
	o := *st.Offer
	s.mu.Lock()
	if s.model != nil && reflect.DeepEqual(s.modelFor, o) {
		defer s.mu.Unlock()
		return s.model, nil
	}
	s.mu.Unlock()
	m := o.AsModel()
	var key string
	if m.KeyRef != "" {
		var err error
		if key, err = s.o.Secrets.Resolve(ctx, m.KeyRef, st.Dir); err != nil {
			return nil, err
		}
	}
	client := s.o.ModelHTTP
	if o.Site || strings.HasPrefix(o.KeyRef, secrets.SchemeSealed) {
		client = s.o.HostedHTTP
	}
	ad, err := s.o.NewAdapter(providers.Config(m, key, client))
	if err != nil {
		return nil, err
	}
	md := &model{ad: ad, offer: o, input: InputOf(m), label: o.Label, maxOut: m.Params.MaxOutputTokens, site: o.Site}
	if !ad.Capabilities().FileInput {
		md.input = InputNone
	}
	if md.maxOut <= 0 {
		md.maxOut = defaultMaxOutput
	}
	if fl, ok := ad.(llm.FileLimiter); ok {
		lim := fl.FileLimits()
		md.pdfPages, md.pdfBytes = lim.PDFPages, lim.PDFBytes
	}
	if md.label == "" {
		md.label = o.Model
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model, s.modelFor = md, o
	return md, nil
}

// modelTries is how many times one range's call is made before the
// version fails (model_error); callTimeout bounds each.
const (
	modelTries  = 3
	callTimeout = 5 * time.Minute
)

// errTruncated is a model's text of a range cut off at its output bound.
var errTruncated = errors.New("transcribe: the model's text was cut off at its output bound")

// errRefused is a model that would not transcribe the pages (its filter,
// or a refusal).
var errRefused = errors.New("the model refused to transcribe the pages")

// spent is what a job's model calls cost: how many, their tokens in and
// out, their cost in pUSD, and whether one had no price.
type spent struct {
	calls         int
	input, output int64
	cost          int64
	unpriced      bool
}

// call asks the model for req, up to modelTries times while it fails in a
// way that may pass (rate limited, overloaded, the network), recording
// each answered call in the ledger and the metrics; it returns the text,
// errTruncated (with the text) when the output bound cut it, or why it
// failed.
func (s *Service) call(ctx context.Context, m *model, req *llm.Request, prices *pricing.Table, cost *spent) (string, error) {
	var last error
	for try := range modelTries {
		if try > 0 {
			d := s.t.ModelBackoff << (try - 1)
			var le *llm.Error
			if errors.As(last, &le) && le.RetryAfter > d {
				d = le.RetryAfter
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(d):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		start := s.now()
		resp, err := m.ad.Call(cctx, req)
		cancel()
		s.account(m, resp, err, s.now().Sub(start), prices, cost)
		if err == nil {
			switch resp.Stop {
			case llm.StopContentFilter, llm.StopRefusal:
				return "", errRefused
			case llm.StopMaxTokens:
				return resp.Text(), errTruncated
			}
			if strings.TrimSpace(resp.Text()) != "" {
				return resp.Text(), nil
			}
			err = errors.New("transcribe: the model wrote nothing")
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		last = err
		var le *llm.Error
		if errors.As(err, &le) && !le.Retryable() {
			return "", err
		}
	}
	return "", last
}

// account records one model call: its cost at the day's prices, a ledger
// row of the transcriber's (no agent's, tenant's, course's or asker's, on
// the school's key), and the metrics.
func (s *Service) account(m *model, resp *llm.Response, err error, took time.Duration, prices *pricing.Table, cost *spent) {
	now := s.now()
	var c int64
	var version string
	priced := false
	if resp != nil {
		if price, ok := prices.Lookup(m.ad.Provider(), m.ad.Model(), now); ok {
			c, version, priced = price.Cost(resp.Usage), price.Version, true
		}
	}
	if s.o.Metrics != nil {
		s.o.Metrics.ObserveLLM(m.ad.Name(), metricModel(m, prices, now), resp, err, float64(c)/1e12, config.KeySchool)
	}
	if resp == nil {
		return
	}
	cost.calls++
	cost.input += resp.Usage.Input
	cost.output += resp.Usage.Output
	cost.cost += c
	cost.unpriced = cost.unpriced || !priced
	u := resp.Usage
	rec := store.LLMCall{ID: uuid.NewString(), At: now, Kind: store.CallTranscription, Adapter: m.ad.Name(), Provider: m.ad.Provider(),
		Model: m.ad.Model(), Stop: string(resp.Stop), RawStop: resp.RawStop, Input: u.Input, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning, Estimated: u.Estimated, RawUsage: u.Raw,
		PriceVersion: version, CostPUSD: c, KeySource: config.KeySchool, LatencyMS: took.Milliseconds()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.o.Store.RecordLLMCall(ctx, rec); err != nil {
		s.log.Error("a transcription's model call was not recorded in the ledger", "err", errText(err))
	}
}

// metricModel is the model a call's metrics name: runtime.yaml's offer's
// as its operator wrote it; a site's, as the price table names it, and
// other where no row prices it, so that no administrator's text is a
// label.
func metricModel(m *model, prices *pricing.Table, at time.Time) string {
	if !m.site {
		return m.ad.Model()
	}
	if name, ok := prices.PricedAs(m.ad.Provider(), m.ad.Model(), at); ok {
		return name
	}
	return "other"
}

// request is the call for pages first to last of a document of total,
// slides or pages, given as files, with the speaker notes of its slides
// when it has any.
func request(m *model, first, last, total int, slides bool, files []*llm.File, notes map[int]string) *llm.Request {
	unit := "pages"
	if slides {
		unit = "slides"
	}
	var b strings.Builder
	if first == last {
		fmt.Fprintf(&b, "This is %s %d of %d of the document. Transcribe it, under the heading %q.", strings.TrimSuffix(unit, "s"), first, total,
			heading(first, slides))
	} else {
		fmt.Fprintf(&b, "These are %s %d to %d of %d of the document, in order. Transcribe each, from %q to %q.", unit, first, last, total,
			heading(first, slides), heading(last, slides))
	}
	if len(files) > 1 {
		b.WriteString(" Each picture is one of them, in order.")
	}
	var said bool
	for n := first; n <= last; n++ {
		if note := strings.TrimSpace(notes[n]); note != "" {
			if !said {
				b.WriteString("\n\nThe speaker notes of these slides, which the pages do not show:")
				said = true
			}
			fmt.Fprintf(&b, "\n\nSlide %d:\n%s", n, note)
		}
	}
	parts := []llm.Part{llm.Text(b.String())}
	for _, f := range files {
		parts = append(parts, llm.Part{Type: llm.PartFile, File: f})
	}
	return &llm.Request{System: Prompt, Messages: []llm.Message{{Role: llm.RoleUser, Parts: parts}}, ToolMode: llm.ToolAuto,
		Limits: llm.Limits{MaxOutputTokens: m.maxOut}}
}
