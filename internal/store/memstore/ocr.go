package memstore

import (
	"context"
	"slices"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// OCRText is the text kept under sum; store.ErrNotFound when there is
// none.
func (s *Store) OCRText(_ context.Context, sum string) (store.OCRText, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ocr[sum]
	if !ok {
		return store.OCRText{}, store.ErrNotFound
	}
	return cloneOCR(t), nil
}

// PutOCRText keeps t under t.Sum, in place of what was there.
func (s *Store) PutOCRText(_ context.Context, t store.OCRText) error {
	if err := store.CheckOCRText(t); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t = cloneOCR(t)
	t.CreatedAt = s.orNow(t.CreatedAt)
	s.ocr[t.Sum] = t
	return nil
}

// PurgeOCRTexts destroys the texts kept before doneBefore and the failures
// kept before failedBefore.
func (s *Store) PurgeOCRTexts(_ context.Context, doneBefore, failedBefore time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for sum, t := range s.ocr {
		before := doneBefore
		if t.Status == store.OCRFailed {
			before = failedBefore
		}
		if t.CreatedAt.Before(before) {
			delete(s.ocr, sum)
			n++
		}
	}
	return n, nil
}

func cloneOCR(t store.OCRText) store.OCRText {
	t.Sections, t.Notes = slices.Clone(t.Sections), slices.Clone(t.Notes)
	return t
}
