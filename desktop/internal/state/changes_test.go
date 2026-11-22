package state

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// changesOf is a session's changes in a checkout: files changed of all
// kinds, the first committed and changed since.
func changesOf(id int64) proto.Changes {
	return proto.Changes{SessionID: id, Repository: true, Top: "/home/team/projects/demo--vera-fix-ci-1", Branch: "vera/fix-ci-1",
		Head: "9f3c2a1b7e6d5c4b3a291807f6e5d4c3b2a19087", Base: "origin/main", BaseRule: proto.ChangesBaseDefault,
		MergeBase: "4b1e0c9d8a7f6e5d4c3b2a190807f6e5d4c3b2a1", Added: 120, Removed: 8,
		Files: []proto.ChangedFile{
			{Path: "ci/arm64.yml", Added: 14, Removed: 6, Committed: true, Uncommitted: true},
			{Path: "internal/run/retry.go", Added: 88, Removed: 2, Committed: true},
			{Path: "internal/run/retry_test.go", Added: 18, New: true, Uncommitted: true},
			{Path: "docs/logo.png", Binary: true, New: true, Uncommitted: true},
		}}
}

// The count a header and a card show, and each file's row, are read from
// the changes by these rules (ADR-0053).
func TestChangesLabels(t *testing.T) {
	c := SessionChanges{Status: ListLoaded, Changes: changesOf(1)}
	if got := ChangesSummary(c, true); got != "4 files +120 −8" {
		t.Errorf("summary %q", got)
	}
	one := c
	one.Changes.Files, one.Changes.More = one.Changes.Files[:1], 0
	if got := ChangesSummary(one, true); got != "1 file +120 −8" {
		t.Errorf("one file %q", got)
	}
	more := c
	more.Changes.More = 3
	if got := ChangesSummary(more, true); got != "7 files +120 −8" {
		t.Errorf("files left out count: %q", got)
	}
	if got := MoreFiles(more.Changes); got != fmt.Sprintf("3 files more not listed: pocketd lists the first %d.", proto.MaxChangedFiles) {
		t.Errorf("more %q", got)
	}
	none := SessionChanges{Status: ListLoaded, Changes: proto.Changes{SessionID: 1, Repository: true, Files: []proto.ChangedFile{}}}
	if ChangesSummary(none, true) != "No changes" || CardChanges(none, true) != "" || CardChanges(c, true) != "4 files +120 −8" {
		t.Errorf("no changes: %q, card %q", ChangesSummary(none, true), CardChanges(none, true))
	}
	for _, hidden := range []SessionChanges{{Status: ListLoaded, Changes: proto.Changes{SessionID: 1}}, {Status: ListLoading}, {}} {
		if got := ChangesSummary(hidden, hidden.Status != 0); got != "" {
			t.Errorf("%+v shows %q", hidden, got)
		}
	}
	files := c.Changes.Files
	for i, want := range []struct{ counts, state string }{
		{"+14 −6", "committed, and changed since"}, {"+88 −2", "committed"}, {"+18 −0", "new, not committed"}, {"binary", "new, not committed"},
	} {
		if FileCounts(files[i]) != want.counts || FileState(files[i]) != want.state {
			t.Errorf("file %d: %q, %q", i, FileCounts(files[i]), FileState(files[i]))
		}
	}
	if got := FileState(proto.ChangedFile{Path: "a", Uncommitted: true}); got != "not committed" {
		t.Errorf("uncommitted %q", got)
	}
	if got := FileCounts(proto.ChangedFile{Path: "a", New: true, Uncounted: true}); got != "not counted" {
		t.Errorf("uncounted %q", got)
	}
	// A repository inside the checkout is listed as its folder, which has
	// no diff pocketd shows.
	inner := proto.ChangedFile{Path: "vendor/inner/", New: true, Uncommitted: true, Uncounted: true}
	if CanDiff(inner) || !CanDiff(files[0]) || FileState(inner) != "a repository inside the checkout, not tracked" {
		t.Errorf("a repository inside: %v, %q", CanDiff(inner), FileState(inner))
	}
	// An entry pocketd shows no diff of, as a path that is no UTF-8, whose
	// spelling with U+FFFD names no file, is never asked for (Codex review
	// of #109, round 2).
	latin := proto.ChangedFile{Path: "caf�.txt", New: true, Uncommitted: true, Uncounted: true, NoDiff: true}
	if CanDiff(latin) || FileState(latin) != "a name pocketd cannot show, no diff" {
		t.Errorf("a name that is no UTF-8: %v, %q", CanDiff(latin), FileState(latin))
	}
	if got := FileName(proto.ChangedFile{Path: "b.go", OldPath: "a.go"}); got != "a.go → b.go" {
		t.Errorf("rename %q", got)
	}
	if got := BaseNote(c.Changes); got != "vera/fix-ci-1 against origin/main since 4b1e0c9d" {
		t.Errorf("base %q", got)
	}
	pull := c.Changes
	pull.BaseRule, pull.Base = proto.ChangesBasePull, "origin/develop"
	if got := BaseNote(pull); got != "vera/fix-ci-1 against origin/develop, the pull request's base, since 4b1e0c9d" {
		t.Errorf("pull base %q", got)
	}
	head := proto.Changes{Repository: true, BaseRule: proto.ChangesBaseHead, Head: "abc"}
	if got := BaseNote(head); got != "a detached HEAD, no base branch found: only what is not committed" {
		t.Errorf("no base %q", got)
	}
	if got := BaseNote(proto.Changes{Repository: true, Branch: "main", BaseRule: proto.ChangesBaseHead}); got != "main, no commit yet: every file counts as new" {
		t.Errorf("no commit %q", got)
	}
}

// A cut diff says so, with the bound it was cut at; a whole one says
// nothing (ADR-0053).
func TestACutDiffSaysSo(t *testing.T) {
	if got := CutNote(proto.FileDiff{Diff: "+a\n", Cut: true}); got != "Only the first 64 KB of this diff show: the rest is in the checkout." {
		t.Errorf("cut %q", got)
	}
	if got := CutNote(proto.FileDiff{Diff: "+a\n"}); got != "" {
		t.Errorf("whole %q", got)
	}
	if NoDiff(proto.FileDiff{Diff: "\n"}) == "" || NoDiff(proto.FileDiff{Diff: "+a\n"}) != "" {
		t.Error("an empty diff is not told apart")
	}
}

// An answer of a fetch begun before one already applied never undoes it:
// a run's end fetches again, and the older answer would put back what the
// run changed since (AGENTS.md: a fetch's answer goes through Begin).
func TestAnOlderChangesAnswerNeverUndoesANewerOne(t *testing.T) {
	var d Data
	older, newer := d.BeginChanges(1), d.BeginChanges(1)
	fresh := changesOf(1)
	d.SetChanges(newer, 1, fresh)
	stale := changesOf(1)
	stale.Files = stale.Files[:1]
	d.SetChanges(older, 1, stale)
	d.SetChangesError(older, 1, "late failure")
	got, ok := d.Changes(1)
	if !ok || got.Status != ListLoaded || len(got.Changes.Files) != 4 || got.Err != "" {
		t.Fatalf("changes %+v", got)
	}
	failing := d.BeginChanges(1)
	d.SetChangesError(failing, 1, "pocketd is away")
	if got, _ := d.Changes(1); got.Status != ListFailed || got.Err != "pocketd is away" || len(got.Changes.Files) != 4 {
		t.Fatalf("a failure lost the changes: %+v", got)
	}
	if _, ok := d.Changes(2); ok {
		t.Fatal("a session never fetched has changes")
	}
	d.SetChanges(Fetch{epoch: 99}, 2, fresh)
	if _, ok := d.Changes(2); ok {
		t.Fatal("an answer no fetch began was taken")
	}
	d.ForgetChanges(map[int64]bool{})
	if _, ok := d.Changes(1); ok {
		t.Fatal("an untracked session kept its changes")
	}
}

// The diff shown is the file asked for last: an answer for another file,
// or an older answer, is dropped.
func TestADiffAnswerForAnotherFileIsDropped(t *testing.T) {
	var d Data
	first := d.BeginDiff(1, "a.go")
	second := d.BeginDiff(1, "b.go")
	d.SetDiff(first, 1, "a.go", proto.FileDiff{SessionID: 1, Path: "a.go", Diff: "+a\n"})
	if got, ok := d.Diff(); !ok || got.Path != "b.go" || got.Status != ListLoading || got.Diff.Diff != "" {
		t.Fatalf("another file's diff shows: %+v", got)
	}
	again := d.BeginDiff(1, "b.go")
	d.SetDiff(again, 1, "b.go", proto.FileDiff{SessionID: 1, Path: "b.go", Diff: "+b2\n"})
	d.SetDiff(second, 1, "b.go", proto.FileDiff{SessionID: 1, Path: "b.go", Diff: "+b1\n"})
	if got, _ := d.Diff(); got.Diff.Diff != "+b2\n" || got.Status != ListLoaded {
		t.Fatalf("an older answer won: %+v", got)
	}
	d.CloseDiff()
	if _, ok := d.Diff(); ok {
		t.Fatal("a closed diff shows")
	}
}

// The client keeps the changes of the open session and of the sessions of
// the board's threads that work on a repository and are not closed.
func TestChangesAreTrackedForTheOpenSessionAndTheBoardsThreads(t *testing.T) {
	var d Data
	d.SetThreads(d.BeginThreads(), []proto.Thread{
		{ID: 1, Session: 11, Repository: "frostyard/nsl", State: proto.ThreadOpen},
		{ID: 2, Session: 12, Repository: "frostyard/nsl", State: proto.ThreadClosed},
		{ID: 3, Session: 13, State: proto.ThreadOpen},
		{ID: 4, Repository: "frostyard/nsl", State: proto.ThreadStarting},
	})
	d.SetOpen(7)
	got := d.ChangesTracked()
	if len(got) != 2 || !got[7] || !got[11] {
		t.Fatalf("tracked %v", got)
	}
}

// A run's end is a session that worked or waited and is idle now.
func TestARunEndsWhenItsSessionGoesIdle(t *testing.T) {
	for _, tc := range []struct {
		was, now proto.SessionState
		want     bool
	}{
		{proto.SessionRunning, proto.SessionIdle, true}, {proto.SessionWaiting, proto.SessionIdle, true},
		{proto.SessionIdle, proto.SessionIdle, false}, {proto.SessionRunning, proto.SessionWaiting, false}, {"", proto.SessionIdle, false},
	} {
		if RunEnded(tc.was, tc.now) != tc.want {
			t.Errorf("%q to %q: %v", tc.was, tc.now, !tc.want)
		}
	}
}

// The open session's changes load as it opens, and again, with the diff
// shown, when its run ends (ADR-0053).
func TestARunsEndFetchesTheOpenSessionsChangesAgain(t *testing.T) {
	h := newHarness(t)
	running := session(1, "Fix CI", 0)
	running.State = proto.SessionRunning
	h.srv.AddSession(running)
	h.srv.SetLive(proto.Live{SessionID: 1, Busy: true})
	h.srv.SetChanges(changesOf(1))
	h.srv.SetFileDiff(proto.FileDiff{SessionID: 1, Path: "ci/arm64.yml", Diff: "@@ -1 +1 @@\n-a\n+b\n"})
	h.connect(t)
	h.store.Open(1)
	h.settle(t)
	path := "/v1/sessions/1/changes"
	if n := len(h.srv.RequestsTo("GET", path)); n != 1 {
		t.Fatalf("%d fetches of the open session's changes", n)
	}
	if got, _ := h.store.Data.Changes(1); ChangesSummary(got, true) != "4 files +120 −8" {
		t.Fatalf("changes %+v", got)
	}
	h.store.ShowDiff(1, "ci/arm64.yml")
	h.settle(t)
	if d, _ := h.store.Data.Diff(); d.Diff.Diff != "@@ -1 +1 @@\n-a\n+b\n" {
		t.Fatalf("diff %+v", d)
	}
	// An event while it runs fetches nothing; its end fetches both again.
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryAssistant, Text: "working"}})
	h.settle(t)
	if n := len(h.srv.RequestsTo("GET", path)); n != 1 {
		t.Fatalf("%d fetches while the run works", n)
	}
	changed := changesOf(1)
	changed.Files, changed.Added, changed.Removed = changed.Files[:2], 102, 8
	h.srv.SetChanges(changed)
	idle := running
	idle.State = proto.SessionIdle
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1}})
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &idle})
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo("GET", path)) == 2 })
	h.settle(t)
	if got, _ := h.store.Data.Changes(1); ChangesSummary(got, true) != "2 files +102 −8" {
		t.Fatalf("changes after the run %+v", got)
	}
	if n := len(h.srv.RequestsTo("GET", "/v1/sessions/1/changes/file")); n != 2 {
		t.Fatalf("%d fetches of the diff shown", n)
	}
	// Another session opened shows no diff of this one.
	h.srv.AddSession(session(2, "Other", 0))
	h.store.Open(2)
	h.settle(t)
	if _, ok := h.store.Data.Diff(); ok {
		t.Fatal("the diff of a session no longer open shows")
	}
}

// The board's threads get their sessions' changes, for their cards; a
// closed thread, or one in its specialist's home, gets none.
func TestTheBoardsThreadsGetTheirSessionsChanges(t *testing.T) {
	h := newHarness(t)
	for id := int64(11); id <= 13; id++ {
		h.srv.AddSession(session(id, "thread", 0))
	}
	h.srv.AddThread(proto.Thread{ID: 1, Agent: "vera", Session: 11, Repository: "frostyard/nsl", State: proto.ThreadOpen, Task: "x"})
	h.srv.AddThread(proto.Thread{ID: 2, Agent: "vera", Session: 12, Repository: "frostyard/nsl", State: proto.ThreadClosed, Task: "x"})
	h.srv.AddThread(proto.Thread{ID: 3, Agent: "duncan", Session: 13, State: proto.ThreadOpen, Task: "x"})
	h.srv.SetChanges(changesOf(11))
	h.connect(t)
	h.settle(t)
	if n := len(h.srv.RequestsTo("GET", "/v1/sessions/11/changes")); n != 1 {
		t.Fatalf("%d fetches of the thread's changes", n)
	}
	for _, id := range []int64{12, 13} {
		if n := len(h.srv.RequestsTo("GET", fmt.Sprintf("/v1/sessions/%d/changes", id))); n != 0 {
			t.Errorf("session %d's changes were fetched %d times", id, n)
		}
	}
	if got, ok := h.store.Data.Changes(11); CardChanges(got, ok) != "4 files +120 −8" {
		t.Fatalf("card %+v", got)
	}
}

// A fetch asked for while one is in flight follows it, once, so a run that
// ended under the first one is counted.
func TestAChangesFetchAskedForInFlightFollowsIt(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "Fix CI", 0))
	h.connect(t)
	h.store.Open(1)
	h.settle(t)
	path := "/v1/sessions/1/changes"
	if n := len(h.srv.RequestsTo("GET", path)); n != 1 {
		t.Fatalf("%d fetches as it opened", n)
	}
	release := h.srv.HoldAnswerOnce("GET", path)
	h.store.LoadChanges(1)
	h.loop.RunUntil(t, func() bool { return h.srv.Holding("GET", path) })
	h.store.LoadChanges(1)
	h.store.LoadChanges(1)
	if n := len(h.srv.RequestsTo("GET", path)); n != 2 {
		t.Fatalf("%d fetches in flight", n)
	}
	h.srv.SetChanges(changesOf(1))
	release()
	h.settle(t)
	if n := len(h.srv.RequestsTo("GET", path)); n != 3 {
		t.Fatalf("%d fetches", n)
	}
	if got, ok := h.store.Data.Changes(1); ChangesSummary(got, ok) != "4 files +120 −8" {
		t.Fatalf("changes %+v", got)
	}
}

// A session the client stopped tracking while its fetch was in flight gets
// no fetch after it: it is fetched afresh when it shows again.
func TestAFetchAfterOneInFlightGoesOnlyWhileTracked(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "Fix CI", 0))
	h.srv.AddSession(session(2, "Other", 0))
	h.connect(t)
	h.store.Open(1)
	h.settle(t)
	path := "/v1/sessions/1/changes"
	release := h.srv.HoldAnswerOnce("GET", path)
	h.store.LoadChanges(1)
	h.loop.RunUntil(t, func() bool { return h.srv.Holding("GET", path) })
	h.store.LoadChanges(1)
	h.store.Open(2)
	release()
	h.settle(t)
	if n := len(h.srv.RequestsTo("GET", path)); n != 2 {
		t.Fatalf("%d fetches of a session no longer tracked", n)
	}
	if _, ok := h.store.Data.Changes(1); ok {
		t.Fatal("a session no longer tracked keeps its changes")
	}
}

// The diff shown of a file the session's changes no longer name goes, as
// pocketd shows no diff of it; while the list is cut, the file may be among
// those left out, and it stays.
func TestADiffWhoseFileLeftTheChangesGoes(t *testing.T) {
	var d Data
	f := d.BeginChanges(1)
	d.SetChanges(f, 1, changesOf(1))
	d.BeginDiff(1, "internal/run/retry.go")
	cut := changesOf(1)
	cut.Files, cut.More = cut.Files[:1], 3
	d.SetChanges(d.BeginChanges(1), 1, cut)
	if _, ok := d.Diff(); !ok {
		t.Fatal("a cut list closed the diff of a file it may leave out")
	}
	other := changesOf(2)
	d.SetChanges(d.BeginChanges(2), 2, other)
	if _, ok := d.Diff(); !ok {
		t.Fatal("another session's list closed the diff")
	}
	gone := changesOf(1)
	gone.Files = gone.Files[:1]
	d.SetChanges(d.BeginChanges(1), 1, gone)
	if got, ok := d.Diff(); ok {
		t.Fatalf("the diff of a file no longer changed shows: %+v", got)
	}
}

// A read that failed shows, as not read, whatever an earlier one said: the
// header offers the Changes, which say why, never a count or "No changes"
// that may be stale; a card says nothing (Codex review of #109, round 1).
func TestAFailedChangesReadShowsAsNotRead(t *testing.T) {
	var d Data
	first := d.BeginChanges(1)
	d.SetChangesError(first, 1, "git cannot read this checkout isolated")
	c, ok := d.Changes(1)
	if got := ChangesSummary(c, ok); got != NotRead || CardChanges(c, ok) != "" {
		t.Fatalf("a first read that failed: %q, card %q", got, CardChanges(c, ok))
	}
	empty := proto.Changes{SessionID: 1, Repository: true, Files: []proto.ChangedFile{}}
	d.SetChanges(d.BeginChanges(1), 1, empty)
	if c, ok := d.Changes(1); ChangesSummary(c, ok) != "No changes" {
		t.Fatalf("no change: %q", ChangesSummary(c, ok))
	}
	d.SetChangesError(d.BeginChanges(1), 1, "pocketd is away")
	if c, ok := d.Changes(1); ChangesSummary(c, ok) != NotRead || c.Err != "pocketd is away" {
		t.Fatalf("a failure after no change: %q, %q", ChangesSummary(c, ok), c.Err)
	}
}

// A changes read that failed, or the diff shown whose read failed, is read
// again as the stream comes back (Codex review of #109, round 1).
func TestAReconnectRetriesAFailedChangesRead(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "Fix CI", 0))
	h.srv.SetChanges(changesOf(1))
	h.srv.SetFileDiff(proto.FileDiff{SessionID: 1, Path: "ci/arm64.yml", Diff: "+a\n"})
	h.connect(t)
	path := "/v1/sessions/1/changes"
	h.srv.FailOnce("GET", path, 500)
	h.srv.FailOnce("GET", path+"/file", 500)
	h.store.Open(1)
	h.store.ShowDiff(1, "ci/arm64.yml")
	h.settle(t)
	if c, ok := h.store.Data.Changes(1); ChangesSummary(c, ok) != NotRead {
		t.Fatalf("a failed read: %+v", c)
	}
	if d, _ := h.store.Data.Diff(); d.Status != ListFailed {
		t.Fatalf("a failed diff: %+v", d)
	}
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool { c, ok := h.store.Data.Changes(1); return ok && c.Status == ListLoaded })
	h.settle(t)
	if c, ok := h.store.Data.Changes(1); ChangesSummary(c, ok) != "4 files +120 −8" {
		t.Fatalf("after the reconnect: %+v", c)
	}
	if d, _ := h.store.Data.Diff(); d.Status != ListLoaded || d.Diff.Diff != "+a\n" {
		t.Fatalf("the diff after the reconnect: %+v", d)
	}
}

// A board of more threads than pocketd reads at once sends at most
// proto.MaxChangesReads fetches of changes: the rest wait their turn here,
// before their deadline starts, and each has ChangesTimeout, room for
// pocketd's read and its wait for a place behind another client's (Codex
// review of #109, round 3).
func TestChangesFetchesWaitTheirTurnBeforeTheirDeadline(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var bounds []time.Duration
	h.store.withTimeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		mu.Lock()
		bounds = append(bounds, d)
		mu.Unlock()
		return context.WithCancel(ctx)
	}
	n := proto.MaxChangesReads + 1
	var releases []func()
	for i := range n {
		id := int64(11 + i)
		h.srv.AddSession(session(id, "thread", 0))
		h.srv.AddThread(proto.Thread{ID: id, Agent: "vera", Session: id, Repository: "frostyard/nsl", State: proto.ThreadOpen, Task: "x"})
		h.srv.SetChanges(changesOf(id))
		releases = append(releases, h.srv.HoldAnswerOnce("GET", fmt.Sprintf("/v1/sessions/%d/changes", id)))
	}
	if err := h.store.Connect(h.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	sent := func() int {
		count := 0
		for i := range n {
			count += len(h.srv.RequestsTo("GET", fmt.Sprintf("/v1/sessions/%d/changes", 11+i)))
		}
		return count
	}
	h.loop.RunUntil(t, func() bool { return sent() == proto.MaxChangesReads })
	for range 3 {
		h.loop.Flush()
	}
	if got := sent(); got != proto.MaxChangesReads {
		t.Fatalf("%d fetches went at once", got)
	}
	for _, release := range releases {
		release()
	}
	h.loop.RunUntil(t, func() bool { return sent() == n })
	h.settle(t)
	for i := range n {
		if c, ok := h.store.Data.Changes(int64(11 + i)); CardChanges(c, ok) != "4 files +120 −8" {
			t.Fatalf("session %d: %+v", 11+i, c)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got := slices.Index(bounds, ChangesTimeout); got < 0 || ChangesTimeout < 60*time.Second {
		t.Fatalf("the fetches asked for %v", bounds)
	}
}

// An event of no tracked session, as an entry of a reply as it streams,
// looks at no board's sessions; a thread's event does (Codex review of
// #109, round 3).
func TestAnEventOfNoSessionLooksAtNoBoard(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(11, "thread", 0))
	h.srv.AddThread(proto.Thread{ID: 1, Agent: "vera", Session: 11, Repository: "frostyard/nsl", State: proto.ThreadOpen, Task: "x"})
	h.connect(t)
	before := h.store.changesScans
	for i := range 20 {
		h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: int64(100 + i), SessionID: 11, Kind: proto.EntryAssistant, Text: "x"}})
	}
	h.srv.Emit(proto.Event{Type: proto.EventLesson, Lesson: &proto.Lesson{ID: 1, Agent: "vera", Text: "x", State: proto.LessonPending}})
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.LiveLesson(1); return ok })
	h.settle(t)
	if got := h.store.changesScans; got != before {
		t.Fatalf("%d looks at the board for events of no thread", got-before)
	}
	h.srv.Emit(proto.Event{Type: proto.EventThread, Thread: &proto.Thread{ID: 1, Agent: "vera", Session: 11, Repository: "frostyard/nsl", State: proto.ThreadDone, Task: "x"}})
	h.loop.RunUntil(t, func() bool { return h.store.changesScans == before+1 })
}
