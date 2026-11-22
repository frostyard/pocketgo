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

// cleanLater queues closed thread id, at the close that came after its
// reopened-th reopen (ADR-0061), without starting a pass or taking any
// team's lock: publishThread may already hold workMu. A close decided in
// this run is not queued again; a later close of the same thread is.
func (s *Server) cleanLater(id int64, reopened int) {
	s.cleanMu.Lock()
	defer s.cleanMu.Unlock()
	if done, ok := s.cleanDone[id]; ok && done == reopened {
		return
	}
	if s.cleanQueue == nil {
		s.cleanQueue = map[int64]int{}
	}
	s.cleanQueue[id] = reopened
}

// keptNote is a kept worktree's line not yet written in its thread's
// report, with the close it is of, by the thread's reopen count then.
type keptNote struct {
	note     string
	reopened int
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
// Each decision is of one close, by the thread's reopen count: a note of
// a close a reopen ended is dropped, and the thread is looked at again as
// it is (ADR-0061; Codex review of #121, round 1).
func (s *Server) cleanPass(ctx context.Context) {
	s.cleanMu.Lock()
	if s.cleansDue == nil {
		s.cleansDue = map[int64]bool{}
	}
	for id := range s.cleanQueue {
		s.cleansDue[id] = true
	}
	s.cleanMu.Unlock()
	for id := range s.cleansDue {
		pending, ok := s.cleanNotes[id]
		if !ok {
			kept, reopened, err := s.cleanThread(ctx, id)
			if errors.Is(err, errReopened) {
				// It reopened before its pass: it is decided at its next
				// close, not now (ADR-0061).
				s.cleanMu.Lock()
				if queued, ok := s.cleanQueue[id]; ok && queued < reopened {
					delete(s.cleanQueue, id)
				}
				if _, ok := s.cleanQueue[id]; !ok {
					delete(s.cleansDue, id)
				}
				s.cleanMu.Unlock()
				continue
			}
			if err != nil {
				continue
			}
			pending = keptNote{note: kept, reopened: reopened}
		}
		// A kept worktree's note must reach the report before the thread is
		// decided: a failed write keeps the note for the next pass. One of a
		// close a reopen ended is dropped, and the next pass looks at the
		// thread as it is then (ADR-0061).
		if pending.note != "" {
			written, stale := s.noteKept(ctx, id, pending)
			if stale {
				delete(s.cleanNotes, id)
				continue
			}
			if !written {
				if s.cleanNotes == nil {
					s.cleanNotes = map[int64]keptNote{}
				}
				s.cleanNotes[id] = pending
				continue
			}
		}
		delete(s.cleanNotes, id)
		s.cleanMu.Lock()
		if s.cleanDone == nil {
			s.cleanDone = map[int64]int{}
		}
		s.cleanDone[id] = pending.reopened
		// A later close queued meanwhile stays queued, for the next pass.
		if queued, ok := s.cleanQueue[id]; ok && queued == pending.reopened {
			delete(s.cleanQueue, id)
		}
		if _, ok := s.cleanQueue[id]; !ok {
			delete(s.cleansDue, id)
		}
		s.cleanMu.Unlock()
	}
}

// noteKept adds a kept worktree's line to a closed thread's report under
// workMu, only while the thread is still closed by the close the line is
// of. written is false only when the write failed and should be retried;
// stale says the thread reopened since, and takes no line (ADR-0061).
func (s *Server) noteKept(ctx context.Context, id int64, kept keptNote) (written, stale bool) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	row, changed, err := s.store.NoteClosedThreadAt(ctx, id, kept.reopened, kept.note)
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
	return true, !changed && (row.State != store.ThreadClosed || row.Reopened != kept.reopened)
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
func (s *Server) cleanThread(ctx context.Context, id int64) (note string, reopened int, err error) {
	defer s.lockClean(id)()
	// The thread and the messages that may reopen it, from one snapshot: a
	// noting that reopens it ends its message's follow in the same
	// transaction, and no new such message is kept while the lock is held
	// (ADR-0061; Codex review of #121, round 1).
	row, waiting, err := s.store.CleanState(ctx, id)
	if s.afterCleanState != nil {
		s.afterCleanState(id)
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", 0, nil
	}
	if err != nil {
		s.log.Warn("read a closed thread to clean", "thread", id, "err", err)
		return "", 0, errCleanLater
	}
	reopened = row.Reopened
	if row.State != store.ThreadClosed {
		return "", reopened, errReopened
	}
	if row.Repository == "" {
		return "", reopened, nil
	}
	// A message that may reopen the thread keeps its worktree until it is
	// noted: placed, it reopens the thread; gone, the thread stays closed
	// and the next pass looks again (ADR-0061).
	if waiting {
		return "", reopened, errCleanLater
	}
	for _, session := range []int64{row.Session, row.WorkerSession} {
		if session == 0 {
			continue
		}
		busy, err := s.engine.Busy(ctx, session)
		if err != nil {
			s.log.Warn("check a closed thread's session before cleanup", "thread", id, "session", session, "err", err)
			return "", reopened, errCleanLater
		}
		if busy {
			return "", reopened, errCleanLater
		}
	}
	path, branch := row.Cwd, row.Branch
	if path == "" {
		if s.profiles == nil {
			s.log.Warn("a closed thread has no profile for cleanup", "thread", id)
			return "", reopened, nil
		}
		target, err := s.profiles.Find(row.Agent)
		if err != nil {
			s.log.Warn("read a closed thread's profile for cleanup", "thread", id, "err", err)
			return "", reopened, nil
		}
		_, checkout := threadCheckout(target, row)
		if checkout == "" {
			s.log.Warn("a closed thread has no checkout for cleanup", "thread", id)
			return "", reopened, nil
		}
		path, branch = team.Worktree(checkout, row.Agent, team.Slug(row.Title, row.ID, row.Owner))
		if row.ReviewPull != 0 {
			branch = ""
		}
	}
	if !strings.HasSuffix(filepath.Base(path), "-t"+strconv.FormatInt(row.ID, 10)+"-"+row.Owner) {
		s.log.Warn("a closed thread's worktree is not named for it", "thread", id, "path", path)
		return "", reopened, nil
	}
	owner, err := s.store.SessionWorktreeAt(ctx, path)
	if err != nil {
		s.log.Warn("check an owner's session worktree before cleanup", "thread", id, "path", path, "err", err)
		return "", reopened, errCleanLater
	}
	if owner {
		s.log.Warn("a closed thread names an owner's session worktree", "thread", id, "path", path)
		return "", reopened, nil
	}
	r := worktree.Removal{Path: path}
	if row.ReviewPull != 0 {
		r.Head = row.ReviewHead
	} else {
		r.Branch = branch
		pulls, err := s.store.PullsOfThread(ctx, id)
		if err != nil {
			s.log.Warn("read a closed thread's pulls before cleanup", "thread", id, "err", err)
			return "", reopened, errCleanLater
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
		return "", reopened, nil
	}
	var kept worktree.Kept
	if errors.As(err, &kept) {
		s.log.Warn("kept a closed thread's worktree", "thread", id, "path", path, "why", kept.Reason)
		return team.WorktreeKept(path, kept.Reason), reopened, nil
	}
	s.log.Warn("could not clean a closed thread's worktree", "thread", id, "path", path, "err", err)
	return "", reopened, errCleanLater
}
