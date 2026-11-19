package state

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func thread(id int64, state proto.ThreadState, updated time.Duration) proto.Thread {
	return proto.Thread{ID: id, Title: "Fix CI", Agent: "vera", Supervisor: 1, Session: 10 + id, Task: "Fix the flaky job.",
		State: state, CreatedAt: pocketdtest.Now.Add(-time.Hour), UpdatedAt: pocketdtest.Now.Add(updated)}
}

func pull(id, threadID int64, number int64, state proto.PullState) proto.PullRequest {
	return proto.PullRequest{ID: id, Thread: threadID, Repository: "frostyard/nsl", Number: number, Title: "Pin the runner",
		URL: "https://github.com/frostyard/nsl/pull/1", State: state, Head: "vera/fix-ci", Base: "main"}
}

func threadIDs(list []proto.Thread) []int64 {
	var out []int64
	for _, t := range list {
		out = append(out, t.ID)
	}
	return out
}

func pullIDs(list []proto.PullRequest) []int64 {
	var out []int64
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

// A thread closed while a fetch was on its way stays closed when the
// fetch's stale copy arrives; one delegated during the fetch stays, listed
// or not; and an older fetch answering last changes nothing (the Data
// merge rule).
func TestThreadsFollowTheirEventsOverAStaleList(t *testing.T) {
	var d Data
	d.SetThreads(d.BeginThreads(), []proto.Thread{thread(1, proto.ThreadOpen, -time.Hour)})
	f := d.BeginThreads()
	closed := thread(1, proto.ThreadClosed, 0)
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &closed})
	put := thread(2, proto.ThreadStarting, time.Minute)
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &put})
	d.SetThreads(f, []proto.Thread{thread(1, proto.ThreadOpen, -time.Hour)})
	if got, _ := d.Thread(1); got.State != proto.ThreadClosed {
		t.Errorf("T1 is %s after the stale fetch; its event closed it", got.State)
	}
	if got := threadIDs(d.Threads()); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("threads %v, want the one delegated since too, most recently changed first: [2 1]", got)
	}
	if st, _ := d.ThreadsStatus(); st != ListLoaded {
		t.Errorf("status %v", st)
	}
	older := d.BeginThreads()
	newest := d.BeginThreads()
	d.SetThreads(newest, []proto.Thread{thread(3, proto.ThreadOpen, 0)})
	d.SetThreads(older, []proto.Thread{thread(4, proto.ThreadOpen, 0)})
	if got := threadIDs(d.Threads()); !slices.Equal(got, []int64{3}) {
		t.Errorf("an older fetch answering last left %v, want [3]", got)
	}
	d.SetThreadsError(d.BeginThreads(), "timeout")
	if st, err := d.ThreadsStatus(); st != ListFailed || err != "timeout" || len(d.Threads()) != 1 {
		t.Errorf("a failed fetch: %v %q, threads %v", st, err, threadIDs(d.Threads()))
	}
}

// A pull request that merged while a fetch was on its way stays merged,
// and the markers go once no fetch is in flight.
func TestPullsFollowTheirEventsOverAStaleList(t *testing.T) {
	var d Data
	d.SetPulls(d.BeginPulls(), []proto.PullRequest{pull(1, 1, 31, proto.PullOpen)})
	f := d.BeginPulls()
	merged := pull(1, 1, 31, proto.PullMerged)
	d.Apply(proto.Event{Type: proto.EventPull, Pull: &merged})
	d.SetPulls(f, []proto.PullRequest{pull(1, 1, 31, proto.PullOpen), pull(2, 1, 32, proto.PullOpen)})
	if got := d.PullsOf(1); len(got) != 2 || got[0].State != proto.PullMerged || got[1].ID != 2 {
		t.Errorf("pull requests %+v, want #31 merged then #32", got)
	}
	if d.pulls.touched != nil {
		t.Errorf("markers %v outlive every fetch", d.pulls.touched)
	}
	// With no fetch in flight, an event keeps no marker: every later fetch
	// begins after it.
	open := pull(3, 2, 7, proto.PullOpen)
	d.Apply(proto.Event{Type: proto.EventPull, Pull: &open})
	if d.pulls.touched != nil {
		t.Errorf("an event with no fetch in flight left markers %v", d.pulls.touched)
	}
	if got := pullIDs(d.Pulls()); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("pull requests %v, want oldest first: [1 2 3]", got)
	}
}

// Who moves a thread next: the owner for an open ask or a pull request to
// review, else the specialist while it starts or works, the supervisor
// once it is done or blocked, and nobody once it is closed.
func TestTheBoardGroupsThreadsByWhoMovesNext(t *testing.T) {
	var d Data
	d.SetThreads(d.BeginThreads(), []proto.Thread{
		thread(1, proto.ThreadOpen, -6*time.Minute),     // an open ask
		thread(2, proto.ThreadDone, -5*time.Minute),     // a pull request to review
		thread(3, proto.ThreadOpen, -4*time.Minute),     // a draft only
		thread(4, proto.ThreadStarting, -3*time.Minute), // starting
		thread(5, proto.ThreadBlocked, -2*time.Minute),  // blocked, no ask
		thread(6, proto.ThreadDone, -time.Minute),       // done, its pull request merged
		thread(7, proto.ThreadClosed, 0),                // closed, with an open ask
		thread(8, "paused", -7*time.Minute),             // a state this client does not know
	})
	d.SetAsks(d.BeginAsks(), []proto.Ask{{ID: 1, Thread: 1, SessionID: 11, Question: "Squash?", State: proto.AskOpen},
		{ID: 2, Thread: 7, SessionID: 17, Question: "Keep it?", State: proto.AskOpen}})
	draft := pull(2, 3, 33, proto.PullOpen)
	draft.Draft = true
	d.SetPulls(d.BeginPulls(), []proto.PullRequest{pull(1, 2, 32, proto.PullOpen), draft, pull(3, 6, 36, proto.PullMerged)})
	want := map[Mover][]int64{
		MoverOwner:      {2, 1},
		MoverSpecialist: {4, 3},
		MoverSupervisor: {6, 5, 8},
		MoverNobody:     {7},
	}
	board := d.Board()
	if len(board) != len(Movers) {
		t.Fatalf("board has %d groups", len(board))
	}
	for i, g := range board {
		if g.Mover != Movers[i] {
			t.Errorf("group %d is %v, want %v", i, g.Mover, Movers[i])
		}
		if got := threadIDs(g.Threads); !slices.Equal(got, want[g.Mover]) {
			t.Errorf("mover %v moves %v next, want %v", g.Mover, got, want[g.Mover])
		}
	}
	if n := d.OnTheDesk(); n != 7 {
		t.Errorf("%d threads on the desk, want 7", n)
	}
	// The ask answered, T1 goes back to its specialist.
	answered := proto.Ask{ID: 1, Thread: 1, SessionID: 11, Question: "Squash?", State: proto.AskAnswered}
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &answered})
	if m := d.NextMover(thread(1, proto.ThreadOpen, 0)); m != MoverSpecialist {
		t.Errorf("T1 with its ask answered goes to %v", m)
	}
	// Its pull request merged, T2 goes to its supervisor.
	merged := pull(1, 2, 32, proto.PullMerged)
	d.Apply(proto.Event{Type: proto.EventPull, Pull: &merged})
	if m := d.NextMover(thread(2, proto.ThreadDone, 0)); m != MoverSupervisor {
		t.Errorf("T2 with its pull request merged goes to %v", m)
	}
}

// The pull requests that wait for review are the open ones that are no
// draft, a closed thread's included, oldest first; the Inbox counts them.
func TestPullsToReviewAreOpenAndNoDraft(t *testing.T) {
	var d Data
	draft := pull(3, 1, 33, proto.PullOpen)
	draft.Draft = true
	d.SetThreads(d.BeginThreads(), []proto.Thread{thread(1, proto.ThreadOpen, 0), thread(2, proto.ThreadClosed, 0)})
	d.SetPulls(d.BeginPulls(), []proto.PullRequest{pull(4, 2, 34, proto.PullOpen), pull(1, 1, 31, proto.PullOpen),
		pull(2, 1, 32, proto.PullMerged), draft, pull(5, 1, 35, proto.PullClosed)})
	if got := pullIDs(d.PullsToReview()); !slices.Equal(got, []int64{1, 4}) {
		t.Errorf("to review %v, want [1 4]", got)
	}
	var kind InboxKind
	for _, k := range d.Inbox() {
		if k.List == "pull requests" {
			kind = k
		}
	}
	if kind.Count != 2 || kind.Status != ListLoaded {
		t.Errorf("the Inbox counts %+v", kind)
	}
	d.SetAsks(d.BeginAsks(), nil)
	d.SetLessons(d.BeginLessons(), nil)
	d.SetApprovals(d.Begin(), nil)
	if note := InboxNote(d.Inbox()); note != "2 pull requests wait for you" {
		t.Errorf("the Inbox says %q", note)
	}
	d.SetPullsError(d.BeginPulls(), "timeout")
	if note := InboxNote(d.Inbox()); note != "The pull requests are not loaded" {
		t.Errorf("a failed pull requests fetch: the Inbox says %q", note)
	}
}

// A pull request opens on GitHub, from its repository and number, or not
// at all: never the URL pocketd sent.
func TestPullURLIsBuiltFromItsRepositoryAndNumber(t *testing.T) {
	p := pull(1, 1, 31, proto.PullOpen)
	p.URL = "https://evil.example/frostyard/nsl/pull/31"
	if got, ok := PullURL(p); !ok || got != "https://github.com/frostyard/nsl/pull/31" {
		t.Errorf("PullURL = %q, %v", got, ok)
	}
	p.Repository = "Frost-yard/my_repo.go"
	if got, ok := PullURL(p); !ok || got != "https://github.com/Frost-yard/my_repo.go/pull/31" {
		t.Errorf("PullURL = %q, %v", got, ok)
	}
	for _, repo := range []string{"", "frostyard", "frostyard/nsl/x", "frostyard/..", "frostyard/.", "../nsl", "-x/nsl", "frost yard/nsl",
		"frostyard/ns l", "javascript:alert(1)/x", "frostyard/nsl?x=1", "frostyard/nsl#2", "evil.example/nsl"} {
		p.Repository = repo
		if got, ok := PullURL(p); ok {
			t.Errorf("repository %q opens %q", repo, got)
		}
	}
	p.Repository = "frostyard/nsl"
	for _, n := range []int64{0, -1} {
		p.Number = n
		if got, ok := PullURL(p); ok {
			t.Errorf("number %d opens %q", n, got)
		}
	}
}

// The store loads the threads and pull requests with the other lists and
// follows their events.
func TestTheStoreLoadsThreadsAndPullsAndFollowsTheirEvents(t *testing.T) {
	h := newHarness(t)
	h.srv.AddThread(thread(1, proto.ThreadOpen, 0))
	h.srv.AddPull(pull(1, 1, 31, proto.PullOpen))
	h.connect(t)
	if st, _ := h.store.Data.ThreadsStatus(); st != ListLoaded || len(h.store.Data.Threads()) != 1 {
		t.Fatalf("threads %v %v", st, h.store.Data.Threads())
	}
	if st, _ := h.store.Data.PullsStatus(); st != ListLoaded || len(h.store.Data.PullsToReview()) != 1 {
		t.Fatalf("pull requests %v %v", st, h.store.Data.Pulls())
	}
	merged := pull(1, 1, 31, proto.PullMerged)
	h.srv.Emit(proto.Event{Type: proto.EventPull, Pull: &merged})
	done := thread(1, proto.ThreadClosed, time.Minute)
	h.srv.Emit(proto.Event{Type: proto.EventThread, Thread: &done})
	h.loop.RunUntil(t, func() bool { th, _ := h.store.Data.Thread(1); return th.State == proto.ThreadClosed })
	if got := h.store.Data.PullsToReview(); len(got) != 0 {
		t.Errorf("a merged pull request still waits for review: %+v", got)
	}
	// pocketd's lists hold what its events said: a refresh brings no open
	// copy back.
	h.store.Refresh()
	h.settle(t)
	if th, _ := h.store.Data.Thread(1); th.State != proto.ThreadClosed {
		t.Errorf("after a refresh T1 is %s", th.State)
	}
	if got := h.store.Data.PullsToReview(); len(got) != 0 {
		t.Errorf("after a refresh a merged pull request waits for review again: %+v", got)
	}
}

// The board's note counts the threads on the desk once they loaded, and
// how many wait for the owner only once the asks and the pull requests
// loaded too.
func TestTheBoardNoteClaimsCountsOnlyFromLoadedLists(t *testing.T) {
	var d Data
	if got := d.BoardNote(); got != "The threads are not loaded" {
		t.Errorf("before a fetch: %q", got)
	}
	d.SetThreads(d.BeginThreads(), []proto.Thread{thread(1, proto.ThreadOpen, 0), thread(2, proto.ThreadDone, 0)})
	d.SetAsks(d.BeginAsks(), []proto.Ask{{ID: 1, Thread: 1, State: proto.AskOpen}})
	// The pull requests are still loading: T2 may wait for a review.
	if got := d.BoardNote(); got != "2 threads on the desk; the questions or pull requests are not loaded" {
		t.Errorf("with the pull requests loading: %q", got)
	}
	d.SetPullsError(d.BeginPulls(), "timeout")
	if got := d.BoardNote(); got != "2 threads on the desk; the questions or pull requests are not loaded" {
		t.Errorf("with the pull requests failed: %q", got)
	}
	d.SetPulls(d.BeginPulls(), []proto.PullRequest{pull(1, 2, 32, proto.PullOpen)})
	if got := d.BoardNote(); got != "2 threads on the desk, 2 wait for you" {
		t.Errorf("with every list loaded: %q", got)
	}
	d.SetThreads(d.BeginThreads(), nil)
	if got := d.BoardNote(); got != "No thread is on the desk" {
		t.Errorf("with no thread: %q", got)
	}
}

// A report folds when it is long or has many lines; a short one always
// shows whole, however it wraps.
func TestAReportFoldsOnlyWhenItIsLong(t *testing.T) {
	short := "The job times out on the 2-year-old runner image, so I pinned it and raised the timeout."
	if ReportFolds(short) {
		t.Errorf("a short report folds: %q", short)
	}
	for _, report := range []string{
		strings.Repeat("é", 281),
		"one\ntwo\nthree\nfour\nfive",
	} {
		if !ReportFolds(report) {
			t.Errorf("%q does not fold", report)
		}
	}
	if ReportFolds(strings.Repeat("é", 280)) || ReportFolds("one\ntwo\nthree\nfour") {
		t.Error("a report at the limits folds")
	}
}

// A profile fetch that failed is fetched again when the stream comes back,
// as the lists are: the board and the Inbox name agents by them.
func TestAReconnectRetriesTheProfiles(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera"})
	h.srv.FailOnce(http.MethodGet, proto.PathProfiles, http.StatusBadGateway)
	h.connect(t)
	if status, _ := h.store.Data.ProfilesStatus(); status != ListFailed {
		t.Fatalf("profiles %v after a failed fetch", status)
	}
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool { status, _ := h.store.Data.ProfilesStatus(); return status == ListLoaded })
	if p := h.store.Data.Profiles(); len(p) != 1 || p[0].Name != "Vera" {
		t.Errorf("profiles %+v", p)
	}
}

// A close that failed keeps its request id for the retry, and the retry's
// reply closes the thread.
func TestACloseTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddThread(thread(1, proto.ThreadBlocked, 0))
	h.connect(t)
	p := proto.Path(proto.PathCloseThread, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	h.store.CloseThread(1)
	h.loop.RunUntil(t, func() bool { return h.store.Closing(1).Err != "" })
	h.store.CloseThread(1)
	h.loop.RunUntil(t, func() bool { th, _ := h.store.Data.Thread(1); return th.State == proto.ThreadClosed })
	h.settle(t)
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] != got[1] {
		t.Errorf("request ids %q, want the retry to keep the first id", got)
	}
	if c := h.store.Closing(1); c.Busy || c.Err != "" {
		t.Errorf("the close's state outlived it: %+v", c)
	}
}

// A close whose reply was lost leaves nothing once the thread is closed:
// its event drops the retry state, and a later close is a new request.
func TestALostCloseReplyLeavesNoRetryState(t *testing.T) {
	h := newHarness(t)
	h.srv.AddThread(thread(1, proto.ThreadOpen, 0))
	h.connect(t)
	p := proto.Path(proto.PathCloseThread, 1)
	h.srv.LoseAnswerOnce(http.MethodPost, p)
	h.store.CloseThread(1)
	h.loop.RunUntil(t, func() bool {
		th, _ := h.store.Data.Thread(1)
		return th.State == proto.ThreadClosed && !h.store.Closing(1).Busy
	})
	h.settle(t)
	if _, kept := h.store.closes[1]; kept {
		t.Error("the close's retry state outlived its thread's close")
	}
	if id := h.store.ids.For(closeKey(1), "close"); id == sentIDs(t, h, p)[0] {
		t.Error("the close's request id outlived its thread's close")
	}
}

// A closed thread's pull request shows only while open, as GET /v1/pulls
// lists them, whichever event comes first: the merge or the close.
func TestAClosedThreadsMergedPullLeavesInEitherOrder(t *testing.T) {
	for _, mergeFirst := range []bool{true, false} {
		var d Data
		d.SetThreads(d.BeginThreads(), []proto.Thread{thread(1, proto.ThreadOpen, 0), thread(2, proto.ThreadOpen, 0)})
		d.SetPulls(d.BeginPulls(), []proto.PullRequest{pull(1, 1, 31, proto.PullOpen), pull(2, 2, 32, proto.PullOpen)})
		merged := pull(1, 1, 31, proto.PullMerged)
		closed := thread(1, proto.ThreadClosed, time.Minute)
		events := []proto.Event{{Type: proto.EventPull, Pull: &merged}, {Type: proto.EventThread, Thread: &closed}}
		if !mergeFirst {
			events[0], events[1] = events[1], events[0]
		}
		for _, ev := range events {
			d.Apply(ev)
		}
		if got := d.PullsOf(1); len(got) != 0 {
			t.Errorf("merge first %v: the closed T1 shows %+v", mergeFirst, got)
		}
		// An open pull request of a closed thread still shows.
		closed2 := thread(2, proto.ThreadClosed, time.Minute)
		d.Apply(proto.Event{Type: proto.EventThread, Thread: &closed2})
		if got := pullIDs(d.Pulls()); !slices.Equal(got, []int64{2}) {
			t.Errorf("merge first %v: pull requests %v, want the open one of the closed T2: [2]", mergeFirst, got)
		}
	}
}

// A label is backed only by a ready verdict at the head with green CI,
// both as pocketd last read them.
func TestReadyBacked(t *testing.T) {
	head := strings.Repeat("c", 40)
	ready := &proto.PullReview{Verdict: proto.ReviewReady, Head: head}
	if !ReadyBacked(proto.PullRequest{HeadSHA: head, Review: ready, Checks: proto.ChecksGreen}) {
		t.Error("a ready verdict at the head with green CI backs nothing")
	}
	for name, p := range map[string]proto.PullRequest{
		"no review":          {HeadSHA: head, Checks: proto.ChecksGreen},
		"red CI":             {HeadSHA: head, Review: ready, Checks: proto.ChecksFailing},
		"CI not read":        {HeadSHA: head, Review: ready},
		"a review elsewhere": {HeadSHA: strings.Repeat("d", 40), Review: ready, Checks: proto.ChecksGreen},
		"not ready":          {HeadSHA: head, Review: &proto.PullReview{Verdict: proto.ReviewNotReady, Head: head}, Checks: proto.ChecksGreen},
	} {
		if ReadyBacked(p) {
			t.Errorf("%s backs the label", name)
		}
	}
}
