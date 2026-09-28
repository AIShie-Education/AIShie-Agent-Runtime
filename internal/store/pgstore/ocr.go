package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// OCRText is the text kept under sum; store.ErrNotFound when there is
// none.
func (s *Store) OCRText(ctx context.Context, sum string) (store.OCRText, error) {
	var t store.OCRText
	var sections, notes []byte
	err := s.pool.QueryRow(ctx, `
		SELECT sum, status, kind, text, pages, pages_of, sections, notes, reason, engine, duration_ms, created_at
		  FROM ocr_text WHERE sum = $1`, sum).
		Scan(&t.Sum, &t.Status, &t.Kind, &t.Text, &t.Pages, &t.PagesOf, &sections, &notes, &t.Reason, &t.Engine, &t.DurationMS, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OCRText{}, store.ErrNotFound
	}
	if err != nil {
		return store.OCRText{}, fmt.Errorf("store: OCR text: %w", err)
	}
	if err := json.Unmarshal(sections, &t.Sections); err != nil {
		return store.OCRText{}, fmt.Errorf("store: OCR text: its sections: %w", err)
	}
	if err := json.Unmarshal(notes, &t.Notes); err != nil {
		return store.OCRText{}, fmt.Errorf("store: OCR text: its notes: %w", err)
	}
	utc(&t.CreatedAt)
	return t, nil
}

// PutOCRText keeps t under t.Sum, in place of what was there.
func (s *Store) PutOCRText(ctx context.Context, t store.OCRText) error {
	if err := store.CheckOCRText(t); err != nil {
		return err
	}
	sections, err := json.Marshal(nonNil(t.Sections))
	if err != nil {
		return err
	}
	notes, err := json.Marshal(nonNil(t.Notes))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO ocr_text (sum, status, kind, text, pages, pages_of, sections, notes, reason, engine, duration_ms, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11, COALESCE($12::timestamptz, now()))
		ON CONFLICT (sum) DO UPDATE SET status = EXCLUDED.status, kind = EXCLUDED.kind, text = EXCLUDED.text,
		       pages = EXCLUDED.pages, pages_of = EXCLUDED.pages_of, sections = EXCLUDED.sections, notes = EXCLUDED.notes,
		       reason = EXCLUDED.reason, engine = EXCLUDED.engine, duration_ms = EXCLUDED.duration_ms,
		       created_at = EXCLUDED.created_at`,
		t.Sum, t.Status, t.Kind, t.Text, t.Pages, t.PagesOf, string(sections), string(notes), t.Reason, t.Engine, t.DurationMS,
		orNow(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: put OCR text: %w", err)
	}
	return nil
}

// PurgeOCRTexts destroys the texts kept before doneBefore and the failures
// kept before failedBefore.
func (s *Store) PurgeOCRTexts(ctx context.Context, doneBefore, failedBefore time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM ocr_text
		 WHERE (status = 'done' AND created_at < $1) OR (status = 'failed' AND created_at < $2)`, doneBefore, failedBefore)
	if err != nil {
		return 0, fmt.Errorf("store: purge OCR texts: %w", err)
	}
	return tag.RowsAffected(), nil
}

// nonNil is s, or an empty slice for nil, which JSON writes as [].
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
