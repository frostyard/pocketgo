package store

import (
	"errors"
	"testing"
)

// A profile has one standing session, recorded with the request that
// created it; a session is the standing session of one profile at most,
// and the sessions list says which is.
func TestAStandingSessionIsAProfilesOne(t *testing.T) {
	s, _ := open(t)
	if _, err := s.StandingSession(ctx, "odrade"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before any: %v", err)
	}
	row := SessionRow{ID: 7, Title: "Odrade", Cwd: "/w", Provider: "p", Model: "m", Profile: "odrade", Route: "profile", CreatedBy: "d"}
	if _, err := s.CreateStandingSession(ctx, "k1", row); err != nil {
		t.Fatal(err)
	}
	// The same session again, as a retry records it, is the same one.
	if _, err := s.CreateStandingSession(ctx, "k1", row); err != nil {
		t.Fatalf("again: %v", err)
	}
	if id, err := s.StandingSession(ctx, "odrade"); err != nil || id != 7 {
		t.Fatalf("standing %d, %v", id, err)
	}
	if p, err := s.StandingProfile(ctx, 7); err != nil || p != "odrade" {
		t.Fatalf("profile %q, %v", p, err)
	}
	if result, ok, err := s.LookupRequest(ctx, "k1", "createSession"); err != nil || !ok || result != "7" {
		t.Fatalf("request %q %v %v", result, ok, err)
	}
	// Another session for the same profile is refused, with nothing
	// recorded.
	other := row
	other.ID = 8
	if _, err := s.CreateStandingSession(ctx, "k2", other); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second standing session: %v", err)
	}
	if _, err := s.Session(ctx, 8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused session was recorded: %v", err)
	}
	if _, ok, _ := s.LookupRequest(ctx, "k2", "createSession"); ok {
		t.Fatal("the refused request was recorded")
	}
	// A plain session of the same profile is no standing one.
	if _, err := s.CreateSession(ctx, "k3", SessionRow{ID: 9, Title: "t", Cwd: "/w", Provider: "p", Model: "m", Profile: "odrade", CreatedBy: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StandingProfile(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a plain session: %v", err)
	}
	rows, err := s.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Standing != (r.ID == 7) {
			t.Errorf("session %d standing %v", r.ID, r.Standing)
		}
	}
	if one, err := s.Session(ctx, 7); err != nil || !one.Standing {
		t.Errorf("session 7: %+v, %v", one, err)
	}
}

// A standing session takes its profile's new model, with the rule that
// chose it, at the model's default thinking level.
func TestSetSessionModel(t *testing.T) {
	s, _ := open(t)
	row := SessionRow{ID: 7, Title: "Odrade", Cwd: "/w", Provider: "p", Model: "m", Thinking: "high", Profile: "odrade", Route: "profile", CreatedBy: "d"}
	if _, err := s.CreateStandingSession(ctx, "k1", row); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionModel(ctx, 7, "q", "n", "", "profile-alias"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, 7)
	if err != nil || got.Provider != "q" || got.Model != "n" || got.Route != "profile-alias" || got.Thinking != "" {
		t.Fatalf("session %+v, %v", got, err)
	}
	if err := s.SetSessionModel(ctx, 99, "q", "n", "", "profile"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such session: %v", err)
	}
}

// ChangeModel records the owner's pick in one transaction: a session takes
// the owner's route when its model changed, else keeps its own; a standing
// session takes it in any case, and keeps the pick until it is dropped
// (ADR-0028).
func TestChangeModelKeepsAStandingSessionsPick(t *testing.T) {
	s, _ := open(t)
	standing := SessionRow{ID: 7, Title: "Odrade", Cwd: "/w", Provider: "p", Model: "m", Profile: "odrade", Route: "profile", CreatedBy: "d"}
	if _, err := s.CreateStandingSession(ctx, "k1", standing); err != nil {
		t.Fatal(err)
	}
	plain := SessionRow{ID: 8, Title: "Vera", Cwd: "/w", Provider: "p", Model: "m", Profile: "vera", Route: "profile", CreatedBy: "d"}
	if _, err := s.CreateSession(ctx, "k2", plain); err != nil {
		t.Fatal(err)
	}
	if row, err := s.ChangeModel(ctx, 8, Pick{Provider: "p", Model: "m", Thinking: "high"}, "owner"); err != nil || row.Route != "profile" || row.Thinking != "high" {
		t.Fatalf("a level alone: %+v, %v", row, err)
	}
	if row, err := s.ChangeModel(ctx, 8, Pick{Provider: "q", Model: "n"}, "owner"); err != nil || row.Route != "owner" || row.Model != "n" || row.Thinking != "" {
		t.Fatalf("a new model: %+v, %v", row, err)
	}
	if _, found, err := s.StandingPick(ctx, 8); err != nil || found {
		t.Fatalf("a plain session keeps a pick: %v %v", found, err)
	}
	if row, err := s.ChangeModel(ctx, 7, Pick{Provider: "p", Model: "m", Thinking: "low"}, "owner"); err != nil || row.Route != "owner" || row.Thinking != "low" {
		t.Fatalf("a standing session's level: %+v, %v", row, err)
	}
	if pick, found, err := s.StandingPick(ctx, 7); err != nil || !found || pick != (Pick{Provider: "p", Model: "m", Thinking: "low"}) {
		t.Fatalf("the pick %+v %v %v", pick, found, err)
	}
	if err := s.DropStandingPick(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.StandingPick(ctx, 7); err != nil || found {
		t.Fatalf("a dropped pick: %v %v", found, err)
	}
	if _, err := s.ChangeModel(ctx, 99, Pick{Provider: "p", Model: "m"}, "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such session: %v", err)
	}
}
