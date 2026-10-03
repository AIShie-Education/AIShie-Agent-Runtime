package openrouter

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "rewrite testdata/wire.golden.json")

func ptr[T any](v T) *T { return &v }

// Every rule of the routing, each at its member and in its own words: a
// slug's shape, a list's length, a slug twice, ignore covering what order
// or only names, order outside only, sort with order, data_collection's
// and quantizations' values, sort's, the thresholds' bounds, and
// max_price's decimals. A routing that sets nothing, or sets it well, has
// no problem.
func TestCheck(t *testing.T) {
	many := make([]string, 51)
	for i := range many {
		many[i] = "p" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	for _, tc := range []struct {
		name string
		r    Routing
		want []string // "pointer: message"
	}{
		{name: "nothing", r: Routing{}},
		{name: "everything, well", r: Routing{Order: []string{"deepinfra/turbo", "groq"}, AllowFallbacks: ptr(false), RequireParameters: ptr(true),
			DataCollection: ptr("deny"), ZDR: ptr(true), EnforceDistillableText: ptr(false), Only: []string{"groq", "deepinfra", "together"},
			Ignore: []string{"novita"}, Quantizations: []string{"fp8", "bf16", "unknown"},
			PreferredMinThroughput: &Threshold{P50: ptr(50.0), P90: ptr(20.0)}, PreferredMaxLatency: &Threshold{P50: ptr(3.0), bare: true},
			MaxPrice: &MaxPrice{Prompt: ptr("1"), Completion: ptr("2.50"), Request: ptr("0.000001"), Image: ptr("1000000")}}},
		{name: "slugs that pass", r: Routing{Only: []string{"groq", "deepinfra/turbo", "google-vertex/us-east5", "google-vertex/global/flex",
			"a/b.c_d-e", "x0/y/z/w"}}},
		{name: "bad slugs", r: Routing{Only: []string{"Groq", "-groq", "groq/", "a/b/c/d/e", "groq turbo", "a/" + strings.Repeat("b", 65),
			strings.Repeat("a", 64) + "/" + strings.Repeat("b", 64)}},
			want: []string{"/only/0: " + MsgSlug, "/only/1: " + MsgSlug, "/only/2: " + MsgSlug, "/only/3: " + MsgSlug, "/only/4: " + MsgSlug,
				"/only/5: " + MsgSlug, "/only/6: " + MsgSlug}},
		{name: "a list too long", r: Routing{Ignore: many}, want: []string{"/ignore: " + MsgList("ignore")}},
		{name: "a slug twice", r: Routing{Order: []string{"groq", "together", "groq"}}, want: []string{"/order/2: " + MsgDuplicate}},
		{name: "ignore covers order and only", r: Routing{Order: []string{"deepinfra/turbo"}, Only: []string{"deepinfra", "google-vertex/us-east5"},
			Ignore: []string{"deepinfra", "google-vertex", "groq", "deepinfra/turbo"}},
			want: []string{"/ignore/0: " + MsgIgnoreCovers, "/ignore/1: " + MsgIgnoreCovers, "/ignore/3: " + MsgIgnoreCovers}},
		{name: "a region ignored beside its base slug in only", r: Routing{Only: []string{"google-vertex"}, Ignore: []string{"google-vertex/us-east5"}}},
		{name: "order outside only", r: Routing{Order: []string{"groq", "google-vertex/us-east5", "together"}, Only: []string{"google-vertex", "groq"}},
			want: []string{"/order/2: " + MsgOrderNotOnly}},
		{name: "order without only", r: Routing{Order: []string{"together"}}},
		{name: "sort with order", r: Routing{Order: []string{"groq"}, Sort: &Sort{By: "price"}}, want: []string{"/sort: " + MsgSortWithOrder}},
		{name: "data_collection", r: Routing{DataCollection: ptr("never")}, want: []string{"/data_collection: " + MsgDataCollect}},
		{name: "data_collection empty", r: Routing{DataCollection: ptr("")}, want: []string{"/data_collection: " + MsgDataCollect}},
		{name: "quantizations", r: Routing{Quantizations: []string{"fp8", "FP8", "int3", "fp8"}},
			want: []string{"/quantizations/1: " + MsgQuantizations, "/quantizations/2: " + MsgQuantizations, "/quantizations/3: " + MsgQuantizations}},
		{name: "quantizations, too many", r: Routing{Quantizations: append(append([]string{}, Quantizations...), "fp8")},
			want: []string{"/quantizations: " + MsgQuantizations, "/quantizations/12: " + MsgQuantizations}},
		{name: "sort as a string", r: Routing{Sort: &Sort{By: "cheapest"}}, want: []string{"/sort: " + MsgSort}},
		{name: "sort as a mapping", r: Routing{Sort: &Sort{By: "cheapest", Partition: "provider", mapping: true}},
			want: []string{"/sort/by: " + MsgSort, "/sort/partition: " + MsgPartition}},
		{name: "sort as a mapping without by", r: Routing{Sort: &Sort{Partition: "model", mapping: true}}, want: []string{"/sort/by: " + MsgSort}},
		{name: "sort exacto, partitioned", r: Routing{Sort: &Sort{By: "exacto", Partition: "none", mapping: true}}},
		{name: "thresholds", r: Routing{PreferredMinThroughput: &Threshold{P50: ptr(0.0), bare: true},
			PreferredMaxLatency: &Threshold{P75: ptr(600.0), P90: ptr(600.5), P99: ptr(-1.0)}},
			want: []string{"/preferred_min_throughput: " + MsgThroughput, "/preferred_max_latency/p90: " + MsgLatency,
				"/preferred_max_latency/p99: " + MsgLatency}},
		{name: "a throughput too high", r: Routing{PreferredMinThroughput: &Threshold{P50: ptr(100000.5)}},
			want: []string{"/preferred_min_throughput/p50: " + MsgThroughput}},
		{name: "max_price", r: Routing{MaxPrice: &MaxPrice{Prompt: ptr("-1"), Completion: ptr("1e-3"), Request: ptr("0.0000001"), Image: ptr("1000000.01")}},
			want: []string{"/max_price/prompt: " + MsgPrice, "/max_price/completion: " + MsgPrice, "/max_price/request: " + MsgPrice,
				"/max_price/image: " + MsgPrice}},
		{name: "max_price at its bounds", r: Routing{MaxPrice: &MaxPrice{Prompt: ptr("0"), Completion: ptr("1000000.000000"), Request: ptr("01.50")}}},
		{name: "max_price, not decimals", r: Routing{MaxPrice: &MaxPrice{Prompt: ptr(""), Completion: ptr("1."), Request: ptr(".5"), Image: ptr(" 1")}},
			want: []string{"/max_price/prompt: " + MsgPrice, "/max_price/completion: " + MsgPrice, "/max_price/request: " + MsgPrice,
				"/max_price/image: " + MsgPrice}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, p := range tc.r.Check() {
				got = append(got, p.Pointer()+": "+p.Msg)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// A problem's place is a JSON Pointer for the API, and a dotted path with
// indexes for the configuration.
func TestProblemPaths(t *testing.T) {
	for _, tc := range []struct {
		p               Problem
		pointer, dotted string
	}{
		{Problem{}, "", ""},
		{Problem{Path: []any{"only", 2}}, "/only/2", "only[2]"},
		{Problem{Path: []any{"max_price", "prompt"}}, "/max_price/prompt", "max_price.prompt"},
		{Problem{Path: []any{"a/b~c"}}, "/a~1b~0c", "a/b~c"},
	} {
		if got := tc.p.Pointer(); got != tc.pointer {
			t.Errorf("%v: pointer %q, want %q", tc.p.Path, got, tc.pointer)
		}
		if got := tc.p.Dotted(); got != tc.dotted {
			t.Errorf("%v: dotted %q, want %q", tc.p.Path, got, tc.dotted)
		}
	}
}

// Coverage, both ways: a base slug covers its own endpoints, and nothing
// else; a slug with a part covers itself alone.
func TestCovers(t *testing.T) {
	for _, tc := range []struct {
		entry, s string
		want     bool
	}{
		{"google-vertex", "google-vertex/us-east5", true},
		{"google-vertex", "google-vertex", true},
		{"google-vertex/us-east5", "google-vertex", false},
		{"google-vertex/us-east5", "google-vertex/us-east5", true},
		{"google-vertex/us-east5", "google-vertex/us-east5/flex", false},
		{"google", "google-vertex/us-east5", false},
		{"deepinfra", "deepinfra/turbo", true},
		{"groq", "together", false},
	} {
		if got := Covers(tc.entry, tc.s); got != tc.want {
			t.Errorf("Covers(%q, %q) = %v", tc.entry, tc.s, got)
		}
	}
}

// The canonical form: only the members set, in OpenRouter's order; false
// kept; lists as given; sort {by} as its string, with a partition as the
// mapping; a threshold of p50 alone as its number; max_price's members as
// decimal strings in their shortest form; and nothing left, none.
func TestCanonical(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"nothing", `{}`, `null`},
		{"empty lists and mappings, and nulls", `{"order":[],"only":null,"sort":{},"preferred_max_latency":{},"max_price":{"prompt":null}}`, `null`},
		{"false kept", `{"allow_fallbacks":false}`, `{"allow_fallbacks":false}`},
		{"the order of the members", `{"max_price":{"image":"1","prompt":2},"zdr":true,"only":["groq","deepinfra"],"order":["groq"],"data_collection":"deny"}`,
			`{"order":["groq"],"data_collection":"deny","zdr":true,"only":["groq","deepinfra"],"max_price":{"prompt":"2","image":"1"}}`},
		{"sort by alone", `{"sort":{"by":"price"}}`, `{"sort":"price"}`},
		{"sort with a partition", `{"sort":{"partition":"none","by":"latency"}}`, `{"sort":{"by":"latency","partition":"none"}}`},
		{"p50 alone", `{"preferred_min_throughput":{"p50":3}}`, `{"preferred_min_throughput":3}`},
		{"percentiles", `{"preferred_max_latency":{"p99":10,"p50":0.5}}`, `{"preferred_max_latency":{"p50":0.5,"p99":10}}`},
		{"decimals", `{"max_price":{"prompt":"01.50","completion":"2.000000","request":0,"image":0.000125}}`,
			`{"max_price":{"prompt":"1.5","completion":"2","request":"0","image":"0.000125"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := Parse(json.RawMessage(tc.in))
			if p != nil {
				t.Fatalf("refused: %v", p)
			}
			if got := string(r.JSON()); got != tc.want {
				t.Errorf("%s, want %s", got, tc.want)
			}
		})
	}
	if (*Routing)(nil).Canonical() != nil || !Same(nil, &Routing{Only: []string{}}) || Same(nil, &Routing{ZDR: ptr(false)}) {
		t.Error("none is none, whatever is empty, and false is something")
	}
	r := &Routing{Only: []string{"groq"}, MaxPrice: &MaxPrice{Prompt: ptr("01.0")}}
	c := r.Canonical()
	c.Only[0] = "x"
	*c.MaxPrice.Prompt = "y"
	if r.Only[0] != "groq" || *r.MaxPrice.Prompt != "01.0" {
		t.Error("Canonical shares with the routing it was made from")
	}
}

// What is sent: a golden of the wire object, every member set, in
// OpenRouter's order, byte for byte; the contract's own example (YAML to
// the wire) beside it.
func TestWireGolden(t *testing.T) {
	every := `{"max_price":{"image":"0.01","request":0,"completion":"2.50","prompt":1},"preferred_max_latency":{"p90":3},` +
		`"preferred_min_throughput":{"p50":50,"p99":10},"quantizations":["fp8","bf16","unknown"],` +
		`"ignore":["novita"],"only":["groq","together","deepinfra"],"enforce_distillable_text":false,"zdr":true,"data_collection":"deny",` +
		`"require_parameters":true,"allow_fallbacks":false,"order":["deepinfra/turbo","groq"]}`
	r, p := Parse(json.RawMessage(every))
	if p != nil {
		t.Fatal(p)
	}
	// sort with order is refused: the golden holds sort apart.
	got := string(r.JSON()) + "\n"
	sorted, p := Parse(json.RawMessage(`{"sort":{"by":"exacto","partition":"model"},"only":["groq"]}`))
	if p != nil {
		t.Fatal(p)
	}
	got += string(sorted.JSON()) + "\n"

	var y struct {
		Model struct {
			OpenRouter *Routing `yaml:"openrouter"`
		} `yaml:"model"`
	}
	const example = `
model:
  openrouter:
    data_collection: deny
    require_parameters: true
    allow_fallbacks: false
    only: [groq, together, deepinfra]
    order: [deepinfra/turbo, groq]
    quantizations: [fp8, bf16, unknown]
    preferred_max_latency: {p90: 3}
    max_price: {prompt: 1, completion: "2.50"}
`
	if err := yaml.Unmarshal([]byte(example), &y); err != nil {
		t.Fatal(err)
	}
	if len(y.Model.OpenRouter.Check()) != 0 {
		t.Fatalf("the example: %v", y.Model.OpenRouter.Check())
	}
	got += string(y.Model.OpenRouter.JSON()) + "\n"
	const contract = `{"order":["deepinfra/turbo","groq"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny",` +
		`"only":["groq","together","deepinfra"],"quantizations":["fp8","bf16","unknown"],"preferred_max_latency":{"p90":3},` +
		`"max_price":{"prompt":"1","completion":"2.5"}}`
	if string(y.Model.OpenRouter.JSON()) != contract {
		t.Errorf("the contract's example:\n%s\nwant\n%s", y.Model.OpenRouter.JSON(), contract)
	}

	path := filepath.Join("testdata", "wire.golden.json")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test -update writes it)", err)
	}
	if got != string(want) {
		t.Errorf("the wire differs from %s:\n%s", path, got)
	}
}

// Parse answers the first problem only: member by member in OpenRouter's
// order, then by index, a member of no such name first; and at any depth
// a member of no such name is unknown. Of the wrong kind, a member is
// refused at itself.
func TestParseFirstProblem(t *testing.T) {
	for _, tc := range []struct {
		in, pointer, msg string
		unknown          bool
	}{
		{`[]`, "", MsgObject, false},
		{`"deny"`, "", MsgObject, false},
		{`{"providers":["groq"],"order":["Groq"]}`, "/providers", MsgUnknown, true},
		{`{"max_price":{"prompt":"x"},"order":["Groq"]}`, "/order/0", MsgSlug, false},
		{`{"only":["groq","Bad","worse!"]}`, "/only/1", MsgSlug, false},
		{`{"only":["groq",3]}`, "/only", MsgList("only"), false},
		{`{"ignore":"groq"}`, "/ignore", MsgList("ignore"), false},
		{`{"order":["groq","groq"]}`, "/order/1", MsgDuplicate, false},
		{`{"ignore":["google-vertex"],"only":["google-vertex/us-east5"]}`, "/ignore/0", MsgIgnoreCovers, false},
		{`{"order":["together"],"only":["groq"]}`, "/order/0", MsgOrderNotOnly, false},
		{`{"order":["groq"],"sort":"price"}`, "/sort", MsgSortWithOrder, false},
		{`{"allow_fallbacks":"yes"}`, "/allow_fallbacks", MsgBool("allow_fallbacks"), false},
		{`{"zdr":1}`, "/zdr", MsgBool("zdr"), false},
		{`{"data_collection":false}`, "/data_collection", MsgDataCollect, false},
		{`{"data_collection":"maybe"}`, "/data_collection", MsgDataCollect, false},
		{`{"quantizations":"fp8"}`, "/quantizations", MsgQuantizations, false},
		{`{"quantizations":["fp8",8]}`, "/quantizations/1", MsgQuantizations, false},
		{`{"sort":3}`, "/sort", MsgSort, false},
		{`{"sort":""}`, "/sort", MsgSort, false},
		{`{"sort":{"by":"price","order":"asc"}}`, "/sort/order", MsgUnknown, true},
		{`{"sort":{"by":1}}`, "/sort/by", MsgSort, false},
		{`{"sort":{"by":"price","partition":"all"}}`, "/sort/partition", MsgPartition, false},
		{`{"preferred_min_throughput":"fast"}`, "/preferred_min_throughput", MsgThroughput, false},
		{`{"preferred_min_throughput":{"p95":3}}`, "/preferred_min_throughput/p95", MsgUnknown, true},
		{`{"preferred_max_latency":{"p50":"3"}}`, "/preferred_max_latency/p50", MsgLatency, false},
		{`{"preferred_max_latency":{"p50":1,"p90":700}}`, "/preferred_max_latency/p90", MsgLatency, false},
		{`{"max_price":"1"}`, "/max_price", MsgPrice, false},
		{`{"max_price":{"tokens":"1"}}`, "/max_price/tokens", MsgUnknown, true},
		{`{"max_price":{"prompt":true}}`, "/max_price/prompt", MsgPrice, false},
		{`{"max_price":{"completion":1e-3,"prompt":"2"}}`, "/max_price/completion", MsgPrice, false},
		{`{"max_price":{"image":"1","request":"-0"}}`, "/max_price/request", MsgPrice, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			r, p := Parse(json.RawMessage(tc.in))
			if p == nil || r != nil {
				t.Fatalf("taken: %v", r.JSON())
			}
			if p.Pointer() != tc.pointer || p.Msg != tc.msg || p.Unknown != tc.unknown {
				t.Errorf("%s: %s (unknown %v); want %s: %s", p.Pointer(), p.Msg, p.Unknown, tc.pointer, tc.msg)
			}
		})
	}
	for _, in := range []string{``, `null`, `{}`, `{"only":[],"sort":null,"max_price":{}}`} {
		if r, p := Parse(json.RawMessage(in)); r != nil || p != nil {
			t.Errorf("%q: %v %v; want none", in, r, p)
		}
	}
}

// The union members read and write their forms through JSON and YAML
// alike; a routing read back from its JSON is the same routing.
func TestUnionForms(t *testing.T) {
	const doc = `
sort: {by: throughput, partition: none}
preferred_min_throughput: 40
preferred_max_latency: {p50: 1.5, p99: 8}
max_price: {prompt: 0.40, completion: "1.60", request: 0}
`
	var r Routing
	if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
		t.Fatal(err)
	}
	const want = `{"sort":{"by":"throughput","partition":"none"},"preferred_min_throughput":40,"preferred_max_latency":{"p50":1.5,"p99":8},` +
		`"max_price":{"prompt":"0.4","completion":"1.6","request":"0"}}`
	if got := string(r.JSON()); got != want {
		t.Fatalf("from YAML: %s", got)
	}
	var back Routing
	if err := json.Unmarshal(r.JSON(), &back); err != nil || string(back.JSON()) != want {
		t.Fatalf("from its JSON: %s, %v", back.JSON(), err)
	}
	out, err := yaml.Marshal(r.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	var again Routing
	if err := yaml.Unmarshal(out, &again); err != nil || string(again.JSON()) != want {
		t.Fatalf("through YAML again:\n%s%s, %v", out, again.JSON(), err)
	}
	if !Same(&r, r.Clone()) || r.Clone().MaxPrice == r.MaxPrice {
		t.Error("Clone")
	}
	if g := r.Generic(); g["preferred_min_throughput"] != 40.0 || g["sort"].(map[string]any)["by"] != "throughput" {
		t.Errorf("Generic: %v", g)
	}
}
