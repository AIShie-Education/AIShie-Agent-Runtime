package registry

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// Every endpoint an offer makes is an official one for each adapter it
// offers, over https: fixed ones, each choice, an Azure resource, and
// Bedrock in each suggested region; no self-hosted provider is offered.
func TestOffersAreOfficial(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range Offers() {
		if seen[o.Provider] {
			t.Errorf("%s offered twice", o.Provider)
		}
		seen[o.Provider] = true
		if o.Label == "" || len(o.Adapters) == 0 {
			t.Errorf("%s: %+v", o.Provider, o)
		}
		for _, p := range selfHosted {
			if o.Provider == p {
				t.Errorf("a self-hosted provider is offered: %s", p)
			}
		}
		var urls []string
		switch e := o.Endpoint; e.Kind {
		case EndpointFixed:
			urls = append(urls, e.BaseURL)
			if e.Stored != "" {
				urls = append(urls, e.Stored)
			}
		case EndpointChoice:
			for _, c := range e.Choices {
				urls = append(urls, c.BaseURL)
			}
		case EndpointAzureResource:
			base, _, err := o.ModelBase("", e.Example, "")
			if err != nil {
				t.Fatal(err)
			}
			urls = append(urls, base)
		case EndpointBedrockRegion:
			for _, r := range e.Suggested {
				if !RegionRe.MatchString(r) {
					t.Errorf("a suggested region not of the pattern: %s", r)
				}
				urls = append(urls, "https://bedrock-runtime."+r+".amazonaws.com")
			}
		default:
			t.Errorf("%s: endpoint kind %q", o.Provider, e.Kind)
		}
		for _, raw := range urls {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" {
				t.Errorf("%s: %q", o.Provider, raw)
				continue
			}
			for _, ad := range o.Adapters {
				if !Official(ad, u) {
					t.Errorf("%s: %s is not official for %s", o.Provider, raw, ad)
				}
				if p := llm.DetectProvider(ad, raw); o.Endpoint.Stored != "" && p != o.Provider {
					t.Errorf("%s: %s is detected as %s", o.Provider, raw, p)
				}
			}
		}
	}
	for _, p := range []string{llm.ProviderOpenAI, llm.ProviderAzure, llm.ProviderAnthropic, llm.ProviderGemini, llm.ProviderBedrock,
		llm.ProviderDeepSeek, llm.ProviderQwen, llm.ProviderMoonshot, llm.ProviderGLM, llm.ProviderOpenRouter} {
		if !seen[p] {
			t.Errorf("%s is not offered", p)
		}
	}
	// A copy is the caller's.
	a := Offers()
	a[0].Adapters[0] = "changed"
	if Offers()[0].Adapters[0] == "changed" {
		t.Error("Offers gave the table itself")
	}
}

// ModelBase makes the base_url and region from the owner's choice, and
// refuses what the offer does not have; ChoiceOf reads a base_url back.
func TestModelBase(t *testing.T) {
	for _, tc := range []struct {
		provider, endpoint, resource, region string
		base, modelRegion                    string
		err                                  error
	}{
		{provider: "openai", base: ""},
		{provider: "deepseek", base: "https://api.deepseek.com"},
		{provider: "qwen", base: "https://dashscope-us.aliyuncs.com/compatible-mode/v1"},
		{provider: "qwen", endpoint: "dashscope-intl", base: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		{provider: "moonshot", endpoint: "china", base: "https://api.moonshot.cn/v1"},
		{provider: "glm", endpoint: "china", err: ErrUnknownEndpoint},
		{provider: "qwen", endpoint: "evil.example.com", err: ErrUnknownEndpoint},
		{provider: "openai", endpoint: "global", err: ErrUnknownEndpoint},
		{provider: "azure", resource: "my-res1", base: "https://my-res1.openai.azure.com/openai/v1/"},
		{provider: "azure", resource: "evil.com/x", err: ErrBadResource},
		{provider: "azure", resource: "Upper", err: ErrBadResource},
		{provider: "azure", err: ErrBadResource},
		{provider: "openai", resource: "my-res1", err: ErrBadResource},
		{provider: "bedrock", region: "eu-west-3", modelRegion: "eu-west-3"},
		{provider: "bedrock", region: "us-gov-west-1", modelRegion: "us-gov-west-1"},
		{provider: "bedrock", region: "us-east-1.evil.com", err: ErrBadRegion},
		{provider: "bedrock", err: ErrBadRegion},
		{provider: "anthropic", region: "us-east-1", err: ErrBadRegion},
	} {
		o, ok := OfferOf(tc.provider)
		if !ok {
			t.Fatalf("no offer %s", tc.provider)
		}
		base, region, err := o.ModelBase(tc.endpoint, tc.resource, tc.region)
		if !errors.Is(err, tc.err) || base != tc.base || region != tc.modelRegion {
			t.Errorf("%+v: %q %q %v", tc, base, region, err)
			continue
		}
		if err != nil {
			continue
		}
		endpoint, resource, ok := o.ChoiceOf(base)
		wantEndpoint := tc.endpoint
		if o.Endpoint.Kind == EndpointChoice && wantEndpoint == "" {
			wantEndpoint = o.Endpoint.Choices[0].ID
		}
		if !ok || endpoint != wantEndpoint || resource != tc.resource {
			t.Errorf("%+v read back: %q %q %v", tc, endpoint, resource, ok)
		}
	}
	if _, ok := OfferOf("ollama"); ok {
		t.Error("ollama is offered")
	}
	azure, _ := OfferOf("azure")
	for _, base := range []string{"https://evil.com/openai/v1/", "https://a.b.openai.azure.com/openai/v1/", "https://x.openai.azure.com/"} {
		if _, _, ok := azure.ChoiceOf(base); ok {
			t.Errorf("%s read as a resource", base)
		}
	}
	openai, _ := OfferOf("openai")
	if _, _, ok := openai.ChoiceOf("https://api.deepseek.com"); ok {
		t.Error("another provider's endpoint read as OpenAI's")
	}
}

// Check holds one row to what Build holds it to, reading nothing: it
// passes a row Build runs, and refuses, saying why, one that Build would
// not run, the runtime's defaults among the causes.
func TestCheck(t *testing.T) {
	ctx := context.Background()
	o := Options{CoreBaseURL: core}
	good := row("agt_1", `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own", "params": {"max_output_tokens": 500}}}`)
	if err := Check(ctx, &config.Config{}, good, nil, o); err != nil {
		t.Fatalf("a good row: %v", err)
	}
	denied := &config.Config{Runtime: config.Runtime{DeniedModels: []string{"*:*:gpt-4.1*"}}}
	breaking := &config.Config{Runtime: config.Runtime{Defaults: map[string]any{"model": map[string]any{"params": map[string]any{"temperature": 5.0}}}}}
	noKey := good
	noKey.KeySecretID = ""
	custom := row("agt_1", `{"model": {"adapter": "openai_chat", "model": "m", "base_url": "https://10.1.2.3/v1"}}`)
	for _, tc := range []struct {
		name string
		yaml *config.Config
		a    store.HostedAgent
		o    Options
		want string
	}{
		{"a model the school denies", denied, good, o, "denied by runtime.denied_models"},
		{"the runtime's defaults break it", breaking, good, o, "temperature"},
		{"no CORE_BASE_URL", &config.Config{}, good, Options{}, "CORE_BASE_URL is not set"},
		{"a YAML agent's id", &config.Config{Agents: []*config.Agent{{ID: "agt_1"}}}, good, o, "a YAML agent has this id"},
		{"no key", &config.Config{}, noKey, o, "no key of the owner's is stored"},
		{"an endpoint not official", &config.Config{}, custom, o, "not an official provider's endpoint"},
	} {
		err := Check(ctx, tc.yaml, tc.a, nil, tc.o)
		if err == nil || !strings.Contains(config.Rejection{Err: err}.Detail(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
