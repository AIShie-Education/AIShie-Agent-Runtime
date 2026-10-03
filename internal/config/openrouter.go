package config

import (
	"reflect"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
)

// OpenRouter's upstream routing in a model section (model.openrouter, and
// an offer's): its members of more than one form walked their own way,
// each problem at its line; its values checked with the model (checkModel).

var routingType = reflect.TypeFor[openrouter.Routing]()

// shapeOf is what a mapping of type t may hold beyond its fields, and how
// its members of more than one form are walked.
func (w *walker) shapeOf(t reflect.Type) shape {
	if t != routingType {
		return shape{}
	}
	return shape{special: map[string]func(*yaml.Node, string) any{
		"sort":                     w.routingSort,
		"preferred_min_throughput": w.routingThreshold(openrouter.MsgThroughput),
		"preferred_max_latency":    w.routingThreshold(openrouter.MsgLatency),
		"max_price":                w.routingMaxPrice,
	}}
}

// routingSort walks sort: a string, or a mapping of by and partition.
func (w *walker) routingSort(n *yaml.Node, path string) any {
	n = resolve(n)
	switch {
	case !w.spend(n, path) || isNull(n):
		return nil
	case n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str" && n.Value != "":
		return n.Value
	case n.Kind != yaml.MappingNode:
		w.problem(n, path, "%s", openrouter.MsgSort)
		return nil
	}
	out := map[string]any{}
	for _, kv := range w.pairs(n, path) {
		p := join(path, kv.key)
		switch kv.key {
		case "by", "partition":
			out[kv.key] = w.value(kv.value, stringType, p)
		default:
			w.problem(kv.keyNode, p, "unknown field")
		}
	}
	return out
}

// routingThreshold walks a threshold: a number, or a mapping of p50,
// p75, p90 and p99, each a number.
func (w *walker) routingThreshold(msg string) func(*yaml.Node, string) any {
	number := func(n *yaml.Node, path string) any {
		var v float64
		if n.Kind != yaml.ScalarNode || (n.ShortTag() != "!!int" && n.ShortTag() != "!!float") || n.Decode(&v) != nil {
			w.problem(n, path, "%s", msg)
			return nil
		}
		return v
	}
	return func(n *yaml.Node, path string) any {
		n = resolve(n)
		switch {
		case !w.spend(n, path) || isNull(n):
			return nil
		case n.Kind != yaml.MappingNode:
			return number(n, path)
		}
		out := map[string]any{}
		for _, kv := range w.pairs(n, path) {
			p := join(path, kv.key)
			switch v := resolve(kv.value); {
			case !slices.Contains(openrouter.PercentileNames, kv.key):
				w.problem(kv.keyNode, p, "unknown field")
			case isNull(v):
			default:
				out[kv.key] = number(v, p)
			}
		}
		return out
	}
}

// routingMaxPrice walks max_price: a mapping of prompt, completion,
// request and image, each a number or a decimal string, kept as written
// so that a decimal is never read through a float.
func (w *walker) routingMaxPrice(n *yaml.Node, path string) any {
	n = resolve(n)
	switch {
	case !w.spend(n, path) || isNull(n):
		return nil
	case n.Kind != yaml.MappingNode:
		w.problem(n, path, "%s", openrouter.MsgPrice)
		return nil
	}
	out := map[string]any{}
	for _, kv := range w.pairs(n, path) {
		p := join(path, kv.key)
		switch v := resolve(kv.value); {
		case !slices.Contains(openrouter.PriceMembers, kv.key):
			w.problem(kv.keyNode, p, "unknown field")
		case isNull(v):
		case v.Kind == yaml.ScalarNode && (v.ShortTag() == "!!int" || v.ShortTag() == "!!float" || v.ShortTag() == "!!str"):
			out[kv.key] = v.Value
		default:
			w.problem(v, p, "%s", openrouter.MsgPrice)
		}
	}
	return out
}

// checkRouting holds a model section's upstream routing to where it may
// stand, a model of OpenRouter's behind an adapter that sends it, and to
// its own rules, each problem at its member.
func checkRouting(is *issues, path string, m *Model) {
	if m.OpenRouter.Canonical() != nil {
		switch p := m.EffectiveProvider(); {
		case p != llm.ProviderOpenRouter:
			is.add(path+".openrouter", "upstream routing is OpenRouter's; this model's provider is %s", p)
		case m.Adapter != llm.AdapterOpenAIChat && m.Adapter != llm.AdapterAnthropic:
			is.add(path+".openrouter", "upstream routing is sent by the openai_chat and anthropic adapters alone")
		}
	}
	for _, p := range m.OpenRouter.Check() {
		is.add(path+".openrouter."+p.Dotted(), "%s", p.Msg)
	}
}
