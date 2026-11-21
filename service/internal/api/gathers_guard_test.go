package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Guards for the Codex review of #57 (ADR-0033).

// A gather's skill is read as the agents' sandbox shows it, never with
// pocketd's rights: a skill the sandbox shows no file for is no skill, to
// team_gather and to a schedule's listing (round 1).
func TestAGatherReadsItsSkillAsTheSandboxShowsIt(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming"}}]}`)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.unseen = map[string]bool{filepath.Join(h.skills, "grooming", profiles.SkillFile): true}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming"}); err == nil || !strings.Contains(err.Error(), "no such skill") {
		t.Fatalf("a skill the sandbox does not show: %v", err)
	}
	h.server.StartSchedules()
	h.runSchedulePasses()
	list, err := h.client.Schedules(ctx)
	if err != nil || !strings.Contains(list.Schedules[0].Problem, "no such skill") {
		t.Fatalf("the schedule %+v %v", list.Schedules, err)
	}
}

// Only a supervisor's schedule gathers: one for another standing profile
// misses its time, and says why, with no standing session opened and no
// gather started (round 1).
func TestAScheduledGatherIsASupervisors(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "irulan", "when": "every day 23:00", "profile": "irulan", "gather": {"skill": "grooming"}}]}`)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(h.dir, "irulan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nsession: standing\n---\n# Irulan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.StartSchedules()
	h.runSchedulePasses()
	list, err := h.client.Schedules(ctx)
	if err != nil || !strings.Contains(list.Schedules[0].Problem, "not a supervisor") {
		t.Fatalf("the schedule %+v %v", list.Schedules, err)
	}
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	if row := h.scheduleRow("irulan"); row.LastOutcome != store.OutcomeMissed || !strings.Contains(row.LastWhy, "not a supervisor") {
		t.Fatalf("the time %+v", row)
	}
	if _, err := h.store.StandingSession(ctx, "irulan"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a standing session opened: %v", err)
	}
	if rows, _ := h.store.GathersSince(ctx, time.Time{}); len(rows) != 0 {
		t.Fatalf("a gather started: %+v", rows)
	}
}

// A scheduled gather whose skill the sandbox could not read now keeps its
// time due, and goes out at a later pass once it can (round 1).
func TestAScheduledGatherWaitsForASkillItCannotReadNow(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming", "agents": ["duncan"]}}]}`)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.engine.mu.Lock()
	h.engine.readErr = errors.New("bwrap did not start")
	h.engine.mu.Unlock()
	due := time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)
	h.at(due)
	if row := h.scheduleRow("evening"); row.Runs != 0 || row.LastOutcome != "" || row.NextAt == nil || !row.NextAt.Equal(due) {
		t.Fatalf("the time after a read that failed %+v", row)
	}
	h.engine.mu.Lock()
	h.engine.readErr = nil
	h.engine.mu.Unlock()
	h.at(due.Add(scheduleCheck))
	evening := h.scheduleRow("evening")
	if evening.Runs != 1 || evening.LastOutcome != store.OutcomeSent {
		t.Fatalf("the time once the skill reads %+v", evening)
	}
	if facts, err := h.server.gatherByKey(ctx, evening.NoteKey(0)); err != nil || facts.Gather.Skill != "grooming" {
		t.Fatalf("the gather %+v %v", facts, err)
	}
}

// An ask's new session starts from its agent's profile as it is under the
// team's locks: an agent made private after its session was specified
// gets none, and its ask fails (round 1).
func TestAnAskRechecksItsAgentBeforeItsSessionStarts(t *testing.T) {
	h := newGatherHarness(t, "")
	h.server.afterAskSpec = func() {
		if err := os.WriteFile(filepath.Join(h.dir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"duncan"}})
	if got := h.askOf(1, "duncan"); got.State != team.AskFailed || got.Session != 0 || !strings.Contains(got.Why, "private") {
		t.Fatalf("the ask %+v", got)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, spec := range h.engine.specs {
		if spec.Gather != 0 {
			t.Fatalf("a proposal session started: %+v", spec)
		}
	}
}

// A proposal session made before a stop keeps the profile it was made
// from: a read of that profile that fails now waits for a later pass, and
// the session is recorded with its profile and a brief naming its
// repositories (round 1).
func TestARecoveredAskSessionKeepsItsProfile(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	if _, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming", Agents: []string{"duncan"}}); err != nil {
		t.Fatal(err)
	}
	ask := h.askOf(1, "duncan")
	duncan, err := profiles.Folder{Dir: h.dir, Home: h.home}.Find("duncan")
	if err != nil {
		t.Fatal(err)
	}
	// PiG made the session; pocketd stopped before it recorded it.
	id, err := h.engine.CreateSession(ctx, ask.SessionKey(), SessionSpec{Title: "G1 · grooming proposals", Cwd: duncan.Start(), Model: model, Profile: &duncan, Gather: ask.ID})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, fail: map[string]bool{"duncan": true}}
	h.server.SetProfiles(flaky)
	h.runScheduled(5)
	if got := h.askOf(1, "duncan"); got.State != team.AskStarting || got.Session != 0 {
		t.Fatalf("the ask with its profile unread %+v", got)
	}
	flaky.mu.Lock()
	flaky.fail = nil
	flaky.mu.Unlock()
	h.server.deliverLater()
	h.runScheduled(20)
	if got := h.askOf(1, "duncan"); got.State != team.AskRunning || got.Session != id {
		t.Fatalf("the ask once its profile reads %+v", got)
	}
	if row, err := h.store.Session(ctx, id); err != nil || row.Profile != "duncan" {
		t.Fatalf("the session %+v %v", row, err)
	}
	if brief := h.sentTo(id, store.GatherBriefPrefix); len(brief) != 1 || !strings.Contains(brief[0], "frostyard/example") {
		t.Fatalf("the brief %q", brief)
	}
}

// Past the gather's time an agent's part has ended, whether or not a pass
// ended its ask yet: it drops nothing, and its done is the time's end, no
// answer for an agent with no proposal (round 1).
func TestAnAgentsPartEndsAtItsTimeBeforeAnyPass(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming"})
	duncan, vera := h.askOf(1, "duncan"), h.askOf(1, "vera")
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "p1"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("A")}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(team.GatherLimit)
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "d1"}, team.ProposeCall{Action: team.ProposeDrop, Row: 1, Rev: 1}); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("a drop past the time: %v", err)
	}
	if p, err := h.store.Proposal(ctx, 1); err != nil || p.State != team.ProposalProposed {
		t.Fatalf("the proposal %+v %v", p, err)
	}
	text, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "v1"}, team.ProposeCall{Action: team.ProposeDone})
	if err != nil || !strings.Contains(text, "had passed") {
		t.Fatalf("a done past the time %q %v", text, err)
	}
	if got := h.askOf(1, "vera"); got.State != team.AskFailed || !strings.Contains(got.Why, "60 minutes passed") {
		t.Fatalf("vera %+v", got)
	}
}

// heldReads records whether every read of a gather to publish ran with
// workMu held.
type heldReads struct {
	mu     sync.Mutex
	unheld int
}

func (r *heldReads) watch(h *gatherHarness) {
	read := h.server.gatherRead
	h.server.gatherRead = func(ctx context.Context, id int64) (store.GatherFacts, error) {
		if h.server.workMu.TryLock() {
			h.server.workMu.Unlock()
			r.mu.Lock()
			r.unheld++
			r.mu.Unlock()
		}
		return read(ctx, id)
	}
}

// A scheduled gather is read for its event with workMu held, so a pass
// that started an ask meanwhile is never undone by an older copy (round
// 1).
func TestAScheduledGatherIsReadUnderTheWorkLock(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming", "agents": ["duncan"]}}]}`)
	h.server.StartSchedules()
	h.runSchedulePasses()
	var reads heldReads
	reads.watch(h)
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	if got := h.scheduleRow("evening"); got.Runs != 1 {
		t.Fatalf("the time %+v", got)
	}
	if reads.unheld != 0 {
		t.Fatalf("%d reads to publish ran without workMu", reads.unheld)
	}
}

// An order's events carry the rows as the order left them, and an order
// run again sends none (round 1).
func TestABoardOrderIsPublishedOnce(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	for i, title := range []string{"A", "B"} {
		if _, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "p" + title}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields(title)}); err != nil {
			t.Fatal(i, err)
		}
	}
	proposals := func() []proto.Proposal {
		h.eventsMu.Lock()
		defer h.eventsMu.Unlock()
		var out []proto.Proposal
		for _, e := range h.events {
			if e.Type == proto.EventProposal {
				out = append(out, *e.Proposal)
			}
		}
		return out
	}
	before := len(proposals())
	order := TeamCall{Session: h.odrade, Task: 3, CallID: "o1"}
	if _, err := h.server.TeamBoard(ctx, order, team.BoardCall{Action: team.BoardOrder, Rows: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	sent := proposals()[before:]
	row, err := h.store.Proposal(ctx, 2)
	if err != nil || len(sent) != 1 || sent[0].ID != 2 || sent[0].Order != row.Order {
		t.Fatalf("the order's events %+v, the row %+v %v", sent, row, err)
	}
	if _, err := h.server.TeamBoard(ctx, order, team.BoardCall{Action: team.BoardOrder, Rows: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	if again := proposals()[before+1:]; len(again) != 0 {
		t.Fatalf("the order run again sent %+v", again)
	}
}

// A proposal's event that a failed read held back goes at a later delivery
// pass: its change stands (round 1).
func TestAProposalEventLostToAReadGoesLater(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	read := h.server.gatherRead
	failing := true
	h.server.gatherRead = func(ctx context.Context, id int64) (store.GatherFacts, error) {
		if failing {
			return store.GatherFacts{}, errors.New("disk I/O error")
		}
		return read(ctx, id)
	}
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "p1"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("A")}); err != nil {
		t.Fatal(err)
	}
	sent := func() bool {
		h.eventsMu.Lock()
		defer h.eventsMu.Unlock()
		for _, e := range h.events {
			if e.Type == proto.EventProposal && e.Proposal.ID == 1 {
				return true
			}
		}
		return false
	}
	if sent() {
		t.Fatal("an event with no read")
	}
	h.server.workMu.Lock()
	failing = false
	h.server.workMu.Unlock()
	h.runScheduled(20)
	if !sent() {
		t.Fatal("the proposal's event never went")
	}
}

// The board's list reads one supervisor's rows in one query, without the
// proposals' text: another's board is never read (round 1).
func TestTheBoardListIsOneSupervisorsSummary(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "p1"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("A")}); err != nil {
		t.Fatal(err)
	}
	rows, err := h.store.BoardOf(ctx, "odrade")
	if err != nil || len(rows) != 1 || rows[0].ID != 1 || rows[0].Title != "A" || rows[0].Task != "" || rows[0].StopWhen != "" || rows[0].GatheredAt.IsZero() {
		t.Fatalf("odrade's board %+v %v", rows, err)
	}
	if rows, err := h.store.BoardOf(ctx, "leto"); err != nil || len(rows) != 0 {
		t.Fatalf("leto's board %+v %v", rows, err)
	}
}

// A schedule opens a standing session from its profile as it is under
// createMu: one the owner made keep none meanwhile misses the time, with
// no session made (round 2).
func TestAScheduleOpensFromTheProfileAsItIsThen(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "message": "Look at the board."}]}`, false)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.server.beforeCreateLock = func() { writeOdrade(t, h.dir, "session: standing\ncompactAt: 300000\n", "") }
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	if row := h.scheduleRow("evening"); row.LastOutcome != store.OutcomeMissed || !strings.Contains(row.LastWhy, "keeps no standing session") {
		t.Fatalf("the time %+v", row)
	}
	if _, err := h.store.StandingSession(ctx, "odrade"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a standing session opened: %v", err)
	}
	if h.engine.created != 0 {
		t.Fatalf("the engine made %d sessions", h.engine.created)
	}
}

// A schedule's time goes only to a watched session: a standing session
// pocketd opened whose watch failed is watched again before the time is
// filed (round 2).
func TestAScheduleWatchesItsStandingSessionBeforeItsTime(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "message": "Look at the board."}]}`, false)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the stream would not start")
	h.engine.mu.Unlock()
	due := time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)
	h.at(due)
	id, err := h.store.StandingSession(ctx, "odrade")
	if err != nil {
		t.Fatalf("the standing session: %v", err)
	}
	if row := h.scheduleRow("evening"); row.Runs != 0 || row.LastOutcome != "" {
		t.Fatalf("the time with no watch %+v", row)
	}
	h.engine.mu.Lock()
	h.engine.watchErr = nil
	h.engine.mu.Unlock()
	h.at(due.Add(scheduleCheck))
	h.engine.mu.Lock()
	watched := h.engine.watching[id]
	h.engine.mu.Unlock()
	if row := h.scheduleRow("evening"); row.Runs != 1 || !watched {
		t.Fatalf("the time %+v, watched %v", row, watched)
	}
}

// A gather of every agent waits for a profile the folder could not read
// now: it never leaves that agent out (round 2).
func TestAGatherOfEveryAgentWaitsForAnUnreadProfile(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming"}}]}`)
	ctx := context.Background()
	flaky := &unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, fail: map[string]bool{"duncan": true}}
	h.server.SetProfiles(flaky)
	h.server.StartSchedules()
	h.runSchedulePasses()
	due := time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)
	h.at(due)
	if row := h.scheduleRow("evening"); row.Runs != 0 || row.LastOutcome != "" {
		t.Fatalf("the time with duncan unread %+v", row)
	}
	flaky.mu.Lock()
	flaky.fail = nil
	flaky.mu.Unlock()
	h.at(due.Add(scheduleCheck))
	evening := h.scheduleRow("evening")
	facts, err := h.server.gatherByKey(ctx, evening.NoteKey(0))
	if err != nil || len(facts.Asks) != 2 || facts.Asks[0].Agent != "duncan" {
		t.Fatalf("the gather %+v %v", facts.Asks, err)
	}
}

// A proposal session made before a stop gets the brief its start kept,
// naming the repositories as they were, whatever the profile says by then
// (round 2).
func TestARecoveredAskSessionGetsTheBriefItsStartKept(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createLost = errors.New("the answer was lost")
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming", Agents: []string{"duncan"}}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(1)
	if got := h.askOf(1, "duncan"); got.State != team.AskStarting {
		t.Fatalf("the ask after the lost answer %+v", got)
	}
	// The owner moves Duncan to another repository meanwhile.
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(h.dir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/other: "+elsewhere+"\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.deliverLater()
	h.runScheduled(20)
	ask := h.askOf(1, "duncan")
	if ask.State != team.AskRunning {
		t.Fatalf("the ask %+v", ask)
	}
	if row, err := h.store.Session(ctx, ask.Session); err != nil || row.Profile != "duncan" {
		t.Fatalf("the session %+v %v", row, err)
	}
	if brief := h.sentTo(ask.Session, store.GatherBriefPrefix); len(brief) != 1 || !strings.Contains(brief[0], "frostyard/example") || strings.Contains(brief[0], "frostyard/other") {
		t.Fatalf("the brief %q", brief)
	}
}

// A proposal's time is the store's, read in the transaction that decides
// it: a call the service saw before the deadline whose write came after
// is refused (round 2).
func TestAProposalPastItsTimeIsRefusedByTheStoresClock(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"duncan"}})
	duncan := h.askOf(1, "duncan")
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "p1"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("A")}); err != nil {
		t.Fatal(err)
	}
	seen := h.clock.Now()
	h.server.now = func() time.Time { return seen }
	h.clock.Advance(team.GatherLimit)
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "p2"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("B")}); err == nil {
		t.Fatal("a proposal written past the gather's time")
	}
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "d1"}, team.ProposeCall{Action: team.ProposeDrop, Row: 1, Rev: 1}); err == nil {
		t.Fatal("a drop written past the gather's time")
	}
}

// A board change recorded already answers as it did when its call runs
// again, whatever the caller's profile says now (round 2).
func TestABoardChangeAnswersAgainBeforeItsCallerIsChecked(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "p1"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("A")}); err != nil {
		t.Fatal(err)
	}
	drop := TeamCall{Session: h.odrade, Task: 3, CallID: "d1"}
	first, err := h.server.TeamBoard(ctx, drop, team.BoardCall{Action: team.BoardDrop, Row: 1, Rev: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.server.SetProfiles(&unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, fail: map[string]bool{"odrade": true}})
	if again, err := h.server.TeamBoard(ctx, drop, team.BoardCall{Action: team.BoardDrop, Row: 1, Rev: 1}); err != nil || again != first {
		t.Fatalf("the drop again %q %v, first %q", again, err, first)
	}
}

// team_schedule's list shows a gather's action: its skill, its agents and
// its note (round 2).
func TestTeamScheduleListShowsAGather(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming", "agents": ["duncan"], "note": "Evening."}}]}`)
	list, err := h.server.TeamSchedule(context.Background(), TeamCall{Session: h.odrade, Task: 1, CallID: "l1"}, schedule.Call{Action: schedule.ActionList})
	if err != nil || !strings.Contains(list, `evening (the owner's): every day at 23:00`) || !strings.Contains(list, `· a gather with the skill grooming, asking duncan; its note: "Evening."`) {
		t.Fatalf("the list %q %v", list, err)
	}
}

// A run the owner aborted before its gather was recorded starts none
// (round 3).
func TestAnAbortedGatherStartsNone(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming"}); err == nil {
		t.Fatal("an aborted run gathered")
	}
	if rows, _ := h.store.GathersSince(context.Background(), time.Time{}); len(rows) != 0 {
		t.Fatalf("a gather started: %+v", rows)
	}
}

// A standing session the owner's New Session opened while the schedule
// waited for the lock takes the time only once it is watched (round 3).
func TestAScheduleWatchesAStandingSessionOpenedMeanwhile(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "message": "Look at the board."}]}`, false)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	opened := false
	h.server.beforeCreateLock = func() {
		if opened {
			return
		}
		opened = true
		h.engine.mu.Lock()
		h.engine.watchErr = errors.New("the stream would not start")
		h.engine.mu.Unlock()
		_, _ = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	}
	due := time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)
	h.at(due)
	id, err := h.store.StandingSession(ctx, "odrade")
	if err != nil {
		t.Fatalf("the owner's standing session: %v", err)
	}
	if row := h.scheduleRow("evening"); row.Runs != 0 || row.LastOutcome != "" {
		t.Fatalf("the time with no watch %+v", row)
	}
	h.engine.mu.Lock()
	h.engine.watchErr = nil
	h.engine.mu.Unlock()
	h.at(due.Add(scheduleCheck))
	h.engine.mu.Lock()
	watched := h.engine.watching[id]
	h.engine.mu.Unlock()
	if row := h.scheduleRow("evening"); row.Runs != 1 || !watched {
		t.Fatalf("the time %+v, watched %v", row, watched)
	}
}

// A gather of every agent asks a profile the listing could not read but
// a read of its own can (round 3).
func TestAGatherOfEveryAgentAsksAProfileThatReadsAgain(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming"}}]}`)
	ctx := context.Background()
	h.server.SetProfiles(&unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, loadFail: map[string]bool{"duncan": true}})
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	facts, err := h.server.gatherByKey(ctx, h.scheduleRow("evening").NoteKey(0))
	if err != nil || len(facts.Asks) != 2 || facts.Asks[0].Agent != "duncan" || facts.Asks[1].Agent != "vera" {
		t.Fatalf("the gather %+v %v", facts.Asks, err)
	}
}

// An ask's timed end is decided by the store's clock: an agent's done the
// service saw before the deadline, ended after it, is the time's end
// (round 3).
func TestAnAsksEndIsDecidedByTheStoresClock(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	seen := h.clock.Now()
	h.server.now = func() time.Time { return seen }
	h.clock.Advance(team.GatherLimit)
	text, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "v1"}, team.ProposeCall{Action: team.ProposeDone})
	if err != nil || !strings.Contains(text, "had passed") {
		t.Fatalf("the done %q %v", text, err)
	}
	if got := h.askOf(1, "vera"); got.State != team.AskFailed || got.Why != team.GatherTimeWhy {
		t.Fatalf("vera %+v", got)
	}
}

// A proposal session keeps no lessons, checked by team_memory itself, not
// only by the tools it is offered (round 3).
func TestAProposalSessionKeepsNoLessons(t *testing.T) {
	h := newGatherHarness(t, "")
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	if _, err := h.server.TeamMemory(context.Background(), TeamCall{Session: vera.Session, Task: 2, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "Look first."}); err == nil || !strings.Contains(err.Error(), "proposal session") {
		t.Fatalf("a proposal session's lesson: %v", err)
	}
}

// An order sends the proposals it moved and no gather: it changes no
// count (round 3).
func TestABoardOrderSendsNoGather(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming", Agents: []string{"vera"}})
	vera := h.askOf(1, "vera")
	for _, title := range []string{"A", "B", "C"} {
		if _, err := h.server.TeamPropose(ctx, TeamCall{Session: vera.Session, Task: 2, CallID: "p" + title}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields(title)}); err != nil {
			t.Fatal(err)
		}
	}
	h.eventsMu.Lock()
	before := len(h.events)
	h.eventsMu.Unlock()
	if _, err := h.server.TeamBoard(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: "o1"}, team.BoardCall{Action: team.BoardOrder, Rows: []int64{3, 2}}); err != nil {
		t.Fatal(err)
	}
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	var kinds []proto.EventType
	for _, e := range h.events[before:] {
		kinds = append(kinds, e.Type)
	}
	if len(kinds) != 2 || kinds[0] != proto.EventProposal || kinds[1] != proto.EventProposal {
		t.Fatalf("the order's events %v", kinds)
	}
}
