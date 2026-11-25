package store

import (
	"errors"
	"strings"
	"testing"
)

// workRow is a work Vera's session 3 called, on frostyard/example.
func workRow(key string) WorkRow {
	return WorkRow{Key: key, Caller: 3, Agent: "vera", Repository: "frostyard/example", Checkout: "/src/example",
		Path: "/src/example--vera-fix-abc123", Branch: "vera/fix-abc123", Title: "Fix", Task: "Fix the flaky test."}
}

// A work's record is kept by its key, once: a call run again finds what
// its first run named, whatever it names now; its session is recorded
// with the session's row, the key's request and its task, in one
// transaction, and once (ADR-0073).
func TestAWorkSessionIsRecordedOnceWithItsTask(t *testing.T) {
	s, path := open(t)
	first, err := s.StartWork(ctx, workRow("work:team:3:7:c1"))
	if err != nil || first.Session != 0 || first.Caller != 3 || first.Path != "/src/example--vera-fix-abc123" {
		t.Fatalf("started %+v, %v", first, err)
	}
	other := workRow("work:team:3:7:c1")
	other.Path, other.Task = "/elsewhere", "Another task."
	again, err := s.StartWork(ctx, other)
	if err != nil || again.Path != first.Path || again.Task != first.Task {
		t.Fatalf("a run again keeps the first record: %+v, %v", again, err)
	}
	if _, err := s.WorkOfSession(ctx, 21); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session no work names: %v", err)
	}
	if _, err := s.WorkSessions(ctx); err != nil {
		t.Fatal(err)
	}
	session := SessionRow{ID: 21, Title: "Vera: Fix", Cwd: "/real/src/example--vera-fix-abc123", Provider: "faux", Model: "faux-1", Profile: "vera", Route: "profile", CreatedBy: "team"}
	task := NoteRow{Key: "work:team:3:7:c1:task", Text: "You work in …"}
	row, err := s.RecordWorkSession(ctx, "work:team:3:7:c1", session, task)
	if err != nil || row.Session != 21 {
		t.Fatalf("recorded %+v, %v", row, err)
	}
	if got, err := s.Session(ctx, 21); err != nil || got.CreatedBy != "team" || got.Profile != "vera" {
		t.Fatalf("the session %+v, %v", got, err)
	}
	if result, existed, err := s.LookupRequest(ctx, "work:team:3:7:c1", "createSession"); err != nil || !existed || result != "21" {
		t.Fatalf("the key's request %q %v %v", result, existed, err)
	}
	if note, ok, err := s.OldestNote(ctx, 21); err != nil || !ok || note.Key != task.Key || note.Text != task.Text {
		t.Fatalf("the task %+v %v %v", note, ok, err)
	}
	// Recorded again, nothing changes; another session is refused, with
	// nothing written.
	if _, err := s.RecordWorkSession(ctx, "work:team:3:7:c1", session, task); err != nil {
		t.Fatalf("recorded again: %v", err)
	}
	if _, err := s.RecordWorkSession(ctx, "work:team:3:7:c1", SessionRow{ID: 22, Title: "x", CreatedBy: "team"}, NoteRow{Key: "other:task", Text: "x"}); !errors.Is(err, ErrWorkSessionTaken) {
		t.Fatalf("another session: %v", err)
	}
	if _, err := s.Session(ctx, 22); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused record kept its session: %v", err)
	}
	if _, err := s.Note(ctx, "other:task"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused record kept its task: %v", err)
	}
	// A key with no work names no session.
	if _, err := s.RecordWorkSession(ctx, "work:none", SessionRow{ID: 23, Title: "x", CreatedBy: "team"}, NoteRow{Key: "none:task", Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no work: %v", err)
	}
	_ = s.Close()
	reopened, _ := openPath(t, path)
	if got, err := reopened.WorkOfSession(ctx, 21); err != nil || got.Key != "work:team:3:7:c1" || got.Caller != 3 {
		t.Fatalf("after a reopen %+v, %v", got, err)
	}
	if all, err := reopened.WorkSessions(ctx); err != nil || len(all) != 1 || all[0].Session != 21 {
		t.Fatalf("the work sessions %+v, %v", all, err)
	}
}

// A work session's answers are kept once each, by key, with the cursor's
// move in one transaction; the cursor never moves back (ADR-0073).
func TestAWorkSessionsAnswersAreKeptOnce(t *testing.T) {
	s, _ := open(t)
	if _, err := s.StartWork(ctx, workRow("work:k")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordWorkSession(ctx, "work:k", SessionRow{ID: 21, Title: "Vera: Fix", CreatedBy: "team"}, NoteRow{Key: "work:k:task", Text: "task"}); err != nil {
		t.Fatal(err)
	}
	replies := []NoteRow{{Key: "work-reply:21:5", SessionID: 3, Text: "first"}, {Key: "work-reply:21:9", SessionID: 3, Text: "second"}}
	if err := s.QueueWorkReplies(ctx, 21, replies, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueWorkReplies(ctx, 21, replies, 7); err != nil {
		t.Fatalf("kept again: %v", err)
	}
	if err := s.QueueWorkReplies(ctx, 21, nil, 4); err != nil {
		t.Fatal(err)
	}
	row, err := s.WorkOfSession(ctx, 21)
	if err != nil || row.Cursor != 7 {
		t.Fatalf("the cursor %+v, %v", row, err)
	}
	var texts []string
	for {
		note, ok, err := s.OldestNote(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		texts = append(texts, note.Text)
		if err := s.NoteSent(ctx, note.Key); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(texts, ",") != "first,second" {
		t.Fatalf("the caller's notes %q", texts)
	}
	if err := s.QueueWorkReplies(ctx, 99, nil, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session that is no work session: %v", err)
	}
}
