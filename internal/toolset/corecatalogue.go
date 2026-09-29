// The toolset of a seat, from Core's own catalogue: what the worker calls.
// The logic is Catalogue's; these take core.Catalogue as it is fetched.

package toolset

import (
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
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
// tool exists and is of its gate's kind, and its schema sanitises and
// compiles (Catalogue.Check).
func CheckCatalogue(cat *core.Catalogue) error {
	return FromCore(cat).Check()
}

// Build is a seat's toolset from Core's catalogue (Catalogue.Build).
func Build(cat *core.Catalogue, perms map[string]string, cfg config.Tools, access Access, dialect toolschema.Dialect, cache *toolschema.Cache) (*Set, error) {
	return FromCore(cat).Build(perms, cfg, access, dialect, cache)
}
