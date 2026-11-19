package store

import (
	"errors"
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
	row, outcome, err := s.ChangeLesson(ctx, pending.ID, "vera", "vera", 4, "a2")
	if err != nil || outcome != LessonChanged || row.Text != "a2" || row.State != LessonPending {
		t.Fatalf("update of a waiting lesson: %+v %s %v", row, outcome, err)
	}
	if _, _, err := s.ChangeLesson(ctx, pending.ID, "duncan", "duncan", 5, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another agent's lesson: %v", err)
	}
	row, outcome, err = s.ChangeLesson(ctx, pending.ID, "vera", "vera", 4, "")
	if err != nil || outcome != LessonDropped || row.State != LessonDiscarded {
		t.Fatalf("remove of a waiting lesson: %+v %s %v", row, outcome, err)
	}
	if _, outcome, err := s.ChangeLesson(ctx, pending.ID, "vera", "vera", 4, ""); err != nil || outcome != LessonWasGone {
		t.Fatalf("remove run again: %s %v", outcome, err)
	}

	active := approved(t, s, "vera", "b")
	row, outcome, err = s.ChangeLesson(ctx, active.ID, "vera", "vera", 4, "b2")
	if err != nil || outcome != LessonProposed || row.Text != "b" || row.ChangeText != "b2" || row.ChangeWriter != "vera" || !row.Waits() {
		t.Fatalf("update of a lesson in force: %+v %s %v", row, outcome, err)
	}
	// Back to its own text: the writer's change goes.
	row, outcome, err = s.ChangeLesson(ctx, active.ID, "vera", "vera", 4, "b")
	if err != nil || outcome != LessonKept || row.ChangeText != "" || row.Waits() {
		t.Fatalf("back to the text in force: %+v %s %v", row, outcome, err)
	}
	row, outcome, err = s.ChangeLesson(ctx, active.ID, "vera", "vera", 4, "")
	if err != nil || outcome != LessonForgetProposed || row.State != LessonActive || !row.ChangeForget {
		t.Fatalf("remove of a lesson in force: %+v %s %v", row, outcome, err)
	}
	// Another writer's change stays when this one keeps the text.
	row, outcome, err = s.ChangeLesson(ctx, active.ID, "vera", "odrade", 1, "b")
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
	_, _, _ = s.ChangeLesson(ctx, row.ID, "vera", "vera", 4, "a2")
	if _, _, _, err := s.DecideLesson(ctx, "r3", row.ID, DecideApprove, "a", false, noLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("an approval of the old text: %v", err)
	}
	got, _, inForce, err = s.DecideLesson(ctx, "r3", row.ID, DecideApprove, "a2", false, noLimit)
	if err != nil || !inForce || got.Text != "a2" || got.Waits() {
		t.Fatalf("approve new text %+v %v %v", got, inForce, err)
	}
	// Forgetting waits; the approval names it.
	_, _, _ = s.ChangeLesson(ctx, row.ID, "vera", "vera", 4, "")
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
	_, _, _ = s.ChangeLesson(ctx, active.ID, "vera", "vera", 4, "b2")
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
