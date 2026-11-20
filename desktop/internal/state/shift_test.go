package state

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// shifted is thread id, open, with shift sh, changed at Now plus at.
func shifted(id int64, sh proto.Shift, at time.Duration) proto.Thread {
	th := thread(id, proto.ThreadOpen, at)
	th.Shift = &sh
	return th
}

// A shift's line says where it is, and the owner's Stop and Resume follow
// pocketd's rule (ADR-0024, protocol rule 55): none on a thread pocketd
// refuses them for, and no Resume of a shift that ended at its deadline
// (Codex review of #37).
func TestShiftLineAndActions(t *testing.T) {
	deadline := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	starting := shifted(1, proto.Shift{State: proto.ShiftRunning, Round: 1, MaxRounds: 8}, 0)
	starting.State = proto.ThreadStarting
	noSession := shifted(1, proto.Shift{State: proto.ShiftPaused, Round: 1, MaxRounds: 8, Why: "the owner wrote to its session"}, 0)
	noSession.Session = 0
	closed := shifted(1, proto.Shift{State: proto.ShiftEnded, Reason: "closed", Round: 4, MaxRounds: 8}, 0)
	closed.State = proto.ThreadClosed
	closedPaused := shifted(1, proto.Shift{State: proto.ShiftPaused, Round: 2, MaxRounds: 8, Why: "the owner wrote to its session"}, 0)
	closedPaused.State = proto.ThreadClosed
	for name, tc := range map[string]struct {
		th           proto.Thread
		line         string
		stop, resume bool
	}{
		"running":         {shifted(1, proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 8, Deadline: &deadline}, 0), "Shift running: round 3 of 8, until Sat 07:00 UTC.", true, false},
		"summary":         {shifted(1, proto.Shift{State: proto.ShiftSummary, Round: 8, MaxRounds: 8, Why: "it reached its 8 rounds"}, 0), "Shift stopped at round 8 of 8: it reached its 8 rounds. Its report is asked for.", true, false},
		"paused":          {shifted(1, proto.Shift{State: proto.ShiftPaused, Round: 2, MaxRounds: 8, Why: "the owner wrote to its session"}, 0), "Shift paused at round 2 of 8: the owner wrote to its session.", true, true},
		"stopped":         {shifted(1, proto.Shift{State: proto.ShiftEnded, Reason: "stopped", Round: 2, MaxRounds: 8, Why: "the owner stopped it"}, 0), "Shift ended at round 2 of 8: the owner stopped it.", false, true},
		"done":            {shifted(1, proto.Shift{State: proto.ShiftEnded, Reason: "done", Round: 4, MaxRounds: 8, Why: "the stop condition holds"}, 0), "Shift ended", false, false},
		"at its deadline": {shifted(1, proto.Shift{State: proto.ShiftEnded, Reason: "deadline", Round: 2, MaxRounds: 8, Why: "its deadline passed"}, 0), "Shift ended", false, false},
		"no rounds":       {shifted(1, proto.Shift{State: proto.ShiftEnded, Reason: "max-rounds", Round: 8, MaxRounds: 8}, 0), "Shift ended", false, false},
		"closed":          {closed, "Shift ended", false, false},
		"closed, paused":  {closedPaused, "Shift paused", false, false},
		"starting":        {starting, "Shift running", false, false},
		"no session":      {noSession, "Shift paused", false, false},
	} {
		if got := ShiftLine(*tc.th.Shift); !strings.HasPrefix(got, tc.line) {
			t.Errorf("%s: %q, want %q", name, got, tc.line)
		}
		if got := CanStopShift(tc.th); got != tc.stop {
			t.Errorf("%s: stop %v, want %v", name, got, tc.stop)
		}
		if got := CanResumeShift(tc.th); got != tc.resume {
			t.Errorf("%s: resume %v, want %v", name, got, tc.resume)
		}
	}
	if CanStopShift(thread(1, proto.ThreadOpen, 0)) || CanResumeShift(thread(1, proto.ThreadOpen, 0)) {
		t.Error("a thread with no shift offers a shift's action")
	}
}

// A paused shift waits for the owner, and a running one for its
// specialist, whatever its thread's last report said: a stop files a
// blocked report, which a resume leaves (Codex review of #37). An ended
// shift leaves the thread to the usual rule, and the owner's other gates
// still come first.
func TestAShiftsThreadIsMovedByWhoItWaitsFor(t *testing.T) {
	var d Data
	for name, tc := range map[string]struct {
		state proto.ThreadState
		shift proto.ShiftState
		want  Mover
	}{
		"paused":            {proto.ThreadOpen, proto.ShiftPaused, MoverOwner},
		"resumed, blocked":  {proto.ThreadBlocked, proto.ShiftRunning, MoverSpecialist},
		"summary, done":     {proto.ThreadDone, proto.ShiftSummary, MoverSpecialist},
		"stopped, blocked":  {proto.ThreadBlocked, proto.ShiftEnded, MoverSupervisor},
		"running, closed":   {proto.ThreadClosed, proto.ShiftRunning, MoverNobody},
		"paused and closed": {proto.ThreadClosed, proto.ShiftPaused, MoverNobody},
	} {
		th := shifted(1, proto.Shift{State: tc.shift, Round: 2, MaxRounds: 8}, 0)
		th.State = tc.state
		if got := d.NextMover(th); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}

	d.SetAsks(d.BeginAsks(), []proto.Ask{{ID: 1, Thread: 1, Question: "Which runner?", State: proto.AskOpen}})
	th := shifted(1, proto.Shift{State: proto.ShiftRunning, Round: 2, MaxRounds: 8}, 0)
	if got := d.NextMover(th); got != MoverOwner {
		t.Errorf("a running shift's thread with an open ask: %v, want the owner", got)
	}
}

// A shift's stop or resume answers with the thread as pocketd had it then,
// and the thread goes on changing: the copy with the later updatedAt wins,
// whichever came first, and a fetch that may have read the thread before
// the change is fetched again (Codex review of #37).
func TestAShiftChangeReplyNeverUndoesANewerThread(t *testing.T) {
	at := func(round int) proto.Thread {
		return shifted(1, proto.Shift{State: proto.ShiftRunning, Round: round, MaxRounds: 8}, time.Duration(round)*time.Minute)
	}
	roundOf := func(d *Data) int {
		th, _ := d.Thread(1)
		return th.Shift.Round
	}
	loaded := func() *Data {
		d := &Data{}
		d.SetThreads(d.BeginThreads(), []proto.Thread{at(1)})
		return d
	}

	d := loaded()
	if refetch := d.ThreadChanged(d.BeginThreadChange(), at(2)); refetch || roundOf(d) != 2 {
		t.Errorf("alone: round %d, refetch %v; want the reply, no fetch", roundOf(d), refetch)
	}

	d = loaded()
	f := d.BeginThreadChange()
	ev := at(3)
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &ev})
	if refetch := d.ThreadChanged(f, at(2)); refetch || roundOf(d) != 3 {
		t.Errorf("after a newer event: round %d, refetch %v; want the event's, no fetch", roundOf(d), refetch)
	}

	d = loaded()
	f = d.BeginThreadChange()
	ev = at(1)
	ev.Report = "an older event, sent before the change"
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &ev})
	if refetch := d.ThreadChanged(f, at(2)); refetch || roundOf(d) != 2 {
		t.Errorf("after an older event: round %d, refetch %v; want the reply, no fetch", roundOf(d), refetch)
	}

	d = loaded()
	f = d.BeginThreadChange()
	d.SetThreads(d.BeginThreads(), []proto.Thread{at(3)})
	if refetch := d.ThreadChanged(f, at(2)); refetch || roundOf(d) != 3 {
		t.Errorf("after a newer fetch: round %d, refetch %v; want the fetch's, no fetch", roundOf(d), refetch)
	}

	// A fetch begun before the change that read the thread after it, and
	// answered before the reply.
	d = loaded()
	before := d.BeginThreads()
	f = d.BeginThreadChange()
	d.SetThreads(before, []proto.Thread{at(3)})
	if refetch := d.ThreadChanged(f, at(2)); refetch || roundOf(d) != 3 {
		t.Errorf("after a newer fetch begun before: round %d, refetch %v; want the fetch's, no fetch", roundOf(d), refetch)
	}

	// A fetch that failed brought no copy, and takes none from the reply.
	d = loaded()
	f = d.BeginThreadChange()
	d.SetThreadsError(d.BeginThreads(), "offline")
	if refetch := d.ThreadChanged(f, at(2)); refetch || roundOf(d) != 2 {
		t.Errorf("after a failed fetch: round %d, refetch %v; want the reply, no fetch", roundOf(d), refetch)
	}

	d = loaded()
	before = d.BeginThreads()
	f = d.BeginThreadChange()
	if refetch := d.ThreadChanged(f, at(2)); !refetch || roundOf(d) != 2 {
		t.Errorf("a fetch in flight: round %d, refetch %v; want the reply, and a fetch", roundOf(d), refetch)
	}
	d.SetThreads(before, []proto.Thread{at(1)})
	if roundOf(d) != 2 {
		t.Errorf("a fetch begun before the change undid its reply: round %d", roundOf(d))
	}

	d = loaded()
	f = d.BeginThreadChange()
	after := d.BeginThreads()
	if refetch := d.ThreadChanged(f, at(2)); !refetch {
		t.Error("a fetch begun after the change, in flight: no fetch asked for")
	}
	d.SetThreads(after, []proto.Thread{at(3)})
	if roundOf(d) != 3 {
		t.Errorf("a fetch begun after the change lost to its reply: round %d", roundOf(d))
	}

	// Two copies of one moment that differ cannot be ordered.
	d = loaded()
	f = d.BeginThreadChange()
	same := at(2)
	same.Report = "another change in the same millisecond"
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &same})
	if refetch := d.ThreadChanged(f, at(2)); !refetch {
		t.Error("a copy as old as the reply that differs: no fetch asked for")
	}
	if refetch := d.ThreadChanged(d.BeginThreadChange(), same); refetch {
		t.Error("the copy held again: a fetch asked for")
	}
}

// A stop whose answer was lost keeps its request id only for the shift it
// acted on: after a resume, the next stop is a new request, which pocketd
// carries out, never a retry it answers with the running shift (Codex
// review of #37).
func TestAStopWhoseAnswerWasLostDoesNotTargetTheNextStop(t *testing.T) {
	h := newHarness(t)
	h.srv.AddThread(shifted(4, proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 8}, 0))
	h.connect(t)
	stop := proto.Path(proto.PathStopShift, 4)
	shiftOf := func() proto.Shift { th, _ := h.store.Data.Thread(4); return *th.Shift }

	h.srv.LoseAnswerOnce(http.MethodPost, stop)
	h.store.StopShift(4)
	h.loop.RunUntil(t, func() bool {
		return !h.store.ShiftChanging(4).Busy && shiftOf().State == proto.ShiftEnded && h.store.InFlight() == 0
	})
	// The shift moved as asked: the lost reply's failure says nothing more
	// (Codex review of #37, round 2).
	if err := h.store.ShiftChanging(4).Err; err != "" {
		t.Errorf("a stop pocketd carried out still shows its lost reply: %q", err)
	}
	// Each step waits for the stream too: the resume's event, delivered
	// after the second stop's reply, would show the shift running until
	// the stop's own event came (pocketgo#42).
	caughtUp := func() bool { return h.store.lastSeq == h.srv.Seq() }
	h.store.ResumeShift(4)
	h.loop.RunUntil(t, func() bool { return shiftOf().State == proto.ShiftRunning && h.store.InFlight() == 0 && caughtUp() })
	h.store.StopShift(4)
	h.loop.RunUntil(t, func() bool { return !h.store.ShiftChanging(4).Busy && h.store.InFlight() == 0 && caughtUp() })
	if got := sentIDs(t, h, stop); len(got) != 2 || got[0] == got[1] {
		t.Errorf("stop request ids %q: the second stop reused the first's", got)
	}
	if sh := shiftOf(); sh.State != proto.ShiftEnded || sh.Round != 4 {
		t.Errorf("the second stop did not stop the resumed shift: %+v", sh)
	}

	// A resume whose answer was lost, then a pause at the next round: the
	// next resume is a new request too.
	resume := proto.Path(proto.PathResumeShift, 6)
	paused := func(round int) proto.Thread {
		return shifted(6, proto.Shift{State: proto.ShiftPaused, Round: round, MaxRounds: 8, Why: "the owner wrote to its session"}, time.Duration(round)*time.Minute)
	}
	h.srv.AddThread(paused(2))
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Thread(6); return ok && h.store.InFlight() == 0 })
	h.srv.LoseAnswerOnce(http.MethodPost, resume)
	h.store.ResumeShift(6)
	h.loop.RunUntil(t, func() bool {
		th, _ := h.store.Data.Thread(6)
		return !h.store.ShiftChanging(6).Busy && th.Shift.State == proto.ShiftRunning && h.store.InFlight() == 0
	})
	if err := h.store.ShiftChanging(6).Err; err != "" {
		t.Errorf("a resume pocketd carried out still shows its lost reply: %q", err)
	}
	again := paused(3)
	h.srv.AddThread(again)
	h.srv.Emit(proto.Event{Type: proto.EventThread, Thread: &again})
	h.loop.RunUntil(t, func() bool { th, _ := h.store.Data.Thread(6); return th.Shift.State == proto.ShiftPaused })
	h.store.ResumeShift(6)
	h.loop.RunUntil(t, func() bool { return !h.store.ShiftChanging(6).Busy && h.store.InFlight() == 0 })
	if got := sentIDs(t, h, resume); len(got) != 2 || got[0] == got[1] {
		t.Errorf("resume request ids %q: the second pause's resume reused the first's", got)
	}
	if th, _ := h.store.Data.Thread(6); th.Shift.State != proto.ShiftRunning || th.Shift.Round != 4 {
		t.Errorf("the second resume did not resume the paused shift: %+v", th.Shift)
	}

	// A last round's stop whose event comes before its lost reply: no
	// action is left on the card, and no failure stays on it.
	h.srv.AddThread(shifted(7, proto.Shift{State: proto.ShiftRunning, Round: 8, MaxRounds: 8}, 0))
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Thread(7); return ok && h.store.InFlight() == 0 })
	release := h.srv.HoldAndLoseAnswerOnce(http.MethodPost, proto.Path(proto.PathStopShift, 7))
	h.store.StopShift(7)
	h.loop.RunUntil(t, func() bool { th, _ := h.store.Data.Thread(7); return th.Shift.State == proto.ShiftEnded })
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.ShiftChanging(7).Busy && h.store.InFlight() == 0 })
	if err := h.store.ShiftChanging(7).Err; err != "" {
		t.Errorf("a stop whose event came first still shows its lost reply: %q", err)
	}

	// A stop tried again on the shift it acted on keeps its id.
	h.srv.AddThread(shifted(5, proto.Shift{State: proto.ShiftRunning, Round: 1, MaxRounds: 8}, 0))
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Thread(5); return ok && h.store.InFlight() == 0 })
	h.srv.FailOnce(http.MethodPost, proto.Path(proto.PathStopShift, 5), http.StatusServiceUnavailable)
	h.store.StopShift(5)
	h.loop.RunUntil(t, func() bool { return h.store.ShiftChanging(5).Err != "" && h.store.InFlight() == 0 })
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { return h.store.InFlight() == 0 })
	if h.store.ShiftChanging(5).Err == "" {
		t.Error("a stop that failed on a shift that did not move lost its failure")
	}
	h.store.StopShift(5)
	h.loop.RunUntil(t, func() bool { return !h.store.ShiftChanging(5).Busy && h.store.InFlight() == 0 })
	if got := sentIDs(t, h, proto.Path(proto.PathStopShift, 5)); len(got) != 2 || got[0] != got[1] {
		t.Errorf("stop request ids %q: a stop tried again unchanged took a new id", got)
	}
}

// A threads fetch begun while a shift's stop was on its way may read the
// thread before the stop committed and answer after its reply: its older
// copy never undoes the reply, even when the fetch that follows fails, and
// a copy marked as of the change never lowers an event's marker (Codex
// review of #37, round 2).
func TestAStaleFetchNeverUndoesAShiftChange(t *testing.T) {
	running := shifted(1, proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 8}, time.Minute)
	stopped := shifted(1, proto.Shift{State: proto.ShiftEnded, Reason: "stopped", Round: 3, MaxRounds: 8}, 2*time.Minute)
	stateOf := func(d *Data) proto.ShiftState {
		th, _ := d.Thread(1)
		return th.Shift.State
	}

	var d Data
	d.SetThreads(d.BeginThreads(), []proto.Thread{running})
	f := d.BeginThreadChange()
	stale := d.BeginThreads()
	if refetch := d.ThreadChanged(f, stopped); !refetch {
		t.Error("a fetch in flight: no fetch asked for")
	}
	d.SetThreads(stale, []proto.Thread{running})
	d.SetThreadsError(d.BeginThreads(), "offline")
	if got := stateOf(&d); got != proto.ShiftEnded {
		t.Errorf("a fetch that read the thread before the stop undid it: %s", got)
	}

	// An event after the fetch began, then the change's reply, newer: the
	// fetch's copy, as old as the reply, does not replace it.
	d = Data{}
	d.SetThreads(d.BeginThreads(), []proto.Thread{running})
	f = d.BeginThreadChange()
	g := d.BeginThreads()
	ev := shifted(1, proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 8}, 90*time.Second)
	d.Apply(proto.Event{Type: proto.EventThread, Thread: &ev})
	d.ThreadChanged(f, stopped)
	same := stopped
	same.Report = "another change in the same millisecond"
	d.SetThreads(g, []proto.Thread{same})
	if th, _ := d.Thread(1); th.Report != stopped.Report {
		t.Errorf("a fetch the event beat replaced the reply: %q", th.Report)
	}
}
