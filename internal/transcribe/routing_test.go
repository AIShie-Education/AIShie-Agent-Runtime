package transcribe

import (
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
)

// The model made from an offer at OpenRouter carries the offer's upstream
// routing to its adapter, and is made again when the routing changes.
func TestModelCarriesTheOffersRouting(t *testing.T) {
	var mu sync.Mutex
	var made []llm.Config
	r := newRig(t, func(r *rig, o *Options) {
		o.NewAdapter = func(c llm.Config) (llm.Adapter, error) {
			mu.Lock()
			defer mu.Unlock()
			made = append(made, c)
			return r.model, nil
		}
	})
	deny := "deny"
	or := config.SchoolOffer{ID: "llama", Label: "Llama", Adapter: llm.AdapterOpenAIChat, BaseURL: "https://openrouter.ai/api/v1",
		Model: "meta-llama/llama-4-maverick", KeyRef: "secret://school/keys/flash",
		OpenRouter: &openrouter.Routing{DataCollection: &deny, Only: []string{"groq"}}}
	st := r.setting
	st.Offer = &or
	if _, err := r.svc.modelOf(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(made) != 1 || string(made[0].OpenRouter.JSON()) != `{"data_collection":"deny","only":["groq"]}` {
		t.Fatalf("the model's routing: %+v", made)
	}
	mu.Unlock()
	if _, err := r.svc.modelOf(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	changed := or
	changed.OpenRouter = &openrouter.Routing{DataCollection: &deny, Only: []string{"together"}}
	st.Offer = &changed
	if _, err := r.svc.modelOf(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(made) != 2 || string(made[1].OpenRouter.JSON()) != `{"data_collection":"deny","only":["together"]}` {
		t.Errorf("after the change: %d models", len(made))
	}
}
