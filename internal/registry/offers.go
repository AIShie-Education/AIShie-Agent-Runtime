package registry

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// The providers an owner may put their own key to (the product owner's D9,
// the API contract's §5.3): their official endpoints and nothing else, so
// that the owner never gives a URL. The API builds a model's base_url from
// an offer and the owner's choice of its endpoint, resource or region, each
// held to a pattern with no dots; official checks the result again, as it
// does every hosted model. Self-hosted servers (Ollama, LM Studio, vLLM,
// any other OpenAI-compatible one) are never offered.

// How an offer's endpoint is chosen.
const (
	// EndpointFixed: one endpoint, which the owner does not choose.
	EndpointFixed = "fixed"
	// EndpointChoice: one of a list of the provider's endpoints.
	EndpointChoice = "choice"
	// EndpointAzureResource: the owner's Azure OpenAI resource, by name.
	EndpointAzureResource = "azure_resource"
	// EndpointBedrockRegion: an AWS region.
	EndpointBedrockRegion = "bedrock_region"
)

// Offer is one provider an owner may put their own key to.
type Offer struct {
	Provider string
	Label    string
	// Adapters are the APIs the provider is spoken to by, the first the
	// default.
	Adapters []string
	Endpoint Endpoint
	// KeyPrefix begins the provider's keys, a hint for the form and never
	// enforced; "" for none.
	KeyPrefix string
}

// Endpoint is how an offer's base_url is made.
type Endpoint struct {
	Kind string
	// BaseURL is a fixed endpoint's URL, for the owner to read.
	BaseURL string
	// Stored is what a fixed endpoint's model keeps as its base_url: ""
	// for the adapter's own default.
	Stored string
	// Choices are a choice's endpoints, the first the default.
	Choices []Choice
	// Pattern and Example are an Azure resource's, for the form.
	Pattern string
	Example string
	// Suggested are the Bedrock regions the form suggests.
	Suggested []string
}

// Choice is one endpoint of a provider's to choose from.
type Choice struct {
	ID, Label, BaseURL string
}

// ResourceRe and RegionRe are what an Azure resource's name and an AWS
// region may be: letters, digits and dashes, no dot, so that neither can
// put the endpoint under another host.
var (
	ResourceRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	RegionRe   = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]{1,2}$`)
)

// azureURL is the endpoint of an Azure OpenAI resource: the v1 API, which
// both the Responses and the Chat Completions adapters speak.
const azureURL = "https://%s.openai.azure.com/openai/v1/"

// offers is the table; Offers gives copies.
var offers = []Offer{
	{Provider: llm.ProviderOpenAI, Label: "OpenAI", Adapters: []string{llm.AdapterOpenAIChat, llm.AdapterOpenAIResponses},
		Endpoint: Endpoint{Kind: EndpointFixed, BaseURL: "https://api.openai.com/v1"}, KeyPrefix: "sk-"},
	{Provider: llm.ProviderAzure, Label: "Azure OpenAI", Adapters: []string{llm.AdapterOpenAIResponses, llm.AdapterOpenAIChat},
		Endpoint: Endpoint{Kind: EndpointAzureResource, Pattern: ResourceRe.String(), Example: "my-resource"}},
	{Provider: llm.ProviderAnthropic, Label: "Anthropic", Adapters: []string{llm.AdapterAnthropic},
		Endpoint: Endpoint{Kind: EndpointFixed, BaseURL: "https://api.anthropic.com"}, KeyPrefix: "sk-ant-"},
	{Provider: llm.ProviderGemini, Label: "Google Gemini", Adapters: []string{llm.AdapterGemini},
		Endpoint: Endpoint{Kind: EndpointFixed, BaseURL: "https://generativelanguage.googleapis.com"}, KeyPrefix: "AIza"},
	{Provider: llm.ProviderBedrock, Label: "Amazon Bedrock", Adapters: []string{llm.AdapterBedrockConverse},
		Endpoint: Endpoint{Kind: EndpointBedrockRegion, Pattern: RegionRe.String(), Suggested: []string{"us-east-1", "us-east-2", "us-west-2",
			"eu-central-1", "eu-west-1", "eu-west-3", "ap-northeast-1", "ap-southeast-1", "ap-southeast-2", "ap-south-1"}}, KeyPrefix: "ABSK"},
	{Provider: llm.ProviderDeepSeek, Label: "DeepSeek", Adapters: []string{llm.AdapterOpenAIChat},
		Endpoint: Endpoint{Kind: EndpointFixed, BaseURL: "https://api.deepseek.com", Stored: "https://api.deepseek.com"}, KeyPrefix: "sk-"},
	{Provider: llm.ProviderQwen, Label: "Qwen (Alibaba Cloud Model Studio)", Adapters: []string{llm.AdapterOpenAIChat},
		Endpoint: Endpoint{Kind: EndpointChoice, Choices: []Choice{
			{ID: "dashscope-us", Label: "United States (Virginia)", BaseURL: "https://dashscope-us.aliyuncs.com/compatible-mode/v1"},
			{ID: "dashscope", Label: "China (Beijing)", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"},
			{ID: "dashscope-intl", Label: "International (Singapore)", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		}}, KeyPrefix: "sk-"},
	{Provider: llm.ProviderMoonshot, Label: "Moonshot (Kimi)", Adapters: []string{llm.AdapterOpenAIChat},
		Endpoint: Endpoint{Kind: EndpointChoice, Choices: []Choice{
			{ID: "global", Label: "Global", BaseURL: "https://api.moonshot.ai/v1"},
			{ID: "china", Label: "China", BaseURL: "https://api.moonshot.cn/v1"},
		}}, KeyPrefix: "sk-"},
	// GLM's mainland endpoint is offered once a live keys/test has passed
	// against it (the API contract's §11, question 7).
	{Provider: llm.ProviderGLM, Label: "Zhipu GLM", Adapters: []string{llm.AdapterOpenAIChat},
		Endpoint: Endpoint{Kind: EndpointChoice, Choices: []Choice{
			{ID: "global", Label: "Global", BaseURL: "https://api.z.ai/api/paas/v4"},
		}}},
	{Provider: llm.ProviderOpenRouter, Label: "OpenRouter", Adapters: []string{llm.AdapterOpenAIChat},
		Endpoint: Endpoint{Kind: EndpointFixed, BaseURL: "https://openrouter.ai/api/v1", Stored: "https://openrouter.ai/api/v1"}, KeyPrefix: "sk-or-"},
}

// Offers are the providers an owner may put their own key to, in the order
// a form lists them.
func Offers() []Offer {
	out := make([]Offer, len(offers))
	for i, o := range offers {
		out[i] = o.clone()
	}
	return out
}

// OfferOf is the offer of provider.
func OfferOf(provider string) (Offer, bool) {
	for _, o := range offers {
		if o.Provider == provider {
			return o.clone(), true
		}
	}
	return Offer{}, false
}

func (o Offer) clone() Offer {
	o.Adapters = slices.Clone(o.Adapters)
	o.Endpoint.Choices = slices.Clone(o.Endpoint.Choices)
	o.Endpoint.Suggested = slices.Clone(o.Endpoint.Suggested)
	return o
}

// Errors of ModelBase.
var (
	// ErrUnknownEndpoint is an endpoint choice the offer does not have.
	ErrUnknownEndpoint = errors.New("registry: not one of the provider's endpoints")
	// ErrBadResource and ErrBadRegion are a resource or region not of their
	// pattern, or missing where needed.
	ErrBadResource = errors.New("registry: not an Azure resource's name")
	ErrBadRegion   = errors.New("registry: not an AWS region")
)

// ModelBase is the base_url and region a model of the offer keeps, made
// from the owner's choice of endpoint (a choice's id, "" for its first),
// Azure resource, or AWS region: "" for the adapter's own default. It
// refuses a choice the offer does not have, a resource or region not of
// their patterns, and one given where the endpoint takes none.
func (o Offer) ModelBase(endpoint, resource, region string) (baseURL, modelRegion string, err error) {
	e := o.Endpoint
	if endpoint != "" && e.Kind != EndpointChoice {
		return "", "", ErrUnknownEndpoint
	}
	if resource != "" && e.Kind != EndpointAzureResource {
		return "", "", ErrBadResource
	}
	if region != "" && e.Kind != EndpointBedrockRegion {
		return "", "", ErrBadRegion
	}
	switch e.Kind {
	case EndpointFixed:
		return e.Stored, "", nil
	case EndpointChoice:
		if endpoint == "" {
			return e.Choices[0].BaseURL, "", nil
		}
		for _, c := range e.Choices {
			if c.ID == endpoint {
				return c.BaseURL, "", nil
			}
		}
		return "", "", ErrUnknownEndpoint
	case EndpointAzureResource:
		if !ResourceRe.MatchString(resource) {
			return "", "", ErrBadResource
		}
		return fmt.Sprintf(azureURL, resource), "", nil
	case EndpointBedrockRegion:
		if !RegionRe.MatchString(region) {
			return "", "", ErrBadRegion
		}
		return "", region, nil
	}
	return "", "", ErrUnknownEndpoint
}

// ChoiceOf is which of the offer's endpoints a model's base_url is: the
// choice's id, or the Azure resource's name; ok is false for a base_url
// the offer would not have made.
func (o Offer) ChoiceOf(baseURL string) (endpoint, resource string, ok bool) {
	e := o.Endpoint
	switch e.Kind {
	case EndpointFixed:
		return "", "", baseURL == e.Stored
	case EndpointChoice:
		for _, c := range e.Choices {
			if c.BaseURL == baseURL {
				return c.ID, "", true
			}
		}
	case EndpointAzureResource:
		u, err := url.Parse(baseURL)
		if err != nil {
			return "", "", false
		}
		name, found := strings.CutSuffix(u.Hostname(), ".openai.azure.com")
		if found && fmt.Sprintf(azureURL, name) == baseURL && ResourceRe.MatchString(name) {
			return "", name, true
		}
	case EndpointBedrockRegion:
		return "", "", baseURL == ""
	}
	return "", "", false
}

// Official reports whether u is an endpoint the runtime calls for a hosted
// agent's model behind adapter: a provider's own (the package's comment).
func Official(adapter string, u *url.URL) bool { return official(adapter, u) }
