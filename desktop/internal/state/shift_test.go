package state

import (
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A shift's line says where it is, and the owner's Stop and Resume follow
// pocketd's rule (ADR-0024, protocol rule 55).
func TestShiftLineAndActions(t *testing.T) {
	deadline := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		sh           proto.Shift
		line         string
		stop, resume bool
	}{
		"running":   {proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 8, Deadline: &deadline}, "Shift running: round 3 of 8, until Sat 07:00 UTC.", true, false},
		"summary":   {proto.Shift{State: proto.ShiftSummary, Round: 8, MaxRounds: 8, Why: "it reached its 8 rounds"}, "Shift stopped at round 8 of 8: it reached its 8 rounds. Its report is asked for.", true, false},
		"paused":    {proto.Shift{State: proto.ShiftPaused, Round: 2, MaxRounds: 8, Why: "the owner wrote to its session"}, "Shift paused at round 2 of 8: the owner wrote to its session.", true, true},
		"stopped":   {proto.Shift{State: proto.ShiftEnded, Reason: "stopped", Round: 2, MaxRounds: 8, Why: "the owner stopped it"}, "Shift ended at round 2 of 8: the owner stopped it.", false, true},
		"done":      {proto.Shift{State: proto.ShiftEnded, Reason: "done", Round: 4, MaxRounds: 8, Why: "the stop condition holds"}, "Shift ended", false, false},
		"closed":    {proto.Shift{State: proto.ShiftEnded, Reason: "closed", Round: 4, MaxRounds: 8}, "Shift ended", false, false},
		"no rounds": {proto.Shift{State: proto.ShiftEnded, Reason: "max-rounds", Round: 8, MaxRounds: 8}, "Shift ended", false, false},
	} {
		if got := ShiftLine(tc.sh); !strings.HasPrefix(got, tc.line) {
			t.Errorf("%s: %q, want %q", name, got, tc.line)
		}
		if got := CanStopShift(tc.sh); got != tc.stop {
			t.Errorf("%s: stop %v, want %v", name, got, tc.stop)
		}
		if got := CanResumeShift(tc.sh); got != tc.resume {
			t.Errorf("%s: resume %v, want %v", name, got, tc.resume)
		}
	}
}

// A shift's stop or resume answers with the thread as pocketd had it then,
// and the thread goes on changing: the reply never undoes what the client
// saw since the request began, and a fetch that may have read the thread
// before the change is fetched again.
func TestAShiftChangeReplyNeverUndoesANewerThread(t *testing.T) {
	at := func(round int) proto.Thread {
		th := thread(1, proto.ThreadOpen, time.Duration(round)*time.Minute)
		th.Shift = &proto.Shift{State: proto.ShiftRunning, Round: round, MaxRounds: 8}
		return th
	}
	roundOf := func(d *Data) int {
		th, _ := d.Thread(1)
		return th.Shift.Round
	}

	var d Data
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(1)})
	if refetch := d.ThreadChanged(d.BeginThreadChange(), at(2)); refetch || roundOf(&d) != 2 {
		t.Errorf("alone: round %d, refetch %v; want the reply, no fetch", roundOf(&d), refetch)
	}

	d = Data{}
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(1)})
	f := d.BeginThreadChange()
	ev := at(3)
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &ev})
	if refetch := d.ThreadChanged(f, at(2)); refetch || roundOf(&d) != 3 {
		t.Errorf("after an event: round %d, refetch %v; want the event's, no fetch", roundOf(&d), refetch)
	}

	d = Data{}
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(1)})
	f = d.BeginThreadChange()
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(3)})
	if refetch := d.ThreadChanged(f, at(2)); !refetch || roundOf(&d) != 3 {
		t.Errorf("after a fetch: round %d, refetch %v; want the fetch's, and a fetch", roundOf(&d), refetch)
	}

	d = Data{}
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(1)})
	before := d.BeginThreads()
	f = d.BeginThreadChange()
	if refetch := d.ThreadChanged(f, at(2)); !refetch || roundOf(&d) != 2 {
		t.Errorf("a fetch in flight: round %d, refetch %v; want the reply, and a fetch", roundOf(&d), refetch)
	}
	d.SetThreads(before, []proto.Thread{at(1)})
	if roundOf(&d) != 2 {
		t.Errorf("a fetch begun before the change undid its reply: round %d", roundOf(&d))
	}

	d = Data{}
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(1)})
	f = d.BeginThreadChange()
	after := d.BeginThreads()
	if refetch := d.ThreadChanged(f, at(2)); !refetch {
		t.Error("a fetch begun after the change, in flight: no fetch asked for")
	}
	d.SetThreads(after, []proto.Thread{at(3)})
	if roundOf(&d) != 3 {
		t.Errorf("a fetch begun after the change lost to its reply: round %d", roundOf(&d))
	}
}
