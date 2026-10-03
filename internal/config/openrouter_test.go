package config

import (
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
)

// orAgent is an agent of its owner's at OpenRouter, with upstream routing,
// and a fallback at OpenRouter too.
const orAgent = `
agent:
  id: a1
  display_name: A1
  core: {base_url: "https://lms.example.edu", agent_id: "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0aa1"}
  model:
    adapter: openai_chat
    base_url: https://openrouter.ai/api/v1
    model: meta-llama/llama-3.3-70b-instruct
    key_ref: env://OPENROUTER_KEY
    openrouter:
      data_collection: deny
      only: [groq, together, deepinfra]
      sort: price
      preferred_max_latency: 3
      preferred_min_throughput: {p90: 30}
      max_price: {prompt: 1.50, completion: "2.000000"}
    fallback:
      adapter: anthropic
      base_url: https://openrouter.ai/api/v1
      model: anthropic/claude-haiku-4.5
      key_ref: env://OPENROUTER_KEY
courses:
  0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b:
    model:
      openrouter:
        only: [deepinfra/turbo]
        zdr: true
        sort: {by: throughput, partition: none}
  0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1c:
    model:
      fallback:
        openrouter: {ignore: [novita]}
`

// Upstream routing loads on an agent's model, its union members in every
// form; the fallback has its own, none inherited from the model; and a
// course's is merged member by member over the agent's.
func TestRoutingLoads(t *testing.T) {
	dir := write(t, map[string]string{"agent.yaml": orAgent})
	cfg, err := Load(dir + "/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents[0]
	const want = `{"data_collection":"deny","only":["groq","together","deepinfra"],"sort":"price","preferred_min_throughput":{"p90":30},` +
		`"preferred_max_latency":3,"max_price":{"prompt":"1.5","completion":"2"}}`
	if got := string(a.Model.OpenRouter.JSON()); got != want {
		t.Errorf("the model's:\n%s\nwant\n%s", got, want)
	}
	if fb := a.Model.Fallback; fb == nil || fb.OpenRouter != nil {
		t.Errorf("the fallback inherited the model's routing: %+v", fb)
	}
	e, err := a.ForCourse(tutorCourse)
	if err != nil {
		t.Fatal(err)
	}
	const merged = `{"data_collection":"deny","zdr":true,"only":["deepinfra/turbo"],"sort":{"by":"throughput","partition":"none"},` +
		`"preferred_min_throughput":{"p90":30},"preferred_max_latency":3,"max_price":{"prompt":"1.5","completion":"2"}}`
	if got := string(e.Model.OpenRouter.JSON()); got != merged {
		t.Errorf("the course's:\n%s\nwant\n%s", got, merged)
	}
	e, err = a.ForCourse(disabledCourse)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(e.Model.Fallback.OpenRouter.JSON()); got != `{"ignore":["novita"]}` || string(e.Model.OpenRouter.JSON()) != want {
		t.Errorf("the course's fallback: %s", got)
	}
}

// An offer of runtime.yaml's at OpenRouter takes routing, and its model
// section (Section, AsModel) carries it, canonical; an agent on the
// offer must have it exactly.
func TestRoutingOnAnOffer(t *testing.T) {
	rt := schoolRuntime()
	rt["allowed_models"] = []any{"*:openrouter:*"}
	rt["school"].(map[string]any)["offers"] = []any{map[string]any{
		"id": "llama", "label": "Llama", "adapter": "openai_chat", "base_url": "https://openrouter.ai/api/v1", "model": "meta-llama/llama-3.3-70b-instruct",
		"key_ref":    "secret://school/keys/openrouter",
		"openrouter": map[string]any{"data_collection": "deny", "sort": map[string]any{"by": "price"}, "max_price": map[string]any{"prompt": "0.80"}},
	}}
	agent := planAgent()
	agent["model"] = map[string]any{"adapter": "openai_chat", "base_url": "https://openrouter.ai/api/v1", "model": "meta-llama/llama-3.3-70b-instruct",
		"key_ref": "secret://school/keys/openrouter", "key_source": "school", "offer": "llama",
		"openrouter": map[string]any{"data_collection": "deny", "sort": "price", "max_price": map[string]any{"prompt": 0.8}}}
	dir := write(t, map[string]string{
		"agent.yaml":   yamlOf(t, map[string]any{"agent": agent}),
		"runtime.yaml": yamlOf(t, map[string]any{"runtime": rt}),
	})
	cfg, err := Load(dir+"/agent.yaml", dir+"/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"data_collection":"deny","sort":"price","max_price":{"prompt":"0.8"}}`
	o := cfg.Runtime.School.Offers[0]
	if got := string(o.OpenRouter.JSON()); got != want {
		t.Fatalf("the offer's: %s", got)
	}
	if m := o.AsModel(); string(m.OpenRouter.JSON()) != want || m.OpenRouter == o.OpenRouter {
		t.Errorf("AsModel: %s", m.OpenRouter.JSON())
	}
	if got := yamlOf(t, o.Section()["openrouter"]); got != "data_collection: deny\nmax_price:\n    prompt: \"0.8\"\nsort: price\n" {
		t.Errorf("Section:\n%s", got)
	}
	if _, ok := (SchoolOffer{ID: "x"}).Section()["openrouter"]; ok {
		t.Error("Section writes routing an offer has none of")
	}

	// An agent or a course on the offer cannot loosen it.
	agent["model"].(map[string]any)["openrouter"] = map[string]any{"data_collection": "allow", "sort": "price", "max_price": map[string]any{"prompt": 0.8}}
	dir = write(t, map[string]string{
		"agent.yaml":   yamlOf(t, map[string]any{"agent": agent}),
		"runtime.yaml": yamlOf(t, map[string]any{"runtime": rt}),
	})
	_, err = Load(dir+"/agent.yaml", dir+"/runtime.yaml")
	expectProblems(t, err, []problem{{agent: "a1", path: "agent.model", msg: "is not runtime.school's offer llama"}})
}

// Routing is refused where it may not stand: in runtime.defaults, which
// reach every model; on a model that is not OpenRouter's, or behind an
// adapter that does not send it; and at each member that breaks its rules,
// a member of the wrong kind at its line.
func TestRoutingRefused(t *testing.T) {
	model := func(adapter, extra string) string {
		return `
agent:
  id: a1
  display_name: A1
  core: {base_url: "https://lms.example.edu", agent_id: "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0aa1"}
  model:
    adapter: ` + adapter + `
    model: m
    key_ref: env://KEY
` + extra
	}
	or := "    base_url: https://openrouter.ai/api/v1\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []problem
	}{
		{"in runtime.defaults", map[string]string{"runtime.yaml": `
runtime:
  defaults:
    model:
      openrouter: {data_collection: deny}
      fallback:
        openrouter:
          zdr: true
`, "agent.yaml": model("openai_chat", or)}, []problem{
			{file: "runtime.yaml", line: 5, path: "runtime.defaults.model.openrouter",
				msg: "set upstream routing on the model or the offer that calls OpenRouter: the defaults reach every model"},
			{file: "runtime.yaml", line: 8, path: "runtime.defaults.model.fallback.openrouter", msg: "the defaults reach every model"},
		}},
		{"another provider's model", map[string]string{"agent.yaml": model("anthropic", "    openrouter: {zdr: true}\n")}, []problem{
			{agent: "a1", path: "agent.model.openrouter", msg: "upstream routing is OpenRouter's; this model's provider is anthropic"},
		}},
		{"a provider named", map[string]string{"agent.yaml": model("openai_chat", or+"    provider: deepseek\n    openrouter: {zdr: true}\n")}, []problem{
			{agent: "a1", path: "agent.model.openrouter", msg: "this model's provider is deepseek"},
		}},
		{"openai_responses", map[string]string{"agent.yaml": model("openai_responses", or+"    openrouter: {zdr: true}\n")}, []problem{
			{agent: "a1", path: "agent.model.openrouter", msg: "upstream routing is sent by the openai_chat and anthropic adapters alone"},
		}},
		{"gemini", map[string]string{"agent.yaml": model("gemini", "    provider: openrouter\n    openrouter: {zdr: true}\n")}, []problem{
			{agent: "a1", path: "agent.model.openrouter", msg: "the openai_chat and anthropic adapters alone"},
		}},
		{"bedrock_converse", map[string]string{"agent.yaml": model("bedrock_converse", "    region: us-east-1\n    provider: openrouter\n    openrouter: {zdr: true}\n")},
			[]problem{{agent: "a1", path: "agent.model.openrouter", msg: "the openai_chat and anthropic adapters alone"}}},
		{"a fallback's", map[string]string{"agent.yaml": model("openai_chat", or+"    fallback:\n      adapter: anthropic\n      model: claude-haiku-4-5\n      key_ref: env://K\n      openrouter: {zdr: true}\n")},
			[]problem{{agent: "a1", path: "agent.model.fallback.openrouter", msg: "this model's provider is anthropic"}}},
		{"its rules", map[string]string{"agent.yaml": model("openai_chat", or+`    openrouter:
      order: [groq, together]
      only: [groq, Deepinfra, groq]
      sort: latency
      data_collection: maybe
      quantizations: [fp8, fp9]
      preferred_max_latency: {p50: 0}
      max_price: {prompt: "-1", image: 1e-3}
`)}, []problem{
			{agent: "a1", path: "agent.model.openrouter.order[1]", msg: "tried first, but not among only"},
			{agent: "a1", path: "agent.model.openrouter.only[1]", msg: openrouter.MsgSlug},
			{agent: "a1", path: "agent.model.openrouter.only[2]", msg: "this slug is in the list already"},
			{agent: "a1", path: "agent.model.openrouter.sort", msg: "sort is not used while order is set"},
			{agent: "a1", path: "agent.model.openrouter.data_collection", msg: "data_collection is allow or deny"},
			{agent: "a1", path: "agent.model.openrouter.quantizations[1]", msg: "quantizations are int4"},
			{agent: "a1", path: "agent.model.openrouter.preferred_max_latency.p50", msg: "preferred_max_latency is seconds"},
			{agent: "a1", path: "agent.model.openrouter.max_price.prompt", msg: openrouter.MsgPrice},
			{agent: "a1", path: "agent.model.openrouter.max_price.image", msg: openrouter.MsgPrice},
		}},
		{"members of the wrong kind, at their lines", map[string]string{"agent.yaml": model("openai_chat", or+`    openrouter:
      order: groq
      zdr: maybe
      sort: [price]
      preferred_min_throughput: fast
      preferred_max_latency: {p50: soon, p95: 3}
      max_price: {prompt: [1], tokens: 2}
      providers: [groq]
`)}, []problem{
			{line: 12, agent: "a1", path: "agent.model.openrouter.order", msg: "must be a list"},
			{line: 13, agent: "a1", path: "agent.model.openrouter.zdr", msg: "must be true or false"},
			{line: 14, agent: "a1", path: "agent.model.openrouter.sort", msg: openrouter.MsgSort},
			{line: 15, agent: "a1", path: "agent.model.openrouter.preferred_min_throughput", msg: openrouter.MsgThroughput},
			{line: 16, agent: "a1", path: "agent.model.openrouter.preferred_max_latency.p50", msg: openrouter.MsgLatency},
			{line: 16, agent: "a1", path: "agent.model.openrouter.preferred_max_latency.p95", msg: "unknown field"},
			{line: 17, agent: "a1", path: "agent.model.openrouter.max_price.prompt", msg: openrouter.MsgPrice},
			{line: 17, agent: "a1", path: "agent.model.openrouter.max_price.tokens", msg: "unknown field"},
			{line: 18, agent: "a1", path: "agent.model.openrouter.providers", msg: "unknown field"},
		}},
		{"an offer's", map[string]string{"runtime.yaml": `
runtime:
  school:
    offers:
      - {id: llama, label: Llama, adapter: openai_chat, base_url: "https://openrouter.ai/api/v1", model: m, key_ref: "secret://school/keys/or",
         openrouter: {ignore: [groq], only: [groq], max_price: {prompt: "1.0000001"}}}
      - {id: haiku, label: Haiku, adapter: anthropic, model: claude-haiku-4-5, key_ref: "secret://school/keys/a", openrouter: {zdr: false}}
`, "agent.yaml": model("openai_chat", or)}, []problem{
			{path: "runtime.school.offers[0].openrouter.ignore[0]", msg: "this slug skips an upstream provider that order or only names"},
			{path: "runtime.school.offers[0].openrouter.max_price.prompt", msg: openrouter.MsgPrice},
			{path: "runtime.school.offers[1].openrouter", msg: "this model's provider is anthropic"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := write(t, tc.files)
			paths := []string{}
			for name := range tc.files {
				paths = append(paths, dir+"/"+name)
			}
			_, err := Load(paths...)
			expectProblems(t, err, tc.want)
		})
	}

	// Empty is not set, and never refused, even where routing may not
	// stand.
	dir := write(t, map[string]string{
		"runtime.yaml": "runtime:\n  defaults:\n    model:\n      openrouter: {}\n",
		"agent.yaml":   model("anthropic", "    openrouter: {only: [], sort: null, max_price: {}}\n"),
	})
	cfg, err := Load(dir+"/runtime.yaml", dir+"/agent.yaml")
	if err != nil {
		t.Fatalf("empty routing refused: %v", err)
	}
	if cfg.Agents[0].Model.OpenRouter.Canonical() != nil {
		t.Errorf("empty routing kept: %s", cfg.Agents[0].Model.OpenRouter.JSON())
	}
}
