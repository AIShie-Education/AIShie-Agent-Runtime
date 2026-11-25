package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
)

func privacyEffective(model config.Model) *config.Effective {
	return &config.Effective{Agent: config.Agent{Model: model}, Enabled: true}
}

// The disclosure is deliberately much smaller than the owner's model view:
// provider ids and existing display labels alone, never a model, endpoint,
// key reference or other model setting.
func TestPrivacyProviders(t *testing.T) {
	school := config.School{Offers: []config.SchoolOffer{
		{ID: "hosted", Label: "School DeepSeek", Provider: "deepseek"},
		{ID: "gpu", Label: "CS GPU server", Provider: "openai_compatible"},
	}}
	openAI, anthropic, deepseek, gpu := "OpenAI", "Anthropic", "DeepSeek", "CS GPU server"
	for _, tc := range []struct {
		name         string
		model        config.Model
		disabled     bool
		paused       bool
		wantPrimary  *PrimaryModelProvider
		wantFallback *FallbackModelProvider
	}{
		{
			name: "an official provider on the owner's key",
			model: config.Model{Adapter: "openai_chat", Model: "gpt-test", Provider: "openai", KeySource: "own",
				BaseURL: "https://api.openai.com/v1", KeyRef: "sealed://sec_must_not_appear"},
			wantPrimary: &PrimaryModelProvider{Plan: "own", Provider: "openai", ProviderLabel: &openAI},
		},
		{
			name: "a custom endpoint has no trusted provider label",
			model: config.Model{Adapter: "openai_chat", Model: "private-model", Provider: "openai_compatible", KeySource: "own",
				BaseURL: "https://models.internal.example/v1"},
			wantPrimary: &PrimaryModelProvider{Plan: "own", Provider: "openai_compatible"},
		},
		{
			name: "a custom endpoint cannot borrow an official provider's label",
			model: config.Model{Adapter: "openai_chat", Model: "private-model", Provider: "openai", KeySource: "own",
				BaseURL: "https://models.internal.example/v1"},
			wantPrimary: &PrimaryModelProvider{Plan: "own", Provider: "openai"},
		},
		{
			name: "a hosted school offer and an own-key fallback",
			model: config.Model{Adapter: "openai_chat", Model: "deepseek-chat", Provider: "deepseek", KeySource: "school", Offer: "hosted",
				BaseURL: "https://api.deepseek.com", KeyRef: "secret://school/keys/deepseek",
				Fallback: &config.Model{Adapter: "anthropic", Model: "claude-test", Provider: "anthropic", KeySource: "own", KeyRef: "sealed://sec_private"}},
			wantPrimary:  &PrimaryModelProvider{Plan: "school", Provider: "deepseek", ProviderLabel: &deepseek, SchoolOfferLabel: strPtr("School DeepSeek")},
			wantFallback: &FallbackModelProvider{Provider: "anthropic", ProviderLabel: &anthropic},
		},
		{
			name: "the school's own model server uses its real offer label",
			model: config.Model{Adapter: "openai_chat", Model: "llama-private", Provider: "openai_compatible", KeySource: "school", Offer: "gpu",
				BaseURL: "https://gpu.internal.example/v1", KeyRef: "secret://school/keys/gpu"},
			wantPrimary: &PrimaryModelProvider{Plan: "school", Provider: "openai_compatible", SchoolOfferLabel: &gpu},
		},
		{
			name: "an own-key primary also discloses its fallback",
			model: config.Model{Adapter: "openai_chat", Model: "gpt-test", Provider: "openai", KeySource: "own",
				Fallback: &config.Model{Adapter: "anthropic", Model: "claude-test", Provider: "anthropic", KeySource: "own"}},
			wantPrimary:  &PrimaryModelProvider{Plan: "own", Provider: "openai", ProviderLabel: &openAI},
			wantFallback: &FallbackModelProvider{Provider: "anthropic", ProviderLabel: &anthropic},
		},
		{name: "no model", model: config.Model{}},
		{name: "disabled in the course", model: config.Model{Adapter: "openai_chat", Model: "m", Provider: "openai"}, disabled: true},
		{name: "paused", model: config.Model{Adapter: "openai_chat", Model: "m", Provider: "openai"}, paused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eff := privacyEffective(tc.model)
			eff.Enabled = !tc.disabled
			eff.Paused = tc.paused
			primary, fallback := privacyProviders(eff, school)
			if !reflect.DeepEqual(primary, tc.wantPrimary) || !reflect.DeepEqual(fallback, tc.wantFallback) {
				t.Errorf("primary/fallback:\n got %+v %+v\nwant %+v %+v", primary, fallback, tc.wantPrimary, tc.wantFallback)
			}
		})
	}
}

// The wire shape has nulls rather than omitted disclosures, and contains
// none of the secrets or identifying model configuration put in the input.
func TestAgentPrivacyJSONIsMinimal(t *testing.T) {
	eff := privacyEffective(config.Model{Adapter: "openai_chat", Model: "llama-private", Provider: "openai_compatible", KeySource: "school", Offer: "gpu",
		BaseURL: "https://gpu.internal.example/v1", KeyRef: "secret://school/keys/gpu", Headers: map[string]string{"X-Internal": "private"}})
	school := config.School{Offers: []config.SchoolOffer{{ID: "gpu", Label: "CS GPU server", Provider: "openai_compatible"}}}
	primary, fallback := privacyProviders(eff, school)
	raw, err := json.Marshal(AgentPrivacy{AgentID: "0192f3c1-0000-7000-8000-00000000abcd", Primary: primary, Fallback: fallback})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"agent_id":"0192f3c1-0000-7000-8000-00000000abcd","primary":{"plan":"school","provider":"openai_compatible","provider_label":null,"school_offer_label":"CS GPU server"},"fallback":null}`
	if string(raw) != want {
		t.Errorf("JSON:\n%s\nwant:\n%s", raw, want)
	}
	var members map[string]any
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Errorf("top-level members: %v", members)
	}
}
