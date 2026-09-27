package pricing

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestLookup(t *testing.T) {
	tab, err := Load("testdata/table.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if tab.Version != "t1" {
		t.Fatalf("version %q", tab.Version)
	}
	for _, tc := range []struct {
		name, provider, model string
		at                    time.Time
		want                  Price
		ok                    bool
	}{
		{
			name: "glob row", provider: "anthropic", model: "claude-sonnet-4-20250514", at: day("2025-06-01"), ok: true,
			want: Price{Version: "t1/sonnet-4", In: 3_000_000, CacheRead: 300_000, CacheWrite: 3_750_000, Out: 15_000_000},
		},
		{
			name: "a later glob row takes over from its date", provider: "anthropic", model: "claude-sonnet-4-20250514", at: day("2026-06-01"), ok: true,
			want: Price{Version: "t1/sonnet-4-cheaper", In: 2_000_000, CacheRead: 2_000_000, CacheWrite: 2_000_000, Out: 10_000_000},
		},
		{
			name: "the day before a row starts", provider: "anthropic", model: "claude-sonnet-4-20250514", at: day("2026-06-01").Add(-time.Nanosecond), ok: true,
			want: Price{Version: "t1/sonnet-4", In: 3_000_000, CacheRead: 300_000, CacheWrite: 3_750_000, Out: 15_000_000},
		},
		{
			name: "an exact row beats every glob", provider: "anthropic", model: "claude-sonnet-4-5", at: day("2026-07-01"), ok: true,
			want: Price{Version: "t1/2", In: 3_500_000, CacheRead: 3_500_000, CacheWrite: 3_500_000, Out: 16_000_000},
		},
		{
			name: "before the exact row, the globs", provider: "anthropic", model: "claude-sonnet-4-5", at: day("2025-09-28"), ok: true,
			want: Price{Version: "t1/sonnet-4", In: 3_000_000, CacheRead: 300_000, CacheWrite: 3_750_000, Out: 15_000_000},
		},
		{
			name: "same date, the more specific glob", provider: "anthropic", model: "claude-haiku-4-5", at: day("2026-01-01"), ok: true,
			want: Price{Version: "t1/4", In: 50_000_000, CacheRead: 50_000_000, CacheWrite: 50_000_000, Out: 50_000_000},
		},
		{
			name: "a catch-all", provider: "anthropic", model: "other", at: day("2026-01-01"), ok: true,
			want: Price{Version: "t1/3", In: 100_000_000, CacheRead: 100_000_000, CacheWrite: 100_000_000, Out: 100_000_000},
		},
		{
			name: "decimal prices exactly", provider: "deepseek", model: "deepseek-chat", at: day("2025-10-01"), ok: true,
			want: Price{Version: "t1/5", In: 280_000, CacheRead: 28_000, CacheWrite: 280_000, Out: 420_000},
		},
		{
			name: "glob across slashes and colons", provider: "openrouter", model: "anthropic/claude-sonnet-4:beta", at: day("2026-01-01"), ok: true,
			want: Price{Version: "t1/6", In: 1_000_000_000, CacheRead: 1_000_000_000, CacheWrite: 1_000_000_000, Out: 1},
		},
		{name: "before any row", provider: "deepseek", model: "deepseek-chat", at: day("2025-01-01")},
		{name: "another provider", provider: "openai", model: "claude-sonnet-4-5", at: day("2026-01-01")},
		{name: "exact model only", provider: "deepseek", model: "deepseek-chat-v2", at: day("2026-01-01")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tab.Lookup(tc.provider, tc.model, tc.at)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Lookup = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	var nilTable *Table
	if _, ok := nilTable.Lookup("a", "b", time.Now()); ok {
		t.Fatal("a nil table prices nothing")
	}
}

func TestParseUSDPerMTok(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		err  string
	}{
		{in: "0", want: 0},
		{in: "3", want: 3_000_000},
		{in: "0.40", want: 400_000},
		{in: "0.028", want: 28_000},
		{in: "3.75", want: 3_750_000},
		{in: "0.000001", want: 1},
		{in: "1.25e-1", want: 125_000},
		{in: "1e3", want: 1_000_000_000},
		{in: ".5", want: 500_000},
		{in: "+2", want: 2_000_000},
		{in: "1_000", want: 1_000_000_000},
		// 0.1 and 0.2 are not exact in binary; here they are.
		{in: "0.1", want: 100_000},
		{in: "0.2", want: 200_000},
		{in: "9223372036854.775807", want: math.MaxInt64},
		{in: "0.0000001", err: "more precise"},
		{in: "0.0000015", err: "more precise"},
		{in: "-1", err: "not a price"},
		{in: "abc", err: "not a price"},
		{in: ".inf", err: "not a price"},
		{in: ".nan", err: "not a price"},
		{in: "0x10", err: "not a price"},
		{in: "", err: "not a price"},
		{in: "9223372036854.775808", err: "too large"},
	} {
		got, err := ParseUSDPerMTok(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParseUSDPerMTok(%q) = %d, %v; want an error with %q", tc.in, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseUSDPerMTok(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestCost(t *testing.T) {
	p := Price{In: 3_000_000, CacheRead: 300_000, CacheWrite: 3_750_000, Out: 15_000_000}
	for _, tc := range []struct {
		name string
		u    llm.Usage
		want int64
	}{
		{"nothing", llm.Usage{}, 0},
		{"uncached", llm.Usage{Input: 1000, Output: 100}, 1000*3_000_000 + 100*15_000_000},
		{
			"cache read and write out of the input",
			llm.Usage{Input: 10_000, CacheRead: 6_000, CacheWrite: 2_000, Output: 500},
			2_000*3_000_000 + 6_000*300_000 + 2_000*3_750_000 + 500*15_000_000,
		},
		{"reasoning is inside output, not added", llm.Usage{Input: 10, Output: 100, Reasoning: 80}, 10*3_000_000 + 100*15_000_000},
		{"cache larger than input", llm.Usage{Input: 100, CacheRead: 150}, 150 * 300_000},
		{"negative counts are zero", llm.Usage{Input: -5, Output: -1}, 0},
		{"capped", llm.Usage{Input: math.MaxInt64, Output: math.MaxInt64}, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Cost(tc.u); got != tc.want {
				t.Fatalf("Cost = %d, want %d", got, tc.want)
			}
		})
	}
	// A million output tokens at $15 per million are exactly $15.
	if got := USD(p.Cost(llm.Usage{Output: 1_000_000})); got != 15 {
		t.Fatalf("USD = %v", got)
	}
}

func TestDollars(t *testing.T) {
	for usd, want := range map[float64]int64{
		0: 0, 1: PUSDPerUSD, 0.40: 400_000_000_000, 20: 20 * PUSDPerUSD, -0.5: -500_000_000_000,
		math.NaN(): 0, math.Inf(1): math.MaxInt64, math.Inf(-1): math.MinInt64, 1e10: math.MaxInt64,
	} {
		if got := PUSD(usd); got != want {
			t.Errorf("PUSD(%v) = %d, want %d", usd, got, want)
		}
	}
	if USD(400_000_000_000) != 0.4 {
		t.Fatal("USD")
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"gpt-4.1", "gpt-4.1", true},
		{"gpt-4.1", "gpt-4.1-mini", false},
		{"gpt-4.1*", "gpt-4.1-mini", true},
		{"gpt-4.1*", "gpt-4.1", true},
		{"*", "", true},
		{"*mini", "gpt-4.1-mini", true},
		{"gpt-*-mini", "gpt-4.1-mini", true},
		{"gpt-*-mini", "gpt-mini", false},
		{"a*a", "a", false},
		{"a*a", "aa", true},
		{"a*bc*bc", "abcbc", true},
		{"anthropic.claude-*:0", "anthropic.claude-3-5-sonnet-20240620-v1:0", true},
		{"llama3.1:*", "llama3.1:8b", true},
		{"deepseek-*", "DeepSeek-chat", false},
	} {
		if got := Match(tc.pattern, tc.s); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		errs       []string
	}{
		{"empty", "", []string{"the table is empty"}},
		{"unknown key", "version: v\nprices: []\nextra: 1\n", []string{"field extra not found"}},
		{"unknown row key", "version: v\nprices:\n  - {provider: a, model: m, from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}, per_mtok: 2}\n", []string{"field per_mtok not found"}},
		{"two documents", "version: v\nprices: []\n---\nversion: w\n", []string{"one YAML document"}},
		{
			"every problem at once",
			"prices:\n  - {provider: OpenAI, from: 2025-01-01, usd_per_mtok: {}}\n  - {provider: a, model: m, from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}}\n  - {provider: a, model: m, from: 2025-01-01, id: bad id, usd_per_mtok: {input: 1, output: 1}}\n",
			[]string{"version is required", "prices[0].provider", "prices[0].model: required", "prices[0].usd_per_mtok.input: required", "prices[0].usd_per_mtok.output: required", "prices[2]: the same provider, model and from as prices[1]", "prices[2].id"},
		},
		{"no from", "version: v\nprices:\n  - {provider: a, model: m, usd_per_mtok: {input: 1, output: 1}}\n", []string{"prices[0].from: required"}},
		{"duplicate id", "version: v\nprices:\n  - {provider: a, model: m, id: x, from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}}\n  - {provider: a, model: n, id: x, from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}}\n", []string{`prices[1].id: "x" is also prices[0]'s`}},
		{
			"bad values, all reported",
			"version: v\nprices:\n  - {provider: a, model: m, from: 2025-13-01, usd_per_mtok: {input: -1, cache_read: 0.0000001, output: [1]}}\n",
			[]string{"line 3: \"2025-13-01\" is not a date", "\"-1\" is not a price", "more precise", "a price is a number"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("accepted")
			}
			for _, e := range tc.errs {
				if !strings.Contains(err.Error(), e) {
					t.Errorf("error lacks %q:\n%v", e, err)
				}
			}
		})
	}
	if _, err := Load("testdata/missing.yaml"); err == nil {
		t.Fatal("a missing file")
	}
}

func TestExampleTable(t *testing.T) {
	tab, err := Load("../../examples/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ provider, model string }{
		{"anthropic", "claude-sonnet-4-5"}, {"deepseek", "deepseek-chat"}, {"openai", "gpt-4.1-mini"},
	} {
		if _, ok := tab.Lookup(m.provider, m.model, time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)); !ok {
			t.Errorf("the example prices nothing for %s %s", m.provider, m.model)
		}
	}
}
