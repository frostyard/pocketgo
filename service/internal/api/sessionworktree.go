package api

import (
	"context"
	"errors"
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
// request key, or finishes the one an earlier try of it made. The path and
// the branch are bound to the key first (store.ClaimWorktree), so another
// request whose name comes out the same is refused before git runs, and
// never takes this one's worktree, and a retry whose checkout moved makes
// no second one. git runs to its end, or worktreeTimeout, even when the
// client goes away: a retry then finds the worktree made. It answers the
// request itself when it fails: conflict for a worktree another request
// holds, or for a request that made another, bad_request for a checkout or
// a worktree git says is not as it must be, and internal, with why, for a
// check that could not tell.
func (s *Server) makeSessionWorktree(w http.ResponseWriter, r *http.Request, key string, wt sessionWorktree) bool {
	ctx := context.WithoutCancel(r.Context())
	if err := s.store.ClaimWorktree(ctx, key, wt.path, wt.branch); errors.Is(err, store.ErrWorktreeTaken) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error()+": start the session with another title")
		return false
	} else if errors.Is(err, store.ErrRequestHasWorktree) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error()+", from a checkout its profile no longer names there: start a new session")
		return false
	} else if err != nil {
		s.internal(w, "claim the worktree", err)
		return false
	}
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	if err := worktree.Add(gitCtx, s.worktreeTools(), wt.checkout, wt.path, wt.branch); err != nil {
		if worktree.Unsure(err) {
			s.log.Error("make a session's worktree", "path", wt.path, "err", err)
			writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, "make the worktree: "+err.Error())
			return false
		}
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "make the worktree: "+err.Error())
		return false
	}
	return true
}
