package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

func gatherStart(key string) GatherStart {
	return GatherStart{Key: key, SupervisorSession: 1, Skill: "grooming", SkillText: "Find dead code.", Note: "n", Agents: []string{"duncan", "vera"}, Deadline: at.Add(time.Hour)}
}

var aProposal = team.Proposal{Title: "Fix A", Task: "Do it.", StopWhen: "Done.", MaxRounds: 5}

// A gather is recorded once per key, with an ask of each agent; an ask's
// proposal session gets its brief in the transaction that records it
// (ADR-0033).
func TestAGatherStartsOncePerKey(t *testing.T) {
	s, _ := open(t)
	facts, replayed, err := s.StartGather(ctx, gatherStart("team:1:2:g1"))
	if err != nil || replayed || facts.Gather.State != team.GatherRunning || facts.Gather.SkillText != "Find dead code." || len(facts.Asks) != 2 ||
		facts.Asks[0].Agent != "duncan" || facts.Asks[0].State != team.AskStarting || facts.Asks[0].Owner == facts.Asks[1].Owner {
		t.Fatalf("started %+v %v %v", facts, replayed, err)
	}
	again, replayed, err := s.StartGather(ctx, gatherStart("team:1:2:g1"))
	if err != nil || !replayed || again.Gather.ID != facts.Gather.ID || len(again.Asks) != 2 {
		t.Fatalf("again %+v %v %v", again, replayed, err)
	}
	if _, _, err := s.StartGather(ctx, GatherStart{Key: "k", SupervisorSession: 1, Skill: "s", Deadline: at}); err == nil {
		t.Fatal("a gather that asks no one started")
	}
	ask := facts.Asks[0]
	recorded, err := s.RecordGatherSession(ctx, ask.SessionKey(), SessionRow{ID: 40, Title: "G1 · grooming", Cwd: "/h", Provider: "p", Model: "m", Profile: "duncan", CreatedBy: "team"}, ask.ID,
		func(g GatherRow) string { return "brief of G" + g.Skill })
	if err != nil || recorded.State != team.AskRunning || recorded.Session != 40 {
		t.Fatalf("recorded %+v %v", recorded, err)
	}
	note, err := s.Note(ctx, ask.BriefKey())
	if err != nil || note.SessionID != 40 || note.Text != "brief of Ggrooming" {
		t.Fatalf("the brief %+v %v", note, err)
	}
	if got, err := s.GatherAskOfSession(ctx, 40); err != nil || got.ID != ask.ID {
		t.Fatalf("the ask of the session %+v %v", got, err)
	}
	open, deadlines, err := s.OpenGatherAsks(ctx)
	if err != nil || len(open) != 2 || !deadlines[0].Equal(at.Add(time.Hour)) {
		t.Fatalf("open asks %+v %v %v", open, deadlines, err)
	}
}

// The last ask that ends ends its gather, and files its supervisor's note
// once; an ask's brief not sent is dropped when it ends.
func TestTheLastAskEndsTheGatherOnce(t *testing.T) {
	s, _ := open(t)
	facts, _, err := s.StartGather(ctx, gatherStart("g"))
	if err != nil {
		t.Fatal(err)
	}
	duncan, vera := facts.Asks[0], facts.Asks[1]
	if _, err := s.RecordGatherSession(ctx, duncan.SessionKey(), SessionRow{ID: 40, Title: "t", Cwd: "/h", Provider: "p", Model: "m", CreatedBy: "team"}, duncan.ID, func(GatherRow) string { return "b" }); err != nil {
		t.Fatal(err)
	}
	done := func(f GatherFacts) string { return "proposals in: " + f.Gather.Skill }
	got, did, ended, err := s.EndGatherAsk(ctx, duncan.ID, team.AskDone, "", done)
	if err != nil || !did || ended || got.Gather.State != team.GatherRunning {
		t.Fatalf("the first end %+v %v %v %v", got, did, ended, err)
	}
	if note, _ := s.Note(ctx, duncan.BriefKey()); note.DroppedAt == nil {
		t.Fatalf("the brief stays to send: %+v", note)
	}
	if _, did, _, err := s.EndGatherAsk(ctx, duncan.ID, team.AskFailed, "again", done); err != nil || did {
		t.Fatalf("an end again %v %v", did, err)
	}
	got, did, ended, err = s.EndGatherAsk(ctx, vera.ID, team.AskFailed, "its 60 minutes passed", done)
	if err != nil || !did || !ended || got.Gather.State != team.GatherDone || got.Gather.EndedAt == nil || got.Asks[1].Why != "its 60 minutes passed" {
		t.Fatalf("the last end %+v %v %v %v", got, did, ended, err)
	}
	note, err := s.Note(ctx, "gather:"+got.Gather.Owner+":done")
	if err != nil || note.SessionID != 1 || note.Text != "proposals in: grooming" {
		t.Fatalf("the supervisor's note %+v %v", note, err)
	}
	if recent, err := s.GathersSince(ctx, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)); err != nil || len(recent) != 1 {
		t.Fatalf("recent gathers %+v %v", recent, err)
	}
}

// A proposal is filed once per key, only while its ask runs and its
// gather's time lasts; it changes once per key, at its rev, while it is
// proposed; an order puts named proposals first.
func TestProposalsChangeOnceAtTheirRev(t *testing.T) {
	s, _ := open(t)
	facts, _, err := s.StartGather(ctx, gatherStart("g"))
	if err != nil {
		t.Fatal(err)
	}
	ask := facts.Asks[0]
	if _, _, err := s.AddProposal(ctx, "p0", ask.ID, aProposal, "duncan"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a proposal of an ask that starts: %v", err)
	}
	if _, err := s.RecordGatherSession(ctx, ask.SessionKey(), SessionRow{ID: 40, Title: "t", Cwd: "/h", Provider: "p", Model: "m", CreatedBy: "team"}, ask.ID, func(GatherRow) string { return "b" }); err != nil {
		t.Fatal(err)
	}
	row, replayed, err := s.AddProposal(ctx, "p1", ask.ID, aProposal, "duncan")
	if err != nil || replayed || row.Agent != "duncan" || row.Kind != "grooming" || row.State != team.ProposalProposed || row.Rev != 1 || row.Order != int(row.ID)*10 || row.GatherID != facts.Gather.ID {
		t.Fatalf("filed %+v %v %v", row, replayed, err)
	}
	// Past the gather's time by the store's own clock, read in the
	// transaction, a proposal is refused; one filed already answers again.
	clock := s.now
	s.now = func() time.Time { return at.Add(time.Hour) }
	if again, replayed, err := s.AddProposal(ctx, "p1", ask.ID, aProposal, "duncan"); err != nil || !replayed || again.ID != row.ID {
		t.Fatalf("again %+v %v %v", again, replayed, err)
	}
	if _, _, err := s.AddProposal(ctx, "p2", ask.ID, aProposal, "duncan"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a proposal past the gather's time: %v", err)
	}
	if _, _, _, err := s.ChangeProposal(ctx, ProposalChange{Key: "late", ID: row.ID, Rev: 1, Ask: ask.ID, Change: func(p ProposalRow) (ProposalRow, error) { return p, nil },
		Answer: func(ProposalRow) string { return "" }}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an agent's change past the gather's time: %v", err)
	}
	s.now = clock
	second, _, err := s.AddProposal(ctx, "p3", ask.ID, team.Proposal{Title: "Fix B", Task: "t", StopWhen: "s", MaxRounds: 1}, "duncan")
	if err != nil {
		t.Fatal(err)
	}
	retitle := func(title string) func(ProposalRow) (ProposalRow, error) {
		return func(p ProposalRow) (ProposalRow, error) { p.Title = title; return p, nil }
	}
	answer := func(p ProposalRow) string { return "edited #" + p.Title }
	edited, text, replayed, err := s.ChangeProposal(ctx, ProposalChange{Key: "e1", ID: row.ID, Rev: 1, Change: retitle("Fix A2"), Answer: answer})
	if err != nil || replayed || edited.Title != "Fix A2" || edited.Rev != 2 || text != "edited #Fix A2" {
		t.Fatalf("edited %+v %q %v %v", edited, text, replayed, err)
	}
	if _, text, replayed, err := s.ChangeProposal(ctx, ProposalChange{Key: "e1", ID: row.ID, Rev: 1, Change: retitle("other"), Answer: answer}); err != nil || !replayed || text != "edited #Fix A2" {
		t.Fatalf("the edit again %q %v %v", text, replayed, err)
	}
	if _, _, _, err := s.ChangeProposal(ctx, ProposalChange{Key: "e2", ID: row.ID, Rev: 1, Change: retitle("stale"), Answer: answer}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "rev 2") {
		t.Fatalf("a stale rev: %v", err)
	}
	refused := errors.New("not yours")
	if _, _, _, err := s.ChangeProposal(ctx, ProposalChange{Key: "e3", ID: row.ID, Rev: 2, Change: func(ProposalRow) (ProposalRow, error) { return ProposalRow{}, refused }, Answer: answer}); !errors.Is(err, refused) {
		t.Fatalf("a refused change: %v", err)
	}
	order, text, _, err := s.OrderProposals(ctx, "o1", []int64{second.ID, row.ID}, func(ProposalRow) error { return nil }, func(rows []ProposalRow) string { return "ordered" })
	if err != nil || text != "ordered" || len(order) != 2 {
		t.Fatalf("ordered %+v %q %v", order, text, err)
	}
	listed, _ := s.Proposals(ctx)
	if len(listed) != 2 || listed[0].ID != second.ID || listed[1].ID != row.ID || listed[1].Rev != 2 {
		t.Fatalf("the board %+v", listed)
	}
	drop := func(p ProposalRow) (ProposalRow, error) { p.State = team.ProposalDropped; return p, nil }
	if dropped, _, _, err := s.ChangeProposal(ctx, ProposalChange{Key: "d1", ID: row.ID, Rev: 2, Change: drop, Answer: answer}); err != nil || dropped.State != team.ProposalDropped {
		t.Fatalf("dropped %+v %v", dropped, err)
	}
	if _, _, _, err := s.ChangeProposal(ctx, ProposalChange{Key: "d2", ID: row.ID, Rev: 3, Change: retitle("x"), Answer: answer}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a change of a dropped one: %v", err)
	}
	if listed, _ := s.Proposals(ctx); len(listed) != 1 {
		t.Fatalf("the board after the drop %+v", listed)
	}
	if mine, _ := s.ProposalsOfAsk(ctx, ask.ID); len(mine) != 1 || mine[0].ID != second.ID {
		t.Fatalf("the ask's %+v", mine)
	}
}

// A gather's schedule starts one gather a time, keyed as its note would
// be, and its repeat skips while its last gather runs (ADR-0033).
func TestAScheduleStartsOneGatherATime(t *testing.T) {
	s, _ := open(t)
	made, _, err := s.AddSchedule(ctx, ScheduleRow{Key: "k", SessionID: 7, Spec: "every 2h", Zone: "UTC", Text: "", FirstAt: at}, 50)
	if err != nil {
		t.Fatal(err)
	}
	start := gatherStart("")
	next := at.Add(2 * time.Hour)
	if _, outcome, err := s.FireSchedule(ctx, made.ID, at, Fire{Now: at, Session: 7, Next: next, Gather: &start}); err != nil || outcome != OutcomeSent {
		t.Fatalf("the first time %q %v", outcome, err)
	}
	facts, _, err := s.StartGather(ctx, GatherStart{Key: made.NoteKey(0)})
	if err != nil || facts.Gather.SupervisorSession != 7 || len(facts.Asks) != 2 {
		t.Fatalf("its gather %+v %v", facts, err)
	}
	if _, err := s.Note(ctx, made.NoteKey(0)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a gather's time filed a note: %v", err)
	}
	row, outcome, err := s.FireSchedule(ctx, made.ID, next, Fire{Now: next, Session: 7, Next: next.Add(2 * time.Hour), Gather: &start})
	if err != nil || outcome != OutcomeSkipped || row.LastWhy != "its last gather still runs" || row.Runs != 1 {
		t.Fatalf("the second time %+v %q %v", row, outcome, err)
	}
}

// The passes read their partial indexes.
func TestGatherQueriesUseTheirIndexes(t *testing.T) {
	s, _ := open(t)
	for query, index := range map[string]string{
		`SELECT id FROM gathers WHERE state = 'running'`:                                                            "gathers_running",
		`SELECT COUNT(*) FROM gather_asks WHERE gather_id = 3 AND state IN ('starting', 'running')`:                 "gather_asks_open",
		`SELECT ` + gatherAskColumns + ` FROM gather_asks WHERE session = 4 AND session != 0`:                       "gather_asks_session",
		`SELECT ` + proposalColumns + ` FROM proposals WHERE state != 'dropped' ORDER BY ord, id`:                   "proposals_open",
		`SELECT ` + proposalColumns + ` FROM proposals WHERE ask_id = 2 AND state != 'dropped' ORDER BY id`:         "proposals_ask",
		`SELECT ` + proposalColumns + ` FROM proposals WHERE gather_id = 3 AND state != 'dropped' ORDER BY ord, id`: "proposals_gather",
	} {
		if plan := planOf(t, s, query); !strings.Contains(plan, index) {
			t.Errorf("%s: plan %q does not use %s", query, plan, index)
		}
	}
}
