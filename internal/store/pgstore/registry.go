package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// RegistryChannel is what every write to a hosted agent or course is
// notified on, by trigger (migration 0003).
const RegistryChannel = "aishie_registry"

// PutPerson records p, replacing what was known of them.
func (s *Store) PutPerson(ctx context.Context, p store.Person) error {
	if err := required("core_actor_id", p.CoreActorID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO person (core_actor_id, display_name, platform_role, last_seen_at)
		VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()))
		ON CONFLICT (core_actor_id) DO UPDATE
		   SET display_name = EXCLUDED.display_name, platform_role = EXCLUDED.platform_role,
		       last_seen_at = EXCLUDED.last_seen_at`,
		p.CoreActorID, p.DisplayName, p.PlatformRole, orNow(p.LastSeenAt))
	if err != nil {
		return fmt.Errorf("store: put person %s: %w", p.CoreActorID, err)
	}
	return nil
}

// Person is the person, or store.ErrNotFound.
func (s *Store) Person(ctx context.Context, coreActorID string) (*store.Person, error) {
	var p store.Person
	err := s.pool.QueryRow(ctx, `
		SELECT core_actor_id, display_name, platform_role, last_seen_at FROM person WHERE core_actor_id = $1`,
		coreActorID).Scan(&p.CoreActorID, &p.DisplayName, &p.PlatformRole, &p.LastSeenAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("person %s: %w", coreActorID, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: person %s: %w", coreActorID, err)
	}
	utc(&p.LastSeenAt)
	return &p, nil
}

// hostedColumns are what scanHosted reads, in its order.
const hostedColumns = `id, core_actor_id, owner_actor_id, owner_verified, tenant_id, display_name,
	token_secret_id, token_hint, COALESCE(key_secret_id, ''), key_hint, key_provider, paused, settings::text, version,
	created_at, updated_at`

func scanHosted(row pgx.Row) (*store.HostedAgent, error) {
	var a store.HostedAgent
	var settings string
	if err := row.Scan(&a.ID, &a.CoreActorID, &a.OwnerActorID, &a.OwnerVerified, &a.TenantID, &a.DisplayName,
		&a.TokenSecretID, &a.TokenHint, &a.KeySecretID, &a.KeyHint, &a.KeyProvider, &a.Paused, &settings, &a.Version,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Settings = compact(settings)
	utc(&a.CreatedAt)
	utc(&a.UpdatedAt)
	return &a, nil
}

// compact is jsonb's text as memstore keeps JSON: without the spaces jsonb
// puts in.
func compact(s string) json.RawMessage {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return json.RawMessage(s)
	}
	return b.Bytes()
}

// orNull is s, or NULL for a text column when s is empty.
func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// secretOn reads a secret on q, which may be a transaction's.
func secretOn(ctx context.Context, q pgx.Tx, id string) (*store.Secret, error) {
	sec, err := scanSecret(q.QueryRow(ctx, `SELECT `+secretColumns+` FROM secret WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("secret %s: %w", id, store.ErrNotFound)
	}
	return sec, err
}

// hostedErr is a write's error, told in the store's words: an id or a
// Core actor taken is store.ErrExists, and a secret another agent refers
// to is said to be.
func hostedErr(a store.HostedAgent, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "hosted_agent_pkey":
			return fmt.Errorf("hosted agent %s: %w", a.ID, store.ErrExists)
		case "hosted_agent_core_actor_id_key":
			return fmt.Errorf("hosted agent of actor %s: %w", a.CoreActorID, store.ErrExists)
		case "hosted_agent_token_secret_id_key", "hosted_agent_key_secret_id_key":
			return fmt.Errorf("store: hosted agent %s: a secret it refers to is another agent's", a.ID)
		}
	}
	return err
}

// CreateHostedAgent stores a at version 1 with its secrets, in one
// transaction.
func (s *Store) CreateHostedAgent(ctx context.Context, a store.HostedAgent, secrets ...store.Secret) (*store.HostedAgent, error) {
	a, err := store.CheckHostedAgent(a)
	if err != nil {
		return nil, err
	}
	var out *store.HostedAgent
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		for _, sec := range secrets {
			if err := insertSecret(ctx, tx, sec); err != nil {
				return err
			}
		}
		if err := store.CheckAgentSecrets(a, secrets, func(id string) (*store.Secret, error) { return secretOn(ctx, tx, id) }); err != nil {
			return err
		}
		var err error
		out, err = scanHosted(tx.QueryRow(ctx, `
			INSERT INTO hosted_agent (id, core_actor_id, owner_actor_id, owner_verified, tenant_id, display_name,
			                          token_secret_id, token_hint, key_secret_id, key_hint, key_provider, paused, settings, version,
			                          created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, 1,
			        COALESCE($14::timestamptz, now()), COALESCE($14::timestamptz, now()))
			RETURNING `+hostedColumns,
			a.ID, a.CoreActorID, a.OwnerActorID, a.OwnerVerified, a.TenantID, a.DisplayName,
			a.TokenSecretID, a.TokenHint, orNull(a.KeySecretID), a.KeyHint, a.KeyProvider, a.Paused, string(a.Settings), orNow(a.CreatedAt)))
		return hostedErr(a, err)
	})
	if err != nil {
		return nil, fmt.Errorf("store: create hosted agent %s: %w", a.ID, err)
	}
	return out, nil
}

// HostedAgent is the agent id, or store.ErrNotFound.
func (s *Store) HostedAgent(ctx context.Context, id string) (*store.HostedAgent, error) {
	a, err := scanHosted(s.pool.QueryRow(ctx, `SELECT `+hostedColumns+` FROM hosted_agent WHERE id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("hosted agent %s: %w", id, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: hosted agent %s: %w", id, err)
	}
	return a, nil
}

// HostedAgentByActor is the agent of a Core actor, or store.ErrNotFound.
func (s *Store) HostedAgentByActor(ctx context.Context, coreActorID string) (*store.HostedAgent, error) {
	a, err := scanHosted(s.pool.QueryRow(ctx, `SELECT `+hostedColumns+` FROM hosted_agent WHERE core_actor_id = $1`, coreActorID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("hosted agent of actor %s: %w", coreActorID, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: hosted agent of actor %s: %w", coreActorID, err)
	}
	return a, nil
}

// queryHosted lists the agents a query returns.
func (s *Store) queryHosted(ctx context.Context, sql string, args ...any) ([]store.HostedAgent, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.HostedAgent
	for rows.Next() {
		a, err := scanHosted(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// HostedAgentsOwnedBy lists an owner's agents, by id.
func (s *Store) HostedAgentsOwnedBy(ctx context.Context, ownerActorID string) ([]store.HostedAgent, error) {
	out, err := s.queryHosted(ctx, `SELECT `+hostedColumns+` FROM hosted_agent
		 WHERE owner_actor_id = $1 ORDER BY id COLLATE "C"`, ownerActorID)
	if err != nil {
		return nil, fmt.Errorf("store: hosted agents of %s: %w", ownerActorID, err)
	}
	return out, nil
}

// HostedAgents lists every hosted agent, by id.
func (s *Store) HostedAgents(ctx context.Context) ([]store.HostedAgent, error) {
	out, err := s.queryHosted(ctx, `SELECT `+hostedColumns+` FROM hosted_agent ORDER BY id COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("store: hosted agents: %w", err)
	}
	return out, nil
}

// UpdateHostedAgent writes a over the agent of its id, if a.Version is
// still its version, in one transaction with its new secrets and the
// destruction of those it no longer refers to.
func (s *Store) UpdateHostedAgent(ctx context.Context, a store.HostedAgent, secrets ...store.Secret) (*store.HostedAgent, error) {
	a, err := store.CheckHostedAgent(a)
	if err != nil {
		return nil, err
	}
	var out *store.HostedAgent
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		old, err := scanHosted(tx.QueryRow(ctx, `SELECT `+hostedColumns+` FROM hosted_agent WHERE id = $1 FOR UPDATE`, a.ID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("hosted agent %s: %w", a.ID, store.ErrNotFound)
		case err != nil:
			return err
		case old.Version != a.Version:
			return fmt.Errorf("hosted agent %s at version %d: %w", a.ID, a.Version, store.ErrConflict)
		case old.CoreActorID != a.CoreActorID || old.TenantID != a.TenantID:
			return fmt.Errorf("hosted agent %s: its Core actor and tenant do not change", a.ID)
		}
		for _, sec := range secrets {
			if err := insertSecret(ctx, tx, sec); err != nil {
				return err
			}
		}
		if err := store.CheckAgentSecrets(a, secrets, func(id string) (*store.Secret, error) { return secretOn(ctx, tx, id) }); err != nil {
			return err
		}
		out, err = scanHosted(tx.QueryRow(ctx, `
			UPDATE hosted_agent
			   SET owner_actor_id = $2, owner_verified = $3, display_name = $4, token_secret_id = $5, token_hint = $6,
			       key_secret_id = $7, key_hint = $8, key_provider = $9, paused = $10, settings = $11::jsonb,
			       version = version + 1, updated_at = now()
			 WHERE id = $1
			RETURNING `+hostedColumns,
			a.ID, a.OwnerActorID, a.OwnerVerified, a.DisplayName, a.TokenSecretID, a.TokenHint,
			orNull(a.KeySecretID), a.KeyHint, a.KeyProvider, a.Paused, string(a.Settings)))
		if err != nil {
			return hostedErr(a, err)
		}
		var gone []string
		for _, id := range []string{old.TokenSecretID, old.KeySecretID} {
			if id != "" && id != a.TokenSecretID && id != a.KeySecretID {
				gone = append(gone, id)
			}
		}
		if len(gone) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM secret WHERE id = ANY($1)`, gone); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: update hosted agent %s: %w", a.ID, err)
	}
	return out, nil
}

// SetHostedAgentPaused pauses or resumes the agent, at version when it is
// not 0, whatever its version otherwise.
func (s *Store) SetHostedAgentPaused(ctx context.Context, id string, paused bool, version int) (*store.HostedAgent, error) {
	var out *store.HostedAgent
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanHosted(tx.QueryRow(ctx, `
			UPDATE hosted_agent SET paused = $2, version = version + 1, updated_at = now()
			 WHERE id = $1 AND ($3::int = 0 OR version = $3::int)
			RETURNING `+hostedColumns, id, paused, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return missingOrMoved(ctx, tx, id)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: pause hosted agent %s: %w", id, err)
	}
	return out, nil
}

// missingOrMoved is why a write of the agent id that named what it must
// still be found no row: ErrNotFound when it is gone, ErrConflict when it
// is there but has moved on.
func missingOrMoved(ctx context.Context, tx pgx.Tx, id string) error {
	var there bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hosted_agent WHERE id = $1)`, id).Scan(&there); err != nil {
		return err
	}
	if there {
		return fmt.Errorf("hosted agent %s: %w", id, store.ErrConflict)
	}
	return fmt.Errorf("hosted agent %s: %w", id, store.ErrNotFound)
}

// DeleteHostedAgent destroys the agent, its courses (by cascade) and its
// secrets, in one transaction, if it is still as cond says.
func (s *Store) DeleteHostedAgent(ctx context.Context, id string, cond store.DeleteIf) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var token, key string
		err := tx.QueryRow(ctx, `
			DELETE FROM hosted_agent
			 WHERE id = $1 AND ($2::text = '' OR token_secret_id = $2::text) AND ($3::int = 0 OR version = $3::int)
			RETURNING token_secret_id, COALESCE(key_secret_id, '')`, id, cond.TokenSecretID, cond.Version).
			Scan(&token, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingOrMoved(ctx, tx, id)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM secret WHERE id = $1 OR id = $2`, token, key)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: delete hosted agent %s: %w", id, err)
	}
	return nil
}

// PutHostedCourse writes an agent's settings for a course.
func (s *Store) PutHostedCourse(ctx context.Context, c store.HostedCourse) error {
	c, err := store.CheckHostedCourse(c)
	if err != nil {
		return err
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO hosted_course (agent_id, course_id, settings, updated_by, updated_at)
			VALUES ($1, $2, $3::jsonb, $4, COALESCE($5::timestamptz, now()))
			ON CONFLICT (agent_id, course_id) DO UPDATE
			   SET settings = EXCLUDED.settings, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
			c.AgentID, c.CourseID, string(c.Settings), c.UpdatedBy, orNow(c.UpdatedAt))
		return err
	})
	if isForeignKeyViolation(err) {
		return fmt.Errorf("hosted agent %s: %w", c.AgentID, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: put hosted course %s of %s: %w", c.CourseID, c.AgentID, err)
	}
	return nil
}

// queryCourses lists the courses a query returns.
func (s *Store) queryCourses(ctx context.Context, sql string, args ...any) ([]store.HostedCourse, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.HostedCourse
	for rows.Next() {
		var c store.HostedCourse
		var settings string
		if err := rows.Scan(&c.AgentID, &c.CourseID, &settings, &c.UpdatedBy, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Settings = compact(settings)
		utc(&c.UpdatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// HostedCourses lists one agent's courses, by course id.
func (s *Store) HostedCourses(ctx context.Context, agentID string) ([]store.HostedCourse, error) {
	out, err := s.queryCourses(ctx, `
		SELECT agent_id, course_id, settings::text, updated_by, updated_at FROM hosted_course
		 WHERE agent_id = $1 ORDER BY course_id COLLATE "C"`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: hosted courses of %s: %w", agentID, err)
	}
	return out, nil
}

// ListHostedCourses lists every hosted agent's courses.
func (s *Store) ListHostedCourses(ctx context.Context) ([]store.HostedCourse, error) {
	out, err := s.queryCourses(ctx, `
		SELECT agent_id, course_id, settings::text, updated_by, updated_at FROM hosted_course
		 ORDER BY agent_id COLLATE "C", course_id COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("store: hosted courses: %w", err)
	}
	return out, nil
}

// DeleteHostedCourse removes an agent's settings for a course. A delete of
// settings that are not there is rolled back: the trigger moves the
// registry's revision on for every statement, even one that deletes
// nothing, and every worker would rebuild for it.
func (s *Store) DeleteHostedCourse(ctx context.Context, agentID, courseID string) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM hosted_course WHERE agent_id = $1 AND course_id = $2`, agentID, courseID)
		if err == nil && tag.RowsAffected() == 0 {
			return errNothingDeleted
		}
		return err
	})
	if errors.Is(err, errNothingDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: delete hosted course %s of %s: %w", courseID, agentID, err)
	}
	return nil
}

// errNothingDeleted rolls back a delete that found nothing.
var errNothingDeleted = errors.New("nothing to delete")

// RegistryRev is the registry's revision.
func (s *Store) RegistryRev(ctx context.Context) (int64, error) {
	var rev int64
	if err := s.pool.QueryRow(ctx, `SELECT rev FROM registry_rev`).Scan(&rev); err != nil {
		return 0, fmt.Errorf("store: the registry's revision: %w", err)
	}
	return rev, nil
}

// ListenRegistry listens for the registry's changes on a connection of its
// own, not the pool's: it calls ready once it listens, and changed at each
// notification (one per transaction that wrote a hosted agent or course),
// until ctx is done or the connection fails, which it returns. The caller
// connects again (registry.Watcher), and reads the registry after ready,
// since what changed while it was not listening was told to nobody.
func (s *Store) ListenRegistry(ctx context.Context, ready, changed func()) error {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("pgstore: connect to listen: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{RegistryChannel}.Sanitize()); err != nil {
		return fmt.Errorf("pgstore: listen: %w", err)
	}
	ready()
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return fmt.Errorf("pgstore: listen: %w", err)
		}
		changed()
	}
}
