package pricing

import (
	"slices"
	"testing"
	"time"
)

// The site's rows under a version of their own: taken before the file's
// where both would price a model alike, and not where the file's is the
// better row (an exact model over a glob, a later from); each cost names
// the site's row by the site's version; with no site's rows, the file's
// table is itself; with no file, the site's alone.
func TestWithSite(t *testing.T) {
	file, err := Parse([]byte(`
version: "t1"
prices:
  - {provider: openai, model: gpt-4.1-mini, from: 2025-04-14, id: mini, usd_per_mtok: {input: 0.4, output: 1.6}}
  - {provider: openai, model: "gpt-5*", from: 2025-08-07, usd_per_mtok: {input: 1.25, output: 10}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if file.WithSite("site-x", nil) != file {
		t.Error("no site's rows: not the file's table")
	}
	v := SiteVersion(time.Date(2026, 9, 30, 10, 15, 0, 999, time.FixedZone("CST", 8*3600)))
	if v != "site-20260930T021500Z" {
		t.Errorf("SiteVersion = %q", v)
	}
	tab := file.WithSite(v, []Row{
		{ID: "mini-cheaper", Provider: "openai", Model: "gpt-4.1-mini", From: day("2025-04-14"), In: 300_000, CacheRead: 300_000, CacheWrite: 300_000, Out: 1_200_000},
		{ID: "gpt5-glob", Provider: "openai", Model: "gpt-5*", From: day("2025-01-01"), In: 1, CacheRead: 1, CacheWrite: 1, Out: 1},
		{ID: "deepseek", Provider: "deepseek", Model: "deepseek-chat", From: day("2025-09-29"), In: 280_000, CacheRead: 28_000, CacheWrite: 280_000, Out: 420_000},
	})
	if tab.Version != "t1+site-20260930T021500Z" {
		t.Errorf("version %q", tab.Version)
	}
	at := day("2026-09-30")
	for _, c := range []struct {
		provider, model, version string
		in                       int64
	}{
		{"openai", "gpt-4.1-mini", "site-20260930T021500Z/mini-cheaper", 300_000},
		{"openai", "gpt-5-mini", "t1/1", 1_250_000},
		{"deepseek", "deepseek-chat", "site-20260930T021500Z/deepseek", 280_000},
	} {
		p, ok := tab.Lookup(c.provider, c.model, at)
		if !ok || p.Version != c.version || p.In != c.in {
			t.Errorf("Lookup(%s, %s) = %+v, %v; want %s", c.provider, c.model, p, ok, c.version)
		}
	}
	if p, _ := file.Lookup("openai", "gpt-4.1-mini", at); p.Version != "t1/mini" {
		t.Errorf("the file's table was changed: %+v", p)
	}
	rows := tab.Rows()
	if len(rows) != 5 || !rows[0].Site || rows[3].Site || rows[0].Name != "mini-cheaper" || rows[3].Name != "mini" || rows[4].Name != "1" {
		t.Errorf("Rows = %+v", rows)
	}
	if got := tab.Models("openai", at); !slices.Equal(got, []string{"gpt-4.1-mini"}) {
		t.Errorf("Models = %v", got)
	}
	alone := (*Table)(nil).WithSite(v, []Row{{ID: "a", Provider: "openai", Model: "o3", From: day("2025-01-01"), In: 2, Out: 8}})
	if p, ok := alone.Lookup("openai", "o3", at); !ok || alone.Version != v || p.Version != v+"/a" {
		t.Errorf("the site's alone: %q %+v %v", alone.Version, p, ok)
	}
}

func TestFormatAndParseUSD(t *testing.T) {
	for pusd, want := range map[int64]string{400_000: "0.4", 15_000_000: "15", 28_000: "0.028", 1: "0.000001", 0: "0"} {
		if got := FormatUSDPerMTok(pusd); got != want {
			t.Errorf("FormatUSDPerMTok(%d) = %q, want %q", pusd, got, want)
		}
		if back, err := ParseUSDPerMTok(want); err != nil || back != pusd {
			t.Errorf("ParseUSDPerMTok(%q) = %d, %v", want, back, err)
		}
	}
	for s, want := range map[string]int64{"2.5": 2_500_000_000_000, "0.000001": 1_000_000, "10": 10 * PUSDPerUSD, "+1.10": 1_100_000_000_000} {
		if got, err := ParseUSD(s, 6); err != nil || got != want {
			t.Errorf("ParseUSD(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "-1", "0.0000001", "1e3", "abc", "1,5", "1_000", "99999999"} {
		if _, err := ParseUSD(s, 6); err == nil {
			t.Errorf("ParseUSD(%q) was taken", s)
		}
	}
	if got := FormatUSD(2_500_000_000_000, 6); got != "2.500000" {
		t.Errorf("FormatUSD = %q", got)
	}
}
