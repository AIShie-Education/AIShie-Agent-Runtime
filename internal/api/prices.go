package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// GET /admin/prices, and POST, GET, PATCH and DELETE of the site's rows
// under /admin/prices/{id}: the price table in force, the price file's
// (PRICES, or runtime.prices_ref; source file, which only the operator
// changes) with the site's rows before it (source site). The site's rows
// are a table of their own, versioned by when they last changed,
// site-<UTC second> (pricing.SiteVersion): a cost the ledger records names
// the table's version and the row it was priced by, as
// site-20260930T101500Z/<id>, and every change to the site's rows is a
// new version, so that the costs recorded before it keep the version they
// were priced by. A site's row of the same provider, model and from as a
// file's stands before it. A change is refused where it would leave a
// quota in dollars that no price holds (unpriced).

// Where a row of the price table comes from.
const (
	PriceSourceFile = "file"
	PriceSourceSite = "site"
)

// PriceTable is GET /admin/prices' answer: the table in force's version
// (the file's and the site's joined by '+', null with neither), the
// file's (null for none) and the site's (null while it has no rows), when
// the site's rows last changed (null for never); every row, the site's
// first by id, then the file's as it lists them; and the offers of the
// plan, on or off, whose model no row prices today.
type PriceTable struct {
	Version        *string         `json:"version"`
	FileVersion    *string         `json:"file_version"`
	SiteVersion    *string         `json:"site_version"`
	SiteChangedAt  *time.Time      `json:"site_changed_at"`
	Rows           []PriceRow      `json:"rows"`
	UnpricedOffers []UnpricedOffer `json:"unpriced_offers"`
}

// PriceRow is one row: its id (a file's row without one is named by its
// index, as the ledger names it), where it comes from, the provider and
// the model it prices (exactly, or by a glob where * is any text), the
// day it is priced from, its prices in dollars per million tokens as
// exact decimals, and the version a cost it prices is recorded under. Of
// a file's row, overridden is whether a site's row of its provider, model
// and from stands before it; of a site's, its version (the ETag), and
// when and by whom it was made and last changed.
type PriceRow struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	Provider   string     `json:"provider"`
	Model      string     `json:"model"`
	Glob       bool       `json:"glob"`
	From       string     `json:"from"`
	USDPerMTok PriceRates `json:"usd_per_mtok"`
	Version    string     `json:"version"`
	Overridden bool       `json:"overridden"`
	RowVersion *int       `json:"row_version"`
	CreatedAt  *time.Time `json:"created_at"`
	CreatedBy  *string    `json:"created_by"`
	UpdatedAt  *time.Time `json:"updated_at"`
	UpdatedBy  *string    `json:"updated_by"`
}

// PriceRates are a row's prices in dollars per million tokens: uncached
// input, cache reads and writes, and output.
type PriceRates struct {
	Input      string `json:"input"`
	CacheRead  string `json:"cache_read"`
	CacheWrite string `json:"cache_write"`
	Output     string `json:"output"`
}

// UnpricedOffer is an offer of the plan whose model no row prices today:
// with a quota of the plan's in dollars, it could not be held to it.
type UnpricedOffer struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Enabled  bool   `json:"enabled"`
}

// maxPriceModel bounds a site's row's model, in characters.
const maxPriceModel = 200

// ratesOf are p's prices as the API says them.
func ratesOf(p pricing.Price) PriceRates {
	return PriceRates{Input: pricing.FormatUSDPerMTok(p.In), CacheRead: pricing.FormatUSDPerMTok(p.CacheRead),
		CacheWrite: pricing.FormatUSDPerMTok(p.CacheWrite), Output: pricing.FormatUSDPerMTok(p.Out)}
}

// priceKey is what no two rows of one table share: the provider, the
// model and the day.
func priceKey(provider, model string, from time.Time) string {
	return provider + "\x00" + model + "\x00" + utcDay(from)
}

// priceTable is the table in force as GET /admin/prices answers it: the
// file's, with the site's rows as the store has them now.
func (s *Server) priceTable(ctx context.Context) (*PriceTable, error) {
	stored, changed, err := s.o.Store.SitePrices(ctx)
	if err != nil {
		return nil, err
	}
	offers, err := s.o.Store.SchoolOffers(ctx)
	if err != nil {
		return nil, err
	}
	file := s.prices()
	rows := make([]pricing.Row, len(stored))
	byID := map[string]store.SitePrice{}
	site := map[string]bool{}
	for i, p := range stored {
		rows[i] = registry.PriceRow(p)
		byID[p.ID] = p
		site[priceKey(p.Provider, p.Model, p.From)] = true
	}
	version := pricing.SiteVersion(changed)
	table := file.WithSite(version, rows)
	out := &PriceTable{Rows: []PriceRow{}, UnpricedOffers: []UnpricedOffer{}}
	if table != nil {
		out.Version = strPtr(table.Version)
	}
	if file != nil {
		out.FileVersion = strPtr(file.Version)
	}
	if len(rows) > 0 {
		out.SiteVersion = &version
	}
	if !changed.IsZero() {
		at := changed.UTC()
		out.SiteChangedAt = &at
	}
	for _, l := range table.Rows() {
		v := PriceRow{ID: l.Name, Source: PriceSourceFile, Provider: l.Provider, Model: l.Model, Glob: strings.Contains(l.Model, "*"),
			From: utcDay(l.From), USDPerMTok: ratesOf(l.Price), Version: l.Price.Version}
		if l.Site {
			p := byID[l.Name]
			rv, created, updated := p.Version, p.CreatedAt.UTC(), p.UpdatedAt.UTC()
			v.Source, v.RowVersion, v.CreatedAt, v.CreatedBy, v.UpdatedAt, v.UpdatedBy = PriceSourceSite, &rv, &created, strPtr(p.CreatedBy),
				&updated, strPtr(p.UpdatedBy)
		} else {
			v.Overridden = site[priceKey(l.Provider, l.Model, l.From)]
		}
		out.Rows = append(out.Rows, v)
	}
	now := s.o.Now()
	for _, o := range s.yaml().Runtime.School.Offers {
		m := o.AsModel()
		if _, ok := table.Lookup(m.EffectiveProvider(), m.Model, now); !ok {
			out.UnpricedOffers = append(out.UnpricedOffers, UnpricedOffer{ID: o.ID, Source: SourceConfig, Provider: m.EffectiveProvider(),
				Model: m.Model, Enabled: true})
		}
	}
	for _, o := range offers {
		if _, ok := table.Lookup(o.Provider, o.Model, now); !ok {
			out.UnpricedOffers = append(out.UnpricedOffers, UnpricedOffer{ID: o.ID, Source: SourceSite, Provider: o.Provider, Model: o.Model,
				Enabled: o.Enabled})
		}
	}
	return out, nil
}

func (s *Server) getPrices(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	t, err := s.priceTable(ctx)
	if err != nil {
		s.storeUnavailable(w, "the price table", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

var errPriceNotFound = Error{Code: CodeNotFound, Reason: ReasonPriceNotFound, Message: "the price table has no row of this id"}

// writePrice answers the row id of the table in force, the site's with
// its ETag, or the file's when the site has none of that id.
func (s *Server) writePrice(ctx context.Context, w http.ResponseWriter, status int, id string) {
	t, err := s.priceTable(ctx)
	if err != nil {
		s.storeUnavailable(w, "the price table", err)
		return
	}
	var file *PriceRow
	for i, row := range t.Rows {
		switch {
		case row.ID != id:
		case row.Source == PriceSourceSite:
			w.Header().Set("ETag", etag(*row.RowVersion))
			writeJSON(w, status, row)
			return
		case file == nil:
			file = &t.Rows[i]
		}
	}
	if file == nil {
		WriteError(w, errPriceNotFound)
		return
	}
	writeJSON(w, status, file)
}

// getPrice is GET /admin/prices/{id}: a row as GET /admin/prices lists
// it, the site's with its ETag. The site's is the one PATCH and DELETE
// change, and is given where the file has a row of its id too.
func (s *Server) getPrice(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	s.writePrice(ctx, w, http.StatusOK, r.PathValue("id"))
}

// priceRequest is POST /admin/prices' body: the row's id, which names it
// in the ledger's versions and is never changed; the provider, as the
// ledger names it; the model, exactly or by a glob; the day, YYYY-MM-DD
// in UTC, the price starts; and its prices, in dollars per million
// tokens, each a decimal string or a number: input and output, and the
// cache's, which are the input's when left out.
type priceRequest struct {
	ID         string          `json:"id"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	From       string          `json:"from"`
	USDPerMTok json.RawMessage `json:"usd_per_mtok"`
}

// ratesRequest is usd_per_mtok as a request gives it.
type ratesRequest struct {
	Input      json.RawMessage `json:"input"`
	CacheRead  json.RawMessage `json:"cache_read"`
	CacheWrite json.RawMessage `json:"cache_write"`
	Output     json.RawMessage `json:"output"`
}

// readRate reads one price, at field, into *into: a decimal of dollars
// per million tokens, zero or more, to six places (pricing.ParseUSDPerMTok).
func readRate(raw json.RawMessage, into *int64, field string) *Error {
	bad := fieldError(CodeInvalidArgument, ReasonInvalidField, field,
		"a price is dollars per million tokens, zero or more, to six places, as a decimal string or a number, such as \"0.40\"")
	if isNull(raw) {
		return bad
	}
	s, ok := decimalOf(raw)
	if !ok {
		return bad
	}
	v, err := pricing.ParseUSDPerMTok(s)
	if err != nil {
		return bad
	}
	*into = v
	return nil
}

// readRates reads usd_per_mtok over p's prices: of a new row (create),
// input and output are required and a cache price left out is the
// input's; of a row changed, a price left out is kept.
func readRates(raw json.RawMessage, p *store.SitePrice, create bool) *Error {
	if raw == nil {
		if create {
			return fieldError(CodeInvalidArgument, ReasonMissingField, "/usd_per_mtok", "the prices are required: {\"input\": …, \"output\": …}")
		}
		return nil
	}
	var r ratesRequest
	if e := decodeMember(raw, &r, "/usd_per_mtok"); e != nil {
		return e
	}
	if create {
		switch {
		case r.Input == nil:
			return fieldError(CodeInvalidArgument, ReasonMissingField, "/usd_per_mtok/input", "the input price is required")
		case r.Output == nil:
			return fieldError(CodeInvalidArgument, ReasonMissingField, "/usd_per_mtok/output", "the output price is required")
		}
	}
	for _, f := range []struct {
		raw   json.RawMessage
		into  *int64
		field string
	}{
		{r.Input, &p.InputPUSD, "/usd_per_mtok/input"}, {r.CacheRead, &p.CacheReadPUSD, "/usd_per_mtok/cache_read"},
		{r.CacheWrite, &p.CacheWritePUSD, "/usd_per_mtok/cache_write"}, {r.Output, &p.OutputPUSD, "/usd_per_mtok/output"},
	} {
		if f.raw == nil {
			continue
		}
		if e := readRate(f.raw, f.into, f.field); e != nil {
			return e
		}
	}
	if create && r.CacheRead == nil {
		p.CacheReadPUSD = p.InputPUSD
	}
	if create && r.CacheWrite == nil {
		p.CacheWritePUSD = p.InputPUSD
	}
	return nil
}

// priceShape refuses a row whose provider, model or from is not one: the
// provider as the ledger names it, the model one line of text without
// spaces (a glob's * any text), from a day.
func priceShape(provider, model, from string) (time.Time, *Error) {
	switch {
	case provider == "":
		return time.Time{}, fieldError(CodeInvalidArgument, ReasonMissingField, "/provider", "the provider is required, as the ledger names it")
	case !pricing.ValidProvider(provider):
		return time.Time{}, fieldError(CodeInvalidArgument, ReasonInvalidField, "/provider",
			"a provider is lower-case letters, digits and '_', as the ledger names it (openai, anthropic, deepseek, …)")
	case model == "":
		return time.Time{}, fieldError(CodeInvalidArgument, ReasonMissingField, "/model", "the model is required: exactly, or a glob")
	case utf8.RuneCountInString(model) > maxPriceModel || !utf8.ValidString(model) || strings.IndexFunc(model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0:
		return time.Time{}, fieldError(CodeInvalidArgument, ReasonInvalidField, "/model",
			"a model is its id as the provider names it, or a glob where * is any text, of at most 200 characters and no spaces")
	case from == "":
		return time.Time{}, fieldError(CodeInvalidArgument, ReasonMissingField, "/from", "from is required: the day the price starts, YYYY-MM-DD")
	}
	day, err := time.Parse(time.DateOnly, from)
	if err != nil || day.Year() < 2000 || day.Year() > 2100 {
		return time.Time{}, fieldError(CodeInvalidArgument, ReasonInvalidField, "/from", "from is the day the price starts, YYYY-MM-DD, in UTC")
	}
	return day, nil
}

// validPriceID reports whether id may name a site's row: pricing's ids,
// beginning with a letter or a digit, so that it is a path's segment.
func validPriceID(id string) bool {
	return pricing.ValidRowID(id) && (unicode.IsLetter(rune(id[0])) || unicode.IsDigit(rune(id[0])))
}

// priceTaken refuses p where another of the site's rows has its id or its
// provider, model and from (409 price_exists, with the other's id).
func priceTaken(rows []store.SitePrice, p store.SitePrice, create bool) *Error {
	for _, o := range rows {
		switch {
		case create && o.ID == p.ID:
			return &Error{Code: CodeConflict, Reason: ReasonPriceExists, Message: "the site has a row of this id",
				Details: map[string]any{"field": "/id", "id": o.ID}}
		case o.ID != p.ID && o.Provider == p.Provider && o.Model == p.Model && o.From.Equal(p.From):
			return &Error{Code: CodeConflict, Reason: ReasonPriceExists,
				Message: "the site has a row of this provider, model and from: change that one", Details: map[string]any{"field": "/from", "id": o.ID}}
		}
	}
	return nil
}

// priceDetail is the audit's detail of p.
func priceDetail(au *auditing, p store.SitePrice) {
	au.detail["provider"], au.detail["model"], au.detail["from"] = p.Provider, p.Model, utcDay(p.From)
	au.detail["usd_per_mtok"] = map[string]string{"input": pricing.FormatUSDPerMTok(p.InputPUSD), "cache_read": pricing.FormatUSDPerMTok(p.CacheReadPUSD),
		"cache_write": pricing.FormatUSDPerMTok(p.CacheWritePUSD), "output": pricing.FormatUSDPerMTok(p.OutputPUSD)}
}

// createPrice is POST /admin/prices: a site's row, from the next second
// in a new version of the site's table.
func (s *Server) createPrice(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	var req priceRequest
	if !readBody(w, r, &req) {
		return
	}
	var e *Error
	switch {
	case req.ID == "":
		e = fieldError(CodeInvalidArgument, ReasonMissingField, "/id", "an id is required: the row's name in the ledger's versions")
	case !validPriceID(req.ID):
		e = fieldError(CodeInvalidArgument, ReasonInvalidField, "/id",
			"an id is letters, digits, '.', '_' and '-', at most 64, beginning with a letter or a digit")
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.target("site_price", req.ID)
	from, e := priceShape(req.Provider, req.Model, req.From)
	p := store.SitePrice{ID: req.ID, Provider: req.Provider, Model: req.Model, From: from, CreatedBy: c.ActorID, UpdatedBy: c.ActorID}
	if e == nil {
		e = readRates(req.USDPerMTok, &p, true)
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	priceDetail(au, p)
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	rows, _, err := s.o.Store.SitePrices(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's prices", err)
		return
	}
	if e := priceTaken(rows, p, true); e != nil {
		WriteError(w, *e)
		return
	}
	p.CreatedAt = s.o.Now().UTC()
	created, err := s.o.Store.CreateSitePrice(ctx, p)
	switch {
	case errors.Is(err, store.ErrExists):
		WriteError(w, Error{Code: CodeConflict, Reason: ReasonPriceExists, Message: "the site has a row of this id, or of this provider, model and from",
			Details: map[string]any{"field": "/id"}})
		return
	case err != nil:
		s.storeUnavailable(w, "a price made", err)
		return
	}
	au.detail["version"] = created.Version
	s.writePrice(ctx, w, http.StatusCreated, created.ID)
}

// pricePatch is PATCH /admin/prices/{id}'s body, a merge-patch: a member
// left out is kept as it is, and of usd_per_mtok, a price left out. The
// id is not changed: it names the row in the ledger's versions.
type pricePatch struct {
	Provider   json.RawMessage `json:"provider"`
	Model      json.RawMessage `json:"model"`
	From       json.RawMessage `json:"from"`
	USDPerMTok json.RawMessage `json:"usd_per_mtok"`
}

// sitePrice is the site's row id, answering when there is none: a row of
// the file's is the operator's to change (403 price_read_only), and any
// other 404 price_not_found.
func (s *Server) sitePrice(ctx context.Context, w http.ResponseWriter, id string) (*store.SitePrice, []store.SitePrice) {
	rows, _, err := s.o.Store.SitePrices(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's prices", err)
		return nil, nil
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], rows
		}
	}
	if slices.ContainsFunc(s.prices().Rows(), func(l pricing.Listed) bool { return l.Name == id }) {
		WriteError(w, Error{Code: CodeForbidden, Reason: ReasonPriceReadOnly,
			Message: "this row is the price file's: only the runtime's operator changes it; a site's row of its provider, model and from stands before it"})
		return nil, nil
	}
	WriteError(w, errPriceNotFound)
	return nil, nil
}

// priceMismatch is a write of a row at a version it has left.
func priceMismatch(current int) Error {
	return Error{Code: CodeVersionMismatch, Reason: ReasonVersionMismatch,
		Message: "the row has changed since that version was read: read it again", Details: map[string]any{"current_version": current}}
}

// withPrices is site with its rows as rows would make them.
func withPrices(site config.Site, rows []store.SitePrice) config.Site {
	site.Prices = nil
	for _, p := range rows {
		site.Prices = append(site.Prices, registry.PriceRow(p))
	}
	return site
}

// updatePrice is PATCH /admin/prices/{id}: the site's row changed, at the
// version If-Match names when it names one, in a new version of the
// site's table. A row moved off a model is refused where a quota in
// dollars would be left without a price.
func (s *Server) updatePrice(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_price", r.PathValue("id"))
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	var req pricePatch
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	cur, rows := s.sitePrice(ctx, w, r.PathValue("id"))
	if cur == nil {
		return
	}
	if named && version != cur.Version {
		WriteError(w, priceMismatch(cur.Version))
		return
	}
	next := *cur
	provider, model, from := cur.Provider, cur.Model, utcDay(cur.From)
	for _, f := range []struct {
		raw   json.RawMessage
		into  *string
		field string
	}{{req.Provider, &provider, "/provider"}, {req.Model, &model, "/model"}, {req.From, &from, "/from"}} {
		if e := patchString(f.raw, f.into, f.field, false); e != nil {
			WriteError(w, *e)
			return
		}
	}
	day, e := priceShape(provider, model, from)
	if e == nil {
		e = readRates(req.USDPerMTok, &next, false)
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	next.Provider, next.Model, next.From, next.UpdatedBy, next.Version = provider, model, day, c.ActorID, cur.Version
	var changed []string
	moved := next.Provider != cur.Provider || next.Model != cur.Model || !next.From.Equal(cur.From)
	if moved {
		changed = append(changed, "model")
	}
	if next.InputPUSD != cur.InputPUSD || next.CacheReadPUSD != cur.CacheReadPUSD || next.CacheWritePUSD != cur.CacheWritePUSD ||
		next.OutputPUSD != cur.OutputPUSD {
		changed = append(changed, "usd_per_mtok")
	}
	if len(changed) == 0 {
		au.skip = true
		s.writePrice(ctx, w, http.StatusOK, cur.ID)
		return
	}
	priceDetail(au, next)
	au.detail["changed"] = changed
	if e := priceTaken(rows, next, false); e != nil {
		WriteError(w, *e)
		return
	}
	if moved {
		site, err := registry.ReadSite(ctx, s.o.Store)
		if err != nil {
			s.storeUnavailable(w, "the site's settings", err)
			return
		}
		after := slices.Clone(rows)
		for i := range after {
			if after[i].ID == next.ID {
				after[i] = next
			}
		}
		if !s.checkUnpriced(ctx, w, withPrices(site, rows), withPrices(site, after), "/model") {
			return
		}
	}
	updated, err := s.o.Store.UpdateSitePrice(ctx, next)
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errPriceNotFound)
		return
	case errors.Is(err, store.ErrConflict):
		current := cur.Version
		if again, _, err := s.o.Store.SitePrices(ctx); err == nil {
			for _, p := range again {
				if p.ID == cur.ID {
					current = p.Version
				}
			}
		}
		WriteError(w, priceMismatch(current))
		return
	case errors.Is(err, store.ErrExists):
		WriteError(w, Error{Code: CodeConflict, Reason: ReasonPriceExists, Message: "the site has a row of this provider, model and from",
			Details: map[string]any{"field": "/from"}})
		return
	case err != nil:
		s.storeUnavailable(w, "a price updated", err)
		return
	}
	au.detail["version"] = updated.Version
	s.writePrice(ctx, w, http.StatusOK, updated.ID)
}

// deletePrice is DELETE /admin/prices/{id}: the site's row destroyed, at
// the version If-Match names when it names one, in a new version of the
// site's table; the answer is the table. A row a quota in dollars needs
// is refused.
func (s *Server) deletePrice(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_price", r.PathValue("id"))
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	cur, rows := s.sitePrice(ctx, w, r.PathValue("id"))
	if cur == nil {
		return
	}
	priceDetail(au, *cur)
	if named && version != cur.Version {
		WriteError(w, priceMismatch(cur.Version))
		return
	}
	site, err := registry.ReadSite(ctx, s.o.Store)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	after := slices.DeleteFunc(slices.Clone(rows), func(p store.SitePrice) bool { return p.ID == cur.ID })
	if !s.checkUnpriced(ctx, w, withPrices(site, rows), withPrices(site, after), "") {
		return
	}
	switch err := s.o.Store.DeleteSitePrice(ctx, cur.ID, heldTo(version, named)); {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errPriceNotFound)
		return
	case errors.Is(err, store.ErrConflict):
		WriteError(w, priceMismatch(cur.Version))
		return
	case err != nil:
		s.storeUnavailable(w, "a price deleted", err)
		return
	}
	au.detail["version"] = cur.Version
	t, err := s.priceTable(ctx)
	if err != nil {
		s.storeUnavailable(w, "the price table", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
