package openaichat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
)

// routingJSON is the contract's example of upstream routing, as written;
// routingWire is it on the wire, canonical, byte for byte.
const (
	routingJSON = `{"data_collection":"deny","require_parameters":true,"allow_fallbacks":false,"only":["groq","together","deepinfra"],` +
		`"order":["deepinfra/turbo","groq"],"quantizations":["fp8","bf16","unknown"],"preferred_max_latency":{"p90":3},` +
		`"max_price":{"prompt":1,"completion":"2.50"}}`
	routingWire = `"provider":{"order":["deepinfra/turbo","groq"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny",` +
		`"only":["groq","together","deepinfra"],"quantizations":["fp8","bf16","unknown"],"preferred_max_latency":{"p90":3},` +
		`"max_price":{"prompt":"1","completion":"2.5"}}`
)

func testRouting() *openrouter.Routing {
	r, p := openrouter.Parse(json.RawMessage(routingJSON))
	if p != nil {
		panic(p.Error())
	}
	return r
}

// The routing goes to OpenRouter as provider, at the top of the body, its
// members in OpenRouter's order and max_price's as strings, on a call
// whole, a stream and ForceAnswer's alike; with none, and to any other
// provider, even one handed a routing, no provider is sent.
func TestRoutingOnTheWire(t *testing.T) {
	send := func(c llm.Config, req *llm.Request, stream bool) string {
		t.Helper()
		rt := &replay{status: http.StatusOK, body: textReply("stop", "Hi."), header: http.Header{}}
		c.HTTPClient = &http.Client{Transport: rt}
		if stream {
			c.HTTPClient = &http.Client{Transport: eventStream{rt}}
			rt.body = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		}
		a, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		if stream {
			_, err = a.Stream(context.Background(), req, func(string) {})
		} else {
			_, err = a.Call(context.Background(), req)
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(rt.sent)
	}
	routed := cfg(openrouterBase, "meta-llama/llama-3.3-70b-instruct")
	routed.OpenRouter = testRouting()
	simple := &llm.Request{System: system, Messages: []llm.Message{question()}, ToolMode: llm.ToolAuto}
	force := &llm.Request{System: system, Messages: toolRound(), Tools: tools, ToolMode: llm.ToolNone}
	for name, sent := range map[string]string{
		"whole":       send(routed, simple, false),
		"a stream":    send(routed, simple, true),
		"ForceAnswer": send(routed, force, false),
		"named so": send(func() llm.Config {
			c := cfg("https://gateway.example.edu/v1", "m")
			c.Provider = "openrouter"
			c.OpenRouter = testRouting()
			return c
		}(), simple, false),
		"as written": send(func() llm.Config { c := routed; c.OpenRouter = testRouting().Clone(); return c }(), simple, false),
	} {
		if !strings.Contains(sent, routingWire) {
			t.Errorf("%s: no provider as the wire has it:\n%s", name, sent)
		}
	}
	for name, c := range map[string]llm.Config{
		"none":     cfg(openrouterBase, "meta-llama/llama-3.3-70b-instruct"),
		"empty":    func() llm.Config { c := routed; c.OpenRouter = &openrouter.Routing{Only: []string{}}; return c }(),
		"DeepSeek": func() llm.Config { c := cfg(deepseekBase, "deepseek-chat"); c.OpenRouter = testRouting(); return c }(),
		"OpenAI":   func() llm.Config { c := cfg("", "gpt-4.1"); c.OpenRouter = testRouting(); return c }(),
	} {
		for _, stream := range []bool{false, true} {
			if sent := send(c, simple, stream); strings.Contains(sent, `"provider"`) {
				t.Errorf("%s (stream %v): provider sent:\n%s", name, stream, sent)
			}
		}
	}
}
