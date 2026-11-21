package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// launchBoard gathers proposals from Duncan and Vera for Odrade's board:
// titles[i] is proposal #i+1, Duncan's first, all in their homes.
func launchBoard(t *testing.T, titles ...string) *gatherHarness {
	t.Helper()
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming"})
	duncan, vera := h.askOf(1, "duncan"), h.askOf(1, "vera")
	for i, title := range titles {
		session := duncan.Session
		if i%2 == 1 {
			session = vera.Session
		}
		if _, err := h.server.TeamPropose(ctx, TeamCall{Session: session, Task: 2, CallID: fmt.Sprintf("p%d", i)}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields(title)}); err != nil {
			t.Fatal(err)
		}
	}
	h.server.StartLaunches()
	h.runLaunchPasses()
	return h
}

// launchThreads are the threads the launches started, by their keys.
func (h *gatherHarness) launchThreads() []store.ThreadRow {
	h.t.Helper()
	rows, err := h.store.Threads(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.ThreadRow
	for _, row := range rows {
		if strings.HasPrefix(row.Key, "launch:") {
			out = append(out, row)
		}
	}
	return out
}

// The owner launches picks: they start in order as shift threads
// delegated from the supervisor's standing session, until the launch's
// end, at most three at once, the next as one closes; the supervisor hears
// once; a retry answers as the launch did (ADR-0034).
func TestTheOwnerLaunchesPicks(t *testing.T) {
	h := launchBoard(t, "A", "B", "C", "D")
	ctx := context.Background()
	end := h.clock.Now().Add(4 * time.Hour)
	request := proto.CreateLaunch{RequestID: "r1", Proposals: []int64{3, 1, 2, 4}, End: end}
	launch, err := h.client.Launch(ctx, request)
	if err != nil || launch.Supervisor != h.odrade || launch.Profile != "odrade" || len(launch.Items) != 4 || launch.Items[0].Proposal != 3 || launch.Items[0].State != proto.LaunchItemWaiting || launch.By != proto.LaunchByOwner {
		t.Fatalf("the launch %+v %v", launch, err)
	}
	if p, _ := h.store.Proposal(ctx, 3); p.State != team.ProposalLaunched || p.LaunchID != launch.ID {
		t.Fatalf("a launched proposal %+v", p)
	}
	h.runLaunchPasses()
	threads := h.launchThreads()
	if len(threads) != team.LaunchAtOnce {
		t.Fatalf("%d threads started, want %d", len(threads), team.LaunchAtOnce)
	}
	facts, err := h.store.LaunchFacts(ctx, launch.ID)
	if err != nil || facts.Items[0].State != team.LaunchItemStarted || facts.Items[3].State != team.LaunchItemWaiting {
		t.Fatalf("the items %+v %v", facts.Items, err)
	}
	first, err := h.store.Thread(ctx, facts.Items[0].ThreadID)
	if err != nil || first.SupervisorSession != h.odrade || first.Title != "C" || first.Agent != "duncan" || first.Task != "Do C." {
		t.Fatalf("the first thread %+v %v", first, err)
	}
	if shift, err := h.store.Shift(ctx, first.ID); err != nil || !shift.Deadline.Equal(end.Truncate(time.Millisecond)) || shift.Stop != "C is done." || shift.MaxRounds != team.DefaultShiftRounds {
		t.Fatalf("its shift %+v %v", shift, err)
	}
	h.runScheduled(20)
	if note := h.sentTo(h.odrade, "launch:"); len(note) != 1 || !strings.HasPrefix(note[0], "[launch L1] The owner launched 4 proposals from your board") || !strings.Contains(note[0], "#3 duncan: C") {
		t.Fatalf("the supervisor's note %q", note)
	}
	if _, err := h.client.CloseThread(ctx, first.ID, proto.CloseThread{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	h.runLaunchPasses()
	if facts, _ := h.store.LaunchFacts(ctx, launch.ID); facts.Items[3].State != team.LaunchItemStarted || facts.Launch.State != team.LaunchDone {
		t.Fatalf("the last item once a place freed %+v", facts)
	}
	again, err := h.client.Launch(ctx, request)
	if err != nil || again.ID != launch.ID {
		t.Fatalf("the launch again %+v %v", again, err)
	}
	other := request
	other.Proposals = []int64{3}
	var pe *proto.Error
	if _, err := h.client.Launch(ctx, other); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("the request id for another launch: %v", err)
	}
	list, err := h.client.Launches(ctx)
	if err != nil || len(list) != 1 || list[0].ID != launch.ID {
		t.Fatalf("the launches %+v %v", list, err)
	}
}

// A launch leaves the desk's reserve free: no item starts while the desk
// holds its cap less the reserve (ADR-0034).
func TestALaunchKeepsTheDesksReserve(t *testing.T) {
	h := launchBoard(t, "A", "B", "C")
	ctx := context.Background()
	h.server.deskCap = 4
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1, 2, 3}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runLaunchPasses()
	if n := len(h.launchThreads()); n != 4-team.LaunchReserve {
		t.Fatalf("%d threads started on a desk of 4", n)
	}
}

// Past its end a launch starts nothing more: each item still waiting is
// skipped, and its proposal goes back on the board (ADR-0034).
func TestALaunchEndsAtItsEnd(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	h.server.deskCap = team.LaunchReserve
	launch, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1, 2}, End: h.clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h.runLaunchPasses()
	if n := len(h.launchThreads()); n != 0 {
		t.Fatalf("%d threads started on a full desk", n)
	}
	h.clock.Advance(time.Hour)
	if h.fireLaunchWakes() == 0 {
		t.Fatal("no launch wake at its end")
	}
	facts, err := h.store.LaunchFacts(ctx, launch.ID)
	if err != nil || facts.Launch.State != team.LaunchDone || facts.Items[0].State != team.LaunchItemSkipped || facts.Items[0].Why != team.LaunchEndPassed {
		t.Fatalf("the launch past its end %+v %v", facts, err)
	}
	if p, _ := h.store.Proposal(ctx, 1); p.State != team.ProposalProposed || p.LaunchID != 0 {
		t.Fatalf("the proposal %+v", p)
	}
}

// A start that fails before the specialist may have its brief closes its
// thread with why, and the item never gets another thread (ADR-0034).
func TestAFailedLaunchStartIsNeverRetried(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createErr = errors.New("the engine is full")
	h.engine.mu.Unlock()
	launch, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h.runLaunchPasses()
	h.runScheduled(20)
	h.runLaunchPasses()
	threads := h.launchThreads()
	if len(threads) != 1 || threads[0].State != store.ThreadClosed || !strings.Contains(threads[0].Report, "the engine is full") {
		t.Fatalf("the threads %+v", threads)
	}
	facts, _ := h.store.LaunchFacts(ctx, launch.ID)
	if facts.Items[0].State != team.LaunchItemStarted || facts.Items[0].ThreadID != threads[0].ID || facts.Launch.State != team.LaunchDone {
		t.Fatalf("the item %+v", facts)
	}
}

// A start a stop interrupted after its thread was recorded is finished by
// the next pass, with that thread (ADR-0034).
func TestALaunchFinishesAnInterruptedStart(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	launch, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := h.store.LaunchFacts(ctx, launch.ID)
	thread, _, _, started, err := h.store.StartLaunchItem(ctx, facts.Items[0].ID, store.ThreadRow{Title: "A", Agent: "duncan", Task: "Do A."}, store.ShiftRow{Stop: "A is done.", MaxRounds: 20})
	if err != nil || !started || thread.Session != 0 {
		t.Fatalf("the recorded start %+v %v %v", thread, started, err)
	}
	h.runLaunchPasses()
	threads := h.launchThreads()
	if len(threads) != 1 || threads[0].ID != thread.ID || threads[0].Session == 0 {
		t.Fatalf("the threads %+v", threads)
	}
}

// The supervisor launches rows of its own board with team_launch, and the
// call run again answers as it did; another supervisor's rows and a
// caller that is not a supervisor are refused (ADR-0034).
func TestTheSupervisorLaunches(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	hours := 2.0
	call := TeamCall{Session: h.odrade, Task: 5, CallID: "l1"}
	text, err := h.server.TeamLaunch(ctx, call, team.LaunchCall{Rows: []int64{2, 1}, Hours: &hours})
	if err != nil || !strings.HasPrefix(text, "Launch L1: 2 proposals start in this order") || !strings.Contains(text, "#2 vera: B") {
		t.Fatalf("the launch %q %v", text, err)
	}
	if again, err := h.server.TeamLaunch(ctx, call, team.LaunchCall{Rows: []int64{2, 1}, Hours: &hours}); err != nil || again != text {
		t.Fatalf("the call again %q %v", again, err)
	}
	facts, err := h.store.LaunchByKey(ctx, call.key())
	if err != nil || facts.Launch.By != team.LaunchBySupervisor || !facts.Launch.End.Equal(h.clock.Now().Add(2*time.Hour).Truncate(time.Millisecond)) {
		t.Fatalf("its record %+v %v", facts.Launch, err)
	}
	if note := h.sentTo(h.odrade, "launch:"); len(note) != 0 {
		t.Fatalf("a supervisor's own launch sent it a note: %q", note)
	}
	h.gather("g2", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	if _, err := h.server.TeamLaunch(ctx, TeamCall{Session: h.leto, Task: 6, CallID: "l2"}, team.LaunchCall{Rows: []int64{1}}); err == nil {
		t.Fatal("leto launched odrade's proposal")
	}
	vera := h.askOf(1, "vera")
	if _, err := h.server.TeamLaunch(ctx, TeamCall{Session: vera.Session, Task: 6, CallID: "l3"}, team.LaunchCall{Rows: []int64{1}}); err == nil || !strings.Contains(err.Error(), "only a supervisor") {
		t.Fatalf("an agent's launch: %v", err)
	}
}

// A launch is refused for a rule its proposals or its end break
// (ADR-0034).
func TestALaunchIsRefused(t *testing.T) {
	h := launchBoard(t, "A", "B", "C")
	ctx := context.Background()
	hundred := 100
	for i := int64(1); i <= 3; i++ {
		if _, err := h.server.TeamBoard(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: fmt.Sprintf("e%d", i)}, team.BoardCall{Action: team.BoardEdit, Row: i, Rev: 1, ProposalFields: team.ProposalFields{MaxRounds: &hundred}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.server.TeamBoard(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: "d3"}, team.BoardCall{Action: team.BoardDrop, Row: 3, Rev: 2}); err != nil {
		t.Fatal(err)
	}
	now := h.clock.Now()
	for name, tc := range map[string]struct {
		launch proto.CreateLaunch
		want   string
	}{
		"over the budget": {proto.CreateLaunch{Proposals: []int64{1, 2, 99}, End: now.Add(time.Hour)}, "no proposal #99"},
		"a dropped one":   {proto.CreateLaunch{Proposals: []int64{3}, End: now.Add(time.Hour)}, "dropped"},
		"too far":         {proto.CreateLaunch{Proposals: []int64{1}, End: now.Add(25 * time.Hour)}, "more than 24 hours"},
		"past":            {proto.CreateLaunch{Proposals: []int64{1}, End: now}, "has come"},
		"none":            {proto.CreateLaunch{End: now.Add(time.Hour)}, "1 to 20"},
	} {
		tc.launch.RequestID = name
		var pe *proto.Error
		if _, err := h.client.Launch(ctx, tc.launch); !errors.As(err, &pe) || !strings.Contains(pe.Message, tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	two := 2.0
	// The rounds of #1 and #2 add up to 200: a third proposal would not fit.
	if _, err := h.server.TeamLaunch(ctx, TeamCall{Session: h.odrade, Task: 5, CallID: "l1"}, team.LaunchCall{Rows: []int64{1, 2}, Hours: &two}); err != nil {
		t.Fatalf("the budget whole: %v", err)
	}
	if rows, _ := h.store.LaunchesSince(ctx, time.Time{}); len(rows) != 1 {
		t.Fatalf("refusals made launches: %+v", rows)
	}
}

// The owner drops a proposal at the rev the board shows, once per request
// id; a stale rev is refused (ADR-0034).
func TestTheOwnerDropsAProposal(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	var pe *proto.Error
	if _, err := h.client.DropProposal(ctx, 1, proto.DropProposal{RequestID: "d0", Rev: 7}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("a stale rev: %v", err)
	}
	dropped, err := h.client.DropProposal(ctx, 1, proto.DropProposal{RequestID: "d1", Rev: 1})
	if err != nil || dropped.State != proto.ProposalDropped || dropped.ID != 1 {
		t.Fatalf("the drop %+v %v", dropped, err)
	}
	if again, err := h.client.DropProposal(ctx, 1, proto.DropProposal{RequestID: "d1", Rev: 1}); err != nil || again.State != proto.ProposalDropped {
		t.Fatalf("the drop again %+v %v", again, err)
	}
	if _, err := h.client.DropProposal(ctx, 99, proto.DropProposal{RequestID: "d2", Rev: 1}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeNotFound {
		t.Fatalf("no such proposal: %v", err)
	}
	if list, _ := h.client.Proposals(ctx); len(list) != 1 || list[0].ID != 2 {
		t.Fatalf("the board %+v", list)
	}
}
