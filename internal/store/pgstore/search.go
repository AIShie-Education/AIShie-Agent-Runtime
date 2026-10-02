package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// The search of courses' materials (migration 0014): a file's passages,
// by course and version, and the terms each is found by, under a GIN
// index of text[], which every PostgreSQL has and no locale changes.

// UseSearchFiles is what is kept of the files of keys in the course, of
// those kept; it marks them needed at at, those last marked before
// store.SearchUseGrain before it: a search rewrites a file's row once a
// day at most, not at every search.
func (s *Store) UseSearchFiles(ctx context.Context, courseID string, keys []store.SearchFileKey, at time.Time) (map[store.SearchFileKey]store.SearchFileState, error) {
	versions, names := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		versions[i], names[i] = k.VersionID, k.Key
	}
	rows, err := s.pool.Query(ctx, `
		WITH k AS (SELECT DISTINCT * FROM unnest($2::text[], $3::text[]) AS k(version_id, file_key)),
		     used AS (UPDATE search_file f SET used_at = COALESCE($4::timestamptz, now())
		                FROM k
		               WHERE f.course_id = $1 AND f.version_id = k.version_id AND f.file_key = k.file_key
		                 AND f.used_at < COALESCE($4::timestamptz, now()) - $5::interval
		              RETURNING f.version_id)
		SELECT f.version_id, f.file_key, f.revision, f.passages
		  FROM search_file f JOIN k USING (version_id, file_key)
		 WHERE f.course_id = $1`, courseID, versions, names, orNow(at), store.SearchUseGrain)
	if err != nil {
		return nil, fmt.Errorf("store: use search files: %w", err)
	}
	out := map[store.SearchFileKey]store.SearchFileState{}
	var k store.SearchFileKey
	var st store.SearchFileState
	_, err = pgx.ForEachRow(rows, []any{&k.VersionID, &k.Key, &st.Revision, &st.Passages}, func() error {
		out[k] = st
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: use search files: %w", err)
	}
	return out, nil
}

// PutSearchFile keeps f, its passages in place of any its file had.
func (s *Store) PutSearchFile(ctx context.Context, f store.SearchFile) error {
	if err := store.CheckSearchFile(f); err != nil {
		return err
	}
	var length int64
	for _, p := range f.Passages {
		length += int64(p.Length)
	}
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_file (version_id, file_key, course_id, document_id, revision, source, name, position, passages, length,
			                         indexed_at, used_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11::timestamptz, now()), COALESCE($12::timestamptz, now()))
			ON CONFLICT (version_id, file_key) DO UPDATE SET course_id = EXCLUDED.course_id, document_id = EXCLUDED.document_id,
			       revision = EXCLUDED.revision, source = EXCLUDED.source, name = EXCLUDED.name, position = EXCLUDED.position,
			       passages = EXCLUDED.passages, length = EXCLUDED.length, indexed_at = EXCLUDED.indexed_at, used_at = EXCLUDED.used_at`,
			f.VersionID, f.Key, f.CourseID, f.DocumentID, f.Revision, f.Source, f.Name, f.Position, len(f.Passages), length,
			orNow(f.IndexedAt), orNow(f.UsedAt))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM search_passage WHERE version_id = $1 AND file_key = $2`, f.VersionID, f.Key); err != nil {
			return err
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"search_passage"},
			[]string{"version_id", "file_key", "seq", "section_kind", "section_n", "start_offset", "part", "text", "terms", "length"},
			pgx.CopyFromSlice(len(f.Passages), func(i int) ([]any, error) {
				p := f.Passages[i]
				return []any{f.VersionID, f.Key, i, p.Kind, p.N, p.Offset, p.Part, p.Text, nonNil(p.Terms), p.Length}, nil
			}))
		return err
	})
	if err != nil {
		return fmt.Errorf("store: put search file: %w", err)
	}
	return nil
}

// searchScope are the files of a SearchQuery, each at its revision, as a
// CTE: the course's, by $1, and $2, $3 and $4 the files' versions, keys
// and revisions.
const searchScope = `
	WITH scope AS (
		SELECT DISTINCT f.version_id, f.file_key, f.passages, f.length
		  FROM search_file f
		  JOIN unnest($2::text[], $3::text[], $4::text[]) AS r(version_id, file_key, revision)
		    ON f.version_id = r.version_id AND f.file_key = r.file_key AND f.revision = r.revision
		 WHERE f.course_id = $1)`

// SearchPassages are the passages of q.Files, each at its revision, that
// hold any of q.Terms, those that hold the most of them first, of those
// the shortest, then by version, key and place, bytewise; with what they
// are scored against.
// One snapshot reads all three.
func (s *Store) SearchPassages(ctx context.Context, q store.SearchQuery) (store.SearchMatches, error) {
	out := store.SearchMatches{DF: map[string]int{}}
	versions, keys, revisions := make([]string, len(q.Files)), make([]string, len(q.Files)), make([]string, len(q.Files))
	for i, f := range q.Files {
		versions[i], keys[i], revisions[i] = f.VersionID, f.Key, f.Revision
	}
	terms := nonNil(q.Terms)
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, searchScope+`
			SELECT COALESCE(sum(passages), 0), COALESCE(sum(length), 0) FROM scope`, q.CourseID, versions, keys, revisions).
			Scan(&out.Total, &out.Length); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, searchScope+`
			SELECT t, count(*)
			  FROM search_passage p JOIN scope USING (version_id, file_key), unnest(p.terms) AS t
			 WHERE p.terms && $5::text[] AND t = ANY($5::text[])
			 GROUP BY t`, q.CourseID, versions, keys, revisions, terms)
		if err != nil {
			return err
		}
		var term string
		var n int
		if _, err := pgx.ForEachRow(rows, []any{&term, &n}, func() error {
			out.DF[term] = n
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, searchScope+`
			SELECT p.version_id, p.file_key, p.seq, p.section_kind, p.section_n, p.start_offset, p.part, p.text, p.terms, p.length
			  FROM search_passage p JOIN scope USING (version_id, file_key)
			 WHERE p.terms && $5::text[]
			 ORDER BY (SELECT count(*) FROM unnest(p.terms) AS t WHERE t = ANY($5::text[])) DESC, p.length,
			          p.version_id COLLATE "C", p.file_key COLLATE "C", p.seq
			 LIMIT $6`, q.CourseID, versions, keys, revisions, terms, max(q.Limit, 0))
		if err != nil {
			return err
		}
		var m store.SearchMatch
		_, err = pgx.ForEachRow(rows, []any{&m.VersionID, &m.Key, &m.Seq, &m.Kind, &m.N, &m.Offset, &m.Part, &m.Text, &m.Terms, &m.Length},
			func() error {
				out.Passages = append(out.Passages, m)
				m.Terms = nil
				return nil
			})
		return err
	})
	if err != nil {
		return store.SearchMatches{}, fmt.Errorf("store: search passages: %w", err)
	}
	return out, nil
}

// DropSearchVersions destroys what is kept of the versions named.
func (s *Store) DropSearchVersions(ctx context.Context, versionIDs []string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM search_file WHERE version_id = ANY($1::text[])`, nonNil(versionIDs))
	if err != nil {
		return 0, fmt.Errorf("store: drop search versions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DropSearchDocuments destroys what is kept of every version of the
// documents named.
func (s *Store) DropSearchDocuments(ctx context.Context, documentIDs []string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM search_file WHERE document_id = ANY($1::text[])`, nonNil(documentIDs))
	if err != nil {
		return 0, fmt.Errorf("store: drop search documents: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeSearchFiles destroys the files no search has needed since before.
func (s *Store) PurgeSearchFiles(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM search_file WHERE used_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: purge search files: %w", err)
	}
	return tag.RowsAffected(), nil
}
