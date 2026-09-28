package storetest

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// testOCRTexts: a text is kept by its file's sum as it was put, sections
// and notes whole, a zero time the store's now; putting one again replaces
// it; a sum never put is ErrNotFound; what a store must not keep is
// refused; purging destroys the texts kept before one time and the
// failures before another, and says how many.
func testOCRTexts(t *testing.T, open Opener) {
	s := open(t)
	ctx := t.Context()
	sum := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	if _, err := s.OCRText(ctx, sum("a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a sum never put: %v", err)
	}
	text := "## Page 1\n期中考試範圍：第一章到第五章\n\n## Page 2\n[no text found on this page]"
	done := store.OCRText{Sum: sum("a"), Status: store.OCRDone, Kind: store.OCRPDF, Text: text, Pages: 2, PagesOf: 3,
		Sections: []store.OCRSection{{N: 1, Offset: 0}, {N: 2, Offset: strings.Index(text, "## Page 2")}},
		Notes:    []string{"only its first 2 pages of 3 were recognized"}, Engine: "tesseract 5.3.4 chi_sim+chi_tra+eng 300dpi",
		DurationMS: 4200, CreatedAt: at(time.Hour)}
	before := time.Now()
	failed := store.OCRText{Sum: sum("b"), Status: store.OCRFailed, Kind: store.OCRImage, Reason: "timeout"}
	for _, x := range []store.OCRText{done, failed} {
		if err := s.PutOCRText(ctx, x); err != nil {
			t.Fatalf("PutOCRText: %v", err)
		}
	}
	after := time.Now()
	got, err := s.OCRText(ctx, sum("a"))
	if err != nil {
		t.Fatal(err)
	}
	sameTime(t, "created_at", got.CreatedAt, done.CreatedAt)
	got.CreatedAt = done.CreatedAt
	if !reflect.DeepEqual(got, done) {
		t.Errorf("kept %+v\nwant %+v", got, done)
	}
	gotFailed, err := s.OCRText(ctx, sum("b"))
	if err != nil || gotFailed.Status != store.OCRFailed || gotFailed.Reason != "timeout" || gotFailed.Text != "" {
		t.Errorf("the failure: %+v %v", gotFailed, err)
	}
	recent(t, "a zero time", gotFailed.CreatedAt, before, after)

	// Put again: replaced.
	again := done
	again.Text, again.Sections, again.Notes, again.Pages = "## Page 1\nagain", []store.OCRSection{{N: 1, Offset: 0}}, nil, 1
	if err := s.PutOCRText(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, err := s.OCRText(ctx, sum("a")); err != nil || got.Text != again.Text || got.Pages != 1 || len(got.Sections) != 1 || len(got.Notes) != 0 {
		t.Errorf("put again: %+v %v", got, err)
	}

	for _, bad := range []store.OCRText{
		{Sum: "md5:abc", Status: store.OCRDone, Kind: store.OCRPDF},
		{Sum: sum("c"), Status: "running", Kind: store.OCRPDF},
		{Sum: sum("c"), Status: store.OCRDone, Kind: "docx"},
		{Sum: sum("c"), Status: store.OCRDone, Kind: store.OCRPDF, Text: "a\x00b"},
		{Sum: sum("c"), Status: store.OCRDone, Kind: store.OCRPDF, Text: "\xff"},
		{Sum: sum("c"), Status: store.OCRDone, Kind: store.OCRPDF, Text: "short", Sections: []store.OCRSection{{N: 1, Offset: 99}}},
		{Sum: sum("c"), Status: store.OCRDone, Kind: store.OCRPDF, Text: "short", Sections: []store.OCRSection{{N: 1, Offset: 3}, {N: 2, Offset: 1}}},
	} {
		if err := s.PutOCRText(ctx, bad); err == nil {
			t.Errorf("%+v was kept", bad)
		}
	}

	// Purge: the text kept at base+1h goes with doneBefore past it; the
	// failure, kept now, only with failedBefore past now.
	n, err := s.PurgeOCRTexts(ctx, at(time.Minute), time.Now().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Errorf("a purge before both: %d %v", n, err)
	}
	n, err = s.PurgeOCRTexts(ctx, at(2*time.Hour), time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Errorf("the text's purge: %d %v", n, err)
	}
	if _, err := s.OCRText(ctx, sum("a")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the text purged is still there: %v", err)
	}
	n, err = s.PurgeOCRTexts(ctx, at(2*time.Hour), time.Now().Add(clockSlack))
	if err != nil || n != 1 {
		t.Errorf("the failure's purge: %d %v", n, err)
	}
}
