package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// launchBoard is a running gather of supervisor session 1 (odrade) with
// two proposals of duncan's ask.
func launchBoard(t *testing.T, s *Store) (ProposalRow, ProposalRow) {
	t.Helper()
	if _, err := s.CreateSession(ctx, "odrade", SessionRow{ID: 1, Title: "Odrade", Cwd: "/h", Provider: "p", Model: "m", Profile: "odrade", CreatedBy: "d"}); err != nil {
		t.Fatal(err)
	}
	facts, _, err := s.StartGather(ctx, gatherStart("g"))
	if err != nil {
		t.Fatal(err)
	}
	ask := facts.Asks[0]
	if _, err := s.RecordGatherSession(ctx, ask.SessionKey(), SessionRow{ID: 40, Title: "t", Cwd: "/h", Provider: "p", Model: "m", CreatedBy: "team"}, ask.ID, func(GatherRow) string { return "b" }); err != nil {
		t.Fatal(err)
	}
	receipt := func(ProposalRow) string { return "" }
	a, _, _, err := s.AddProposal(ctx, "p1", ask.ID, team.Proposal{Title: "A", Task: "Do A.", StopWhen: "A done.", MaxRounds: 5}, "duncan", receipt)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, err := s.AddProposal(ctx, "p2", ask.ID, team.Proposal{Title: "B", Task: "Do B.", StopWhen: "B done.", MaxRounds: 7}, "duncan", receipt)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

// A launch is recorded once per key, its proposals launched in the same
// transaction; each item starts as one thread keyed by it, with the
// launch's end as its deadline, and never another; a skipped item's
// proposal goes back on the board; the launch ends when none waits
// (ADR-0034).
func TestALaunchStartsEachItemOnce(t *testing.T) {
	s, _ := open(t)
	a, b := launchBoard(t, s)
	end := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	start := LaunchStart{Key: "u:d:r1", RequestKind: "launch:1,2", SupervisorSession: 1, Profile: "odrade", End: end, By: team.LaunchByOwner, Proposals: []int64{b.ID, a.ID},
		Note: func(f LaunchFacts) string { return "launched " + f.Items[0].Title }}
	facts, launched, replayed, err := s.CreateLaunch(ctx, start)
	if err != nil || replayed || len(facts.Items) != 2 || facts.Items[0].ProposalID != b.ID || facts.Items[0].State != team.LaunchItemWaiting || len(launched) != 2 ||
		launched[0].State != team.ProposalLaunched || launched[0].LaunchID != facts.Launch.ID {
		t.Fatalf("the launch %+v %+v %v %v", facts, launched, replayed, err)
	}
	if note, err := s.Note(ctx, "launch:"+facts.Launch.Owner+":told"); err != nil || note.SessionID != 1 || note.Text != "launched B" {
		t.Fatalf("the note %+v %v", note, err)
	}
	again, _, replayed, err := s.CreateLaunch(ctx, start)
	if err != nil || !replayed || again.Launch.ID != facts.Launch.ID {
		t.Fatalf("again %+v %v %v", again, replayed, err)
	}
	other := start
	other.RequestKind = "launch:1"
	if _, _, _, err := s.CreateLaunch(ctx, other); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("the request id for another launch: %v", err)
	}
	if _, _, _, err := s.CreateLaunch(ctx, LaunchStart{Key: "team:1:2:l1", SupervisorSession: 1, Profile: "odrade", End: end, By: team.LaunchBySupervisor, Proposals: []int64{a.ID}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a launched proposal launched again: %v", err)
	}
	first := facts.Items[0]
	row := ThreadRow{Title: "B", Agent: "duncan", Task: "Do B."}
	thread, got, proposal, started, err := s.StartLaunchItem(ctx, first.ID, row, ShiftRow{Stop: "B done.", MaxRounds: 7})
	if err != nil || !started || thread.Key != first.ThreadKey() || thread.SupervisorSession != 1 || proposal.ThreadID != thread.ID || got.Items[0].State != team.LaunchItemStarted || got.Items[0].ThreadID != thread.ID {
		t.Fatalf("the start %+v %+v %+v %v %v", thread, got, proposal, started, err)
	}
	if shift, err := s.Shift(ctx, thread.ID); err != nil || !shift.Deadline.Equal(end) || shift.MaxRounds != 7 {
		t.Fatalf("its shift %+v %v", shift, err)
	}
	if _, _, _, started, err := s.StartLaunchItem(ctx, first.ID, row, ShiftRow{Stop: "B done.", MaxRounds: 7}); err != nil || started {
		t.Fatalf("the item started again: %v %v", started, err)
	}
	if n, err := s.LaunchThreadsOpen(ctx, facts.Launch.ID); err != nil || n != 1 {
		t.Fatalf("open %d %v", n, err)
	}
	ended, proposal, did, err := s.SkipLaunchItem(ctx, facts.Items[1].ID, "duncan is private and takes no delegation")
	if err != nil || !did || proposal.State != team.ProposalProposed || proposal.LaunchID != 0 || ended.Launch.State != team.LaunchDone || ended.Items[1].State != team.LaunchItemSkipped ||
		!strings.Contains(ended.Items[1].Why, "private") {
		t.Fatalf("the skip %+v %+v %v %v", ended, proposal, did, err)
	}
	if running, err := s.RunningLaunches(ctx); err != nil || len(running) != 0 {
		t.Fatalf("running %+v %v", running, err)
	}
	if list, err := s.LaunchesSince(ctx, time.Time{}); err != nil || len(list) != 1 {
		t.Fatalf("the list %+v %v", list, err)
	}
}

// An item whose launch's end came by the store's clock starts no thread:
// it is skipped, and its proposal goes back on the board (ADR-0034).
func TestALaunchItemPastItsEndStartsNoThread(t *testing.T) {
	s, _ := open(t)
	a, _ := launchBoard(t, s)
	end := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	facts, _, _, err := s.CreateLaunch(ctx, LaunchStart{Key: "team:1:2:l1", SupervisorSession: 1, Profile: "odrade", End: end, By: team.LaunchBySupervisor, Proposals: []int64{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	clock := s.now
	s.now = func() time.Time { return end }
	defer func() { s.now = clock }()
	_, got, proposal, started, err := s.StartLaunchItem(ctx, facts.Items[0].ID, ThreadRow{Title: "A", Agent: "duncan", Task: "Do A."}, ShiftRow{Stop: "A done.", MaxRounds: 5})
	if err != nil || started || got.Items[0].State != team.LaunchItemSkipped || got.Items[0].Why != team.LaunchEndPassed || proposal.State != team.ProposalProposed || got.Launch.State != team.LaunchDone {
		t.Fatalf("past the end %+v %+v %v %v", got, proposal, started, err)
	}
}

// The launch pass reads the running launches by their index.
func TestLaunchQueriesUseTheirIndexes(t *testing.T) {
	s, _ := open(t)
	if plan := planOf(t, s, `SELECT id FROM launches WHERE state = 'running' ORDER BY id`); !strings.Contains(plan, "launches_running") {
		t.Errorf("plan %q does not use launches_running", plan)
	}
}
