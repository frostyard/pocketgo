package state

import (
	"slices"
	"sort"

	"github.com/frostyard/pocketgo/proto"
)

// Data is what the client knows of one pocketd: its sessions, the pending
// approvals and the open session, merged from fetches and events by the
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
	sessions  []proto.Session  // most recently updated first
	approvals []proto.Approval // pending only, oldest first
	open      *OpenSession

	epoch           uint64
	sessionTouched  map[int64]uint64
	approvalTouched map[string]uint64
	// sessionApplied is the token of the newest fetch whose copy of a
	// session's summary was applied.
	sessionApplied map[int64]uint64
	// Where the lists' fetches stand.
	sessionsList, approvalsList list
	rev                         uint64
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
	// Live is what its run is doing now.
	Live proto.Live
	// Queued are the messages the owner sent that the transcript does not
	// show yet, oldest first: a message sent while the session runs waits
	// for the run to reach it (protocol rule 16a).
	Queued []QueuedMessage

	liveTouched   uint64
	detailApplied uint64
	rev           uint64
}

// QueuedMessage is a message the owner sent, answered with its
// submission, whose entry has not come.
type QueuedMessage struct {
	SubmissionID int64
	Text         string

	// sent is the newest fetch token when the answer came: a fetch begun
	// later reads the service after the message was taken. sawBusy says the
	// session was seen running since.
	sent    uint64
	sawBusy bool
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
// an approval pending in the client's list is waiting, and else the open
// session is running while its live state is busy: the approval's and the
// live state's events come before the session's own, and a refetch reads
// the lists in separate requests.
func (d *Data) StateOf(id int64) proto.SessionState {
	for _, a := range d.approvals {
		if a.SessionID == id {
			return proto.SessionWaiting
		}
	}
	if d.open != nil && d.open.ID == id && d.open.Live.Busy {
		return proto.SessionRunning
	}
	if s, ok := d.Session(id); ok && s.State != "" {
		return s.State
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
			d.appliedSummary(s.ID, f)
		}
		out = append(out, s)
	}
	for _, s := range d.sessions {
		if !seen[s.ID] && !d.takesSummary(s.ID, f) {
			out = append(out, s)
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
// state is the detail's unless a live event came since f began. The
// queued messages it shows an entry of leave the queue; and when the
// service was not running, so do those answered before f began, which it
// had either placed or withdrawn by then.
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
		o.placed(e)
	}
	if !newer(o.liveTouched, f) {
		o.Live = detail.Live
		o.Live.SessionID = o.ID
	}
	if !detail.Live.Busy {
		o.Queued = slices.DeleteFunc(o.Queued, func(q QueuedMessage) bool { return q.sent < f.epoch })
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

// Apply applies one event of the stream, and reports whether it asks the
// client to refetch what it shows (EventResync). An event of a type this
// client does not know changes nothing.
func (d *Data) Apply(ev proto.Event) (resync bool) {
	d.rev++
	switch ev.Type {
	case proto.EventResync:
		return true
	case proto.EventSession:
		if ev.Session != nil {
			d.touchSession(ev.Session.ID)
			d.putSession(*ev.Session)
		}
	case proto.EventEntry:
		if ev.Entry != nil {
			d.applyEntry(*ev.Entry)
		}
	case proto.EventLive:
		if ev.Live != nil && d.open != nil && ev.Live.SessionID == d.open.ID {
			d.open.Live = *ev.Live
			d.open.liveTouched = d.touched()
			d.open.rev++
			d.open.followRun()
		}
	case proto.EventApproval:
		if ev.Approval != nil {
			d.applyApproval(*ev.Approval)
		}
	}
	return false
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
	o.placed(e)
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
func (d *Data) Created(s proto.Session) {
	d.rev++
	d.touchSession(s.ID)
	d.putSession(s)
}

func (d *Data) putSession(s proto.Session) {
	if i := slices.IndexFunc(d.sessions, func(x proto.Session) bool { return x.ID == s.ID }); i >= 0 {
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

// Sent records that the owner's message text to session id was taken as
// submission: until its entry comes, the open session shows it queued.
// An entry that came before the answer leaves nothing to queue.
func (d *Data) Sent(id, submission int64, text string) {
	o := d.open
	if o == nil || o.ID != id || submission == 0 {
		return
	}
	shown := slices.ContainsFunc(o.Entries, func(e proto.Entry) bool { return e.SubmissionID == submission })
	if shown || slices.ContainsFunc(o.Queued, func(q QueuedMessage) bool { return q.SubmissionID == submission }) {
		return
	}
	d.rev++
	o.rev++
	o.Queued = append(o.Queued, QueuedMessage{SubmissionID: submission, Text: text, sent: d.epoch, sawBusy: o.Live.Busy})
}

// placed takes the message entry e placed out of the queue.
func (o *OpenSession) placed(e proto.Entry) {
	if e.SubmissionID != 0 {
		o.Queued = slices.DeleteFunc(o.Queued, func(q QueuedMessage) bool { return q.SubmissionID == e.SubmissionID })
	}
}

// followRun follows the queued messages through a live change: a message
// waits while the session runs, and leaves the queue when a run seen
// since its answer ends. That run placed it, and its entry came first, or
// an abort withdrew it. A run must have been seen: an idle state the
// stream sent before the service took the message says nothing of it.
func (o *OpenSession) followRun() {
	if o.Live.Busy {
		for i := range o.Queued {
			o.Queued[i].sawBusy = true
		}
		return
	}
	o.Queued = slices.DeleteFunc(o.Queued, func(q QueuedMessage) bool { return q.sawBusy })
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
