package state

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func proposal(id int64, order int, st proto.ProposalState) proto.Proposal {
	return proto.Proposal{ID: id, Gather: 1, GatheredAt: pocketdtest.Now.Add(-time.Hour), Agent: "vera", Repository: "frostyard/nsl",
		Title: "Fix the flaky job", Task: "Find why it flakes.", StopWhen: "it passes 20 times", MaxRounds: 10, Kind: "ci", Order: order,
		Rev: 1, State: st, By: "vera", CreatedAt: pocketdtest.Now.Add(-time.Hour), UpdatedAt: pocketdtest.Now.Add(-time.Hour)}
}

func proposalIDs(list []proto.Proposal) []int64 {
	var out []int64
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

// A proposal launched or dropped while a fetch was on its way stays off
// the board when the fetch's stale copy arrives; one filed meanwhile
// stays, listed or not; and an older fetch answering last changes nothing
// (the Data merge rule, as the pull requests follow it).
func TestProposalsFollowTheirEventsOverAStaleList(t *testing.T) {
	var d Data
	d.SetProposals(d.BeginProposals(), []proto.Proposal{proposal(1, 1, proto.ProposalProposed), proposal(2, 2, proto.ProposalProposed)})
	f := d.BeginProposals()
	launched := proposal(1, 1, proto.ProposalLaunched)
	d.Apply(proto.Event{Type: proto.EventProposal, Proposal: &launched})
	filed := proposal(3, 3, proto.ProposalProposed)
	d.Apply(proto.Event{Type: proto.EventProposal, Proposal: &filed})
	d.SetProposals(f, []proto.Proposal{proposal(1, 1, proto.ProposalProposed), proposal(2, 2, proto.ProposalProposed)})
	if got := proposalIDs(d.BoardProposals()); !slices.Equal(got, []int64{2, 3}) {
		t.Errorf("board %v, want #1 off it (launched since the fetch began) and #3 filed meanwhile: [2 3]", got)
	}
	if st, _ := d.ProposalsStatus(); st != ListLoaded {
		t.Errorf("status %v", st)
	}
	older := d.BeginProposals()
	newest := d.BeginProposals()
	d.SetProposals(newest, []proto.Proposal{proposal(4, 1, proto.ProposalProposed)})
	d.SetProposals(older, []proto.Proposal{proposal(5, 1, proto.ProposalProposed)})
	if got := proposalIDs(d.BoardProposals()); !slices.Equal(got, []int64{4}) {
		t.Errorf("an older fetch answering last left %v, want [4]", got)
	}
	d.SetProposalsError(d.BeginProposals(), "timeout")
	if st, err := d.ProposalsStatus(); st != ListFailed || err != "timeout" || len(d.BoardProposals()) != 1 {
		t.Errorf("a failed fetch: %v %q, board %v", st, err, proposalIDs(d.BoardProposals()))
	}
	if d.proposals.touched != nil {
		t.Errorf("markers %v outlive every fetch", d.proposals.touched)
	}
}

// The board lists the proposals still proposed, by Order then ID, whatever
// order the list and the events brought them in.
func TestBoardProposalsAreTheProposedInBoardOrder(t *testing.T) {
	var d Data
	d.SetProposals(d.BeginProposals(), []proto.Proposal{
		proposal(7, 2, proto.ProposalProposed),
		proposal(3, 1, proto.ProposalLaunched),
		proposal(9, 1, proto.ProposalProposed),
		proposal(4, 2, proto.ProposalProposed),
	})
	moved := proposal(2, 0, proto.ProposalProposed)
	d.Apply(proto.Event{Type: proto.EventProposal, Proposal: &moved})
	dropped := proposal(4, 2, proto.ProposalDropped)
	d.Apply(proto.Event{Type: proto.EventProposal, Proposal: &dropped})
	if got := proposalIDs(d.BoardProposals()); !slices.Equal(got, []int64{2, 9, 7}) {
		t.Errorf("board %v, want [2 9 7]: by order, then id, launched and dropped left out", got)
	}
}

// LaunchEnd: empty is 4 hours on; a clock time is the first such time
// strictly after now in now's zone, so an earlier one, or the current
// minute, is tomorrow's; anything else is an error.
func TestLaunchEnd(t *testing.T) {
	zone := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 18, 22, 0, 0, 0, zone)
	late := time.Date(2026, 11, 18, 22, 0, 30, 0, zone)
	for _, c := range []struct {
		until string
		now   time.Time
		want  time.Time
	}{
		{"", now, now.Add(4 * time.Hour)},
		{"  ", now, now.Add(4 * time.Hour)},
		{"23:30", now, time.Date(2026, 11, 18, 23, 30, 0, 0, zone)},
		{"07:00", now, time.Date(2026, 11, 19, 7, 0, 0, 0, zone)},
		{"7:05", now, time.Date(2026, 11, 19, 7, 5, 0, 0, zone)},
		{" 06:15 ", now, time.Date(2026, 11, 19, 6, 15, 0, 0, zone)},
		{"00:00", now, time.Date(2026, 11, 19, 0, 0, 0, 0, zone)},
		// The exact current minute is tomorrow's, and so is a minute that
		// began before now.
		{"22:00", now, time.Date(2026, 11, 19, 22, 0, 0, 0, zone)},
		{"22:00", late, time.Date(2026, 11, 19, 22, 0, 0, 0, zone)},
		{"22:01", late, time.Date(2026, 11, 18, 22, 1, 0, 0, zone)},
		// Across the month's end.
		{"01:00", time.Date(2026, 11, 30, 23, 0, 0, 0, zone), time.Date(2026, 12, 1, 1, 0, 0, 0, zone)},
	} {
		got, err := LaunchEnd(c.until, c.now)
		if err != nil || !got.Equal(c.want) || got.Location() != c.now.Location() {
			t.Errorf("LaunchEnd(%q, %v) = %v, %v; want %v", c.until, c.now, got, err, c.want)
		}
	}
	for _, bad := range []string{"24:00", "7", "7:5", "07:60", "noon", "7pm", "07:00:00", "-1:00", "07.30", "007:00", "in 4 hours"} {
		if got, err := LaunchEnd(bad, now); err == nil {
			t.Errorf("LaunchEnd(%q) = %v, want an error", bad, got)
		}
	}
}

// Until with a blank clock time is no choice: it would launch for 4 hours.
func TestUntilOf(t *testing.T) {
	if got, err := UntilOf(false, "07:00"); got != "" || err != nil {
		t.Errorf("in 4 hours: %q, %v", got, err)
	}
	if got, err := UntilOf(true, " 07:00 "); got != "07:00" || err != nil {
		t.Errorf("until 07:00: %q, %v", got, err)
	}
	if _, err := UntilOf(true, "  "); !errors.Is(err, ErrNoClock) {
		t.Errorf("until a blank time: %v", err)
	}
}

// Times show in the zone of the desktop's clock, short.
func TestClockAt(t *testing.T) {
	zone := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 18, 22, 0, 0, 0, zone) // a Wednesday
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 11, 18, 21, 30, 0, 0, time.UTC), "today 22:30"},
		{time.Date(2026, 11, 18, 23, 30, 0, 0, time.UTC), "tomorrow 00:30"},
		{time.Date(2026, 11, 17, 8, 0, 0, 0, zone), "yesterday 08:00"},
		{time.Date(2026, 11, 21, 7, 0, 0, 0, zone), "Sat 07:00"},
		{time.Date(2026, 11, 14, 7, 0, 0, 0, zone), "Sat 07:00"},
		{time.Date(2026, 11, 2, 7, 0, 0, 0, zone), "2 Nov 07:00"},
	} {
		if got := ClockAt(c.at, now); got != c.want {
			t.Errorf("ClockAt(%v) = %q, want %q", c.at, got, c.want)
		}
	}
}

// A schedule's row: its repeat or its when, its next time, a gather's
// skill in place of its message, its last time and its problem.
func TestScheduleRowOf(t *testing.T) {
	now := pocketdtest.Now
	next := now.Add(9 * time.Hour)
	got := ScheduleRowOf(proto.Schedule{ID: "nightly", When: "every day 07:00", Repeat: "every day at 07:00", Message: "Plan the day.",
		Next: &next, Last: &proto.ScheduleTime{At: now.Add(-15 * time.Hour), Outcome: proto.ScheduleMissed, Why: "no standing session"}}, now)
	want := ScheduleRow{ID: "nightly", When: "every day at 07:00", Next: "next tomorrow 07:00", Action: "Plan the day.",
		Last: "last missed today 07:00: no standing session"}
	if got != want {
		t.Errorf("a repeat:\n got %+v\nwant %+v", got, want)
	}
	got = ScheduleRowOf(proto.Schedule{ID: "S3", When: "in 2h", Gather: &proto.ScheduleGather{Skill: "night-shift"}, Problem: "no profile odrade"}, now)
	want = ScheduleRow{ID: "S3", When: "in 2h", Action: "gather night-shift", Problem: "no profile odrade"}
	if got != want {
		t.Errorf("a gather once:\n got %+v\nwant %+v", got, want)
	}
}

// A proposal's caption names its agent, its repository or home, its kind,
// its rounds and when its gather ran.
func TestProposalCaption(t *testing.T) {
	var d Data
	d.SetProfiles(d.Begin(), []proto.Profile{{ID: "vera", Name: "Vera"}})
	p := proposal(1, 1, proto.ProposalProposed)
	if got, want := d.ProposalCaption(p, pocketdtest.Now), "Vera · frostyard/nsl · ci · 10 rounds · gathered today 21:00"; got != want {
		t.Errorf("caption %q, want %q", got, want)
	}
	p.CI = true
	if got, want := d.ProposalCaption(p, pocketdtest.Now), "Vera · frostyard/nsl · ci · 10 rounds · needs green CI · gathered today 21:00"; got != want {
		t.Errorf("caption %q, want %q", got, want)
	}
	p.CI = false
	p.Repository, p.Kind, p.MaxRounds, p.GatheredAt = "", "", 1, time.Time{}
	if got, want := d.ProposalCaption(p, pocketdtest.Now), "Vera · home · 1 round"; got != want {
		t.Errorf("caption %q, want %q", got, want)
	}
}

// launchBodies are the bodies of the launches the store sent, failed ones
// included.
func launchBodies(t *testing.T, h *harness) []proto.CreateLaunch {
	t.Helper()
	var out []proto.CreateLaunch
	for _, r := range h.srv.RequestsTo(http.MethodPost, proto.PathLaunches) {
		var body proto.CreateLaunch
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

// Picks are kept in board order, only of proposals on the board, and go
// once their proposal leaves it: one launched elsewhere is never sent, and
// one that comes back is not picked again unseen.
func TestPicksArePrunedToTheBoard(t *testing.T) {
	h := newHarness(t)
	for _, p := range []proto.Proposal{proposal(1, 2, proto.ProposalProposed), proposal(2, 1, proto.ProposalProposed),
		proposal(3, 3, proto.ProposalProposed), proposal(4, 4, proto.ProposalLaunched)} {
		h.srv.AddProposal(p)
	}
	h.connect(t)
	h.store.TogglePick(1)
	h.store.TogglePick(3)
	h.store.TogglePick(2)
	h.store.TogglePick(4) // launched: not on the board
	h.store.TogglePick(99)
	if got := h.store.Picks(); !slices.Equal(got, []int64{2, 1, 3}) {
		t.Fatalf("picks %v, want board order [2 1 3]", got)
	}
	h.store.TogglePick(3)
	if h.store.Picked(3) || !slices.Equal(h.store.Picks(), []int64{2, 1}) {
		t.Errorf("unpicking #3 left %v", h.store.Picks())
	}
	launched := proposal(1, 2, proto.ProposalLaunched)
	h.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &launched})
	h.loop.RunUntil(t, func() bool { p, _ := h.store.Data.Proposal(1); return p.State == proto.ProposalLaunched })
	if got := h.store.Picks(); !slices.Equal(got, []int64{2}) {
		t.Errorf("picks %v after #1 was launched elsewhere, want [2]", got)
	}
	// Its launch skipped it: it is back on the board, and not picked.
	back := proposal(1, 2, proto.ProposalProposed)
	h.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &back})
	h.loop.RunUntil(t, func() bool { p, _ := h.store.Data.Proposal(1); return p.State == proto.ProposalProposed })
	if h.store.Picked(1) || !slices.Equal(h.store.Picks(), []int64{2}) {
		t.Errorf("a proposal back on the board came back picked: %v", h.store.Picks())
	}
}

// A launch that failed keeps its request: pressed again with the same
// picks and chooser, later, it sends the same proposals, end and request
// id. A changed pick is a new request, its end from the clock now; its
// success clears the picks, and the proposals leave the board.
func TestALaunchRetriedUnchangedKeepsItsRequestAndEnd(t *testing.T) {
	h := newHarness(t)
	for _, p := range []proto.Proposal{proposal(1, 1, proto.ProposalProposed), proposal(2, 2, proto.ProposalProposed),
		proposal(3, 3, proto.ProposalProposed)} {
		h.srv.AddProposal(p)
	}
	h.connect(t)
	h.store.TogglePick(2)
	h.store.TogglePick(1)
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" })
	// Ten minutes on, the same press goes again unchanged.
	h.now = h.now.Add(10 * time.Minute)
	if end, err := h.store.PlannedEnd(""); err != nil || !end.Equal(pocketdtest.Now.Add(4*time.Hour)) {
		t.Errorf("the planned end of the retry is %v, %v; want the first try's", end, err)
	}
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" && !h.store.Launching().Busy })
	// A changed pick is a new launch, from now.
	h.store.TogglePick(3)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.BoardProposals()) == 0 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 3 {
		t.Fatalf("launches sent %+v, want 3", got)
	}
	first, retry, changed := got[0], got[1], got[2]
	if first.RequestID == "" || retry.RequestID != first.RequestID || !retry.End.Equal(first.End) || !slices.Equal(retry.Proposals, first.Proposals) {
		t.Errorf("the retry sent %+v, want the first try unchanged: %+v", retry, first)
	}
	if !slices.Equal(first.Proposals, []int64{1, 2}) || !first.End.Equal(pocketdtest.Now.Add(4*time.Hour)) {
		t.Errorf("the first try sent %+v, want [1 2] in board order, 4 hours from the clock", first)
	}
	if changed.RequestID == first.RequestID || !slices.Equal(changed.Proposals, []int64{1, 2, 3}) ||
		!changed.End.Equal(pocketdtest.Now.Add(10*time.Minute+4*time.Hour)) {
		t.Errorf("the changed launch sent %+v, want a new id, [1 2 3], 4 hours from now", changed)
	}
	if p := h.store.Launching(); p.Busy || p.Err != "" || len(h.store.Picks()) != 0 || len(h.store.picks) != 0 {
		t.Errorf("after the launch: %+v, picks %v", p, h.store.picks)
	}
	if l := h.store.Launched(); l == nil || len(l.Items) != 3 {
		t.Errorf("launched %+v", l)
	}
	if _, kept := h.store.ids.kept[launchKey]; kept || h.store.launchKept != nil {
		t.Error("a launch that succeeded keeps its request id")
	}
}

// A launch whose reply was lost is retried with its request id, and
// pocketd answers it with the launch it made: none is made twice. Its
// proposals' events, which leave the board, do not settle it: the answer
// to the same request does (Codex review of #58, round 6).
func TestALaunchWhoseReplyWasLostIsMadeOnce(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.srv.AddProposal(proposal(2, 2, proto.ProposalProposed))
	h.connect(t)
	h.store.TogglePick(1)
	release := h.srv.HoldAndLoseAnswerOnce(http.MethodPost, proto.PathLaunches)
	h.store.LaunchPicks("07:00")
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, proto.PathLaunches) || len(h.srv.Launches()) == 1 })
	release()
	// The reply and the proposal's event may come in either order.
	h.loop.RunUntil(t, func() bool {
		p, _ := h.store.Data.Proposal(1)
		return !h.store.Launching().Busy && p.State == proto.ProposalLaunched
	})
	h.settle(t)
	if len(h.srv.Launches()) != 1 {
		t.Fatalf("launches %+v", h.srv.Launches())
	}
	// #1 left the board with its event, which does not say which request
	// launched it: the launch is offered again, as it was.
	if !h.store.LaunchUncertain() || h.store.Launching().Err == "" || !slices.Equal(h.store.PlannedPicks("07:00"), []int64{1}) {
		t.Fatalf("a launch whose answer was lost: %+v, uncertain %v", h.store.Launching(), h.store.LaunchUncertain())
	}
	h.store.LaunchPicks("07:00")
	h.loop.RunUntil(t, func() bool { return h.store.Launched() != nil })
	h.settle(t)
	if got := h.store.Launching(); got.Err != "" || h.store.LaunchUncertain() || len(h.srv.Launches()) != 1 {
		t.Errorf("after the retry: %+v, uncertain %v, launches %d", got, h.store.LaunchUncertain(), len(h.srv.Launches()))
	}
	want := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	got := h.srv.LaunchRequests()
	if len(got) != 2 || got[1].RequestID != got[0].RequestID || !got[0].End.Equal(want) || !got[1].End.Equal(want) {
		t.Errorf("sent %+v, want one launch until 07:00 tomorrow, sent again", got)
	}
}

// pocketd's refusal recorded nothing: the next press is a new request,
// from the clock now.
func TestARefusedLaunchIsANewRequestNextTime(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.srv.AddProposal(proposal(2, 2, proto.ProposalProposed))
	h.connect(t)
	h.store.TogglePick(1)
	h.store.TogglePick(2)
	// Dropped by its supervisor, and the client has not heard yet.
	h.srv.AddProposal(proposal(2, 2, proto.ProposalDropped))
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" })
	h.settle(t)
	if got := h.store.Launching().Err; got != "pocketd refused the launch: #2 is dropped: only a proposed one launches" {
		t.Errorf("err %q", got)
	}
	// The refetch took #2 off the board, and out of the picks.
	if got := h.store.Picks(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("picks %v", got)
	}
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return len(h.srv.Launches()) == 1 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 2 || got[1].RequestID == got[0].RequestID || !slices.Equal(got[1].Proposals, []int64{1}) {
		t.Errorf("sent %+v", got)
	}
}

// A changed end choice is a new request, and a bad clock time sends none.
func TestAChangedEndIsANewLaunch(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.connect(t)
	h.store.TogglePick(1)
	h.store.LaunchPicks("25:00")
	if got := h.store.Launching(); got.Busy || got.Err == "" {
		t.Errorf("a bad clock time: %+v", got)
	}
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" && !h.store.Launching().Busy })
	h.store.LaunchPicks("06:00")
	h.loop.RunUntil(t, func() bool { return len(h.srv.Launches()) == 1 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 2 || got[1].RequestID == got[0].RequestID || !got[1].End.Equal(time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("sent %+v", got)
	}
}

// A drop sends the rev the board shows; tried again after a failure it
// keeps its request id, and its reply takes the proposal off the board. A
// stale rev is pocketd's final word, and the proposals are fetched again.
func TestADropTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	p := proposal(1, 1, proto.ProposalProposed)
	p.Rev = 3
	h.srv.AddProposal(p)
	h.srv.AddProposal(proposal(2, 2, proto.ProposalProposed))
	h.connect(t)
	path := proto.Path(proto.PathDropProposal, 1)
	h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
	h.store.DropProposal(1, 3)
	h.loop.RunUntil(t, func() bool { return h.store.Dropping(1).Err != "" })
	h.store.DropProposal(1, 3)
	h.loop.RunUntil(t, func() bool { return !slices.Contains(proposalIDs(h.store.Data.BoardProposals()), 1) })
	h.settle(t)
	var ids []string
	for _, r := range h.srv.RequestsTo(http.MethodPost, path) {
		var body proto.DropProposal
		if err := json.Unmarshal(r.Body, &body); err != nil || body.Rev != 3 {
			t.Fatalf("drop body %s", r.Body)
		}
		ids = append(ids, body.RequestID)
	}
	if len(ids) != 2 || ids[0] != ids[1] || ids[0] == "" {
		t.Errorf("request ids %q, want the retry to keep the first id", ids)
	}
	if d := h.store.Dropping(1); d.Busy || d.Err != "" {
		t.Errorf("the drop's state outlived it: %+v", d)
	}
	// Edited since the board showed it: refused, and the board reloads.
	edited := proposal(2, 2, proto.ProposalProposed)
	edited.Rev = 2
	h.srv.AddProposal(edited)
	h.store.DropProposal(2, 1)
	h.loop.RunUntil(t, func() bool { return h.store.Dropping(2).Err != "" })
	h.settle(t)
	if got, _ := h.store.Data.Proposal(2); got.Rev != 2 || got.State != proto.ProposalProposed {
		t.Errorf("after the refusal #2 is %+v, want rev 2 still proposed", got)
	}
	if _, kept := h.store.ids.kept[dropKey(2)]; kept {
		t.Error("a refused drop keeps its request id")
	}
}

// A schedules event carries no list: the store fetches the schedules
// again, and the newest answer shows.
func TestSchedulesAreFetchedAgainOnTheirEvent(t *testing.T) {
	h := newHarness(t)
	next := pocketdtest.Now.Add(time.Hour)
	h.srv.SetSchedules(proto.Schedules{Zone: "Europe/Berlin", Schedules: []proto.Schedule{{ID: "nightly", Source: proto.ScheduleOwner,
		When: "every day 07:00", Repeat: "every day at 07:00", Message: "Plan the day.", Next: &next}}})
	h.connect(t)
	if st, _ := h.store.Data.SchedulesStatus(); st != ListLoaded || h.store.Data.Schedules().Zone != "Europe/Berlin" ||
		len(h.store.Data.Schedules().Schedules) != 1 {
		t.Fatalf("schedules %v %+v", st, h.store.Data.Schedules())
	}
	before := len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules))
	h.srv.SetSchedules(proto.Schedules{Zone: "Europe/Berlin", Schedules: []proto.Schedule{
		{ID: "nightly", Source: proto.ScheduleOwner, When: "every day 07:00", Message: "Plan the day.", Runs: 1},
		{ID: "S1", Source: proto.ScheduleSession, When: "in 2h", Message: "Check CI."},
	}})
	h.srv.Emit(proto.Event{Type: proto.EventSchedules})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Schedules().Schedules) == 2 })
	h.settle(t)
	if got := len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules)); got != before+1 {
		t.Errorf("%d schedules fetches after the event, want %d", got, before+1)
	}
	if got := h.store.Data.Schedules().Schedules[0]; got.Runs != 1 {
		t.Errorf("schedule %+v", got)
	}
	// A fetch begun before another that answered first is dropped.
	older := h.store.Data.BeginSchedules()
	newest := h.store.Data.BeginSchedules()
	h.store.Data.SetSchedules(newest, proto.Schedules{Zone: "UTC"})
	h.store.Data.SetSchedules(older, proto.Schedules{Zone: "Europe/Berlin"})
	if got := h.store.Data.Schedules().Zone; got != "UTC" {
		t.Errorf("an older fetch answering last left zone %q", got)
	}
	h.store.Data.SetSchedulesError(h.store.Data.BeginSchedules(), "timeout")
	if st, err := h.store.Data.SchedulesStatus(); st != ListFailed || err != "timeout" || h.store.Data.Schedules().Zone != "UTC" {
		t.Errorf("a failed fetch: %v %q, schedules %+v", st, err, h.store.Data.Schedules())
	}
}

// A reconnect fetches again the proposals and the schedules that failed
// to load.
func TestAReconnectRetriesTheProposalsAndSchedules(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.srv.FailOnce(http.MethodGet, proto.PathProposals, http.StatusBadGateway)
	h.srv.FailOnce(http.MethodGet, proto.PathSchedules, http.StatusBadGateway)
	h.connect(t)
	if st, _ := h.store.Data.ProposalsStatus(); st != ListFailed {
		t.Fatalf("proposals %v", st)
	}
	if st, _ := h.store.Data.SchedulesStatus(); st != ListFailed {
		t.Fatalf("schedules %v", st)
	}
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) > 0 })
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool {
		p, _ := h.store.Data.ProposalsStatus()
		s, _ := h.store.Data.SchedulesStatus()
		return p == ListLoaded && s == ListLoaded
	})
	if got := proposalIDs(h.store.Data.BoardProposals()); !slices.Equal(got, []int64{1}) {
		t.Errorf("board %v", got)
	}
}

// A launch retried with the same picks, though the board was ordered
// since, resends its request unchanged: the same id, end and order (Codex
// review of #58, round 2).
func TestALaunchRetriedAfterAReorderKeepsItsRequest(t *testing.T) {
	h := newHarness(t)
	for _, p := range []proto.Proposal{proposal(1, 1, proto.ProposalProposed), proposal(2, 2, proto.ProposalProposed)} {
		h.srv.AddProposal(p)
	}
	h.connect(t)
	h.store.TogglePick(1)
	h.store.TogglePick(2)
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" })
	// The supervisor orders #2 first; the owner presses Launch again later.
	moved := proposal(2, 0, proto.ProposalProposed)
	h.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &moved})
	h.loop.RunUntil(t, func() bool { return slices.Equal(h.store.Picks(), []int64{2, 1}) })
	h.now = h.now.Add(10 * time.Minute)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.BoardProposals()) == 0 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 2 || got[1].RequestID != got[0].RequestID || !got[1].End.Equal(got[0].End) || !slices.Equal(got[1].Proposals, got[0].Proposals) {
		t.Fatalf("the launches sent %+v, want the first resent unchanged", got)
	}
}

// A dropped proposal never changes again: an older event or fetch that
// still has it proposed never puts it back on the board (Codex review of
// #58, round 4).
func TestADroppedProposalStaysDropped(t *testing.T) {
	var d Data
	f := d.BeginProposals()
	d.SetProposals(f, []proto.Proposal{proposal(1, 1, proto.ProposalProposed)})
	stale := d.BeginProposals()
	dropped := proposal(1, 1, proto.ProposalDropped)
	d.ProposalDropped(dropped)
	reordered := proposal(1, 0, proto.ProposalProposed)
	d.Apply(proto.Event{Type: proto.EventProposal, Proposal: &reordered})
	if got, _ := d.Proposal(1); got.State != proto.ProposalDropped || len(d.BoardProposals()) != 0 {
		t.Fatalf("after an older event: %+v, board %v", got, proposalIDs(d.BoardProposals()))
	}
	d.SetProposals(stale, []proto.Proposal{proposal(1, 1, proto.ProposalProposed)})
	if got, _ := d.Proposal(1); got.State != proto.ProposalDropped {
		t.Fatalf("after an older fetch: %+v", got)
	}
}

// Schedules events that come while their fetch runs make one more fetch,
// not one each (Codex review of #58, round 4).
func TestSchedulesEventsShareOneFetch(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	before := len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules))
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathSchedules)
	h.srv.Emit(proto.Event{Type: proto.EventSchedules})
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathSchedules) })
	var last proto.Event
	for range 5 {
		last = h.srv.Emit(proto.Event{Type: proto.EventSchedules})
	}
	h.loop.RunUntil(t, func() bool { return h.store.lastSeq == last.Seq })
	release()
	h.settle(t)
	if got := len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules)) - before; got != 2 {
		t.Errorf("%d schedules fetches for 6 events during one fetch, want 2", got)
	}
}

// A launch that failed goes again as it was, though the board took one of
// its picks away since: only the owner's own change of a pick makes a new
// launch (Codex review of #58, round 5).
func TestALaunchRetriedAfterAPickLeftKeepsItsRequest(t *testing.T) {
	h := newHarness(t)
	for _, p := range []proto.Proposal{proposal(1, 1, proto.ProposalProposed), proposal(2, 2, proto.ProposalProposed)} {
		h.srv.AddProposal(p)
	}
	h.connect(t)
	h.store.TogglePick(1)
	h.store.TogglePick(2)
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" })
	// #1's event comes first, as a launch made with a lost reply sends it.
	launched := proposal(1, 1, proto.ProposalLaunched)
	h.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &launched})
	h.loop.RunUntil(t, func() bool { return slices.Equal(h.store.Picks(), []int64{2}) })
	if got := h.store.PlannedPicks(""); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("the retry would send %v, want the first try's [1 2]", got)
	}
	h.now = h.now.Add(10 * time.Minute)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodPost, proto.PathLaunches)) == 2 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 2 || got[1].RequestID != got[0].RequestID || !got[1].End.Equal(got[0].End) || !slices.Equal(got[1].Proposals, got[0].Proposals) {
		t.Fatalf("the launches sent %+v, want the first resent unchanged", got)
	}
}

// A drop seen once is final for the sign-in: once a list without the
// dropped proposal took its row away, an older event or a list that has it
// proposed never puts it back on the board (Codex review of #58, round 5).
func TestADroppedProposalStaysOffTheBoardOnceItsRowIsGone(t *testing.T) {
	var d Data
	d.SetProposals(d.BeginProposals(), []proto.Proposal{proposal(1, 1, proto.ProposalProposed)})
	d.ProposalDropped(proposal(1, 1, proto.ProposalDropped))
	d.SetProposals(d.BeginProposals(), nil)
	if _, ok := d.Proposal(1); ok {
		t.Fatal("a list without #1 kept its row")
	}
	stale := proposal(1, 0, proto.ProposalProposed)
	d.Apply(proto.Event{Type: proto.EventProposal, Proposal: &stale})
	if got := proposalIDs(d.BoardProposals()); len(got) != 0 {
		t.Fatalf("an older event put %v back on the board", got)
	}
	d.SetProposals(d.BeginProposals(), []proto.Proposal{proposal(1, 1, proto.ProposalProposed)})
	if got := proposalIDs(d.BoardProposals()); len(got) != 0 {
		t.Fatalf("a list put %v back on the board", got)
	}
}

// A schedules answer an event came after is not shown: the fetch the event
// asked for answers in its place (Codex review of #58, round 5).
func TestASchedulesAnswerOlderThanItsEventIsNotShown(t *testing.T) {
	h := newHarness(t)
	h.srv.SetSchedules(proto.Schedules{Zone: "UTC"})
	h.connect(t)
	before := len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules))
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathSchedules)
	h.srv.SetSchedules(proto.Schedules{Zone: "Europe/Berlin"})
	h.srv.Emit(proto.Event{Type: proto.EventSchedules})
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathSchedules) })
	h.srv.SetSchedules(proto.Schedules{Zone: "America/New_York"})
	last := h.srv.Emit(proto.Event{Type: proto.EventSchedules})
	h.loop.RunUntil(t, func() bool { return h.store.lastSeq == last.Seq })
	again := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathSchedules)
	release()
	// The second fetch goes once the first answer was handled.
	h.loop.RunUntil(t, func() bool {
		return len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules)) == before+2 && h.srv.Holding(http.MethodGet, proto.PathSchedules)
	})
	if got := h.store.Data.Schedules().Zone; got != "UTC" {
		t.Errorf("the answer older than the event was shown: zone %q", got)
	}
	again()
	h.settle(t)
	if got := h.store.Data.Schedules().Zone; got != "America/New_York" {
		t.Errorf("zone %q after the fetch the event asked for", got)
	}
}

// A schedule's clock times are its zone's: one in another zone than the
// desktop's says which; next and last are the desktop's (Codex review of
// #58, round 5).
func TestAScheduleInAnotherZoneSaysWhich(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zone data:", err)
	}
	now := pocketdtest.Now.In(ny)
	for _, c := range []struct{ zone, want string }{
		{"Europe/Berlin", "every day at 07:00 (Europe/Berlin)"},
		{"America/New_York", "every day at 07:00"},
		{"", "every day at 07:00"},
	} {
		got := ScheduleRowOf(proto.Schedule{ID: "nightly", When: "every day 07:00", Repeat: "every day at 07:00", Zone: c.zone, Message: "Plan."}, now)
		if got.When != c.want {
			t.Errorf("zone %q: when %q, want %q", c.zone, got.When, c.want)
		}
	}
}

// A launch that never reached pocketd stays to be pressed again though the
// supervisor dropped its picks meanwhile: pocketd's refusal of the same
// request says it was not made (Codex review of #58, round 6).
func TestALaunchWhosePicksLeftWaitsForItsAnswer(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.connect(t)
	h.store.TogglePick(1)
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" })
	dropped := proposal(1, 1, proto.ProposalDropped)
	h.srv.AddProposal(dropped)
	h.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &dropped})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.BoardProposals()) == 0 })
	h.settle(t)
	if !h.store.LaunchUncertain() || h.store.Launching().Err == "" {
		t.Fatalf("the launch's outcome was taken from the board: %+v", h.store.Launching())
	}
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return !h.store.LaunchUncertain() })
	h.settle(t)
	if got := h.store.Launching().Err; !strings.HasPrefix(got, "pocketd refused the launch") {
		t.Errorf("after the retry: %q, want pocketd's refusal", got)
	}
	if got := launchBodies(t, h); len(got) != 2 || got[1].RequestID != got[0].RequestID {
		t.Errorf("sent %+v, want the same request twice", got)
	}
}

// A change of the end chooser is the owner's: switched away and back, a
// launch that failed goes as a new request, its end from the clock then
// (Codex review of #58, round 6).
func TestAChangedEndChoiceIsANewLaunch(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.connect(t)
	h.store.TogglePick(1)
	h.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return h.store.Launching().Err != "" })
	h.now = h.now.Add(time.Hour)
	h.store.LaunchEndEdited() // Until
	h.store.LaunchEndEdited() // and back to In 4 hours
	if end, err := h.store.PlannedEnd(""); err != nil || !end.Equal(h.now.Add(4*time.Hour)) {
		t.Errorf("the planned end %v, %v; want 4 hours from now", end, err)
	}
	h.store.LaunchPicks("")
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodPost, proto.PathLaunches)) == 2 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 2 || got[1].RequestID == got[0].RequestID || !got[1].End.Equal(h.now.Add(4*time.Hour)) {
		t.Fatalf("sent %+v, want a new request ending 4 hours from now", got)
	}
}

// A schedules answer begun before a schedules event is not shown, though
// another fetch of the schedules still runs (Codex review of #58, round 6).
func TestASchedulesAnswerOlderThanItsEventIsNotShownBesideAnotherFetch(t *testing.T) {
	h := newHarness(t)
	h.srv.SetSchedules(proto.Schedules{Zone: "UTC"})
	h.connect(t)
	before := len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules))
	h.srv.SetSchedules(proto.Schedules{Zone: "Europe/Berlin"})
	first := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathSchedules)
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathSchedules) })
	second := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathSchedules)
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodGet, proto.PathSchedules)) == before+2 })
	h.srv.SetSchedules(proto.Schedules{Zone: "America/New_York"})
	last := h.srv.Emit(proto.Event{Type: proto.EventSchedules})
	h.loop.RunUntil(t, func() bool { return h.store.lastSeq == last.Seq })
	first()
	h.loop.RunUntil(t, func() bool { return h.store.InFlight() == 1 })
	if got := h.store.Data.Schedules().Zone; got != "UTC" {
		t.Errorf("an answer older than the event was shown beside another fetch: zone %q", got)
	}
	second()
	h.settle(t)
	if got := h.store.Data.Schedules().Zone; got != "America/New_York" {
		t.Errorf("zone %q after the fetch the event asked for", got)
	}
}

// Zones whose clocks agree now but part before a schedule's next time, as
// Phoenix's and Denver's on the night before spring forward, are named
// (Codex review of #58, round 6).
func TestAScheduleWhoseZonePartsLaterSaysWhich(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("no zone data:", err)
	}
	now := time.Date(2027, 3, 13, 22, 0, 0, 0, denver)
	phoenix, _ := time.LoadLocation("America/Phoenix")
	next := time.Date(2027, 3, 14, 7, 0, 0, 0, phoenix)
	got := ScheduleRowOf(proto.Schedule{ID: "standup", When: "every day 07:00", Repeat: "every day at 07:00", Zone: "America/Phoenix", Message: "Plan.", Next: &next}, now)
	if got.When != "every day at 07:00 (America/Phoenix)" {
		t.Errorf("when %q, want Phoenix named", got.When)
	}
}

// The owner's edit retires a launch's request id: a launch made with a
// lost reply, whose proposal its launch skipped back onto the board, is
// launched anew when the owner picks it again for the same end, never
// answered with the old launch (Codex review of #58, round 7).
func TestAnEditedLaunchIsANewRequestForTheSamePicksAndEnd(t *testing.T) {
	h := newHarness(t)
	h.srv.AddProposal(proposal(1, 1, proto.ProposalProposed))
	h.connect(t)
	h.store.TogglePick(1)
	release := h.srv.HoldAndLoseAnswerOnce(http.MethodPost, proto.PathLaunches)
	h.store.LaunchPicks("07:00")
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, proto.PathLaunches) || len(h.srv.Launches()) == 1 })
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.Launching().Busy })
	h.settle(t)
	// The launch's event takes #1 off the board, and its pick with it,
	// before #1 comes back: until then the board still shows #1 picked,
	// and the toggle below would take that old pick away
	// (frostyard/pocketgo#76).
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.BoardProposals()) == 0 })
	// Its launch skipped #1: it is back on the board.
	back := proposal(1, 1, proto.ProposalProposed)
	h.srv.AddProposal(back)
	h.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &back})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.BoardProposals()) == 1 })
	h.store.TogglePick(1)
	h.store.LaunchPicks("07:00")
	h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodPost, proto.PathLaunches)) == 2 })
	h.settle(t)
	got := launchBodies(t, h)
	if len(got) != 2 || got[1].RequestID == got[0].RequestID || len(h.srv.Launches()) != 2 {
		t.Fatalf("sent %+v, launches %d: want a new request and a second launch", got, len(h.srv.Launches()))
	}
}

// pocketd's zone given as Local is its host's, which the desktop cannot
// read: the board says so, never taking it for the desktop's own (Codex
// review of #58, round 7).
func TestAScheduleInPocketdsLocalZoneSaysSo(t *testing.T) {
	now := pocketdtest.Now.In(time.Local)
	got := ScheduleRowOf(proto.Schedule{ID: "nightly", When: "every day 07:00", Repeat: "every day at 07:00", Zone: "Local", Message: "Plan."}, now)
	if got.When != "every day at 07:00 (pocketd's time)" {
		t.Errorf("when %q", got.When)
	}
}

// A redraw of many schedules of one zone loads the zone once, and compares
// the clocks once for the hour and the next time, not for each row each
// time (Codex review of #58, round 7).
func TestScheduleRowsLoadTheirZoneOnce(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Berlin"); err != nil {
		t.Skip("no zone data:", err)
	}
	loads, compares := 0, 0
	defer func(load func(string) (*time.Location, error), compare func(*time.Location, *time.Location, time.Time, *time.Time) bool) {
		loadLocation, compareClocks = load, compare
	}(loadLocation, compareClocks)
	loadLocation = func(name string) (*time.Location, error) {
		loads++
		return time.LoadLocation(name)
	}
	compareClocks = func(loc, here *time.Location, now time.Time, next *time.Time) bool {
		compares++
		return clocksAgree(loc, here, now, next)
	}
	zones.mu.Lock()
	zones.same, zones.loaded = nil, nil
	zones.mu.Unlock()
	now := pocketdtest.Now
	row := func(i int, next *time.Time) {
		ScheduleRowOf(proto.Schedule{ID: "s" + strconv.Itoa(i), When: "every day 07:00", Repeat: "every day at 07:00", Zone: "Europe/Berlin", Message: "Plan.", Next: next}, now)
	}
	for range 3 {
		for i := range 50 {
			row(i, nil)
		}
	}
	if loads != 1 || compares != 1 {
		t.Errorf("%d zone loads and %d comparisons for 3 redraws of 50 rows alike, want 1 and 1", loads, compares)
	}
	for i := range 50 {
		next := now.Add(time.Duration(i+1) * time.Hour)
		row(i, &next)
	}
	if loads != 1 {
		t.Errorf("%d zone loads for rows of one zone with their own next times, want 1", loads)
	}
}
