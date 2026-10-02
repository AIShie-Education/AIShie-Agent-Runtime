package anthropic

import (
	"cmp"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/livetest"
)

// TestLive calls Anthropic's API when LIVE=1 and ANTHROPIC_API_KEY are
// set (make live): a plain answer, one streamed, then every tool of Core's
// catalogue declared, in requests of at most livetest.MaxTools, at 16
// output tokens, so that the API itself checks their schemas (§8.2).
// ANTHROPIC_MODEL picks the model, as the nightly live.yml names it (or
// ANTHROPIC_LIVE_MODEL, its name before).
func TestLive(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if os.Getenv("LIVE") != "1" || key == "" {
		t.Skip("LIVE=1 and ANTHROPIC_API_KEY run this against the real API")
	}
	model := cmp.Or(os.Getenv("ANTHROPIC_MODEL"), os.Getenv("ANTHROPIC_LIVE_MODEL"), "claude-haiku-4-5")
	a, err := New(llm.Config{Model: model, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := a.Call(ctx, &llm.Request{
		System:   "Answer in one word.",
		Messages: []llm.Message{llm.UserText("What colour is a clear daytime sky?")},
		Limits:   llm.Limits{MaxOutputTokens: 16},
	})
	if err != nil {
		t.Fatalf("a plain call: %v", err)
	}
	if resp.Stop != llm.StopEnd && resp.Stop != llm.StopMaxTokens {
		t.Errorf("stop %q (%q), want end or max_tokens", resp.Stop, resp.RawStop)
	}
	if resp.Usage.Input == 0 || resp.Usage.Output == 0 || len(resp.Usage.Raw) == 0 || resp.RequestID == "" {
		t.Errorf("usage %+v, request id %q", resp.Usage, resp.RequestID)
	}

	var told strings.Builder
	resp, err = a.Stream(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserText("Count from 1 to 30, with commas.")},
		Limits:   llm.Limits{MaxOutputTokens: 200},
	}, func(d string) { told.WriteString(d) })
	if err != nil {
		t.Fatalf("a streamed call: %v", err)
	}
	if told.String() != resp.Text() || resp.Usage.Input == 0 || resp.Usage.Output == 0 {
		t.Errorf("streamed: told %q, text %q, usage %+v", told.String(), resp.Text(), resp.Usage)
	}

	for batch := range slices.Chunk(livetest.CatalogueTools(t, true), livetest.MaxTools) {
		resp, err = a.Call(ctx, &llm.Request{
			System:   "You are a course tutor.",
			Messages: []llm.Message{llm.UserText("Which tools do you have? Answer in one sentence.")},
			Tools:    batch,
			ToolMode: llm.ToolAuto,
			Limits:   llm.Limits{MaxOutputTokens: 16},
		})
		if err != nil {
			t.Fatalf("a call declaring %d tools of the catalogue, from %s: %v", len(batch), batch[0].Name, err)
		}
		t.Logf("%d tools declared: stop %s (%s), usage %+v", len(batch), resp.Stop, resp.RawStop, resp.Usage)
	}
}

// Every tool of Core's catalogue can be declared: a name the API takes, and
// a schema that is an object.
func TestCatalogueCanBeDeclared(t *testing.T) {
	tools := livetest.CatalogueTools(t, true)
	if len(tools) != 168 {
		t.Fatalf("%d tools in the catalogue, want 168", len(tools))
	}
	a := newAdapter(t, llm.Config{})
	w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}, Tools: tools, ToolMode: llm.ToolAuto})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range w.Tools {
		if len(tool.Name) == 0 || len(tool.Name) > 128 || strings.IndexFunc(tool.Name, func(r rune) bool { return r > 127 || !idByte(byte(r)) }) >= 0 {
			t.Errorf("tool name %q is not one the API takes", tool.Name)
		}
	}
	if w.Tools[len(w.Tools)-1].CacheControl == nil || w.Tools[0].CacheControl != nil {
		t.Error("only the last tool is marked for caching")
	}
}
