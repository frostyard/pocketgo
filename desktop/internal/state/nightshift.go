package state

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The night shift on the Work Board (ADR-0034): the proposals the team
// gathered, which the owner picks from and launches until an end, and the
// schedules that send messages and start gathers (ADR-0030, ADR-0033).
//
// The proposals merge from fetches and events by the rule of the threads
// (keyed); the schedules are one list, fetched whole, since their event
// carries none (protocol rule 57).

func proposalID(p proto.Proposal) int64 { return p.ID }

// proposalLess orders proposals as the board does: by Order, then ID.
func proposalLess(a, b proto.Proposal) bool {
	if a.Order != b.Order {
		return a.Order < b.Order
	}
	return a.ID < b.ID
}

// BeginProposals starts a proposals fetch, counted as BeginThreads is.
func (d *Data) BeginProposals() Fetch { return d.proposals.begin(d) }

// SetProposals takes the answer of GET /v1/proposals, begun as f, by the
// rule of SetPulls: a proposal an event changed since f began follows the
// event, listed or not.
func (d *Data) SetProposals(f Fetch, list []proto.Proposal) {
	kept := list[:0:0]
	for _, p := range list {
		if !d.droppedProposals[p.ID] || p.State == proto.ProposalDropped {
			kept = append(kept, p)
		}
	}
	if d.proposals.setKeeping(f, kept, proposalID, proposalLess, droppedHeld) {
		d.rev++
	}
}

// droppedHeld says the held copy is dropped and got is not: a dropped
// proposal never changes again, so got is older (Codex review of #58,
// round 4).
func droppedHeld(held, got proto.Proposal) bool {
	return held.State == proto.ProposalDropped && got.State != proto.ProposalDropped
}

// SetProposalsError records that the proposals' fetch f failed, unless a
// fetch begun later already answered; the proposals stay.
func (d *Data) SetProposalsError(f Fetch, err string) {
	if d.proposals.fail(f, err) {
		d.rev++
	}
}

// ProposalsStatus is where the proposals stand, and why their last fetch
// failed. The board says no proposal waits only when it is ListLoaded.
func (d *Data) ProposalsStatus() (ListStatus, string) {
	return d.proposals.list.status, d.proposals.list.err
}

// Proposal returns the proposal with id, in whatever state the client
// last saw it.
func (d *Data) Proposal(id int64) (proto.Proposal, bool) {
	if i := d.proposals.index(id, proposalID); i >= 0 {
		return d.proposals.items[i], true
	}
	return proto.Proposal{}, false
}

// BoardProposals are the proposals the owner may pick, launch or drop:
// those still proposed, in board order (Order, then ID). A launched or
// dropped one is off the board, whatever order its events came in.
func (d *Data) BoardProposals() []proto.Proposal {
	var out []proto.Proposal
	for _, p := range d.proposals.items {
		if onBoard(p) {
			out = append(out, p)
		}
	}
	return out
}

// onBoard says whether p is on the board: proposed.
func onBoard(p proto.Proposal) bool { return p.State == proto.ProposalProposed }

// ProposalDropped applies the reply of POST /v1/proposals/{id}/drop, as
// the proposal's event would: a dropped proposal never changes again.
func (d *Data) ProposalDropped(p proto.Proposal) { d.applyProposal(p) }

func (d *Data) applyProposal(p proto.Proposal) {
	if held, ok := d.Proposal(p.ID); ok && droppedHeld(held, p) {
		return
	}
	// A drop seen once is known for the sign-in, though a list that omits
	// dropped proposals took the row away since (Codex review of #58,
	// round 5).
	if p.State == proto.ProposalDropped {
		if d.droppedProposals == nil {
			d.droppedProposals = map[int64]bool{}
		}
		d.droppedProposals[p.ID] = true
	} else if d.droppedProposals[p.ID] {
		return
	}
	d.rev++
	d.proposals.put(d.touched(), p, proposalID, proposalLess)
}

// ProposalCaption is what a proposal's row says under its title: its
// agent, its repository or "home", its kind, its rounds, "needs green CI"
// when its shift waits for CI (ADR-0047), and when its gather ran, in the
// zone of now (the desktop's).
func (d *Data) ProposalCaption(p proto.Proposal, now time.Time) string {
	where := p.Repository
	if where == "" {
		where = "home"
	}
	parts := []string{d.AgentName(p.Agent), where}
	if p.Kind != "" {
		parts = append(parts, p.Kind)
	}
	parts = append(parts, plural(p.MaxRounds, "round", "rounds"))
	if p.CI {
		parts = append(parts, "needs green CI")
	}
	if !p.GatheredAt.IsZero() {
		parts = append(parts, "gathered "+ClockAt(p.GatheredAt, now))
	}
	return strings.Join(parts, " · ")
}

// BeginSchedules starts a schedules fetch: the list comes whole, and the
// answer of a fetch begun later wins.
func (d *Data) BeginSchedules() Fetch { return d.Begin() }

// SetSchedules records fetch f's schedules, unless a fetch begun later
// already answered: a schedules event refetches, and the answer of the
// fetch it began is newer than one begun before it.
func (d *Data) SetSchedules(f Fetch, s proto.Schedules) {
	if !d.schedulesList.answer(f) {
		return
	}
	d.rev++
	d.schedulesList.status, d.schedulesList.err = ListLoaded, ""
	s.Schedules = slices.Clone(s.Schedules)
	d.schedules = s
}

// SetSchedulesError records that the schedules' fetch f failed, unless a
// fetch begun later already answered; the schedules stay.
func (d *Data) SetSchedulesError(f Fetch, err string) {
	if d.schedulesList.answer(f) {
		d.rev++
		d.schedulesList.status, d.schedulesList.err = ListFailed, err
	}
}

// SchedulesStatus is where the schedules stand, and why their last fetch
// failed.
func (d *Data) SchedulesStatus() (ListStatus, string) {
	return d.schedulesList.status, d.schedulesList.err
}

// Schedules are the schedules of the newest answer: the service's zone,
// the owner's file's problem, and each schedule. Its slice is the Data's
// own.
func (d *Data) Schedules() proto.Schedules { return d.schedules }

// ScheduleRow is what the board shows of one schedule.
type ScheduleRow struct {
	// ID is the schedule's id; When how it repeats, or when it goes out
	// once; Next "next <time>", "" when it goes out no more.
	ID, When, Next string
	// Action is its message, or "gather <skill>" for a gather.
	Action string
	// Last is what its last time did: its outcome, time and why; "" for
	// none yet.
	Last string
	// Problem says why it does not go out, "" when it does.
	Problem string
}

// ScheduleRowOf is s as the board shows it, its next and last times in
// the zone of now (the desktop's). Its repeat or when is in its own zone's
// clock, which it names when the desktop's clock differs (sameZone).
func ScheduleRowOf(s proto.Schedule, now time.Time) ScheduleRow {
	row := ScheduleRow{ID: s.ID, When: s.Repeat, Action: s.Message, Problem: s.Problem}
	if row.When == "" {
		row.When = s.When
	}
	// Its clock times are its zone's; next and last are here (Codex review
	// of #58, round 5). Local is pocketd's host's zone, whatever it is: it
	// names no zone the desktop can read (round 7).
	switch {
	case s.Zone == "Local":
		row.When += " (pocketd's time)"
	case s.Zone != "" && !sameZone(s.Zone, now, s.Next):
		row.When += " (" + s.Zone + ")"
	}
	if s.Next != nil {
		row.Next = "next " + ClockAt(*s.Next, now)
	}
	if s.Gather != nil {
		row.Action = "gather " + s.Gather.Skill
	}
	if l := s.Last; l != nil {
		row.Last = "last " + string(l.Outcome) + " " + ClockAt(l.At, now)
		if l.Why != "" {
			row.Last += ": " + l.Why
		}
	}
	return row
}

// sameZone says the clock of zone, an IANA name, reads as now's, the
// desktop's: the same name and offset at now, at next (its next time, nil
// for none) and through the year after now, so a schedule's clock times
// read the same here whenever it goes out. It looks at each instant either
// clock changes (time.Time.ZoneBounds), and keeps its answer for the hour,
// and each zone it loads, so a redraw does neither again. A zone the
// desktop cannot load is another (Codex review of #58, rounds 5 to 7).
func sameZone(zone string, now time.Time, next *time.Time) bool {
	here := now.Location()
	if zone == here.String() && zone != "Local" {
		return true
	}
	key := zoneKey{zone: zone, here: here, hour: now.Truncate(time.Hour).Unix()}
	if next != nil {
		key.next = next.Unix()
	}
	zones.mu.Lock()
	defer zones.mu.Unlock()
	if same, ok := zones.same[key]; ok {
		return same
	}
	loc, ok := zones.loaded[zone]
	if !ok {
		loc, _ = loadLocation(zone)
		if zones.loaded == nil {
			zones.loaded = map[string]*time.Location{}
		}
		zones.loaded[zone] = loc
	}
	same := loc != nil && compareClocks(loc, here, now, next)
	if zones.same == nil || len(zones.same) >= 1024 {
		zones.same = map[zoneKey]bool{}
	}
	zones.same[key] = same
	return same
}

// loadLocation loads a zone by its IANA name, and compareClocks is
// clocksAgree; tests count their calls.
var (
	loadLocation  = time.LoadLocation
	compareClocks = clocksAgree
)

// zoneKey is what sameZone's answer depends on: the zone, the desktop's
// zone, the hour of now and the next time.
type zoneKey struct {
	zone       string
	here       *time.Location
	hour, next int64
}

// zones are sameZone's answers and the zones it loaded, nil for one that
// would not load.
var zones struct {
	mu     sync.Mutex
	same   map[zoneKey]bool
	loaded map[string]*time.Location
}

// clocksAgree says loc's and here's clocks have the same name and offset
// at next and at every instant from now to a year on: at now and at each
// instant either changes.
func clocksAgree(loc, here *time.Location, now time.Time, next *time.Time) bool {
	same := func(t time.Time) bool {
		name, offset := t.In(loc).Zone()
		hereName, hereOffset := t.In(here).Zone()
		return name == hereName && offset == hereOffset
	}
	if next != nil && !same(*next) {
		return false
	}
	until := now.AddDate(1, 0, 1)
	for t := now; t.Before(until); {
		if !same(t) {
			return false
		}
		_, a := t.In(loc).ZoneBounds()
		_, b := t.In(here).ZoneBounds()
		switch {
		case a.IsZero() && b.IsZero():
			return true
		case a.IsZero() || (!b.IsZero() && b.Before(a)):
			t = b
		default:
			t = a
		}
	}
	return true
}

// ClockAt is t as a short time in the zone of now: "today 15:04",
// "tomorrow 15:04" or "yesterday 15:04", a weekday and the time within a
// week either way, else the date and the time.
func ClockAt(t, now time.Time) string {
	t = t.In(now.Location())
	day := func(x time.Time) time.Time { return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC) }
	days := int(day(t).Sub(day(now)).Hours() / 24)
	clock := t.Format("15:04")
	switch {
	case days == 0:
		return "today " + clock
	case days == 1:
		return "tomorrow " + clock
	case days == -1:
		return "yesterday " + clock
	case days > -7 && days < 7:
		return t.Format("Mon 15:04")
	}
	return t.Format("2 Jan 15:04")
}

// LaunchChoiceHours is how long a launch runs when the owner chooses the end
// chooser's hours segment instead of a clock time (ADR-0034); the chooser
// opens on Until with proto.DefaultLaunchUntil.
const LaunchChoiceHours = 4

// LaunchEnd is the absolute end of a launch the owner makes at now: until
// empty is LaunchChoiceHours from now; a clock time, HH:MM (or H:MM) on
// the 24-hour clock, is the first such time strictly after now in now's
// zone, the desktop's, so the current minute is tomorrow's. Anything else
// is an error.
func LaunchEnd(until string, now time.Time) (time.Time, error) {
	until = strings.TrimSpace(until)
	if until == "" {
		return now.Add(LaunchChoiceHours * time.Hour), nil
	}
	// The service's policy across daylight saving, one function (Codex
	// review of #58, round 1).
	return proto.NextClockTime(until, now, now.Location())
}

// ErrNoClock is what the end chooser says while Until is chosen and its
// clock time is blank.
var ErrNoClock = errors.New("write the clock time the launch ends at, as 07:30")

// UntilOf is the end chooser as LaunchPicks takes it: "" for in
// LaunchChoiceHours hours, else the clock time typed for Until, which a
// blank one is not (ErrNoClock): it would launch for LaunchChoiceHours hours.
func UntilOf(until bool, clock string) (string, error) {
	if !until {
		return "", nil
	}
	clock = strings.TrimSpace(clock)
	if clock == "" {
		return "", ErrNoClock
	}
	return clock, nil
}

// keptLaunch is the owner's launch whose request id is kept: its picks,
// the chooser it was made with and the end computed then. Pressed again
// with the same chooser before the owner changed a pick, it goes again
// unchanged.
type keptLaunch struct {
	ids   []int64
	until string
	end   time.Time
}

const launchKey = "launch"

// launchAction is what a launch's request id is kept for: its proposals,
// in order, and its end.
func launchAction(ids []int64, end time.Time) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",") + "\x00" + end.UTC().Format(time.RFC3339Nano)
}

func dropKey(id int64) string { return fmt.Sprintf("drop:%d", id) }

func (s *Store) now() time.Time {
	if s.opts.Now == nil {
		return time.Now()
	}
	return s.opts.Now()
}

// TogglePick picks proposal id for the next launch, or unpicks it. Only a
// proposal on the board is picked, and nothing changes while a launch is
// on its way: its success clears the picks it sent.
func (s *Store) TogglePick(id int64) {
	if s.launching.Busy {
		return
	}
	// The owner's own change of a pick makes a new launch; a pick the board
	// took away does not (Codex review of #58, rounds 5 and 7).
	s.forgetLaunch()
	if s.picks[id] {
		delete(s.picks, id)
		return
	}
	if p, ok := s.Data.Proposal(id); ok && onBoard(p) {
		s.picks[id] = true
	}
}

// Picked says whether proposal id is picked.
func (s *Store) Picked(id int64) bool { return s.picks[id] }

// Picks are the picked proposals' ids, in board order: what a launch
// sends. Only proposals still on the board count, so a launched or dropped
// one is never sent.
func (s *Store) Picks() []int64 {
	var out []int64
	for _, p := range s.Data.BoardProposals() {
		if s.picks[p.ID] {
			out = append(out, p.ID)
		}
	}
	return out
}

// PlannedEnd is the end LaunchPicks would send now with until: the end of
// the launch that failed while the owner changed neither its picks nor
// its chooser, so a retry goes unchanged, else LaunchEnd from now.
func (s *Store) PlannedEnd(until string) (time.Time, error) {
	if k := s.resend(until); k != nil {
		return k.end, nil
	}
	return LaunchEnd(until, s.now())
}

// PlannedPicks are the proposals LaunchPicks would send now with until:
// those of the launch that failed while the owner changed neither its
// picks nor its chooser, in the order it sent them, though the board was
// ordered since or took one away; else Picks (Codex review of #58, rounds
// 2 and 5).
func (s *Store) PlannedPicks(until string) []int64 {
	if k := s.resend(until); k != nil {
		return k.ids
	}
	return s.Picks()
}

// resend is the kept launch a press with until sends again, nil for none.
func (s *Store) resend(until string) *keptLaunch {
	if k := s.launchKept; k != nil && k.until == strings.TrimSpace(until) {
		return k
	}
	return nil
}

// Launching is where the owner's launch stands.
func (s *Store) Launching() Pending { return s.launching }

// Launched is the last launch this client made, nil before one.
func (s *Store) Launched() *proto.Launch { return s.launched }

// LaunchPicks launches the picked proposals (POST /v1/launches), in board
// order, until the end until names (LaunchEnd), computed once. A launch
// that failed keeps its request: pressed again before the owner changed a
// pick or the chooser, it sends the same proposals, end and request id
// (PlannedPicks, PlannedEnd), so one whose reply was lost is never made
// twice, as pocketd answers a retried request id with the launch it made
// (protocol rule 59). The owner's change of a pick or the end choice is a
// new request; a pick the board took away is not. pocketd's refusal (400, 409) recorded nothing,
// so the next press is a new request, from now.
func (s *Store) LaunchPicks(until string) {
	if s.launching.Busy {
		return
	}
	until = strings.TrimSpace(until)
	ids := s.PlannedPicks(until)
	if len(ids) == 0 {
		s.launching.Err = "Pick a proposal to launch first."
		return
	}
	end, err := s.PlannedEnd(until)
	if err != nil {
		s.launching.Err = err.Error()
		return
	}
	s.launchKept = &keptLaunch{ids: ids, until: until, end: end}
	request := proto.CreateLaunch{RequestID: s.ids.For(launchKey, launchAction(ids, end)), Proposals: ids, End: end.UTC()}
	s.launching = Pending{Busy: true}
	s.log(LevelCommand, "Launch %d proposals until %s", len(ids), end.UTC().Format(time.RFC3339))
	var launched proto.Launch
	s.do("Launching the picks", func(ctx context.Context, c *proto.Client) (err error) {
		launched, err = c.Launch(ctx, request)
		return err
	}, func(err error) {
		s.launching.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(launchKey)
			s.launchKept = nil
			s.launched = &launched
			s.picks = map[int64]bool{}
			s.loadProposals()
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeBadRequest || pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound):
			s.ids.Done(launchKey)
			s.launchKept = nil
			s.launching.Err = "pocketd refused the launch: " + pe.Message
			s.loadProposals()
		default:
			s.launching.Err = "Launching failed: " + err.Error() + "; press Launch again to retry the same launch"
		}
		s.settleProposals()
	})
}

// Dropping is where the owner's drop of proposal id stands.
func (s *Store) Dropping(id int64) Pending { return valueOf(s.drops[id]) }

// DropProposal drops proposal id for the owner at rev, the rev the board
// shows (POST /v1/proposals/{id}/drop). A drop tried again at the same rev
// after a failure keeps its request id, so a lost reply is not applied
// twice; pocketd's refusal (a stale rev, a proposal no longer proposed, or
// none) is final, and the proposals are fetched again.
func (s *Store) DropProposal(id int64, rev int) {
	p := pending(s.drops, id)
	if p.Busy {
		return
	}
	key := dropKey(id)
	request := proto.DropProposal{RequestID: s.ids.For(key, strconv.Itoa(rev)), Rev: rev}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Drop proposal #%d", id)
	var dropped proto.Proposal
	s.do(fmt.Sprintf("Dropping proposal #%d", id), func(ctx context.Context, c *proto.Client) (err error) {
		dropped, err = c.DropProposal(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(key)
			delete(s.drops, id)
			s.Data.ProposalDropped(dropped)
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound || pe.Code == proto.ErrorCodeBadRequest):
			s.ids.Done(key)
			p.Err = pe.Message
			s.loadProposals()
		default:
			p.Err = err.Error()
		}
		s.settleProposals()
	})
}

// settleProposals follows the board: a pick of a proposal no longer on it
// goes, so one that comes back (a launch item skipped) is not picked
// again unseen; a drop of one off the board keeps no retry state; and a
// failed launch whose proposals all left the board keeps none either, as
// when pocketd made it and only its reply was lost. What is on its way
// keeps its state.
func (s *Store) settleProposals() {
	for id := range s.picks {
		if p, ok := s.Data.Proposal(id); !ok || !onBoard(p) {
			delete(s.picks, id)
		}
	}
	for id, p := range s.drops {
		if p.Busy {
			continue
		}
		if prop, ok := s.Data.Proposal(id); !ok || !onBoard(prop) {
			delete(s.drops, id)
			s.ids.Done(dropKey(id))
		}
	}
	// A launch whose answer was lost stays to be pressed again, though its
	// picks left the board: their leaving does not say which request took
	// them, and pocketd's answer to the same request does (Codex review of
	// #58, round 6).
}

// LaunchUncertain says a launch whose answer was lost waits to be pressed
// again: pocketd answers the same request with the launch it made, or
// refuses it, so the board offers it though no pick is left on it.
func (s *Store) LaunchUncertain() bool { return s.launchKept != nil }

// LaunchEndEdited says the owner changed the end chooser: a launch that
// failed goes as a new request at the next press, its end from the clock
// then (Codex review of #58, round 6).
func (s *Store) LaunchEndEdited() {
	if !s.launching.Busy {
		s.forgetLaunch()
	}
}

// forgetLaunch drops the launch that failed, at the owner's edit: its
// request id is retired with it, so the next press is a new request
// though it names the same proposals and end. pocketd's answer to it no
// longer comes, and nothing says it failed (Codex review of #58, round 7).
func (s *Store) forgetLaunch() {
	if s.launchKept == nil {
		return
	}
	s.launchKept = nil
	s.ids.Done(launchKey)
	s.launching.Err = ""
}

func (s *Store) loadProposals() {
	f := s.Data.BeginProposals()
	var list []proto.Proposal
	s.do("Loading the proposals", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Proposals(ctx)
		return err
	}, s.track("proposals", func(err error) {
		if err != nil {
			s.Data.SetProposalsError(f, err.Error())
			return
		}
		s.Data.SetProposals(f, list)
		s.settleProposals()
	}))
}

// refetchSchedules follows a schedules event: one fetch at a time, and one
// more after it for every event that came while it ran (Codex review of
// #58, round 4).
func (s *Store) refetchSchedules() {
	s.schedulesEvents++
	if s.fetching["schedules"] > 0 {
		s.schedulesDirty = true
		return
	}
	s.loadSchedules()
}

func (s *Store) loadSchedules() {
	f := s.Data.BeginSchedules()
	events := s.schedulesEvents
	var list proto.Schedules
	s.do("Loading the schedules", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Schedules(ctx)
		return err
	}, s.track("schedules", func(err error) {
		// An answer a schedules event came after is older than the event,
		// whatever else is fetching: it is not shown. Once none fetches,
		// the fetch the event asked for follows (Codex review of #58,
		// rounds 5 and 6).
		if events == s.schedulesEvents {
			s.schedulesDirty = false
			if err != nil {
				s.Data.SetSchedulesError(f, err.Error())
			} else {
				s.Data.SetSchedules(f, list)
			}
		}
		if s.schedulesDirty && s.fetching["schedules"] == 0 {
			s.schedulesDirty = false
			s.loadSchedules()
		}
	}))
}

// plural is "1 round" or "n rounds".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
