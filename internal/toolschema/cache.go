package toolschema

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// Cache keeps sanitised schemas, so that each tool is sanitised once per
// catalogue and dialect (§3.8), however many seats are offered it. It is
// safe for concurrent use; the zero value is not ready, use NewCache. A nil
// *Cache sanitises every time.
type Cache struct {
	mu sync.Mutex
	m  map[cacheKey]cached
	// max bounds the entries. Past it (many catalogues over a long run)
	// every entry is dropped and made again as it is used, which costs
	// little: a catalogue's worth is about a hundred tools a dialect.
	max int
}

type cacheKey struct {
	hash, tool string
	dialect    Dialect
	// bound is the bound names, joined: the same tool bound otherwise is
	// another schema.
	bound string
}

type cached struct {
	schema json.RawMessage
	err    error
}

// NewCache makes an empty cache.
func NewCache() *Cache {
	return &Cache{m: make(map[cacheKey]cached), max: 4096}
}

// Sanitise is Sanitise(coreSchema, d, bound), made once for the catalogue
// whose hash is catalogueHash and the tool named tool: the catalogue's hash
// names its schemas, so a schema that changes comes with a new hash. A
// schema that cannot be sanitised is remembered as such too. The schema
// returned is the caller's own copy.
func (c *Cache) Sanitise(catalogueHash, tool string, coreSchema json.RawMessage, d Dialect, bound []string) (json.RawMessage, error) {
	if c == nil {
		return Sanitise(coreSchema, d, bound)
	}
	key := cacheKey{hash: catalogueHash, tool: tool, dialect: d, bound: strings.Join(bound, "\x00")}
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if !ok {
		// Made outside the lock: two callers may both make it, which is
		// harmless, and none waits on another's.
		e.schema, e.err = Sanitise(coreSchema, d, bound)
		c.mu.Lock()
		if len(c.m) >= c.max {
			clear(c.m)
		}
		c.m[key] = e
		c.mu.Unlock()
	}
	if e.err != nil {
		return nil, e.err
	}
	return bytes.Clone(e.schema), nil
}

// Len is how many schemas the cache holds.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
