package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// watchRow is a live watch of session, made by call key.
func watchRow(key string, session int64) WatchRow {
	return WatchRow{Key: key, SessionID: session, Repository: "frostyard/example", Number: 12, GitHubID: 1012, Until: team.UntilChecks, Hours: 2,
		Deadline: time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)}
}

// said is an add's answer: the watch's id and key.
func said(w WatchRow) string { return "W" + strconv.FormatInt(w.ID, 10) + " " + w.Key }

// A watch is kept once per call, with its answer: the same call run again
// gets the watch it made and its answer, whatever the counts are by then.
// A live one counts against its session's bound and pocketd's, in the
// transaction that keeps it; one that ended as it was read counts against
// neither (ADR-0072).
func TestAWatchIsKeptOncePerCall(t *testing.T) {
	s, _ := open(t)
	first, existed, err := s.AddWatch(ctx, watchRow("team:5:1:a", 5), 2, 3, said)
	if err != nil || existed || first.State != WatchLive || len(first.Owner) != 16 || first.Ref() != "frostyard/example#12" || first.EndedAt != nil ||
		first.GitHubID != 1012 || first.Answer != "W1 team:5:1:a" {
		t.Fatalf("add %+v %v %v", first, existed, err)
	}
	if again, existed, err := s.AddWatch(ctx, watchRow("team:5:1:a", 5), 0, 0, func(WatchRow) string { return "other" }); err != nil || !existed ||
		again.ID != first.ID || again.Owner != first.Owner || again.Answer != first.Answer {
		t.Fatalf("the call again %+v %v %v", again, existed, err)
	}
	if found, err := s.WatchByKey(ctx, "team:5:1:a"); err != nil || found.ID != first.ID {
		t.Fatalf("by key %+v %v", found, err)
	}
	if _, err := s.WatchByKey(ctx, "team:5:1:z"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such call: %v", err)
	}
	if _, _, err := s.AddWatch(ctx, watchRow("team:5:1:b", 5), 2, 3, said); err != nil {
		t.Fatal(err)
	}
	// Session 5 has two: its third is refused, and keeps nothing.
	if _, _, err := s.AddWatch(ctx, watchRow("team:5:1:c", 5), 2, 3, said); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "at most 2 live watches") {
		t.Fatalf("a session's third: %v", err)
	}
	if _, err := s.WatchByKey(ctx, "team:5:1:c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused add kept %v", err)
	}
	// One that ended as it was read takes no room.
	at := watchRow("team:5:1:d", 5)
	at.State, at.Outcome, at.Seen = WatchEnded, team.WatchMet, "merged"
	if made, _, err := s.AddWatch(ctx, at, 2, 3, said); err != nil || made.State != WatchEnded || made.Outcome != team.WatchMet || made.EndedAt == nil {
		t.Fatalf("an add that ended at once %+v %v", made, err)
	}
	if _, _, err := s.AddWatch(ctx, watchRow("team:6:1:a", 6), 2, 3, said); err != nil {
		t.Fatal(err)
	}
	// pocketd has three: the next is refused, whichever session asks.
	if _, _, err := s.AddWatch(ctx, watchRow("team:7:1:a", 7), 2, 3, said); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "across the team") {
		t.Fatalf("pocketd's fourth: %v", err)
	}
	live, err := s.LiveWatches(ctx)
	if err != nil || len(live) != 3 {
		t.Fatalf("live %+v %v", live, err)
	}
	// A cancel frees room, files no note, and names only the session's own.
	if _, _, err := s.CancelWatch(ctx, first.ID, 6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another session's cancel: %v", err)
	}
	cancelled, done, err := s.CancelWatch(ctx, first.ID, 5)
	if err != nil || !done || cancelled.State != WatchEnded || cancelled.Outcome != team.WatchCancelled {
		t.Fatalf("cancel %+v %v %v", cancelled, done, err)
	}
	if again, done, err := s.CancelWatch(ctx, first.ID, 5); err != nil || done || again.Outcome != team.WatchCancelled {
		t.Fatalf("a cancel again %+v %v %v", again, done, err)
	}
	if _, err := s.Note(ctx, cancelled.NoteKey()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a cancel filed a note: %v", err)
	}
	if _, _, err := s.AddWatch(ctx, watchRow("team:7:1:a", 7), 2, 3, said); err != nil {
		t.Fatalf("after the cancel: %v", err)
	}
	listed, err := s.SessionWatches(ctx, 5, 10)
	if err != nil || len(listed) != 3 || listed[0].State != WatchLive || listed[1].State != WatchEnded || listed[2].State != WatchEnded {
		t.Fatalf("session 5's list %+v %v", listed, err)
	}
	if listed, _ := s.SessionWatches(ctx, 5, 1); len(listed) != 2 {
		t.Fatalf("session 5's list of one ended: %+v", listed)
	}
}

// A watch's end files its one note to its session, in the transaction
// that ends it, keyed by its owner; an end run again, a second pass's or
// one after a restart, ends nothing and files nothing more, and a cancel
// after it changes nothing (ADR-0072).
func TestAWatchsEndFilesItsNoteOnce(t *testing.T) {
	s, path := open(t)
	w, _, err := s.AddWatch(ctx, watchRow("team:5:1:a", 5), 16, 64, said)
	if err != nil {
		t.Fatal(err)
	}
	ended, done, err := s.EndWatch(ctx, w.ID, team.WatchMet, "open, CI at aaaa green", "[W1] green")
	if err != nil || !done || ended.State != WatchEnded || ended.Outcome != team.WatchMet || ended.Seen != "open, CI at aaaa green" || ended.EndedAt == nil {
		t.Fatalf("end %+v %v %v", ended, done, err)
	}
	note, err := s.Note(ctx, WatchNotePrefix+w.Owner)
	if err != nil || note.SessionID != 5 || note.Text != "[W1] green" || note.SentAt != nil {
		t.Fatalf("the note %+v %v", note, err)
	}
	// A second pass, with another outcome, files nothing.
	if again, done, err := s.EndWatch(ctx, w.ID, team.WatchExpired, "open", "[W1] time is up"); err != nil || done || again.Outcome != team.WatchMet {
		t.Fatalf("an end again %+v %v %v", again, done, err)
	}
	// So does one after a restart.
	_ = s.Close()
	s, _ = openPath(t, path)
	if _, done, err := s.EndWatch(ctx, w.ID, team.WatchExpired, "open", "[W1] time is up"); err != nil || done {
		t.Fatalf("an end after a restart %v %v", done, err)
	}
	if _, done, _ := s.CancelWatch(ctx, w.ID, 5); done {
		t.Fatal("a cancel ended an ended watch")
	}
	var notes int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE session_id = 5`).Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("%d notes, %v", notes, err)
	}
	if note, _ := s.Note(ctx, WatchNotePrefix+w.Owner); note.Text != "[W1] green" {
		t.Fatalf("the note became %q", note.Text)
	}
	if _, _, err := s.EndWatch(ctx, 999, team.WatchMet, "x", "y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such watch: %v", err)
	}
	if _, _, err := s.EndWatch(ctx, w.ID, "", "x", ""); err == nil {
		t.Fatal("an end with no note")
	}
}

// A thread's close, whatever closes it, ends its specialist's live
// watches with no note and drops a note an ended one filed that is not
// sent; another session's watch goes on (ADR-0072).
func TestAThreadsCloseEndsItsSpecialistsWatches(t *testing.T) {
	s, _ := open(t)
	for name, closeIt := range map[string]func(ThreadRow) error{
		"a close": func(row ThreadRow) error { _, err := s.CloseThread(ctx, row.ID); return err },
		"a close by request": func(row ThreadRow) error {
			_, err := s.CloseThreadAs(ctx, "close:"+row.Key, row.ID)
			return err
		},
		"a merge": func(row ThreadRow) error {
			if _, _, _, _, err := s.FileReportWithPulls(ctx, "r:"+row.Key, row.ID, ThreadOpen, "PR", nil, []PullRow{pull("frostyard/example", row.ID, PullMerged)}, pullReportText); err != nil {
				return err
			}
			_, _, err := s.CloseMergedThread(ctx, row.ID, func([]PullRow) string { return "merged" })
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			row, _, err := s.CreateThread(ctx, ThreadRow{Key: "d:" + name, Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "Fix it"})
			if err != nil {
				t.Fatal(err)
			}
			session := 100 + row.ID
			if row, err = s.AttachThread(ctx, row.ID, session, "/w", "", "brief"); err != nil {
				t.Fatal(err)
			}
			if row, err = s.OpenThread(ctx, row.ID); err != nil {
				t.Fatal(err)
			}
			live, _, err := s.AddWatch(ctx, watchRow("live:"+name, session), 16, 64, said)
			if err != nil {
				t.Fatal(err)
			}
			met, _, err := s.AddWatch(ctx, watchRow("met:"+name, session), 16, 64, said)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.EndWatch(ctx, met.ID, team.WatchMet, "merged", "[W] merged"); err != nil {
				t.Fatal(err)
			}
			supervisor, _, err := s.AddWatch(ctx, watchRow("super:"+name, 1), 16, 64, said)
			if err != nil {
				t.Fatal(err)
			}
			if err := closeIt(row); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.WatchByKey(ctx, "live:"+name); got.State != WatchEnded || got.Outcome != team.WatchThreadClosed {
				t.Fatalf("the specialist's watch %+v", got)
			}
			if note, _ := s.Note(ctx, met.NoteKey()); note.DroppedAt == nil {
				t.Fatalf("the note of an ended watch went on: %+v", note)
			}
			if got, _ := s.WatchByKey(ctx, "super:"+name); got.State != WatchLive {
				t.Fatalf("the supervisor's watch %+v", got)
			}
			if _, done, _ := s.EndWatch(ctx, live.ID, team.WatchMet, "x", "late"); done {
				t.Fatal("a closed thread's watch ended again")
			}
			if _, done, err := s.CancelWatch(ctx, supervisor.ID, 1); err != nil || !done {
				t.Fatalf("the supervisor's cancel %v %v", done, err)
			}
		})
	}
}

// A plan's done closes its own thread in the transaction of the last
// task's close: its lead's watches end with it (ADR-0072).
func TestAPlansDoneEndsItsLeadsWatches(t *testing.T) {
	s, _ := open(t)
	planThread, facts := approvedPlan(t, s, "p1")
	plan := facts.Plan.ID
	lead, _, err := s.AddWatch(ctx, watchRow("lead", planThread.Session), 16, 64, said)
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range []string{"t1", "t2", "t3"} {
		thread := deliver(t, s, plan, "d"+task, task)
		if _, _, _, err := s.FileReport(ctx, "r"+task, thread.ID, ThreadDone, "Done", nil, reportText); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseThread(ctx, thread.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.WatchByKey(ctx, "lead"); i < 2 && got.State != WatchLive {
			t.Fatalf("after %s the lead's watch %+v", task, got)
		}
	}
	if f, _ := s.PlanFacts(ctx, plan); f.Plan.State != team.PlanDone || f.Thread.State != ThreadClosed {
		t.Fatalf("the plan %s, its thread %s", f.Plan.State, f.Thread.State)
	}
	if got, _ := s.WatchByKey(ctx, "lead"); got.State != WatchEnded || got.Outcome != team.WatchThreadClosed {
		t.Fatalf("the lead's watch %+v", got)
	}
	if _, err := s.Note(ctx, lead.NoteKey()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the close filed a note: %v", err)
	}
}
