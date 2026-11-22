package api

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// cleanLater queues a closed thread without starting a pass or taking any
// team's lock: publishThread may already hold workMu.
func (s *Server) cleanLater(id int64) {
	s.cleanMu.Lock()
	defer s.cleanMu.Unlock()
	if s.cleanDone[id] {
		return
	}
	if s.cleanQueue == nil {
		s.cleanQueue = map[int64]bool{}
	}
	s.cleanQueue[id] = true
}

// cleanPass runs under reviewMu, outside workMu and outboxMu. Only noting
// and publishing the result takes workMu, after the removal has finished.
func (s *Server) cleanPass(ctx context.Context) {
	s.cleanMu.Lock()
	if s.cleansDue == nil {
		s.cleansDue = map[int64]bool{}
	}
	for id := range s.cleanQueue {
		s.cleansDue[id] = true
	}
	s.cleanQueue = nil
	s.cleanMu.Unlock()
	for id := range s.cleansDue {
		done, note := s.cleanThread(ctx, id)
		if !done {
			continue
		}
		s.cleanMu.Lock()
		if s.cleanDone == nil {
			s.cleanDone = map[int64]bool{}
		}
		s.cleanDone[id] = true
		delete(s.cleanQueue, id)
		s.cleanMu.Unlock()
		delete(s.cleansDue, id)
		if note != "" {
			s.workMu.Lock()
			row, changed, err := s.store.NoteClosedThread(ctx, id, note)
			if err != nil {
				s.log.Warn("note a kept closed thread's worktree", "thread", id, "err", err)
			} else if changed {
				s.publishThread(row)
			}
			s.workMu.Unlock()
		}
	}
}

// cleanThread checks a closed thread's ownership and idle sessions before
// asking sandboxed git to remove its worktree. Uncertain checks retry.
func (s *Server) cleanThread(ctx context.Context, id int64) (done bool, note string) {
	row, err := s.store.Thread(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return true, ""
	}
	if err != nil {
		s.log.Warn("read a closed thread to clean", "thread", id, "err", err)
		return false, ""
	}
	if row.State != store.ThreadClosed || row.Repository == "" {
		return true, ""
	}
	for _, session := range []int64{row.Session, row.WorkerSession} {
		if session == 0 {
			continue
		}
		busy, err := s.engine.Busy(ctx, session)
		if err != nil {
			s.log.Warn("check a closed thread's session before cleanup", "thread", id, "session", session, "err", err)
			return false, ""
		}
		if busy {
			return false, ""
		}
	}
	path, branch := row.Cwd, row.Branch
	if path == "" {
		if s.profiles == nil {
			s.log.Warn("a closed thread has no profile for cleanup", "thread", id)
			return true, ""
		}
		target, err := s.profiles.Find(row.Agent)
		if err != nil {
			s.log.Warn("read a closed thread's profile for cleanup", "thread", id, "err", err)
			return true, ""
		}
		_, checkout := threadCheckout(target, row)
		if checkout == "" {
			s.log.Warn("a closed thread has no checkout for cleanup", "thread", id)
			return true, ""
		}
		path, branch = team.Worktree(checkout, row.Agent, team.Slug(row.Title, row.ID, row.Owner))
		if row.ReviewPull != 0 {
			branch = ""
		}
	}
	if !strings.HasSuffix(filepath.Base(path), "-t"+strconv.FormatInt(row.ID, 10)+"-"+row.Owner) {
		s.log.Warn("a closed thread's worktree is not named for it", "thread", id, "path", path)
		return true, ""
	}
	owner, err := s.store.SessionWorktreeAt(ctx, path)
	if err != nil {
		s.log.Warn("check an owner's session worktree before cleanup", "thread", id, "path", path, "err", err)
		return false, ""
	}
	if owner {
		s.log.Warn("a closed thread names an owner's session worktree", "thread", id, "path", path)
		return true, ""
	}
	r := worktree.Removal{Path: path}
	if row.ReviewPull != 0 {
		r.Head = row.ReviewHead
	} else {
		r.Branch = branch
		pulls, err := s.store.PullsOfThread(ctx, id)
		if err != nil {
			s.log.Warn("read a closed thread's pulls before cleanup", "thread", id, "err", err)
			return false, ""
		}
		for _, pull := range pulls {
			if pull.HeadSHA != "" {
				r.Heads = append(r.Heads, pull.HeadSHA)
			}
		}
	}
	gitCtx, cancel := s.withTimeout(ctx, worktreeTimeout)
	defer cancel()
	branchKept, err := worktree.Remove(gitCtx, s.worktreeTools(), r)
	if err == nil {
		s.log.Info("removed a closed thread's worktree", "thread", id, "path", path)
		if branchKept != "" {
			s.log.Warn("kept a closed thread's branch", "thread", id, "branch", branch, "why", branchKept)
		}
		return true, ""
	}
	var kept worktree.Kept
	if errors.As(err, &kept) {
		s.log.Warn("kept a closed thread's worktree", "thread", id, "path", path, "why", kept.Reason)
		return true, team.WorktreeKept(path, kept.Reason)
	}
	s.log.Warn("could not clean a closed thread's worktree", "thread", id, "path", path, "err", err)
	return false, ""
}
