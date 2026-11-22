package state

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Data is what the client knows of one pocketd: its sessions, the pending
// approvals, the open asks, the lessons, the threads and their pull
// requests, and the open session, merged from fetches and events by the
// rules on its methods. It holds no network code and belongs to one
// goroutine, the UI thread; the zero value is empty and ready.
//
// The merge rule: every change reaches the client as an event, while a fetch
// answers with a copy made at some moment while it was in flight. So an
// event about an object that arrives after a fetch began wins over that
// fetch's copy of the object (Fetch). Without it, a fetch answered late
// could put back a state the events already moved past, such as a busy
// live state after the run ended. For the same reason, the answer of a
// fetch begun before one already applied is dropped: per list, and per
// session summary, which both the list and a session's detail carry.
type Data struct {
	// profiles are the agents' profiles (GET /v1/profiles).
	profiles     []proto.Profile
	profilesList list
	// models are the models pocketd runs (GET /v1/models), which New
	// Session and the composer's chip offer.
	models     []proto.Model
	modelsList list
	// prompts are the owner's saved prompts (GET /v1/prompts), which the
	// composer's / picker offers (prompts.go, ADR-0051).
	prompts     proto.Prompts
	promptsList list
	sessions    []proto.Session  // most recently updated first
	approvals   []proto.Approval // pending only, oldest first
	asks        []proto.Ask      // open only, oldest first
	lessons     lessons          // waiting only
	// threads are every thread, most recently changed first, and pulls
	// the linked pull requests, oldest first (board.go).
	threads keyed[proto.Thread]
	pulls   keyed[proto.PullRequest]
	// threadReplied is the updatedAt of each thread's copy an owner's
	// change answered with (ThreadChanged, ThreadClosed), until an event
	// of the thread applies: an older event is dropped while the copy is
	// held (applyThread).
	threadReplied map[int64]time.Time
	// plans are every plan, most recently changed first (plans.go).
	plans keyed[proto.Plan]
	// proposals are the board's proposals, in board order, and schedules
	// the owner's and the supervisors' schedules, fetched whole
	// (nightshift.go).
	proposals keyed[proto.Proposal]
	// droppedProposals are the proposals seen dropped: a drop is final.
	droppedProposals map[int64]bool
	schedules        proto.Schedules
	schedulesList    list
	// usage is the newest usage report (GET /v1/usage, ADR-0052), fetched
	// whole (usage.go); usageOwed are the sessions an answer came to whose
	// session event has not come yet (usageChanged).
	usage     proto.UsageReport
	usageList list
	usageOwed map[int64]bool
	// usageMoved says a session's model changed, from whatever brought it:
	// the totals count an answer that names no model under its session's
	// model now (takeUsageMoved).
	usageMoved bool
	// search is the owner's newest search of every session (ADR-0050).
	search searchState

	open *OpenSession
	// changes are the sessions' changes the client keeps, by session, and
	// diff the file whose diff the Changes view shows (changes.go,
	// ADR-0053).
	changes map[int64]*sessionChanges
	diff    *ShownDiff
	// terminals are the owner's shells, oldest first, replaced whole by
	// each terminals event (terminal.go); terminalsTouched is the last
	// event's epoch.
	terminals        []proto.Terminal
	terminalsList    list
	terminalsTouched uint64
	// terminalsAwaited are the shells the owner started that no list has
	// brought yet, with the epoch of their start.
	terminalsAwaited map[string]uint64
	// signIns are pocketd's sign-ins (signins.go, ADR-0062).
	signIns signInData

	epoch           uint64
	sessionTouched  map[int64]uint64
	approvalTouched map[string]uint64
	askTouched      map[int64]uint64
	// asksInFlight counts the asks fetches begun (BeginAsks) and not
	// answered: with none, no stale copy can land, and no marker is kept.
	asksInFlight int
	// sessionApplied is the token of the newest fetch whose copy of a
	// session's summary was applied.
	sessionApplied map[int64]uint64
	// Where the lists' fetches stand.
	sessionsList, approvalsList, asksList list
	rev                                   uint64
}

// ListStatus is where a list the client fetches stands.
type ListStatus int

const (
	// ListLoading: no fetch of the list answered yet.
	ListLoading ListStatus = iota
	// ListLoaded: the newest fetch that answered succeeded.
	ListLoaded
	// ListFailed: the newest fetch that answered failed; what the list
	// holds may be stale or missing.
	ListFailed
)

// list is where one list's fetches stand: the newest one that answered
// (applied), and whether it failed.
type list struct {
	status  ListStatus
	err     string
	applied uint64
}

// answer reports whether the answer of fetch f is newer than every answer
// applied, and records it as applied.
func (l *list) answer(f Fetch) bool {
	if f.epoch < l.applied {
		return false
	}
	l.applied = f.epoch
	return true
}

// Profiles are the agents' profiles of the newest answer, nil before one
// came. The slice is the Data's own: read it, do not change it.
func (d *Data) Profiles() []proto.Profile { return d.profiles }

// ProfilesStatus is where the profile list stands, and why its last fetch
// failed; a failed fetch keeps the profiles an earlier one brought.
func (d *Data) ProfilesStatus() (ListStatus, string) {
	return d.profilesList.status, d.profilesList.err
}

// SetProfiles records fetch f's profiles, unless a fetch begun later
// already answered.
func (d *Data) SetProfiles(f Fetch, profiles []proto.Profile) {
	if !d.profilesList.answer(f) {
		return
	}
	d.rev++
	d.profilesList.status, d.profilesList.err = ListLoaded, ""
	if profiles == nil {
		profiles = []proto.Profile{}
	}
	d.profiles = slices.Clone(profiles)
}

// SetProfilesError records that the profiles' fetch f failed, unless a
// fetch begun later already answered; the profiles stay.
func (d *Data) SetProfilesError(f Fetch, err string) {
	if d.profilesList.answer(f) {
		d.rev++
		d.profilesList.status, d.profilesList.err = ListFailed, err
	}
}

// Models are the models pocketd runs, of the newest answer, nil before one
// came, and why the last fetch failed, "" when it did not: a failed fetch
// keeps the models an earlier one brought. The slice is the Data's own.
func (d *Data) Models() ([]proto.Model, string) { return d.models, d.modelsList.err }

// SetModels records fetch f's models, unless a fetch begun later already
// answered: a stream that came back to a restarted pocketd gets the
// models it runs now, never an older answer (Codex review of #48, rounds
// 3 and 4).
func (d *Data) SetModels(f Fetch, models []proto.Model) {
	if !d.modelsList.answer(f) {
		return
	}
	d.rev++
	d.modelsList.status, d.modelsList.err = ListLoaded, ""
	if models == nil {
		models = []proto.Model{}
	}
	d.models = slices.Clone(models)
}

// SetModelsError records that the models' fetch f failed, unless a fetch
// begun later already answered; the models stay.
func (d *Data) SetModelsError(f Fetch, err string) {
	if d.modelsList.answer(f) {
		d.rev++
		d.modelsList.status, d.modelsList.err = ListFailed, err
	}
}

// SessionsStatus is where the session list stands, and why its last fetch
// failed.
func (d *Data) SessionsStatus() (ListStatus, string) {
	return d.sessionsList.status, d.sessionsList.err
}

// ApprovalsStatus is where the pending approvals stand, and why their last
// fetch failed. Until a fetch succeeded, and after one failed, the client
// does not know that nothing waits: the Inbox says so only when it is
// ListLoaded.
func (d *Data) ApprovalsStatus() (ListStatus, string) {
	return d.approvalsList.status, d.approvalsList.err
}

// AsksStatus is where the open asks stand, and why their last fetch
// failed. As with the approvals, the Inbox says no question waits only
// when it is ListLoaded.
func (d *Data) AsksStatus() (ListStatus, string) {
	return d.asksList.status, d.asksList.err
}

// SetAsksError records that the asks' fetch f failed, unless a fetch begun
// later already answered.
func (d *Data) SetAsksError(f Fetch, err string) {
	defer d.askAnswered()
	if d.asksList.answer(f) {
		d.rev++
		d.asksList.status, d.asksList.err = ListFailed, err
		d.retireAskMarks()
	}
}

// BeginAsks starts an asks fetch: Begin, counted, so the asks' merge
// markers live only while a fetch is in flight.
func (d *Data) BeginAsks() Fetch {
	d.asksInFlight++
	return d.Begin()
}

// askAnswered counts an asks fetch answered (or failed).
func (d *Data) askAnswered() {
	if d.asksInFlight > 0 {
		d.asksInFlight--
	}
	if d.asksInFlight == 0 {
		d.askTouched = nil
	}
}

// retireAskMarks drops the merge marker of each ask an event touched
// before the newest asks fetch answered: every fetch begun before that
// one is dropped when it answers (list.answer), so no stale copy the
// marker guards against can land any more. An Inbox open for days keeps
// no marker per question it ever showed.
func (d *Data) retireAskMarks() {
	for id, touched := range d.askTouched {
		if touched < d.asksList.applied {
			delete(d.askTouched, id)
		}
	}
}

// Asks are the open asks of every thread, oldest first: what the Inbox
// shows besides the approvals.
func (d *Data) Asks() []proto.Ask { return d.asks }

// Ask returns the open ask with id.
func (d *Data) Ask(id int64) (proto.Ask, bool) {
	for _, a := range d.asks {
		if a.ID == id {
			return a, true
		}
	}
	return proto.Ask{}, false
}

// SetAsks takes the answer of GET /v1/asks, begun as f, by the rule of
// SetApprovals: an ask an event changed since f began follows the event,
// open it stays, answered it is gone even if the answer still lists it.
func (d *Data) SetAsks(f Fetch, list []proto.Ask) {
	defer d.askAnswered()
	if !d.asksList.answer(f) {
		return
	}
	d.rev++
	d.asksList.status, d.asksList.err = ListLoaded, ""
	out := make([]proto.Ask, 0, len(list))
	seen := map[int64]bool{}
	for _, a := range list {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		if newer(d.askTouched[a.ID], f) {
			cur, ok := d.Ask(a.ID)
			if !ok {
				continue // answered since
			}
			a = cur
		}
		if a.State != "" && a.State != proto.AskOpen {
			continue
		}
		out = append(out, a)
	}
	for _, a := range d.asks {
		if !seen[a.ID] && newer(d.askTouched[a.ID], f) {
			out = append(out, a)
		}
	}
	d.asks = out
	d.sortAsks()
	d.retireAskMarks()
}

// Answered applies the answer of POST /v1/asks/{id}, as the ask's event
// would.
func (d *Data) Answered(a proto.Ask) { d.applyAsk(a) }

// applyAsk follows an ask: an open one joins the list, an answered one
// leaves it.
func (d *Data) applyAsk(a proto.Ask) {
	d.rev++
	if d.asksInFlight > 0 {
		if d.askTouched == nil {
			d.askTouched = map[int64]uint64{}
		}
		d.askTouched[a.ID] = d.touched()
	}
	i := slices.IndexFunc(d.asks, func(x proto.Ask) bool { return x.ID == a.ID })
	open := a.State == "" || a.State == proto.AskOpen
	switch {
	case open && i >= 0:
		d.asks[i] = a
	case open:
		d.asks = append(d.asks, a)
		d.sortAsks()
	case i >= 0:
		d.asks = slices.Delete(d.asks, i, i+1)
	}
}

// sortAsks orders the list as GET /v1/asks does, oldest first.
func (d *Data) sortAsks() {
	sort.SliceStable(d.asks, func(i, j int) bool {
		a, b := d.asks[i], d.asks[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// SetSessionsError records that the session list's fetch f failed, unless
// a fetch begun later already answered.
func (d *Data) SetSessionsError(f Fetch, err string) {
	if d.sessionsList.answer(f) {
		d.rev++
		d.sessionsList.status, d.sessionsList.err = ListFailed, err
	}
}

// SetApprovalsError records that the approvals' fetch f failed, unless a
// fetch begun later already answered.
func (d *Data) SetApprovalsError(f Fetch, err string) {
	if d.approvalsList.answer(f) {
		d.rev++
		d.approvalsList.status, d.approvalsList.err = ListFailed, err
	}
}

// Rev counts the changes to the Data: a view that derives from it, as a
// transcript's rows, builds again only when Rev moved.
func (d *Data) Rev() uint64 { return d.rev }

// OpenSession is the session the window shows: its transcript and its live
// state.
type OpenSession struct {
	ID int64
	// Loaded is set once its detail arrived; Err is why loading it failed.
	Loaded bool
	Err    string
	// Entries are the transcript, by ascending id.
	Entries []proto.Entry
	// Live is what its run is doing now, and the messages that wait for a
	// run (Live.Queued, protocol rule 16a).
	Live proto.Live

	liveTouched   uint64
	detailApplied uint64
	rev           uint64
}

// Rev counts the changes to what the open session shows: its entries, its
// live state, its approvals and its load state. Its transcript's rows are
// built again only when it moved, not for another session's updates.
func (o *OpenSession) Rev() uint64 { return o.rev }

// Fetch is the token of one fetch in flight: Begin returns it, and the
// fetch's answer goes to a Set method with it.
type Fetch struct{ epoch uint64 }

// Begin starts a fetch. Events that arrive from now on win over its answer.
func (d *Data) Begin() Fetch {
	d.epoch++
	return Fetch{epoch: d.epoch}
}

// newer reports whether an event touched an object after fetch f began.
func newer(touched uint64, f Fetch) bool { return touched >= f.epoch }

// Reset forgets everything, as after signing out.
func (d *Data) Reset() { *d = Data{rev: d.rev + 1} }

// Sessions are the sessions, most recently updated first. The slice is the
// Data's own: read it, do not change it.
func (d *Data) Sessions() []proto.Session { return d.sessions }

// Session returns the session with id.
func (d *Data) Session(id int64) (proto.Session, bool) {
	for _, s := range d.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return proto.Session{}, false
}

// StateOf is the state a session's row shows: the service's state, except
// where the client knows better by protocol rule 11 itself. A session with
// an approval pending in the client's list, or whose question waits for the
// owner (ADR-0048), is waiting, and else the open session is running while
// its live state is busy: the approval's and the live state's events come
// before the session's own, and a refetch reads the lists in separate
// requests. A question's run is busy while it waits: the question wins.
func (d *Data) StateOf(id int64) proto.SessionState {
	s, _ := d.Session(id)
	_, asked := WaitingQuestion(s)
	waiting := asked || slices.ContainsFunc(d.approvals, func(a proto.Approval) bool { return a.SessionID == id })
	return d.stateOf(id, s.State, waiting)
}

// stateOf is StateOf for session id, whose summary says listed ("" for
// none), with an approval pending in the client's list or not.
func (d *Data) stateOf(id int64, listed proto.SessionState, waiting bool) proto.SessionState {
	switch {
	case waiting:
		return proto.SessionWaiting
	case d.open != nil && d.open.ID == id && d.open.Live.Busy:
		return proto.SessionRunning
	case listed != "":
		return listed
	}
	return proto.SessionIdle
}

// Counts are how many sessions wait for the owner and how many run.
func (d *Data) Counts() (waiting, running int) {
	for _, s := range d.sessions {
		switch d.StateOf(s.ID) {
		case proto.SessionWaiting:
			waiting++
		case proto.SessionRunning:
			running++
		}
	}
	return waiting, running
}

// Approvals are the pending approvals of every session, oldest first: what
// the Inbox lists. The slice is the Data's own.
func (d *Data) Approvals() []proto.Approval { return d.approvals }

// ApprovalsFor are the pending approvals of one session, oldest first:
// what its view docks above the composer.
func (d *Data) ApprovalsFor(sessionID int64) []proto.Approval {
	var out []proto.Approval
	for _, a := range d.approvals {
		if a.SessionID == sessionID {
			out = append(out, a)
		}
	}
	return out
}

// Approval returns the pending approval with id.
func (d *Data) Approval(id string) (proto.Approval, bool) {
	for _, a := range d.approvals {
		if a.ID == id {
			return a, true
		}
	}
	return proto.Approval{}, false
}

// Open is the open session, or nil.
func (d *Data) Open() *OpenSession { return d.open }

// OpenID is the open session's id, 0 for none.
func (d *Data) OpenID() int64 {
	if d.open == nil {
		return 0
	}
	return d.open.ID
}

// SetOpen opens session id (0 closes it). A session already open keeps
// what it holds; another starts empty, until its detail arrives.
func (d *Data) SetOpen(id int64) {
	d.rev++
	switch {
	case id == 0:
		d.open = nil
	case d.open == nil || d.open.ID != id:
		d.open = &OpenSession{ID: id, Live: proto.Live{SessionID: id}}
	}
}

// SetSessions takes the answer of GET /v1/sessions, begun as f, unless the
// answer of a fetch begun later was applied already. A session's summary
// keeps the copy of an event since f began, or of a fetch begun after f (a
// session's detail), and one those brought that the answer lacks stays.
func (d *Data) SetSessions(f Fetch, list []proto.Session) {
	if !d.sessionsList.answer(f) {
		return
	}
	d.rev++
	d.sessionsList.status, d.sessionsList.err = ListLoaded, ""
	out := make([]proto.Session, 0, len(list))
	seen := map[int64]bool{}
	for _, s := range list {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		if cur, ok := d.Session(s.ID); ok && !d.takesSummary(s.ID, f) {
			s = cur
		} else {
			if ok && cur.Model != s.Model {
				d.usageMoved = true
			}
			d.appliedSummary(s.ID, f)
		}
		out = append(out, s)
	}
	for _, s := range d.sessions {
		if !seen[s.ID] && !d.takesSummary(s.ID, f) {
			out = append(out, s)
		}
	}
	if o := d.open; o != nil {
		// The open session's view shows its question (ADR-0048).
		was, _ := d.Session(o.ID)
		var now *proto.Question
		if i := slices.IndexFunc(out, func(s proto.Session) bool { return s.ID == o.ID }); i >= 0 {
			now = out[i].Question
		}
		if !sameQuestion(was.Question, now) {
			o.rev++
		}
	}
	d.sessions = out
	d.sortSessions()
}

// takesSummary reports whether the copy of session id that fetch f
// brought replaces the one held: no event changed it since f began, and no
// fetch begun after f brought a copy already.
func (d *Data) takesSummary(id int64, f Fetch) bool {
	return !newer(d.sessionTouched[id], f) && f.epoch >= d.sessionApplied[id]
}

func (d *Data) appliedSummary(id int64, f Fetch) {
	if d.sessionApplied == nil {
		d.sessionApplied = map[int64]uint64{}
	}
	d.sessionApplied[id] = f.epoch
}

// SetApprovals takes the answer of GET /v1/approvals, begun as f, unless
// the answer of a fetch begun later was applied already. An
// approval an event changed since f began follows the event: pending, it
// stays; decided, it is gone even if the answer still lists it.
func (d *Data) SetApprovals(f Fetch, list []proto.Approval) {
	if !d.approvalsList.answer(f) {
		return
	}
	d.rev++
	d.approvalsList.status, d.approvalsList.err = ListLoaded, ""
	out := make([]proto.Approval, 0, len(list))
	seen := map[string]bool{}
	for _, a := range list {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		if newer(d.approvalTouched[a.ID], f) {
			cur, ok := d.Approval(a.ID)
			if !ok {
				continue // decided since
			}
			a = cur
		}
		if a.State != "" && a.State != proto.ApprovalPending {
			continue
		}
		out = append(out, a)
	}
	for _, a := range d.approvals {
		if !seen[a.ID] && newer(d.approvalTouched[a.ID], f) {
			out = append(out, a)
		}
	}
	if d.open != nil {
		d.open.rev++ // its approvals may have changed
	}
	d.approvals = out
	d.sortApprovals()
}

// SetDetail takes the answer of GET /v1/sessions/{id}, begun as f. Its
// session updates the list by the rule of SetSessions (takesSummary). When
// that session is still open and no detail begun later was applied, its
// entries merge by id with those events brought meanwhile, and its live
// state is the detail's unless a live event came since f began.
func (d *Data) SetDetail(f Fetch, detail proto.SessionDetail) {
	d.rev++
	id := detail.Session.ID
	if _, ok := d.Session(id); !ok || d.takesSummary(id, f) {
		d.appliedSummary(id, f)
		d.putSession(detail.Session)
	}
	o := d.open
	if o == nil || o.ID != id || f.epoch < o.detailApplied {
		return
	}
	o.detailApplied = f.epoch
	o.rev++
	o.Loaded, o.Err = true, ""
	for _, e := range detail.Entries {
		o.addEntry(e)
	}
	if !newer(o.liveTouched, f) {
		o.Live = detail.Live
		o.Live.SessionID = o.ID
	}
}

// SetDetailError records why the detail fetch f of the open session id
// failed, unless a detail fetch begun later answered already; and an older
// detail that answers after it is dropped. A transcript that loaded before
// keeps what it holds, with Err saying it may be stale.
func (d *Data) SetDetailError(f Fetch, id int64, err string) {
	o := d.open
	if o == nil || o.ID != id || f.epoch < o.detailApplied {
		return
	}
	d.rev++
	o.rev++
	o.detailApplied = f.epoch
	o.Err = err
}

// Refetch is what an event asks the client to fetch again.
type Refetch int

const (
	// RefetchNone: the event brought what changed.
	RefetchNone Refetch = iota
	// RefetchAll: the events after the client's last are gone
	// (EventResync), so everything it shows is fetched again.
	RefetchAll
	// RefetchSchedules: the schedules changed (EventSchedules), and the
	// event carries no list (protocol rule 57).
	RefetchSchedules
	// RefetchUsage: a session's usage changed (EventSession), so the
	// day's totals did (ADR-0052).
	RefetchUsage
	// RefetchModels: a provider signed in or out (EventSignIn), so the
	// models pocketd runs changed (ADR-0062).
	RefetchModels
)

// Apply applies one event of the stream, and reports what it asks the
// client to fetch again: everything after EventResync, the schedules after
// EventSchedules, the usage totals after a session event that changes what
// the session used. An event of a type this client does not know changes
// nothing.
func (d *Data) Apply(ev proto.Event) Refetch {
	d.rev++
	switch ev.Type {
	case proto.EventResync:
		// The events a reply's copy waits for may be among those lost, so
		// no event is dropped for a reply held before (applyThread; Codex
		// review of #117).
		d.threadReplied = nil
		return RefetchAll
	case proto.EventSchedules:
		return RefetchSchedules
	case proto.EventProposal:
		if ev.Proposal != nil {
			d.applyProposal(*ev.Proposal)
		}
	case proto.EventSession:
		if ev.Session != nil {
			changed := d.usageChanged(*ev.Session)
			d.touchSession(ev.Session.ID)
			d.putSession(*ev.Session)
			if changed {
				return RefetchUsage
			}
		}
	case proto.EventEntry:
		if ev.Entry != nil {
			d.oweUsage(*ev.Entry)
			d.applyEntry(*ev.Entry)
		}
	case proto.EventLive:
		if ev.Live != nil && d.open != nil && ev.Live.SessionID == d.open.ID {
			d.open.Live = *ev.Live
			d.open.liveTouched = d.touched()
			d.open.rev++
		}
	case proto.EventApproval:
		if ev.Approval != nil {
			d.applyApproval(*ev.Approval)
		}
	case proto.EventAsk:
		if ev.Ask != nil {
			d.applyAsk(*ev.Ask)
		}
	case proto.EventLesson:
		if ev.Lesson != nil {
			d.applyLesson(*ev.Lesson)
		}
	case proto.EventThread:
		if ev.Thread != nil {
			d.applyThread(*ev.Thread)
		}
	case proto.EventPull:
		if ev.Pull != nil {
			d.applyPull(*ev.Pull)
		}
	case proto.EventPlan:
		if ev.Plan != nil {
			d.applyPlan(*ev.Plan)
		}
	case proto.EventTerminals:
		d.applyTerminals(ev.Terminals)
	case proto.EventSignIn:
		if ev.SignIn != nil && d.applySignIn(*ev.SignIn) {
			return RefetchModels
		}
	}
	return RefetchNone
}

// touched is the epoch an event records: the token of the newest fetch
// begun, so the event wins over the answers of the fetches in flight and of
// none begun after it. An event before any fetch records 0, older than every
// token.
func (d *Data) touched() uint64 { return d.epoch }

func (d *Data) touchSession(id int64) {
	if d.sessionTouched == nil {
		d.sessionTouched = map[int64]uint64{}
	}
	d.sessionTouched[id] = d.touched()
}

// applyEntry adds a written entry to the open session's transcript when it
// is that session's. The session's place in the list moves with the
// session event that follows the entry (protocol rule 16b).
func (d *Data) applyEntry(e proto.Entry) {
	o := d.open
	if o == nil || o.ID != e.SessionID {
		return
	}
	o.addEntry(e)
	o.rev++
	// The reply being written became this entry: its live event follows,
	// and until then the reply would show twice.
	if e.Kind == proto.EntryAssistant {
		o.Live.Partial = nil
	}
}

// applyApproval follows an approval: a pending one joins the list, a
// decided one leaves it.
func (d *Data) applyApproval(a proto.Approval) {
	d.rev++
	if d.approvalTouched == nil {
		d.approvalTouched = map[string]uint64{}
	}
	d.approvalTouched[a.ID] = d.touched()
	if d.open != nil && d.open.ID == a.SessionID {
		d.open.rev++
	}
	i := slices.IndexFunc(d.approvals, func(x proto.Approval) bool { return x.ID == a.ID })
	pending := a.State == "" || a.State == proto.ApprovalPending
	switch {
	case pending && i >= 0:
		d.approvals[i] = a
	case pending:
		d.approvals = append(d.approvals, a)
		d.sortApprovals()
	case i >= 0:
		d.approvals = slices.Delete(d.approvals, i, i+1)
	}
}

// Decided applies the answer of POST /v1/approvals/{id}, as the event of
// the decision would.
func (d *Data) Decided(a proto.Approval) { d.applyApproval(a) }

// Created applies the answer of POST /v1/sessions, as the session's event
// would.
//
// The answer may name a session the client knows already, as New Session
// on a standing profile opens its one session (ADR-0026): an event that
// changed it since f began, the create's own fetch token, wins over the
// answer's copy (Codex review of #43, round 5).
func (d *Data) Created(f Fetch, s proto.Session) {
	d.rev++
	if _, ok := d.Session(s.ID); ok && !d.takesSummary(s.ID, f) {
		return
	}
	d.touchSession(s.ID)
	d.putSession(s)
}

func (d *Data) putSession(s proto.Session) {
	if i := slices.IndexFunc(d.sessions, func(x proto.Session) bool { return x.ID == s.ID }); i >= 0 {
		// The open session's view shows its question: a change of it
		// builds the view's rows again (ADR-0048).
		if d.open != nil && d.open.ID == s.ID && !sameQuestion(d.sessions[i].Question, s.Question) {
			d.open.rev++
		}
		if d.sessions[i].Model != s.Model {
			d.usageMoved = true
		}
		d.sessions[i] = s
	} else {
		d.sessions = append(d.sessions, s)
	}
	d.sortSessions()
}

// sortSessions orders the list as GET /v1/sessions does, most recently
// updated first, and the newer session first on a tie.
func (d *Data) sortSessions() {
	sort.SliceStable(d.sessions, func(i, j int) bool {
		a, b := d.sessions[i], d.sessions[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID > b.ID
	})
}

// sortApprovals orders the list as GET /v1/approvals does, oldest first.
func (d *Data) sortApprovals() {
	sort.SliceStable(d.approvals, func(i, j int) bool {
		return d.approvals[i].RequestedAt.Before(d.approvals[j].RequestedAt)
	})
}

// addEntry puts e in the transcript by its id; an id already there keeps
// its entry, since a written entry does not change.
func (o *OpenSession) addEntry(e proto.Entry) {
	n := len(o.Entries)
	if n == 0 || e.ID > o.Entries[n-1].ID {
		o.Entries = append(o.Entries, e)
		return
	}
	i, found := slices.BinarySearchFunc(o.Entries, e.ID, func(x proto.Entry, id int64) int {
		switch {
		case x.ID < id:
			return -1
		case x.ID > id:
			return 1
		}
		return 0
	})
	if found {
		return
	}
	o.Entries = slices.Insert(o.Entries, i, e)
}

// InboxKind is one kind of thing the Inbox lists: how many wait, and
// whether its list loaded.
type InboxKind struct {
	// List names the list ("asks"); One and Many name what it holds.
	List, One, Many string
	Count           int
	Status          ListStatus
}

// Inbox is what the Inbox lists, in its order: the plans that wait for
// the owner's approval, the open asks, the lessons that wait, the pull
// requests that wait for review, the pending approvals, and the sessions
// whose question waits for the owner's answer (QuestionsWaiting).
func (d *Data) Inbox() []InboxKind {
	sessions, _ := d.SessionsStatus()
	plans, _ := d.PlansStatus()
	asks, _ := d.AsksStatus()
	lessons, _ := d.LessonsStatus()
	approvals, _ := d.ApprovalsStatus()
	pulls, _ := d.PullsStatus()
	return []InboxKind{
		{List: "plans", One: "plan", Many: "plans", Count: len(d.PlansToApprove()), Status: plans},
		{List: "asks", One: "ask", Many: "asks", Count: len(d.asks), Status: asks},
		{List: "lessons", One: "lesson", Many: "lessons", Count: len(d.Lessons()), Status: lessons},
		{List: "pull requests", One: "pull request", Many: "pull requests", Count: len(d.PullsToReview()), Status: pulls},
		{List: "approvals", One: "tool call", Many: "tool calls", Count: len(d.approvals), Status: approvals},
		// A session whose question waits for the owner (ADR-0048): never
		// one of the asks above, it is known from the sessions' list.
		{List: "sessions", One: "session", Many: "sessions", Count: len(d.QuestionsWaiting()), Status: sessions},
	}
}

// InboxNote is what the Inbox's title says waits for the owner, of kinds.
// It claims a count only when every list loaded, and says which did not.
func InboxNote(kinds []InboxKind) string {
	var missing, counts []string
	total := 0
	for _, k := range kinds {
		if k.Status != ListLoaded {
			missing = append(missing, "the "+k.List)
		}
		switch {
		case k.Count == 1:
			counts = append(counts, "1 "+k.One)
		case k.Count > 1:
			counts = append(counts, strconv.Itoa(k.Count)+" "+k.Many)
		}
		total += k.Count
	}
	switch {
	case len(missing) > 0:
		note := joinAnd(missing) + " are not loaded"
		return strings.ToUpper(note[:1]) + note[1:]
	case total == 0:
		return "Nothing waits for you"
	case total == 1:
		return counts[0] + " waits for you"
	}
	return joinAnd(counts) + " wait for you"
}

// joinAnd joins parts as a list: "a", "a and b", "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// RouteCrossReview is the route of a cross-review's session, which the
// service starts and runs alone (protocol rule 46).
const RouteCrossReview = "cross-review"

// RouteWorker is the route of a thread's worker's session, which its
// thread's specialist directs (protocol rule 47), and the prefix, with a
// dash, of its other routes: worker-default, worker-team and worker-asked
// (ADR-0021, ADR-0022).
const RouteWorker = "worker"

// IsWorkerRoute says whether route is a thread's worker's.
func IsWorkerRoute(route string) bool {
	return route == RouteWorker || strings.HasPrefix(route, RouteWorker+"-")
}

// ReadOnly says whether session id takes no message from the owner, and
// why: a cross-review's session is the service's alone, which refuses
// every message to it, so the composer offers none (ADR-0019; Codex review
// of pocketgo#26, round 6). Abort stays.
//
// A thread's worker is its specialist's alone, which the service refuses
// every message to as well (ADR-0021).
func (d *Data) ReadOnly(id int64) (bool, string) {
	s, ok := d.Session(id)
	switch {
	case !ok:
		return false, ""
	case s.Route == RouteCrossReview:
		return true, "pocketd runs this cross-review: its session takes no message, and its verdict goes to the pull request."
	case IsWorkerRoute(s.Route):
		return true, "This is a thread's worker: its specialist directs it, and gets its replies. It takes no message from you."
	}
	return false, ""
}
