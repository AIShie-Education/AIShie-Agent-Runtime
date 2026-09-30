package office

import (
	"container/list"
	"time"
)

// cache keeps conversions and ranges in memory, their bytes bounded, the
// one used least recently going first past the bound; a failure is kept
// until its time is up. Its caller holds the lock.
type cache struct {
	max, size int64
	lru       *list.List // of *entry, the most recently used first
	byKey     map[string]*list.Element
}

type entry struct {
	key string
	// out is a conversion's or a range's output; nil for a failure, whose
	// why is kept until expires.
	out     *Output
	why     string
	expires time.Time
	cost    int64
}

func newCache(max int64) *cache {
	return &cache{max: max, lru: list.New(), byKey: map[string]*list.Element{}}
}

// get is the entry under key, nil when there is none or it has expired.
func (c *cache) get(key string, now time.Time) *entry {
	el, ok := c.byKey[key]
	if !ok {
		return nil
	}
	e := el.Value.(*entry)
	if !e.expires.IsZero() && now.After(e.expires) {
		c.remove(el)
		return nil
	}
	c.lru.MoveToFront(el)
	return e
}

// put keeps e, letting go of the entries used least recently past the
// bound. One larger than the whole bound is not kept.
func (c *cache) put(e *entry) {
	e.cost = int64(len(e.key)+len(e.why)) + 128
	if e.out != nil {
		e.cost += int64(len(e.out.Data))
	}
	if e.cost > c.max {
		return
	}
	if el, ok := c.byKey[e.key]; ok {
		c.remove(el)
	}
	c.byKey[e.key] = c.lru.PushFront(e)
	c.size += e.cost
	for c.size > c.max {
		c.remove(c.lru.Back())
	}
}

func (c *cache) remove(el *list.Element) {
	e := el.Value.(*entry)
	c.lru.Remove(el)
	delete(c.byKey, e.key)
	c.size -= e.cost
}
