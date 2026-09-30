package pricing

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The site's rows (docs/design.md §11.5): a price table's rows that the
// runtime's administrators keep in its store beside the file's, as a table
// of their own with a version of its own, "site-<when it last changed>",
// so that a cost the ledger records names the row it was priced by, and a
// change to the site's rows is a new version, as a new file's is. Where a
// site's row and a file's are otherwise alike (the same provider, the
// same exact model or glob, the same from), the site's is taken.

// SiteVersionPrefix begins the version of the site's rows.
const SiteVersionPrefix = "site-"

// SiteVersion is the version of the site's rows that last changed at t:
// site-<t in UTC, to the second>, such as site-20260930T101500Z.
func SiteVersion(t time.Time) string {
	return SiteVersionPrefix + t.UTC().Format("20060102T150405Z")
}

// Row is one row of a table as the API and the store keep it: the site's
// rows are all named, by ID, and their prices are pUSD a token, exact.
type Row struct {
	ID       string
	Provider string
	Model    string
	From     time.Time
	// In, CacheRead, CacheWrite and Out are pUSD a token.
	In, CacheRead, CacheWrite, Out int64
}

// ValidProvider reports whether s may be a row's provider: lower-case
// letters, digits and '_', as llm.DetectProvider names them.
func ValidProvider(s string) bool { return providerRe.MatchString(s) }

// ValidRowID reports whether s may be a row's id: letters, digits, '.',
// '_' and '-', at most 64.
func ValidRowID(s string) bool { return idRe.MatchString(s) }

// WithSite is t, the file's table (nil for none), with the site's rows
// under version, taken before the file's where both would price a model
// alike. With no site's rows, it is t itself; the table's Version is the
// file's and the site's, joined by '+'.
func (t *Table) WithSite(version string, rows []Row) *Table {
	if len(rows) == 0 {
		return t
	}
	out := &Table{Version: version}
	for _, r := range rows {
		out.rows = append(out.rows, row{provider: r.Provider, model: r.Model, glob: strings.Contains(r.Model, "*"), from: r.From, name: r.ID,
			site: true, price: Price{Version: version + "/" + r.ID, In: r.In, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, Out: r.Out}})
	}
	if t != nil {
		out.Version = t.Version + "+" + version
		out.rows = append(out.rows, t.rows...)
	}
	return out
}

// Listed is one row of a table as it is listed: where it comes from (the
// file's, or the site's), its name in versions, and its price.
type Listed struct {
	Site     bool
	Name     string
	Provider string
	Model    string
	From     time.Time
	Price    Price
}

// Rows are the table's rows, the site's first, each as the table has it.
func (t *Table) Rows() []Listed {
	if t == nil {
		return nil
	}
	out := make([]Listed, len(t.rows))
	for i, r := range t.rows {
		out[i] = Listed{Site: r.site, Name: r.name, Provider: r.provider, Model: r.model, From: r.from, Price: r.price}
	}
	return out
}

// FormatUSDPerMTok is pusd a token as ParseUSDPerMTok reads it: dollars
// per million tokens, with no more places than it needs, such as "0.4" or
// "15".
func FormatUSDPerMTok(pusd int64) string {
	s := new(big.Rat).SetFrac64(pusd, 1_000_000).FloatString(6)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// ParseUSD turns an amount of dollars written as a decimal ("2.5",
// "0.000125") into pUSD, exactly: more than places decimal places, a
// negative amount, and one past an int64 are refused.
func ParseUSD(s string, places int) (int64, error) {
	s = strings.TrimSpace(s)
	if !decimalRe.MatchString(s) || strings.ContainsAny(s, "eE_") {
		return 0, fmt.Errorf("%q is not an amount of dollars, such as 2.50", s)
	}
	r, ok := new(big.Rat).SetString(strings.TrimPrefix(s, "+"))
	if !ok {
		return 0, fmt.Errorf("%q is not an amount of dollars, such as 2.50", s)
	}
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt64(pow10(places)))
	if !scaled.IsInt() {
		return 0, fmt.Errorf("%q has more than %d decimal places", s, places)
	}
	r.Mul(r, new(big.Rat).SetInt64(PUSDPerUSD))
	if r.Cmp(maxPUSD) > 0 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return r.Num().Int64(), nil
}

func pow10(n int) int64 {
	p := int64(1)
	for range min(max(n, 0), 12) {
		p *= 10
	}
	return p
}

// FormatUSD is pusd in dollars with places decimal places, rounded to the
// nearest: "2.500000".
func FormatUSD(pusd int64, places int) string {
	return new(big.Rat).SetFrac64(pusd, PUSDPerUSD).FloatString(places)
}
