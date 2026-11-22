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

// cleanForget forgets that thread id was decided in this run, without
// taking any team's lock: a thread that reopened (ADR-0061) is queued
// again at its next close.
func (s *Server) cleanForget(id int64) {
	s.cleanMu.Lock()
	defer s.cleanMu.Unlock()
	delete(s.cleanDone, id)
}

// lockClean holds closed thread id's cleanup lock until the returned
// function runs (ADR-0061): a removal of its worktree, from the look at
// its state to git's end, and a message that would reopen it, from the
// look at its folder to the record that follows the message, go one at a
// time, so no worktree goes from under a message that reopens its thread.
// It is taken before outboxMu and workMu, never under them.
func (s *Server) lockClean(id int64) (unlock func()) {
	return s.cleanLocks.lock(strconv.FormatInt(id, 10))
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
		note, pending := s.cleanNotes[id]
		if !pending {
			kept, err := s.cleanThread(ctx, id)
			if errors.Is(err, errReopened) {
				// It reopened before its pass: it is decided at its next
				// close, not now (ADR-0061).
				delete(s.cleansDue, id)
				continue
			}
			if err != nil {
				continue
			}
			note = kept
		}
		// A kept worktree's note must reach the report before the thread is
		// decided: a failed write keeps the note for the next pass. A
		// thread that reopened meanwhile drops it, and is decided at its
		// next close (ADR-0061).
		if note != "" {
			written, reopened := s.noteKept(ctx, id, note)
			if reopened {
				delete(s.cleanNotes, id)
				delete(s.cleansDue, id)
				continue
			}
			if !written {
				if s.cleanNotes == nil {
					s.cleanNotes = map[int64]string{}
				}
				s.cleanNotes[id] = note
				continue
			}
		}
		delete(s.cleanNotes, id)
		s.cleanMu.Lock()
		if s.cleanDone == nil {
			s.cleanDone = map[int64]bool{}
		}
		s.cleanDone[id] = true
		delete(s.cleanQueue, id)
		s.cleanMu.Unlock()
		delete(s.cleansDue, id)
	}
}

// noteKept adds a kept worktree's line to a closed thread's report under
// workMu. written is false only when the write failed and should be
// retried; reopened says the thread is no longer closed, and takes no
// line (ADR-0061).
func (s *Server) noteKept(ctx context.Context, id int64, note string) (written, reopened bool) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	row, changed, err := s.store.NoteClosedThread(ctx, id, note)
	if errors.Is(err, store.ErrNotFound) {
		return true, false
	}
	if err != nil {
		s.log.Warn("note a kept closed thread's worktree", "thread", id, "err", err)
		return false, false
	}
	if changed {
		s.publishThread(row)
	}
	return true, row.State != store.ThreadClosed
}

// errReopened is a thread to clean that is no longer closed: it reopened
// (ADR-0061), and is decided at its next close. errCleanLater is a
// cleanup tried again at the next pass.
var (
	errReopened   = errors.New("the thread is not closed")
	errCleanLater = errors.New("clean it at the next pass")
)

// cleanThread checks a closed thread's ownership and idle sessions before
// asking sandboxed git to remove its worktree. Uncertain checks retry
// (errCleanLater); a thread no longer closed is errReopened. It holds the
// thread's cleanup lock (lockClean) from its look at the thread to git's
// end, and waits while a message that may reopen the thread is on its way
// (ADR-0061).
func (s *Server) cleanThread(ctx context.Context, id int64) (note string, err error) {
	defer s.lockClean(id)()
	row, err := s.store.Thread(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		s.log.Warn("read a closed thread to clean", "thread", id, "err", err)
		return "", errCleanLater
	}
	if row.State != store.ThreadClosed {
		return "", errReopened
	}
	if row.Repository == "" {
		return "", nil
	}
	// A message that may reopen the thread keeps its worktree until it is
	// noted: placed, it reopens the thread; gone, the thread stays closed
	// and the next pass looks again (ADR-0061).
	if waiting, err := s.store.MessagesToReopen(ctx, id); err != nil {
		s.log.Warn("look for messages that may reopen a closed thread", "thread", id, "err", err)
		return "", errCleanLater
	} else if waiting {
		return "", errCleanLater
	}
	for _, session := range []int64{row.Session, row.WorkerSession} {
		if session == 0 {
			continue
		}
		busy, err := s.engine.Busy(ctx, session)
		if err != nil {
			s.log.Warn("check a closed thread's session before cleanup", "thread", id, "session", session, "err", err)
			return "", errCleanLater
		}
		if busy {
			return "", errCleanLater
		}
	}
	path, branch := row.Cwd, row.Branch
	if path == "" {
		if s.profiles == nil {
			s.log.Warn("a closed thread has no profile for cleanup", "thread", id)
			return "", nil
		}
		target, err := s.profiles.Find(row.Agent)
		if err != nil {
			s.log.Warn("read a closed thread's profile for cleanup", "thread", id, "err", err)
			return "", nil
		}
		_, checkout := threadCheckout(target, row)
		if checkout == "" {
			s.log.Warn("a closed thread has no checkout for cleanup", "thread", id)
			return "", nil
		}
		path, branch = team.Worktree(checkout, row.Agent, team.Slug(row.Title, row.ID, row.Owner))
		if row.ReviewPull != 0 {
			branch = ""
		}
	}
	if !strings.HasSuffix(filepath.Base(path), "-t"+strconv.FormatInt(row.ID, 10)+"-"+row.Owner) {
		s.log.Warn("a closed thread's worktree is not named for it", "thread", id, "path", path)
		return "", nil
	}
	owner, err := s.store.SessionWorktreeAt(ctx, path)
	if err != nil {
		s.log.Warn("check an owner's session worktree before cleanup", "thread", id, "path", path, "err", err)
		return "", errCleanLater
	}
	if owner {
		s.log.Warn("a closed thread names an owner's session worktree", "thread", id, "path", path)
		return "", nil
	}
	r := worktree.Removal{Path: path}
	if row.ReviewPull != 0 {
		r.Head = row.ReviewHead
	} else {
		r.Branch = branch
		pulls, err := s.store.PullsOfThread(ctx, id)
		if err != nil {
			s.log.Warn("read a closed thread's pulls before cleanup", "thread", id, "err", err)
			return "", errCleanLater
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
		return "", nil
	}
	var kept worktree.Kept
	if errors.As(err, &kept) {
		s.log.Warn("kept a closed thread's worktree", "thread", id, "path", path, "why", kept.Reason)
		return team.WorktreeKept(path, kept.Reason), nil
	}
	s.log.Warn("could not clean a closed thread's worktree", "thread", id, "path", path, "err", err)
	return "", errCleanLater
}
