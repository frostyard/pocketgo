package store

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func noLimit(int) error { return nil }

// A lesson is saved once per call: the same key finds the lesson it saved,
// and a writer's waiting lessons for an agent are counted for the limit.
func TestAddLessonSavesOncePerCall(t *testing.T) {
	s, _ := open(t)
	row := LessonRow{Key: "c1", Agent: "vera", Text: "Run the tests.", Writer: "vera", SessionID: 4}
	first, replayed, err := s.AddLesson(ctx, row, noLimit)
	if err != nil || replayed || first.State != LessonPending || first.ID == 0 || !first.Waits() {
		t.Fatalf("add %+v %v %v", first, replayed, err)
	}
	again, replayed, err := s.AddLesson(ctx, row, func(int) error { return errors.New("not called") })
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("a call run again: %+v %v %v", again, replayed, err)
	}
	var counted []int
	full := errors.New("full")
	check := func(n int) error {
		counted = append(counted, n)
		if n >= 2 {
			return full
		}
		return nil
	}
	if _, _, err := s.AddLesson(ctx, LessonRow{Key: "c2", Agent: "vera", Text: "b", Writer: "vera"}, check); err != nil {
		t.Fatal(err)
	}
	// Another writer's, or another agent's, waiting lessons count apart.
	if _, _, err := s.AddLesson(ctx, LessonRow{Key: "c3", Agent: "vera", Text: "c", Writer: "odrade"}, check); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddLesson(ctx, LessonRow{Key: "c4", Agent: "duncan", Text: "d", Writer: "vera"}, check); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddLesson(ctx, LessonRow{Key: "c5", Agent: "vera", Text: "e", Writer: "vera"}, check); !errors.Is(err, full) || !errors.Is(err, ErrConflict) {
		t.Fatalf("the writer's third waiting lesson: %v", err)
	}
	if len(counted) != 4 || counted[0] != 1 || counted[1] != 0 || counted[2] != 0 || counted[3] != 2 {
		t.Fatalf("counted %v", counted)
	}
	live, _ := s.LessonsOf(ctx, "vera")
	if len(live) != 3 || live[0].ID != first.ID {
		t.Fatalf("vera's lessons %+v", live)
	}
}

// An agent's change to its waiting lesson takes effect at once; one to a
// lesson in force waits for the owner, and the lesson stays as it is.
func TestChangeLessonByTheAgent(t *testing.T) {
	s, _ := open(t)
	pending, _, _ := s.AddLesson(ctx, LessonRow{Key: "c1", Agent: "vera", Text: "a", Writer: "vera"}, noLimit)
	row, outcome, _, err := s.ChangeLesson(ctx, "call1", pending.ID, "vera", "vera", 4, "a2", noLimit)
	if err != nil || outcome != LessonChanged || row.Text != "a2" || row.State != LessonPending {
		t.Fatalf("update of a waiting lesson: %+v %s %v", row, outcome, err)
	}
	if _, _, _, err := s.ChangeLesson(ctx, "call2", pending.ID, "duncan", "duncan", 5, "x", noLimit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another agent's lesson: %v", err)
	}
	row, outcome, _, err = s.ChangeLesson(ctx, "call3", pending.ID, "vera", "vera", 4, "", noLimit)
	if err != nil || outcome != LessonDropped || row.State != LessonDiscarded {
		t.Fatalf("remove of a waiting lesson: %+v %s %v", row, outcome, err)
	}
	if _, outcome, _, err := s.ChangeLesson(ctx, "call4", pending.ID, "vera", "vera", 4, "", noLimit); err != nil || outcome != LessonWasGone {
		t.Fatalf("remove run again: %s %v", outcome, err)
	}

	active := approved(t, s, "vera", "b")
	row, outcome, _, err = s.ChangeLesson(ctx, "call5", active.ID, "vera", "vera", 4, "b2", noLimit)
	if err != nil || outcome != LessonProposed || row.Text != "b" || row.ChangeText != "b2" || row.ChangeWriter != "vera" || !row.Waits() {
		t.Fatalf("update of a lesson in force: %+v %s %v", row, outcome, err)
	}
	// Back to its own text: the writer's change goes.
	row, outcome, _, err = s.ChangeLesson(ctx, "call6", active.ID, "vera", "vera", 4, "b", noLimit)
	if err != nil || outcome != LessonKept || row.ChangeText != "" || row.Waits() {
		t.Fatalf("back to the text in force: %+v %s %v", row, outcome, err)
	}
	row, outcome, _, err = s.ChangeLesson(ctx, "call7", active.ID, "vera", "vera", 4, "", noLimit)
	if err != nil || outcome != LessonForgetProposed || row.State != LessonActive || !row.ChangeForget {
		t.Fatalf("remove of a lesson in force: %+v %s %v", row, outcome, err)
	}
	// Another writer's change stays when this one keeps the text.
	row, outcome, _, err = s.ChangeLesson(ctx, "call8", active.ID, "vera", "odrade", 1, "b", noLimit)
	if err != nil || outcome != LessonKept || !row.ChangeForget {
		t.Fatalf("another writer's change was dropped: %+v %s %v", row, outcome, err)
	}
}

// approved is a lesson of agent in force.
func approved(t *testing.T, s *Store, agent, text string) LessonRow {
	t.Helper()
	row, _, err := s.AddLesson(ctx, LessonRow{Key: "k:" + agent + ":" + text, Agent: agent, Text: text, Writer: agent}, noLimit)
	if err != nil {
		t.Fatal(err)
	}
	row, _, inForce, err := s.DecideLesson(ctx, "approve:"+agent+":"+text, row.ID, DecideApprove, text, false, noLimit)
	if err != nil || row.State != LessonActive || !inForce {
		t.Fatalf("approve %+v %v %v", row, inForce, err)
	}
	return row
}

// The owner's approval holds only for what they saw waiting, and a
// decision is made once per request id.
func TestDecideLessonHoldsOnlyForWhatTheOwnerSaw(t *testing.T) {
	s, _ := open(t)
	row, _, _ := s.AddLesson(ctx, LessonRow{Key: "c1", Agent: "vera", Text: "a", Writer: "vera"}, noLimit)
	if _, _, _, err := s.DecideLesson(ctx, "r1", row.ID, DecideApprove, "an older text", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("an approval of text no longer there: %v", err)
	}
	if _, _, _, err := s.DecideLesson(ctx, "r1", row.ID, DecideApprove, "", true, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("an approval of a forgetting that does not wait: %v", err)
	}
	// The refused requests left no record: r1 is free.
	got, replayed, inForce, err := s.DecideLesson(ctx, "r1", row.ID, DecideApprove, "a", false, noLimit)
	if err != nil || replayed || !inForce || got.State != LessonActive {
		t.Fatalf("approve %+v %v %v %v", got, replayed, inForce, err)
	}
	got, replayed, inForce, err = s.DecideLesson(ctx, "r1", row.ID, DecideApprove, "a", false, noLimit)
	if err != nil || !replayed || inForce || got.State != LessonActive {
		t.Fatalf("the request again: %+v %v %v %v", got, replayed, inForce, err)
	}
	if _, _, _, err := s.DecideLesson(ctx, "r2", row.ID, DecideApprove, "a", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("an approval with nothing waiting: %v", err)
	}
	// New text waits; the approval names it.
	_, _, _, _ = s.ChangeLesson(ctx, "call9", row.ID, "vera", "vera", 4, "a2", noLimit)
	if _, _, _, err := s.DecideLesson(ctx, "r3", row.ID, DecideApprove, "a", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("an approval of the old text: %v", err)
	}
	got, _, inForce, err = s.DecideLesson(ctx, "r3", row.ID, DecideApprove, "a2", false, noLimit)
	if err != nil || !inForce || got.Text != "a2" || got.Waits() {
		t.Fatalf("approve new text %+v %v %v", got, inForce, err)
	}
	// Forgetting waits; the approval names it.
	_, _, _, _ = s.ChangeLesson(ctx, "call10", row.ID, "vera", "vera", 4, "", noLimit)
	got, _, inForce, err = s.DecideLesson(ctx, "r4", row.ID, DecideApprove, "", true, noLimit)
	if err != nil || !inForce || got.State != LessonForgotten {
		t.Fatalf("approve forgetting %+v %v %v", got, inForce, err)
	}
	if _, _, _, err := s.DecideLesson(ctx, "r5", row.ID, DecideDiscard, "", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("a decision on a forgotten lesson: %v", err)
	}
	if _, _, _, err := s.DecideLesson(ctx, "r6", 99, DecideDiscard, "", false, noLimit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no lesson: %v", err)
	}
}

// Discard drops what waits and leaves what is in force; forget removes a
// lesson in force; approving a new lesson needs room.
func TestDiscardForgetAndRoom(t *testing.T) {
	s, _ := open(t)
	pending, _, _ := s.AddLesson(ctx, LessonRow{Key: "c1", Agent: "vera", Text: "a", Writer: "vera"}, noLimit)
	full := errors.New("full")
	if _, _, _, err := s.DecideLesson(ctx, "r1", pending.ID, DecideApprove, "a", false, func(int) error { return full }); !errors.Is(err, full) {
		t.Fatalf("approval with no room: %v", err)
	}
	got, _, inForce, err := s.DecideLesson(ctx, "r1", pending.ID, DecideDiscard, "", false, noLimit)
	if err != nil || inForce || got.State != LessonDiscarded {
		t.Fatalf("discard a waiting lesson %+v %v %v", got, inForce, err)
	}
	active := approved(t, s, "vera", "b")
	if _, _, _, err := s.DecideLesson(ctx, "r2", active.ID, DecideDiscard, "", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("discard with nothing waiting: %v", err)
	}
	_, _, _, _ = s.ChangeLesson(ctx, "call11", active.ID, "vera", "vera", 4, "b2", noLimit)
	got, _, inForce, err = s.DecideLesson(ctx, "r2", active.ID, DecideDiscard, "", false, noLimit)
	if err != nil || inForce || got.State != LessonActive || got.Text != "b" || got.Waits() {
		t.Fatalf("discard a change %+v %v %v", got, inForce, err)
	}
	got, _, inForce, err = s.DecideLesson(ctx, "r3", active.ID, DecideForget, "", false, noLimit)
	if err != nil || !inForce || got.State != LessonForgotten {
		t.Fatalf("forget %+v %v %v", got, inForce, err)
	}
	other, _, _ := s.AddLesson(ctx, LessonRow{Key: "c2", Agent: "vera", Text: "c", Writer: "vera"}, noLimit)
	if _, _, _, err := s.DecideLesson(ctx, "r4", other.ID, DecideForget, "", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("forget a waiting lesson: %v", err)
	}
	if _, _, _, err := s.DecideLesson(ctx, "r5", other.ID, "ignore", "", false, noLimit); err == nil {
		t.Fatal("an unknown decision was taken")
	}
	inForceNow, _ := s.ActiveLessons(ctx, "vera")
	live, _ := s.LiveLessons(ctx)
	if len(inForceNow) != 0 || len(live) != 1 || live[0].ID != other.ID {
		t.Fatalf("in force %+v, live %+v", inForceNow, live)
	}
}

// The sessions of a profile are those it started; each profile that
// started one is listed once.
func TestSessionsOfAProfile(t *testing.T) {
	s, _ := open(t)
	for i, p := range []string{"vera", "", "vera", "odrade"} {
		if _, err := s.CreateSession(ctx, "k"+string(rune('a'+i)), SessionRow{ID: int64(i + 1), Title: "t", Cwd: "/w", Provider: "p", Model: "m", Profile: p, CreatedBy: "laptop"}); err != nil {
			t.Fatal(err)
		}
	}
	if ids, err := s.SessionsOfProfile(ctx, "vera"); err != nil || len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("vera's sessions %v %v", ids, err)
	}
	if got, err := s.SessionProfiles(ctx); err != nil || len(got) != 2 || got[0] != "odrade" || got[1] != "vera" {
		t.Fatalf("profiles %v %v", got, err)
	}
}

// An agent's update or remove is recorded with its call: run again, it
// returns its outcome and changes nothing, so a change the owner discarded
// since never comes back (ADR-0015).
func TestAChangeRunAgainChangesNothing(t *testing.T) {
	s, _ := open(t)
	active := approved(t, s, "vera", "a")
	row, outcome, replayed, err := s.ChangeLesson(ctx, "c1", active.ID, "vera", "vera", 4, "b", noLimit)
	if err != nil || replayed || outcome != LessonProposed || row.ChangeText != "b" {
		t.Fatalf("update %+v %s %v %v", row, outcome, replayed, err)
	}
	if _, _, _, err := s.DecideLesson(ctx, "r1", active.ID, DecideDiscard, "", false, noLimit); err != nil {
		t.Fatal(err)
	}
	row, outcome, replayed, err = s.ChangeLesson(ctx, "c1", active.ID, "vera", "vera", 4, "b", noLimit)
	if err != nil || !replayed || outcome != LessonProposed || row.ChangeText != "" || row.Waits() {
		t.Fatalf("the call run again: %+v %s %v %v", row, outcome, replayed, err)
	}
	if got, outcome, found, err := s.LessonCall(ctx, "c1"); err != nil || !found || outcome != LessonProposed || got.ID != active.ID {
		t.Fatalf("the call's record %+v %s %v %v", got, outcome, found, err)
	}
	added, _, _ := s.AddLesson(ctx, LessonRow{Key: "c2", Agent: "vera", Text: "x", Writer: "vera"}, noLimit)
	if got, outcome, found, err := s.LessonCall(ctx, "c2"); err != nil || !found || outcome != "" || got.ID != added.ID {
		t.Fatalf("an add's record %+v %s %v %v", got, outcome, found, err)
	}
	if _, _, found, err := s.LessonCall(ctx, "c3"); err != nil || found {
		t.Fatalf("no call: %v %v", found, err)
	}
}

// A change that waits anew counts against its writer's waiting items, with
// the writer's new lessons; one that replaces the writer's own does not.
func TestChangesCountAgainstTheWritersLimit(t *testing.T) {
	s, _ := open(t)
	a := approved(t, s, "vera", "a")
	b := approved(t, s, "vera", "b")
	var counted []int
	full := errors.New("full")
	check := func(n int) error {
		counted = append(counted, n)
		if n >= 2 {
			return full
		}
		return nil
	}
	if _, _, err := s.AddLesson(ctx, LessonRow{Key: "n1", Agent: "vera", Text: "new", Writer: "vera"}, check); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ChangeLesson(ctx, "c1", a.ID, "vera", "vera", 4, "a2", check); err != nil {
		t.Fatal(err)
	}
	// Replacing its own change takes no more room.
	if _, _, _, err := s.ChangeLesson(ctx, "c2", a.ID, "vera", "vera", 4, "", check); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ChangeLesson(ctx, "c3", b.ID, "vera", "vera", 4, "b2", check); !errors.Is(err, full) {
		t.Fatalf("a third waiting item: %v", err)
	}
	if _, _, err := s.AddLesson(ctx, LessonRow{Key: "n2", Agent: "vera", Text: "newer", Writer: "vera"}, check); !errors.Is(err, full) {
		t.Fatalf("a new lesson past the limit: %v", err)
	}
	if len(counted) != 4 || counted[0] != 0 || counted[1] != 1 || counted[2] != 2 || counted[3] != 2 {
		t.Fatalf("counted %v", counted)
	}
}

// A database that cannot be read says so, never that the lesson is not
// there.
func TestChangeLessonKeepsItsReadError(t *testing.T) {
	s, _ := open(t)
	active := approved(t, s, "vera", "a")
	// The table cannot be read, though the transaction begins.
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE lessons RENAME TO lessons_away`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ChangeLesson(ctx, "c1", active.ID, "vera", "vera", 4, "b", noLimit); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a closed database: %v", err)
	}
}

// tableScan matches a plan that reads a whole table: a scan of a partial
// index (USING ... INDEX) reads only the rows it holds.
var tableScan = regexp.MustCompile(`SCAN (lessons|sessions)($|;| [^U])`)

// The lesson queries read their indexes, never the history of lessons
// that are gone (Codex review of #18, round 2), and the sessions of one
// profile are found by theirs (round 4).
func TestLessonQueriesUseTheirIndexes(t *testing.T) {
	s, _ := open(t)
	for query, index := range map[string]string{
		`SELECT ` + lessonColumns + ` FROM lessons WHERE ` + liveLessons + ` ORDER BY id`: "lessons_live",
		`SELECT ` + lessonColumns + ` FROM lessons WHERE ` + lessonsOf + ` ORDER BY id`:   "lessons_agent",
		`SELECT ` + lessonColumns + ` FROM lessons WHERE ` + activeOf + ` ORDER BY id`:    "lessons_agent",
		`SELECT ` + lessonColumns + ` FROM lessons WHERE ` + lessonByKey:                  "lessons_key",
		`SELECT COUNT(*) FROM lessons WHERE ` + waitingFromOf:                             "lessons_agent",
		`SELECT id FROM sessions WHERE ` + sessionsOf + ` ORDER BY id`:                    "sessions_profile",
		`SELECT DISTINCT profile FROM sessions WHERE ` + profiled + ` ORDER BY profile`:   "sessions_profile",
	} {
		args := make([]any, strings.Count(query, "?"))
		for i := range args {
			args[i] = "x"
		}
		if plan := planOf(t, s, query, args...); !strings.Contains(plan, index) || tableScan.MatchString(plan) {
			t.Errorf("%s:\n  plan %s, want %s", query, plan, index)
		}
	}
}

// New text for another writer's waiting lesson makes it the new writer's,
// and counts against its waiting items: a writer at its limit cannot put a
// text to the owner under another's name (Codex review of #18, round 5).
func TestRewritingAnotherWritersLessonMakesItYours(t *testing.T) {
	s, _ := open(t)
	theirs, _, _ := s.AddLesson(ctx, LessonRow{Key: "o1", Agent: "vera", Text: "from odrade", Writer: "odrade", SessionID: 1}, noLimit)
	full := errors.New("full")
	if _, _, _, err := s.ChangeLesson(ctx, "c1", theirs.ID, "vera", "vera", 4, "from vera", func(int) error { return full }); !errors.Is(err, full) {
		t.Fatalf("a writer at its limit rewrote another's lesson: %v", err)
	}
	row, outcome, _, err := s.ChangeLesson(ctx, "c2", theirs.ID, "vera", "vera", 4, "from vera", noLimit)
	if err != nil || outcome != LessonChanged || row.Writer != "vera" || row.SessionID != 4 || row.Text != "from vera" {
		t.Fatalf("the rewrite: %+v %s %v", row, outcome, err)
	}
	// Its own waiting lesson takes no more room when it changes again.
	if _, _, _, err := s.ChangeLesson(ctx, "c3", theirs.ID, "vera", "vera", 4, "again", func(int) error { return full }); err != nil {
		t.Fatalf("a writer changing its own waiting lesson: %v", err)
	}
}
