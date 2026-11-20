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
// worktree at most, from one checkout, and finishes only one its own git
// made.

// ErrWorktreeTaken is a session's worktree, by its branch or its path,
// that another request makes.
var ErrWorktreeTaken = errors.New("another request makes a session's worktree there")

// ErrRequestHasWorktree is a request that made another worktree already,
// or the same one from another checkout: its retry finishes only that one,
// and makes no other.
var ErrRequestHasWorktree = errors.New("the request made another worktree already")

// SessionWorktree is a request's claim of its session's worktree: its
// path and branch, the top of the checkout it is made from, as git named
// it, and how far the request's own making of it got (MarkWorktree): 0 for
// nothing made, then worktree.Step's steps, its folder and its worktree.
type SessionWorktree struct {
	Path, Branch, Top string
	Made              int
}

// WorktreeOf is the worktree request key claimed; ok is false when it
// claimed none.
func (s *Store) WorktreeOf(ctx context.Context, key string) (w SessionWorktree, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT path, branch, top, made FROM session_worktrees WHERE request = ?`, key).Scan(&w.Path, &w.Branch, &w.Top, &w.Made)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionWorktree{}, false, nil
	}
	return w, err == nil, err
}

// ClaimWorktree binds the session's new worktree w (its path, branch and
// checkout's top) to request key, the device-scoped key of the
// CreateSession that makes it, and reserves key for that CreateSession in
// the same transaction, so no other endpoint takes the key while the
// worktree waits for its session (protocol rule 5). The same key with the
// same path, branch and top again changes nothing. A key that holds
// another is ErrRequestHasWorktree; a branch or a path another key holds
// is ErrWorktreeTaken: the branch is the worktree's name in its
// repository, whatever path names the checkout, so two spellings of one
// checkout never share a worktree. A key reserved for another request is
// ErrRequestReused. Nothing changes on a refusal. A claim is kept for
// good, as pocketd keeps the worktree (it never removes one).
func (s *Store) ClaimWorktree(ctx context.Context, key string, w SessionWorktree) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := claimRequest(ctx, tx, key, "createSession", s.millis()); err != nil {
		return err
	}
	var held SessionWorktree
	err = tx.QueryRowContext(ctx, `SELECT path, branch, top FROM session_worktrees WHERE request = ?`, key).Scan(&held.Path, &held.Branch, &held.Top)
	switch {
	case err == nil && held.Path == w.Path && held.Branch == w.Branch && held.Top == w.Top:
		return tx.Commit()
	case err == nil:
		return fmt.Errorf("%w: %s, on %s, from the checkout %s", ErrRequestHasWorktree, held.Path, held.Branch, held.Top)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var holder string
	err = tx.QueryRowContext(ctx, `SELECT request FROM session_worktrees WHERE branch = ? OR path = ?`, w.Branch, w.Path).Scan(&holder)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s, on %s", ErrWorktreeTaken, w.Path, w.Branch)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_worktrees (branch, path, request, top, created_at) VALUES (?, ?, ?, ?, ?)`,
		w.Branch, w.Path, key, w.Top, s.millis()); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkWorktree records that request key's own making of its worktree got
// to step: a retry goes on from there, and finishes a worktree that is
// there only once its own git made it. A step is never undone.
func (s *Store) MarkWorktree(ctx context.Context, key string, step int) error {
	result, err := s.db.ExecContext(ctx, `UPDATE session_worktrees SET made = MAX(made, ?) WHERE request = ?`, step, key)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("no worktree claimed by the request")
	}
	return nil
}
