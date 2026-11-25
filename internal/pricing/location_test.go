package pricing

import (
	"slices"
	"testing"
	"time"
)

// A price's from is a civil date in the school's zone, not necessarily the
// same date as UTC. It takes effect at the school's midnight.
func TestLookupInSchoolTimeZone(t *testing.T) {
	tab, err := Parse([]byte(`version: "school-zone"
prices:
  - {provider: openai, model: gpt-test, from: 2026-10-02, usd_per_mtok: {input: 1, output: 2}}
`))
	if err != nil {
		t.Fatal(err)
	}
	hk, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 10, 1, 15, 59, 59, 0, time.UTC)
	midnight := time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC) // 2 October, 00:00 in Hong Kong.
	if _, ok := tab.LookupIn("openai", "gpt-test", before, hk); ok {
		t.Fatal("the new price was active before the school's midnight")
	}
	if _, ok := tab.LookupIn("openai", "gpt-test", midnight, hk); !ok {
		t.Fatal("the new price was not active at the school's midnight")
	}
	if _, ok := tab.Lookup("openai", "gpt-test", midnight); ok {
		t.Fatal("the UTC lookup unexpectedly changed behavior")
	}
	if name, ok := tab.PricedAsIn("openai", "gpt-test", midnight, hk); !ok || name != "gpt-test" {
		t.Errorf("PricedAsIn = %q, %v", name, ok)
	}
	if got := tab.ModelsIn("openai", midnight, hk); !slices.Equal(got, []string{"gpt-test"}) {
		t.Errorf("ModelsIn = %v", got)
	}
}
