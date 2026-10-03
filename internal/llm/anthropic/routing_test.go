package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
)

// routing is the contract's example of upstream routing.
func routing(t *testing.T) *openrouter.Routing {
	t.Helper()
	r, p := openrouter.Parse(json.RawMessage(`{"data_collection":"deny","require_parameters":true,"allow_fallbacks":false,` +
		`"only":["anthropic","amazon-bedrock"],"order":["amazon-bedrock"],"max_price":{"prompt":1,"completion":"5.00"}}`))
	if p != nil {
		t.Fatal(p)
	}
	return r
}

// Behind OpenRouter's Messages API the routing goes as provider, at the
// top of the body, canonical, whole and streamed, to OpenRouter's
// /messages; to Anthropic's own API, or another server of its format,
// never, whatever the adapter is handed.
func TestRoutingOnTheWire(t *testing.T) {
	const wire = `"provider":{"order":["amazon-bedrock"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny",` +
		`"only":["anthropic","amazon-bedrock"],"max_price":{"prompt":"1","completion":"5"}}`
	a := newAdapter(t, llm.Config{BaseURL: "https://openrouter.ai/api/v1", Model: "anthropic/claude-haiku-4.5", OpenRouter: routing(t)})
	if a.Endpoint() != "https://openrouter.ai/api/v1/messages" || a.Provider() != llm.ProviderOpenRouter {
		t.Fatalf("%s, %s", a.Endpoint(), a.Provider())
	}
	req := llm.Request{System: "You are CS101's tutor.", Messages: []llm.Message{question}, Tools: []llm.Tool{gradeList}, ToolMode: llm.ToolAuto}
	body, err := a.encodeRequest(&req, false)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "request_openrouter_routing.anthropic.json", body)
	for name, stream := range map[string]bool{"whole": false, "streamed": true} {
		b, err := a.encodeRequest(&req, stream)
		if err != nil || !bytes.Contains(b, []byte(wire)) {
			t.Errorf("%s: no provider as the wire has it: %v\n%s", name, err, b)
		}
	}
	force := req
	force.ToolMode = llm.ToolNone
	if b, err := a.encodeRequest(&force, false); err != nil || !bytes.Contains(b, []byte(wire)) {
		t.Errorf("ForceAnswer: %v\n%s", err, b)
	}
	for name, cfg := range map[string]llm.Config{
		"Anthropic's own":   {OpenRouter: routing(t)},
		"Anthropic's named": {BaseURL: "https://api.anthropic.com", OpenRouter: routing(t)},
		"DeepSeek's":        {BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-chat", OpenRouter: routing(t)},
		"OpenRouter, none":  {BaseURL: "https://openrouter.ai/api/v1", Model: "anthropic/claude-haiku-4.5"},
	} {
		a := newAdapter(t, cfg)
		for _, stream := range []bool{false, true} {
			if b, err := a.encodeRequest(&req, stream); err != nil || bytes.Contains(b, []byte(`"provider"`)) {
				t.Errorf("%s: %v\n%s", name, err, b)
			}
		}
	}
}
