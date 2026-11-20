package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

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
	// target is what the request named (requestTarget): a replay of
	// the request names it too.
	target string
}

// makeSessionWorktree makes the new worktree of a valid CreateSession of
// request key, or finishes the one an earlier try of it made.
//   - The checkout is named as git names it, and the worktree's place as
//     its folder's parent resolves, every link followed (worktreePlace),
//     and both are bound to the key with the path and the branch
//     (store.ClaimWorktree), which reserves the key for this CreateSession
//     too: another request whose name comes out the same is refused before
//     git runs, and a retry whose checkout or place moved, by its path or
//     behind a link, makes no second worktree. git runs in that checkout,
//     at that place, never ones a link names later (Codex review of #46,
//     rounds 4 and 5).
//   - The request makes its worktree's folder alone, and git makes its
//     branch or fails (worktree.AddNew): a folder or a branch that is there
//     was made by something else, another data folder, a pocketd racing for
//     the name, or before a work database restored, and is never adopted,
//     on any try (Codex review of #46, rounds 2 to 4). Each step is
//     recorded as it is done (store.MarkWorktree), before anything that may
//     fail after it: a retry checks a worktree its own git made as it is,
//     and takes up no folder an earlier try left alone (round 5). A start
//     cut off between a step and its record, or after the folder alone,
//     leaves a folder or a worktree no retry takes; the owner removes it.
//
// git runs to its end, or worktreeTimeout, even when the client goes away.
// It answers the request itself when it fails: conflict for a worktree
// another request holds or made, or for a request that made another, or
// whose id another request holds, bad_request for a checkout or a worktree
// git says is not as it must be, and internal, with why, for a check that
// could not tell or a step that could not be recorded.
func (s *Server) makeSessionWorktree(w http.ResponseWriter, r *http.Request, key string, wt sessionWorktree) bool {
	ctx := context.WithoutCancel(r.Context())
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	// A request that claimed another worktree is told so before its new
	// checkout is looked at, which may be no checkout at all (Codex review
	// of #46, round 8).
	if held, ok, err := s.store.WorktreeOf(ctx, key); err != nil {
		s.internal(w, "read the worktree's claim", err)
		return false
	} else if ok && (held.Path != wt.path || held.Branch != wt.branch) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("%v: %s, on %s, not the checkout its profile names there now: start a new session",
			store.ErrRequestHasWorktree, held.Path, held.Branch))
		return false
	}
	place, ok := s.worktreePlace(gitCtx, w, wt)
	if !ok {
		return false
	}
	if err := s.store.ClaimWorktree(ctx, key, place); errors.Is(err, store.ErrWorktreeTaken) {
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
	record := func(step worktree.Step) error {
		if err := s.markWorktree(ctx, key, int(step)); err != nil {
			return progressFailed{err}
		}
		return nil
	}
	// git makes it at its real place, in the checkout the request bound
	// (Codex review of #46, rounds 4 and 5).
	if err := worktree.AddNew(gitCtx, s.worktreeTools(), place.Top, place.Dest, wt.branch, worktreeToken(key), worktree.Step(claim.Made), record); err != nil {
		return s.worktreeFailed(w, wt, err)
	}
	return true
}

// worktreePlace is where wt is made now, both from one resolution of the
// folder the checkout and the worktree share (Engine.Real, every link
// followed): its real place, that folder and its name (Codex review of
// #46, round 5), and its checkout as git names it there (worktree.Top),
// so a link that moves between the two never splits them (round 7). It
// answers the request itself when it fails.
func (s *Server) worktreePlace(ctx context.Context, w http.ResponseWriter, wt sessionWorktree) (store.SessionWorktree, bool) {
	parent, ok, err := s.engine.Real(ctx, filepath.Dir(wt.path))
	switch {
	case err != nil:
		s.log.Error("resolve a session's worktree's folder", "path", wt.path, "err", err)
		writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, "make the worktree: resolve "+filepath.Dir(wt.path)+": "+err.Error())
		return store.SessionWorktree{}, false
	case !ok:
		return store.SessionWorktree{}, s.worktreeFailed(w, wt, fmt.Errorf("%s is not a folder", filepath.Dir(wt.path)))
	}
	top, err := worktree.Top(ctx, s.worktreeTools(), filepath.Join(parent, filepath.Base(wt.checkout)))
	if err != nil {
		return store.SessionWorktree{}, s.worktreeFailed(w, wt, err)
	}
	return store.SessionWorktree{Path: wt.path, Branch: wt.branch, Top: top, Dest: filepath.Join(parent, filepath.Base(wt.path)), Target: wt.target}, true
}

// progressFailed is a step of a session's worktree done that could not be
// recorded: pocketd failed, not the request (Codex review of #46, round 5).
type progressFailed struct{ error }

func (p progressFailed) Unwrap() error { return p.error }

// worktreeFailed answers a check or a make of a session's worktree that
// failed: conflict for a folder or branch its request did not make, or one
// an earlier try left unfinished, internal with why for a check that could
// not tell or a step that could not be recorded, and bad_request with git's
// word for anything else.
func (s *Server) worktreeFailed(w http.ResponseWriter, wt sessionWorktree, err error) bool {
	var progress progressFailed
	switch {
	case errors.As(err, &progress):
		s.internal(w, "record the worktree's making", err)
	case errors.Is(err, worktree.ErrTaken):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error()+": start the session with another title")
	case errors.Is(err, worktree.ErrUnfinished), errors.Is(err, worktree.ErrBranchLeft):
		// git's word, and why no retry takes the name (Codex review of #46,
		// round 7).
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "make the worktree: "+err.Error())
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
// by checkTimeout, and is its real place, where the session works (Codex
// review of #46, round 6): its checkout's path still names the checkout,
// and its folder's path the place, it was made at (worktreePlace); the
// worktree is still git's registered one there, on its branch, not locked
// (worktree.Usable), as a thread's specialist's is checked when its
// session starts; and it is the one the request made, by its mark
// (worktree.Marked). It answers the request itself when it fails.
func (s *Server) usableSessionWorktree(w http.ResponseWriter, r *http.Request, key string, wt sessionWorktree) (string, bool) {
	ctx := context.WithoutCancel(r.Context())
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	claim, held, err := s.store.WorktreeOf(ctx, key)
	if err != nil {
		s.internal(w, "read the worktree's claim", err)
		return "", false
	}
	if !held {
		s.internal(w, "read the worktree's claim", errors.New("the request holds no worktree"))
		return "", false
	}
	place, ok := s.worktreePlace(check, w, wt)
	if !ok {
		return "", false
	}
	if place.Top != claim.Top || place.Dest != claim.Dest {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("%v: %s, on %s, from the checkout %s, at %s, which %s and %s no longer name: start a new session",
			store.ErrRequestHasWorktree, claim.Path, claim.Branch, claim.Top, claim.Dest, wt.checkout, wt.path))
		return "", false
	}
	if err := worktree.Usable(check, s.worktreeTools(), claim.Top, claim.Dest, wt.branch); err != nil {
		return "", s.worktreeFailed(w, wt, fmt.Errorf("the worktree changed since it was made: %w", err))
	}
	// The one the request made, by its mark, never one made again there
	// since (Codex review of #46, round 6).
	if err := worktree.Marked(check, s.worktreeTools(), claim.Dest, worktreeToken(key)); err != nil {
		return "", s.worktreeFailed(w, wt, err)
	}
	return claim.Dest, true
}

// requestTarget is a digest of what a CreateSession with a worktree names:
// its profile, repository, folder as sent and title. A replay of the
// request names the same, and any other request with its id is another
// (Codex review of #46, round 7); nothing of the profiles or the files
// is read for it, so a retry replays whatever changed there since.
func requestTarget(request proto.CreateSession) string {
	// A JSON array keeps each field's bounds whatever it holds, a NUL
	// included (Codex review of #46, round 8).
	fields, _ := json.Marshal([]string{request.Profile, request.Repository, request.Cwd, strings.TrimSpace(request.Title)})
	sum := sha256.Sum256(fields)
	return hex.EncodeToString(sum[:])
}

// worktreeToken is the mark of the worktree request key makes
// (worktree.MarkName): a digest of the key, so the mark names the request
// without its device's id.
func worktreeToken(key string) string {
	sum := sha256.Sum256([]byte("pocketgo session worktree\x00" + key))
	return hex.EncodeToString(sum[:])
}
