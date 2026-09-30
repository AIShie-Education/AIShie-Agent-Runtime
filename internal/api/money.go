package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
)

// The money the runtime's administrators manage (D4; docs/design.md
// §11.5): the site's price table beside the price file's
// (/admin/prices), the plan's quotas in dollars beside its answers
// (/admin/school-plan/quotas), the tenants' daily quotas
// (/admin/tenants), the hosted agents' daily budgets by default
// (/admin/agent-budgets), and what the ledger recorded (/admin/costs).
// runtime.yaml and the price file stay the operator's defaults, which the
// site's settings stand in place of; each is kept in the store and put in
// force by every worker as it reads the registry again, without a
// restart. A change that would leave a quota in dollars no price holds is
// refused (unpriced).

// Bounds of an amount of dollars an administrator sets: more than none,
// at most maxUSD, to the micro-dollar.
const (
	maxUSD    = 1_000_000
	usdPlaces = 6
)

// DailyQuota is a quota a UTC day as the API says it: in answers, and in
// dollars, a decimal with six places ("2.500000"); each null for none.
type DailyQuota struct {
	Answers *int    `json:"answers"`
	USD     *string `json:"usd"`
}

// dailyQuota is q as the API says it.
func dailyQuota(q config.Quota) DailyQuota {
	return DailyQuota{Answers: clonePtr(q.Answers), USD: usdText(q.USD)}
}

// usdText is an amount of dollars as the API says it, nil for none.
func usdText(usd *float64) *string {
	if usd == nil {
		return nil
	}
	s := costUSD(pricing.PUSD(*usd))
	return &s
}

// clonePtr is a pointer to a copy of *p, nil for nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// decimalOf is the text of a JSON member that is a number or a string, as
// written: a number is never read through a float64.
func decimalOf(raw json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(raw))
	if strings.HasPrefix(t, `"`) {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return "", false
	}
	return n.String(), true
}

// readUSD reads a member that is an amount of dollars, a decimal string
// or a number ("2.50", 2.5): more than 0 and at most maxUSD, to six
// places. Null is none. It gives the amount as the configuration holds
// it, and in pUSD, exactly.
func readUSD(raw json.RawMessage, field string) (*float64, *int64, *Error) {
	if isNull(raw) {
		return nil, nil, nil
	}
	bad := fieldError(CodeInvalidArgument, ReasonInvalidField, field,
		"an amount of dollars is a decimal of more than 0, at most 1000000, to six places (\"2.50\"), or null for none")
	s, ok := decimalOf(raw)
	if !ok {
		return nil, nil, bad
	}
	pusd, err := pricing.ParseUSD(s, usdPlaces)
	if err != nil || pusd <= 0 || pusd > maxUSD*pricing.PUSDPerUSD {
		return nil, nil, bad
	}
	// The decimal read as the nearest float64 writes back as itself.
	usd, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(s), "+"), 64)
	if err != nil {
		return nil, nil, bad
	}
	return &usd, &pusd, nil
}

// readAnswers reads a member that is a quota in answers, from minQuota to
// maxQuota; null is none.
func readAnswers(raw json.RawMessage, field string) (*int, *Error) {
	if isNull(raw) {
		return nil, nil
	}
	var n int
	if json.Unmarshal(raw, &n) != nil || n < minQuota || n > maxQuota {
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, field,
			"a quota in answers is a whole number from 1 to 1000000, or null for none")
	}
	return &n, nil
}

// quotaRequest is a daily quota as a request gives it: answers and usd,
// each given, null for none.
type quotaRequest struct {
	Answers json.RawMessage `json:"answers"`
	USD     json.RawMessage `json:"usd"`
}

// readQuota reads the member raw, at field, as a daily quota: an object of
// answers and usd, both given, each null for none. It gives the quota,
// and its dollars in pUSD.
func readQuota(raw json.RawMessage, field string) (config.SiteQuota, *int64, *Error) {
	var q config.SiteQuota
	if raw == nil {
		return q, nil, fieldError(CodeInvalidArgument, ReasonMissingField, field, "a daily quota is given: {\"answers\": …, \"usd\": …}")
	}
	var r quotaRequest
	if e := decodeMember(raw, &r, field); e != nil {
		return q, nil, e
	}
	switch {
	case r.Answers == nil:
		return q, nil, fieldError(CodeInvalidArgument, ReasonMissingField, field+"/answers", "answers is given: a number, or null for none")
	case r.USD == nil:
		return q, nil, fieldError(CodeInvalidArgument, ReasonMissingField, field+"/usd", "usd is given: an amount of dollars, or null for none")
	}
	answers, e := readAnswers(r.Answers, field+"/answers")
	if e != nil {
		return q, nil, e
	}
	usd, pusd, e := readUSD(r.USD, field+"/usd")
	if e != nil {
		return q, nil, e
	}
	return config.SiteQuota{Answers: answers, USD: usd}, pusd, nil
}

// pricesOf is the price table in force with the site's settings eff has:
// the price file's, with the site's rows before it.
func (s *Server) pricesOf(eff *config.Config) *pricing.Table {
	return eff.Runtime.Site.PriceTable(s.prices())
}

// unpriced refuses a change of the site's settings, from cur to next, that
// would leave a quota in dollars that no price holds where none was
// before: an offer of the plan the price table in force would not price
// today while the plan has a quota in dollars (offer_not_priced, their
// ids in offers), or an agent with a quota in dollars whose model, or
// fallback, it would not price (model_not_priced, what is wrong in
// problems), which the runtime would not run. It builds the configuration
// as the registry would, both ways (registry.BuildWith). field is the
// member of the request the refusal names. Neither a refusal nor an error
// lets the change be written.
func (s *Server) unpriced(ctx context.Context, cur, next config.Site, field string) (*Error, error) {
	o := registry.Options{CoreBaseURL: s.o.CoreBaseURL, Allowlist: s.o.Allowlist}
	before, err := registry.BuildWith(ctx, s.yaml(), cur, s.o.Store, o)
	if err != nil {
		return nil, err
	}
	after, err := registry.BuildWith(ctx, s.yaml(), next, s.o.Store, o)
	if err != nil {
		return nil, err
	}
	now := s.o.Now()
	file := s.prices()
	was, is := cur.PriceTable(file), next.PriceTable(file)
	var offers []string
	if sc := after.Runtime.School; sc.USD() {
		var already []string
		if before.Runtime.School.USD() {
			already = config.UnpricedOffers(before.Runtime.School, was, now)
		}
		for _, id := range config.UnpricedOffers(sc, is, now) {
			if !slices.Contains(already, id) {
				offers = append(offers, id)
			}
		}
	}
	if len(offers) > 0 {
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonOfferNotPriced,
			Message: "the plan would have a quota in dollars, and the price table has no price today for these offers' models: add a price for each " +
				"(POST /admin/prices), or leave the quota in answers alone",
			Details: map[string]any{"field": field, "offers": offers}}, nil
	}
	already := config.AgentsUSDWithoutPrices(before, was, now)
	var problems []string
	for _, p := range config.AgentsUSDWithoutPrices(after, is, now) {
		if !slices.Contains(already, p) {
			problems = append(problems, clipRunes(p, maxProblem))
		}
		if len(problems) == maxProblems {
			break
		}
	}
	if len(problems) > 0 {
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonModelNotPriced,
			Message: "agents would have a quota in dollars, and the price table has no price today for their models, so the runtime would not run " +
				"them: add a price for each (POST /admin/prices)",
			Details: map[string]any{"field": field, "problems": problems}}, nil
	}
	return nil, nil
}

// checkUnpriced is unpriced, answering its refusal, or that the store
// cannot be read; it reports whether the change may be written.
func (s *Server) checkUnpriced(ctx context.Context, w http.ResponseWriter, cur, next config.Site, field string) bool {
	e, err := s.unpriced(ctx, cur, next, field)
	switch {
	case err != nil:
		s.storeUnavailable(w, "the configuration tried with the change", err)
		return false
	case e != nil:
		WriteError(w, *e)
		return false
	}
	return true
}

// utcDay is t's UTC day as YYYY-MM-DD.
func utcDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }
