package nsgoclient

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// seedNightShift gives the fake pocketd a board of proposals, one of them
// launched already, and the schedules: a gather, a message, a
// supervisor's own and one with a problem. It is the MyGo client's
// seedNightShift.
func seedNightShift(s *pocketdtest.Server) {
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	s.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan", Role: "specialist", Home: "/home/team/agents/duncan"})
	proposal := func(id int64, order int, agent, repo, title, kind string, rounds int, note string) proto.Proposal {
		return proto.Proposal{ID: id, Gather: 3, GatheredAt: at(-2 * time.Hour), Agent: agent, Repository: repo, Title: title,
			Task: "Do it.", StopWhen: "it is done", MaxRounds: rounds, Kind: kind, Note: note, Order: order, Rev: 1,
			State: proto.ProposalProposed, By: agent, CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-2 * time.Hour)}
	}
	s.AddProposal(proposal(12, 1, "vera", "frostyard/nsl", "Pin the CI runner image", "ci-sweep", 12,
		"The arm64 job flakes on the 2-year-old image; pinning it should end the flakes."))
	s.AddProposal(proposal(14, 2, "duncan", "", "Write the release notes for 0.9", "docs", 8, ""))
	thirteen := proposal(13, 3, "vera", "frostyard/snow", "Find why snow fails to boot", "ci-sweep", 20, "")
	thirteen.Rev = 2
	s.AddProposal(thirteen)
	launched := proposal(11, 0, "vera", "frostyard/nsl", "Bump Go to 1.27", "ci-sweep", 10, "")
	launched.State, launched.Launch = proto.ProposalLaunched, 1
	s.AddProposal(launched)
	tomorrow21, tomorrow7, soon := at(23*time.Hour), at(9*time.Hour), at(2*time.Hour)
	s.SetSchedules(proto.Schedules{Zone: "UTC", Schedules: []proto.Schedule{
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

// board opens the work board of a client signed in to s.
func board(t *testing.T, s *pocketdtest.Server, opts ...nsgotest.Option) *nsgotest.App {
	t.Helper()
	app := signedInApp(t, s, 1230, 1000, opts...)
	app.Press("sidebar-panel >> text:Work board")
	return app
}

// inOrder checks that texts draw top to bottom in that order.
func inOrder(t *testing.T, app *nsgotest.App, texts ...string) {
	t.Helper()
	last := float32(-1)
	for _, s := range texts {
		y := float32(app.Find("text:" + s).Bounds().Origin.Y)
		if y <= last {
			t.Errorf("%s draws at %.0f, above the text before it at %.0f", s, y, last)
		}
		last = y
	}
}

// launch presses Launch picks (n), and confirms it.
func launch(app *nsgotest.App, n string) {
	app.Press("label:Launch picks (" + n + ")")
	app.Press("role:alertdialog >> role:button Launch")
}

// The board shows the night shift: the proposals still proposed, in board
// order, with who proposed them, where and when, and their notes; the end
// chooser, which opens on Until the default clock time and says the end;
// and the schedules, a row each. TestTheBoardsNightShiftSnapshot holds
// this for the MyGo client.
func TestTheBoardsNightShift(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.WaitFor(`text:"#12 · Pin the CI runner image"`, 5*time.Second)
	for _, says := range []string{`"#14 · Write the release notes for 0.9"`, `"#13 · Find why snow fails to boot"`,
		`"Vera · frostyard/nsl · ci-sweep · 12 rounds · gathered today 20:00"`, `"Duncan · home · docs · 8 rounds · gathered today 20:00"`,
		`"The arm64 job flakes on the 2-year-old image; pinning it should end the flakes."`,
		`"Until"`, `"In 4 hours"`, `"Ends tomorrow 06:45"`, `"Launch picks (0)"`,
		`"nightly-gather"`, `"every weekday at 21:00"`, `"next tomorrow 21:00"`, `"gather night-shift"`, `"last sent today 21:00"`,
		`"standup"`, `"Plan the day: read the night's reports and brief the team."`,
		`"last missed today 07:00: profile odrade keeps no standing session"`,
		`"S2"`, `"in 2h"`, `"next tomorrow 00:00"`, `"when: "blursday" is no day"`} {
		app.Find("text:" + says)
	}
	if app.Exists("text:Bump Go to 1.27") || app.Exists("text:#11") {
		t.Error("a launched proposal shows on the board")
	}
	if got := app.Find("label:Until time").Text; got != proto.DefaultLaunchUntil {
		t.Errorf("the end chooser opens on Until %q, want %q", got, proto.DefaultLaunchUntil)
	}
	inOrder(t, app, "nightly-gather", "standup", "S2", "weekly",
		"#12 · Pin the CI runner image", "#14 · Write the release notes for 0.9", "#13 · Find why snow fails to boot", "Launch picks (0)")
	if app.Enabled("label:Launch picks (0)") {
		t.Error("Launch picks (0) is enabled")
	}
	app.Press("label:Pick #12")
	app.Press("label:Pick #13")
	app.WaitFor("label:Launch picks (2)", 3*time.Second)
	if !app.Enabled("label:Launch picks (2)") {
		t.Error("Launch picks (2) is disabled")
	}
	app.Find("label:Unpick #12")
	app.Find("label:Pick #14")
	app.Screenshot("work-board-night-shift")
}

// Picking two proposals and launching them launches them in board order,
// whatever order they were picked in, until the next 06:45; they leave
// the board, and the section says what launched.
// TestLaunchingPicksPostsThemInBoardOrder holds this for the MyGo client;
// here a confirmation names the proposals and the end first.
func TestLaunchingPicksPostsThemInBoardOrder(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.Press("label:Pick #13")
	app.Press("label:Pick #12")
	app.Press("label:Launch picks (2)")
	app.WaitFor(`role:alertdialog >> text:"Launch 2 proposals?"`, 3*time.Second)
	app.Find(`role:alertdialog >> text:"Launch #12 and #13 as shifts from the supervisor's standing session, until tomorrow 06:45."`)
	if len(s.LaunchRequests()) != 0 {
		t.Fatal("the launch went before the owner confirmed it")
	}
	// The answer is held: the section says the launch is on its way, and
	// takes no pick, drop or second launch meanwhile.
	release := s.HoldAnswerOnce(http.MethodPost, proto.PathLaunches)
	app.Press("role:alertdialog >> role:button Launch")
	app.WaitFor(`text:"Launching 2 proposals…"`, 5*time.Second)
	for _, control := range []string{"label:Pick #14", "label:Drop #14", "label:Until time"} {
		if app.Enabled(control) {
			t.Errorf("%s is enabled while the launch is on its way", control)
		}
	}
	release()
	app.WaitFor(`text:"Launched L1: 2 proposals until tomorrow 06:45, from the supervisor's standing session."`, 5*time.Second)
	got := s.LaunchRequests()
	if len(got) != 1 || got[0].RequestID == "" || !slices.Equal(got[0].Proposals, []int64{12, 13}) ||
		!got[0].End.Equal(time.Date(2026, 11, 19, 6, 45, 0, 0, time.UTC)) {
		t.Fatalf("launched %+v, want #12 then #13 until tomorrow 06:45", got)
	}
	app.Find(`text:"#14 · Write the release notes for 0.9"`)
	app.Find("label:Launch picks (0)")
	app.WaitForGone("text:Pin the CI runner image", 5*time.Second)
	if app.Exists("text:Find why snow fails to boot") || app.Exists("text:Launching 2 proposals…") {
		t.Error("a launched proposal stays on the board, or the launch still says it is on its way")
	}
}

// Until with a clock time launches until the next such time; Until with
// none launches nothing, and says what to write: a blank field never
// means 4 hours. TestLaunchingUntilAClockTimeSnapshot holds this for the
// MyGo client.
func TestLaunchingUntilAClockTime(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.Press("label:Pick #14")
	app.WaitFor(`text:"Ends tomorrow 06:45"`, 3*time.Second)
	app.Clear("label:Until time")
	app.WaitFor(`text:"Write the clock time the launch ends at, as 07:30."`, 3*time.Second)
	if app.Enabled("label:Launch picks (1)") {
		t.Error("Launch picks (1) is enabled with no clock time")
	}
	// What is no clock time says so, and launches nothing.
	app.Type("label:Until time", "25:99")
	app.WaitFor(`text:""25:99" is no clock time: write HH:MM on the 24-hour clock, as 07:30."`, 3*time.Second)
	if app.Enabled("label:Launch picks (1)") {
		t.Error("Launch picks (1) is enabled with what is no clock time")
	}
	app.Clear("label:Until time")
	app.Type("label:Until time", "6:30")
	app.WaitFor(`text:"Ends tomorrow 06:30"`, 3*time.Second)
	launch(app, "1")
	app.WaitFor(`text:"Launched L1: 1 proposal until tomorrow 06:30, from the supervisor's standing session."`, 5*time.Second)
	want := time.Date(2026, 11, 19, 6, 30, 0, 0, time.UTC)
	if got := s.LaunchRequests(); len(got) != 1 || !slices.Equal(got[0].Proposals, []int64{14}) || !got[0].End.Equal(want) {
		t.Fatalf("launched %+v, want #14 until %v", got, want)
	}
}

// The hours segment launches 4 hours from now, and says so before the
// launch. TestLaunchingWithTheHoursSegment holds this for the MyGo client.
func TestLaunchingWithTheHoursSegment(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.Press("label:Pick #14")
	app.Press("text:In 4 hours")
	app.WaitFor(`text:"Ends tomorrow 02:00"`, 3*time.Second)
	if app.Exists("label:Until time") {
		t.Error("the hours segment shows the clock time's field")
	}
	launch(app, "1")
	app.WaitFor(`text:"Launched L1: 1 proposal until tomorrow 02:00, from the supervisor's standing session."`, 5*time.Second)
	if got := s.LaunchRequests(); len(got) != 1 || !slices.Equal(got[0].Proposals, []int64{14}) || !got[0].End.Equal(pocketdtest.Now.Add(4*time.Hour)) {
		t.Fatalf("launched %+v, want #14 until four hours from now", got)
	}
}

// Drop, after its confirmation, posts the rev the board shows, and the
// proposal leaves the board. TestDroppingAProposalPostsItsRev holds this
// for the MyGo client, which asks no confirmation.
func TestDroppingAProposalPostsItsRev(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	drops := func() []pocketdtest.Request {
		return s.RequestsTo(http.MethodPost, proto.Path(proto.PathDropProposal, 13))
	}
	app.Press("label:Drop #13")
	app.WaitFor(`role:alertdialog >> text:"Drop #13?"`, 3*time.Second)
	if len(drops()) != 0 {
		t.Fatal("the drop went before the owner confirmed it")
	}
	// A drop that failed says so, and goes again at the next press.
	s.FailOnce(http.MethodPost, proto.Path(proto.PathDropProposal, 13), http.StatusBadGateway)
	app.Press(`role:alertdialog >> role:button "Drop #13"`)
	app.WaitFor("text:Dropping #13 failed: ", 5*time.Second)
	app.Press("label:Drop #13")
	app.Press(`role:alertdialog >> role:button "Drop #13"`)
	app.WaitForGone("text:Find why snow fails to boot", 5*time.Second)
	reqs := drops()
	var sent proto.DropProposal
	if len(reqs) != 2 || json.Unmarshal(reqs[1].Body, &sent) != nil || sent.RequestID == "" || sent.Rev != 2 {
		t.Fatalf("the drop sent %+v", reqs)
	}
	if p, _ := s.Proposal(13); p.State != proto.ProposalDropped {
		t.Fatalf("proposal #13 is %s", p.State)
	}
	app.Find(`text:"#12 · Pin the CI runner image"`)
}

// With nothing in them, the night shift's sections say so, and what adds
// a proposal and a schedule; while their lists load, they say that.
// TestTheNightShiftShowsWithNoThreadsSnapshot holds this for the MyGo
// client.
func TestTheNightShiftSaysWhenItHoldsNothing(t *testing.T) {
	s := pocketdtest.New()
	releaseProposals := s.HoldAnswerOnce(http.MethodGet, proto.PathProposals)
	releaseSchedules := s.HoldAnswerOnce(http.MethodGet, proto.PathSchedules)
	app := board(t, s)
	app.WaitFor(`text:"Loading the proposals…"`, 5*time.Second)
	app.Find(`text:"Loading the schedules…"`)
	if app.Exists("text:No proposal waits.") || app.Exists("text:No schedule.") {
		t.Error("a section says it holds nothing while its list loads")
	}
	releaseProposals()
	releaseSchedules()
	app.WaitFor(`text:"No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch."`, 5*time.Second)
	app.WaitFor(`text:"No schedule. Your schedules file, a supervisor's team_schedule and a private agent's schedule add them."`, 5*time.Second)
	inOrder(t, app, "No threads yet", "No schedule.", "No proposal waits.")
	if app.Exists("text:Launch picks") || app.Exists("label:Until time") {
		t.Error("an empty board offers a launch")
	}
}

// Two schedules with one id, an owner's and a session's, are two rows.
// TestSchedulesSharingAnIDAreTwoRows holds this for the MyGo client.
func TestSchedulesSharingAnIDAreTwoRows(t *testing.T) {
	s := pocketdtest.New()
	s.SetSchedules(proto.Schedules{Zone: "UTC", Schedules: []proto.Schedule{
		{ID: "S2", Source: proto.ScheduleOwner, Profile: "odrade", When: "every day 07:00", Message: "Plan the day."},
		{ID: "S2", Source: proto.ScheduleSession, Session: 4, When: "in 2h", Message: "Check the job."},
	}})
	app := board(t, s)
	app.WaitFor(`text:"Plan the day."`, 5*time.Second)
	inOrder(t, app, "Plan the day.", "Check the job.")
	if got := len(app.Query(`text:"S2"`)); got != 2 {
		t.Errorf("%d rows are named S2, want 2", got)
	}
}

// drop makes the fake pocketd drop proposals, as a supervisor does, with
// their events.
func dropProposals(s *pocketdtest.Server, ids ...int64) {
	for _, id := range ids {
		p, _ := s.Proposal(id)
		p.State = proto.ProposalDropped
		s.AddProposal(p)
		s.Emit(proto.Event{Type: proto.EventProposal, Proposal: &p})
	}
}

// A launch refused as its last pick left the board still says why.
// TestARefusedLaunchShowsOnAnEmptyBoard holds this for the MyGo client.
func TestARefusedLaunchShowsOnAnEmptyBoard(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.Press("label:Pick #12")
	app.Press("label:Launch picks (1)")
	app.WaitFor("role:alertdialog", 3*time.Second)
	// The supervisor drops #12, #13 and #14 before the owner confirms:
	// the fake changes them with no event, so the board still shows them.
	for _, id := range []int64{12, 13, 14} {
		p, _ := s.Proposal(id)
		p.State = proto.ProposalDropped
		s.AddProposal(p)
	}
	app.Press("role:alertdialog >> role:button Launch")
	app.WaitFor(`text:"pocketd refused the launch: #12 is dropped: only a proposed one launches"`, 5*time.Second)
	app.WaitFor(`text:"No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch."`, 5*time.Second)
	app.Find(`text:"pocketd refused the launch: #12 is dropped: only a proposed one launches"`)
}

// A launch whose answer was lost is offered again on a board its picks
// left, and pocketd's answer to the same request settles it.
// TestAnUncertainLaunchIsOfferedOnAnEmptyBoard holds this for the MyGo
// client.
func TestAnUncertainLaunchIsOfferedOnAnEmptyBoard(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.Press("label:Pick #12")
	s.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	launch(app, "1")
	app.WaitFor("text:Launching failed: ", 5*time.Second)
	dropProposals(s, 12, 13, 14)
	app.WaitForGone("text:Pin the CI runner image", 5*time.Second)
	// The launch to press again, though no pick is left on the board.
	if !app.Enabled("label:Launch picks (1)") {
		t.Fatal("the launch whose answer was lost is not offered again")
	}
	launch(app, "1")
	app.WaitFor(`text:"pocketd refused the launch: #12 is dropped: only a proposed one launches"`, 5*time.Second)
	app.WaitFor(`text:"No proposal waits. When a gather asks the team for work, its proposals show here for you to pick and launch."`, 5*time.Second)
	sent := s.RequestsTo(http.MethodPost, proto.PathLaunches)
	var first, second proto.CreateLaunch
	if len(sent) != 2 || json.Unmarshal(sent[0].Body, &first) != nil || json.Unmarshal(sent[1].Body, &second) != nil ||
		first.RequestID == "" || first.RequestID != second.RequestID {
		t.Fatalf("sent %+v, want the same request twice", sent)
	}
}

// Changing the end chooser makes a launch that failed a new request: the
// picks on the board now, not those it sent, which the board took one of
// since. TestChangingTheEndChooserMakesANewLaunch holds this for the MyGo
// client.
func TestChangingTheEndChooserMakesANewLaunch(t *testing.T) {
	s := pocketdtest.New()
	seedNightShift(s)
	app := board(t, s)
	app.Press("label:Pick #12")
	app.Press("label:Pick #14")
	s.FailOnce(http.MethodPost, proto.PathLaunches, http.StatusBadGateway)
	launch(app, "2")
	app.WaitFor("text:Launching failed: ", 5*time.Second)
	// #12 is launched elsewhere: the failed launch would go again as it was.
	p, _ := s.Proposal(12)
	p.State = proto.ProposalLaunched
	s.AddProposal(p)
	s.Emit(proto.Event{Type: proto.EventProposal, Proposal: &p})
	app.WaitForGone("text:Pin the CI runner image", 5*time.Second)
	app.Find("label:Launch picks (2)")
	app.Press("text:In 4 hours")
	app.WaitFor("label:Launch picks (1)", 3*time.Second)
	if app.Exists("text:Launching failed: ") {
		t.Error("the failure of a launch the owner changed still shows")
	}
	launch(app, "1")
	app.WaitUntil(5*time.Second, func() bool { return len(s.Launches()) == 1 })
	var sent []proto.CreateLaunch
	for _, r := range s.RequestsTo(http.MethodPost, proto.PathLaunches) {
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
