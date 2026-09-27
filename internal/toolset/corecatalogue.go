//go:build core_catalogue

// core.Catalogue (internal/core/catalogue.go) is built on branch
// wt/core-client at the same time as this package, so until both are in one
// tree this file and its test build only with the core_catalogue tag. At the
// merge, delete the first two lines of both (the tag and the blank line
// after it) and this comment: the functions below are then the package's API
// as the worker calls it. They were checked against that branch's
// catalogue.go, and the plain build keeps the same logic on Catalogue.

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
