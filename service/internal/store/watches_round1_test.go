package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// A session's list of watches is one snapshot: a watch a pass ends
// between the reads of the live and the ended ones is in one list, never
// both (Codex review of #173, round 1).
func TestSessionWatchesReadsOneSnapshot(t *testing.T) {
	s, _ := open(t)
	w, _, err := s.AddWatch(ctx, watchRow("team:5:1:a", 5), 16, 64, said)
	if err != nil {
		t.Fatal(err)
	}
	s.watchesRead = func() {
		s.watchesRead = nil
		if _, _, err := s.EndWatch(ctx, w.ID, team.WatchMet, "merged", "[W1] merged"); err != nil {
			t.Error(err)
		}
	}
	listed, err := s.SessionWatches(ctx, 5, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != w.ID || listed[0].State != WatchLive {
		t.Fatalf("the list %+v %v", listed, err)
	}
	if listed, _ := s.SessionWatches(ctx, 5, 10); len(listed) != 1 || listed[0].State != WatchEnded {
		t.Fatalf("the list after the end %+v", listed)
	}
}

// A closed thread's specialist keeps no live watch, checked in the
// transaction that would keep it, which a close comes before or after;
// one answered at once, which nothing checks, is kept as ended (Codex
// review of #173, round 1).
func TestAClosedThreadsSpecialistKeepsNoLiveWatch(t *testing.T) {
	s, _ := open(t)
	row, _, err := s.CreateThread(ctx, ThreadRow{Key: "d1", Title: "Fix CI", Agent: "vera", SupervisorSession: 1, Task: "Fix it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachThread(ctx, row.ID, 9, "/w", "", "brief"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddWatch(ctx, watchRow("open", 9), 16, 64, said); err != nil {
		t.Fatalf("an open thread's specialist: %v", err)
	}
	if _, err := s.CloseThread(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddWatch(ctx, watchRow("closed", 9), 16, 64, said); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "T1 is closed") {
		t.Fatalf("a closed thread's specialist: %v", err)
	}
	if _, err := s.WatchByKey(ctx, "closed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused add kept %v", err)
	}
	at := watchRow("at once", 9)
	at.State, at.Outcome, at.Seen = WatchEnded, team.WatchMet, "merged"
	if made, _, err := s.AddWatch(ctx, at, 16, 64, said); err != nil || made.State != WatchEnded {
		t.Fatalf("an answer at once %+v %v", made, err)
	}
	// The supervisor, whose session is no thread's, watches on.
	if _, _, err := s.AddWatch(ctx, watchRow("supervisor", 1), 16, 64, said); err != nil {
		t.Fatalf("the supervisor: %v", err)
	}
}
