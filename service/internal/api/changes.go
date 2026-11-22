package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/changes"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// changesTimeout bounds the git of one read of a session's changes
// (ADR-0053): a checkout an agent filled with a named pipe or a huge file
// holds a read no longer.
const changesTimeout = 30 * time.Second

// maxChangesReads is how many reads of sessions' changes run at once: a
// board of many threads asks for each one's.
const maxChangesReads = 4

// errNoCheckout is a session's folder git says is in no checkout.
var errNoCheckout = errors.New("the session's folder is in no git checkout")

// notIsolated is a checkout git cannot read isolated
// (worktree.AttributesIsolated): pocketd shows none of its changes.
type notIsolated struct{ error }

func (n notIsolated) Unwrap() error { return n.error }

// changesSpot is where a read of a session's changes stands: its checkout,
// its HEAD and the base its changes are counted from.
type changesSpot struct {
	top, branch, head string
	base              changes.Base
	// mergeBase is the commit the changes are counted from, and since
	// the commit the folder is compared with for what is not committed:
	// HEAD, or the empty tree before the first commit.
	mergeBase, since string
}

// sessionChanges is GET /v1/sessions/{id}/changes (rule 63).
func (s *Server) sessionChanges(w http.ResponseWriter, r *http.Request) {
	s.readChanges(w, r, func(ctx context.Context, row store.SessionRow) (any, error) {
		out, err := s.changesOf(ctx, row)
		return out, err
	})
}

// changesFile is GET /v1/sessions/{id}/changes/file?path= (rule 64).
func (s *Server) changesFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if err := changes.CheckPath(path); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	s.readChanges(w, r, func(ctx context.Context, row store.SessionRow) (any, error) {
		return s.fileDiffOf(ctx, row, path)
	})
}

// readChanges finds the session of the request, takes a place among the
// reads that run (maxChangesReads), runs read with changesTimeout, and
// answers what it gives or why it failed.
func (s *Server) readChanges(w http.ResponseWriter, r *http.Request, read func(ctx context.Context, row store.SessionRow) (any, error)) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	row, err := s.store.Session(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", id))
		return
	} else if err != nil {
		s.internal(w, "read session", err)
		return
	}
	select {
	case s.changesSlots <- struct{}{}:
		defer func() { <-s.changesSlots }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := s.withTimeout(r.Context(), changesTimeout)
	defer cancel()
	out, err := read(ctx, row)
	var isolation notIsolated
	var missing fileMissing
	switch {
	case errors.As(err, &missing):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, missing.Error())
	case errors.As(err, &isolation) && !worktree.Unsure(err):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "git cannot read this checkout isolated, so pocketd shows none of its changes: "+isolation.Error())
	case err != nil:
		s.internal(w, "read the session's changes", err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// fileMissing is a file no change of the session's names: 404.
type fileMissing struct{ error }

// spotOf is where session's changes stand: errNoCheckout when its folder
// is in no git checkout.
func (s *Server) spotOf(ctx context.Context, session store.SessionRow) (changesSpot, error) {
	var exit *worktree.GitExit
	top, err := s.engine.Git(ctx, session.Cwd, changes.TopArgs()...)
	switch {
	case errors.As(err, &exit):
		return changesSpot{}, errNoCheckout
	case err != nil:
		return changesSpot{}, err
	case !filepath.IsAbs(top) || strings.ContainsAny(top, "\n\x00"):
		return changesSpot{}, fmt.Errorf("git named the checkout's top %q, not an absolute path", top)
	}
	// What no option of git's turns off is checked before any read of the
	// checkout, as before a review's team_diff (ADR-0019).
	if err := worktree.AttributesIsolated(ctx, s.worktreeTools(), top); err != nil {
		return changesSpot{}, notIsolated{err}
	}
	spot := changesSpot{top: top}
	head, err := s.engine.Git(ctx, top, changes.HeadArgs()...)
	switch {
	case errors.As(err, &exit) && exit.Status == 1:
		head = ""
	case err != nil:
		return changesSpot{}, err
	case !changes.FullCommit(head):
		return changesSpot{}, fmt.Errorf("git named HEAD's commit %q", head)
	}
	spot.head = head
	ref, err := s.engine.Git(ctx, top, changes.BranchArgs()...)
	switch {
	case errors.As(err, &exit) && exit.Status == 1:
	case err != nil:
		return changesSpot{}, err
	default:
		spot.branch = changes.ShortBranch(ref)
	}
	pullBase, err := s.pullBaseOf(ctx, session.ID)
	if err != nil {
		return changesSpot{}, err
	}
	out, err := s.engine.Git(ctx, top, changes.RefsArgs(changes.Candidates(pullBase))...)
	if err != nil {
		return changesSpot{}, err
	}
	refs, err := changes.ParseRefs(out)
	if err != nil {
		return changesSpot{}, err
	}
	spot.base = changes.ChooseBase(pullBase, refs)
	spot.since = head
	if head == "" {
		// No commit yet: the branch committed nothing, and what the folder
		// holds is counted from nothing.
		spot.since = changes.EmptyTree
		spot.mergeBase = changes.EmptyTree
		return spot, nil
	}
	spot.mergeBase = head
	if spot.base.Commit == "" {
		return spot, nil
	}
	mergeBase, err := s.engine.Git(ctx, top, changes.MergeBaseArgs(spot.base.Commit, head)...)
	switch {
	case errors.As(err, &exit) && exit.Status == 1:
		// The branch shares no commit with its base: only what is not
		// committed is counted.
		spot.base = changes.Base{Rule: proto.ChangesBaseHead}
	case err != nil:
		return changesSpot{}, err
	case !changes.FullCommit(mergeBase):
		return changesSpot{}, fmt.Errorf("git named the merge base %q", mergeBase)
	default:
		spot.mergeBase = mergeBase
	}
	return spot, nil
}

// pullBaseOf is the base branch of the pull request of session's thread
// (changes.PullBase): the session is the thread's specialist, its worker,
// or a review of one of its pull requests; "" for none.
func (s *Server) pullBaseOf(ctx context.Context, session int64) (string, error) {
	thread, err := s.store.ThreadOfSession(ctx, session)
	if errors.Is(err, store.ErrNotFound) {
		thread, err = s.store.ThreadOfWorker(ctx, session)
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	var rows []store.PullRow
	if thread.ReviewPull != 0 {
		p, err := s.store.PullByID(ctx, thread.ReviewPull)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		} else if err == nil {
			rows = append(rows, p)
		}
	} else if rows, err = s.store.PullsOfThread(ctx, thread.ID); err != nil {
		return "", err
	}
	pulls := make([]changes.Pull, 0, len(rows))
	for _, p := range rows {
		pulls = append(pulls, changes.Pull{ID: p.ID, Open: p.State == store.PullOpen, Base: p.Base})
	}
	return changes.PullBase(pulls), nil
}

// changesOf reads what session changed in its checkout (rule 63).
func (s *Server) changesOf(ctx context.Context, session store.SessionRow) (proto.Changes, error) {
	spot, err := s.spotOf(ctx, session)
	if errors.Is(err, errNoCheckout) {
		return proto.Changes{SessionID: session.ID, Files: []proto.ChangedFile{}}, nil
	} else if err != nil {
		return proto.Changes{}, err
	}
	out := proto.Changes{SessionID: session.ID, Repository: true, Top: spot.top, Branch: spot.branch, Head: spot.head,
		Base: spot.base.Name, BaseRule: spot.base.Rule, MergeBase: spot.mergeBase, Files: []proto.ChangedFile{}}
	listed := map[string]int{}
	keep := func(f proto.ChangedFile) {
		if len(out.Files) >= proto.MaxChangedFiles {
			out.More++
			return
		}
		listed[f.Path] = len(out.Files)
		out.Files = append(out.Files, f)
	}
	var numstat changes.Numstat
	err = s.engine.GitEach(ctx, spot.top, 0, func(record string) error {
		f, ok, err := numstat.Next(record)
		if err != nil || !ok {
			return err
		}
		out.Added += f.Added
		out.Removed += f.Removed
		keep(proto.ChangedFile{Path: f.Path, OldPath: f.OldPath, Added: f.Added, Removed: f.Removed, Binary: f.Binary})
		return nil
	}, changes.NumstatArgs(spot.mergeBase)...)
	if err == nil {
		err = numstat.Done()
	}
	if err != nil {
		return proto.Changes{}, err
	}
	// Which listed files a commit changed, and which the folder changes
	// since HEAD.
	mark := func(args []string, set func(*proto.ChangedFile)) error {
		return s.engine.GitEach(ctx, spot.top, 0, func(path string) error {
			if i, ok := listed[path]; ok {
				set(&out.Files[i])
			}
			return nil
		}, args...)
	}
	if spot.head != "" && spot.mergeBase != spot.head {
		if err := mark(changes.CommittedArgs(spot.mergeBase, spot.head), func(f *proto.ChangedFile) { f.Committed = true }); err != nil {
			return proto.Changes{}, err
		}
	}
	if err := mark(changes.UncommittedArgs(spot.since), func(f *proto.ChangedFile) { f.Uncommitted = true }); err != nil {
		return proto.Changes{}, err
	}
	// The new files git does not track yet, the first MaxCountedNew of
	// them counted.
	var fresh []int
	err = s.engine.GitEach(ctx, spot.top, 0, func(path string) error {
		if path == "" {
			return nil
		}
		// A repository inside the checkout git lists as its folder, with
		// a slash at its end: no file, so no line of it is counted.
		if len(out.Files) < proto.MaxChangedFiles && !strings.HasSuffix(path, "/") {
			fresh = append(fresh, len(out.Files))
		}
		keep(proto.ChangedFile{Path: path, New: true, Uncommitted: true, Uncounted: true})
		return nil
	}, changes.NewArgs()...)
	if err != nil {
		return proto.Changes{}, err
	}
	for n, i := range fresh {
		if n >= proto.MaxCountedNew {
			break
		}
		f := &out.Files[i]
		count, counted, err := s.countNew(ctx, spot.top, f.Path)
		if err != nil {
			return proto.Changes{}, err
		} else if !counted {
			continue
		}
		f.Added, f.Binary, f.Uncounted = count.Added, count.Binary, false
		out.Added += count.Added
	}
	return out, nil
}

// countNew counts the lines of the new file at path in top, as git diff
// counts them against nothing. A file git answers it cannot count, gone or
// changed into another kind since it was listed, is not counted: counted
// is false. git that did not run is an error.
func (s *Server) countNew(ctx context.Context, top, path string) (count changes.File, counted bool, err error) {
	page, _, err := s.engine.GitPage(ctx, top, 0, 4096, changes.NewNumstatArgs(path)...)
	page, err = changes.Differs(page, err)
	var exit *worktree.GitExit
	if errors.As(err, &exit) {
		return changes.File{}, false, nil
	} else if err != nil {
		return changes.File{}, false, err
	}
	if count, err = changes.NewCount(page); err != nil {
		return changes.File{}, false, nil
	}
	return count, true, nil
}

// fileDiffOf is the diff of the changed file at path of session's checkout
// (rule 64): only a file the changes list, a tracked one git's numstat
// names or a new one git lists, is read.
func (s *Server) fileDiffOf(ctx context.Context, session store.SessionRow, path string) (proto.FileDiff, error) {
	spot, err := s.spotOf(ctx, session)
	if errors.Is(err, errNoCheckout) {
		return proto.FileDiff{}, fileMissing{fmt.Errorf("session %d's folder is in no git checkout", session.ID)}
	} else if err != nil {
		return proto.FileDiff{}, err
	}
	var args []string
	var numstat changes.Numstat
	found := errors.New("found")
	err = s.engine.GitEach(ctx, spot.top, 0, func(record string) error {
		f, ok, err := numstat.Next(record)
		if err != nil || !ok || f.Path != path {
			return err
		}
		args = changes.PatchArgs(spot.mergeBase, f.OldPath, f.Path)
		return found
	}, changes.NumstatArgs(spot.mergeBase)...)
	if err != nil && !errors.Is(err, found) {
		return proto.FileDiff{}, err
	}
	fresh := false
	if args == nil {
		listed, err := s.engine.Git(ctx, spot.top, changes.NewArgs(path)...)
		if err != nil {
			return proto.FileDiff{}, err
		}
		for _, p := range strings.Split(listed, "\x00") {
			if p == path {
				args, fresh = changes.NewPatchArgs(path), true
				break
			}
		}
	}
	if args == nil {
		return proto.FileDiff{}, fileMissing{fmt.Errorf("no change of session %d's checkout names %s", session.ID, path)}
	}
	page, more, err := s.engine.GitPage(ctx, spot.top, 0, proto.MaxFileDiff, args...)
	if fresh {
		page, err = changes.Differs(page, err)
	}
	if err != nil {
		return proto.FileDiff{}, err
	}
	if more {
		page = changes.CutAtLine(page)
	}
	return proto.FileDiff{SessionID: session.ID, Path: path, MergeBase: spot.mergeBase, Diff: page, Cut: more}, nil
}
