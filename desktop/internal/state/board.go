package state

import (
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// The threads and their pull requests (ADR-0014, ADR-0016), merged from
// fetches and events by the rule of the asks: an object an event changed
// after a fetch began follows the event, and the answer of a fetch begun
// before one already applied is dropped. Unlike an answered ask, a closed
// thread stays, and so does a merged pull request: each list holds what
// its newest fetch listed, and what events brought since.

// keyed is a list of objects with an id, merged by that rule. Its zero
// value is empty and ready.
type keyed[T any] struct {
	items []T
	// touched marks the objects events changed, by the epoch they came at,
	// while a fetch is in flight (inFlight): with none, no stale copy can
	// land, and no marker is kept.
	touched  map[int64]uint64
	inFlight int
	list     list
}

func (k *keyed[T]) begin(d *Data) Fetch {
	k.inFlight++
	return d.Begin()
}

func (k *keyed[T]) answered() {
	if k.inFlight > 0 {
		k.inFlight--
	}
	if k.inFlight == 0 {
		k.touched = nil
	}
}

// retire drops the marker of each object an event touched before the
// newest fetch that answered: every fetch begun before that one is
// dropped when it answers, so no copy the marker guards against can land.
func (k *keyed[T]) retire() {
	for id, at := range k.touched {
		if at < k.list.applied {
			delete(k.touched, id)
		}
	}
}

// abandon ends a fetch begun and never sent, with no answer: the list,
// its status and the other fetches stay as they are.
func (k *keyed[T]) abandon() { k.answered() }

// fail records that fetch f failed, unless a fetch begun later already
// answered; it reports whether it did.
func (k *keyed[T]) fail(f Fetch, err string) bool {
	defer k.answered()
	if !k.list.answer(f) {
		return false
	}
	k.list.status, k.list.err = ListFailed, err
	k.retire()
	return true
}

// set takes fetch f's list, unless a fetch begun later already answered;
// it reports whether it did. An object an event changed since f began
// keeps the event's copy, listed or not.
func (k *keyed[T]) set(f Fetch, list []T, id func(T) int64, less func(a, b T) bool) bool {
	return k.setKeeping(f, list, id, less, nil)
}

// setKeeping is set, and also keeps an object held over the fetch's copy
// when keep says the held one is newer, nil for none.
func (k *keyed[T]) setKeeping(f Fetch, list []T, id func(T) int64, less func(a, b T) bool, keep func(held, got T) bool) bool {
	defer k.answered()
	if !k.list.answer(f) {
		return false
	}
	k.list.status, k.list.err = ListLoaded, ""
	out := make([]T, 0, len(list))
	seen := map[int64]bool{}
	for _, x := range list {
		key := id(x)
		if seen[key] {
			continue
		}
		seen[key] = true
		if i := k.index(key, id); i >= 0 && (newer(k.touched[key], f) || keep != nil && keep(k.items[i], x)) {
			x = k.items[i]
		}
		out = append(out, x)
	}
	for _, x := range k.items {
		if key := id(x); !seen[key] && newer(k.touched[key], f) {
			out = append(out, x)
		}
	}
	k.items = out
	sort.SliceStable(k.items, func(i, j int) bool { return less(k.items[i], k.items[j]) })
	k.retire()
	return true
}

// put follows an event about x, known at epoch at: x joins the list or
// takes its place. Its marker never moves back: a copy known as of an
// earlier epoch never lets a fetch an event already beat undo it (Codex
// review of #37, round 2).
func (k *keyed[T]) put(at uint64, x T, id func(T) int64, less func(a, b T) bool) {
	key := id(x)
	if k.inFlight > 0 {
		if k.touched == nil {
			k.touched = map[int64]uint64{}
		}
		k.touched[key] = max(k.touched[key], at)
	}
	if i := k.index(key, id); i >= 0 {
		k.items[i] = x
	} else {
		k.items = append(k.items, x)
	}
	sort.SliceStable(k.items, func(i, j int) bool { return less(k.items[i], k.items[j]) })
}

func (k *keyed[T]) index(key int64, id func(T) int64) int {
	return slices.IndexFunc(k.items, func(x T) bool { return id(x) == key })
}

func threadID(t proto.Thread) int64 { return t.ID }

// threadLess orders threads as GET /v1/threads does: most recently changed
// first.
func threadLess(a, b proto.Thread) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.ID > b.ID
}

func pullID(p proto.PullRequest) int64 { return p.ID }

// pullLess orders pull requests as GET /v1/pulls does: oldest first.
func pullLess(a, b proto.PullRequest) bool { return a.ID < b.ID }

// BeginThreads starts a threads fetch: Begin, counted, so the threads'
// merge markers live only while a fetch is in flight.
func (d *Data) BeginThreads() Fetch { return d.threads.begin(d) }

// SetThreads takes the answer of GET /v1/threads, begun as f, by the rule
// of the asks: a thread an event changed since f began follows the event.
// A thread held with a later updatedAt than f's copy stays too: pocketd
// moves a thread's updatedAt with each change, never back, so f read it
// before that change, as a fetch begun while a shift's stop was on its way
// may have (Codex review of #37, round 2).
func (d *Data) SetThreads(f Fetch, list []proto.Thread) {
	if d.threads.setKeeping(f, list, threadID, threadLess, threadNewer) {
		d.rev++
	}
}

// threadNewer says whether held is a later copy of its thread than got.
func threadNewer(held, got proto.Thread) bool { return held.UpdatedAt.After(got.UpdatedAt) }

// SetThreadsError records that the threads' fetch f failed, unless a fetch
// begun later already answered; the threads stay.
func (d *Data) SetThreadsError(f Fetch, err string) {
	if d.threads.fail(f, err) {
		d.rev++
	}
}

// ThreadsStatus is where the threads stand, and why their last fetch
// failed.
func (d *Data) ThreadsStatus() (ListStatus, string) {
	return d.threads.list.status, d.threads.list.err
}

// Threads are every thread, closed ones too, most recently changed first.
// The slice is the Data's own: read it, do not change it.
func (d *Data) Threads() []proto.Thread { return d.threads.items }

// Thread returns the thread with id.
func (d *Data) Thread(id int64) (proto.Thread, bool) {
	if i := d.threads.index(id, threadID); i >= 0 {
		return d.threads.items[i], true
	}
	return proto.Thread{}, false
}

// ThreadClosed applies the reply of POST /v1/threads/{id}/close over a
// copy that is not closed, whatever its updatedAt: a thread leaves closed
// only by a reopen, which counts in reopens, so a copy of the same reopens
// that is not closed is older, though pocketd's clock may give the close
// the same updatedAt or an earlier one (Codex review of #117). A copy with
// more reopens is of a later reopen than the close, and stays (ADR-0061;
// Codex review of #121, round 1). Over a closed copy it applies only when
// later, as ThreadChanged does: a closed thread still changes (pocketd
// notes on its report), and the event of such a change may come before
// the reply.
//
// A reply that is not closed is a close run again after a reopen, which
// answers with the thread as it is: a snapshot, which applies only over an
// older copy, as ThreadChanged's does (Codex review of #121, round 2).
func (d *Data) ThreadClosed(t proto.Thread) {
	if cur, ok := d.Thread(t.ID); ok {
		if cur.Reopens > t.Reopens {
			return
		}
		if (cur.State == proto.ThreadClosed || t.State != proto.ThreadClosed) && !t.UpdatedAt.After(cur.UpdatedAt) {
			return
		}
	}
	d.putReply(d.touched(), t)
}

// BeginThreadChange starts an owner's request that answers with the thread
// it changed (POST /v1/threads/{id}/shift/stop or .../resume, ADR-0024):
// its reply is the thread as it was when pocketd answered, and the thread
// goes on changing (a resumed shift's next round).
func (d *Data) BeginThreadChange() Fetch { return d.threads.begin(d) }

// ThreadChanged applies the reply of a change begun as f, unless the copy
// the client holds is newer. pocketd moves a thread's updatedAt with each
// change of it or its shift, never back (protocol rule 55), so the later
// updatedAt is the newer copy, wherever it came from: an event, or a fetch
// begun before the change or after it, whose answer came first (Codex
// review of #37). A fetch that failed brought no copy, and keeps none from
// the reply. A held copy as old as the reply that differs from it cannot
// be ordered: it stays, and refetch says to fetch the threads again.
//
// The reply applied is marked as of f, never as of its arrival, so a fetch
// begun before f never undoes it, and one begun after f wins over it
// unless its copy is older (SetThreads). One still in flight may have read
// the thread after a later change, or as old as the reply: refetch asks
// for a fetch, begun once the change committed, which settles it.
func (d *Data) ThreadChanged(f Fetch, t proto.Thread) (refetch bool) {
	defer d.threads.answered()
	if cur, ok := d.Thread(t.ID); ok && !t.UpdatedAt.After(cur.UpdatedAt) {
		return t.UpdatedAt.Equal(cur.UpdatedAt) && !reflect.DeepEqual(cur, t)
	}
	d.putReply(f.epoch, t)
	return d.threads.inFlight > 1
}

// ThreadChangeFailed ends a change begun as f that got no reply.
func (d *Data) ThreadChangeFailed(Fetch) { d.threads.answered() }

// putReply puts t, the reply of an owner's change, known at epoch at, and
// notes its updatedAt until the change's own event comes (applyThread).
func (d *Data) putReply(at uint64, t proto.Thread) {
	d.rev++
	d.threads.put(at, t, threadID, threadLess)
	if d.threadReplied == nil {
		d.threadReplied = map[int64]time.Time{}
	}
	d.threadReplied[t.ID] = t.UpdatedAt
}

// applyThread follows a thread's event. Events come in the order pocketd
// made their changes, but an owner's change answers with the thread too,
// and that reply may come before the events of the changes ahead of it,
// the stream lagging behind. While the client holds the reply's copy, an
// event older than it is dropped, where the thread showed the older copy
// until the change's own event came (pocketgo#42). That event comes,
// pocketd sending one with each change, and ends the drop, unless a
// resync lost it: a resync ends every drop (Apply).
//
// Every other event applies, the stream's order making it the newest: one
// as old as the reply, since a change in the reply's millisecond may have
// come after it; one with no updatedAt, which orders nothing; and one
// older than a fetch's copy, by the Data merge rule (an event that came
// after a fetch began wins over its copy). A fetch may hold a change whose
// event this stream never carries, so no event would follow to end a drop
// where pocketd's clock moved back.
func (d *Data) applyThread(t proto.Thread) {
	if at, ok := d.threadReplied[t.ID]; ok {
		if cur, _ := d.Thread(t.ID); cur.UpdatedAt.Equal(at) && at.After(t.UpdatedAt) && !t.UpdatedAt.IsZero() {
			return
		}
		delete(d.threadReplied, t.ID)
	}
	d.rev++
	d.threads.put(d.touched(), t, threadID, threadLess)
}

// BeginPulls starts a pull requests fetch, counted as BeginThreads is.
func (d *Data) BeginPulls() Fetch { return d.pulls.begin(d) }

// SetPulls takes the answer of GET /v1/pulls, begun as f, by the rule of
// SetThreads. It says whether it took the answer: false when a fetch
// begun later answered first.
func (d *Data) SetPulls(f Fetch, list []proto.PullRequest) bool {
	if d.pulls.set(f, list, pullID, pullLess) {
		d.rev++
		return true
	}
	return false
}

// AbandonPulls ends pull requests fetch f, begun and never sent: the list,
// its status and the other fetches stay as they are (Codex review of #26,
// round 11).
func (d *Data) AbandonPulls(Fetch) { d.pulls.abandon() }

// SetPullsError records that the pull requests' fetch f failed, unless a
// fetch begun later already answered.
func (d *Data) SetPullsError(f Fetch, err string) {
	if d.pulls.fail(f, err) {
		d.rev++
	}
}

// PullsStatus is where the pull requests stand, and why their last fetch
// failed. The Inbox says no pull request waits only when it is ListLoaded.
func (d *Data) PullsStatus() (ListStatus, string) {
	return d.pulls.list.status, d.pulls.list.err
}

// Pulls are the linked pull requests the client shows, oldest first: as
// GET /v1/pulls lists them, the open ones and those of the threads not
// closed (pullShows), whatever order their events came in.
func (d *Data) Pulls() []proto.PullRequest {
	var out []proto.PullRequest
	for _, p := range d.pulls.items {
		if d.pullShows(p) {
			out = append(out, p)
		}
	}
	return out
}

// PullsOf are thread's pull requests that show (Pulls), oldest first.
func (d *Data) PullsOf(thread int64) []proto.PullRequest {
	var out []proto.PullRequest
	for _, p := range d.pulls.items {
		if p.Thread == thread && d.pullShows(p) {
			out = append(out, p)
		}
	}
	return out
}

// pullShows is protocol rule 40's listing: p is open, or its thread is
// not closed as far as the client knows. A merged pull request whose
// thread then closed leaves at that thread's event, as the next fetch
// would leave it out.
func (d *Data) pullShows(p proto.PullRequest) bool {
	if p.State == proto.PullOpen {
		return true
	}
	t, ok := d.Thread(p.Thread)
	return !ok || t.State != proto.ThreadClosed
}

// PullsToReview are the pull requests that wait for the owner's review,
// oldest first: open, and no draft. A closed thread's open pull request
// waits too: it is still open on GitHub.
func (d *Data) PullsToReview() []proto.PullRequest {
	var out []proto.PullRequest
	for _, p := range d.pulls.items {
		if ToReview(p) {
			out = append(out, p)
		}
	}
	return out
}

// ToReview says whether p waits for the owner's review: open, and no draft.
func ToReview(p proto.PullRequest) bool { return p.State == proto.PullOpen && !p.Draft }

func (d *Data) applyPull(p proto.PullRequest) {
	d.rev++
	d.pulls.put(d.touched(), p, pullID, pullLess)
}

// AsksOf are thread's open asks, oldest first.
func (d *Data) AsksOf(thread int64) []proto.Ask {
	var out []proto.Ask
	for _, a := range d.asks {
		if a.Thread == thread {
			out = append(out, a)
		}
	}
	return out
}

// Mover is who moves a thread next: the work board groups threads by it.
type Mover int

const (
	// MoverOwner: a question of the thread waits for the owner's answer,
	// or one of its pull requests for the owner's review.
	MoverOwner Mover = iota
	// MoverSpecialist: the specialist works on it, or pocketd sets it up.
	MoverSpecialist
	// MoverSupervisor: the specialist reported it done or blocked, and its
	// supervisor reads the report, then closes it or briefs again.
	MoverSupervisor
	// MoverNobody: the thread is closed.
	MoverNobody
)

// Movers are the movers in the board's order.
var Movers = []Mover{MoverOwner, MoverSpecialist, MoverSupervisor, MoverNobody}

// NextMover is who moves t next, from what the client holds: nobody once
// it is closed; else the owner while one of its asks is open, one of its
// pull requests waits for review (ToReview), or the plan it plans waits
// for the owner's approval (ToApprove). A plan's own thread whose plan is
// approved is the owner's while its gate waits (an open ask, as the plan
// says, also before the ask itself loaded), its supervisor's while a task
// of it is ready to deliver (ReadyTasks), and its tasks' specialists'
// otherwise. Else a thread whose shift is paused is the owner's, and one
// whose shift runs or asks for its summary is its specialist's (ADR-0024).
// Else a thread is its specialist's while it starts or is open, and its
// supervisor's once the specialist reported it done or blocked. A state this client does
// not know goes to the supervisor, who has the thread's report.
func (d *Data) NextMover(t proto.Thread) Mover {
	if t.State == proto.ThreadClosed {
		return MoverNobody
	}
	if len(d.AsksOf(t.ID)) > 0 {
		return MoverOwner
	}
	if p, ok := d.Plan(t.Plan); ok && PlanThread(t) {
		switch {
		case ToApprove(p), len(OpenAsksOf(p)) > 0:
			// An open ask of the plan waits for the owner's answer, as the
			// plan's gate says, also before the ask itself loaded (Codex
			// review of #25, rounds 2 and 3).
			return MoverOwner
		case p.State == proto.PlanDrafting && len(p.Waiting) > 0:
			// An answer to write in, or a consult that runs: the lead's,
			// whatever its thread's last report said (Codex review of #25,
			// round 4).
			return MoverSpecialist
		case p.State == proto.PlanApproved || p.State == proto.PlanBuilding:
			if len(ReadyTasks(p)) > 0 {
				return MoverSupervisor
			}
			// No task to deliver: the plan moves with its tasks' threads,
			// the owner's first, then the supervisor's (Codex review of
			// #25, round 6).
			return d.taskMover(p)
		}
	}
	for _, p := range d.PullsOf(t.ID) {
		if ToReview(p) {
			return MoverOwner
		}
	}
	if sh := t.Shift; sh != nil {
		switch sh.State {
		case proto.ShiftPaused:
			// The owner's message paused it: no round goes until the owner
			// resumes or stops it (Codex review of #37).
			return MoverOwner
		case proto.ShiftRunning, proto.ShiftSummary:
			// pocketd sends the specialist its rounds, whatever its
			// thread's last report said: a stop files a blocked report,
			// which a resume leaves (Codex review of #37).
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
// supervisor when one waits for it, else the tasks' specialists.
func (d *Data) taskMover(p proto.Plan) Mover {
	best := MoverSpecialist
	for _, t := range TaskThreads(p) {
		th, ok := d.Thread(t)
		if !ok {
			continue
		}
		switch m := d.NextMover(th); m {
		case MoverOwner:
			return MoverOwner
		case MoverSupervisor:
			best = MoverSupervisor
		}
	}
	return best
}

// TaskThreadsMovedBy are the threads of p's delivered tasks that m moves
// next, in graph order: those a plan's card names for its group (Codex
// review of #25, round 7).
func (d *Data) TaskThreadsMovedBy(p proto.Plan, m Mover) []int64 {
	var out []int64
	for _, id := range TaskThreads(p) {
		if th, ok := d.Thread(id); ok && d.NextMover(th) == m {
			out = append(out, id)
		}
	}
	return out
}

// TaskThreads are the threads of p's delivered tasks, in graph order.
func TaskThreads(p proto.Plan) []int64 {
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

// BoardGroup is the threads one mover moves next, most recently changed
// first.
type BoardGroup struct {
	Mover   Mover
	Threads []proto.Thread
}

// Board is the work board: the threads grouped by NextMover, one group per
// mover in the order of Movers, empty ones included.
func (d *Data) Board() []BoardGroup {
	groups := make([]BoardGroup, len(Movers))
	for i, m := range Movers {
		groups[i].Mover = m
	}
	for _, t := range d.threads.items {
		m := d.NextMover(t)
		groups[m].Threads = append(groups[m].Threads, t)
	}
	return groups
}

// OnTheDesk counts the threads on the desk, as pocketd's cap counts
// them: every one not closed, but a plan's own thread (PlanThread).
func (d *Data) OnTheDesk() int {
	n := 0
	for _, t := range d.threads.items {
		if t.State != proto.ThreadClosed && !PlanThread(t) {
			n++
		}
	}
	return n
}

// GitHub's owner and repository names, as service/internal/github checks
// them.
var (
	githubOwner = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	githubName  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// PullURL is the link to p on GitHub, built from its repository and
// number: https://github.com/<owner>/<name>/pull/<number>. The client
// never opens the URL pocketd sent, so a pull request opens on GitHub, or
// not at all: ok is false when the repository is no GitHub name or the
// number is not positive.
func PullURL(p proto.PullRequest) (string, bool) {
	owner, name, ok := strings.Cut(p.Repository, "/")
	if !ok || !githubOwner.MatchString(owner) || !githubName.MatchString(name) || name == "." || name == ".." || p.Number <= 0 {
		return "", false
	}
	return "https://github.com/" + owner + "/" + name + "/pull/" + strconv.FormatInt(p.Number, 10), true
}

// PullRef is p as a report names it: owner/name#number.
func PullRef(p proto.PullRequest) string {
	return p.Repository + "#" + strconv.FormatInt(p.Number, 10)
}

// BoardNote is what the work board's title says: how many threads are on
// the desk, and how many of them wait for the owner. It claims the desk's
// count only when the threads loaded, and how many wait only when the asks
// and the pull requests loaded too: from a list that failed or is still
// loading, a count would be stale or short.
func (d *Data) BoardNote() string {
	if st, _ := d.ThreadsStatus(); st != ListLoaded {
		return "The threads are not loaded"
	}
	n := d.OnTheDesk()
	note := "No thread is on the desk"
	switch {
	case n == 1:
		note = "1 thread on the desk"
	case n > 1:
		note = strconv.Itoa(n) + " threads on the desk"
	}
	// The threads on the desk that wait for the owner, and apart from
	// them the plans that wait for the owner's approval: a plan's own
	// thread is no desk thread (Codex review of #25, round 1).
	waiting, planThreads := 0, 0
	for _, t := range d.threads.items {
		switch {
		case t.State == proto.ThreadClosed:
		case PlanThread(t):
			planThreads++
		case d.NextMover(t) == MoverOwner:
			waiting++
		}
	}
	asks, _ := d.AsksStatus()
	pulls, _ := d.PullsStatus()
	switch {
	case n > 0 && (asks != ListLoaded || pulls != ListLoaded):
		note += "; the asks or pull requests are not loaded"
	case waiting == 1:
		note += ", 1 waits for you"
	case waiting > 1:
		note += ", " + strconv.Itoa(waiting) + " wait for you"
	}
	plans, _ := d.PlansStatus()
	switch approve := len(d.PlansToApprove()); {
	case planThreads > 0 && plans != ListLoaded:
		note += "; the plans are not loaded"
	case approve == 1:
		note += "; 1 plan waits for your approval"
	case approve > 1:
		note += "; " + strconv.Itoa(approve) + " plans wait for your approval"
	}
	return note
}

// ReportFolds says whether a thread's report folds on the board to its
// first four lines, with a way to unfold it: more than 280 characters, or
// more than four lines. A shorter one always shows whole.
func ReportFolds(report string) bool {
	return utf8.RuneCountInString(report) > 280 || strings.Count(report, "\n") >= 4
}

// JoinAnd joins parts as a list: "a", "a and b", "a, b and c".
func JoinAnd(parts []string) string { return joinAnd(parts) }

// ReadyBacked says whether pocketd's last observations of p back its
// ready-for-human (ADR-0017): the newest verdict at its head said ready,
// and CI was green when pocketd last read it there. Both are past reads,
// not proof of now: the service decides the label.
func ReadyBacked(p proto.PullRequest) bool {
	r := p.Review
	return r != nil && r.Verdict == proto.ReviewReady && r.Head == p.HeadSHA && p.Checks == proto.ChecksGreen
}
