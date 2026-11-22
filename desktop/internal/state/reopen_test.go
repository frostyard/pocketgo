package state

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A thread a message reopened is marked so while it is not closed again
// (ADR-0061).
func TestReopened(t *testing.T) {
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		thread proto.Thread
		want   bool
	}{
		"never reopened":   {proto.Thread{State: proto.ThreadOpen}, false},
		"reopened, open":   {proto.Thread{State: proto.ThreadOpen, ReopenedAt: &at}, true},
		"reopened, done":   {proto.Thread{State: proto.ThreadDone, ReopenedAt: &at}, true},
		"closed again":     {proto.Thread{State: proto.ThreadClosed, ReopenedAt: &at}, false},
		"closed, never so": {proto.Thread{State: proto.ThreadClosed}, false},
	} {
		if got := Reopened(tc.thread); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// A thread's card names the thread it carries on and the one that carries
// it on (ADR-0061).
func TestThreadLinks(t *testing.T) {
	for _, tc := range []struct {
		thread proto.Thread
		want   string
	}{
		{proto.Thread{}, ""},
		{proto.Thread{Follows: 77}, "Carries on T77."},
		{proto.Thread{Successor: 84}, "Carried on in T84."},
		{proto.Thread{Follows: 77, Successor: 90}, "Carries on T77. Carried on in T90."},
	} {
		if got := ThreadLinks(tc.thread); got != tc.want {
			t.Errorf("ThreadLinks(%+v) = %q, want %q", tc.thread, got, tc.want)
		}
	}
}
