package state

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// The Changes view (ADR-0053): what a session changed in its git checkout,
// before any pull request, read from pocketd (GET /v1/sessions/{id}/changes
// and its file's diff). The client keeps the changes of the sessions it
// shows a count for, the open session's and those of the board's threads,
// and fetches them again when their run ends.

// SessionChanges is what the client knows of one session's changes: where
// their fetches stand and the newest answer.
type SessionChanges struct {
	// Status is ListLoading until a fetch answered; Err says why the last
	// one failed, which keeps the changes an earlier one brought.
	Status  ListStatus
	Err     string
	Changes proto.Changes
}

// sessionChanges is a session's changes and the fetch last applied.
type sessionChanges struct {
	SessionChanges
	list list
}

// ShownDiff is the file whose diff the Changes view shows.
type ShownDiff struct {
	Session int64
	Path    string
	// Status is ListLoading until its first fetch answered; Err says why
	// the last one failed, which keeps the diff an earlier one brought.
	Status ListStatus
	Err    string
	Diff   proto.FileDiff
	list   list
}

// Changes is what the client knows of session id's changes; false before
// the client fetched them.
func (d *Data) Changes(id int64) (SessionChanges, bool) {
	c := d.changes[id]
	if c == nil {
		return SessionChanges{}, false
	}
	return c.SessionChanges, true
}

// BeginChanges starts a fetch of session id's changes: Begin, with the
// session's entry made, loading until an answer comes.
func (d *Data) BeginChanges(id int64) Fetch {
	if d.changes == nil {
		d.changes = map[int64]*sessionChanges{}
	}
	if d.changes[id] == nil {
		d.rev++
		d.changes[id] = &sessionChanges{}
	}
	return d.Begin()
}

// SetChanges records fetch f's answer for session id, unless a fetch of
// them begun later answered already: a run's end fetches again, and the
// older answer would put back what the run changed since. The diff shown
// of a file of the session that the whole list no longer names goes: the
// file is as at the base again, and pocketd shows no diff of it.
func (d *Data) SetChanges(f Fetch, id int64, c proto.Changes) {
	sc := d.changes[id]
	if sc == nil || !sc.list.answer(f) {
		return
	}
	d.rev++
	if c.Files == nil {
		c.Files = []proto.ChangedFile{}
	}
	sc.Status, sc.Err, sc.Changes = ListLoaded, "", c
	if shown := d.diff; shown != nil && shown.Session == id && c.More == 0 &&
		!slices.ContainsFunc(c.Files, func(f proto.ChangedFile) bool { return f.Path == shown.Path }) {
		d.diff = nil
	}
}

// SetChangesError records that fetch f of session id's changes failed,
// unless one begun later answered; the changes an earlier one brought stay.
func (d *Data) SetChangesError(f Fetch, id int64, err string) {
	sc := d.changes[id]
	if sc == nil || !sc.list.answer(f) {
		return
	}
	d.rev++
	sc.Status, sc.Err = ListFailed, err
}

// ForgetChanges drops the changes of every session but keep: a session the
// client no longer shows a count for is fetched afresh when it does again.
func (d *Data) ForgetChanges(keep map[int64]bool) {
	for id := range d.changes {
		if !keep[id] {
			d.rev++
			delete(d.changes, id)
		}
	}
}

// Diff is the file whose diff the Changes view shows; false for none.
func (d *Data) Diff() (ShownDiff, bool) {
	if d.diff == nil {
		return ShownDiff{}, false
	}
	return *d.diff, true
}

// BeginDiff starts a fetch of the diff of the file at path in session
// id's changes, and shows that file: another file's diff goes, the same
// file's stays shown until the answer.
func (d *Data) BeginDiff(id int64, path string) Fetch {
	if d.diff == nil || d.diff.Session != id || d.diff.Path != path {
		d.rev++
		d.diff = &ShownDiff{Session: id, Path: path}
	}
	return d.Begin()
}

// SetDiff records fetch f's diff of the file at path of session id, unless
// another file shows now or a fetch begun later answered.
func (d *Data) SetDiff(f Fetch, id int64, path string, diff proto.FileDiff) {
	if d.diff == nil || d.diff.Session != id || d.diff.Path != path || !d.diff.list.answer(f) {
		return
	}
	d.rev++
	d.diff.Status, d.diff.Err, d.diff.Diff = ListLoaded, "", diff
}

// SetDiffError records that fetch f of the file's diff failed, by the rule
// of SetDiff; a diff an earlier fetch brought stays.
func (d *Data) SetDiffError(f Fetch, id int64, path, err string) {
	if d.diff == nil || d.diff.Session != id || d.diff.Path != path || !d.diff.list.answer(f) {
		return
	}
	d.rev++
	d.diff.Status, d.diff.Err = ListFailed, err
}

// CloseDiff shows no file's diff.
func (d *Data) CloseDiff() {
	if d.diff != nil {
		d.rev++
		d.diff = nil
	}
}

// ChangesTracked are the sessions whose changes the client keeps current:
// the open session, for its header's count and its Changes view, and the
// sessions of the threads not closed that work on a repository, for their
// cards' counts.
func (d *Data) ChangesTracked() map[int64]bool {
	out := map[int64]bool{}
	if id := d.OpenID(); id != 0 {
		out[id] = true
	}
	for _, t := range d.Threads() {
		if t.State != proto.ThreadClosed && t.Session != 0 && t.Repository != "" {
			out[t.Session] = true
		}
	}
	return out
}

// RunEnded says a session's changes are fetched again: its state was not
// idle, a run working or waiting for the owner, and now it is idle.
func RunEnded(was, now proto.SessionState) bool {
	return was != proto.SessionIdle && was != "" && now == proto.SessionIdle
}

// NotRead is the count of a session whose last read of its changes
// failed: the header still offers the Changes, which say why and retry.
const NotRead = "Changes not read"

// ChangesSummary is the count a session's header shows ("4 files +120
// −8"), "" when there is none to show: changes not loaded yet, or a folder
// in no checkout. A checkout with no change says so, and a read that
// failed says it was not read (NotRead), whatever an earlier one said: the
// owner sees why, never a count or "No changes" that may be stale.
func ChangesSummary(c SessionChanges, ok bool) string {
	switch {
	case !ok || c.Status == ListLoading:
		return ""
	case c.Status == ListFailed:
		return NotRead
	case !c.Changes.Repository:
		return ""
	}
	n := len(c.Changes.Files) + c.Changes.More
	if n == 0 {
		return "No changes"
	}
	return fmt.Sprintf("%s +%d −%d", plural(n, "file", "files"), c.Changes.Added, c.Changes.Removed)
}

// CardChanges is the count a thread's card shows: ChangesSummary of a read
// that succeeded with a change, and none else.
func CardChanges(c SessionChanges, ok bool) string {
	if s := ChangesSummary(c, ok); s != "No changes" && s != NotRead {
		return s
	}
	return ""
}

// FileCounts is a changed file's lines as its row shows them: "+3 −1",
// "binary", or, for a new file pocketd did not count, "new".
func FileCounts(f proto.ChangedFile) string {
	switch {
	case f.Binary:
		return "binary"
	case f.Uncounted:
		return "not counted"
	}
	return fmt.Sprintf("+%d −%d", f.Added, f.Removed)
}

// FileState says where a changed file's change is: committed, not yet, or
// both, or new to git.
func FileState(f proto.ChangedFile) string {
	switch {
	case !CanDiff(f):
		return "a repository inside the checkout, not tracked"
	case f.New:
		return "new, not committed"
	case f.Committed && f.Uncommitted:
		return "committed, and changed since"
	case f.Committed:
		return "committed"
	}
	return "not committed"
}

// CanDiff says whether a changed file has a diff pocketd shows: not a
// repository inside the checkout, which git lists as its folder, with a
// slash at its end (rule 63), and pocketd refuses as a file's path.
func CanDiff(f proto.ChangedFile) bool { return !strings.HasSuffix(f.Path, "/") }

// FileName is a changed file's path as its row shows it, a rename's old
// path first.
func FileName(f proto.ChangedFile) string {
	if f.OldPath != "" && f.OldPath != f.Path {
		return f.OldPath + " → " + f.Path
	}
	return f.Path
}

// MoreFiles is the line under a list that left files out, "" for none.
func MoreFiles(c proto.Changes) string {
	if c.More == 0 {
		return ""
	}
	return fmt.Sprintf("%s more not listed: pocketd lists the first %d.", plural(c.More, "file", "files"), proto.MaxChangedFiles)
}

// BaseNote says what the changes are counted against: the branch and the
// commit where the session's branch leaves it, and how pocketd chose it.
func BaseNote(c proto.Changes) string {
	branch := c.Branch
	if branch == "" {
		branch = "a detached HEAD"
	}
	switch c.BaseRule {
	case proto.ChangesBasePull:
		return fmt.Sprintf("%s against %s, the pull request's base, since %s", branch, c.Base, shortCommit(c.MergeBase))
	case proto.ChangesBaseDefault:
		return fmt.Sprintf("%s against %s since %s", branch, c.Base, shortCommit(c.MergeBase))
	}
	if c.Head == "" {
		return branch + ", no commit yet: every file counts as new"
	}
	return branch + ", no base branch found: only what is not committed"
}

// shortCommit is a commit as git shows it short.
func shortCommit(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// CutNote is the line over a diff pocketd cut, "" for one it sent whole.
func CutNote(d proto.FileDiff) string {
	if !d.Cut {
		return ""
	}
	return fmt.Sprintf("Only the first %d KB of this diff show: the rest is in the checkout.", proto.MaxFileDiff>>10)
}

// NoDiff is what the Changes view shows for a diff git printed nothing
// of, as a file whose mode alone changed back: "" for a diff with text.
func NoDiff(d proto.FileDiff) string {
	if strings.TrimSpace(d.Diff) != "" {
		return ""
	}
	return "git shows no lines for this file."
}

// changesKey names a session's changes in fetching.
func changesKey(id int64) string { return fmt.Sprintf("changes:%d", id) }

// LoadChanges fetches session id's changes. One asked for while its fetch
// is in flight follows it, once: an answer begun before a run ended may
// miss what the run changed.
func (s *Store) LoadChanges(id int64) {
	if s.client == nil || id == 0 {
		return
	}
	if s.changesLoading[id] {
		s.changesAgain[id] = true
		return
	}
	s.changesLoading[id] = true
	f := s.Data.BeginChanges(id)
	gen := s.gen
	var out proto.Changes
	s.do(fmt.Sprintf("Reading session %d's changes", id), func(ctx context.Context, c *proto.Client) (err error) {
		out, err = c.Changes(ctx, id)
		return err
	}, s.track(changesKey(id), func(err error) {
		if gen != s.gen {
			return
		}
		delete(s.changesLoading, id)
		if err != nil {
			s.Data.SetChangesError(f, id, err.Error())
		} else {
			s.Data.SetChanges(f, id, out)
		}
		// The fetch that follows goes only while the session is still
		// tracked: one the client stopped showing is fetched afresh when
		// it shows it again.
		if s.changesAgain[id] {
			delete(s.changesAgain, id)
			if s.Data.ChangesTracked()[id] {
				s.LoadChanges(id)
			}
		}
	}))
}

// ChangesLoading says a fetch of session id's changes is in flight.
func (s *Store) ChangesLoading(id int64) bool { return s.changesLoading[id] }

// ShowDiff shows the diff of the file at path of session id's changes,
// and fetches it.
func (s *Store) ShowDiff(id int64, path string) {
	if s.client == nil {
		return
	}
	f := s.Data.BeginDiff(id, path)
	var out proto.FileDiff
	s.do("Reading the diff of "+path, func(ctx context.Context, c *proto.Client) (err error) {
		out, err = c.ChangesFile(ctx, id, path)
		return err
	}, s.track(changesKey(id)+":file", func(err error) {
		if err != nil {
			s.Data.SetDiffError(f, id, path, err.Error())
			return
		}
		s.Data.SetDiff(f, id, path, out)
	}))
}

// RefreshChanges fetches session id's changes again, and the diff shown
// of its file: the Changes view's Refresh, and a run's end.
func (s *Store) RefreshChanges(id int64) {
	s.LoadChanges(id)
	if d, ok := s.Data.Diff(); ok && d.Session == id {
		s.ShowDiff(id, d.Path)
	}
}

// syncChanges keeps the tracked sessions' changes current
// (Data.ChangesTracked): it fetches those not fetched yet, and again those
// whose run ended since it last looked (RunEnded), and forgets those no
// longer tracked.
func (s *Store) syncChanges() {
	if s.client == nil {
		return
	}
	tracked := s.Data.ChangesTracked()
	for id := range tracked {
		now := s.Data.StateOf(id)
		was, seen := s.changesState[id]
		s.changesState[id] = now
		_, known := s.Data.Changes(id)
		switch {
		case !known:
			s.LoadChanges(id)
		case seen && RunEnded(was, now):
			s.RefreshChanges(id)
		}
	}
	for id := range s.changesState {
		if !tracked[id] {
			delete(s.changesState, id)
		}
	}
	s.Data.ForgetChanges(tracked)
	// The Changes view shows the open session's files alone.
	if d, ok := s.Data.Diff(); ok && d.Session != s.Data.OpenID() {
		s.Data.CloseDiff()
	}
}

// retryChanges fetches again, as the stream comes back, each tracked
// session's changes whose read failed or was in flight, and the diff shown
// when its read did: one in flight may have missed a run's end while the
// stream was down.
func (s *Store) retryChanges() {
	for id := range s.Data.ChangesTracked() {
		if c, ok := s.Data.Changes(id); !ok || c.Status != ListLoaded || s.changesLoading[id] {
			s.LoadChanges(id)
		}
	}
	if d, ok := s.Data.Diff(); ok && (d.Status != ListLoaded || s.fetching[changesKey(d.Session)+":file"] > 0) {
		s.ShowDiff(d.Session, d.Path)
	}
}

// reloadChanges fetches every tracked session's changes again, as after a
// resync, which may have missed a run's end.
func (s *Store) reloadChanges() {
	for id := range s.Data.ChangesTracked() {
		s.RefreshChanges(id)
	}
}
