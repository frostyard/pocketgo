package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// gatherHarness is a schedule harness with a team to ask: Odrade
// supervises from her standing session, Leto is another supervisor, Duncan
// works in his home with a repository, Vera in her home; the skills folder
// holds grooming.
type gatherHarness struct {
	*scheduleHarness
	skills string
	leto   int64
}

func newGatherHarness(t *testing.T, file string) *gatherHarness {
	t.Helper()
	h := &gatherHarness{scheduleHarness: newScheduleHarness(t, file, true)}
	checkout := t.TempDir()
	for name, text := range map[string]string{
		"duncan.md": "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/example: " + checkout + "\n---\n# Duncan\n",
		"leto.md":   leto,
	} {
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.skills = t.TempDir()
	if err := os.MkdirAll(filepath.Join(h.skills, "grooming"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.skills, "grooming", "SKILL.md"), []byte("Find dead code and flaky tests."), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.SetSkills(h.skills)
	session, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	h.leto = session.ID
	// The agents work until a test says their runs ended.
	busy := false
	h.engine.mu.Lock()
	h.engine.idleSays = &busy
	h.engine.mu.Unlock()
	return h
}

// runsEnd has every session's run end, as the engine would say once each
// is idle, and runs the passes that follow.
func (h *gatherHarness) runsEnd() {
	idle := true
	h.engine.mu.Lock()
	h.engine.idleSays = &idle
	h.engine.mu.Unlock()
	h.server.deliverLater()
	h.runScheduled(20)
}

// askOf is the ask of agent in gather g.
func (h *gatherHarness) askOf(g int64, agent string) store.GatherAskRow {
	h.t.Helper()
	facts, err := h.store.GatherFacts(context.Background(), g)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, a := range facts.Asks {
		if a.Agent == agent {
			return a
		}
	}
	h.t.Fatalf("G%d asks no %s: %+v", g, agent, facts.Asks)
	return store.GatherAskRow{}
}

// gather starts gather g for Odrade, and runs the passes that start its
// asks' sessions and send their briefs.
func (h *gatherHarness) gather(call string, g team.Gather) string {
	h.t.Helper()
	text, err := h.server.TeamGather(context.Background(), TeamCall{Session: h.odrade, Task: 1, CallID: call}, g)
	if err != nil {
		h.t.Fatal(err)
	}
	h.runScheduled(20)
	return text
}

func fields(title string) team.ProposalFields {
	task, stop := "Do "+title+".", title+" is done."
	return team.ProposalFields{Title: &title, Task: &task, StopWhen: &stop}
}

// A supervisor gathers proposals: each agent it asks gets a read-only
// proposal session of its own, keyed by its ask, whose brief carries the
// skill's text, and takes no place on the desk (ADR-0033).
func TestASupervisorGathersProposals(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	text := h.gather("g1", team.Gather{Skill: "grooming", Note: "Mind the CI."})
	if !strings.Contains(text, "Gather G1 started with the skill grooming: pocketd asks duncan, vera") {
		t.Fatalf("the answer %q", text)
	}
	// The same call again answers as the first did, and starts no other.
	if again, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming"}); err != nil || again != text {
		t.Fatalf("again %q %v", again, err)
	}
	duncan := h.askOf(1, "duncan")
	if duncan.State != team.AskRunning || duncan.Session == 0 {
		t.Fatalf("duncan's ask %+v", duncan)
	}
	var spec SessionSpec
	for _, s := range h.engine.specs {
		if s.Gather == duncan.ID {
			spec = s
		}
	}
	if spec.Profile == nil || spec.Profile.ID != "duncan" || spec.Cwd != h.home || spec.Title != "G1 · grooming proposals" || spec.Thread != 0 {
		t.Fatalf("duncan's session spec %+v", spec)
	}
	brief := h.sentTo(duncan.Session, store.GatherBriefPrefix)
	if len(brief) != 1 || !strings.Contains(brief[0], "Find dead code and flaky tests.") || !strings.Contains(brief[0], "Mind the CI.") ||
		!strings.Contains(brief[0], "Odrade, the team's supervisor") || !strings.Contains(brief[0], "frostyard/example at ") {
		t.Fatalf("duncan's brief %q", brief)
	}
	if threads, _ := h.store.Threads(ctx); len(threads) != 0 {
		t.Fatalf("a gather opened threads: %+v", threads)
	}
	// The owner's message waits for the brief: here it is in.
	if _, err := h.client.Send(ctx, duncan.Session, proto.SendMessage{RequestID: "m1", Text: "hello"}); err != nil {
		t.Fatalf("a message after the brief: %v", err)
	}
	h.eventsMu.Lock()
	gathers := 0
	for _, e := range h.events {
		if e.Type == proto.EventGather && e.Gather.ID == 1 {
			gathers++
		}
	}
	h.eventsMu.Unlock()
	if gathers == 0 {
		t.Fatal("no gather event")
	}
}

// An asked agent files, edits, lists and drops its own proposals at their
// rev, while its part runs; done ends its part, and the last part's end
// tells the supervisor once (ADR-0033).
func TestAnAgentProposesAndTheSupervisorIsToldOnce(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming"})
	duncan, vera := h.askOf(1, "duncan"), h.askOf(1, "vera")
	propose := func(session int64, call string, c team.ProposeCall) (string, error) {
		return h.server.TeamPropose(ctx, TeamCall{Session: session, Task: 2, CallID: call}, c)
	}
	repo := "frostyard/example"
	add := team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("Fix the flaky test")}
	add.Repository = &repo
	got, err := propose(duncan.Session, "p1", add)
	if err != nil || got != "Proposed #1 at rev 1: Fix the flaky test" {
		t.Fatalf("an add %q %v", got, err)
	}
	if again, err := propose(duncan.Session, "p1", add); err != nil || again != got {
		t.Fatalf("the add again %q %v", again, err)
	}
	row, err := h.store.Proposal(ctx, 1)
	if err != nil || row.Agent != "duncan" || row.Kind != "grooming" || row.Repository != repo || row.MaxRounds != team.DefaultShiftRounds || row.By != "duncan" {
		t.Fatalf("the row %+v %v", row, err)
	}
	if _, err := propose(duncan.Session, "p2", team.ProposeCall{Action: team.ProposeAdd, ProposalFields: func() team.ProposalFields {
		f := fields("Other repo")
		other := "frostyard/other"
		f.Repository = &other
		return f
	}()}); err == nil || !strings.Contains(err.Error(), "none of the agent's repositories") {
		t.Fatalf("another repository: %v", err)
	}
	title := "Fix the flaky tests"
	if got, err := propose(duncan.Session, "e1", team.ProposeCall{Action: team.ProposeEdit, Row: 1, Rev: 1, ProposalFields: team.ProposalFields{Title: &title}}); err != nil || got != "Edited #1: it is at rev 2 now." {
		t.Fatalf("an edit %q %v", got, err)
	}
	if _, err := propose(duncan.Session, "e2", team.ProposeCall{Action: team.ProposeEdit, Row: 1, Rev: 1, ProposalFields: team.ProposalFields{Title: &title}}); err == nil || !strings.Contains(err.Error(), "rev 2") {
		t.Fatalf("a stale rev: %v", err)
	}
	// Vera touches only her own.
	if _, err := propose(vera.Session, "v1", team.ProposeCall{Action: team.ProposeDrop, Row: 1, Rev: 2}); err == nil || !strings.Contains(err.Error(), "not yours") {
		t.Fatalf("another's drop: %v", err)
	}
	if _, err := propose(vera.Session, "v2", team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("Tidy docs")}); err != nil {
		t.Fatal(err)
	}
	if _, err := propose(vera.Session, "v3", team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("Drop me")}); err != nil {
		t.Fatal(err)
	}
	if got, err := propose(vera.Session, "v4", team.ProposeCall{Action: team.ProposeDrop, Row: 3, Rev: 1}); err != nil || got != "Dropped #3: it leaves the board." {
		t.Fatalf("a drop %q %v", got, err)
	}
	if list, err := propose(vera.Session, "v5", team.ProposeCall{Action: team.ProposeList}); err != nil || !strings.Contains(list, "#2 rev 1 · vera · home · grooming · 20 rounds · G1 · Tidy docs") || strings.Contains(list, "#3") {
		t.Fatalf("vera's list %q %v", list, err)
	}
	// Duncan is done; his part takes no proposal more.
	if got, err := propose(duncan.Session, "d1", team.ProposeCall{Action: team.ProposeDone}); err != nil || !strings.Contains(got, "Your part in G1 is done, with 1 proposals") {
		t.Fatalf("done %q %v", got, err)
	}
	if _, err := propose(duncan.Session, "p3", team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("Late")}); err == nil {
		t.Fatal("a proposal after done")
	}
	if _, err := propose(duncan.Session, "e3", team.ProposeCall{Action: team.ProposeEdit, Row: 1, Rev: 2, ProposalFields: team.ProposalFields{Title: &title}}); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("an edit after done: %v", err)
	}
	if note := h.sentTo(h.odrade, "gather:"); len(note) != 0 {
		t.Fatalf("the supervisor was told before every part ended: %q", note)
	}
	// Vera's run ends: her part ends, and the gather with it.
	h.runsEnd()
	if got := h.askOf(1, "vera"); got.State != team.AskDone {
		t.Fatalf("vera's ask %+v", got)
	}
	facts, _ := h.store.GatherFacts(ctx, 1)
	if facts.Gather.State != team.GatherDone {
		t.Fatalf("the gather %+v", facts.Gather)
	}
	note := h.sentTo(h.odrade, "gather:")
	if len(note) != 1 || !strings.Contains(note[0], "[G1 · grooming · proposals in] 2 proposals from 2 agents asked.") ||
		!strings.Contains(note[0], "#1 duncan: Fix the flaky tests") || !strings.Contains(note[0], "#2 vera: Tidy docs") {
		t.Fatalf("the supervisor's note %q", note)
	}
	h.runsEnd()
	if again := h.sentTo(h.odrade, "gather:"); len(again) != 1 {
		t.Fatalf("told again: %q", again)
	}
	// Some session that answers no gather files nothing.
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: h.odrade, Task: 3, CallID: "x"}, add); err == nil || !strings.Contains(err.Error(), "proposal session's") {
		t.Fatalf("a supervisor's proposal: %v", err)
	}
}

// An ask whose time passes ends: done with its proposals, failed with
// none; the gather ends, and the supervisor hears once (ADR-0033).
func TestAnAskEndsWhenItsTimePasses(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming"})
	duncan := h.askOf(1, "duncan")
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "p1"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("A")}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(team.GatherLimit)
	if _, err := h.server.TeamPropose(ctx, TeamCall{Session: duncan.Session, Task: 2, CallID: "p2"}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields("B")}); err == nil {
		t.Fatal("a proposal past the gather's time")
	}
	h.server.deliverLater()
	h.runScheduled(20)
	if got := h.askOf(1, "duncan"); got.State != team.AskDone || !strings.Contains(got.Why, "60 minutes passed") {
		t.Fatalf("duncan %+v", got)
	}
	if got := h.askOf(1, "vera"); got.State != team.AskFailed {
		t.Fatalf("vera %+v", got)
	}
	note := h.sentTo(h.odrade, "gather:")
	if len(note) != 1 || !strings.Contains(note[0], "No answer: vera (its 60 minutes passed).") {
		t.Fatalf("the note %q", note)
	}
}

// Only a supervisor gathers, with a skill the skills folder has and agents
// a gather may ask (ADR-0033).
func TestTeamGatherRefuses(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "vera", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		session int64
		g       team.Gather
		want    string
	}{
		"an agent":         {vera.ID, team.Gather{Skill: "grooming"}, "only a supervisor"},
		"no such skill":    {h.odrade, team.Gather{Skill: "triage"}, "no such skill"},
		"a supervisor":     {h.odrade, team.Gather{Skill: "grooming", Agents: []string{"leto"}}, "supervisor and takes no"},
		"no such agent":    {h.odrade, team.Gather{Skill: "grooming", Agents: []string{"nobody"}}, "no agent nobody"},
		"a bad skill name": {h.odrade, team.Gather{Skill: "../x"}, "no skill's name"},
	} {
		if _, err := h.server.TeamGather(ctx, TeamCall{Session: tc.session, Task: 1, CallID: name}, tc.g); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if rows, _ := h.store.GathersSince(ctx, time.Time{}); len(rows) != 0 {
		t.Fatalf("refusals started gathers: %+v", rows)
	}
}

// The supervisor keeps the board of its gathers' proposals: it lists and
// reads them, edits and drops one at its rev, and orders them; another
// supervisor sees none of them (ADR-0033).
func TestTheSupervisorKeepsTheBoard(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.gather("g1", team.Gather{Skill: "grooming"})
	duncan, vera := h.askOf(1, "duncan"), h.askOf(1, "vera")
	for i, p := range []struct {
		session int64
		title   string
	}{{duncan.Session, "A"}, {vera.Session, "B"}, {vera.Session, "C"}} {
		if _, err := h.server.TeamPropose(ctx, TeamCall{Session: p.session, Task: 2, CallID: "p" + string(rune('0'+i))}, team.ProposeCall{Action: team.ProposeAdd, ProposalFields: fields(p.title)}); err != nil {
			t.Fatal(err)
		}
	}
	board := func(session int64, call string, c team.BoardCall) (string, error) {
		return h.server.TeamBoard(ctx, TeamCall{Session: session, Task: 3, CallID: call}, c)
	}
	list, err := board(h.odrade, "l1", team.BoardCall{Action: team.BoardList})
	if err != nil || !strings.Contains(list, "#1 rev 1 · duncan · home · grooming · 20 rounds · G1 (Wed 18 Nov 22:00 UTC) · A") || strings.Index(list, "#1 ") > strings.Index(list, "#3 ") {
		t.Fatalf("the list %q %v", list, err)
	}
	if read, err := board(h.odrade, "r1", team.BoardCall{Action: team.BoardRead, Row: 2}); err != nil || !strings.Contains(read, "Task:\nDo B.") || !strings.Contains(read, "Stop when:\nB is done.") {
		t.Fatalf("a read %q %v", read, err)
	}
	if got, err := board(h.odrade, "o1", team.BoardCall{Action: team.BoardOrder, Rows: []int64{3, 1}}); err != nil || got != "Ordered: #3, #1 are first on the board, in that order." {
		t.Fatalf("an order %q %v", got, err)
	}
	if list, _ := board(h.odrade, "l2", team.BoardCall{Action: team.BoardList}); strings.Index(list, "#3 ") >= strings.Index(list, "#1 ") || strings.Index(list, "#1 ") >= strings.Index(list, "#2 ") {
		t.Fatalf("the ordered list %q", list)
	}
	rounds := 5
	if got, err := board(h.odrade, "e1", team.BoardCall{Action: team.BoardEdit, Row: 2, Rev: 1, ProposalFields: team.ProposalFields{MaxRounds: &rounds}}); err != nil || got != "Edited #2: it is at rev 2 now." {
		t.Fatalf("an edit %q %v", got, err)
	}
	if got, err := board(h.odrade, "d1", team.BoardCall{Action: team.BoardDrop, Row: 1, Rev: 1}); err != nil || got != "Dropped #1: it leaves the board." {
		t.Fatalf("a drop %q %v", got, err)
	}
	if again, err := board(h.odrade, "d1", team.BoardCall{Action: team.BoardDrop, Row: 1, Rev: 1}); err != nil || again != "Dropped #1: it leaves the board." {
		t.Fatalf("the drop again %q %v", again, err)
	}
	// Leto's board holds none of them.
	if list, err := board(h.leto, "l3", team.BoardCall{Action: team.BoardList}); err != nil || list != "The board holds no proposal of yours." {
		t.Fatalf("leto's list %q %v", list, err)
	}
	if _, err := board(h.leto, "d2", team.BoardCall{Action: team.BoardDrop, Row: 2, Rev: 2}); err == nil || !strings.Contains(err.Error(), "another supervisor's board") {
		t.Fatalf("leto's drop: %v", err)
	}
	if _, err := board(duncan.Session, "x", team.BoardCall{Action: team.BoardList}); err == nil || !strings.Contains(err.Error(), "only a supervisor") {
		t.Fatalf("an agent's board: %v", err)
	}
	// GET /v1/proposals lists the board in its order, and GET /v1/gathers
	// the gather.
	proposals, err := h.client.Proposals(ctx)
	if err != nil || len(proposals) != 2 || proposals[0].ID != 3 || proposals[1].ID != 2 || proposals[1].MaxRounds != 5 || proposals[1].Rev != 2 ||
		proposals[0].Kind != "grooming" || proposals[0].Gather != 1 || !proposals[0].GatheredAt.Equal(time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("the proposals %+v %v", proposals, err)
	}
	gathers, err := h.client.Gathers(ctx)
	if err != nil || len(gathers) != 1 || gathers[0].Skill != "grooming" || gathers[0].Supervisor != h.odrade || len(gathers[0].Asks) != 2 || gathers[0].Asks[0].Proposals != 0 || gathers[0].Asks[1].Proposals != 2 {
		t.Fatalf("the gathers %+v %v", gathers, err)
	}
}

// A proposal session takes no message before its brief (ADR-0033).
func TestAProposalSessionTakesNoMessageBeforeItsBrief(t *testing.T) {
	h := newGatherHarness(t, "")
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamGather(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "g1"}, team.Gather{Skill: "grooming", Agents: []string{"vera"}}); err != nil {
		t.Fatal(err)
	}
	// The ask's session starts, and its brief cannot go yet.
	h.engine.mu.Lock()
	h.engine.sendErr = errBriefTest
	h.engine.mu.Unlock()
	h.runScheduled(3)
	vera := h.askOf(1, "vera")
	if vera.Session == 0 {
		t.Fatal("no session")
	}
	h.engine.mu.Lock()
	h.engine.sendErr = nil
	h.engine.mu.Unlock()
	if _, err := h.client.Send(ctx, vera.Session, proto.SendMessage{RequestID: "m1", Text: "hello"}); err == nil || !strings.Contains(err.Error(), "does not have its brief yet") {
		t.Fatalf("a message before the brief: %v", err)
	}
}

var errBriefTest = errorString("the session takes no message now")

type errorString string

func (e errorString) Error() string { return string(e) }

// A schedule's gather starts a gather for its profile's standing session,
// one at a time, and one whose skill is not there misses its time
// (ADR-0033).
func TestAScheduledGather(t *testing.T) {
	h := newGatherHarness(t, `{"schedules": [
		{"id": "evening", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "grooming", "agents": ["duncan"], "note": "Evening."}},
		{"id": "triage", "when": "every day 23:00", "profile": "odrade", "gather": {"skill": "triage"}}
	]}`)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	list, err := h.client.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Schedules[0]; got.Gather == nil || got.Gather.Skill != "grooming" || got.Gather.Note != "Evening." || got.Problem != "" {
		t.Fatalf("the evening's schedule %+v", got)
	}
	if got := list.Schedules[1]; !strings.Contains(got.Problem, "no such skill") {
		t.Fatalf("triage's schedule %+v", got)
	}
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	evening := h.scheduleRow("evening")
	facts, err := h.server.gatherByKey(ctx, evening.NoteKey(0))
	if err != nil || facts.Gather.SupervisorSession != h.odrade || facts.Gather.Schedule != "evening" || facts.Gather.Note != "Evening." || len(facts.Asks) != 1 || facts.Asks[0].State != team.AskRunning {
		t.Fatalf("the evening's gather %+v %v", facts, err)
	}
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 0 {
		t.Fatalf("a gather's schedule sent a message: %q", got)
	}
	if triage := h.scheduleRow("triage"); triage.LastOutcome != store.OutcomeMissed || !strings.Contains(triage.LastWhy, "no such skill") {
		t.Fatalf("triage %+v", triage)
	}
	// The gather's time ends it, and the next day's time starts another.
	h.setClock(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))
	h.server.deliverLater()
	h.runScheduled(20)
	if facts, _ := h.server.gatherByKey(ctx, evening.NoteKey(0)); facts.Gather.State != team.GatherDone {
		t.Fatalf("the gather after its time %+v", facts.Gather)
	}
	h.at(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC))
	if got := h.scheduleRow("evening"); got.Runs != 2 {
		t.Fatalf("the next day %+v", got)
	}
}
