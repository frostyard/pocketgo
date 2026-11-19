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
// fetch begun before one already applied is dropped.
type Data struct {
	sessions  []proto.Session  // most recently updated first
	approvals []proto.Approval // pending only, oldest first
	open      *OpenSession

	epoch           uint64
	sessionTouched  map[int64]uint64
	approvalTouched map[string]uint64
	// The token of the newest list fetch applied.
	sessionsApplied, approvalsApplied uint64
	rev                               uint64
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

	liveTouched   uint64
	detailApplied uint64
}

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
// answer of a fetch begun later was applied already. A session
// an event changed since f began keeps the event's copy, and one an event
// created since stays.
func (d *Data) SetSessions(f Fetch, list []proto.Session) {
	if f.epoch < d.sessionsApplied {
		return
	}
	d.rev++
	d.sessionsApplied = f.epoch
	out := make([]proto.Session, 0, len(list))
	seen := map[int64]bool{}
	for _, s := range list {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		if newer(d.sessionTouched[s.ID], f) {
			if cur, ok := d.Session(s.ID); ok {
				s = cur
			}
		}
		out = append(out, s)
	}
	for _, s := range d.sessions {
		if !seen[s.ID] && newer(d.sessionTouched[s.ID], f) {
			out = append(out, s)
		}
	}
	d.sessions = out
	d.sortSessions()
}

// SetApprovals takes the answer of GET /v1/approvals, begun as f, unless
// the answer of a fetch begun later was applied already. An
// approval an event changed since f began follows the event: pending, it
// stays; decided, it is gone even if the answer still lists it.
func (d *Data) SetApprovals(f Fetch, list []proto.Approval) {
	if f.epoch < d.approvalsApplied {
		return
	}
	d.rev++
	d.approvalsApplied = f.epoch
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
	d.approvals = out
	d.sortApprovals()
}

// SetDetail takes the answer of GET /v1/sessions/{id}, begun as f, when
// that session is still open and no detail begun later was applied. Entries merge by id with those events
// brought meanwhile; the live state is the detail's unless a live event
// came since f began. The detail's session updates the list as SetSessions
// would.
func (d *Data) SetDetail(f Fetch, detail proto.SessionDetail) {
	d.rev++
	if _, ok := d.Session(detail.Session.ID); !ok || !newer(d.sessionTouched[detail.Session.ID], f) {
		d.putSession(detail.Session)
	}
	o := d.open
	if o == nil || o.ID != detail.Session.ID || f.epoch < o.detailApplied {
		return
	}
	o.detailApplied = f.epoch
	o.Loaded, o.Err = true, ""
	for _, e := range detail.Entries {
		o.addEntry(e)
	}
	if !newer(o.liveTouched, f) {
		o.Live = detail.Live
		o.Live.SessionID = o.ID
	}
}

// SetDetailError records why the open session id did not load.
func (d *Data) SetDetailError(id int64, err string) {
	d.rev++
	if d.open != nil && d.open.ID == id {
		d.open.Err = err
	}
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

// applyEntry adds a written entry: to the open session's transcript when it
// is that session's, and to the session's place in the list, which the
// service orders by when a session last changed. The service does not send
// the session again for a new entry, so the client moves it itself.
func (d *Data) applyEntry(e proto.Entry) {
	for i := range d.sessions {
		if d.sessions[i].ID == e.SessionID && e.At.After(d.sessions[i].UpdatedAt) {
			d.sessions[i].UpdatedAt = e.At
			d.sortSessions()
			break
		}
	}
	o := d.open
	if o == nil || o.ID != e.SessionID {
		return
	}
	o.addEntry(e)
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
