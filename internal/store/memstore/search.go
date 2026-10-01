package memstore

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// UseSearchFiles is what is kept of the files of keys in the course, of
// those kept; it marks them needed at at.
func (s *Store) UseSearchFiles(_ context.Context, courseID string, keys []store.SearchFileKey, at time.Time) (map[store.SearchFileKey]store.SearchFileState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at = s.orNow(at)
	out := map[store.SearchFileKey]store.SearchFileState{}
	for _, k := range keys {
		f, ok := s.search[k]
		if !ok || f.CourseID != courseID {
			continue
		}
		out[k] = store.SearchFileState{Revision: f.Revision, Passages: len(f.Passages)}
		if f.UsedAt.Before(at) {
			f.UsedAt = at
			s.search[k] = f
		}
	}
	return out, nil
}

// PutSearchFile keeps f, its passages in place of any its file had.
func (s *Store) PutSearchFile(_ context.Context, f store.SearchFile) error {
	if err := store.CheckSearchFile(f); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f.IndexedAt, f.UsedAt = s.orNow(f.IndexedAt), s.orNow(f.UsedAt)
	f.Passages = slices.Clone(f.Passages)
	for i := range f.Passages {
		f.Passages[i].Terms = slices.Clone(f.Passages[i].Terms)
	}
	s.search[store.SearchFileKey{VersionID: f.VersionID, Key: f.Key}] = f
	return nil
}

// SearchPassages are the passages of q.Files, each at its revision, that
// hold any of q.Terms, those that hold the most of them first, then by
// version, key and place; with what they are scored against.
func (s *Store) SearchPassages(_ context.Context, q store.SearchQuery) (store.SearchMatches, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := store.SearchMatches{DF: map[string]int{}}
	want := map[string]bool{}
	for _, t := range q.Terms {
		want[t] = true
	}
	type found struct {
		m       store.SearchMatch
		matched int
	}
	var all []found
	seen := map[store.SearchFileKey]bool{}
	for _, ref := range q.Files {
		f, ok := s.search[ref.SearchFileKey]
		if !ok || seen[ref.SearchFileKey] || f.CourseID != q.CourseID || f.Revision != ref.Revision {
			continue
		}
		seen[ref.SearchFileKey] = true
		for i, p := range f.Passages {
			out.Total++
			out.Length += int64(p.Length)
			matched := 0
			for _, t := range p.Terms {
				if want[t] {
					matched++
					out.DF[t]++
				}
			}
			if matched > 0 {
				p.Terms = slices.Clone(p.Terms)
				all = append(all, found{store.SearchMatch{SearchFileKey: ref.SearchFileKey, Seq: i, SearchPassage: p}, matched})
			}
		}
	}
	slices.SortFunc(all, func(a, b found) int {
		return cmp.Or(cmp.Compare(b.matched, a.matched), cmp.Compare(a.m.VersionID, b.m.VersionID), cmp.Compare(a.m.Key, b.m.Key),
			cmp.Compare(a.m.Seq, b.m.Seq))
	})
	for _, f := range all {
		if len(out.Passages) >= q.Limit {
			break
		}
		out.Passages = append(out.Passages, f.m)
	}
	return out, nil
}

// DropSearchVersions destroys what is kept of the versions named.
func (s *Store) DropSearchVersions(_ context.Context, versionIDs []string) (int64, error) {
	return s.dropSearch(func(f store.SearchFile) bool { return slices.Contains(versionIDs, f.VersionID) }), nil
}

// DropSearchDocuments destroys what is kept of every version of the
// documents named.
func (s *Store) DropSearchDocuments(_ context.Context, documentIDs []string) (int64, error) {
	return s.dropSearch(func(f store.SearchFile) bool { return slices.Contains(documentIDs, f.DocumentID) }), nil
}

// PurgeSearchFiles destroys the files no search has needed since before.
func (s *Store) PurgeSearchFiles(_ context.Context, before time.Time) (int64, error) {
	return s.dropSearch(func(f store.SearchFile) bool { return f.UsedAt.Before(before) }), nil
}

func (s *Store) dropSearch(drop func(store.SearchFile) bool) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, f := range s.search {
		if drop(f) {
			delete(s.search, k)
			n++
		}
	}
	return n
}
