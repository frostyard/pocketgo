package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/frostyard/pocketgo/proto"
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

// ChangeModel records the owner's change with the route its caller
// decided (router.ChangeRule), and completes the change's request in the
// same transaction: a retry finds it completed once the session's record
// is, and never applies the change again (Codex review of #48, round 1).
func TestChangeModelRecordsTheSessionAndItsRequestTogether(t *testing.T) {
	s, _ := open(t)
	plain := SessionRow{ID: 8, Title: "Vera", Cwd: "/w", Provider: "p", Model: "m", Profile: "vera", Route: "profile", CreatedBy: "d"}
	if _, err := s.CreateSession(ctx, "k2", plain); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRequest(ctx, "c1", "model:c1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeModel(ctx, 8, "c1", "changed", Pick{Provider: "q", Model: "n", Thinking: "high"}, "owner"); err != nil {
		t.Fatal(err)
	}
	if row, err := s.Session(ctx, 8); err != nil || row.Provider != "q" || row.Model != "n" || row.Thinking != "high" || row.Route != "owner" {
		t.Fatalf("the record: %+v, %v", row, err)
	}
	if result, existed, err := s.LookupRequest(ctx, "c1", "model:c1"); err != nil || !existed || result != "changed" {
		t.Fatalf("the request with the record: %q %v %v", result, existed, err)
	}
	if err := s.ChangeModel(ctx, 99, "c4", "changed", Pick{Provider: "p", Model: "m"}, "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such session: %v", err)
	}
}

// Migration 15 gives each review recorded before it the author's model
// its reviewer was chosen by, its author's session's model then, since no
// model changed before it: a change of the author's model after the
// upgrade counts from the next review (Codex review of #48, round 2). A
// review whose author has no session, and any other thread, keep none.
func TestMigration15KeepsTheAuthorsModelOfReviewsUnderWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.sqlite")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{`CREATE TABLE schema_version (version INTEGER NOT NULL)`}
	steps = append(steps, migrations[:14]...)
	steps = append(steps, `INSERT INTO schema_version (version) VALUES (14)`,
		`INSERT INTO sessions (id, title, cwd, provider, model, created_by, created_at, updated_at) VALUES (5, 'Vera', '/w', 'anthropic', 'claude-opus-5-5', 'd', 1, 1)`,
		`INSERT INTO threads (id, key, title, agent, supervisor_session, session, task, state, owner, created_at, updated_at)
			VALUES (1, 'k1', 'Fix', 'vera', 9, 5, 't', 'open', 'o1', 1, 1), (2, 'k2', 'Plan', 'teg', 9, 0, 't', 'open', 'o2', 1, 1)`,
		`INSERT INTO pull_requests (id, ref, github_id, thread_id, repository, number, title, url, state, head, head_sha, base, checked_at, created_at, updated_at)
			VALUES (1, 'frostyard/example#3', 3, 1, 'frostyard/example', 3, 'Fix', 'u', 'open', 'h', 'aaa', 'main', 1, 1, 1),
			(2, 'frostyard/example#4', 4, 2, 'frostyard/example', 4, 'Plan', 'u', 'open', 'h', 'bbb', 'main', 1, 1, 1)`,
		`INSERT INTO threads (id, key, title, agent, supervisor_session, task, state, owner, review_pull, review_head, created_at, updated_at)
			VALUES (3, 'r1', 'Review', 'vera', 9, 't', 'open', 'o3', 1, 'aaa', 1, 1), (4, 'r2', 'Review', 'teg', 9, 't', 'open', 'o4', 2, 'bbb', 1, 1)`)
	for i, step := range steps {
		if _, err := db.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	_ = db.Close()
	s, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for id, want := range map[int64]proto.ModelRef{1: {}, 2: {}, 3: {Provider: "anthropic", ID: "claude-opus-5-5"}, 4: {}} {
		if row, err := s.Thread(ctx, id); err != nil || row.ReviewAuthor != want {
			t.Errorf("thread %d keeps the author's model %+v (%v), want %+v", id, row.ReviewAuthor, err, want)
		}
	}
}
