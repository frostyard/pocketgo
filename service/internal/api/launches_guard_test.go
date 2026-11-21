package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Guards for the Codex review of #58 (ADR-0034).

// A proposal edited after the launch checked it refuses the launch: the
// rev checked is the rev recorded (round 1).
func TestALaunchRefusesARowEditedWhileItWasMade(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.server.afterLaunchPlan = func() {
		rounds := 100
		if _, err := h.server.TeamBoard(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: "e1"}, team.BoardCall{Action: team.BoardEdit, Row: 1, Rev: 1, ProposalFields: team.ProposalFields{MaxRounds: &rounds}}); err != nil {
			t.Error(err)
		}
	}
	var pe *proto.Error
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict || !strings.Contains(pe.Message, "changed while the launch was made") {
		t.Fatalf("a launch of an edited row: %v", err)
	}
	if rows, _ := h.store.LaunchesSince(ctx, time.Time{}); len(rows) != 0 {
		t.Fatalf("a launch was made: %+v", rows)
	}
}

// A launch whose end comes while it is checked is refused when it would
// be recorded (round 1).
func TestALaunchWhoseEndCameWhileItWasMadeIsRefused(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.server.afterLaunchPlan = func() { h.clock.Advance(time.Hour) }
	var pe *proto.Error
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); !errors.As(err, &pe) || !strings.Contains(pe.Message, "end came") {
		t.Fatalf("a launch past its end: %v", err)
	}
	if p, _ := h.store.Proposal(ctx, 1); p.State != team.ProposalProposed {
		t.Fatalf("the proposal %+v", p)
	}
}

// The standing session a launch is delegated from is a supervisor's, as
// read when it is found or opened (round 1).
func TestALaunchFromADemotedSupervisorIsRefused(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.server.beforeCreateLock = func() { writeOdrade(t, h.dir, "role: supervisor\n", "") }
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err == nil || !strings.Contains(err.Error(), "not a supervisor") {
		t.Fatalf("a launch from a demoted supervisor: %v", err)
	}
}

// A skipped item lets the next one start in the same pass (round 1).
func TestALaunchGoesOnPastASkippedItem(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	launch, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1, 2}, End: h.clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.runLaunchPasses()
	facts, err := h.store.LaunchFacts(ctx, launch.ID)
	if err != nil || facts.Items[0].State != team.LaunchItemSkipped || !strings.Contains(facts.Items[0].Why, "private") || facts.Items[1].State != team.LaunchItemStarted {
		t.Fatalf("the items %+v %v", facts.Items, err)
	}
}

// A pass that cannot read the launches' starting threads runs again,
// whatever launch runs (round 1).
func TestALaunchPassRetriesStartsItCouldNotRead(t *testing.T) {
	h := newGatherHarness(t, "")
	failing := true
	h.server.launchUnstarted = func(ctx context.Context) ([]store.ThreadRow, error) {
		if failing {
			return nil, errors.New("disk I/O error")
		}
		return h.store.UnstartedLaunchThreads(ctx)
	}
	h.server.StartLaunches()
	h.runLaunchPasses()
	failing = false
	h.clock.Advance(launchCheck + time.Second)
	if h.fireLaunchWakes() == 0 {
		t.Fatal("no pass after the read failed")
	}
}

// A launch keeps its items as it took them, and its replays answer as it
// was made: an edit of a skipped proposal rewrites neither (round 1).
func TestALaunchKeepsItsItemsAsLaunched(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.server.deskCap = team.LaunchReserve
	request := proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}
	first, err := h.client.Launch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Hour)
	h.fireLaunchWakes()
	title := "A, renamed"
	if _, err := h.server.TeamBoard(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: "e1"}, team.BoardCall{Action: team.BoardEdit, Row: 1, Rev: 1, ProposalFields: team.ProposalFields{Title: &title}}); err != nil {
		t.Fatal(err)
	}
	list, err := h.client.Launches(ctx)
	if err != nil || len(list) != 1 || list[0].Items[0].Title != "A" || list[0].Items[0].State != proto.LaunchItemSkipped {
		t.Fatalf("the launches %+v %v", list, err)
	}
	h.clock.Advance(-time.Hour)
	again, err := h.client.Launch(ctx, request)
	if err != nil || again.Items[0].State != proto.LaunchItemWaiting || again.Items[0].Title != "A" || again.ID != first.ID {
		t.Fatalf("the launch again %+v %v", again, err)
	}
}

// The owner's drop fails as a server failure for a failure of pocketd's,
// never as a conflict (round 1).
func TestADropThatFailsIsAServerFailure(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.server.dropChange = func(context.Context, store.ProposalChange) (store.ProposalRow, string, bool, error) {
		return store.ProposalRow{}, "", false, errors.New("disk I/O error")
	}
	var pe *proto.Error
	if _, err := h.client.DropProposal(ctx, 1, proto.DropProposal{RequestID: "d1", Rev: 1}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeInternal {
		t.Fatalf("a failed drop: %v", err)
	}
}

// A drop's request id names that drop alone, claimed in its transaction:
// the same id for another proposal is refused and changes nothing
// (round 1).
func TestADropsRequestNamesItAlone(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	if _, err := h.client.DropProposal(ctx, 1, proto.DropProposal{RequestID: "d1", Rev: 1}); err != nil {
		t.Fatal(err)
	}
	if _, kept, err := h.store.LookupRequest(ctx, requestKeyOf(h.harness, "d1"), "dropProposal:1@1"); err != nil || !kept {
		t.Fatalf("the drop's request %v %v", kept, err)
	}
	var pe *proto.Error
	if _, err := h.client.DropProposal(ctx, 2, proto.DropProposal{RequestID: "d1", Rev: 1}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("the same id for another drop: %v", err)
	}
	if p, _ := h.store.Proposal(ctx, 2); p.State != team.ProposalProposed {
		t.Fatalf("#2 %+v", p)
	}
}

// A launched proposal is off the supervisor's board list (round 1).
func TestALaunchedProposalLeavesTheSupervisorsList(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	list, err := h.server.TeamBoard(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: "l1"}, team.BoardCall{Action: team.BoardList})
	if err != nil || strings.Contains(list, "#1 ") || !strings.Contains(list, "#2 ") {
		t.Fatalf("the board %q %v", list, err)
	}
}

// The owner's request id with another end, however near, is another
// launch, refused (round 1).
func TestALaunchRequestKeepsItsEndWhole(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	end := h.clock.Now().Add(time.Hour)
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: end}); err != nil {
		t.Fatal(err)
	}
	var pe *proto.Error
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: end.Add(time.Microsecond)}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("the same id with another end: %v", err)
	}
}
