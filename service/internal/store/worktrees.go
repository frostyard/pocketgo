package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A session's new worktree (ADR-0027) is bound to the request that makes
// it: its name comes from the request id (proto.SessionWorktree), so only
// that request, tried again, finishes it, and another request whose name
// comes out the same is refused before git runs.

// ErrWorktreeTaken is a session's worktree another request makes.
var ErrWorktreeTaken = errors.New("another request makes a session's worktree at that path")

// ClaimWorktree binds path, a session's new worktree, to request key, the
// device-scoped key of the request that makes it. The same key again
// changes nothing; another key is ErrWorktreeTaken, and nothing changes.
// A claim is kept for good, as pocketd keeps the worktree (it never
// removes one).
func (s *Store) ClaimWorktree(ctx context.Context, path, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_worktrees (path, request, created_at) VALUES (?, ?, ?)
		ON CONFLICT (path) DO NOTHING`, path, key, s.millis()); err != nil {
		return err
	}
	var holder string
	if err := tx.QueryRowContext(ctx, `SELECT request FROM session_worktrees WHERE path = ?`, path).Scan(&holder); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("the claim of %s is gone", path)
		}
		return err
	}
	if holder != key {
		return fmt.Errorf("%w: %s", ErrWorktreeTaken, path)
	}
	return tx.Commit()
}
