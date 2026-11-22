package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A work database at version 23 whose close a stop cut off before its
// thread's worker's, shift's and supervisor's messages were marked sent
// has them marked by migration 24, so no reopen sends them; its other
// messages, and those of a thread not closed, stay (Codex review of #121,
// round 3).
func TestTheReopenMigrationRetiresAClosedThreadsLeftMessages(t *testing.T) {
	if SchemaVersion < 24 || !strings.Contains(migrations[23], "owner_messages") {
		t.Fatalf("schema version %d: the reopen is not migration 24", SchemaVersion)
	}
	path := filepath.Join(t.TempDir(), "work.sqlite")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{`CREATE TABLE schema_version (version INTEGER NOT NULL)`}
	steps = append(steps, migrations[:23]...)
	steps = append(steps, `INSERT INTO schema_version (version) VALUES (23)`,
		`INSERT INTO threads (id, key, title, agent, supervisor_session, session, task, state, owner, created_at, updated_at)
			VALUES (1, 'k1', 't', 'vera', 1, 9, 'x', 'closed', 'aaaaaaaaaaaaaaaa', 1, 1)`,
		`INSERT INTO threads (id, key, title, agent, supervisor_session, session, task, state, owner, created_at, updated_at)
			VALUES (2, 'k2', 't', 'vera', 1, 10, 'x', 'open', 'bbbbbbbbbbbbbbbb', 1, 1)`)
	for _, key := range []string{"worker-reply:a", "worker-msg:b", "shift-round:c:1", "shift-summary:c:2", "team-send:1:d", "review-verdict:e", "report:f"} {
		steps = append(steps, `INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES ('`+key+`', 1, 9, 'x', 1)`,
			`INSERT INTO reports (key, thread_id, session_id, text, created_at) VALUES ('`+key+`:open', 2, 10, 'x', 1)`)
	}
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
	unsent, err := s.UnsentReportsOf(ctx, 1)
	if err != nil || len(unsent) != 2 || unsent[0].Key != "review-verdict:e" || unsent[1].Key != "report:f" {
		t.Fatalf("the closed thread's unsent messages: %+v, %v", unsent, err)
	}
	if open, err := s.UnsentReportsOf(ctx, 2); err != nil || len(open) != 7 {
		t.Fatalf("the open thread's unsent messages: %d, %v", len(open), err)
	}
}
