package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// The owner's new answer files one note to the thread's supervisor in the
// transaction that records it, keyed by the thread's owner, the ask's key
// and a digest of the answer. A replay, a second answer and an answer to
// a withdrawn ask file nothing, and a withdrawal files no answer note; a
// thread with no supervisor session, or whose supervisor session asked,
// gets none; a closed thread's supervisor is told it is closed (ADR-0072).
func TestAnAnswerTellsTheSupervisorOnce(t *testing.T) {
	s, _ := open(t)
	notes := func() []NoteRow {
		t.Helper()
		rows, err := queryRows(ctx, s.db, scanNote, `SELECT `+noteColumns+` FROM notes WHERE key LIKE 'answered:%' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	thread, _, err := s.CreateThread(ctx, ThreadRow{Key: "t", Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachThread(ctx, thread.ID, 9, "/w", "", "brief"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	_, asks, _, err := s.FileReport(ctx, "r", thread.ID, ThreadBlocked, "Questions.", []AskRow{{Key: "r:ask:0", SessionID: 9, Question: "Squash?"},
		{Key: "r:ask:1", SessionID: 9, Question: "Rebase?"}, {Key: "r:ask:2", SessionID: 9, Question: "Ship?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a1", asks[0].ID, "Yes, squash.", "laptop"); err != nil {
		t.Fatal(err)
	}
	got := notes()
	if len(got) != 1 || got[0].Key != team.AnsweredKey(thread.Owner, asks[0].Key, "Yes, squash.") || got[0].SessionID != 1 ||
		!strings.HasPrefix(got[0].Text, "[T"+itoa(thread.ID)+" · A"+itoa(asks[0].ID)+" answered]") ||
		!strings.Contains(got[0].Text, "A"+itoa(asks[1].ID)+" and A"+itoa(asks[2].ID)+" of T"+itoa(thread.ID)+" still wait for the owner.") {
		t.Fatalf("the note %+v", got)
	}
	// The same request again replays and files nothing; another request is
	// a conflict, and files nothing.
	if _, replayed, err := s.AnswerAsk(ctx, "a1", asks[0].ID, "Yes, squash.", "laptop"); err != nil || !replayed {
		t.Fatalf("the replay: %v %v", replayed, err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a2", asks[0].ID, "No.", "phone"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second answer: %v", err)
	}
	if n := len(notes()); n != 1 {
		t.Fatalf("%d notes after a replay and a second answer", n)
	}
	// A supervisor's message withdraws the other asks: no answer note, and
	// an answer to a withdrawn ask files none either.
	if _, err := s.FileSend(ctx, ReportRow{Key: "team-send:1:a", ThreadID: thread.ID, SessionID: 9, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, change, err := s.SendDelivered(ctx, "team-send:1:a", thread.ID); err != nil || len(change.Withdrawn) != 2 {
		t.Fatalf("the withdrawal %+v %v", change, err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a3", asks[1].ID, "Rebase.", "laptop"); !errors.Is(err, ErrWithdrawn) {
		t.Fatalf("an answer to a withdrawn ask: %v", err)
	}
	if n := len(notes()); n != 1 {
		t.Fatalf("%d notes after a withdrawal", n)
	}

	// A closed thread's supervisor is told, with what it can do.
	closed, _, err := s.CreateThread(ctx, ThreadRow{Key: "c", Title: "Closed", Agent: "duncan", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, closedAsks, _, err := s.FileReport(ctx, "rc", closed.ID, ThreadBlocked, "Q", []AskRow{{Key: "rc:ask:0", SessionID: 8, Question: "Keep it?"}}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseThread(ctx, closed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AnswerAsk(ctx, "a4", closedAsks[0].ID, "Keep it.", "laptop"); err != nil {
		t.Fatal(err)
	}
	if got := notes(); len(got) != 2 || !strings.Contains(got[1].Text, "T"+itoa(closed.ID)+" is closed, so duncan files no report on it") {
		t.Fatalf("a closed thread's note %+v", got)
	}

	// No supervisor session, or the asking session as its own: no note.
	for name, session := range map[string]int64{"no supervisor session": 0, "the asking session": 7} {
		row, _, err := s.CreateThread(ctx, ThreadRow{Key: name, Title: name, Agent: "vera", SupervisorSession: session, Task: "x"})
		if err != nil {
			t.Fatal(err)
		}
		_, own, _, err := s.FileReport(ctx, name+":r", row.ID, ThreadBlocked, "Q", []AskRow{{Key: name + ":ask", SessionID: 7, Question: "Which?"}}, reportText)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.AnswerAsk(ctx, name+":a", own[0].ID, "That.", "laptop"); err != nil {
			t.Fatal(err)
		}
		if n := len(notes()); n != 2 {
			t.Errorf("%s: %d notes", name, n)
		}
	}
}

// An answer's note names at most team.MaxAnsweredOpenAsks of the thread's
// other open asks, the oldest, and counts the rest: a backlog of asks
// never makes each note longer than the bound (Codex review of #175,
// round 4).
func TestAnAnswersNoteNamesABoundedPrefixOfTheOpenAsks(t *testing.T) {
	s, _ := open(t)
	thread, _, err := s.CreateThread(ctx, ThreadRow{Key: "t", Title: "Backlog", Agent: "vera", SupervisorSession: 1, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var asks []AskRow
	for r := range 4 {
		var rows []AskRow
		for i := range team.MaxAsks {
			key := "r" + itoa(int64(r)) + ":ask:" + itoa(int64(i))
			rows = append(rows, AskRow{Key: key, SessionID: 9, Question: "Question " + key + "?"})
		}
		_, filed, _, err := s.FileReport(ctx, "r"+itoa(int64(r)), thread.ID, ThreadBlocked, "Questions.", rows, reportText)
		if err != nil {
			t.Fatal(err)
		}
		asks = append(asks, filed...)
	}
	if len(asks) != 4*team.MaxAsks {
		t.Fatalf("%d asks", len(asks))
	}
	if _, _, err := s.AnswerAsk(ctx, "a", asks[0].ID, "Yes.", "laptop"); err != nil {
		t.Fatal(err)
	}
	note, err := s.Note(ctx, team.AnsweredKey(thread.Owner, asks[0].Key, "Yes."))
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for _, a := range asks[1 : 1+team.MaxAnsweredOpenAsks] {
		named = append(named, "A"+itoa(a.ID))
	}
	more := len(asks) - 1 - team.MaxAnsweredOpenAsks
	want := strings.Join(named, ", ") + " and " + strconv.Itoa(more) + " more of T" + itoa(thread.ID) + " still wait for the owner."
	if !strings.Contains(note.Text, want) || strings.Contains(note.Text, "A"+itoa(asks[1+team.MaxAnsweredOpenAsks].ID)+" ") {
		t.Fatalf("the note %q, want it to end %q", note.Text, want)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
