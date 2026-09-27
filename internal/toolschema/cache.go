package toolschema

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// Cache keeps sanitised schemas, so that each tool is sanitised once per
// catalogue and dialect (§3.8), however many seats are offered it. It is
// safe for concurrent use, and the zero value is an empty cache. A nil
// *Cache sanitises every time.
type Cache struct {
	mu sync.Mutex
	m  map[cacheKey]cached
	// max bounds the entries (defaultCacheMax when 0). Past it (many
	// catalogues over a long run) every entry is dropped and made again as
	// it is used, which costs little: a catalogue's worth is about a hundred
	// tools a dialect.
	max int
}

const defaultCacheMax = 4096

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
	return &Cache{m: make(map[cacheKey]cached), max: defaultCacheMax}
}

// Sanitise is Sanitise(coreSchema, d, bound), made once for the catalogue
// whose hash is catalogueHash and the tool named tool: the catalogue's hash
// names its schemas, so a schema that changes comes with a new hash. A
// schema that cannot be sanitised is remembered as such too. With no hash
// there is nothing to say two schemas are the same, so nothing is cached.
// The schema returned is the caller's own copy.
func (c *Cache) Sanitise(catalogueHash, tool string, coreSchema json.RawMessage, d Dialect, bound []string) (json.RawMessage, error) {
	if c == nil || catalogueHash == "" {
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
		if c.m == nil {
			c.m = make(map[cacheKey]cached)
		}
		limit := c.max
		if limit <= 0 {
			limit = defaultCacheMax
		}
		if len(c.m) >= limit {
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

// Len is how many schemas the cache holds; none for a nil cache.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
