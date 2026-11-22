package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/changes"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// changesTimeout bounds the git of one read of a session's changes
// (ADR-0053): a checkout an agent filled with a huge file holds a read no
// longer.
const changesTimeout = 30 * time.Second

// maxChangesReads is how many reads of sessions' changes run at once: a
// board of many threads asks for each one's.
const maxChangesReads = 4

// errNoCheckout is a session's folder in no git checkout: no folder above
// it, itself included, holds a .git.
var errNoCheckout = errors.New("the session's folder is in no git checkout")

// refusal is a read pocketd will not run, and why, which the owner reads:
// a git older than changes.MinGit, a checkout git cannot read isolated
// (worktree.AttributesIsolated), or one git cannot read at all. It is
// conflict, but a check that could not tell (worktree.Unsure).
type refusal struct{ error }

func (r refusal) Unwrap() error { return r.error }

// fileMissing is a file no change of the session's names: 404.
type fileMissing struct{ error }

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
	if err := proto.CheckChangedPath(path); err != nil {
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
	var refused refusal
	var missing fileMissing
	switch {
	case errors.As(err, &missing):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, missing.Error())
	case errors.As(err, &refused) && !worktree.Unsure(err):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, refused.Error())
	case err != nil:
		s.internal(w, "read the session's changes", err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// gitWords is what git said of its failure, as err carries it after its
// exit status (Engine.Git's error), else err whole.
func gitWords(err error) string {
	var exit *worktree.GitExit
	if errors.As(err, &exit) {
		if _, words, ok := strings.Cut(err.Error(), exit.Error()+": "); ok && strings.TrimSpace(words) != "" {
			return strings.TrimSpace(words)
		}
	}
	return err.Error()
}

// isolated checks, in top, what no option of git's turns off, so git
// reads no attribute the checkout sets (worktree.AttributesIsolated): a
// read checks it before it reads and again once it read, and refuses an
// answer an attributes file may have changed between.
func (s *Server) isolated(ctx context.Context, top string) error {
	if err := worktree.AttributesIsolated(ctx, s.worktreeTools(), top); err != nil {
		if worktree.Unsure(err) {
			return err
		}
		return refusal{fmt.Errorf("git cannot read this checkout isolated, so pocketd shows none of its changes: %w", err)}
	}
	return nil
}

// inCheckout says whether a folder at or above dir holds a .git, as a
// command in the sandbox sees it: where one does, git that cannot name
// dir's checkout fails to read it, and the owner is told why.
func (s *Server) inCheckout(ctx context.Context, dir string) (bool, error) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		kind, err := s.engine.Kind(ctx, filepath.Join(d, ".git"))
		if err != nil {
			return false, err
		}
		if kind != "absent" {
			return true, nil
		}
		if d == filepath.Dir(d) {
			return false, nil
		}
	}
}

// spotOf is where session's changes stand: errNoCheckout when its folder
// is in no git checkout.
func (s *Server) spotOf(ctx context.Context, session store.SessionRow) (changesSpot, error) {
	var exit *worktree.GitExit
	top, err := s.engine.Git(ctx, session.Cwd, changes.TopArgs()...)
	switch {
	case errors.As(err, &exit):
		// No checkout, or one git cannot read: a .git above the folder
		// tells them apart, as git finds one.
		found, kindErr := s.inCheckout(ctx, session.Cwd)
		switch {
		case kindErr != nil:
			return changesSpot{}, kindErr
		case !found:
			return changesSpot{}, errNoCheckout
		}
		return changesSpot{}, refusal{fmt.Errorf("git cannot read the checkout of %s: %s", session.Cwd, gitWords(err))}
	case err != nil:
		return changesSpot{}, err
	case !filepath.IsAbs(top) || strings.ContainsAny(top, "\n\x00"):
		return changesSpot{}, fmt.Errorf("git named the checkout's top %q, not an absolute path", top)
	}
	// Every later git takes --no-lazy-fetch, which an older git refuses.
	version, err := s.engine.Git(ctx, top, changes.VersionArgs()...)
	if err != nil {
		return changesSpot{}, err
	}
	if !changes.GitVersionOK(version) {
		return changesSpot{}, refusal{fmt.Errorf("pocketd reads a session's changes with git %s or later, which fetches nothing a partial clone lacks: this host has %s",
			changes.MinGit, strings.TrimSpace(version))}
	}
	// What no option of git's turns off is checked before any read of the
	// checkout, as before a review's team_diff (ADR-0019).
	if err := s.isolated(ctx, top); err != nil {
		return changesSpot{}, err
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
	if head == "" {
		// No commit yet: the branch committed nothing, and what the folder
		// holds is counted from nothing, the empty tree of the
		// repository's object format.
		format, err := s.engine.Git(ctx, top, changes.ObjectFormatArgs()...)
		if err != nil {
			return changesSpot{}, err
		}
		empty, err := changes.EmptyTreeOf(strings.TrimSpace(format))
		if err != nil {
			return changesSpot{}, err
		}
		spot.base = changes.Base{Rule: proto.ChangesBaseHead}
		spot.since, spot.mergeBase = empty, empty
		return spot, nil
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
	spot.since, spot.mergeBase = head, head
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

// eachChange gives each file git's numstat with args names to each.
func (s *Server) eachChange(ctx context.Context, top string, args []string, each func(changes.File) error) error {
	var numstat changes.Numstat
	err := s.engine.GitEach(ctx, top, 0, func(record string) error {
		f, ok, err := numstat.Next(record)
		if err != nil || !ok {
			return err
		}
		return each(f)
	}, args...)
	if err != nil {
		return err
	}
	return numstat.Done()
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
	// rows finds a listed file by its path, and ends by either end of its
	// change: a rename's old path too.
	rows, ends := map[string]int{}, map[string][]int{}
	err = s.eachChange(ctx, spot.top, changes.NumstatArgs(spot.mergeBase), func(f changes.File) error {
		out.Added += f.Added
		out.Removed += f.Removed
		if len(out.Files) >= proto.MaxChangedFiles {
			out.More++
			return nil
		}
		i := len(out.Files)
		out.Files = append(out.Files, proto.ChangedFile{Path: f.Path, OldPath: f.OldPath, Added: f.Added, Removed: f.Removed, Binary: f.Binary})
		rows[f.Path] = i
		ends[f.Path] = append(ends[f.Path], i)
		if f.OldPath != "" && f.OldPath != f.Path {
			ends[f.OldPath] = append(ends[f.OldPath], i)
		}
		return nil
	})
	if err != nil {
		return proto.Changes{}, err
	}
	// Which listed files a commit changed, and which the folder changes
	// since HEAD, by either end of each change: a file a commit edited and
	// the folder then renamed is both.
	mark := func(args []string, set func(*proto.ChangedFile)) error {
		return s.eachChange(ctx, spot.top, args, func(f changes.File) error {
			for _, path := range []string{f.Path, f.OldPath} {
				for _, i := range ends[path] {
					set(&out.Files[i])
				}
			}
			return nil
		})
	}
	if spot.head != "" && spot.mergeBase != spot.head {
		if err := mark(changes.CommittedArgs(spot.mergeBase, spot.head), func(f *proto.ChangedFile) { f.Committed = true }); err != nil {
			return proto.Changes{}, err
		}
	}
	if err := mark(changes.NumstatArgs(spot.since), func(f *proto.ChangedFile) { f.Uncommitted = true }); err != nil {
		return proto.Changes{}, err
	}
	// The new files git does not track yet. One at a path a tracked change
	// names already is that change: git counts the file gone from the
	// repository, as a commit would make it, and its row says so. The
	// first MaxCountedNew are counted, listed or not.
	type candidate struct {
		path string
		row  int
	}
	var count []candidate
	err = s.engine.GitEach(ctx, spot.top, 0, func(path string) error {
		if _, tracked := rows[path]; path == "" || tracked {
			return nil
		}
		row := -1
		if len(out.Files) < proto.MaxChangedFiles {
			row = len(out.Files)
			rows[path] = row
			out.Files = append(out.Files, proto.ChangedFile{Path: path, New: true, Uncommitted: true, Uncounted: true})
		} else {
			out.More++
		}
		// A repository inside the checkout git lists as its folder, with
		// a slash at its end: no file, so no line of it is counted.
		if len(count) < proto.MaxCountedNew && !strings.HasSuffix(path, "/") {
			count = append(count, candidate{path, row})
		}
		return nil
	}, changes.NewArgs()...)
	if err != nil {
		return proto.Changes{}, err
	}
	for _, c := range count {
		// Read by pocketd's own read in the sandbox, below the top and
		// through no link (Engine.ReadIn): a folder swapped for a link
		// since git listed the file leads the read nowhere.
		kind, data, err := s.engine.ReadIn(ctx, spot.top, c.path, changes.MaxCountBytes)
		if err != nil {
			return proto.Changes{}, err
		}
		added, binary, counted := changes.CountNew(kind, data)
		if !counted {
			continue
		}
		out.Added += added
		if c.row >= 0 {
			f := &out.Files[c.row]
			f.Added, f.Binary, f.Uncounted = added, binary, false
		}
	}
	// An attributes file that came while git read refuses the answer.
	if err := s.isolated(ctx, spot.top); err != nil {
		return proto.Changes{}, err
	}
	return out, nil
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
	var record *changes.File
	found := errors.New("found")
	err = s.eachChange(ctx, spot.top, changes.NumstatArgs(spot.mergeBase), func(f changes.File) error {
		if f.Path != path {
			return nil
		}
		record = &f
		return found
	})
	if err != nil && !errors.Is(err, found) {
		return proto.FileDiff{}, err
	}
	out := proto.FileDiff{SessionID: session.ID, Path: path, MergeBase: spot.mergeBase}
	switch {
	case record != nil:
		// The ends of the change below which another change lies: a file
		// that became a folder, or the other way, whose folder's changes
		// its patch leaves out.
		folders := map[string]bool{}
		err := s.eachChange(ctx, spot.top, changes.NumstatArgs(spot.mergeBase), func(f changes.File) error {
			for _, p := range changes.Under(f, record.Path, record.OldPath) {
				folders[p] = true
			}
			return nil
		})
		if err != nil {
			return proto.FileDiff{}, err
		}
		below := slices.Sorted(maps.Keys(folders))
		page, more, err := s.engine.GitPage(ctx, spot.top, 0, proto.MaxFileDiff, changes.PatchArgs(spot.mergeBase, record.OldPath, record.Path, below...)...)
		if err != nil {
			return proto.FileDiff{}, err
		}
		out.Diff, out.Cut = changes.Fit(page, more, proto.MaxFileDiff)
	default:
		listed, err := s.engine.Git(ctx, spot.top, changes.NewArgs(path)...)
		if err != nil {
			return proto.FileDiff{}, err
		}
		if !strings.Contains("\x00"+listed+"\x00", "\x00"+path+"\x00") {
			return proto.FileDiff{}, fileMissing{fmt.Errorf("no change of session %d's checkout names %s", session.ID, path)}
		}
		kind, data, err := s.engine.ReadIn(ctx, spot.top, path, proto.MaxFileDiff)
		if err != nil {
			return proto.FileDiff{}, err
		}
		if kind == changes.KindNone {
			return proto.FileDiff{}, fileMissing{fmt.Errorf("%s is no longer a file of session %d's checkout", path, session.ID)}
		}
		out.Diff, out.Cut = changes.NewFileDiff(path, kind, data, proto.MaxFileDiff)
	}
	if err := s.isolated(ctx, spot.top); err != nil {
		return proto.FileDiff{}, err
	}
	return out, nil
}
