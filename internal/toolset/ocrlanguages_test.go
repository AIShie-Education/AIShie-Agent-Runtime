package toolset

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
)

// langOCR is a fakeOCR that says which languages it recognizes in, as
// *ocr.Service does, which a test changes as the site's setting would.
type langOCR struct {
	*fakeOCR
	mu    sync.Mutex
	langs string
}

func (l *langOCR) Languages() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.langs
}

func (l *langOCR) set(langs, off string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.langs = langs
	l.off = off
}

// TestOCRTextOfOtherLanguagesIsNotGiven: a text OCR recognized is kept for
// the next question, in the languages it was recognized in; once OCR is in
// others, OCR is asked again, and once it is turned off, the text kept is
// not given at all.
func TestOCRTextOfOtherLanguagesIsNotGiven(t *testing.T) {
	srv, _, _ := swapServer(t, scanned)
	o := &langOCR{langs: "chi_sim+chi_tra+eng", fakeOCR: &fakeOCR{respond: func(n int, _ func(context.Context) ([]byte, error)) ocr.State {
		if n == 1 {
			return done("## Page 1\n期中考試範圍")
		}
		return done("## Page 1\nThe midterm's scope")
	}}}
	r := Runner{Client: core.NewClient(versionedCore(srv.URL+"/scan", "application/pdf", len(scanned))),
		Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0), OCR: o}
	read := func() (map[string]any, string) {
		t.Helper()
		recs, text := readAll(t, r, `{"document_id":"`+docID+`"}`)
		return recs[0], text
	}
	if _, text := read(); !strings.Contains(text, "期中考試範圍") || o.times() != 1 {
		t.Fatalf("first: %q, OCR asked %d times", text, o.times())
	}
	if _, text := read(); !strings.Contains(text, "期中考試範圍") || o.times() != 1 {
		t.Errorf("again, kept: %q, OCR asked %d times", text, o.times())
	}
	o.set("eng", "")
	if _, text := read(); !strings.Contains(text, "The midterm's scope") || o.times() != 2 {
		t.Errorf("in other languages: %q, OCR asked %d times", text, o.times())
	}
	o.set("eng", "it is turned off")
	rec, text := read()
	if text != "" || rec["ocr"] != OCRUnavailable || !strings.Contains(rec["note"].(string), "it is turned off") || o.times() != 2 {
		t.Errorf("turned off: %q %v, OCR asked %d times", text, rec, o.times())
	}
}
