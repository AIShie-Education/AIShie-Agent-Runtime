// Package openrouter is OpenRouter's upstream routing: its provider
// routing object, which says which of the upstream providers serving a
// model may answer a call, which are tried first, and on what terms, as
// the configuration, the store and the API hold it and the adapters send
// it (as `provider`, in every call to OpenRouter); and the list of the
// upstream providers serving one model, which the administrators' page
// reads (catalogue.go). See docs/design.md §3 and §11.5.
//
// It is a leaf: it depends on nothing of the runtime's, so that config,
// llm, store and api may all import it.
package openrouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Routing is OpenRouter's provider routing, its members named as
// OpenRouter names them. Every member is optional: one not set is not
// sent, and OpenRouter's default applies. The fields are in OpenRouter's
// own order, which is the order the canonical form (Canonical) and the
// wire keep.
type Routing struct {
	// Order are the upstream providers' slugs tried first, in order.
	Order []string `json:"order,omitempty" yaml:"order,omitempty"`
	// AllowFallbacks lets OpenRouter go on to other upstreams when those
	// of Order (or the one it picks first) cannot answer; true when unset.
	AllowFallbacks *bool `json:"allow_fallbacks,omitempty" yaml:"allow_fallbacks,omitempty"`
	// RequireParameters routes only to upstreams that take every
	// parameter of the call.
	RequireParameters *bool `json:"require_parameters,omitempty" yaml:"require_parameters,omitempty"`
	// DataCollection is allow or deny: deny leaves out upstreams that may
	// store what they are sent.
	DataCollection *string `json:"data_collection,omitempty" yaml:"data_collection,omitempty"`
	// ZDR routes only to zero-data-retention endpoints.
	ZDR *bool `json:"zdr,omitempty" yaml:"zdr,omitempty"`
	// EnforceDistillableText routes only to models whose maker allows
	// distillation.
	EnforceDistillableText *bool `json:"enforce_distillable_text,omitempty" yaml:"enforce_distillable_text,omitempty"`
	// Only are the upstreams that may answer, and Ignore those that may
	// not.
	Only   []string `json:"only,omitempty" yaml:"only,omitempty"`
	Ignore []string `json:"ignore,omitempty" yaml:"ignore,omitempty"`
	// Quantizations are the precisions an upstream may run the model at.
	Quantizations []string `json:"quantizations,omitempty" yaml:"quantizations,omitempty"`
	// Sort orders the upstreams by price, throughput, latency or exacto,
	// strictly, in place of OpenRouter's load balancing.
	Sort *Sort `json:"sort,omitempty" yaml:"sort,omitempty"`
	// PreferredMinThroughput (tokens a second) and PreferredMaxLatency
	// (seconds) put the upstreams that miss them last.
	PreferredMinThroughput *Threshold `json:"preferred_min_throughput,omitempty" yaml:"preferred_min_throughput,omitempty"`
	PreferredMaxLatency    *Threshold `json:"preferred_max_latency,omitempty" yaml:"preferred_max_latency,omitempty"`
	// MaxPrice leaves out the upstreams that charge more.
	MaxPrice *MaxPrice `json:"max_price,omitempty" yaml:"max_price,omitempty"`
}

// Sort is sort: a string, or {by, partition}. By alone is written as the
// string.
type Sort struct {
	By        string
	Partition string
	// mapping is set for a sort read as a mapping, so that a problem with
	// it is placed at sort.by rather than at sort.
	mapping bool
}

// Threshold is preferred_min_throughput or preferred_max_latency: a
// number, or the percentiles {p50, p75, p90, p99}. P50 alone is written as
// the number.
type Threshold struct {
	P50, P75, P90, P99 *float64
	// bare is set for a threshold read as a number, so that a problem with
	// it is placed at the member rather than at its p50.
	bare bool
}

// MaxPrice is max_price: dollars, each a decimal, prompt and completion
// per million tokens, request per request, image per image. Each is kept
// as it was written (a number's text, or a string's), and written as a
// decimal string (Canonical), as OpenRouter's API types them.
type MaxPrice struct {
	Prompt, Completion, Request, Image *string
}

// Members are the routing's members in OpenRouter's order, which the
// canonical form keeps.
var Members = []string{"order", "allow_fallbacks", "require_parameters", "data_collection", "zdr", "enforce_distillable_text",
	"only", "ignore", "quantizations", "sort", "preferred_min_throughput", "preferred_max_latency", "max_price"}

// The values of the members that take a closed list.
var (
	DataCollections = []string{"allow", "deny"}
	Quantizations   = []string{"int4", "int8", "fp4", "mxfp4", "nvfp4", "fp6", "fp8", "mxfp8", "fp16", "bf16", "fp32", "unknown"}
	SortBy          = []string{"price", "throughput", "latency", "exacto"}
	Partitions      = []string{"model", "none"}
	PercentileNames = []string{"p50", "p75", "p90", "p99"}
	PriceMembers    = []string{"prompt", "completion", "request", "image"}
)

// The bounds.
const (
	// MaxSlugs bounds order, only and ignore; MaxSlug a slug, in bytes.
	MaxSlugs = 50
	MaxSlug  = 128
	// MaxThroughput is the highest preferred_min_throughput, in tokens a
	// second; MaxLatency the highest preferred_max_latency, in seconds.
	MaxThroughput = 100000
	MaxLatency    = 600
	// MaxPriceUSD is the highest of max_price's members, in dollars, and
	// PricePlaces the most decimal places one may have.
	MaxPriceUSD = 1000000
	PricePlaces = 6
)

// SlugRe is what an upstream provider's slug is: OpenRouter's provider
// slug, then up to three /parts (a region, a variant).
var SlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}(?:/[a-z0-9][a-z0-9._-]{0,63}){0,3}$`)

// decimalRe is what a max_price member is written as: digits, and a
// fraction; no sign, no exponent.
var decimalRe = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

// The problems' messages, the same for the YAML and the API.
const (
	MsgObject        = "openrouter is OpenRouter's provider routing: an object, or null"
	MsgUnknown       = "OpenRouter's provider routing has no such member"
	MsgSlug          = "a slug is lower-case letters, digits and -, then /parts, such as deepinfra/turbo; at most 128 characters"
	MsgDuplicate     = "this slug is in the list already"
	MsgIgnoreCovers  = "this slug skips an upstream provider that order or only names"
	MsgOrderNotOnly  = "tried first, but not among only"
	MsgSortWithOrder = "sort is not used while order is set: choose one"
	MsgDataCollect   = "data_collection is allow or deny"
	MsgQuantizations = "quantizations are int4, int8, fp4, mxfp4, nvfp4, fp6, fp8, mxfp8, fp16, bf16, fp32 or unknown, each once"
	MsgSort          = "sort is price, throughput, latency or exacto, or {by, partition}"
	MsgPartition     = "partition is model or none"
	MsgThroughput    = "preferred_min_throughput is tokens a second, more than 0 and at most 100000, or {p50, p75, p90, p99} of them"
	MsgLatency       = "preferred_max_latency is seconds, more than 0 and at most 600, or {p50, p75, p90, p99} of them"
	MsgPrice         = "dollars, 0 or more, at most 1,000,000, at most 6 decimal places"
)

// MsgList is the message of a list that is not one of slugs, or too long.
func MsgList(name string) string {
	return name + " is a list of at most 50 upstream providers' slugs"
}

// MsgBool is the message of a member that is not true or false.
func MsgBool(name string) string { return name + " is true or false" }

// Problem is one thing wrong with a routing, and where: Path is from the
// routing, member names and list indexes (string and int), empty for the
// routing itself.
type Problem struct {
	Path []any
	Msg  string
	// Unknown is set for a member the routing has no such of.
	Unknown bool
}

// Pointer is where the problem is as a JSON Pointer from the routing:
// /only/2, /max_price/prompt; "" for the routing itself.
func (p Problem) Pointer() string {
	var b strings.Builder
	for _, x := range p.Path {
		b.WriteByte('/')
		switch v := x.(type) {
		case int:
			b.WriteString(strconv.Itoa(v))
		case string:
			b.WriteString(strings.ReplaceAll(strings.ReplaceAll(v, "~", "~0"), "/", "~1"))
		}
	}
	return b.String()
}

// Dotted is where the problem is as the configuration's paths say it:
// only[2], max_price.prompt; "" for the routing itself.
func (p Problem) Dotted() string {
	var b strings.Builder
	for _, x := range p.Path {
		switch v := x.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", v)
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		}
	}
	return b.String()
}

func (p Problem) Error() string {
	if at := p.Dotted(); at != "" {
		return at + ": " + p.Msg
	}
	return p.Msg
}

// Covers reports whether the slug entry covers the slug s: s is entry,
// or entry is a provider's slug alone (no /) and s one of its endpoints
// (entry/…), as OpenRouter matches a base slug.
func Covers(entry, s string) bool {
	return s == entry || !strings.Contains(entry, "/") && strings.HasPrefix(s, entry+"/")
}

// coveredBy is the first of list that covers s, and whether one does.
func coveredBy(list []string, s string) (string, bool) {
	for _, e := range list {
		if Covers(e, s) {
			return e, true
		}
	}
	return "", false
}

// Check is what is wrong with r's values (each member's own rules, and
// those between members), with paths from the routing, by member in
// OpenRouter's order, then by index. A member not set is never wrong.
// What JSON or YAML gives of the wrong kind is Parse's, or the
// configuration walker's, to refuse.
func (r *Routing) Check() []Problem {
	if r == nil {
		return nil
	}
	var out []Problem
	add := func(msg string, path ...any) { out = append(out, Problem{Path: path, Msg: msg}) }
	slugs := func(name string, list []string) {
		if len(list) > MaxSlugs {
			add(MsgList(name), name)
		}
		seen := map[string]bool{}
		for i, s := range list {
			switch {
			case len(s) > MaxSlug || !SlugRe.MatchString(s):
				add(MsgSlug, name, i)
			case seen[s]:
				add(MsgDuplicate, name, i)
			}
			seen[s] = true
		}
	}
	slugs("order", r.Order)
	if len(r.Only) > 0 {
		for i, s := range r.Order {
			if _, ok := coveredBy(r.Only, s); !ok {
				add(MsgOrderNotOnly, "order", i)
			}
		}
	}
	if r.DataCollection != nil && !slices.Contains(DataCollections, *r.DataCollection) {
		add(MsgDataCollect, "data_collection")
	}
	slugs("only", r.Only)
	slugs("ignore", r.Ignore)
	for i, e := range r.Ignore {
		named := false
		for _, s := range append(slices.Clone(r.Order), r.Only...) {
			named = named || Covers(e, s)
		}
		if named {
			add(MsgIgnoreCovers, "ignore", i)
		}
	}
	if len(r.Quantizations) > len(Quantizations) {
		add(MsgQuantizations, "quantizations")
	}
	seen := map[string]bool{}
	for i, q := range r.Quantizations {
		if !slices.Contains(Quantizations, q) || seen[q] {
			add(MsgQuantizations, "quantizations", i)
		}
		seen[q] = true
	}
	if s := r.Sort; !s.zero() {
		switch {
		case !slices.Contains(SortBy, s.By) && !s.mapping && s.Partition == "":
			add(MsgSort, "sort")
		case !slices.Contains(SortBy, s.By):
			add(MsgSort, "sort", "by")
		case len(r.Order) > 0:
			add(MsgSortWithOrder, "sort")
		}
		if s.Partition != "" && !slices.Contains(Partitions, s.Partition) {
			add(MsgPartition, "sort", "partition")
		}
	}
	threshold := func(name, msg string, t *Threshold, most float64) {
		if t.zero() {
			return
		}
		for i, v := range t.values() {
			if v == nil || finite(*v) && *v > 0 && *v <= most {
				continue
			}
			if t.bare && i == 0 {
				add(msg, name)
			} else {
				add(msg, name, PercentileNames[i])
			}
		}
	}
	threshold("preferred_min_throughput", MsgThroughput, r.PreferredMinThroughput, MaxThroughput)
	threshold("preferred_max_latency", MsgLatency, r.PreferredMaxLatency, MaxLatency)
	if p := r.MaxPrice; p != nil {
		for i, v := range p.values() {
			if v != nil && !validPrice(*v) {
				add(MsgPrice, "max_price", PriceMembers[i])
			}
		}
	}
	sortProblems(out)
	return out
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// validPrice reports whether s is a max_price member: a decimal, no sign
// and no exponent, of at most PricePlaces places, from 0 to MaxPriceUSD.
func validPrice(s string) bool {
	if !decimalRe.MatchString(s) {
		return false
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > PricePlaces {
		return false
	}
	whole = strings.TrimLeft(whole, "0")
	if len(whole) > len(strconv.Itoa(MaxPriceUSD)) {
		return false
	}
	n, _ := strconv.Atoi("0" + whole)
	return n < MaxPriceUSD || n == MaxPriceUSD && strings.Trim(frac, "0") == ""
}

// CanonicalDecimal is a max_price member in its shortest form: leading
// zeros and trailing fractional zeros removed, and no '.' without a
// fraction ("01.50" → "1.5", "2.000000" → "2"). One that is not a decimal
// is as it was.
func CanonicalDecimal(s string) string {
	if !decimalRe.MatchString(s) {
		return s
	}
	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

func (s *Sort) zero() bool { return s == nil || s.By == "" && s.Partition == "" }

func (t *Threshold) values() []*float64 { return []*float64{t.P50, t.P75, t.P90, t.P99} }

func (t *Threshold) zero() bool {
	return t == nil || t.P50 == nil && t.P75 == nil && t.P90 == nil && t.P99 == nil
}

func (p *MaxPrice) values() []*string { return []*string{p.Prompt, p.Completion, p.Request, p.Image} }

func (p *MaxPrice) zero() bool {
	return p == nil || p.Prompt == nil && p.Completion == nil && p.Request == nil && p.Image == nil
}

// IsZero reports whether r sets nothing: nil, or every member empty.
func (r *Routing) IsZero() bool {
	return r == nil || len(r.Order) == 0 && r.AllowFallbacks == nil && r.RequireParameters == nil && r.DataCollection == nil &&
		r.ZDR == nil && r.EnforceDistillableText == nil && len(r.Only) == 0 && len(r.Ignore) == 0 && len(r.Quantizations) == 0 &&
		r.Sort.zero() && r.PreferredMinThroughput.zero() && r.PreferredMaxLatency.zero() && r.MaxPrice.zero()
}

// Canonical is r as it is stored, answered and sent: only the members set,
// a list empty, a mapping empty or a member null being none; sort {by}
// alone as its string; a threshold of p50 alone as its number; and
// max_price's members as decimal strings in their shortest form. nil when
// nothing is left. r is not changed, and the result shares nothing with
// it.
func (r *Routing) Canonical() *Routing {
	if r.IsZero() {
		return nil
	}
	c := &Routing{
		Order: cloneList(r.Order), AllowFallbacks: clonePtr(r.AllowFallbacks), RequireParameters: clonePtr(r.RequireParameters),
		DataCollection: clonePtr(r.DataCollection), ZDR: clonePtr(r.ZDR), EnforceDistillableText: clonePtr(r.EnforceDistillableText),
		Only: cloneList(r.Only), Ignore: cloneList(r.Ignore), Quantizations: cloneList(r.Quantizations),
	}
	if !r.Sort.zero() {
		c.Sort = &Sort{By: r.Sort.By, Partition: r.Sort.Partition}
	}
	for _, t := range []struct {
		from *Threshold
		into **Threshold
	}{{r.PreferredMinThroughput, &c.PreferredMinThroughput}, {r.PreferredMaxLatency, &c.PreferredMaxLatency}} {
		if !t.from.zero() {
			*t.into = &Threshold{P50: clonePtr(t.from.P50), P75: clonePtr(t.from.P75), P90: clonePtr(t.from.P90), P99: clonePtr(t.from.P99)}
		}
	}
	if !r.MaxPrice.zero() {
		c.MaxPrice = &MaxPrice{}
		for i, v := range r.MaxPrice.values() {
			if v != nil {
				d := CanonicalDecimal(*v)
				*c.MaxPrice.members()[i] = &d
			}
		}
	}
	return c
}

// Clone is a copy of r that shares nothing with it, as r is: not made
// canonical.
func (r *Routing) Clone() *Routing {
	if r == nil {
		return nil
	}
	c := *r
	c.Order, c.Only, c.Ignore, c.Quantizations = slices.Clone(r.Order), slices.Clone(r.Only), slices.Clone(r.Ignore), slices.Clone(r.Quantizations)
	c.AllowFallbacks, c.RequireParameters, c.DataCollection = clonePtr(r.AllowFallbacks), clonePtr(r.RequireParameters), clonePtr(r.DataCollection)
	c.ZDR, c.EnforceDistillableText, c.Sort = clonePtr(r.ZDR), clonePtr(r.EnforceDistillableText), clonePtr(r.Sort)
	for _, t := range []**Threshold{&c.PreferredMinThroughput, &c.PreferredMaxLatency} {
		if *t != nil {
			x := **t
			x.P50, x.P75, x.P90, x.P99 = clonePtr(x.P50), clonePtr(x.P75), clonePtr(x.P90), clonePtr(x.P99)
			*t = &x
		}
	}
	if r.MaxPrice != nil {
		p := *r.MaxPrice
		p.Prompt, p.Completion, p.Request, p.Image = clonePtr(p.Prompt), clonePtr(p.Completion), clonePtr(p.Request), clonePtr(p.Image)
		c.MaxPrice = &p
	}
	return &c
}

func cloneList(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	return slices.Clone(l)
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// JSON is r's canonical form as JSON, as it is stored and sent; null for
// none.
func (r *Routing) JSON() []byte {
	c := r.Canonical()
	if c == nil {
		return []byte("null")
	}
	b, err := json.Marshal(c)
	if err != nil { // a Routing always marshals
		return []byte("null")
	}
	return b
}

// Same reports whether a and b route alike: their canonical forms are
// equal.
func Same(a, b *Routing) bool { return bytes.Equal(a.JSON(), b.JSON()) }

// Generic is r's canonical form as generic JSON (maps, lists, strings,
// numbers, booleans), as a model section holds it; nil for none.
func (r *Routing) Generic() map[string]any {
	c := r.Canonical()
	if c == nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(c.JSON(), &m) != nil {
		return nil
	}
	return m
}

// MarshalJSON writes the sort as its string when it has no partition, and
// as {by, partition} otherwise.
func (s Sort) MarshalJSON() ([]byte, error) {
	if s.Partition == "" {
		return json.Marshal(s.By)
	}
	return json.Marshal(struct {
		By        string `json:"by"`
		Partition string `json:"partition"`
	}{s.By, s.Partition})
}

// UnmarshalJSON reads a sort as a string or {by, partition}.
func (s *Sort) UnmarshalJSON(b []byte) error {
	var by string
	if json.Unmarshal(b, &by) == nil {
		*s = Sort{By: by}
		return nil
	}
	var m struct {
		By        string `json:"by"`
		Partition string `json:"partition"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("openrouter: %s", MsgSort)
	}
	*s = Sort{By: m.By, Partition: m.Partition, mapping: true}
	return nil
}

// MarshalYAML writes the sort as MarshalJSON does.
func (s Sort) MarshalYAML() (any, error) {
	if s.Partition == "" {
		return s.By, nil
	}
	return map[string]string{"by": s.By, "partition": s.Partition}, nil
}

// UnmarshalYAML reads a sort as a string or {by, partition}.
func (s *Sort) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = Sort{By: n.Value}
		return nil
	case yaml.MappingNode:
		var m struct {
			By        string `yaml:"by"`
			Partition string `yaml:"partition"`
		}
		if err := n.Decode(&m); err != nil {
			return err
		}
		*s = Sort{By: m.By, Partition: m.Partition, mapping: true}
		return nil
	}
	return fmt.Errorf("openrouter: %s", MsgSort)
}

// percentileMap is a threshold as a mapping, with the percentiles set.
type percentileMap struct {
	P50 *float64 `json:"p50,omitempty" yaml:"p50,omitempty"`
	P75 *float64 `json:"p75,omitempty" yaml:"p75,omitempty"`
	P90 *float64 `json:"p90,omitempty" yaml:"p90,omitempty"`
	P99 *float64 `json:"p99,omitempty" yaml:"p99,omitempty"`
}

func (t Threshold) asNumber() bool {
	return t.P50 != nil && t.P75 == nil && t.P90 == nil && t.P99 == nil
}

// MarshalJSON writes a threshold of p50 alone as the number, and any
// other as the mapping of the percentiles set.
func (t Threshold) MarshalJSON() ([]byte, error) {
	if t.asNumber() {
		return json.Marshal(*t.P50)
	}
	return json.Marshal(percentileMap{t.P50, t.P75, t.P90, t.P99})
}

// UnmarshalJSON reads a threshold as a number or {p50, p75, p90, p99}.
func (t *Threshold) UnmarshalJSON(b []byte) error {
	var n float64
	if json.Unmarshal(b, &n) == nil {
		*t = Threshold{P50: &n, bare: true}
		return nil
	}
	var m percentileMap
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("openrouter: a threshold is a number or {p50, p75, p90, p99}")
	}
	*t = Threshold{P50: m.P50, P75: m.P75, P90: m.P90, P99: m.P99}
	return nil
}

// MarshalYAML writes the threshold as MarshalJSON does.
func (t Threshold) MarshalYAML() (any, error) {
	if t.asNumber() {
		return *t.P50, nil
	}
	return percentileMap{t.P50, t.P75, t.P90, t.P99}, nil
}

// UnmarshalYAML reads a threshold as a number or {p50, p75, p90, p99}.
func (t *Threshold) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var v float64
		if err := n.Decode(&v); err != nil {
			return err
		}
		*t = Threshold{P50: &v, bare: true}
		return nil
	case yaml.MappingNode:
		var m percentileMap
		if err := n.Decode(&m); err != nil {
			return err
		}
		*t = Threshold{P50: m.P50, P75: m.P75, P90: m.P90, P99: m.P99}
		return nil
	}
	return fmt.Errorf("openrouter: a threshold is a number or {p50, p75, p90, p99}")
}

// prices is max_price as a mapping of what each member was written as.
type prices struct {
	Prompt     *string `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Completion *string `json:"completion,omitempty" yaml:"completion,omitempty"`
	Request    *string `json:"request,omitempty" yaml:"request,omitempty"`
	Image      *string `json:"image,omitempty" yaml:"image,omitempty"`
}

// MarshalJSON writes max_price's members set, each a string.
func (p MaxPrice) MarshalJSON() ([]byte, error) {
	return json.Marshal(prices(p))
}

// UnmarshalJSON reads max_price's members, each a string or a number,
// kept as written.
func (p *MaxPrice) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return fmt.Errorf("openrouter: max_price is {prompt, completion, request, image}")
	}
	*p = MaxPrice{}
	for k, v := range raw {
		i := slices.Index(PriceMembers, k)
		if i < 0 {
			return fmt.Errorf("openrouter: max_price has no member %q", k)
		}
		s, ok := decimalText(v)
		if !ok {
			return fmt.Errorf("openrouter: max_price.%s: %s", k, MsgPrice)
		}
		if s != nil {
			*p.members()[i] = s
		}
	}
	return nil
}

func (p *MaxPrice) members() []**string {
	return []**string{&p.Prompt, &p.Completion, &p.Request, &p.Image}
}

// decimalText is a max_price member as written: a string's value, or a
// number's text; nil for null; false for anything else.
func decimalText(v json.RawMessage) (*string, bool) {
	v = bytes.TrimSpace(v)
	switch {
	case len(v) == 0:
		return nil, false
	case bytes.Equal(v, []byte("null")):
		return nil, true
	case v[0] == '"':
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil, false
		}
		return &s, true
	case v[0] == '-' || v[0] >= '0' && v[0] <= '9':
		var n json.Number
		if json.Unmarshal(v, &n) != nil {
			return nil, false
		}
		s := string(v)
		return &s, true
	}
	return nil, false
}

// MarshalYAML writes max_price's members set, each a string.
func (p MaxPrice) MarshalYAML() (any, error) { return prices(p), nil }

// UnmarshalYAML reads max_price's members, each a string or a number,
// kept as written.
func (p *MaxPrice) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("openrouter: max_price is {prompt, completion, request, image}")
	}
	*p = MaxPrice{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		j := slices.Index(PriceMembers, k)
		switch {
		case j < 0:
			return fmt.Errorf("openrouter: max_price has no member %q", k)
		case v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null":
		case v.Kind == yaml.ScalarNode && (v.ShortTag() == "!!str" || v.ShortTag() == "!!int" || v.ShortTag() == "!!float"):
			s := v.Value
			*p.members()[j] = &s
		default:
			return fmt.Errorf("openrouter: max_price.%s: %s", k, MsgPrice)
		}
	}
	return nil
}
