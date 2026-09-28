package toolset

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext"
)

// TextCache keeps what the runtime read of documents' files for the models
// given their text, so that a file read in parts is fetched and read once,
// not once a part (design §4, Files). One is kept per worker process and
// shared by its agents: a reading is keyed by the document version Core
// named, which Core names to a caller only while it may read it, and a
// version's file never changes. Only what took reading is kept, never a
// file's bytes; the bytes it holds in all are bounded, and past them the
// reading used least recently goes first. It is safe for concurrent use;
// a nil *TextCache keeps nothing.
type TextCache struct {
	mu    sync.Mutex
	max   int64
	size  int64
	lru   *list.List // of *cachedReading, the most recently used first
	byKey map[string]*list.Element
	hits  uint64
	miss  uint64
}

// DefaultTextCacheBytes bounds a TextCache made with a size of 0 or less:
// a hundred long decks' text, and far below what would hurt a worker.
const DefaultTextCacheBytes = 32 << 20

// NewTextCache keeps readings of at most maxBytes of text in all.
func NewTextCache(maxBytes int64) *TextCache {
	if maxBytes <= 0 {
		maxBytes = DefaultTextCacheBytes
	}
	return &TextCache{max: maxBytes, lru: list.New(), byKey: map[string]*list.Element{}}
}

// fileReading is what the runtime made of a file's bytes for a model given
// its text: the text read, or why none could be.
type fileReading struct {
	// mt is the file's media type: Core's, or as the runtime sniffed it
	// when Core's said nothing.
	mt string
	// size is the file's bytes, and sum their sha256, as "sha256:<hex>",
	// as the runtime computed it.
	size int64
	sum  string
	// res is the text read: doctext's of an Office file or a PDF, and a
	// text file's own as Result{Text}. nil when err says why there is
	// none.
	res *doctext.Result
	err error
}

type cachedReading struct {
	key  string
	r    *fileReading
	cost int64
}

// cost is what a reading is counted as in the cache's bound.
func (r *fileReading) cost() int64 {
	n := int64(len(r.mt)+len(r.sum)) + 256
	if r.res != nil {
		n += int64(len(r.res.Text)) + int64(len(r.res.Sections))*32
		for _, s := range r.res.Notes {
			n += int64(len(s))
		}
	}
	return n
}

// get is the reading kept under key, nil when there is none.
func (c *TextCache) get(key string) *fileReading {
	if c == nil || key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[key]
	if !ok {
		c.miss++
		return nil
	}
	c.hits++
	c.lru.MoveToFront(e)
	return e.Value.(*cachedReading).r
}

// put keeps r under key, and lets go of the readings used least recently
// past the bound. One larger than the whole bound is not kept.
func (c *TextCache) put(key string, r *fileReading) {
	if c == nil || key == "" || r == nil {
		return
	}
	cost := r.cost()
	c.mu.Lock()
	defer c.mu.Unlock()
	if cost > c.max {
		return
	}
	if e, ok := c.byKey[key]; ok {
		c.size -= e.Value.(*cachedReading).cost
		c.lru.Remove(e)
		delete(c.byKey, key)
	}
	c.byKey[key] = c.lru.PushFront(&cachedReading{key: key, r: r, cost: cost})
	c.size += cost
	for c.size > c.max {
		last := c.lru.Back()
		old := last.Value.(*cachedReading)
		c.lru.Remove(last)
		delete(c.byKey, old.key)
		c.size -= old.cost
	}
}

// TextCacheStats are a cache's counts.
type TextCacheStats struct {
	Readings     int
	Bytes        int64
	Hits, Misses uint64
}

// Stats are the cache's counts now.
func (c *TextCache) Stats() TextCacheStats {
	if c == nil {
		return TextCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return TextCacheStats{Readings: c.lru.Len(), Bytes: c.size, Hits: c.hits, Misses: c.miss}
}

// textKey is what a document's reading is kept under: its version, as
// Core names it, with the checksum Core gave, and the limits it is read
// within. "" (nothing kept) when Core named no version.
func (r Runner) textKey(d *docFile) string {
	if d.versionID == "" {
		return ""
	}
	return fmt.Sprintf("%s\x00%s\x00%+v", d.versionID, d.checksum, r.DocLimits)
}

// checksum is data's sha256, as "sha256:<hex>": Core's own form of a
// checksum where its store computes one from the bytes.
func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
