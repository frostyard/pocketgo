package state

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
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
		if newer(k.touched[key], f) {
			if i := k.index(key, id); i >= 0 {
				x = k.items[i]
			}
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
// takes its place.
func (k *keyed[T]) put(at uint64, x T, id func(T) int64, less func(a, b T) bool) {
	key := id(x)
	if k.inFlight > 0 {
		if k.touched == nil {
			k.touched = map[int64]uint64{}
		}
		k.touched[key] = at
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
func (d *Data) SetThreads(f Fetch, list []proto.Thread) {
	if d.threads.set(f, list, threadID, threadLess) {
		d.rev++
	}
}

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

// ThreadClosed applies the reply of POST /v1/threads/{id}/close, as the
// thread's event would: a closed thread never changes again.
func (d *Data) ThreadClosed(t proto.Thread) { d.applyThread(t) }

func (d *Data) applyThread(t proto.Thread) {
	d.rev++
	d.threads.put(d.touched(), t, threadID, threadLess)
}

// BeginPulls starts a pull requests fetch, counted as BeginThreads is.
func (d *Data) BeginPulls() Fetch { return d.pulls.begin(d) }

// SetPulls takes the answer of GET /v1/pulls, begun as f, by the rule of
// SetThreads.
func (d *Data) SetPulls(f Fetch, list []proto.PullRequest) {
	if d.pulls.set(f, list, pullID, pullLess) {
		d.rev++
	}
}

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
// it is closed; else the owner while one of its asks is open or one of its
// pull requests waits for review (ToReview); else its specialist while it
// starts or is open, and its supervisor once the specialist reported it
// done or blocked. A state this client does not know goes to the
// supervisor, who has the thread's report.
func (d *Data) NextMover(t proto.Thread) Mover {
	if t.State == proto.ThreadClosed {
		return MoverNobody
	}
	if len(d.AsksOf(t.ID)) > 0 {
		return MoverOwner
	}
	for _, p := range d.PullsOf(t.ID) {
		if ToReview(p) {
			return MoverOwner
		}
	}
	switch t.State {
	case proto.ThreadStarting, proto.ThreadOpen:
		return MoverSpecialist
	default:
		return MoverSupervisor
	}
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

// OnTheDesk counts the threads that are not closed.
func (d *Data) OnTheDesk() int {
	n := 0
	for _, t := range d.threads.items {
		if t.State != proto.ThreadClosed {
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
	waiting := 0
	for _, t := range d.threads.items {
		if t.State != proto.ThreadClosed && d.NextMover(t) == MoverOwner {
			waiting++
		}
	}
	asks, _ := d.AsksStatus()
	pulls, _ := d.PullsStatus()
	switch {
	case n > 0 && (asks != ListLoaded || pulls != ListLoaded):
		note += "; the questions or pull requests are not loaded"
	case waiting == 1:
		note += ", 1 waits for you"
	case waiting > 1:
		note += ", " + strconv.Itoa(waiting) + " wait for you"
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
