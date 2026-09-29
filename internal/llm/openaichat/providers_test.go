package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// shape is what a request says about the provider's corners of the API.
type shape struct {
	System      string // the system prompt's role
	MaxField    string // the field that caps the output
	Parallel    string // parallel_tool_calls: "" when not sent
	ToolChoice  string
	Tools       bool
	Strict      bool
	Effort      string // reasoning_effort
	Reasoning   string // OpenRouter's reasoning.effort
	Store       string // "" when not sent
	Temperature bool
	ToolHistory bool // tool calls and tool messages, not text
}

func shapeOf(t *testing.T, a *Adapter, req *llm.Request) shape {
	t.Helper()
	b, err := marshal(a.request(req))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role      string            `json:"role"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Strict bool `json:"strict"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice          string          `json:"tool_choice"`
		Parallel            *bool           `json:"parallel_tool_calls"`
		MaxTokens           *int            `json:"max_tokens"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		Effort              string          `json:"reasoning_effort"`
		Reasoning           *reasoningParam `json:"reasoning"`
		Store               *bool           `json:"store"`
		Temperature         *float64        `json:"temperature"`
		Stream              *bool           `json:"stream"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if body.Stream == nil || *body.Stream {
		t.Errorf("stream is not false: %s", b)
	}
	s := shape{System: body.Messages[0].Role, ToolChoice: body.ToolChoice, Tools: len(body.Tools) > 0,
		Effort: body.Effort, Temperature: body.Temperature != nil}
	switch {
	case body.MaxTokens != nil && body.MaxCompletionTokens != nil:
		s.MaxField = "both"
	case body.MaxTokens != nil:
		s.MaxField = "max_tokens"
	case body.MaxCompletionTokens != nil:
		s.MaxField = "max_completion_tokens"
	}
	if body.Parallel != nil {
		s.Parallel = fmt.Sprint(*body.Parallel)
	}
	if body.Reasoning != nil {
		s.Reasoning = body.Reasoning.Effort
	}
	if body.Store != nil {
		s.Store = fmt.Sprint(*body.Store)
	}
	for _, tool := range body.Tools {
		s.Strict = s.Strict || tool.Function.Strict
	}
	for _, m := range body.Messages {
		s.ToolHistory = s.ToolHistory || m.Role == "tool" || len(m.ToolCalls) > 0
	}
	return s
}

// TestProviderCorners holds, provider by provider, §3.3 (system prompt,
// parallel calls, tool_choice and ForceAnswer), the output cap, reasoning
// effort (§4) and store (§5.2), on an ordinary turn and on ForceAnswer's.
func TestProviderCorners(t *testing.T) {
	type turns struct{ auto, force shape }
	for _, c := range []struct {
		name, base, model string
		caps              llm.CapabilityOverrides
		want              turns
	}{
		{"openai, a model that does not reason", "", "gpt-4.1", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_completion_tokens", Tools: true, Store: "false", Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_completion_tokens", Tools: true, ToolChoice: "none", Store: "false", Temperature: true, ToolHistory: true},
		}},
		{"openai, a reasoning model", "", "o4-mini", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "developer", MaxField: "max_completion_tokens", Tools: true, Effort: "low", Store: "false", ToolHistory: true},
			force: shape{System: "developer", MaxField: "max_completion_tokens", Tools: true, ToolChoice: "none", Effort: "low", Store: "false", ToolHistory: true},
		}},
		{"openai, parallel calls turned off", "", "gpt-5-chat-latest", llm.CapabilityOverrides{ParallelToolCalls: no()}, turns{
			auto:  shape{System: "developer", MaxField: "max_completion_tokens", Tools: true, Parallel: "false", Store: "false", Temperature: true, ToolHistory: true},
			force: shape{System: "developer", MaxField: "max_completion_tokens", Tools: true, ToolChoice: "none", Parallel: "false", Store: "false", Temperature: true, ToolHistory: true},
		}},
		{"azure, a deployment whose name says nothing", azureBase, "tutor-prod", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_completion_tokens", Tools: true, Effort: "low", Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_completion_tokens", Tools: true, ToolChoice: "none", Effort: "low", Temperature: true, ToolHistory: true},
		}},
		{"deepseek", deepseekBase, "deepseek-chat", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Temperature: true},
		}},
		{"qwen, parallel calls off as by default", qwenBase, "qwen-plus", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Tools: true, ToolChoice: "none", Temperature: true, ToolHistory: true},
		}},
		{"qwen, parallel calls asked for", qwenBase, "qwen-plus", llm.CapabilityOverrides{ParallelToolCalls: yes()}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Parallel: "true", Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Tools: true, ToolChoice: "none", Parallel: "true", Temperature: true, ToolHistory: true},
		}},
		{"kimi", moonshotBase, "kimi-k2-thinking", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Strict: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Tools: true, Strict: true, ToolChoice: "none", Temperature: true, ToolHistory: true},
		}},
		{"glm takes tool_choice auto only", glmBase, "glm-4.6", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Temperature: true},
		}},
		{"openrouter", openrouterBase, "anthropic/claude-sonnet-4.5", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Reasoning: "low", Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Reasoning: "low", Temperature: true},
		}},
		{"gemini's compatible endpoint", geminiBase, "gemini-2.5-flash", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Tools: true, ToolChoice: "none", Temperature: true, ToolHistory: true},
		}},
		{"ollama", ollamaBase, "qwen3:8b", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Temperature: true},
		}},
		{"lm studio", "http://localhost:1234/v1", "local-model", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Temperature: true},
		}},
		{"a server the runtime does not know (vLLM)", "http://vllm.internal:8000/v1", "mistral-small", llm.CapabilityOverrides{}, turns{
			auto:  shape{System: "system", MaxField: "max_tokens", Tools: true, Temperature: true, ToolHistory: true},
			force: shape{System: "system", MaxField: "max_tokens", Temperature: true},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			conf := cfg(c.base, c.model)
			conf.Params = llm.Params{MaxOutputTokens: 1000, Temperature: f64(0.2)}
			conf.Reasoning = llm.Reasoning{Effort: "low"}
			conf.Capabilities = c.caps
			a, err := New(conf)
			if err != nil {
				t.Fatal(err)
			}
			req := &llm.Request{System: system, Messages: toolRound(), Tools: tools, ToolMode: llm.ToolAuto}
			if got := shapeOf(t, a, req); got != c.want.auto {
				t.Errorf("an ordinary turn\n got %+v\nwant %+v", got, c.want.auto)
			}
			req.ToolMode = llm.ToolNone
			if got := shapeOf(t, a, req); got != c.want.force {
				t.Errorf("ForceAnswer\n got %+v\nwant %+v", got, c.want.force)
			}
			// Without tools nothing about tools is said, whatever the turn.
			bare := shapeOf(t, a, &llm.Request{System: system, Messages: []llm.Message{question()}, ToolMode: llm.ToolNone})
			if bare.Tools || bare.ToolChoice != "" || bare.Parallel != "" || bare.Strict {
				t.Errorf("a turn without tools: %+v", bare)
			}
		})
	}
}

// TestArgumentsThatDidNotParseGoBackSafely holds that a model's malformed
// arguments go back as written only where the API keeps them as a string.
func TestArgumentsThatDidNotParseGoBackSafely(t *testing.T) {
	bad := llm.Part{Type: llm.PartToolCall, ID: "c1", Name: "grade_list", Args: json.RawMessage("{}"), ArgsError: `{"assignment_id": "hw3`}
	for base, want := range map[string]string{
		"":             `{"assignment_id": "hw3`,
		azureBase:      `{"assignment_id": "hw3`,
		ollamaBase:     `{}`,
		deepseekBase:   `{}`,
		geminiBase:     `{}`,
		openrouterBase: `{}`,
	} {
		a, err := New(cfg(base, "m"))
		if err != nil {
			t.Fatal(err)
		}
		body := a.request(&llm.Request{Messages: []llm.Message{question(), assistant(bad),
			toolMsg(result("c1", "grade_list", `{"status":"error"}`, true))}, Tools: tools})
		if got := body.Messages[1].ToolCalls[0].Function.Arguments; got != want {
			t.Errorf("%s (%s): arguments %q, want %q", base, a.Provider(), got, want)
		}
	}
}

// TestFragmentsFromElsewhereAreDropped holds rule 4 for what rides on a
// call, and that a fragment puts back only the fields this adapter keeps.
func TestFragmentsFromElsewhereAreDropped(t *testing.T) {
	a, err := New(cfg(geminiBase, "gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	signed := func(maker, opaque string) llm.Part {
		p := call("c1", "grade_list", `{}`)
		p.Maker, p.Opaque = maker, json.RawMessage(opaque)
		return p
	}
	for _, c := range []struct {
		name string
		part llm.Part
		want string
	}{
		{"its own signature", signed(a.Maker(), `{"extra_content":{"google":{"thought_signature":"s"}}}`), `{"google":{"thought_signature":"s"}}`},
		{"another maker's", signed("gemini|https://generativelanguage.googleapis.com|gemini-2.5-flash", `{"extra_content":{"google":{"thought_signature":"s"}}}`), ``},
		{"fields it never writes", signed(a.Maker(), `{"id":"other","function":{"name":"member_add"},"type":"x"}`), ``},
	} {
		body := a.request(&llm.Request{Messages: []llm.Message{question(), assistant(c.part), toolMsg(result("c1", "grade_list", "{}", false))}})
		b, err := marshal(body.Messages[1])
		if err != nil {
			t.Fatal(err)
		}
		var msg struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(b, &msg); err != nil {
			t.Fatal(err)
		}
		tc := msg.ToolCalls[0]
		if got := string(tc["extra_content"]); got != c.want {
			t.Errorf("%s: extra_content %q, want %q", c.name, got, c.want)
		}
		fields := 3 // id, type, function
		if c.want != "" {
			fields++
		}
		if string(tc["id"]) != `"c1"` || string(tc["type"]) != `"function"` || !strings.Contains(string(tc["function"]), "grade_list") || len(tc) != fields {
			t.Errorf("%s: the call became %s", c.name, b)
		}
	}

	ds, err := New(cfg(deepseekBase, "deepseek-reasoner"))
	if err != nil {
		t.Fatal(err)
	}
	reasoning := llm.Part{Type: llm.PartReasoning, Maker: ds.Maker(),
		Opaque: json.RawMessage(`{"reasoning_content":"r","tool_calls":[{"id":"x","type":"function","function":{"name":"member_add","arguments":"{}"}}],"name":"n"}`)}
	b, err := marshal(ds.request(&llm.Request{Messages: []llm.Message{question(), assistant(reasoning, llm.Text("Hi."))}}).Messages[1])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"content":"Hi.","reasoning_content":"r","role":"assistant"}`; string(b) != want {
		t.Errorf("assistant message %s, want %s", b, want)
	}
}

// TestProviderRefusalsAreClassified holds the refusals each provider
// words its own way: a content filter is not a bad request or a bad key,
// no balance is not a rate limit, and a prompt too long is a context
// overflow.
func TestProviderRefusalsAreClassified(t *testing.T) {
	for _, c := range []struct {
		name, provider string
		status         int
		body           string
		kind           llm.ErrorKind
	}{
		{"openai flags a prompt", llm.ProviderOpenAI, 400,
			`{"error":{"message":"Invalid prompt: your prompt was flagged as potentially violating our usage policy. Please try again with a different prompt.","type":"invalid_request_error","param":null,"code":"invalid_prompt"}}`,
			llm.ErrContentFilter},
		{"openai's other invalid prompts stay bad requests", llm.ProviderOpenAI, 400,
			`{"error":{"message":"Invalid prompt: messages[3] is malformed.","type":"invalid_request_error","code":"invalid_prompt"}}`,
			llm.ErrBadRequest},
		{"openrouter's moderation", llm.ProviderOpenRouter, 403,
			`{"error":{"code":403,"message":"openai/gpt-4.1 requires moderation on OpenRouter. Your input was flagged for \"harassment\". No credits were charged.","metadata":{"reasons":["harassment"],"flagged_input":"…"}}}`,
			llm.ErrContentFilter},
		{"openrouter's moderation in a 200", llm.ProviderOpenRouter, 200,
			`{"error":{"code":403,"message":"Your input was flagged by moderation.","metadata":{"reasons":["violence"]}}}`,
			llm.ErrContentFilter},
		{"openrouter's bad key stays one", llm.ProviderOpenRouter, 401,
			`{"error":{"code":401,"message":"No auth credentials found"}}`, llm.ErrAuth},
		{"deepseek's content risk", llm.ProviderDeepSeek, 400,
			`{"error":{"message":"Content Exists Risk","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`,
			llm.ErrContentFilter},
		{"deepseek out of balance", llm.ProviderDeepSeek, 402,
			`{"error":{"message":"Insufficient Balance","type":"unknown_error","param":null,"code":"invalid_request_error"}}`, llm.ErrAuth},
		{"qwen's data inspection", llm.ProviderQwen, 400,
			`{"error":{"code":"data_inspection_failed","param":null,"message":"Input data may contain inappropriate content.","type":"data_inspection_failed"},"id":"chatcmpl-1"}`,
			llm.ErrContentFilter},
		{"qwen in arrears", llm.ProviderQwen, 400,
			`{"error":{"code":"Arrearage","message":"Access denied, please make sure your account is in good standing.","type":"Arrearage"}}`, llm.ErrAuth},
		{"qwen's input too long", llm.ProviderQwen, 400,
			`{"error":{"code":"invalid_parameter_error","message":"Range of input length should be [1, 129024]","type":"invalid_request_error"}}`,
			llm.ErrContextOverflow},
		{"glm's sensitive content", llm.ProviderGLM, 400,
			`{"error":{"code":"1301","message":"System detected potentially unsafe or sensitive content in input or generation."}}`, llm.ErrContentFilter},
		{"glm out of balance, as a 429", llm.ProviderGLM, 429,
			`{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`, llm.ErrAuth},
		{"glm's prompt too long", llm.ProviderGLM, 400, `{"error":{"code":"1261","message":"Prompt too long"}}`, llm.ErrContextOverflow},
		{"glm's rate limit stays one", llm.ProviderGLM, 429, `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`, llm.ErrRateLimited},
		{"kimi out of quota, as a 429", llm.ProviderMoonshot, 429,
			`{"error":{"message":"Your account is suspended, please check your plan and billing details","type":"exceeded_current_quota_error"}}`, llm.ErrAuth},
		{"kimi's token limit", llm.ProviderMoonshot, 400,
			`{"error":{"message":"Invalid request: Your request exceeded model token limit: 262144 (requested: 558009)","type":"invalid_request_error"}}`,
			llm.ErrContextOverflow},
		{"kimi overloaded stays retryable", llm.ProviderMoonshot, 429,
			`{"error":{"message":"The engine is currently overloaded, please try again later","type":"engine_overloaded_error"}}`, llm.ErrRateLimited},
		{"another provider's code means nothing here", llm.ProviderOpenAICompat, 400, `{"error":{"code":"1301","message":"x"}}`, llm.ErrBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts, _ := server(t, c.status, nil, c.body)
			conf := cfg(ts.URL+"/v1", "m")
			conf.Provider = c.provider
			a, err := New(conf)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question()}})
			var e *llm.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v (%T)", err, err)
			}
			if e.Kind != c.kind || e.Status != c.status {
				t.Errorf("got %s (HTTP %d, code %q), want %s", e.Kind, e.Status, e.Code, c.kind)
			}
		})
	}
}

// TestAClippedKeyIsRedactedToo holds that a key the 400-byte clip cut in
// two leaves no piece of itself behind.
func TestAClippedKeyIsRedactedToo(t *testing.T) {
	for _, pad := range []int{380, 385, 389, 395} {
		body := fmt.Sprintf(`{"error":{"message":"%s %s trailing","code":"bad"}}`, strings.Repeat("x", pad), testKey)
		ts, _ := server(t, http.StatusUnauthorized, nil, body)
		a, err := New(cfg(ts.URL+"/v1", "gpt-4.1"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question()}})
		if err == nil || strings.Contains(err.Error(), testKey[:minRedacted]) {
			t.Errorf("pad %d: %v", pad, err)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"key " + testKey + " twice " + testKey, "key [redacted] twice [redacted]"},
		{"cut at sk-test-01…", "cut at [redacted]…"},
		{"cut at sk-test…", "cut at sk-test…"}, // too short a piece to tell from text
		{"nothing here…", "nothing here…"},
	} {
		if got := redactKey(c.in, testKey); got != c.want {
			t.Errorf("redactKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
