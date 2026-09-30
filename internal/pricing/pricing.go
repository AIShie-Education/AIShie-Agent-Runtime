// Package pricing is the versioned price table and what a model call costs
// by it (Core's docs/agent-runtime.md §3.5, §5.3). Prices are never
// constants in code: an administrator keeps them in a YAML file, and every
// cost the ledger records names the table's version and row.
//
// Money is counted in pico-dollars (pUSD, 10⁻¹² USD) in int64: a price of
// one dollar per million tokens is 1,000,000 pUSD a token, so a price with
// up to six decimal places of a dollar per million tokens is a whole
// number, and costs add up exactly, without floating-point rounding. An
// int64 holds about 9.2 million dollars.
//
// The table:
//
//	version: "2026-09-27"
//	prices:
//	  - provider: anthropic           # as llm.DetectProvider names it
//	    model: claude-sonnet-4*       # exact, or a glob where * is any text
//	    from: 2025-05-22              # the date the price starts, UTC
//	    id: sonnet-4                  # optional; the row's name in versions
//	    usd_per_mtok: {input: 3, cache_read: "0.30", cache_write: 3.75, output: 15}
package pricing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/bits"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// PUSDPerUSD is how many pico-dollars make a dollar.
const PUSDPerUSD = 1_000_000_000_000

// Price is one row's prices, in pUSD a token.
type Price struct {
	// Version is the table's version and the row's id (or, without one, its
	// index in prices, from 0): "2026-09-27/sonnet-4". The ledger keeps it
	// beside every cost.
	Version    string
	In         int64
	CacheRead  int64
	CacheWrite int64
	Out        int64
}

// Cost is what u costs at p, in pUSD (§5.3): uncached input at the input
// price, cache reads and writes at theirs, and output at the output price.
// u.Input counts every input token, cached ones included, so the uncached
// ones are what is left after the cache's; u.Output already includes the
// reasoning tokens (§3.5), so u.Reasoning is not charged again. Negative
// counts are taken as zero, and a cost too large for an int64 is capped at
// math.MaxInt64.
func (p Price) Cost(u llm.Usage) int64 {
	cacheRead, cacheWrite := nonNeg(u.CacheRead), nonNeg(u.CacheWrite)
	uncached := nonNeg(u.Input) - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	var total int64
	for _, t := range [...]struct{ n, price int64 }{
		{uncached, p.In}, {cacheRead, p.CacheRead}, {cacheWrite, p.CacheWrite}, {nonNeg(u.Output), p.Out},
	} {
		total = addSat(total, mulSat(t.n, t.price))
	}
	return total
}

func nonNeg(n int64) int64 { return max(n, 0) }

func mulSat(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo)
}

func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// PUSD is usd in pico-dollars, rounded to the nearest; for the dollar
// amounts of configuration, such as a quota's usd. NaN is 0, and amounts
// beyond an int64 are capped.
func PUSD(usd float64) int64 {
	switch {
	case math.IsNaN(usd):
		return 0
	case usd >= math.MaxInt64/PUSDPerUSD:
		return math.MaxInt64
	case usd <= math.MinInt64/PUSDPerUSD:
		return math.MinInt64
	}
	return int64(math.Round(usd * PUSDPerUSD))
}

// USD is pusd in dollars, for display and metrics.
func USD(pusd int64) float64 { return float64(pusd) / PUSDPerUSD }

// Table is a loaded price table.
type Table struct {
	// Version is the table's own version, as its file gives it.
	Version string
	rows    []row
}

type row struct {
	provider string
	model    string
	glob     bool
	from     time.Time
	name     string // the row's id, or its index
	// site is set for a row of the site's (WithSite).
	site  bool
	price Price
}

// Load reads the table at path.
func Load(path string) (*Table, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's configuration names the file.
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	t, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// file is the table as written. Unknown keys are refused.
type file struct {
	Version string    `yaml:"version"`
	Prices  []fileRow `yaml:"prices"`
}

type fileRow struct {
	Provider   string     `yaml:"provider"`
	Model      string     `yaml:"model"`
	From       date       `yaml:"from"`
	ID         string     `yaml:"id"`
	USDPerMTok filePrices `yaml:"usd_per_mtok"`
}

type filePrices struct {
	Input      *perMTok `yaml:"input"`
	CacheRead  *perMTok `yaml:"cache_read"`
	CacheWrite *perMTok `yaml:"cache_write"`
	Output     *perMTok `yaml:"output"`
}

var (
	providerRe = regexp.MustCompile(`^[a-z0-9_]+$`)
	idRe       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Parse reads a table from YAML, checking every row, and reports every
// problem at once.
func Parse(b []byte) (*Table, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f file
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("pricing: the table is empty")
		}
		return nil, fmt.Errorf("pricing: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("pricing: the table is one YAML document")
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("pricing: "+format, args...)) }
	t := &Table{Version: strings.TrimSpace(f.Version)}
	if t.Version == "" {
		bad("version is required")
	}
	if len(f.Prices) == 0 {
		bad("prices is empty")
	}
	seen := map[string]int{}
	ids := map[string]int{}
	for i, fr := range f.Prices {
		where := fmt.Sprintf("prices[%d]", i)
		r := row{provider: fr.Provider, model: fr.Model, glob: strings.Contains(fr.Model, "*"), from: fr.From.t, name: strconv.Itoa(i)}
		if !providerRe.MatchString(fr.Provider) {
			bad("%s.provider: required, in lower case letters, digits and '_'", where)
		}
		if strings.TrimSpace(fr.Model) == "" {
			bad("%s.model: required", where)
		}
		if fr.From.t.IsZero() {
			bad("%s.from: required, as YYYY-MM-DD", where)
		}
		if fr.ID != "" {
			if !idRe.MatchString(fr.ID) {
				bad("%s.id: letters, digits, '.', '_' and '-', at most 64", where)
			} else if j, dup := ids[fr.ID]; dup {
				bad("%s.id: %q is also prices[%d]'s", where, fr.ID, j)
			}
			ids[fr.ID] = i
			r.name = fr.ID
		}
		key := fr.Provider + "\x00" + fr.Model + "\x00" + fr.From.t.Format(time.DateOnly)
		if j, dup := seen[key]; dup {
			bad("%s: the same provider, model and from as prices[%d]", where, j)
		}
		seen[key] = i
		p := fr.USDPerMTok
		if p.Input == nil {
			bad("%s.usd_per_mtok.input: required", where)
		}
		if p.Output == nil {
			bad("%s.usd_per_mtok.output: required", where)
		}
		r.price = Price{Version: t.Version + "/" + r.name, In: p.Input.get(), Out: p.Output.get()}
		// A cache price left out is the input price.
		r.price.CacheRead, r.price.CacheWrite = r.price.In, r.price.In
		if p.CacheRead != nil {
			r.price.CacheRead = p.CacheRead.get()
		}
		if p.CacheWrite != nil {
			r.price.CacheWrite = p.CacheWrite.get()
		}
		t.rows = append(t.rows, r)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return t, nil
}

// Lookup is the price of provider's model at at: among the rows for that
// provider whose model matches and whose from is on or before at, the one
// with the latest from. A row naming the model exactly beats every glob,
// whatever their dates: a model priced on its own stays so until its own
// row changes. Among globs, the latest from wins, then the most specific
// pattern (the most characters besides *), then the first row.
func (t *Table) Lookup(provider, model string, at time.Time) (Price, bool) {
	r := t.lookup(provider, model, at)
	if r == nil {
		return Price{}, false
	}
	return r.price, true
}

// PricedAs is what the row Lookup takes for provider's model at at names:
// the model itself, when a row names it exactly, or the glob that prices
// it; false when none does. It is always the table's own text, whatever
// model is asked about, for a metric to be labelled with.
func (t *Table) PricedAs(provider, model string, at time.Time) (string, bool) {
	r := t.lookup(provider, model, at)
	if r == nil {
		return "", false
	}
	return r.model, true
}

// lookup is the row Lookup takes, nil for none.
func (t *Table) lookup(provider, model string, at time.Time) *row {
	if t == nil {
		return nil
	}
	var best *row
	better := func(r *row) bool {
		switch {
		case best == nil:
			return true
		case r.glob != best.glob:
			return !r.glob
		case !r.from.Equal(best.from):
			return r.from.After(best.from)
		case r.glob:
			return literalLen(r.model) > literalLen(best.model)
		}
		return false
	}
	for i := range t.rows {
		r := &t.rows[i]
		if r.provider != provider || r.from.After(at) {
			continue
		}
		if r.glob && !Match(r.model, model) || !r.glob && r.model != model {
			continue
		}
		if better(r) {
			best = r
		}
	}
	return best
}

func literalLen(pattern string) int { return len(pattern) - strings.Count(pattern, "*") }

// Models are the models the table prices by name for provider at at: its
// rows that name a model exactly, not a glob, dated at or before at, each
// model once, in the table's order. A form suggests them.
func (t *Table) Models(provider string, at time.Time) []string {
	if t == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range t.rows {
		if r.provider != provider || r.glob || r.from.After(at) || seen[r.model] {
			continue
		}
		seen[r.model] = true
		out = append(out, r.model)
	}
	return out
}

// Match reports whether s matches pattern, where * stands for any run of
// characters, '/' and ':' included, and everything else stands for itself.
// It is the one glob the runtime uses for model names, here and in the
// school's model lists.
func Match(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return len(s) >= len(last) && strings.HasSuffix(s, last)
}

// date is a YYYY-MM-DD, quoted or not, as midnight UTC.
type date struct{ t time.Time }

func (d *date) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return typeError(n, "a date is YYYY-MM-DD")
	}
	t, err := time.Parse(time.DateOnly, n.Value)
	if err != nil {
		return typeError(n, fmt.Sprintf("%q is not a date such as 2026-01-31", n.Value))
	}
	d.t = t
	return nil
}

// typeError is a *yaml.TypeError, which the decoder collects and goes on
// from, so that every bad value in the table is reported at once.
func typeError(n *yaml.Node, msg string) error {
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %s", n.Line, msg)}}
}

// perMTok is a price in dollars per million tokens, read exactly and held
// as pUSD a token.
type perMTok struct{ pusd int64 }

func (p *perMTok) get() int64 {
	if p == nil {
		return 0
	}
	return p.pusd
}

// UnmarshalYAML reads the price from its text, a number or a decimal
// string, never through a float64: "0.028" is exactly 28,000 pUSD a token.
func (p *perMTok) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || (n.ShortTag() != "!!int" && n.ShortTag() != "!!float" && n.ShortTag() != "!!str") {
		return typeError(n, "a price is a number of dollars per million tokens, such as 0.40")
	}
	v, err := ParseUSDPerMTok(n.Value)
	if err != nil {
		return typeError(n, err.Error())
	}
	p.pusd = v
	return nil
}

var (
	oneMillion = big.NewRat(1_000_000, 1)
	maxPUSD    = new(big.Rat).SetInt64(math.MaxInt64)
	decimalRe  = regexp.MustCompile(`^\+?[0-9][0-9_]*(\.[0-9_]*)?([eE][+-]?[0-9]+)?$|^\+?\.[0-9_]+([eE][+-]?[0-9]+)?$`)
)

// ParseUSDPerMTok turns a price in dollars per million tokens, written as a
// decimal ("3", "0.40", "1.25e-1"), into pUSD a token, exactly. It refuses
// a negative price, and one more precise than a pico-dollar a token (more
// than six decimal places of a dollar per million tokens).
func ParseUSDPerMTok(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !decimalRe.MatchString(s) {
		return 0, fmt.Errorf("%q is not a price in dollars per million tokens, such as 0.40", s)
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(strings.TrimPrefix(s, "+"), "_", ""))
	if !ok {
		return 0, fmt.Errorf("%q is not a price in dollars per million tokens, such as 0.40", s)
	}
	r.Mul(r, oneMillion)
	if !r.IsInt() {
		return 0, fmt.Errorf("%q is more precise than a pico-dollar a token (six decimal places)", s)
	}
	if r.Cmp(maxPUSD) > 0 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return r.Num().Int64(), nil
}
