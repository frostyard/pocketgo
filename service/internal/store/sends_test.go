package store

import (
	"errors"
	"testing"
)

// sendThread is an open thread whose specialist works in session.
func sendThread(t *testing.T, s *Store, key string, session int64) ThreadRow {
	t.Helper()
	row, _, err := s.CreateThread(ctx, ThreadRow{Key: key, Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "Fix it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachThread(ctx, row.ID, session, "/w", "", "the brief"); err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenThread(ctx, row.ID)
	if err != nil || opened.State != ThreadOpen {
		t.Fatalf("open %+v %v", opened, err)
	}
	return opened
}

// A supervisor's message is kept once by its key, only for a thread that
// is open, done or blocked, to its specialist's session (ADR-0032).
func TestASupervisorsMessageIsKeptOnce(t *testing.T) {
	s, _ := open(t)
	thread := sendThread(t, s, "delegate:1:c1", 9)
	row := ReportRow{Key: "team-send:1:team:1:4:s1", ThreadID: thread.ID, SessionID: 9, Text: "Renumber to 0089."}
	kept, err := s.FileSend(ctx, row)
	if err != nil || kept.Key != row.Key || kept.SessionID != 9 || kept.Text != row.Text || kept.SentAt != nil {
		t.Fatalf("kept %+v %v", kept, err)
	}
	// The same call again finds its row, whatever the thread is now.
	if _, err := s.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.FileSend(ctx, ReportRow{Key: row.Key, ThreadID: thread.ID, SessionID: 9, Text: "other"})
	if err != nil || again.Text != row.Text {
		t.Fatalf("again %+v %v", again, err)
	}
	// A new one for a closed thread is refused, and nothing is kept.
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:new", ThreadID: thread.ID, SessionID: 9, Text: "late"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("to a closed thread: %v", err)
	}
	if filed, _ := s.ReportFiled(ctx, "team-send:1:new"); filed {
		t.Fatal("a refused message was kept")
	}
	// So is one for a thread still starting, and one to another session.
	starting, _, err := s.CreateThread(ctx, ThreadRow{Key: "delegate:1:c2", Title: "x", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:starting", ThreadID: starting.ID, SessionID: 9, Text: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("to a starting thread: %v", err)
	}
	other := sendThread(t, s, "delegate:1:c3", 11)
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:wrong", ThreadID: other.ID, SessionID: 10, Text: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("to another session: %v", err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:none", ThreadID: 99, SessionID: 9, Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("to no thread: %v", err)
	}
	// Done and blocked threads take one.
	for i, state := range []string{ThreadDone, ThreadBlocked} {
		th := sendThread(t, s, "delegate:1:d"+state, int64(20+i))
		if _, _, _, err := s.FileReport(ctx, "r:"+state, th.ID, state, "x", nil, reportText); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:" + state, ThreadID: th.ID, SessionID: int64(20 + i), Text: "x"}); err != nil {
			t.Fatalf("%d: to a %s thread: %v", i, state, err)
		}
	}
	// It is owed to the specialist's session until it is noted delivered.
	if reports, _, err := s.Owed(ctx, 9); err != nil || len(reports) != 1 || reports[0].Key != row.Key {
		t.Fatalf("owed %d, %v", len(reports), err)
	}
}

// Its delivery takes a done thread back to open in the same transaction,
// once; a blocked, an open or a closed thread keeps its state (ADR-0032).
func TestADeliveredMessageReopensADoneThread(t *testing.T) {
	s, _ := open(t)
	thread := sendThread(t, s, "delegate:1:c1", 9)
	if _, _, _, err := s.FileReport(ctx, "r1", thread.ID, ThreadDone, "done", nil, reportText); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"team-send:1:a", "team-send:1:b"} {
		if _, err := s.FileSend(ctx, ReportRow{Key: key, ThreadID: thread.ID, SessionID: 9, Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	got, reopened, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID)
	if err != nil || !reopened || got.State != ThreadOpen {
		t.Fatalf("delivered %+v %v %v", got, reopened, err)
	}
	if row, _ := s.Report(ctx, "team-send:1:a"); row.SentAt == nil {
		t.Fatal("the message is not noted delivered")
	}
	// Noted again, it reopens nothing: a report filed since stands.
	if _, _, _, err := s.FileReport(ctx, "r2", thread.ID, ThreadDone, "done again", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if got, reopened, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil || reopened || got.State != ThreadDone {
		t.Fatalf("noted again %+v %v %v", got, reopened, err)
	}
	// A blocked thread stays blocked.
	if _, _, _, err := s.FileReport(ctx, "r3", thread.ID, ThreadBlocked, "stuck", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if got, reopened, err := s.SendDelivered(ctx, "team-send:1:b", thread.ID); err != nil || reopened || got.State != ThreadBlocked {
		t.Fatalf("a blocked thread %+v %v %v", got, reopened, err)
	}
	// A closed thread stays closed.
	closed := sendThread(t, s, "delegate:1:c2", 10)
	if _, _, _, err := s.FileReport(ctx, "r4", closed.ID, ThreadDone, "done", nil, reportText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:c", ThreadID: closed.ID, SessionID: 10, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, closed.ID); err != nil {
		t.Fatal(err)
	}
	if got, reopened, err := s.SendDelivered(ctx, "team-send:1:c", closed.ID); err != nil || reopened || got.State != ThreadClosed {
		t.Fatalf("a closed thread %+v %v %v", got, reopened, err)
	}
}
