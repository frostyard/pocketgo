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
