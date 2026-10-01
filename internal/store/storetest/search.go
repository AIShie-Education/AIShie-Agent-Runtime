package storetest

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/search"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// passage is a passage of text on slide or page n of kind, at offset,
// in part 1, with its terms as the search makes them.
func passage(kind string, n, offset int, text string) store.SearchPassage {
	terms := search.Terms(text)
	return store.SearchPassage{Kind: kind, N: n, Offset: offset, Part: 1, Text: text, Terms: search.Distinct(terms), Length: len(terms)}
}

// testSearchIndex: a file is kept by its version and key, at a revision,
// its passages in place of any it had; a search reads the files it names,
// each at the revision it names and in the course it names, and no other,
// those passages that hold any of its terms, those that hold the most
// first, then by version, key and place, bytewise, at most its limit,
// with how many passages hold each term and how many and how long they
// are in all; using files marks them needed, and says the revisions they
// are kept at and how many passages they have; files are dropped by version, by document, and when no
// search has needed them since a time; what a store must not keep is
// refused.
func testSearchIndex(t *testing.T, open Opener) {
	s := open(t)
	ctx := t.Context()
	const (
		course, other = "0192f3c1-0000-7000-8000-00000000c001", "0192f3c1-0000-7000-8000-00000000c002"
		doc1, doc2    = "0192f3c1-0000-7000-8000-00000000d001", "0192f3c1-0000-7000-8000-00000000d002"
		v1, v2, v3    = "0192f3c1-0000-7000-8000-00000000a001", "0192f3c1-0000-7000-8000-00000000a002", "0192f3c1-0000-7000-8000-00000000a003"
		slides        = "0192f3c1-0000-7000-8000-00000000f001"
		handout       = "0192f3c1-0000-7000-8000-00000000f002"
	)
	key := func(v, k string) store.SearchFileKey { return store.SearchFileKey{VersionID: v, Key: k} }
	ref := func(v, k, rev string) store.SearchFileRef {
		return store.SearchFileRef{SearchFileKey: key(v, k), Revision: rev}
	}

	if got, err := s.UseSearchFiles(ctx, course, []store.SearchFileKey{key(v1, slides)}, time.Time{}); err != nil || len(got) != 0 {
		t.Fatalf("nothing kept: %v %v", got, err)
	}
	if got, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: course, Files: []store.SearchFileRef{ref(v1, slides, "text:1")},
		Terms: []string{"排序"}, Limit: 10}); err != nil || len(got.Passages) != 0 || got.Total != 0 || got.Length != 0 {
		t.Fatalf("a search of nothing kept: %+v %v", got, err)
	}

	slide1 := "## 投影片 1\n第三週：合併排序"
	slide2 := "## 投影片 2\n排序的複雜度：合併排序 O(n log n)"
	deck := store.SearchFile{CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "text:3", Source: store.SourceAI,
		Name: "week3.pptx", Position: 1, IndexedAt: at(0), UsedAt: at(0),
		Passages: []store.SearchPassage{passage("slide", 1, 0, slide1), passage("slide", 2, len(slide1)+1, slide2)}}
	notes := store.SearchFile{CourseID: course, DocumentID: doc1, VersionID: v1, Key: handout, Revision: "file:sha256:ab", Source: store.SourceRuntime,
		Name: "handout.pdf", Position: 2, UsedAt: at(2 * time.Hour),
		Passages: []store.SearchPassage{passage("page", 1, 0, "Merge sort splits the list in two; 合併排序")}}
	body := store.SearchFile{CourseID: course, DocumentID: doc2, VersionID: v2, Key: store.SearchBody, Revision: "body:1", Source: store.SourceBody,
		UsedAt: at(2 * time.Hour), Passages: []store.SearchPassage{passage("", 0, 0, "The exam covers sorting.")}}
	empty := store.SearchFile{CourseID: course, DocumentID: doc2, VersionID: v2, Key: slides, Revision: "file:sha256:cd", Source: store.SourceRuntime,
		UsedAt: at(2 * time.Hour)}
	elsewhere := store.SearchFile{CourseID: other, DocumentID: "d-other", VersionID: v3, Key: slides, Revision: "text:1", Source: store.SourceStaff,
		UsedAt: at(2 * time.Hour), Passages: []store.SearchPassage{passage("", 0, 0, "排序 elsewhere")}}
	for _, f := range []store.SearchFile{deck, notes, body, empty, elsewhere} {
		if err := s.PutSearchFile(ctx, f); err != nil {
			t.Fatalf("PutSearchFile %s/%s: %v", f.VersionID, f.Key, err)
		}
	}

	all := []store.SearchFileKey{key(v1, slides), key(v1, handout), key(v2, store.SearchBody), key(v2, slides), key(v3, slides), key(v3, "none")}
	got, err := s.UseSearchFiles(ctx, course, all, at(time.Hour))
	want := map[store.SearchFileKey]store.SearchFileState{key(v1, slides): {Revision: "text:3", Passages: 2},
		key(v1, handout): {Revision: "file:sha256:ab", Passages: 1}, key(v2, store.SearchBody): {Revision: "body:1", Passages: 1},
		key(v2, slides): {Revision: "file:sha256:cd"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("UseSearchFiles = %v %v, want %v (no file of another course)", got, err, want)
	}

	scope := []store.SearchFileRef{ref(v1, slides, "text:3"), ref(v1, handout, "file:sha256:ab"), ref(v2, store.SearchBody, "body:1"),
		ref(v2, slides, "file:sha256:cd"), ref(v3, slides, "text:1")}
	m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: course, Files: scope, Terms: []string{"合併", "排序", "複雜"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, p := range m.Passages {
		found = append(found, p.Key[len(p.Key)-4:]+":"+string(rune('0'+p.Seq)))
	}
	// Slide 2 holds all three, slide 1 and the handout two each, in the
	// order of their keys; another course's file is never searched.
	if want := []string{"f001:1", "f001:0", "f002:0"}; !slices.Equal(found, want) {
		t.Errorf("found %v, want %v", found, want)
	}
	if want := map[string]int{"合併": 3, "排序": 3, "複雜": 1}; !reflect.DeepEqual(m.DF, want) {
		t.Errorf("DF = %v, want %v", m.DF, want)
	}
	wantLen := int64(deck.Passages[0].Length + deck.Passages[1].Length + notes.Passages[0].Length + body.Passages[0].Length)
	if m.Total != 4 || m.Length != wantLen {
		t.Errorf("Total, Length = %d, %d; want 4, %d", m.Total, m.Length, wantLen)
	}
	p := m.Passages[0]
	if p.VersionID != v1 || p.Kind != "slide" || p.N != 2 || p.Offset != len(slide1)+1 || p.Part != 1 || p.Text != slide2 ||
		!reflect.DeepEqual(p.Terms, deck.Passages[1].Terms) || p.Length != deck.Passages[1].Length {
		t.Errorf("the passage found: %+v", p)
	}
	if m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: course, Files: scope, Terms: []string{"合併", "排序", "複雜"}, Limit: 1}); err != nil ||
		len(m.Passages) != 1 || m.Passages[0].Seq != 1 || m.Total != 4 {
		t.Errorf("a limit of 1: %+v %v", m, err)
	}

	// At another revision, or named in another course, a file is not
	// searched.
	stale := []store.SearchFileRef{ref(v1, slides, "text:2"), ref(v1, handout, "file:sha256:ab")}
	if m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: course, Files: stale, Terms: []string{"排序"}, Limit: 10}); err != nil ||
		len(m.Passages) != 1 || m.Passages[0].Key != handout || m.Total != 1 {
		t.Errorf("a revision not kept: %+v %v", m, err)
	}
	if m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: other, Files: scope, Terms: []string{"排序"}, Limit: 10}); err != nil ||
		len(m.Passages) != 1 || m.Passages[0].VersionID != v3 {
		t.Errorf("another course's search: %+v %v", m, err)
	}

	// Put again, at another revision: its passages replace the ones it had.
	again := deck
	again.Revision, again.Source, again.IndexedAt, again.UsedAt = "text:4", store.SourceStaff, time.Time{}, time.Time{}
	again.Passages = []store.SearchPassage{passage("slide", 1, 0, "## 投影片 1\n快速排序")}
	if err := s.PutSearchFile(ctx, again); err != nil {
		t.Fatal(err)
	}
	if m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: course, Files: []store.SearchFileRef{ref(v1, slides, "text:4")},
		Terms: []string{"排序", "合併"}, Limit: 10}); err != nil || len(m.Passages) != 1 || m.Passages[0].Text != again.Passages[0].Text || m.Total != 1 {
		t.Errorf("put again: %+v %v", m, err)
	}
	if m, err := s.SearchPassages(ctx, store.SearchQuery{CourseID: course, Files: []store.SearchFileRef{ref(v1, slides, "text:3")},
		Terms: []string{"排序"}, Limit: 10}); err != nil || len(m.Passages) != 0 {
		t.Errorf("the revision replaced: %+v %v", m, err)
	}

	// Purged when unused since a time: the deck, put again and so used at
	// the store's now, stays; the others, used at two hours, stay until a
	// time past it, and the body, used again at five, past that.
	if n, err := s.PurgeSearchFiles(ctx, at(90*time.Minute)); err != nil || n != 0 {
		t.Errorf("PurgeSearchFiles before an hour and a half: %d %v (each was used at two hours or later)", n, err)
	}
	if _, err := s.UseSearchFiles(ctx, course, []store.SearchFileKey{key(v2, store.SearchBody)}, at(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeSearchFiles(ctx, at(3*time.Hour)); err != nil || n != 3 {
		t.Errorf("PurgeSearchFiles before three hours: %d %v, want the handout, the empty file and the other course's", n, err)
	}
	if got, err := s.UseSearchFiles(ctx, course, all, time.Time{}); err != nil || len(got) != 2 || got[key(v1, slides)].Revision != "text:4" ||
		got[key(v1, slides)].Passages != 1 || got[key(v2, store.SearchBody)].Revision != "body:1" {
		t.Errorf("after the purge: %v %v", got, err)
	}

	// Dropped by version, and by document.
	for _, f := range []store.SearchFile{notes, empty} {
		if err := s.PutSearchFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DropSearchVersions(ctx, []string{v1, "no-such-version"}); err != nil || n != 2 {
		t.Errorf("DropSearchVersions: %d %v, want the deck and the handout", n, err)
	}
	if n, err := s.DropSearchDocuments(ctx, []string{doc2}); err != nil || n != 2 {
		t.Errorf("DropSearchDocuments: %d %v, want the body and the empty file", n, err)
	}
	if got, err := s.UseSearchFiles(ctx, course, all, time.Time{}); err != nil || len(got) != 0 {
		t.Errorf("after the drops: %v %v", got, err)
	}
	if n, err := s.DropSearchVersions(ctx, nil); err != nil || n != 0 {
		t.Errorf("DropSearchVersions of none: %d %v", n, err)
	}

	for name, f := range map[string]store.SearchFile{
		"no course":    {DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: store.SourceAI},
		"no revision":  {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Source: store.SourceAI},
		"a source":     {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: "model"},
		"out of order": {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: store.SourceAI, Passages: []store.SearchPassage{passage("", 0, 5, "a"), passage("", 0, 5, "b")}},
		"no part":      {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: store.SourceAI, Passages: []store.SearchPassage{{Text: "a", Terms: []string{"a"}, Length: 1}}},
		"unsorted":     {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: store.SourceAI, Passages: []store.SearchPassage{{Part: 1, Text: "b a", Terms: []string{"b", "a"}, Length: 2}}},
		"NUL":          {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: store.SourceAI, Passages: []store.SearchPassage{passage("", 0, 0, "a\x00b")}},
		"too long":     {CourseID: course, DocumentID: doc1, VersionID: v1, Key: slides, Revision: "r", Source: store.SourceAI, Passages: []store.SearchPassage{passage("", 0, 0, strings.Repeat("a ", store.MaxSearchText/2+1))}},
	} {
		if err := s.PutSearchFile(ctx, f); err == nil {
			t.Errorf("%s: kept", name)
		}
	}
	if got, err := s.UseSearchFiles(ctx, course, []store.SearchFileKey{key(v1, slides)}, time.Time{}); err != nil || len(got) != 0 {
		t.Errorf("a refused file was kept: %v %v", got, err)
	}
}
