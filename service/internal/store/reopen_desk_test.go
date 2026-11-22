package store

import (
	"errors"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// A delegation recorded under the desk's cap counts the desk again in its
// own transaction: a reopen that took the last place since the delegation
// counted it leaves the new thread unrecorded; a delegation run again finds
// its own thread whatever the desk holds (Codex review of #121, round 8).
func TestARecordedDelegationChecksTheDeskAgain(t *testing.T) {
	s, _ := open(t)
	openedThread(t, s, "k1", 9)
	if _, _, err := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x", DeskCap: 1}); !errors.Is(err, team.ErrDeskFull) {
		t.Fatalf("a delegation over the cap: %v", err)
	}
	if _, err := s.ThreadByKey(ctx, "k2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused delegation was recorded: %v", err)
	}
	made, existed, err := s.CreateThread(ctx, ThreadRow{Key: "k3", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x", DeskCap: 2})
	if err != nil || existed {
		t.Fatalf("a delegation with a place free: %+v %v %v", made, existed, err)
	}
	if again, existed, err := s.CreateThread(ctx, ThreadRow{Key: "k3", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x", DeskCap: 2}); err != nil || !existed || again.ID != made.ID {
		t.Fatalf("the delegation run again: %+v %v %v", again, existed, err)
	}
}
