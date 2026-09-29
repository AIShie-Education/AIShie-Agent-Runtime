package toolset

import (
	"os"
	"slices"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// TestCoreCatalogue checks the toolset's face for core.Catalogue against
// Core's catalogue snapshot, parsed as the runtime parses GET /v1/tools.
func TestCoreCatalogue(t *testing.T) {
	raw, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := core.ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCatalogue(cat); err != nil {
		t.Fatalf("the snapshot fails: %v", err)
	}
	view := FromCore(cat)
	if view.Hash != cat.Hash() || len(view.Tools) != 135 {
		t.Fatalf("FromCore: hash %q, %d tools", view.Hash, len(view.Tools))
	}
	if got := view.Tools["course_get"]; got.Kind != KindRead || got.Name != "course_get" || len(got.InputSchema) == 0 || got.Description == "" {
		t.Errorf("course_get is %+v", got)
	}
	s, err := Build(cat, tutorPerms, config.Tools{}, ReadOnly, toolschema.OpenAI, toolschema.NewCache())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"assignment_get", "assignment_list", "course_get", "document_get", "document_list"}; !slices.Equal(s.Names(), want) {
		t.Errorf("a tutor is offered %v, want %v", s.Names(), want)
	}
	if err := CheckCatalogue(nil); err == nil {
		t.Error("a nil catalogue passes")
	}
}
