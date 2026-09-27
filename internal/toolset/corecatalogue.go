//go:build core_catalogue

// core.Catalogue (internal/core/catalogue.go) is built on another branch at
// the same time as this package. Until both are in one tree this file is
// behind the core_catalogue tag; once they are, the tag line comes off and
// the functions below are the package's API as the worker calls it.

package toolset

import (
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// FromCore is the toolset's view of Core's catalogue: its tools by MCP
// name, and its hash. A nil catalogue has no tools, which Check refuses.
func FromCore(c *core.Catalogue) *Catalogue {
	out := &Catalogue{Tools: map[string]CatalogueTool{}}
	if c == nil {
		return out
	}
	out.Hash = c.Hash()
	for _, t := range c.Tools() {
		out.Tools[t.MCPName] = CatalogueTool{
			Name: t.MCPName, Description: t.Description, Kind: t.Kind, InputSchema: t.InputSchema,
		}
	}
	return out
}

// CheckCatalogue holds the hand-kept gates to Core's catalogue: every gated
// tool exists and is a read, and its schema sanitises and compiles
// (Catalogue.Check).
func CheckCatalogue(cat *core.Catalogue) error {
	return FromCore(cat).Check()
}

// Build is a seat's toolset from Core's catalogue (Catalogue.Build).
func Build(cat *core.Catalogue, perms map[string]string, cfg config.Tools, dialect toolschema.Dialect, cache *toolschema.Cache) (*Set, error) {
	return FromCore(cat).Build(perms, cfg, dialect, cache)
}
