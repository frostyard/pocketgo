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
// comes out the same is refused before git runs. A request makes one
// worktree at most.

// ErrWorktreeTaken is a session's worktree, by its branch or its path,
// that another request makes.
var ErrWorktreeTaken = errors.New("another request makes a session's worktree there")

// ErrRequestHasWorktree is a request that made another worktree already:
// its retry finishes that one, and makes no other.
var ErrRequestHasWorktree = errors.New("the request made another worktree already")

// ClaimWorktree binds the session's new worktree at path, on branch, to
// request key, the device-scoped key of the request that makes it. The
// same key with the same path and branch again changes nothing. A key that
// holds another path or branch is ErrRequestHasWorktree; a branch or a path
// another key holds is ErrWorktreeTaken: the branch is the worktree's name
// in its repository, whatever path names the checkout, so two spellings
// of one checkout never share a worktree. Nothing changes on a refusal. A
// claim is kept for good, as pocketd keeps the worktree (it never removes
// one).
func (s *Store) ClaimWorktree(ctx context.Context, key, path, branch string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var heldPath, heldBranch string
	err = tx.QueryRowContext(ctx, `SELECT path, branch FROM session_worktrees WHERE request = ?`, key).Scan(&heldPath, &heldBranch)
	switch {
	case err == nil && heldPath == path && heldBranch == branch:
		return nil
	case err == nil:
		return fmt.Errorf("%w: %s, on %s", ErrRequestHasWorktree, heldPath, heldBranch)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var holder string
	err = tx.QueryRowContext(ctx, `SELECT request FROM session_worktrees WHERE branch = ? OR path = ?`, branch, path).Scan(&holder)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s, on %s", ErrWorktreeTaken, path, branch)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_worktrees (branch, path, request, created_at) VALUES (?, ?, ?, ?)`,
		branch, path, key, s.millis()); err != nil {
		return err
	}
	return tx.Commit()
}
