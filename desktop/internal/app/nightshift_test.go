package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// seedNightShift gives the fake pocketd a board of proposals, one of them
// launched already, and the schedules: a gather, a message, a
// supervisor's own and one with a problem.
func (x *testApp) seedNightShift() {
	x.srv.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan", Role: "specialist", Home: "/home/team/agents/duncan"})
	proposal := func(id int64, order int, agent, repo, title, kind string, rounds int, note string) proto.Proposal {
		return proto.Proposal{ID: id, Gather: 3, GatheredAt: at(-2 * time.Hour), Agent: agent, Repository: repo, Title: title,
			Task: "Do it.", StopWhen: "it is done", MaxRounds: rounds, Kind: kind, Note: note, Order: order, Rev: 1,
			State: proto.ProposalProposed, By: agent, CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-2 * time.Hour)}
	}
	x.srv.AddProposal(proposal(12, 1, "vera", "frostyard/nsl", "Pin the CI runner image", "ci-sweep", 12,
		"The arm64 job flakes on the 2-year-old image; pinning it should end the flakes."))
	x.srv.AddProposal(proposal(14, 2, "duncan", "", "Write the release notes for 0.9", "docs", 8, ""))
	thirteen := proposal(13, 3, "vera", "frostyard/snow", "Find why snow fails to boot", "ci-sweep", 20, "")
	thirteen.Rev = 2
	x.srv.AddProposal(thirteen)
	launched := proposal(11, 0, "vera", "frostyard/nsl", "Bump Go to 1.27", "ci-sweep", 10, "")
	launched.State, launched.Launch = proto.ProposalLaunched, 1
	x.srv.AddProposal(launched)
	tomorrow21, tomorrow7, soon := at(23*time.Hour), at(9*time.Hour), at(2*time.Hour)
	x.srv.SetSchedules(proto.Schedules{Zone: "UTC", Schedules: []proto.Schedule{
		{ID: "nightly-gather", Source: proto.ScheduleOwner, Profile: "odrade", When: "every weekday 21:00", Repeat: "every weekday at 21:00",
			Zone: "UTC", Gather: &proto.ScheduleGather{Skill: "night-shift"}, Next: &tomorrow21, Runs: 4,
			Last: &proto.ScheduleTime{At: at(-time.Hour), Outcome: proto.ScheduleSent}},
		{ID: "standup", Source: proto.ScheduleOwner, Profile: "odrade", When: "every day 07:00", Repeat: "every day at 07:00", Zone: "UTC",
			Message: "Plan the day: read the night's reports and brief the team.", Next: &tomorrow7, Runs: 9,
			Last: &proto.ScheduleTime{At: at(-15 * time.Hour), Outcome: proto.ScheduleMissed, Why: "profile odrade keeps no standing session"}},
		{ID: "S2", Source: proto.ScheduleSession, Session: 1, When: "in 2h", Zone: "UTC", Message: "Check whether the arm64 job passed.", Next: &soon},
		{ID: "weekly", Source: proto.ScheduleOwner, Profile: "odrade", When: "every blursday 19:00", Zone: "UTC", Message: "Sum up the week.",
			Problem: `when: "blursday" is no day`},
	}})
}

// The board shows the night shift first: the proposals still proposed,
// in board order, with who proposed them, where and when, then the
// schedules, then the threads.
func TestTheBoardsNightShiftSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.seedNightShift()
	x.tt.SetSize(1280, 2600)
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "Proposals", "#12 · Pin the CI runner image", "#14 · Write the release notes for 0.9", "#13 · Find why snow fails to boot",
		"Vera · frostyard/nsl · ci-sweep · 12 rounds · gathered today 20:00", "Duncan · home · docs · 8 rounds · gathered today 20:00",
		"The arm64 job flakes on the 2-year-old image; pinning it should end the flakes.",
		"In 4 hours", "Until", "Ends tomorrow 02:00", "Launch picks (0)",
		"Schedules", "nightly-gather", "every weekday at 21:00", "next tomorrow 21:00", "gather night-shift", "last sent today 21:00",
		"standup", "Plan the day: read the night's reports and brief the team.",
		"last missed today 07:00: profile odrade keeps no standing session",
		"S2", "in 2h", "next tomorrow 00:00", `when: "blursday" is no day`,
		"Waits for you", "T1 · Fix the flaky arm64 job")
	if x.tt.HasText("Bump Go to 1.27") && x.tt.HasText("#11") {
		t.Error("a launched proposal shows on the board")
	}
	order := []string{"#12 · Pin the CI runner image", "#14 · Write the release notes for 0.9", "#13 · Find why snow fails to boot",
		"Launch picks (0)", "Schedules", "nightly-gather", "standup", "S2", "weekly", "Waits for you"}
	x.inOrder(t, order...)
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Launch picks (0)"); !ok || !b.Disabled {
		t.Errorf("Launch picks (0) is enabled or missing: %+v, found %v", b, ok)
	}
	x.click(t, "Pick #12")
	x.click(t, "Pick #13")
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Launch picks (2)"); !ok || b.Disabled {
		t.Errorf("Launch picks (2) is disabled or missing: %+v, found %v", b, ok)
	}
	x.snapshot(t, "board-night-shift")
}

// inOrder checks that texts draw top to bottom in that order.
func (x *testApp) inOrder(t *testing.T, texts ...string) {
	t.Helper()
	var last float32 = -1
	for _, s := range texts {
		r, ok := x.tt.Find(s)
		if !ok {
			t.Errorf("missing %q in %q", s, x.tt.Texts())
			continue
		}
		if r.Y <= last {
			t.Errorf("%q draws at %.0f, above the text before it at %.0f", s, r.Y, last)
		}
		last = r.Y
	}
}

// Picking two proposals and pressing Launch picks (2) launches them in
// board order, whatever order they were picked in, until 4 hours from the
// clock; they leave the board, and the section says what launched.
func TestLaunchingPicksPostsThemInBoardOrder(t *testing.T) {
	x := newTestApp(t)
	x.seedNightShift()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Pick #13")
	x.click(t, "Pick #12")
	x.hasAll(t, "Launch picks (2)")
	x.click(t, "Launch picks (2)")
	x.until(t, func() bool {
		return len(x.srv.Launches()) == 1 && x.a.st.InFlight() == 0 && len(x.a.st.Data.BoardProposals()) == 1
	})
	got := x.srv.LaunchRequests()
	if len(got) != 1 || got[0].RequestID == "" || !slices.Equal(got[0].Proposals, []int64{12, 13}) ||
		!got[0].End.Equal(pocketdtest.Now.Add(4*time.Hour)) {
		t.Fatalf("launched %+v, want #12 then #13 until 4 hours from the clock", got)
	}
	x.hasAll(t, "#14 · Write the release notes for 0.9", "Launch picks (0)",
		"Launched L1: 2 proposals until tomorrow 02:00, from the supervisor's standing session.")
	if x.tt.HasText("Pin the CI runner image") || x.tt.HasText("Find why snow fails to boot") {
		t.Error("a launched proposal stays on the board")
	}
}

// Until with a clock time launches until the next such time; Until with
// none launches nothing, and says what to write.
func TestLaunchingUntilAClockTimeSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedNightShift()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Pick #14")
	x.click(t, "Until")
	x.hasAll(t, "Write the clock time the launch ends at, as 07:30")
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Launch picks (1)"); !ok || !b.Disabled {
		t.Errorf("Launch picks (1) is enabled with no clock time: %+v, found %v", b, ok)
	}
	x.typeInto(t, "Until time", "6:30")
	x.hasAll(t, "Ends tomorrow 06:30")
	x.snapshot(t, "board-launch-until")
	x.click(t, "Launch picks (1)")
	x.until(t, func() bool { return len(x.srv.Launches()) == 1 && x.a.st.InFlight() == 0 })
	want := time.Date(2026, 11, 19, 6, 30, 0, 0, time.UTC)
	if got := x.srv.LaunchRequests(); len(got) != 1 || !slices.Equal(got[0].Proposals, []int64{14}) || !got[0].End.Equal(want) {
		t.Fatalf("launched %+v, want #14 until %v", got, want)
	}
}

// Drop posts the rev the board shows, and the proposal leaves the board.
func TestDroppingAProposalPostsItsRev(t *testing.T) {
	x := newTestApp(t)
	x.seedNightShift()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Drop #13")
	x.until(t, func() bool {
		p, _ := x.srv.Proposal(13)
		return p.State == proto.ProposalDropped && x.a.st.InFlight() == 0
	})
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathDropProposal, 13))
	var body proto.DropProposal
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.RequestID == "" || body.Rev != 2 {
		t.Fatalf("the drop sent %+v", reqs)
	}
	x.until(t, func() bool { return len(x.a.st.Data.BoardProposals()) == 2 })
	if x.tt.HasText("Find why snow fails to boot") {
		t.Error("a dropped proposal stays on the board")
	}
}

// With no thread yet, the night shift's sections still show, above the
// threads' empty state; with nothing in them, they say so.
func TestTheNightShiftShowsWithNoThreadsSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	x.seedNightShift()
	x.tt.SetSize(1280, 1400)
	x.connect(t)
	x.click(t, "Work Board")
	x.inOrder(t, "Proposals", "#12 · Pin the CI runner image", "Schedules", "nightly-gather", "No threads yet")
	x.snapshot(t, "board-night-shift-no-threads")

	empty := newTestApp(t)
	empty.connect(t)
	empty.click(t, "Work Board")
	empty.inOrder(t, "Proposals", "No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch.",
		"Schedules", "No schedule. Your schedules file and a supervisor's team_schedule add them.", "No threads yet")
	if empty.tt.HasText("Launch picks") {
		t.Error("an empty board offers a launch")
	}
}

// Two schedules with one id, an owner's and a session's, are two rows
// with keys of their own (Codex review of #58, round 2).
func TestSchedulesSharingAnIDAreTwoRows(t *testing.T) {
	x := newTestApp(t)
	x.srv.SetSchedules(proto.Schedules{Zone: "UTC", Schedules: []proto.Schedule{
		{ID: "S2", Source: proto.ScheduleOwner, Profile: "odrade", When: "every day 07:00", Message: "Plan the day."},
		{ID: "S2", Source: proto.ScheduleSession, Session: 4, When: "in 2h", Message: "Check the job."},
	}})
	x.connect(t)
	x.click(t, "Work Board")
	x.inOrder(t, "Schedules", "Plan the day.", "Check the job.")
}

// A launch refused as its last pick left the board still says why
// (Codex review of #58, round 3).
func TestARefusedLaunchShowsOnAnEmptyBoard(t *testing.T) {
	x := newTestApp(t)
	x.seedNightShift()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Pick #12")
	// The supervisor drops #12, #13 and #14 before the owner presses Launch.
	for _, id := range []int64{12, 13, 14} {
		p, _ := x.srv.Proposal(id)
		p.State = proto.ProposalDropped
		x.srv.AddProposal(p)
	}
	x.click(t, "Launch picks (1)")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 && len(x.a.st.Data.BoardProposals()) == 0 })
	x.hasAll(t, "No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch.")
	if !x.tt.HasText("pocketd refused the launch: #12 is dropped: only a proposed one launches") {
		t.Fatal("the refusal is gone from the empty board")
	}
}

// A launch whose answer was lost is offered again on a board its picks
// left, and pocketd's answer to the same request settles it (Codex review
// of #58, round 6).
func TestAnUncertainLaunchIsOfferedOnAnEmptyBoard(t *testing.T) {
	x := newTestApp(t)
	x.seedNightShift()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Pick #12")
	x.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	x.click(t, "Launch picks (1)")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 && x.a.st.Launching().Err != "" })
	for _, id := range []int64{12, 13, 14} {
		p, _ := x.srv.Proposal(id)
		p.State = proto.ProposalDropped
		x.srv.AddProposal(p)
		x.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &p})
	}
	x.until(t, func() bool { return len(x.a.st.Data.BoardProposals()) == 0 && x.a.st.InFlight() == 0 })
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Launch picks (1)"); !ok || b.Disabled {
		t.Fatalf("the launch to press again: %+v, found %v", b, ok)
	}
	x.click(t, "Launch picks (1)")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 && !x.a.st.LaunchUncertain() })
	x.hasAll(t, "No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch.")
	if !x.tt.HasText("pocketd refused the launch: #12 is dropped: only a proposed one launches") {
		t.Fatal("pocketd's answer to the launch is not shown")
	}
}

// Changing the end chooser makes a launch that failed a new request: the
// picks on the board now, not those it sent, which the board took one of
// since (Codex review of #58, round 6).
func TestChangingTheEndChooserMakesANewLaunch(t *testing.T) {
	x := newTestApp(t)
	x.seedNightShift()
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Pick #12")
	x.click(t, "Pick #14")
	x.srv.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	x.click(t, "Launch picks (2)")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 && x.a.st.Launching().Err != "" })
	// #12 is launched elsewhere: the failed launch would go again as it was.
	p, _ := x.srv.Proposal(12)
	p.State = proto.ProposalLaunched
	x.srv.AddProposal(p)
	x.srv.Emit(proto.Event{Type: proto.EventProposal, Proposal: &p})
	x.until(t, func() bool { return len(x.a.st.Data.BoardProposals()) == 2 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Launch picks (2)")
	x.click(t, "Until")
	x.click(t, "In 4 hours")
	x.click(t, "Launch picks (1)")
	x.until(t, func() bool { return len(x.srv.Launches()) == 1 && x.a.st.InFlight() == 0 })
	var sent []proto.CreateLaunch
	for _, r := range x.srv.RequestsTo(http.MethodPost, proto.PathLaunches) {
		var body proto.CreateLaunch
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, body)
	}
	if len(sent) != 2 || sent[1].RequestID == sent[0].RequestID || !slices.Equal(sent[1].Proposals, []int64{14}) {
		t.Fatalf("sent %+v, want a new request for #14 after the end changed", sent)
	}
}
