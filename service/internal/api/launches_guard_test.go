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

// No delivery pass takes over a launch thread while its failed start is
// being settled: its brief never goes, and the thread closes with why
// (round 2).
func TestNoDeliveryTakesOverALaunchStartThatFailed(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	h.server.afterLaunchStart = func() {
		h.engine.mu.Lock()
		h.engine.watchErr = nil
		h.engine.mu.Unlock()
		h.server.deliverLater()
		h.runScheduled(20)
	}
	h.runLaunchPasses()
	threads := h.launchThreads()
	if len(threads) != 1 || threads[0].State != store.ThreadClosed {
		t.Fatalf("the threads %+v", threads)
	}
	if brief := h.sentTo(threads[0].Session, "thread:"); len(brief) != 0 {
		t.Fatalf("a brief went to a failed start: %q", brief)
	}
}

// A failed launch start pocketd could not close yet is closed by a later
// pass, never started: the pass looks again (round 2).
func TestALaunchStartThatCouldNotCloseIsClosedLater(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createErr = errors.New("the engine is full")
	created := h.engine.created
	h.engine.mu.Unlock()
	closing := h.server.closeFailed
	failing := true
	h.server.closeFailed = func(c context.Context, id int64, why string) (store.ThreadRow, error) {
		if failing {
			failing = false
			return store.ThreadRow{}, errors.New("disk I/O error")
		}
		return closing(c, id, why)
	}
	launch, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// A stop came after the item's thread was recorded: its launch is
	// done, and only the recovery of its start remains.
	facts, _ := h.store.LaunchFacts(ctx, launch.ID)
	if _, _, _, started, err := h.store.StartLaunchItem(ctx, facts.Items[0].ID, store.ThreadRow{Title: "A", Agent: "duncan", Task: "Do A."}, store.ShiftRow{Stop: "A is done.", MaxRounds: 20}); err != nil || !started {
		t.Fatalf("the recorded start %v %v", started, err)
	}
	h.runLaunchPasses()
	if threads := h.launchThreads(); len(threads) != 1 || threads[0].State != store.ThreadStarting {
		t.Fatalf("the thread before %+v", threads)
	}
	h.clock.Advance(launchCheck + time.Second)
	if h.fireLaunchWakes() == 0 {
		t.Fatal("no pass looked at the thread again")
	}
	threads := h.launchThreads()
	h.engine.mu.Lock()
	again := h.engine.created - created
	h.engine.mu.Unlock()
	if len(threads) != 1 || threads[0].State != store.ThreadClosed || !strings.Contains(threads[0].Report, "the engine is full") || again != 0 {
		t.Fatalf("the thread after %+v, %d sessions made", threads, again)
	}
}

// A standing session the board's supervisor cannot start refuses the
// launch with why: the owner's to fix, not a server failure (round 2).
func TestALaunchWhoseStandingSessionCannotOpenIsRefused(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	h.server.launchStanding = func(context.Context, string, bool) (int64, string, error) {
		return 0, "", errors.New("faux/faux-big is not available")
	}
	var pe *proto.Error
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeBadRequest || !strings.Contains(pe.Message, "faux-big is not available") {
		t.Fatalf("a launch whose standing session cannot open: %v", err)
	}
}

// A supervisor's launch whose run the owner aborted before it committed
// launches nothing (round 3).
func TestAnAbortedLaunchLaunchesNothing(t *testing.T) {
	h := launchBoard(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.server.TeamLaunch(ctx, TeamCall{Session: h.odrade, Task: 5, CallID: "l1"}, team.LaunchCall{Rows: []int64{1}}); err == nil {
		t.Fatal("an aborted run launched")
	}
	if rows, _ := h.store.LaunchesSince(context.Background(), time.Time{}); len(rows) != 0 {
		t.Fatalf("a launch was made: %+v", rows)
	}
}

// A failed start whose thread has its session, which pocketd could not
// settle at once, is never briefed by a delivery pass: a later pass closes
// it with its start's why (round 3).
func TestAFailedStartWithASessionIsClosedNeverBriefed(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	h.server.afterLaunchStart = func() {
		h.engine.mu.Lock()
		h.engine.watchErr = nil
		h.engine.placedErr = errors.New("the durable store is busy")
		h.engine.mu.Unlock()
	}
	h.runLaunchPasses()
	h.engine.mu.Lock()
	h.engine.placedErr = nil
	h.engine.mu.Unlock()
	h.server.deliverLater()
	h.runScheduled(20)
	threads := h.launchThreads()
	if len(threads) != 1 || threads[0].Session == 0 || threads[0].State != store.ThreadStarting {
		t.Fatalf("the thread before %+v", threads)
	}
	if brief := h.sentTo(threads[0].Session, "thread:"); len(brief) != 0 {
		t.Fatalf("a delivery pass briefed a failed start: %q", brief)
	}
	h.clock.Advance(launchCheck + time.Second)
	h.fireLaunchWakes()
	threads = h.launchThreads()
	if len(threads) != 1 || threads[0].State != store.ThreadClosed || !strings.Contains(threads[0].Report, "the stream would not start") {
		t.Fatalf("the thread after %+v", threads)
	}
}

// A start that may have briefed its specialist hands the thread to a
// delivery pass at once, which notes its brief and opens it (round 3).
func TestAStartThatMayHaveBriefedIsFollowedAtOnce(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	// The start failed, and PiG took its brief all the same, as a record
	// that failed after the brief went would leave it.
	h.server.afterLaunchStart = func() {
		h.engine.mu.Lock()
		h.engine.watchErr = nil
		h.engine.mu.Unlock()
		th := h.launchThreads()[0]
		if _, err := h.engine.Send(ctx, th.Session, briefKey(th), th.Brief); err != nil {
			t.Error(err)
		}
	}
	h.runLaunchPasses()
	h.runScheduled(20)
	if threads := h.launchThreads(); len(threads) != 1 || threads[0].State != store.ThreadOpen || !threads[0].Briefed {
		t.Fatalf("the threads %+v", threads)
	}
}

// A launch's proposals go out without their gather: a launch changes no
// gather's counts (round 3).
func TestALaunchSendsItsProposalsWithoutTheirGather(t *testing.T) {
	h := launchBoard(t, "A", "B")
	ctx := context.Background()
	h.eventsMu.Lock()
	before := len(h.events)
	h.eventsMu.Unlock()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1, 2}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	proposals := 0
	for _, e := range h.events[before:] {
		switch e.Type {
		case proto.EventGather:
			t.Fatalf("a launch sent its gather: %+v", e.Gather)
		case proto.EventProposal:
			proposals++
		}
	}
	if proposals != 2 {
		t.Fatalf("%d proposal events, want 2", proposals)
	}
}

// A failed start left unsettled is kept in the work database: after a
// restart, which empties pocketd's memory, no delivery pass briefs it,
// and the launch pass closes it with its start's why (round 4).
func TestAFailedStartIsSettledAcrossARestart(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	h.server.afterLaunchStart = func() {
		h.engine.mu.Lock()
		h.engine.watchErr = nil
		h.engine.placedErr = errors.New("the durable store is busy")
		h.engine.mu.Unlock()
	}
	h.runLaunchPasses()
	// The restart: pocketd's memory of the start is gone.
	h.server.workMu.Lock()
	h.server.launchSettle = map[int64]string{}
	h.server.workMu.Unlock()
	h.engine.mu.Lock()
	h.engine.placedErr = nil
	h.engine.mu.Unlock()
	h.server.deliverLater()
	h.runScheduled(20)
	threads := h.launchThreads()
	if len(threads) != 1 || threads[0].State != store.ThreadStarting {
		t.Fatalf("the thread before %+v", threads)
	}
	if brief := h.sentTo(threads[0].Session, "thread:"); len(brief) != 0 {
		t.Fatalf("a delivery pass briefed a failed start after a restart: %q", brief)
	}
	h.server.launchSoon()
	h.runLaunchPasses()
	threads = h.launchThreads()
	if len(threads) != 1 || threads[0].State != store.ThreadClosed || !strings.Contains(threads[0].Report, "the stream would not start") {
		t.Fatalf("the thread after %+v", threads)
	}
}

// A start that fails while pocketd stops, though the stop did not cut it
// short, is settled as any failed start: never left for a next start that
// would take it for interrupted (Codex review of #58, round 5).
func TestAStartThatFailsDuringAStopIsSettled(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	h.server.afterLaunchStart = func() {
		h.server.sessionMu.Lock()
		h.server.stopped = true
		h.server.sessionMu.Unlock()
	}
	h.runLaunchPasses()
	threads := h.launchThreads()
	if len(threads) != 1 {
		t.Fatalf("threads %+v", threads)
	}
	why, err := h.store.LaunchStartFailure(ctx, threads[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if threads[0].State != store.ThreadClosed && why == "" {
		t.Fatalf("a start that failed during a stop was left unsettled: %+v", threads[0])
	}
}

// A failed start that cannot be settled yet has its failure written again
// at each pass until the work database keeps it, so a restart never loses
// it (Codex review of #58, round 5).
func TestAFailedStartIsKeptThoughItsFirstWriteFailed(t *testing.T) {
	h := launchBoard(t, "A")
	ctx := context.Background()
	if _, err := h.client.Launch(ctx, proto.CreateLaunch{RequestID: "r1", Proposals: []int64{1}, End: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(20)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	h.server.afterLaunchStart = func() {
		h.engine.mu.Lock()
		h.engine.watchErr = nil
		h.engine.placedErr = errors.New("the durable store is busy")
		h.engine.mu.Unlock()
	}
	mark, failed := h.server.markLaunchFailed, false
	h.server.markLaunchFailed = func(ctx context.Context, thread int64, why string) error {
		if !failed {
			failed = true
			return errors.New("the work database is busy")
		}
		return mark(ctx, thread, why)
	}
	h.runLaunchPasses()
	threads := h.launchThreads()
	if len(threads) != 1 || threads[0].State != store.ThreadStarting {
		t.Fatalf("threads %+v", threads)
	}
	if why, err := h.store.LaunchStartFailure(ctx, threads[0].ID); err != nil || !strings.Contains(why, "the stream would not start") {
		t.Fatalf("the failure kept %q, %v", why, err)
	}
}
