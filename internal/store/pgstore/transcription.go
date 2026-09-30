package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// serviceColumns are what scanService reads of the credential, in its
// order.
const serviceColumns = `secret_id, hint, credential_id, tested, set_by, set_at, last_ok_at, rejected_at, last_error`

func scanService(row pgx.Row) (*store.TranscriptionCredential, error) {
	var c store.TranscriptionCredential
	if err := row.Scan(&c.SecretID, &c.Hint, &c.CredentialID, &c.Tested, &c.SetBy, &c.SetAt, &c.LastOKAt, &c.RejectedAt,
		&c.LastError); err != nil {
		return nil, err
	}
	utc(&c.SetAt)
	for _, t := range []*time.Time{c.LastOKAt, c.RejectedAt} {
		if t != nil {
			utc(t)
		}
	}
	return &c, nil
}

// TranscriptionCredential is the credential, or store.ErrNotFound.
func (s *Store) TranscriptionCredential(ctx context.Context) (*store.TranscriptionCredential, error) {
	c, err := scanService(s.pool.QueryRow(ctx, `SELECT `+serviceColumns+` FROM transcription_credential`))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("the transcription credential: %w", store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: the transcription credential: %w", err)
	}
	return c, nil
}

// PutTranscriptionCredential keeps c with its token in place of the one
// before, whose secret goes in the same transaction.
func (s *Store) PutTranscriptionCredential(ctx context.Context, c store.TranscriptionCredential, token store.Secret) error {
	if err := store.CheckTranscriptionCredential(c, token); err != nil {
		return err
	}
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var old string
		err := tx.QueryRow(ctx, `SELECT secret_id FROM transcription_credential FOR UPDATE`).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := insertSecret(ctx, tx, token); err != nil {
			return err
		}
		if old != "" {
			if _, err := tx.Exec(ctx, `DELETE FROM transcription_credential`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM secret WHERE id = $1`, old); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO transcription_credential (secret_id, hint, credential_id, tested, set_by, set_at, last_error)
			VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), '')`,
			c.SecretID, c.Hint, c.CredentialID, c.Tested, c.SetBy, orNow(c.SetAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("store: put the transcription credential: %w", err)
	}
	return nil
}

// DeleteTranscriptionCredential forgets the credential and destroys its
// secret. None is rolled back, so that the trigger moves nothing on.
func (s *Store) DeleteTranscriptionCredential(ctx context.Context) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var old string
		err := tx.QueryRow(ctx, `DELETE FROM transcription_credential RETURNING secret_id`).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNothingDeleted
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM secret WHERE id = $1`, old)
		return err
	})
	if errors.Is(err, errNothingDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: delete the transcription credential: %w", err)
	}
	return nil
}

// NoteTranscriptionCredential records what Core made of the credential
// secretID, if it is still the one kept. It writes no column the
// registry's trigger watches.
func (s *Store) NoteTranscriptionCredential(ctx context.Context, secretID string, ok bool, at time.Time, why string) error {
	q, args := `UPDATE transcription_credential SET last_ok_at = COALESCE($2::timestamptz, now()), rejected_at = NULL, last_error = ''
	             WHERE secret_id = $1`, []any{secretID, orNow(at)}
	if !ok {
		if why == "" {
			why = "refused"
		}
		q, args = `UPDATE transcription_credential SET rejected_at = COALESCE($2::timestamptz, now()), last_error = $3
		            WHERE secret_id = $1`, append(args, why)
	}
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: note the transcription credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("the transcription credential %s: %w", secretID, store.ErrNotFound)
	}
	return nil
}

// jobColumns are what scanJob reads, in its order.
const jobColumns = `id, seq, version_id, document_id, course_id, lease_id, status, reason, backfill, attempt, content_type, byte_size,
	pages, pages_sent, offer, model, model_calls, cost_pusd, input_tokens, output_tokens, worker, started_at, heartbeat_at, finished_at`

func scanJob(row pgx.Row) (*store.TranscriptionJob, error) {
	var j store.TranscriptionJob
	if err := row.Scan(&j.ID, &j.Seq, &j.VersionID, &j.DocumentID, &j.CourseID, &j.LeaseID, &j.Status, &j.Reason, &j.Backfill,
		&j.Attempt, &j.ContentType, &j.ByteSize, &j.Pages, &j.PagesSent, &j.Offer, &j.Model, &j.ModelCalls, &j.CostPUSD,
		&j.InputTokens, &j.OutputTokens, &j.Worker, &j.StartedAt, &j.HeartbeatAt, &j.FinishedAt); err != nil {
		return nil, err
	}
	utc(&j.StartedAt)
	utc(&j.HeartbeatAt)
	if j.FinishedAt != nil {
		utc(j.FinishedAt)
	}
	return &j, nil
}

// PutTranscriptionJob keeps j in place of the job of its id, which keeps
// its place; a new one takes the next.
func (s *Store) PutTranscriptionJob(ctx context.Context, j store.TranscriptionJob) (*store.TranscriptionJob, error) {
	if err := store.CheckTranscriptionJob(j); err != nil {
		return nil, err
	}
	out, err := scanJob(s.pool.QueryRow(ctx, `
		INSERT INTO transcription_job (id, version_id, document_id, course_id, lease_id, status, reason, backfill, attempt,
		                               content_type, byte_size, pages, pages_sent, offer, model, model_calls, cost_pusd,
		                               input_tokens, output_tokens, worker, started_at, heartbeat_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
		        COALESCE($21::timestamptz, now()), COALESCE($22::timestamptz, now()), $23)
		ON CONFLICT (id) DO UPDATE
		   SET version_id = EXCLUDED.version_id, document_id = EXCLUDED.document_id, course_id = EXCLUDED.course_id,
		       lease_id = EXCLUDED.lease_id, status = EXCLUDED.status, reason = EXCLUDED.reason, backfill = EXCLUDED.backfill,
		       attempt = EXCLUDED.attempt, content_type = EXCLUDED.content_type, byte_size = EXCLUDED.byte_size,
		       pages = EXCLUDED.pages, pages_sent = EXCLUDED.pages_sent, offer = EXCLUDED.offer, model = EXCLUDED.model,
		       model_calls = EXCLUDED.model_calls, cost_pusd = EXCLUDED.cost_pusd, input_tokens = EXCLUDED.input_tokens,
		       output_tokens = EXCLUDED.output_tokens, worker = EXCLUDED.worker, started_at = EXCLUDED.started_at,
		       heartbeat_at = EXCLUDED.heartbeat_at, finished_at = EXCLUDED.finished_at
		RETURNING `+jobColumns,
		j.ID, j.VersionID, j.DocumentID, j.CourseID, j.LeaseID, j.Status, j.Reason, j.Backfill, j.Attempt, j.ContentType,
		j.ByteSize, j.Pages, j.PagesSent, j.Offer, j.Model, j.ModelCalls, j.CostPUSD, j.InputTokens, j.OutputTokens, j.Worker,
		orNow(j.StartedAt), orNow(j.HeartbeatAt), j.FinishedAt))
	if err != nil {
		return nil, fmt.Errorf("store: put transcription job %s: %w", j.ID, err)
	}
	return out, nil
}

// TranscriptionJobs lists the jobs as q says, newest first.
func (s *Store) TranscriptionJobs(ctx context.Context, q store.JobQuery) ([]store.TranscriptionJob, error) {
	if err := store.CheckJobQuery(q); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+jobColumns+` FROM transcription_job
		 WHERE ($1::text = '' OR status = $1::text) AND ($2::bigint = 0 OR seq < $2::bigint)
		 ORDER BY seq DESC
		 LIMIT $3`, q.Status, q.Before, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: transcription jobs: %w", err)
	}
	defer rows.Close()
	var out []store.TranscriptionJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: transcription jobs: %w", err)
		}
		out = append(out, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: transcription jobs: %w", err)
	}
	return out, nil
}

// TranscriptionDay sums the jobs begun since since.
func (s *Store) TranscriptionDay(ctx context.Context, since time.Time) (store.TranscriptionDay, error) {
	var d store.TranscriptionDay
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(pages_sent), 0)::int, (count(*) FILTER (WHERE status = 'done'))::int,
		       (count(*) FILTER (WHERE status = 'failed'))::int, (count(*) FILTER (WHERE status = 'skipped'))::int,
		       COALESCE(sum(cost_pusd), 0)::bigint
		  FROM transcription_job WHERE started_at >= $1`, since).Scan(&d.Pages, &d.Documents, &d.Failed, &d.Skipped, &d.CostPUSD)
	if err != nil {
		return d, fmt.Errorf("store: the transcriber's day: %w", err)
	}
	return d, nil
}

// InterruptTranscriptionJobs ends the jobs still working whose claim was
// last held before before.
func (s *Store) InterruptTranscriptionJobs(ctx context.Context, before, at time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE transcription_job SET status = 'dropped', reason = $3, finished_at = COALESCE($2::timestamptz, now())
		 WHERE status = 'working' AND heartbeat_at < $1`, before, orNow(at), store.ReasonInterrupted)
	if err != nil {
		return 0, fmt.Errorf("store: interrupt transcription jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneTranscriptionJobs destroys the jobs begun before before.
func (s *Store) PruneTranscriptionJobs(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM transcription_job WHERE started_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: prune transcription jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}
