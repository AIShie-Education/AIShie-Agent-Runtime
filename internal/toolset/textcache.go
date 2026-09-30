package toolset

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
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
	// byTag are the keys of the readings kept for each tag: a file a
	// message of a conversation carries, and its message, whose readings
	// go when the message is retracted (Drop).
	byTag map[string]map[string]struct{}
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
	return &TextCache{max: maxBytes, lru: list.New(), byKey: map[string]*list.Element{}, byTag: map[string]map[string]struct{}{}}
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
	// fam is the file's Office family when it is an Office file the
	// runtime converts (Runner.Office): its text is given as that
	// family's is. of is what the text was read from when that is not the
	// file itself but what LibreOffice made of it: a document's PDF, an
	// older or OpenDocument deck's PowerPoint form, a workbook's Excel
	// form.
	fam office.Family
	of  office.Target
	// text is the version's text version this is (textversion.go), nil
	// for a reading of the file.
	text *core.TextView
}

type cachedReading struct {
	key  string
	r    *fileReading
	cost int64
	tags []string
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

// get is the reading kept under key, nil when there is none; one found is
// tagged with tags besides the tags it has (put).
func (c *TextCache) get(key string, tags ...string) *fileReading {
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
	cr := e.Value.(*cachedReading)
	c.tagLocked(cr, tags)
	return cr.r
}

// put keeps r under key, tagged with tags (Drop), and lets go of the
// readings used least recently past the bound. One larger than the whole
// bound is not kept.
func (c *TextCache) put(key string, r *fileReading, tags ...string) {
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
		// Read again: kept for whatever it was kept for before, too.
		tags = append(slices.Clone(e.Value.(*cachedReading).tags), tags...)
		c.removeLocked(e)
	}
	cr := &cachedReading{key: key, r: r, cost: cost}
	c.byKey[key] = c.lru.PushFront(cr)
	c.size += cost
	c.tagLocked(cr, tags)
	for c.size > c.max {
		c.removeLocked(c.lru.Back())
	}
}

// tagLocked tags cr with tags it has not yet.
func (c *TextCache) tagLocked(cr *cachedReading, tags []string) {
	for _, tag := range tags {
		if tag == "" || slices.Contains(cr.tags, tag) {
			continue
		}
		cr.tags = append(cr.tags, tag)
		keys := c.byTag[tag]
		if keys == nil {
			keys = map[string]struct{}{}
			c.byTag[tag] = keys
		}
		keys[cr.key] = struct{}{}
	}
}

// removeLocked lets go of one reading, and of its tags.
func (c *TextCache) removeLocked(e *list.Element) {
	cr := e.Value.(*cachedReading)
	c.lru.Remove(e)
	delete(c.byKey, cr.key)
	c.size -= cr.cost
	for _, tag := range cr.tags {
		if keys := c.byTag[tag]; keys != nil {
			delete(keys, cr.key)
			if len(keys) == 0 {
				delete(c.byTag, tag)
			}
		}
	}
}

// Drop lets go of every reading tagged tag: the worker drops those of a
// retracted message's files by the message's id, and the files'
// readings are gone with them, whatever else they were kept for.
func (c *TextCache) Drop(tag string) {
	if c == nil || tag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.byTag[tag] {
		if e, ok := c.byKey[key]; ok {
			c.removeLocked(e)
		}
	}
	delete(c.byTag, tag)
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
// Core names it, and the file of it, by its id where Core names one (a
// version holds several since AIShie-Core #49), with the checksum Core
// gave, and the limits it is read within. "" (nothing kept) when Core
// named no version.
func (r Runner) textKey(d *docFile) string {
	if d.attachmentID != "" {
		return r.attachmentKey(d)
	}
	if d.versionID == "" {
		return ""
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%+v", d.versionID, d.fileID, d.checksum, r.DocLimits)
}

// checksum is data's sha256, as "sha256:<hex>": Core's own form of a
// checksum where its store computes one from the bytes.
func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// kept is the reading kept under key, nil when there is none; one found is
// tagged with the runner's tags, as keep tags one.
func (r Runner) kept(key string) *fileReading { return r.Texts.get(key, r.tags...) }

// keep keeps rd under key, tagged with the runner's tags: a file a message
// carries is kept tagged with its id and its message's (attachments.go), so
// that what was read of it goes when the message is retracted.
func (r Runner) keep(key string, rd *fileReading) { r.Texts.put(key, rd, r.tags...) }
