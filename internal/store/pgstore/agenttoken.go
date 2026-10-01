package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// The operator's agents' tokens (migration 0013), as Core issued them to
// the runtime by the agents' ids: one row an agent, its token sealed.

// AgentToken is the agent's token, or store.ErrNotFound.
func (s *Store) AgentToken(ctx context.Context, agentID string) (*store.AgentToken, error) {
	var t store.AgentToken
	err := s.pool.QueryRow(ctx, `
		SELECT agent_id, core_actor_id, secret_id, credential_id, hint, issued_at, issued_by FROM agent_token WHERE agent_id = $1`,
		agentID).Scan(&t.AgentID, &t.CoreActorID, &t.SecretID, &t.CredentialID, &t.Hint, &t.IssuedAt, &t.IssuedBy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("agent token of %s: %w", agentID, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: agent token of %s: %w", agentID, err)
	}
	utc(&t.IssuedAt)
	return &t, nil
}

// PutAgentToken stores t with its secret in place of the agent's token,
// while that is still the secret prev, destroying the one replaced.
func (s *Store) PutAgentToken(ctx context.Context, t store.AgentToken, secret store.Secret, prev string) error {
	if err := store.CheckAgentToken(t, secret); err != nil {
		return err
	}
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var held string
		err := tx.QueryRow(ctx, `SELECT secret_id FROM agent_token WHERE agent_id = $1 FOR UPDATE`, t.AgentID).Scan(&held)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			held = ""
		case err != nil:
			return err
		}
		if held != prev {
			return fmt.Errorf("agent token of %s: %w", t.AgentID, store.ErrConflict)
		}
		if err := insertSecret(ctx, tx, secret); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_token (agent_id, core_actor_id, secret_id, credential_id, hint, issued_at, issued_by)
			VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), $7)
			ON CONFLICT (agent_id) DO UPDATE
			   SET core_actor_id = EXCLUDED.core_actor_id, secret_id = EXCLUDED.secret_id, credential_id = EXCLUDED.credential_id,
			       hint = EXCLUDED.hint, issued_at = EXCLUDED.issued_at, issued_by = EXCLUDED.issued_by`,
			t.AgentID, t.CoreActorID, t.SecretID, t.CredentialID, t.Hint, orNow(t.IssuedAt), t.IssuedBy); err != nil {
			return err
		}
		if held == "" {
			return nil
		}
		_, err = tx.Exec(ctx, `DELETE FROM secret WHERE id = $1`, held)
		return err
	})
	if errors.Is(err, store.ErrConflict) {
		return err
	}
	if err != nil {
		return fmt.Errorf("store: put agent token of %s: %w", t.AgentID, err)
	}
	return nil
}

// DeleteAgentToken forgets the agent's token, while it is still secretID
// ("" for any), and destroys its secret.
func (s *Store) DeleteAgentToken(ctx context.Context, agentID, secretID string) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var held string
		err := tx.QueryRow(ctx, `
			DELETE FROM agent_token WHERE agent_id = $1 AND ($2::text = '' OR secret_id = $2::text) RETURNING secret_id`,
			agentID, secretID).Scan(&held)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNothingDeleted
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM secret WHERE id = $1`, held)
		return err
	})
	if errors.Is(err, errNothingDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: delete agent token of %s: %w", agentID, err)
	}
	return nil
}
