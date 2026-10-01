package pgstore

import (
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/search"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// TestSearchWhateverTheLocale: the search's terms are the runtime's own,
// kept in a text[] under a GIN index, so a database whose collation and
// character classes are C, where pg_trgm finds no word in Chinese at all,
// finds a Traditional Chinese passage by a word of it, and an English one,
// as the en_US.utf8 databases of CI and the deploy do.
func TestSearchWhateverTheLocale(t *testing.T) {
	u := freshDatabaseWith(t, "TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'")
	if err := Migrate(u, Up); err != nil {
		t.Fatal(err)
	}
	s := openOn(t, u)
	ctx := t.Context()
	var ctype string
	if err := s.pool.QueryRow(ctx, `SELECT datctype FROM pg_database WHERE datname = current_database()`).Scan(&ctype); err != nil || ctype != "C" {
		t.Fatalf("the database's ctype is %q (%v)", ctype, err)
	}
	texts := []string{"## 投影片 2\n排序的複雜度：合併排序 O(n log n)", "Merge sort splits the list in two."}
	f := store.SearchFile{CourseID: "c", DocumentID: "d", VersionID: "v", Key: "f", Revision: "text:1", Source: store.SourceAI}
	for i, text := range texts {
		terms := search.Terms(text)
		f.Passages = append(f.Passages, store.SearchPassage{Offset: i * 100, Part: 1, Text: text, Terms: search.Distinct(terms), Length: len(terms)})
	}
	if err := s.PutSearchFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	ref := []store.SearchFileRef{{SearchFileKey: store.SearchFileKey{VersionID: "v", Key: "f"}, Revision: "text:1"}}
	for q, want := range map[string]int{"複雜度": 0, "合併排序": 0, "merges": 1} {
		m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: "c", Files: ref, Terms: search.QueryTerms(q), Limit: 5})
		if err != nil || len(m.Passages) != 1 || m.Passages[0].Seq != want || m.Total != 2 {
			t.Errorf("%s: %+v %v", q, m, err)
		}
	}
	if got, err := s.UseSearchFiles(ctx, "c", []store.SearchFileKey{{VersionID: "v", Key: "f"}}, time.Time{}); err != nil || got[store.SearchFileKey{VersionID: "v", Key: "f"}].Revision != "text:1" {
		t.Errorf("UseSearchFiles: %v %v", got, err)
	}
}
