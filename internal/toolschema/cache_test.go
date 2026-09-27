package toolschema

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
)

func TestCache(t *testing.T) {
	schema := catalogueTool(t, "document_get").InputSchema
	c := NewCache()
	first, err := c.Sanitise("h1", "document_get", schema, OpenAI, testBound)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Sanitise(schema, OpenAI, testBound)
	if string(first) != string(want) {
		t.Fatalf("cached %s, want %s", first, want)
	}
	// The caller's copy is its own.
	first[0] = 'X'
	again, _ := c.Sanitise("h1", "document_get", schema, OpenAI, testBound)
	if string(again) != string(want) {
		t.Fatalf("a caller's change reached the cache: %s", again)
	}
	if c.Len() != 1 {
		t.Fatalf("%d entries after the same call twice, want 1", c.Len())
	}
	// Another dialect, catalogue, tool or binding is another entry.
	_, _ = c.Sanitise("h1", "document_get", schema, Kimi, testBound)
	_, _ = c.Sanitise("h2", "document_get", schema, OpenAI, testBound)
	_, _ = c.Sanitise("h1", "grade_get", schema, OpenAI, testBound)
	_, _ = c.Sanitise("h1", "document_get", schema, OpenAI, []string{"course_id"})
	if c.Len() != 5 {
		t.Fatalf("%d entries, want 5", c.Len())
	}
}

// TestCacheKeysOnHash checks that the cache trusts the catalogue's hash: the
// same hash and tool are the same schema, which is what the hash promises.
func TestCacheKeysOnHash(t *testing.T) {
	c := NewCache()
	a, _ := c.Sanitise("h", "t", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`), OpenAI, nil)
	b, _ := c.Sanitise("h", "t", json.RawMessage(`{"type":"object","properties":{"b":{"type":"string"}}}`), OpenAI, nil)
	if string(a) != string(b) {
		t.Fatalf("the same key gave %s and %s", a, b)
	}
}

func TestCacheRemembersErrors(t *testing.T) {
	c := NewCache()
	bad := json.RawMessage(`{"type":"object","properties":{"n":{"$ref":"#"}}}`)
	for range 2 {
		if _, err := c.Sanitise("h", "t", bad, OpenAI, nil); err == nil {
			t.Fatal("a recursive schema was taken")
		}
	}
	if c.Len() != 1 {
		t.Fatalf("%d entries, want the error remembered once", c.Len())
	}
}

func TestCacheBounded(t *testing.T) {
	c := NewCache()
	c.max = 3
	schema := json.RawMessage(`{"type":"object"}`)
	for i := range 10 {
		if _, err := c.Sanitise(strconv.Itoa(i), "t", schema, OpenAI, nil); err != nil {
			t.Fatal(err)
		}
		if c.Len() > 3 {
			t.Fatalf("%d entries, past the bound of 3", c.Len())
		}
	}
}

func TestNilCache(t *testing.T) {
	var c *Cache
	got, err := c.Sanitise("h", "t", json.RawMessage(`{"type":"object"}`), OpenAI, nil)
	if err != nil || string(got) != `{"properties":{},"type":"object"}` {
		t.Fatalf("got %s, %v", got, err)
	}
	if c.Len() != 0 {
		t.Fatal("a nil cache holds something")
	}
}

func TestZeroCache(t *testing.T) {
	var c Cache
	for range 2 {
		got, err := c.Sanitise("h", "t", json.RawMessage(`{"type":"object"}`), OpenAI, nil)
		if err != nil || string(got) != `{"properties":{},"type":"object"}` {
			t.Fatalf("got %s, %v", got, err)
		}
	}
	if c.Len() != 1 {
		t.Fatalf("%d entries, want 1", c.Len())
	}
}

// TestCacheWithoutHash checks that a catalogue with no hash is not cached:
// nothing then says that two schemas of one tool are the same.
func TestCacheWithoutHash(t *testing.T) {
	c := NewCache()
	a, _ := c.Sanitise("", "t", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`), OpenAI, nil)
	b, _ := c.Sanitise("", "t", json.RawMessage(`{"type":"object","properties":{"b":{"type":"string"}}}`), OpenAI, nil)
	if string(a) == string(b) || c.Len() != 0 {
		t.Fatalf("got %s and %s, %d entries", a, b, c.Len())
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := NewCache()
	tools := loadCatalogue(t)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i, tool := range tools {
				d := allDialects[(i+w)%len(allDialects)]
				if _, err := c.Sanitise("h", tool.mcpName(), tool.InputSchema, d, testBound); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}
