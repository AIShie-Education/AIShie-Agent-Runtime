package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// SearchFile is one file of a version of a course's document as the
// runtime's search of the course's materials keeps it (docs/design.md §4,
// Search): the text a model reading it is given, cut into passages, each
// with its terms (package search). It is kept per course and version, by
// the file's key in its version, at the revision of the text it was read
// at; built when a search first needs it, read again when a reader is
// shown another revision, and dropped when its version is purged, or when
// no search has needed it for a while. Which seat may read it is never
// kept here: every search asks Core, as the asking seat, which versions
// and files it may read, and only those are searched.
type SearchFile struct {
	CourseID, DocumentID, VersionID string
	// Key names the file in its version: its id, or SearchBody for the
	// version's own text.
	Key string
	// Revision is what the text was read at, as the runtime names it: the
	// text version's revision, the file's checksum, the body's sum.
	Revision string
	// Source is whose the text is: SourceStaff or SourceAI for Core's text
	// version, SourceRuntime for the runtime's own reading of the file,
	// SourceBody for the version's own text.
	Source string
	// Name and Position are the file's name and place in its version.
	Name     string
	Position int
	// Passages are the text's, in order; none for a file with no text to
	// search, which is kept so that it is not read at every search.
	Passages []SearchPassage
	// IndexedAt is when it was read; UsedAt when a search last needed it.
	// A zero time is the store's now.
	IndexedAt, UsedAt time.Time
}

// SearchPassage is one passage of a SearchFile's text.
type SearchPassage struct {
	// Kind and N are the slide, page or sheet it is on: "slide" 3; "" and
	// 0 for none.
	Kind string
	N    int
	// Offset is where it begins in the file's text, and Part the part of
	// that text document_get gives it in, from 1.
	Offset, Part int
	Text         string
	// Terms are its terms once each, sorted bytewise, and Length how many
	// it has, repeats counted.
	Terms  []string
	Length int
}

// SearchFileKey names a SearchFile: its version and its key there.
type SearchFileKey struct {
	VersionID, Key string
}

// SearchFileRef is a SearchFile at a revision: what a search may read.
type SearchFileRef struct {
	SearchFileKey
	Revision string
}

// SearchFileState is what is kept of a file: the revision it was read at,
// and how many passages it has (none: no text to search).
type SearchFileState struct {
	Revision string
	Passages int
}

// SearchQuery is a search of a course's files: the passages of Files, at
// their revisions, that hold any of Terms.
type SearchQuery struct {
	CourseID string
	Files    []SearchFileRef
	Terms    []string
	// Limit bounds the passages returned: those that hold the most of
	// Terms, and then by version, key and place.
	Limit int
}

// SearchMatches are what a SearchQuery found.
type SearchMatches struct {
	Passages []SearchMatch
	// DF is how many passages of the files searched hold each term, and
	// Total how many passages they have in all, and Length their terms in
	// all, repeats counted: what a passage's score is reckoned against.
	DF     map[string]int
	Total  int
	Length int64
}

// SearchMatch is one passage found: its file, its place among the file's
// passages, from 0, and the passage.
type SearchMatch struct {
	SearchFileKey
	Seq int
	SearchPassage
}

// Sources of a SearchFile's text.
const (
	SourceStaff   = "staff"
	SourceAI      = "ai"
	SourceRuntime = "runtime"
	SourceBody    = "body"
)

// SearchBody is the key of a version's own text, which no file's id is.
const SearchBody = "body"

// Bounds of a SearchFile: twice the 2 MiB of text Core keeps of a file's
// text version, and the runtime reads of a file.
const (
	MaxSearchText     = 4 << 20
	MaxSearchPassages = 20000
	MaxSearchTerms    = 4096
)

// CheckSearchFile refuses a SearchFile a store must not keep: without its
// course, document, version, key or revision, of a source of no known
// value, of passages out of order, past the bounds, or holding text that
// is not valid UTF-8 or holds NUL.
func CheckSearchFile(f SearchFile) error {
	switch {
	case f.CourseID == "" || f.DocumentID == "" || f.VersionID == "" || f.Key == "" || f.Revision == "":
		return errors.New("store: search file: course, document, version, key and revision required")
	case f.Source != SourceStaff && f.Source != SourceAI && f.Source != SourceRuntime && f.Source != SourceBody:
		return fmt.Errorf("store: search file: source %q is not staff, ai, runtime or body", f.Source)
	case len(f.Passages) > MaxSearchPassages:
		return errors.New("store: search file: more passages than MaxSearchPassages")
	case !validText(f.Name) || f.Position < 0:
		return errors.New("store: search file: a name that is not valid UTF-8 without NUL, or a negative position")
	}
	size, last := 0, -1
	for _, p := range f.Passages {
		size += len(p.Text)
		switch {
		case p.Offset <= last || p.Part < 1 || p.N < 0 || p.Length < len(p.Terms) || len(p.Terms) > MaxSearchTerms:
			return errors.New("store: search file: a passage out of order, of no part, or counting fewer terms than it has")
		case !validText(p.Text) || !validText(p.Kind):
			return errors.New("store: search file: a passage that is not valid UTF-8 without NUL")
		case !slices.IsSorted(p.Terms) || len(slices.Compact(slices.Clone(p.Terms))) != len(p.Terms):
			return errors.New("store: search file: a passage's terms are not distinct and sorted")
		case slices.ContainsFunc(p.Terms, func(t string) bool { return t == "" || !validText(t) }):
			return errors.New("store: search file: an empty term, or one that is not valid UTF-8 without NUL")
		}
		last = p.Offset
	}
	if size > MaxSearchText {
		return errors.New("store: search file: the text is past MaxSearchText")
	}
	return nil
}

func validText(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// SearchIndex keeps the runtime's search of courses' materials: their
// files' passages, by course and version, shared by every agent and seat
// in the course, each search narrowed to the files its seat may read.
type SearchIndex interface {
	// UseSearchFiles is what is kept of the files of keys in the course,
	// of those kept; it marks them needed at at, so that they are not
	// purged while searches need them.
	UseSearchFiles(ctx context.Context, courseID string, keys []SearchFileKey, at time.Time) (map[SearchFileKey]SearchFileState, error)
	// PutSearchFile keeps f, its passages in place of any its file had.
	PutSearchFile(ctx context.Context, f SearchFile) error
	// SearchPassages are the passages of q.Files, each at its revision,
	// that hold any of q.Terms, and what they are scored against.
	SearchPassages(ctx context.Context, q SearchQuery) (SearchMatches, error)
	// DropSearchVersions and DropSearchDocuments destroy what is kept of
	// the versions, or of every version of the documents, named: a
	// version or a document purged in Core. They say how many files they
	// destroyed.
	DropSearchVersions(ctx context.Context, versionIDs []string) (int64, error)
	DropSearchDocuments(ctx context.Context, documentIDs []string) (int64, error)
	// PurgeSearchFiles destroys the files no search has needed since
	// before, and says how many.
	PurgeSearchFiles(ctx context.Context, before time.Time) (int64, error)
}
