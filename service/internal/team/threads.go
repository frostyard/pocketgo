package team

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The supervisor's view of the threads (ADR-0071): team_threads lists,
// from pocketd's own records alone, every thread not closed with who
// moves it next, as the desktop's work board groups it, and the threads
// closed in the last InboxWindow; and it reads one thread whole. The
// supervisor reads it before it asks the owner or a specialist where a
// thread stands.

// team_threads' actions.
const (
	ThreadsList = "list"
	ThreadsRead = "read"
)

// The bounds of team_threads' answers (ADR-0071).
const (
	// MaxThreadsBytes and MaxThreadsLines bound a whole answer, as
	// MaxInboxBytes and MaxInboxLines bound team_inbox's: when it would
	// be longer, every list is cut to the same number of first items, the
	// most that fit. The tool's output limits let an answer this size
	// through whole.
	MaxThreadsBytes = 24 << 10
	MaxThreadsLines = 400
	// A read shows at most this many characters and lines of a thread's
	// task and of its latest report: with a full list of each, both fit
	// the bounds.
	threadsBlock      = 2000
	threadsBlockLines = 30
	// The most pull requests a listed thread's line names; a read names
	// them all.
	threadsLinePulls = 5
	// The most characters a read shows of an answer, a shift's stop and
	// a reason.
	threadsAnswer = 300
	threadsStop   = 300
	threadsWhy    = 200
)

// ThreadsGuide is the line of the supervisor's instructions that names
// team_threads (ADR-0071).
var ThreadsGuide = fmt.Sprintf("Before you ask the owner or a specialist where a thread stands, read it with team_threads: list shows every thread not closed with who moves it next, and the closed threads whose records changed in the last %d days; read shows one whole, with its asks, pull requests and shift. ", InboxDays)

// ThreadsCall is a team_threads call.
type ThreadsCall struct {
	Action string `json:"action"`
	// Thread is the thread a read reads, 5 for T5; a list takes none.
	Thread int64 `json:"thread,omitempty"`
}

// CheckThreadsCall refuses a team_threads call that breaks a rule.
func CheckThreadsCall(c ThreadsCall) error {
	switch c.Action {
	case ThreadsList:
		if c.Thread != 0 {
			return errors.New("list takes no thread: it lists them all; read one with action read")
		}
	case ThreadsRead:
		if c.Thread <= 0 {
			return errors.New("read needs thread, the thread's number: 5 for T5")
		}
	default:
		return fmt.Errorf("action %q is not list or read", c.Action)
	}
	return nil
}

// CheckThreadsCaller refuses team_threads from a caller that is no
// supervisor: caller is the calling session's profile, nil for a session
// without one.
func CheckThreadsCaller(caller *profiles.Profile) error {
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return errors.New("only a supervisor reads the threads with team_threads: this session's profile is not one")
	}
	return nil
}

// Mover is who moves a thread next, as the desktop's work board groups
// threads (state.Mover).
type Mover int

const (
	// MoverOwner: a question of the thread waits for the owner's answer,
	// one of its pull requests for the owner's review, its plan for the
	// owner's approval, or its paused shift for the owner.
	MoverOwner Mover = iota
	// MoverSpecialist: the specialist works on it, or pocketd sets it up.
	MoverSpecialist
	// MoverSupervisor: the specialist reported it done or blocked, and its
	// supervisor reads the report, then closes it or briefs again.
	MoverSupervisor
	// MoverNobody: the thread is closed.
	MoverNobody
)

// String is who m is, as team_threads names it.
func (m Mover) String() string {
	switch m {
	case MoverOwner:
		return "the owner"
	case MoverSpecialist:
		return "its specialist"
	case MoverSupervisor:
		return "its supervisor"
	}
	return "nobody: it is closed"
}

// Board is what NextMover reads, as the desktop's work board holds it: the
// threads, each with its shift; the open asks, as GET /v1/asks lists
// them; the pull requests; and the plans, by id.
type Board struct {
	Threads map[int64]proto.Thread
	Asks    []proto.Ask
	Pulls   []proto.PullRequest
	Plans   map[int64]proto.Plan
	// index, once Indexed made it, holds by thread whether one of its pull
	// requests waits for review and how many of its asks are open, read in
	// one pass over Pulls and Asks: a list's movers read it, never a scan
	// of the board for each thread (Codex review of #175, round 4). nil
	// for a board not indexed, whose readers scan.
	index *boardIndex
}

// boardIndex is a board's facts by thread (Board.Indexed).
type boardIndex struct {
	pullWaits map[int64]bool
	openAsks  map[int64]int
}

// Indexed is b with its facts by thread read in one pass: whether a pull
// request of each waits for review (PullWaits, which only an open one
// does, and an open one shows whatever its thread, as state.Data.PullsOf
// lists it) and how many of its asks are open.
func (b Board) Indexed() Board {
	ix := &boardIndex{pullWaits: map[int64]bool{}, openAsks: map[int64]int{}}
	for _, p := range b.Pulls {
		if PullWaits(p) {
			ix.pullWaits[p.Thread] = true
		}
	}
	for _, a := range b.Asks {
		if a.State == proto.AskOpen {
			ix.openAsks[a.Thread]++
		}
	}
	b.index = ix
	return b
}

// boardScanned, when a test sets it, sees each scan of a board not
// indexed; nil otherwise.
var boardScanned func()

// NextMover is who moves t next. It mirrors the desktop's work board,
// state.Data.NextMover in desktop/internal/state/board.go, rule for rule:
// a change to one is made to the other (ADR-0071). Nobody once t is
// closed; else the owner while one of its asks is open, one of its pull
// requests waits for review (PullWaits), or the plan it plans waits for
// the owner's approval (PlanWaits). A plan's own thread whose plan is
// approved is the owner's while its gate waits (an open ask, as the plan
// says), its supervisor's while a task of it is ready to deliver, and its
// tasks' specialists' otherwise. Else a thread whose shift is paused is
// the owner's, and one whose shift runs or asks for its summary is its
// specialist's (ADR-0024). Else a thread is its specialist's while it
// starts or is open, and its supervisor's once the specialist reported it
// done or blocked; a state the rule does not know goes to the supervisor,
// who has the thread's report.
func (b Board) NextMover(t proto.Thread) Mover {
	if t.State == proto.ThreadClosed {
		return MoverNobody
	}
	if b.hasOpenAsk(t.ID) {
		return MoverOwner
	}
	if p, ok := b.Plans[t.Plan]; ok && planOwnThread(t) {
		switch {
		case PlanWaits(p), len(gateAsks(p)) > 0:
			return MoverOwner
		case p.State == proto.PlanDrafting && len(p.Waiting) > 0:
			// An answer to write in, or a consult that runs: the lead's.
			return MoverSpecialist
		case p.State == proto.PlanApproved || p.State == proto.PlanBuilding:
			if len(planReady(p)) > 0 {
				return MoverSupervisor
			}
			return b.taskMover(p)
		}
	}
	if b.pullWaiting(t.ID) {
		return MoverOwner
	}
	if sh := t.Shift; sh != nil {
		switch sh.State {
		case proto.ShiftPaused:
			return MoverOwner
		case proto.ShiftRunning, proto.ShiftSummary:
			return MoverSpecialist
		}
	}
	switch t.State {
	case proto.ThreadStarting, proto.ThreadOpen:
		return MoverSpecialist
	default:
		return MoverSupervisor
	}
}

// taskMover is who moves an approved plan with no task to deliver: the
// owner when a delivered task's thread waits for them, else the
// supervisor when one waits for it, else the tasks' specialists
// (state.Data.taskMover).
func (b Board) taskMover(p proto.Plan) Mover {
	best := MoverSpecialist
	for _, id := range planTaskThreads(p) {
		th, ok := b.Threads[id]
		if !ok {
			continue
		}
		switch b.NextMover(th) {
		case MoverOwner:
			return MoverOwner
		case MoverSupervisor:
			best = MoverSupervisor
		}
	}
	return best
}

// hasOpenAsk says an ask of thread waits for the owner (state.Data.AsksOf).
func (b Board) hasOpenAsk(thread int64) bool { return b.openAsks(thread) > 0 }

// openAsks counts thread's open asks.
func (b Board) openAsks(thread int64) int {
	if b.index != nil {
		return b.index.openAsks[thread]
	}
	if boardScanned != nil {
		boardScanned()
	}
	n := 0
	for _, a := range b.Asks {
		if a.Thread == thread && a.State == proto.AskOpen {
			n++
		}
	}
	return n
}

// pullWaiting says a pull request of thread waits for the owner's review,
// as the work board finds it among the thread's pull requests it shows
// (state.Data.PullsOf, ToReview): PullWaits holds only of an open one,
// and the board shows an open one whatever its thread.
func (b Board) pullWaiting(thread int64) bool {
	if b.index != nil {
		return b.index.pullWaits[thread]
	}
	if boardScanned != nil {
		boardScanned()
	}
	return slices.ContainsFunc(b.Pulls, func(p proto.PullRequest) bool { return p.Thread == thread && PullWaits(p) })
}

// ReachesTaskMover says NextMover of t, on b, decides by its plan's
// delivered tasks' threads (taskMover), as NextMover's precedence reaches
// it: a plan's own thread not closed, with no open ask, whose approved or
// building plan waits for no open ask and has no task ready. Only then
// does it read those threads, and a read of t loads them only then (Codex
// review of #175, round 4).
func (b Board) ReachesTaskMover(t proto.Thread) bool {
	if t.State == proto.ThreadClosed || b.hasOpenAsk(t.ID) || !planOwnThread(t) {
		return false
	}
	p, ok := b.Plans[t.Plan]
	if !ok || PlanWaits(p) || len(gateAsks(p)) > 0 {
		return false
	}
	return (p.State == proto.PlanApproved || p.State == proto.PlanBuilding) && len(planReady(p)) == 0
}

// planOwnThread says t is a plan's own thread, whose specialist plans
// (state.PlanThread).
func planOwnThread(t proto.Thread) bool { return t.Plan != 0 && t.PlanTask == "" }

// gateAsks are the entries of p's gate that wait for the owner: its open
// asks ("A3 open") (state.OpenAsksOf).
func gateAsks(p proto.Plan) []string {
	var out []string
	for _, w := range p.Waiting {
		if strings.HasSuffix(w, " open") {
			out = append(out, w)
		}
	}
	return out
}

// planReady are the ids of p's tasks that the supervisor may deliver now,
// in graph order: ready in its approved graph (state.ReadyTasks).
func planReady(p proto.Plan) []string {
	if p.Graph == nil || !p.Graph.Approved || (p.State != proto.PlanApproved && p.State != proto.PlanBuilding) {
		return nil
	}
	var out []string
	for _, t := range p.Graph.Tasks {
		if t.State == proto.TaskReady {
			out = append(out, t.ID)
		}
	}
	return out
}

// planTaskThreads are the threads of p's delivered tasks, in graph order
// (state.TaskThreads).
func planTaskThreads(p proto.Plan) []int64 {
	if p.Graph == nil {
		return nil
	}
	var out []int64
	for _, t := range p.Graph.Tasks {
		if t.State == proto.TaskDelegated && t.Thread != 0 {
			out = append(out, t.Thread)
		}
	}
	return out
}

// ThreadReview is what team_threads shows of a cross-review pocketd
// started (ADR-0019): the pull request it reviews, as owner/name#number,
// and the head it reviews.
type ThreadReview struct {
	Pull string
	Head string
}

// Threads is what team_threads reads, all of it from pocketd's records:
// no call to GitHub is made for it.
type Threads struct {
	// Now is when it was read; Zone is the service's time zone, which its
	// times are shown in.
	Now  time.Time
	Zone *time.Location
	// PullsStarted and PullEvery say how fresh the pull requests are, as
	// team_inbox's do.
	PullsStarted bool
	PullEvery    time.Duration
	// Caller is the calling session: the threads it delegated are marked.
	Caller int64
	// Desk counts the threads on the desk as its cap counts them, and
	// DeskCap is the cap.
	Desk, DeskCap int
	// Board holds every thread not closed and every thread closed whose
	// record changed in the window, each with its shift; the open asks;
	// those threads' pull requests, each with its verdict at its head; and
	// the plans of the plan threads not closed.
	Board Board
	// Reviews are the cross-reviews pocketd started, by thread.
	Reviews map[int64]ThreadReview
}

// ThreadsText is team_threads' list: a few lines on where its facts come
// from, the desk and the rule of who moves next, then every thread not
// closed, grouped by who moves it next, the most recently changed first,
// then the cross-reviews pocketd runs, then the threads closed whose
// record changed in the window. An answer longer than MaxThreadsBytes or
// MaxThreadsLines keeps the same number of first items of every list, the
// most that fit, says so at its top, and each cut list's heading says how
// many items it has.
func ThreadsText(t Threads) string { return t.listText(MaxThreadsBytes, MaxThreadsLines) }

// listText is ThreadsText within maxBytes and maxLines.
func (t Threads) listText(maxBytes, maxLines int) string {
	since := InboxSince(t.Now)
	lists := t.lists(since)
	return fitLists(inboxBounds{maxBytes, maxLines}, lists, func(keep int, cut *inboxBounds) string {
		var b strings.Builder
		t.head(&b, "team_threads", cut, keep)
		fmt.Fprintf(&b, "On the desk: %d threads not closed, of its cap of %d; a cross-review takes a place, a plan's own thread does not.\n", t.Desk, t.DeskCap)
		b.WriteString("Who moves each thread next is the work board's rule: the owner while an ask of it is open, one of its pull requests waits for review (open, no draft), its plan waits for approval or its shift is paused; its specialist while it starts or works, or its shift runs; its supervisor once the specialist reported it done or blocked.\n")
		for _, l := range lists {
			b.WriteString("\n")
			writeList(&b, l, keep)
		}
		b.WriteString("\nRead one thread whole with team_threads read. A closed thread's time is when pocketd last changed its record: its close, or a later note on its report.")
		return b.String()
	}, "[team_threads: cut at a whole line to fit its bounds.]")
}

// head writes the answer's first lines: when it was read, where its facts
// come from and how fresh its pull requests are, and, when it was cut to
// fit cut, how.
func (t Threads) head(b *strings.Builder, what string, cut *inboxBounds, keep int) {
	fmt.Fprintf(b, "%s at %s, from pocketd's records alone: nothing was read on GitHub for this answer. %s\n", what, t.clock().when(t.Now), pullFreshness(t.PullsStarted, t.PullEvery))
	if cut != nil {
		fmt.Fprintf(b, "[team_threads: cut to fit %d bytes and %d lines: each list shows its first %d items, and a cut list's heading says how many it has.]\n", cut.bytes, cut.lines, keep)
	}
}

func (t Threads) clock() readClock { return readClock{t.Now, t.Zone} }

// sorted are the board's threads, the most recently changed first.
func (t Threads) sorted() []proto.Thread {
	out := make([]proto.Thread, 0, len(t.Board.Threads))
	for _, th := range t.Board.Threads {
		out = append(out, th)
	}
	slices.SortFunc(out, func(a, b proto.Thread) int {
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(b.ID, a.ID))
	})
	return out
}

// lists are the list's lists: the threads not closed by who moves them
// next, the cross-reviews that run, and the threads closed at since or
// later. A closed review is left out: its verdict shows on its pull
// request.
func (t Threads) lists(since time.Time) []inboxList {
	t.Board = t.Board.Indexed()
	pulls := t.pullsByThread()
	movers := map[Mover][]string{}
	var reviews, closed []string
	for _, th := range t.sorted() {
		_, review := t.Reviews[th.ID]
		switch {
		case review && th.State != proto.ThreadClosed:
			reviews = append(reviews, t.reviewLine(th))
		case review:
		case th.State == proto.ThreadClosed:
			if !th.UpdatedAt.Before(since) {
				closed = append(closed, t.closedLine(th, pulls[th.ID]))
			}
		default:
			m := t.Board.NextMover(th)
			movers[m] = append(movers[m], t.openLine(th, pulls[th.ID]))
		}
	}
	return []inboxList{
		{"Waiting for the owner", movers[MoverOwner]},
		{"With their specialists", movers[MoverSpecialist]},
		{"Waiting for their supervisor", movers[MoverSupervisor]},
		{"Cross-reviews pocketd runs and closes at their verdicts", reviews},
		{fmt.Sprintf("Closed threads whose records changed in the last %d days", InboxDays), closed},
	}
}

// openLine is a thread not closed, with its pull requests, as the list
// shows it.
func (t Threads) openLine(th proto.Thread, pulls []proto.PullRequest) string {
	parts := []string{threadName(th), th.Agent, string(th.State)}
	if plan := t.planPart(th); plan != "" {
		parts = append(parts, plan)
	}
	if sh := th.Shift; sh != nil {
		parts = append(parts, shiftPart(sh))
	}
	switch n := t.Board.openAsks(th.ID); n {
	case 0:
	case 1:
		parts = append(parts, "1 open ask")
	default:
		parts = append(parts, fmt.Sprintf("%d open asks", n))
	}
	if part := t.pullsPart(pulls); part != "" {
		parts = append(parts, part)
	}
	if th.Follows != 0 {
		parts = append(parts, fmt.Sprintf("follows T%d", th.Follows))
	}
	parts = append(parts, "changed "+t.clock().rel(th.UpdatedAt), t.delegator(th))
	return strings.Join(parts, " · ")
}

// reviewLine is a cross-review that runs as the list shows it.
func (t Threads) reviewLine(th proto.Thread) string {
	r := t.Reviews[th.ID]
	return strings.Join([]string{threadName(th), th.Agent, string(th.State), fmt.Sprintf("reviews %s at %s", r.Pull, short(r.Head)),
		"changed " + t.clock().rel(th.UpdatedAt)}, " · ")
}

// closedLine is a closed thread, with its pull requests, as the list
// shows it.
func (t Threads) closedLine(th proto.Thread, pulls []proto.PullRequest) string {
	parts := []string{threadName(th), th.Agent, "closed"}
	if part := t.pullsPart(pulls); part != "" {
		parts = append(parts, part)
	}
	if th.Successor != 0 {
		parts = append(parts, fmt.Sprintf("carried on by T%d", th.Successor))
	}
	parts = append(parts, "record last changed "+t.clock().rel(th.UpdatedAt), t.delegator(th))
	return strings.Join(parts, " · ")
}

// planPart names th's plan: the plan it leads, with the plan's state, or
// the plan and task it delivers; "" for none.
func (t Threads) planPart(th proto.Thread) string {
	switch {
	case th.Plan == 0:
		return ""
	case th.PlanTask != "":
		return fmt.Sprintf("plan P%d, task %s", th.Plan, th.PlanTask)
	}
	if p, ok := t.Board.Plans[th.Plan]; ok {
		return fmt.Sprintf("leads plan P%d, %s", th.Plan, p.State)
	}
	return fmt.Sprintf("leads plan P%d", th.Plan)
}

// shiftPart is a shift's round and state on one line.
func shiftPart(sh *proto.Shift) string {
	part := fmt.Sprintf("shift round %d of %d, %s", sh.Round, sh.MaxRounds, sh.State)
	if sh.State != proto.ShiftRunning && sh.Reason != "" {
		part += " (" + sh.Reason + ")"
	}
	return part
}

// pullsPart names a thread's pull requests, pulls, on one line, at most
// threadsLinePulls of them: "pull requests: frostyard/x#3 (open, CI
// green, verdict ready, read 5m ago)"; "" for none. Only those shown are
// written.
func (t Threads) pullsPart(pulls []proto.PullRequest) string {
	shown := pulls[:min(len(pulls), threadsLinePulls)]
	refs := make([]string, len(shown))
	for i, p := range shown {
		refs[i] = t.pullBrief(p)
	}
	switch n := len(pulls); {
	case n == 0:
		return ""
	case n > threadsLinePulls:
		return fmt.Sprintf("pull requests: %s and %d more", strings.Join(refs, ", "), n-threadsLinePulls)
	}
	return "pull requests: " + strings.Join(refs, ", ")
}

// pullBriefed, when a test sets it, sees each pull request a list's line
// writes; nil otherwise.
var pullBriefed func(proto.PullRequest)

// pullsByThread are the board's pull requests by thread, each thread's in
// the board's order: one pass, however many threads are listed (Codex
// review of #175, round 3).
func (t Threads) pullsByThread() map[int64][]proto.PullRequest {
	out := map[int64][]proto.PullRequest{}
	for _, p := range t.Board.Pulls {
		out[p.Thread] = append(out[p.Thread], p)
	}
	return out
}

// pullBrief is p as a list's line names it: its reference, then its
// state, draft, CI and verdict at its head as pocketd last read them, and
// how long ago that read was: the thread's own age says nothing of it
// (Codex review of #175, round 1).
func (t Threads) pullBrief(p proto.PullRequest) string {
	if pullBriefed != nil {
		pullBriefed(p)
	}
	facts := []string{string(p.State)}
	if p.Draft {
		facts = append(facts, "draft")
	}
	facts = append(facts, "CI "+checksOf(p))
	verdict := "no verdict"
	if p.Review != nil {
		verdict = "verdict " + string(p.Review.Verdict)
	}
	facts = append(facts, verdict, "read "+t.clock().rel(p.CheckedAt))
	return fmt.Sprintf("%s#%d (%s)", p.Repository, p.Number, strings.Join(facts, ", "))
}

// checksOf is p's CI as pocketd last read it.
func checksOf(p proto.PullRequest) string {
	if p.Checks == "" {
		return "not read yet"
	}
	return string(p.Checks)
}

// threadName names th with its title, as T5 "Fix CI".
func threadName(th proto.Thread) string {
	name := "T" + strconv.FormatInt(th.ID, 10)
	if title := flat(th.Title, inboxTitle); title != "" {
		name += " " + strconv.Quote(title)
	}
	return name
}

// delegator says which session delegated th: the calling one, or another.
func (t Threads) delegator(th proto.Thread) string {
	if th.Supervisor == t.Caller {
		return "delegated by you"
	}
	return fmt.Sprintf("delegated by session %d", th.Supervisor)
}

// ThreadSession is a session team_threads names: its id, its profile and
// its model; a zero ID for none.
type ThreadSession struct {
	ID      int64
	Profile string
	Model   proto.ModelRef
}

// ThreadWhole is one thread as team_threads' read shows it.
type ThreadWhole struct {
	// Thread is the thread with its shift.
	Thread proto.Thread
	// Asks are every ask of the thread, oldest first; Pulls every pull
	// request it links, oldest first, each with its verdict at its head.
	Asks  []InboxAsk
	Pulls []proto.PullRequest
	// Plan is the plan it works for, nil for none.
	Plan *proto.Plan
	// Supervisor is the session that delegated it, Specialist its
	// specialist's and Worker its worker's.
	Supervisor, Specialist, Worker ThreadSession
	// WorkerAsked is the worker model the supervisor asked for it,
	// zero for none.
	WorkerAsked proto.ModelRef
}

// ThreadText is team_threads' read of w: where its facts come from, then
// the thread whole, its task and latest report each cut to a fixed size,
// then its shift's rounds, its asks and its pull requests. An answer
// longer than MaxThreadsBytes or MaxThreadsLines keeps the same number of
// first items of each of those lists, the most that fit, and says so.
func ThreadText(t Threads, w ThreadWhole) string {
	return t.readText(w, MaxThreadsBytes, MaxThreadsLines)
}

// readText is ThreadText within maxBytes and maxLines.
func (t Threads) readText(w ThreadWhole, maxBytes, maxLines int) string {
	th := w.Thread
	lists := []inboxList{
		{"Asks", t.askLines(w.Asks)},
		{"Pull requests", t.wholePullLines(w.Pulls)},
	}
	if th.Shift != nil {
		lists = append([]inboxList{{"Shift rounds", t.roundLines(th.Shift.Rounds)}}, lists...)
	}
	return fitLists(inboxBounds{maxBytes, maxLines}, lists, func(keep int, cut *inboxBounds) string {
		var b strings.Builder
		t.head(&b, fmt.Sprintf("team_threads read T%d", th.ID), cut, keep)
		fmt.Fprintf(&b, "\n%s · %s · %s · next: %s\n", threadName(th), th.Agent, th.State, t.Board.NextMover(th))
		for _, line := range t.facts(w) {
			b.WriteString("- " + line + "\n")
		}
		writeBlock(&b, "Task", th.Task)
		writeBlock(&b, "Latest report", th.Report)
		if sh := th.Shift; sh != nil {
			b.WriteString("\n" + t.shiftLine(sh) + "\n")
		}
		for _, l := range lists {
			b.WriteString("\n")
			writeList(&b, l, keep)
		}
		b.WriteString("\nAn ask's times are pocketd's own; a pull request's state, CI and verdict are as pocketd last read them.")
		return b.String()
	}, "[team_threads: cut at a whole line to fit its bounds.]")
}

// facts are the read's lines on where the thread comes from and works.
func (t Threads) facts(w ThreadWhole) []string {
	th := w.Thread
	c := t.clock()
	var out []string
	// A cross-review copies its author's thread's supervisor session, but
	// pocketd started it, and no supervisor sends to it or closes it
	// (ADR-0019; Codex review of #175, round 1).
	if r, ok := t.Reviews[th.ID]; ok {
		out = append(out, fmt.Sprintf("Started by: pocketd, as a cross-review of %s at %s; it closes at its verdict, and no supervisor sends to it or closes it",
			r.Pull, short(r.Head)))
	} else {
		switch w.Supervisor.ID {
		case 0:
			out = append(out, "Delegated by: no session pocketd recorded")
		case t.Caller:
			out = append(out, fmt.Sprintf("Delegated by: session %d, this session", w.Supervisor.ID))
		default:
			out = append(out, fmt.Sprintf("Delegated by: session %d, %s", w.Supervisor.ID, profileOf(w.Supervisor)))
		}
	}
	switch {
	case w.Specialist.ID != 0:
		out = append(out, fmt.Sprintf("Specialist's session: %d, %s", w.Specialist.ID, modelOf(w.Specialist.Model)))
	case th.State == proto.ThreadStarting:
		out = append(out, "Specialist's session: none yet; pocketd starts it")
	default:
		// A thread that failed before its start closed with no session
		// (Codex review of #175, round 1).
		out = append(out, "Specialist's session: none; pocketd started no session for it")
	}
	switch {
	case th.Repository != "":
		out = append(out, fmt.Sprintf("Repository: %s · branch %s · worktree %s", th.Repository, orNone(th.Branch), orNone(th.Cwd)))
	case th.Cwd != "":
		// The profile's home or its workspace: the thread's record names
		// the folder, not which (Codex review of #175, round 3).
		out = append(out, "Working folder: "+th.Cwd)
	}
	if line := planLine(th, w.Plan); line != "" {
		out = append(out, line)
	}
	if th.Follows != 0 {
		out = append(out, fmt.Sprintf("Follows: T%d", th.Follows))
	}
	if th.Successor != 0 {
		out = append(out, fmt.Sprintf("Carried on by: T%d", th.Successor))
	}
	out = append(out, workerLine(w))
	if th.Reopens > 0 {
		line := "Reopened by a message once"
		if th.Reopens > 1 {
			line = fmt.Sprintf("Reopened by a message %d times", th.Reopens)
		}
		if th.ReopenedAt != nil {
			line += ", last " + c.ago(*th.ReopenedAt)
		}
		out = append(out, line)
	}
	out = append(out, fmt.Sprintf("Created %s; last changed %s", c.ago(th.CreatedAt), c.ago(th.UpdatedAt)))
	return out
}

// planLine names the plan th works for, "" for none.
func planLine(th proto.Thread, p *proto.Plan) string {
	if th.Plan == 0 {
		return ""
	}
	state := ""
	if p != nil {
		state = fmt.Sprintf(" (%q, %s", flat(p.Title, inboxTitle), p.State)
		if len(p.Waiting) > 0 {
			state += "; its gate waits for " + strings.Join(p.Waiting, ", ")
		}
		state += ")"
	}
	if th.PlanTask != "" {
		return fmt.Sprintf("Plan: delivers task %s of plan P%d%s", th.PlanTask, th.Plan, state)
	}
	return fmt.Sprintf("Plan: its specialist leads plan P%d%s", th.Plan, state)
}

// workerLine names th's worker and the worker model asked for it.
func workerLine(w ThreadWhole) string {
	asked := ""
	if w.WorkerAsked.Provider != "" {
		asked = "; the supervisor asked for " + modelOf(w.WorkerAsked)
	}
	if w.Worker.ID == 0 {
		return "Worker: none yet" + asked
	}
	return fmt.Sprintf("Worker: session %d, %s%s", w.Worker.ID, modelOf(w.Worker.Model), asked)
}

// modelOf is m as provider/id.
func modelOf(m proto.ModelRef) string {
	if m.Provider == "" && m.ID == "" {
		return "no model recorded"
	}
	if m.ID == "" {
		return m.Provider
	}
	return m.Provider + "/" + m.ID
}

// profileOf names s's profile.
func profileOf(s ThreadSession) string {
	if s.Profile == "" {
		return "of no profile"
	}
	return "of profile " + s.Profile
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// writeBlock writes text under heading, each line indented, cut to
// threadsBlock characters and threadsBlockLines lines, and says so when
// it was cut.
func writeBlock(b *strings.Builder, heading, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintf(b, "\n%s: none.\n", heading)
		return
	}
	n := utf8.RuneCountInString(text)
	lines := strings.Split(text, "\n")
	shown := text
	if n > threadsBlock {
		shown = string([]rune(text)[:threadsBlock])
	}
	if shownLines := strings.Split(shown, "\n"); len(shownLines) > threadsBlockLines {
		shown = strings.Join(shownLines[:threadsBlockLines], "\n")
	}
	if shown == text {
		fmt.Fprintf(b, "\n%s:\n", heading)
	} else {
		fmt.Fprintf(b, "\n%s (%d characters in %d lines; the first %d characters shown):\n", heading, n, len(lines), utf8.RuneCountInString(shown))
	}
	for _, line := range strings.Split(shown, "\n") {
		b.WriteString("  " + strings.TrimRight(line, " \t\r") + "\n")
	}
}

// shiftLine is a read's line on th's shift.
func (t Threads) shiftLine(sh *proto.Shift) string {
	line := fmt.Sprintf("Shift: %s, round %d of at most %d, until %q", sh.State, sh.Round, sh.MaxRounds, flat(sh.Stop, threadsStop))
	if sh.CI {
		line += "; done also needs CI green"
	}
	if sh.Deadline != nil {
		line += "; deadline " + t.clock().when(*sh.Deadline)
	}
	if sh.Reason != "" {
		line += "; " + sh.Reason
		if why := flat(sh.Why, threadsWhy); why != "" {
			line += ": " + why
		}
	}
	if sh.EndedAt != nil {
		line += "; ended " + t.clock().ago(*sh.EndedAt)
	}
	return line
}

// roundLines are a shift's rounds that ended, oldest first.
func (t Threads) roundLines(rounds []proto.ShiftRound) []string {
	out := make([]string, len(rounds))
	for i, r := range rounds {
		line := fmt.Sprintf("round %d · %s", r.N, r.Verdict)
		if reason := flat(r.Reason, threadsWhy); reason != "" {
			line += ": " + reason
		}
		out[i] = line + " · " + t.clock().ago(r.At)
	}
	return out
}

// askLines are every ask of a thread, oldest first, each with its state.
func (t Threads) askLines(asks []InboxAsk) []string {
	c := t.clock()
	out := make([]string, len(asks))
	for i, a := range asks {
		question := fmt.Sprintf("%q", flat(a.Question, inboxQuestion))
		switch a.State {
		case proto.AskAnswered:
			by := ""
			if device := flat(a.AnsweredBy, inboxDevice); device != "" {
				by = " on " + device
			}
			at := "at no time pocketd recorded"
			if a.AnsweredAt != nil {
				at = c.ago(*a.AnsweredAt)
			}
			out[i] = fmt.Sprintf("A%d · answered %s%s: %s · the answer: %q", a.ID, at, by, question, flat(a.Answer, threadsAnswer))
		case proto.AskWithdrawn:
			at := "at no time pocketd recorded"
			if a.WithdrawnAt != nil {
				at = c.ago(*a.WithdrawnAt)
			}
			out[i] = fmt.Sprintf("A%d · withdrawn %s by a supervisor's message, unanswered: %s", a.ID, at, question)
		default:
			line := fmt.Sprintf("A%d · open, asked %s: %s", a.ID, c.ago(a.CreatedAt), question)
			if len(a.Options) > 0 {
				options := make([]string, len(a.Options))
				for j, o := range a.Options {
					options[j] = flat(o, inboxTitle)
				}
				line += " Options: " + strings.Join(options, " | ") + "."
			}
			out[i] = line
		}
	}
	return out
}

// wholePullLines are every pull request of a thread, oldest first, as a
// read shows them.
func (t Threads) wholePullLines(pulls []proto.PullRequest) []string {
	c := t.clock()
	out := make([]string, len(pulls))
	for i, p := range pulls {
		state := string(p.State)
		if p.Draft {
			state += ", draft"
		}
		ready := "no"
		if p.Ready {
			ready = "yes"
		}
		verdict := "none"
		if r := p.Review; r != nil {
			verdict = fmt.Sprintf("%s, by %s", r.Verdict, r.Agent)
			if r.Family != "" {
				verdict += " (" + r.Family + ")"
			}
		}
		out[i] = fmt.Sprintf("%s#%d · %q · %s · ready-for-human: %s · verdict at its head: %s · CI: %s · read %s",
			p.Repository, p.Number, flat(p.Title, inboxTitle), state, ready, verdict, checksOf(p), c.ago(p.CheckedAt))
	}
	return out
}
