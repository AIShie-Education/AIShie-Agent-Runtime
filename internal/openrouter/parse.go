package openrouter

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
)

// Parse reads a routing as the API takes it: a JSON object, or null. It
// returns the routing as given (Canonical makes it canonical), or the
// first problem with it, by member in OpenRouter's order, then by index:
// a member of no such name, at any depth; a member of the wrong kind; and
// what Check finds. Every problem's path is from the routing. null, and a
// routing that sets nothing ({}, empty lists and mappings, null members),
// are nil with no problem.
func Parse(raw json.RawMessage) (*Routing, *Problem) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var members map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &members) != nil {
		return nil, &Problem{Msg: MsgObject}
	}
	p := &parser{}
	r := &Routing{}
	names := make([]string, 0, len(members))
	for k := range members {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := members[k]
		if isNull(v) {
			if !slices.Contains(Members, k) {
				p.unknown(k)
			}
			continue
		}
		switch k {
		case "order":
			r.Order = p.slugs(k, v)
		case "only":
			r.Only = p.slugs(k, v)
		case "ignore":
			r.Ignore = p.slugs(k, v)
		case "allow_fallbacks":
			r.AllowFallbacks = p.bool(k, v)
		case "require_parameters":
			r.RequireParameters = p.bool(k, v)
		case "zdr":
			r.ZDR = p.bool(k, v)
		case "enforce_distillable_text":
			r.EnforceDistillableText = p.bool(k, v)
		case "data_collection":
			var s string
			if json.Unmarshal(v, &s) != nil {
				p.add(MsgDataCollect, k)
			} else {
				r.DataCollection = &s
			}
		case "quantizations":
			r.Quantizations = p.quantizations(v)
		case "sort":
			r.Sort = p.sort(v)
		case "preferred_min_throughput":
			r.PreferredMinThroughput = p.threshold(k, MsgThroughput, v)
		case "preferred_max_latency":
			r.PreferredMaxLatency = p.threshold(k, MsgLatency, v)
		case "max_price":
			r.MaxPrice = p.maxPrice(v)
		default:
			p.unknown(k)
		}
	}
	p.list = append(p.list, r.Check()...)
	if len(p.list) > 0 {
		sortProblems(p.list)
		first := p.list[0]
		return nil, &first
	}
	if r.IsZero() {
		return nil, nil
	}
	return r, nil
}

// parser collects the problems of the kinds of a routing's members.
type parser struct{ list []Problem }

func (p *parser) add(msg string, path ...any) { p.list = append(p.list, Problem{Path: path, Msg: msg}) }

func (p *parser) unknown(path ...any) {
	p.list = append(p.list, Problem{Path: path, Msg: MsgUnknown, Unknown: true})
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

// object reads v as a JSON object, its members by name; false when it is
// not one.
func object(v json.RawMessage) (map[string]json.RawMessage, []string, bool) {
	v = bytes.TrimSpace(v)
	var m map[string]json.RawMessage
	if len(v) == 0 || v[0] != '{' || json.Unmarshal(v, &m) != nil {
		return nil, nil, false
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return m, names, true
}

// list reads v as a JSON array of strings; false when it is not one.
func list(v json.RawMessage) ([]string, bool) {
	var raw []json.RawMessage
	if json.Unmarshal(v, &raw) != nil || raw == nil {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		var s string
		if json.Unmarshal(x, &s) != nil {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func (p *parser) slugs(name string, v json.RawMessage) []string {
	l, ok := list(v)
	if !ok {
		p.add(MsgList(name), name)
		return nil
	}
	return l
}

func (p *parser) bool(name string, v json.RawMessage) *bool {
	var b bool
	if json.Unmarshal(v, &b) != nil {
		p.add(MsgBool(name), name)
		return nil
	}
	return &b
}

func (p *parser) quantizations(v json.RawMessage) []string {
	var raw []json.RawMessage
	if json.Unmarshal(v, &raw) != nil || raw == nil {
		p.add(MsgQuantizations, "quantizations")
		return nil
	}
	out := make([]string, 0, len(raw))
	for i, x := range raw {
		var s string
		if json.Unmarshal(x, &s) != nil {
			p.add(MsgQuantizations, "quantizations", i)
			continue
		}
		out = append(out, s)
	}
	if len(out) < len(raw) {
		return nil
	}
	return out
}

func (p *parser) sort(v json.RawMessage) *Sort {
	var by string
	if json.Unmarshal(v, &by) == nil {
		if by == "" {
			p.add(MsgSort, "sort")
			return nil
		}
		return &Sort{By: by}
	}
	m, names, ok := object(v)
	if !ok {
		p.add(MsgSort, "sort")
		return nil
	}
	s := &Sort{mapping: true}
	bad := false
	for _, k := range names {
		x := m[k]
		if isNull(x) && (k == "by" || k == "partition") {
			continue
		}
		var into *string
		msg := MsgSort
		switch k {
		case "by":
			into = &s.By
		case "partition":
			into, msg = &s.Partition, MsgPartition
		default:
			p.unknown("sort", k)
			bad = true
			continue
		}
		if json.Unmarshal(x, into) != nil {
			p.add(msg, "sort", k)
			bad = true
		}
	}
	if bad || s.zero() {
		return nil
	}
	return s
}

func (p *parser) threshold(name, msg string, v json.RawMessage) *Threshold {
	if n, ok := number(v); ok {
		return &Threshold{P50: &n, bare: true}
	}
	m, names, ok := object(v)
	if !ok {
		p.add(msg, name)
		return nil
	}
	t := &Threshold{}
	into := []**float64{&t.P50, &t.P75, &t.P90, &t.P99}
	for _, k := range names {
		i := slices.Index(PercentileNames, k)
		switch {
		case i < 0:
			p.unknown(name, k)
		case isNull(m[k]):
		default:
			n, ok := number(m[k])
			if !ok {
				p.add(msg, name, k)
				continue
			}
			*into[i] = &n
		}
	}
	if t.zero() {
		return nil
	}
	return t
}

// number reads v as a JSON number.
func number(v json.RawMessage) (float64, bool) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || v[0] != '-' && (v[0] < '0' || v[0] > '9') {
		return 0, false
	}
	n, err := strconv.ParseFloat(string(v), 64)
	return n, err == nil
}

func (p *parser) maxPrice(v json.RawMessage) *MaxPrice {
	m, names, ok := object(v)
	if !ok {
		p.add(MsgPrice, "max_price")
		return nil
	}
	mp := &MaxPrice{}
	for _, k := range names {
		i := slices.Index(PriceMembers, k)
		if i < 0 {
			p.unknown("max_price", k)
			continue
		}
		s, ok := decimalText(m[k])
		if !ok {
			p.add(MsgPrice, "max_price", k)
			continue
		}
		if s != nil {
			*mp.members()[i] = s
		}
	}
	if mp.zero() {
		return nil
	}
	return mp
}

// rank is where a path's step stands among its siblings: a member by
// OpenRouter's order, a list's entry by its index, a member of sort, a
// threshold or max_price by theirs; a member of no such name first.
func rank(x any, parent string) int {
	switch v := x.(type) {
	case int:
		return v
	case string:
		var order []string
		switch parent {
		case "":
			order = Members
		case "sort":
			order = []string{"by", "partition"}
		case "preferred_min_throughput", "preferred_max_latency":
			order = PercentileNames
		case "max_price":
			order = PriceMembers
		}
		return slices.Index(order, v)
	}
	return -1
}

// sortProblems puts problems in the order the API answers the first of:
// by member in OpenRouter's order, then by index or by the member's own
// order, a problem with a member before those within it.
func sortProblems(list []Problem) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].Path, list[j].Path
		for k := 0; k < len(a) && k < len(b); k++ {
			parent := ""
			if k > 0 {
				parent, _ = a[0].(string)
			}
			ra, rb := rank(a[k], parent), rank(b[k], parent)
			if ra != rb {
				return ra < rb
			}
		}
		return len(a) < len(b)
	})
}
