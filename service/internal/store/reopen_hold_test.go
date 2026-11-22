package store

import "testing"

// A plan the desk's cap held is told of a free place only once a close
// leaves the desk below the cap it was held at: a reopen put the desk
// over it, and the close that only brings it back to the cap frees none
// (Codex review of #121, round 4).
func TestAHeldPlanWaitsForTheDeskBelowItsCap(t *testing.T) {
	s, _ := open(t)
	_, facts := approvedPlan(t, s, "p1")
	a := openedThread(t, s, "k1", 9)
	b := openedThread(t, s, "k2", 10)
	// The cap is 1: the desk holds a and b, one over it, as a reopen
	// leaves it.
	if held, err := s.HoldIfFull(ctx, facts.Plan.ID, 1, func(PlanFacts) error { return nil }); err != nil || !held {
		t.Fatalf("held %v, %v", held, err)
	}
	key := func(owner string) string { return "plan:" + facts.Thread.Owner + ":free:" + owner }
	if _, err := s.CloseThread(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Note(ctx, key(a.Owner)); err == nil {
		t.Fatal("told of a free place with the desk still at its cap")
	}
	if got, _ := s.Plan(ctx, facts.Plan.ID); !got.Held {
		t.Fatal("the hold went with no place free")
	}
	if _, err := s.CloseThread(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if note, err := s.Note(ctx, key(b.Owner)); err != nil || note.Text == "" {
		t.Fatalf("no note once a place is free: %+v, %v", note, err)
	}
	if got, _ := s.Plan(ctx, facts.Plan.ID); got.Held {
		t.Fatal("the hold outlived its note")
	}
}
