package state

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A close run again after a reopen answers with the thread as it is, not
// closed: that reply is a snapshot, which never undoes a later report's
// copy (Codex review of #121, round 2).
func TestAnOpenCloseReplyNeverUndoesALaterReport(t *testing.T) {
	d := &Data{}
	open := thread(1, proto.ThreadOpen, time.Minute)
	at := open.UpdatedAt
	open.Reopens, open.ReopenedAt = 1, &at
	d.SetThreads(d.BeginThreads(), []proto.Thread{open})
	done := thread(1, proto.ThreadDone, 2*time.Minute)
	done.Reopens, done.ReopenedAt = 1, &at
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &done})
	d.ThreadClosed(open)
	if th, _ := d.Thread(1); th.State != proto.ThreadDone {
		t.Fatalf("the open reply undid the later report: %s", th.State)
	}
}

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

// A close's reply never undoes a reopen that came after it: the close's
// event, then the reopen's, then the reply leave the thread open, marked
// reopened (ADR-0061; Codex review of #121, round 1).
func TestACloseReplyNeverUndoesALaterReopen(t *testing.T) {
	d := &Data{}
	d.SetThreads(d.BeginThreads(), []proto.Thread{thread(1, proto.ThreadOpen, 0)})
	apply := func(th proto.Thread) { d.Apply(proto.Event{Type: proto.EventThread, Thread: &th}) }
	closed := thread(1, proto.ThreadClosed, time.Minute)
	apply(closed)
	reopened := thread(1, proto.ThreadOpen, 2*time.Minute)
	at := closed.UpdatedAt.Add(time.Minute)
	reopened.Reopens, reopened.ReopenedAt = 1, &at
	apply(reopened)
	d.ThreadClosed(closed)
	if th, _ := d.Thread(1); th.State != proto.ThreadOpen || !Reopened(th) {
		t.Fatalf("the close's late reply undid the reopen: %+v", th)
	}
	// The reply of a close after the reopen closes it.
	again := thread(1, proto.ThreadClosed, 3*time.Minute)
	again.Reopens, again.ReopenedAt = 1, &at
	d.ThreadClosed(again)
	if th, _ := d.Thread(1); th.State != proto.ThreadClosed {
		t.Fatalf("the later close's reply: %+v", th)
	}
}
