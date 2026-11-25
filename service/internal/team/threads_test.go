package team

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Only a supervisor reads the threads (ADR-0071).
func TestCheckThreadsCaller(t *testing.T) {
	if err := CheckThreadsCaller(&profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}); err != nil {
		t.Fatalf("a supervisor: %v", err)
	}
	for name, caller := range map[string]*profiles.Profile{
		"an agent":   {ID: "vera", Role: profiles.RoleAgent},
		"a clerk":    {ID: "clerk", Role: profiles.RoleClerk},
		"no profile": nil,
	} {
		if err := CheckThreadsCaller(caller); err == nil || !strings.Contains(err.Error(), "only a supervisor") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A list takes no thread, a read takes one, and nothing else is an
// action (ADR-0071).
func TestCheckThreadsCall(t *testing.T) {
	for _, c := range []ThreadsCall{{Action: ThreadsList}, {Action: ThreadsRead, Thread: 5}} {
		if err := CheckThreadsCall(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for c, want := range map[ThreadsCall]string{
		{Action: ThreadsList, Thread: 5}:  "list takes no thread",
		{Action: ThreadsRead}:             "read needs thread",
		{Action: ThreadsRead, Thread: -1}: "read needs thread",
		{Action: "close", Thread: 5}:      `action "close" is not list or read`,
		{}:                                `action "" is not list or read`,
	} {
		if err := CheckThreadsCall(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", c, err, want)
		}
	}
}

func boardThread(id int64, state proto.ThreadState) proto.Thread {
	return proto.Thread{ID: id, Title: fmt.Sprintf("Thread %d", id), Agent: "vera", Supervisor: 1, State: state}
}

// boardWith is a board of threads, with its asks, pull requests and plans.
func boardWith(threads []proto.Thread, asks []proto.Ask, pulls []proto.PullRequest, plans ...proto.Plan) Board {
	b := Board{Threads: map[int64]proto.Thread{}, Asks: asks, Pulls: pulls, Plans: map[int64]proto.Plan{}}
	for _, t := range threads {
		b.Threads[t.ID] = t
	}
	for _, p := range plans {
		b.Plans[p.ID] = p
	}
	return b
}

// NextMover mirrors the desktop's work board rule for rule (ADR-0071):
// each case here is a case of state's TestTheBoardGroupsThreadsByWhoMovesNext,
// TestAPlanThreadOnTheBoard, TestAnApprovedPlansThreadMovesWithItsTasks or
// TestAShiftsThreadIsMovedByWhoItWaitsFor, with the same answer. A change
// to one rule is made to the other, with its cases.
func TestNextMoverMirrorsTheWorkBoard(t *testing.T) {
	// state's TestTheBoardGroupsThreadsByWhoMovesNext.
	threads := []proto.Thread{
		boardThread(1, proto.ThreadOpen),     // an open ask
		boardThread(2, proto.ThreadDone),     // a pull request to review
		boardThread(3, proto.ThreadOpen),     // a draft only
		boardThread(4, proto.ThreadStarting), // starting
		boardThread(5, proto.ThreadBlocked),  // blocked, no ask
		boardThread(6, proto.ThreadDone),     // done, its pull request merged
		boardThread(7, proto.ThreadClosed),   // closed, with an open ask
		boardThread(8, "paused"),             // a state the rule does not know
	}
	asks := []proto.Ask{{ID: 1, Thread: 1, State: proto.AskOpen}, {ID: 2, Thread: 7, State: proto.AskOpen},
		// An answered ask holds nothing.
		{ID: 3, Thread: 6, State: proto.AskAnswered}}
	pulls := []proto.PullRequest{{ID: 1, Thread: 2, State: proto.PullOpen}, {ID: 2, Thread: 3, State: proto.PullOpen, Draft: true},
		{ID: 3, Thread: 6, State: proto.PullMerged}}
	b := boardWith(threads, asks, pulls)
	want := map[int64]Mover{1: MoverOwner, 2: MoverOwner, 3: MoverSpecialist, 4: MoverSpecialist, 5: MoverSupervisor, 6: MoverSupervisor,
		7: MoverNobody, 8: MoverSupervisor}
	for id, m := range want {
		if got := b.NextMover(b.Threads[id]); got != m {
			t.Errorf("T%d: %v, want %v", id, got, m)
		}
	}

	// state's TestAPlanThreadOnTheBoard and
	// TestAnApprovedPlansThreadMovesWithItsTasks.
	rev := strings.Repeat("a", 64)
	planThread := proto.Thread{ID: 11, Agent: "duncan", State: proto.ThreadOpen, Plan: 1}
	task := proto.Thread{ID: 12, Agent: "vera", State: proto.ThreadOpen, Plan: 1, PlanTask: "t1"}
	graph := func(state proto.TaskState, thread int64, approved bool) *proto.PlanGraph {
		return &proto.PlanGraph{Version: 1, Rev: rev, Approved: approved, Tasks: []proto.PlanTask{{ID: "t1", Title: "Keys", State: state, Thread: thread}}}
	}
	review := proto.Plan{ID: 1, Thread: 11, State: proto.PlanReview, Rev: rev, Graph: graph(proto.TaskWaiting, 0, false)}
	if got := boardWith([]proto.Thread{planThread, task}, nil, nil).NextMover(planThread); got != MoverSpecialist {
		t.Errorf("a plan thread with no plan read: %v", got)
	}
	if got := boardWith([]proto.Thread{planThread, task}, nil, nil, review).NextMover(planThread); got != MoverOwner {
		t.Errorf("a plan thread whose plan waits for the owner: %v", got)
	}
	approved := review
	approved.State, approved.Graph = proto.PlanBuilding, graph(proto.TaskReady, 0, true)
	if got := boardWith([]proto.Thread{planThread}, nil, nil, approved).NextMover(planThread); got != MoverSupervisor {
		t.Errorf("a plan with a ready task: %v", got)
	}
	gated := approved
	gated.Waiting = []string{"A3 open"}
	if got := boardWith([]proto.Thread{planThread}, nil, nil, gated).NextMover(planThread); got != MoverOwner {
		t.Errorf("a plan whose gate waits: %v", got)
	}
	delivered := approved
	delivered.Graph = graph(proto.TaskDelegated, 12, true)
	doneTask := task
	doneTask.State = proto.ThreadDone
	if got := boardWith([]proto.Thread{planThread, doneTask}, nil, nil, delivered).NextMover(planThread); got != MoverSupervisor {
		t.Errorf("a plan whose task reported done: %v", got)
	}
	if got := boardWith([]proto.Thread{planThread, task}, nil, nil, delivered).NextMover(planThread); got != MoverSpecialist {
		t.Errorf("a plan whose tasks work: %v", got)
	}
	if got := boardWith([]proto.Thread{planThread, task}, []proto.Ask{{ID: 9, Thread: 12, State: proto.AskOpen}}, nil, delivered).NextMover(planThread); got != MoverOwner {
		t.Errorf("a plan whose task's thread waits for the owner: %v", got)
	}
	drafting := review
	drafting.State, drafting.Waiting = proto.PlanDrafting, []string{"A4 not written", "K1 running"}
	blocked := planThread
	blocked.State = proto.ThreadBlocked
	for _, th := range []proto.Thread{planThread, blocked} {
		if got := boardWith([]proto.Thread{th}, nil, nil, drafting).NextMover(th); got != MoverSpecialist {
			t.Errorf("a drafting plan with nothing open for the owner, its thread %s: %v", th.State, got)
		}
	}
	drafting.Waiting = []string{"A3 open", "A4 not written"}
	if got := boardWith([]proto.Thread{planThread}, nil, nil, drafting).NextMover(planThread); got != MoverOwner {
		t.Errorf("a drafting plan with an open ask: %v", got)
	}

	// state's TestAShiftsThreadIsMovedByWhoItWaitsFor.
	for name, tc := range map[string]struct {
		state proto.ThreadState
		shift proto.ShiftState
		want  Mover
	}{
		"paused":            {proto.ThreadOpen, proto.ShiftPaused, MoverOwner},
		"resumed, blocked":  {proto.ThreadBlocked, proto.ShiftRunning, MoverSpecialist},
		"summary, done":     {proto.ThreadDone, proto.ShiftSummary, MoverSpecialist},
		"stopped, blocked":  {proto.ThreadBlocked, proto.ShiftEnded, MoverSupervisor},
		"running, closed":   {proto.ThreadClosed, proto.ShiftRunning, MoverNobody},
		"paused and closed": {proto.ThreadClosed, proto.ShiftPaused, MoverNobody},
	} {
		th := boardThread(1, tc.state)
		th.Shift = &proto.Shift{State: tc.shift, Round: 2, MaxRounds: 8}
		if got := boardWith([]proto.Thread{th}, nil, nil).NextMover(th); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	th := boardThread(1, proto.ThreadOpen)
	th.Shift = &proto.Shift{State: proto.ShiftRunning, Round: 2, MaxRounds: 8}
	if got := boardWith([]proto.Thread{th}, []proto.Ask{{ID: 1, Thread: 1, State: proto.AskOpen}}, nil).NextMover(th); got != MoverOwner {
		t.Errorf("a running shift's thread with an open ask: %v", got)
	}
}

// A list's movers read the board's facts by thread, indexed once, never a
// scan of every pull request and ask for each thread; an indexed board
// moves each thread as a scanned one does (Codex review of #175, round
// 4).
func TestTheListsMoversReadTheBoardsIndex(t *testing.T) {
	scans := 0
	boardScanned = func() { scans++ }
	defer func() { boardScanned = nil }()
	_ = ThreadsText(threadsFixture())
	if scans != 0 {
		t.Fatalf("the list scanned the board %d times", scans)
	}
	th := threadsFixture()
	indexed := th.Board.Indexed()
	for id, want := range map[int64]Mover{5: MoverSpecialist, 6: MoverOwner, 7: MoverSupervisor, 3: MoverNobody} {
		if got := indexed.NextMover(th.Board.Threads[id]); got != want || got != th.Board.NextMover(th.Board.Threads[id]) {
			t.Errorf("T%d: indexed %v, scanned %v, want %v", id, got, th.Board.NextMover(th.Board.Threads[id]), want)
		}
	}
	// A pull request to review moves its thread to the owner from the index.
	waiting := boardWith([]proto.Thread{boardThread(1, proto.ThreadDone)}, nil, []proto.PullRequest{{ID: 1, Thread: 1, State: proto.PullOpen}}).Indexed()
	waiting.Pulls = nil
	if got := waiting.NextMover(waiting.Threads[1]); got != MoverOwner {
		t.Errorf("a pull request to review, from the index: %v", got)
	}
}

// ReachesTaskMover holds exactly where NextMover decides by a plan lead's
// tasks' threads: an open ask, a gate that waits, a ready task or a plan
// not approved decides first, and a read loads the tasks' threads only
// when it holds (Codex review of #175, round 4).
func TestReachesTaskMover(t *testing.T) {
	lead := proto.Thread{ID: 11, Agent: "duncan", State: proto.ThreadOpen, Plan: 1}
	plan := func(state proto.PlanState, task proto.TaskState, waiting ...string) proto.Plan {
		return proto.Plan{ID: 1, Thread: 11, State: state, Waiting: waiting,
			Graph: &proto.PlanGraph{Version: 1, Approved: state == proto.PlanApproved || state == proto.PlanBuilding, Tasks: []proto.PlanTask{{ID: "t1", State: task, Thread: 12}}}}
	}
	for name, tc := range map[string]struct {
		board Board
		th    proto.Thread
		want  bool
	}{
		"its tasks work":  {boardWith([]proto.Thread{lead}, nil, nil, plan(proto.PlanBuilding, proto.TaskDelegated)), lead, true},
		"a task is ready": {boardWith([]proto.Thread{lead}, nil, nil, plan(proto.PlanBuilding, proto.TaskReady)), lead, false},
		"its gate waits":  {boardWith([]proto.Thread{lead}, nil, nil, plan(proto.PlanBuilding, proto.TaskDelegated, "A3 open")), lead, false},
		"an open ask":     {boardWith([]proto.Thread{lead}, []proto.Ask{{ID: 3, Thread: 11, State: proto.AskOpen}}, nil, plan(proto.PlanBuilding, proto.TaskDelegated)), lead, false},
		"drafting":        {boardWith([]proto.Thread{lead}, nil, nil, plan(proto.PlanDrafting, proto.TaskWaiting)), lead, false},
		"no plan read":    {boardWith([]proto.Thread{lead}, nil, nil), lead, false},
		"closed":          {boardWith(nil, nil, nil, plan(proto.PlanBuilding, proto.TaskDelegated)), proto.Thread{ID: 11, State: proto.ThreadClosed, Plan: 1}, false},
		"a task's thread": {boardWith(nil, nil, nil, plan(proto.PlanBuilding, proto.TaskDelegated)), proto.Thread{ID: 12, State: proto.ThreadOpen, Plan: 1, PlanTask: "t1"}, false},
	} {
		if got := tc.board.ReachesTaskMover(tc.th); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
		// Where it holds, the mover follows the tasks' threads: with none
		// read, their specialists.
		if tc.want && tc.board.NextMover(tc.th) != MoverSpecialist {
			t.Errorf("%s: NextMover %v", name, tc.board.NextMover(tc.th))
		}
	}
}

// threadsFixture is a desk of threads read at inboxNow by session 1: one
// each mover moves, a review, and closed ones on both sides of the
// window's edge.
func threadsFixture() Threads {
	open := boardThread(5, proto.ThreadOpen)
	open.Title, open.UpdatedAt = "Fix CI", at(2*time.Hour)
	open.Shift = &proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 20}
	open.Follows = 3
	asking := boardThread(6, proto.ThreadBlocked)
	asking.Title, asking.Agent, asking.Supervisor, asking.UpdatedAt = "Tidy the docs", "duncan", 9, at(time.Hour)
	done := boardThread(7, proto.ThreadDone)
	done.Title, done.UpdatedAt = "Bump the pin", at(30*time.Minute)
	review := boardThread(8, proto.ThreadOpen)
	review.Title, review.Agent, review.UpdatedAt = "Review frostyard/example#12", "irulan", at(5*time.Minute)
	closed := boardThread(3, proto.ThreadClosed)
	closed.Title, closed.UpdatedAt, closed.Successor = "First try", at(2*24*time.Hour), 5
	old := boardThread(2, proto.ThreadClosed)
	old.Title, old.UpdatedAt = "Ancient", at(InboxWindow+time.Minute)
	closedReview := boardThread(4, proto.ThreadClosed)
	closedReview.Title, closedReview.UpdatedAt = "Review frostyard/example#11", at(time.Hour)
	return Threads{
		Now: inboxNow, Zone: time.UTC, PullsStarted: true, PullEvery: 5 * time.Minute, Caller: 1, Desk: 4, DeskCap: 12,
		Board: boardWith([]proto.Thread{open, asking, done, review, closed, old, closedReview},
			[]proto.Ask{{ID: 3, Thread: 6, State: proto.AskOpen}, {ID: 4, Thread: 6, State: proto.AskOpen}},
			[]proto.PullRequest{
				{ID: 1, Thread: 5, Repository: "frostyard/example", Number: 12, State: proto.PullOpen, Draft: true, CheckedAt: at(4 * time.Minute)},
				{ID: 2, Thread: 7, Repository: "frostyard/example", Number: 13, State: proto.PullMerged, Checks: proto.ChecksGreen, CheckedAt: at(time.Hour),
					Review: &proto.PullReview{Verdict: proto.ReviewReady}},
				{ID: 3, Thread: 3, Repository: "frostyard/example", Number: 11, State: proto.PullClosed, Checks: proto.ChecksFailing, CheckedAt: at(3 * 24 * time.Hour)},
			}),
		Reviews: map[int64]ThreadReview{8: {Pull: "frostyard/example#12", Head: strings.Repeat("c", 40)}, 4: {Pull: "frostyard/example#11", Head: "d"}},
	}
}

// The list groups the threads not closed by who moves them next, the most
// recently changed first, each with its state, shift, asks, pull requests
// and delegating session; then the cross-reviews that run, apart; then
// the threads closed in the window. An older closed thread and a closed
// review are left out (ADR-0071).
func TestThreadsTextGroupsTheThreadsByWhoMovesNext(t *testing.T) {
	text := ThreadsText(threadsFixture())
	for _, want := range []string{
		"team_threads at Sat 21 Nov 15:00 UTC, from pocketd's records alone: nothing was read on GitHub for this answer. pocketd reads each open pull request on GitHub again every 5m;",
		"On the desk: 4 threads not closed, of its cap of 12; a cross-review takes a place, a plan's own thread does not.",
		"Who moves each thread next is the work board's rule:",
		"Waiting for the owner (1):\n" + `- T6 "Tidy the docs" · duncan · blocked · 2 open asks · changed 1h ago · delegated by session 9` + "\n",
		"With their specialists (1):\n" + `- T5 "Fix CI" · vera · open · shift round 3 of 20, running · pull requests: frostyard/example#12 (open, draft, CI not read yet, no verdict, read 4m ago) · follows T3 · changed 2h ago · delegated by you` + "\n",
		"Waiting for their supervisor (1):\n" + `- T7 "Bump the pin" · vera · done · pull requests: frostyard/example#13 (merged, CI green, verdict ready, read 1h ago) · changed 30m ago · delegated by you` + "\n",
		"Cross-reviews pocketd runs and closes at their verdicts (1):\n" + `- T8 "Review frostyard/example#12" · irulan · open · reviews frostyard/example#12 at cccccccccccc · changed 5m ago` + "\n",
		"Closed threads whose records changed in the last 7 days (1):\n" + `- T3 "First try" · vera · closed · pull requests: frostyard/example#11 (closed, CI failing, no verdict, read 3d ago) · carried on by T5 · record last changed 2d ago · delegated by you` + "\n",
		"Read one thread whole with team_threads read.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the list lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"T2 ", "Ancient", "T4 ", "example#11 ·", "[team_threads: cut"} {
		if strings.Contains(text, not) {
			t.Errorf("the list holds %q:\n%s", not, text)
		}
	}
	// An empty desk says each list is empty.
	empty := Threads{Now: inboxNow, DeskCap: 12, Board: boardWith(nil, nil, nil)}
	for _, want := range []string{"Waiting for the owner: none.", "With their specialists: none.", "Waiting for their supervisor: none.",
		"Cross-reviews pocketd runs and closes at their verdicts: none.", "Closed threads whose records changed in the last 7 days: none.",
		"have not begun since it started"} {
		if text := ThreadsText(empty); !strings.Contains(text, want) {
			t.Errorf("an empty desk lacks %q:\n%s", want, text)
		}
	}
}

// A listed thread names at most a few pull requests, and how many more
// it has; a plan's own thread names its plan's state, and a task's thread
// its plan and task.
func TestThreadsTextNamesPlansAndCountsPullRequests(t *testing.T) {
	lead := proto.Thread{ID: 11, Title: "Signing", Agent: "duncan", State: proto.ThreadOpen, Plan: 1, Supervisor: 1}
	task := proto.Thread{ID: 12, Title: "Keys", Agent: "vera", State: proto.ThreadOpen, Plan: 1, PlanTask: "t1", Supervisor: 1}
	var pulls []proto.PullRequest
	for i := range threadsLinePulls + 2 {
		pulls = append(pulls, proto.PullRequest{ID: int64(i + 1), Thread: 12, Repository: "frostyard/example", Number: int64(100 + i), State: proto.PullOpen})
	}
	th := Threads{Now: inboxNow, Caller: 1, Board: boardWith([]proto.Thread{lead, task}, nil, pulls,
		proto.Plan{ID: 1, Thread: 11, State: proto.PlanDrafting})}
	text := ThreadsText(th)
	for _, want := range []string{`T11 "Signing" · duncan · open · leads plan P1, drafting`, `T12 "Keys" · vera · open · plan P1, task t1`,
		"frostyard/example#104 (open, CI not read yet, no verdict, read at no time pocketd recorded) and 2 more"} {
		if !strings.Contains(text, want) {
			t.Errorf("the list lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "#105") {
		t.Errorf("the list names a pull request past its few:\n%s", text)
	}
}

// The list groups the pull requests by thread once and writes only those
// a line shows: a thread with many writes its first few, and each
// thread's line names its own, whatever the board holds (Codex review of
// #175, round 3).
func TestThreadsTextWritesOnlyThePullRequestsItShows(t *testing.T) {
	th := Threads{Now: inboxNow, Caller: 1, Board: boardWith(nil, nil, nil)}
	id := int64(0)
	for thread := int64(1); thread <= 40; thread++ {
		state := proto.ThreadOpen
		if thread%2 == 0 {
			state = proto.ThreadClosed
		}
		th.Board.Threads[thread] = proto.Thread{ID: thread, Title: "t", Agent: "vera", State: state, Supervisor: 1, UpdatedAt: at(time.Hour)}
		for range 30 {
			id++
			th.Board.Pulls = append(th.Board.Pulls, proto.PullRequest{ID: id, Thread: thread, Repository: "frostyard/example", Number: id, State: proto.PullOpen})
		}
	}
	written := map[int64]int{}
	pullBriefed = func(p proto.PullRequest) { written[p.Thread]++ }
	defer func() { pullBriefed = nil }()
	text := th.listText(1<<30, 1<<30)
	for thread := int64(1); thread <= 40; thread++ {
		if written[thread] != threadsLinePulls {
			t.Errorf("T%d: %d pull requests written, want the %d shown", thread, written[thread], threadsLinePulls)
		}
	}
	if !strings.Contains(text, "frostyard/example#1 (") || !strings.Contains(text, "frostyard/example#5 (") || strings.Contains(text, "frostyard/example#6 (") ||
		!strings.Contains(text, "frostyard/example#31 (") || strings.Count(text, " and 25 more") != 40 {
		t.Errorf("the lines:\n%s", text)
	}
	byThread := th.pullsByThread()
	if len(byThread) != 40 || len(byThread[7]) != 30 || byThread[7][0].Number != 181 || byThread[7][29].Number != 210 {
		t.Errorf("the pull requests by thread: %d threads, T7's %+v", len(byThread), byThread[7])
	}
}

// A list longer than its bounds keeps the same number of first threads of
// every group, the most that fit, and says so; one more each would not
// have fitted.
func TestThreadsTextIsCutToFitItsBounds(t *testing.T) {
	long := strings.Repeat("\U0001F600", 200)
	th := Threads{Now: inboxNow, Zone: time.UTC, Caller: 1, Board: boardWith(nil, nil, nil)}
	for i := range 300 {
		id := int64(i + 1)
		state := []proto.ThreadState{proto.ThreadOpen, proto.ThreadDone}[i%2]
		thread := proto.Thread{ID: id, Title: long, Agent: "vera", State: state, Supervisor: 1, UpdatedAt: at(time.Duration(id) * time.Minute)}
		th.Board.Threads[id] = thread
	}
	closed := proto.Thread{ID: 1000, Title: "Done", Agent: "vera", State: proto.ThreadClosed, UpdatedAt: at(time.Hour)}
	th.Board.Threads[closed.ID] = closed
	text := ThreadsText(th)
	if len(text) > MaxThreadsBytes || strings.Count(text, "\n")+1 > MaxThreadsLines {
		t.Fatalf("%d bytes and %d lines", len(text), strings.Count(text, "\n")+1)
	}
	keep := 0
	if _, err := fmt.Sscanf(text[strings.Index(text, "each list shows its first "):], "each list shows its first %d items", &keep); err != nil || keep == 0 {
		t.Fatalf("no cut note (%v):\n%s", err, text)
	}
	for _, want := range []string{fmt.Sprintf("With their specialists (150; the first %d shown):", keep),
		fmt.Sprintf("Waiting for their supervisor (150; the first %d shown):", keep), "T1000 \"Done\""} {
		if !strings.Contains(text, want) {
			t.Errorf("the cut list lacks %q:\n%s", want, text)
		}
	}
	if (inboxBounds{MaxThreadsBytes, MaxThreadsLines}).fits(th.listText(1<<30, 1<<30)) {
		t.Fatal("the whole list fits: the test cuts nothing")
	}
	// Bounds even the headings break are met at a whole line, and said.
	if tiny := threadsFixture().listText(400, 6); len(tiny) > 400 || strings.Count(tiny, "\n")+1 > 6 ||
		!strings.HasSuffix(tiny, "[team_threads: cut at a whole line to fit its bounds.]") {
		t.Errorf("tiny bounds: %d bytes, %d lines:\n%s", len(tiny), strings.Count(tiny, "\n")+1, tiny)
	}
}

// wholeFixture is T5 of threadsFixture read whole: its sessions, folder,
// plan, worker, task, report, shift, asks of each state and pull requests.
func wholeFixture() (Threads, ThreadWhole) {
	th := threadsFixture()
	thread := th.Board.Threads[5]
	thread.Repository, thread.Branch, thread.Cwd = "frostyard/example", "vera/fix-ci-t5-ab12", "/src/example--vera-fix-ci-t5-ab12"
	thread.Task = "Fix the flaky test.\n\nRun make verify."
	thread.Report = "Round 3: the test is fixed."
	thread.Plan, thread.PlanTask = 2, "t1"
	thread.Successor, thread.Reopens, thread.ReopenedAt = 9, 1, atPtr(3*time.Hour)
	thread.CreatedAt = at(26 * time.Hour)
	deadline := inboxNow.Add(4 * time.Hour)
	thread.Shift = &proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 20, Stop: "CI is green on the pull request", CI: true, Deadline: &deadline,
		Rounds: []proto.ShiftRound{{N: 1, Verdict: "continue", Reason: "the test still fails", At: at(90 * time.Minute)}, {N: 2, Verdict: "continue", At: at(time.Hour)}}}
	th.Board.Threads[5] = thread
	return th, ThreadWhole{
		Thread: thread,
		Asks: []InboxAsk{
			{Ask: proto.Ask{ID: 1, Thread: 5, Question: "Squash?", State: proto.AskAnswered, Answer: "yes, squash it", AnsweredBy: "laptop", AnsweredAt: atPtr(time.Hour)}},
			{Ask: proto.Ask{ID: 2, Thread: 5, Question: "Which branch?", State: proto.AskWithdrawn}, WithdrawnAt: atPtr(30 * time.Minute)},
			{Ask: proto.Ask{ID: 3, Thread: 5, Question: "Ship it?", Options: []string{"yes", "no"}, State: proto.AskOpen, CreatedAt: at(10 * time.Minute)}},
		},
		Pulls: []proto.PullRequest{{ID: 1, Thread: 5, Repository: "frostyard/example", Number: 12, Title: "Fix the flaky test", State: proto.PullOpen, Draft: true,
			Ready: true, Checks: proto.ChecksGreen, Review: &proto.PullReview{Agent: "irulan", Family: "openai", Verdict: proto.ReviewReady}, CheckedAt: at(3 * time.Minute)}},
		Plan:        &proto.Plan{ID: 2, Title: "Sign the ISO", State: proto.PlanBuilding},
		Supervisor:  ThreadSession{ID: 1, Profile: "odrade"},
		Specialist:  ThreadSession{ID: 14, Profile: "vera", Model: proto.ModelRef{Provider: "faux", ID: "faux-1"}},
		Worker:      ThreadSession{ID: 15, Model: proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}},
		WorkerAsked: proto.ModelRef{Provider: "deepseek"},
	}
}

// A read shows the thread whole: who moves it next, who delegated it, its
// sessions and models, its folder, plan, successor, worker and reopens,
// its task and report, its shift with each round, every ask with its
// answer or withdrawal, and every pull request with CI and verdict
// (ADR-0071).
func TestThreadTextReadsAThreadWhole(t *testing.T) {
	th, w := wholeFixture()
	text := ThreadText(th, w)
	for _, want := range []string{
		"team_threads read T5 at Sat 21 Nov 15:00 UTC, from pocketd's records alone: nothing was read on GitHub for this answer.",
		`T5 "Fix CI" · vera · open · next: its specialist` + "\n",
		"- Delegated by: session 1, this session\n",
		"- Specialist's session: 14, faux/faux-1\n",
		"- Repository: frostyard/example · branch vera/fix-ci-t5-ab12 · worktree /src/example--vera-fix-ci-t5-ab12\n",
		`- Plan: delivers task t1 of plan P2 ("Sign the ISO", building)` + "\n",
		"- Follows: T3\n- Carried on by: T9\n",
		"- Worker: session 15, deepseek/deepseek-flash; the supervisor asked for deepseek\n",
		"- Reopened by a message once, last 3h ago (Sat 21 Nov 12:00 UTC)\n",
		"- Created 26h ago (Fri 20 Nov 13:00 UTC); last changed 2h ago (Sat 21 Nov 13:00 UTC)\n",
		"Task:\n  Fix the flaky test.\n  \n  Run make verify.\n",
		"Latest report:\n  Round 3: the test is fixed.\n",
		`Shift: running, round 3 of at most 20, until "CI is green on the pull request"; done also needs CI green; deadline Sat 21 Nov 19:00 UTC`,
		"Shift rounds (2):\n- round 1 · continue: the test still fails · 1h ago (Sat 21 Nov 13:30 UTC)",
		"- round 2 · continue · 1h ago",
		"Asks (3):\n" + `- A1 · answered 1h ago (Sat 21 Nov 14:00 UTC) on laptop: "Squash?" · the answer: "yes, squash it"` + "\n" +
			`- A2 · withdrawn 30m ago (Sat 21 Nov 14:30 UTC) by a supervisor's message, unanswered: "Which branch?"` + "\n" +
			`- A3 · open, asked 10m ago (Sat 21 Nov 14:50 UTC): "Ship it?" Options: yes | no.` + "\n",
		"Pull requests (1):\n" + `- frostyard/example#12 · "Fix the flaky test" · open, draft · ready-for-human: yes · verdict at its head: ready, by irulan (openai) · CI: green · read 3m ago`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the read lacks %q:\n%s", want, text)
		}
	}
	// Another supervisor's thread names its delegating session and
	// profile; a review says what it reviews; a home thread its home; a
	// thread that starts its session to come; a closed thread nobody.
	other := w
	other.Thread = th.Board.Threads[6]
	other.Thread.Cwd, other.Thread.State = "/home/duncan", proto.ThreadStarting
	other.Supervisor, other.Specialist, other.Worker, other.WorkerAsked, other.Plan = ThreadSession{ID: 9, Profile: "leto"}, ThreadSession{}, ThreadSession{}, proto.ModelRef{}, nil
	text = ThreadText(th, other)
	for _, want := range []string{"- Delegated by: session 9, of profile leto\n", "- Specialist's session: none yet; pocketd starts it\n",
		"- Working folder: /home/duncan\n", "- Worker: none yet\n", "next: the owner"} {
		if !strings.Contains(text, want) {
			t.Errorf("another supervisor's thread lacks %q:\n%s", want, text)
		}
	}
	// A cross-review copies its author's supervisor session, but pocketd
	// started it: the read never names a delegating session for it (Codex
	// review of #175, round 1).
	review := other
	review.Thread, review.Supervisor = th.Board.Threads[8], ThreadSession{ID: 1, Profile: "odrade"}
	text = ThreadText(th, review)
	if !strings.Contains(text, "- Started by: pocketd, as a cross-review of frostyard/example#12 at cccccccccccc; it closes at its verdict, and no supervisor sends to it or closes it\n") ||
		strings.Contains(text, "Delegated by") {
		t.Errorf("a review:\n%s", text)
	}
	// A thread that closed before it started has no session to come
	// (Codex review of #175, round 1).
	closed := other
	closed.Thread = th.Board.Threads[3]
	text = ThreadText(th, closed)
	if !strings.Contains(text, "· closed · next: nobody: it is closed") || !strings.Contains(text, "- Specialist's session: none; pocketd started no session for it\n") ||
		strings.Contains(text, "pocketd starts it") {
		t.Errorf("a closed thread that never started:\n%s", text)
	}
}

// A read cuts a long task and report to a fixed size and says so, and a
// read longer than its bounds keeps the same number of first items of its
// lists, the most that fit, and says so.
func TestThreadTextIsCutToFitItsBounds(t *testing.T) {
	th, w := wholeFixture()
	w.Thread.Task = strings.Repeat("\U0001F600", 5000)
	w.Thread.Report = strings.Repeat("line\n", 100)
	long := strings.Repeat("\U0001F600", 300)
	w.Asks, w.Pulls = nil, nil
	for i := range 200 {
		id := int64(i + 1)
		w.Asks = append(w.Asks, InboxAsk{Ask: proto.Ask{ID: id, Thread: 5, Question: long, State: proto.AskOpen, CreatedAt: at(time.Hour)}})
		w.Pulls = append(w.Pulls, proto.PullRequest{ID: id, Thread: 5, Repository: "frostyard/example", Number: id, Title: long, State: proto.PullOpen})
	}
	text := ThreadText(th, w)
	if len(text) > MaxThreadsBytes || strings.Count(text, "\n")+1 > MaxThreadsLines {
		t.Fatalf("%d bytes and %d lines", len(text), strings.Count(text, "\n")+1)
	}
	keep := 0
	if _, err := fmt.Sscanf(text[strings.Index(text, "each list shows its first "):], "each list shows its first %d items", &keep); err != nil || keep == 0 {
		t.Fatalf("no cut note (%v):\n%s", err, text)
	}
	for _, want := range []string{
		fmt.Sprintf("Task (5000 characters in 1 lines; the first %d characters shown):", threadsBlock),
		fmt.Sprintf("Latest report (499 characters in 100 lines; the first %d characters shown):", 5*threadsBlockLines-1),
		fmt.Sprintf("Asks (200; the first %d shown):", keep), fmt.Sprintf("Pull requests (200; the first %d shown):", keep),
		"Shift rounds (2):",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the cut read lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, fmt.Sprintf("A%d ·", keep+1)) {
		t.Errorf("A%d is past the cut", keep+1)
	}
}

// A read names the first few entries of a plan's gate and counts the
// rest: a gate grown across many reports never pushes the plan, the task,
// the report and the asks out of the answer (Codex review of #175, round
// 5).
func TestThreadTextBoundsAPlansGate(t *testing.T) {
	th, w := wholeFixture()
	plan := *w.Plan
	for i := range 3000 {
		plan.Waiting = append(plan.Waiting, fmt.Sprintf("A%d not written", 1000000+i))
	}
	w.Plan = &plan
	text := ThreadText(th, w)
	if len(text) > MaxThreadsBytes || strings.Count(text, "\n")+1 > MaxThreadsLines {
		t.Fatalf("%d bytes and %d lines", len(text), strings.Count(text, "\n")+1)
	}
	for _, want := range []string{
		"its gate waits for A1000000 not written, A1000001 not written",
		fmt.Sprintf("A%d not written and %d more)", 1000000+threadsGate-1, 3000-threadsGate),
		"Task:\n  Fix the flaky test.", "Latest report:\n  Round 3: the test is fixed.", "Asks (3):", "Pull requests (1):",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the read lacks %q:\n%.2000s", want, text)
		}
	}
	if strings.Contains(text, fmt.Sprintf("A%d not written", 1000000+threadsGate)) {
		t.Errorf("the read names a gate entry past its few")
	}
}

// The supervisor's instructions name team_threads (ADR-0071).
func TestThreadsGuideNamesTheTool(t *testing.T) {
	for _, want := range []string{"team_threads", "before you ask the owner or a specialist where a thread stands", "closed threads whose records changed in the last 7 days"} {
		if !strings.Contains(strings.ToLower(ThreadsGuide), strings.ToLower(want)) {
			t.Errorf("the guide lacks %q: %s", want, ThreadsGuide)
		}
	}
}
