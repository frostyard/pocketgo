package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// A session the owner starts on a repository may work in a new git
// worktree of its checkout (ADR-0027, protocol rule 6a): beside the
// checkout, on a new branch from its HEAD, named from the request
// (team.SessionWorktree), as a thread's worktree is (ADR-0014). git runs
// in the sandbox, through the engine (worktree.Add), never with pocketd's
// rights. pocketd never removes one.

// sessionWorktree is a session's new worktree: the checkout it is made
// from, its path and its branch.
type sessionWorktree struct {
	checkout, path, branch string
}

// makeSessionWorktree makes the new worktree of a valid CreateSession of
// request key, or finishes the one an earlier try of it made.
//   - The checkout is named as git names it, every link resolved
//     (worktree.Top), and bound to the key with the path and the branch
//     (store.ClaimWorktree), which reserves the key for this CreateSession
//     too: another request whose name comes out the same is refused before
//     git runs, and a retry whose checkout moved, by its path or behind a
//     link, makes no second worktree. git runs in that checkout, never in
//     the one a link names later (Codex review of #46, round 4).
//   - The request makes its worktree's folder alone, and git makes its
//     branch or fails (worktree.AddNew): a folder or a branch that is there
//     was made by something else, another data folder, a pocketd racing for
//     the name, or before a work database restored, and is never adopted,
//     on any try (Codex review of #46, rounds 2 to 4). Each step is
//     recorded as it is done (store.MarkWorktree), before anything that may
//     fail after it, so a retry goes on from it, and finishes a worktree
//     that is there only once its own git made it. A start cut off between
//     a step and its record leaves a folder or a worktree no retry takes;
//     the owner removes it.
//
// git runs to its end, or worktreeTimeout, even when the client goes away.
// It answers the request itself when it fails: conflict for a worktree
// another request holds or made, or for a request that made another, or
// whose id another request holds, bad_request for a checkout or a worktree
// git says is not as it must be, and internal, with why, for a check that
// could not tell.
func (s *Server) makeSessionWorktree(w http.ResponseWriter, r *http.Request, key string, wt sessionWorktree) bool {
	ctx := context.WithoutCancel(r.Context())
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	top, err := worktree.Top(gitCtx, s.worktreeTools(), wt.checkout)
	if err != nil {
		return s.worktreeFailed(w, wt, err)
	}
	if err := s.store.ClaimWorktree(ctx, key, store.SessionWorktree{Path: wt.path, Branch: wt.branch, Top: top}); errors.Is(err, store.ErrWorktreeTaken) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error()+": start the session with another title")
		return false
	} else if errors.Is(err, store.ErrRequestHasWorktree) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error()+", not the checkout its profile names there now: start a new session")
		return false
	} else if errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return false
	} else if err != nil {
		s.internal(w, "claim the worktree", err)
		return false
	}
	claim, _, err := s.store.WorktreeOf(ctx, key)
	if err != nil {
		s.internal(w, "read the worktree's claim", err)
		return false
	}
	record := func(step worktree.Step) error { return s.store.MarkWorktree(ctx, key, int(step)) }
	if err := worktree.AddNew(gitCtx, s.worktreeTools(), top, wt.path, wt.branch, worktree.Step(claim.Made), record); err != nil {
		return s.worktreeFailed(w, wt, err)
	}
	return true
}

// worktreeFailed answers a check or a make of a session's worktree that
// failed: conflict for a path or branch its request did not make, internal
// with why for a check that could not tell, and bad_request with git's word
// for anything else.
func (s *Server) worktreeFailed(w http.ResponseWriter, wt sessionWorktree, err error) bool {
	switch {
	case errors.Is(err, worktree.ErrTaken):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error()+": start the session with another title")
	case worktree.Unsure(err):
		s.log.Error("make a session's worktree", "path", wt.path, "err", err)
		writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, "make the worktree: "+err.Error())
	default:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "make the worktree: "+err.Error())
	}
	return false
}

// usableSessionWorktree checks, under createMu and before the profile is
// read the last time, the worktree request key made, as it is now, bound
// by checkTimeout: its checkout's path still names the checkout its
// worktree was made from (worktree.Top), and the worktree is still git's
// registered one at its path, on its branch, not locked (worktree.Usable),
// as a thread's specialist's is checked when its session starts (Codex
// review of #46, rounds 2 and 4). It answers the request itself when it
// fails.
func (s *Server) usableSessionWorktree(w http.ResponseWriter, r *http.Request, key string, wt sessionWorktree) bool {
	ctx := context.WithoutCancel(r.Context())
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	claim, held, err := s.store.WorktreeOf(ctx, key)
	if err != nil {
		s.internal(w, "read the worktree's claim", err)
		return false
	}
	if !held {
		s.internal(w, "read the worktree's claim", errors.New("the request holds no worktree"))
		return false
	}
	top, err := worktree.Top(check, s.worktreeTools(), wt.checkout)
	if err != nil {
		return s.worktreeFailed(w, wt, fmt.Errorf("the checkout changed since its worktree was made: %w", err))
	}
	if top != claim.Top {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("%v: %s, on %s, from the checkout %s, which %s no longer names: start a new session",
			store.ErrRequestHasWorktree, claim.Path, claim.Branch, claim.Top, wt.checkout))
		return false
	}
	if err := worktree.Usable(check, s.worktreeTools(), top, wt.path, wt.branch); err != nil {
		return s.worktreeFailed(w, wt, fmt.Errorf("the worktree changed since it was made: %w", err))
	}
	return true
}
