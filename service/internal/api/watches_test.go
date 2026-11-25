package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// watchGitHub is GitHub as the watches read it: frostyard/example#7,
// open at headA, with CI pending there.
func watchGitHub(h *teamHarness) *fakeGitHub {
	g := &fakeGitHub{repo: "frostyard/example", number: 7, branch: "vera/fix", head: headA, state: "open", labels: map[string]bool{},
		ci: map[string]string{headA: pendingRuns}}
	h.engine.mu.Lock()
	h.engine.onGH = g.gh
	h.engine.mu.Unlock()
	return g
}

// watchUntil is an add of a watch of frostyard/example#7 until until.
func watchUntil(until string) team.WatchCall {
	return team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#7", Until: until}
}

// watchOf is the watch call made.
func (h *teamHarness) watchOf(call TeamCall) store.WatchRow {
	h.t.Helper()
	w, err := h.store.WatchByKey(context.Background(), call.key())
	if err != nil {
		h.t.Fatalf("the watch of %+v: %v", call, err)
	}
	return w
}

// watchNotesTo are the watches' messages session got, in order.
func (h *teamHarness) watchNotesTo(session int64) []string {
	var out []string
	for _, text := range h.sentTo(session) {
		if strings.HasPrefix(text, "[W") {
			out = append(out, text)
		}
	}
	return out
}

// pullReads counts gh's reads of frostyard/example#7.
func (g *fakeGitHub) pullReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, call := range g.calls {
		if strings.HasSuffix(call, "repos/frostyard/example/pulls/7") {
			n++
		}
	}
	return n
}

// team_watch is every session's that keeps lessons, but a shift's
// specialist: a supervisor's, a session the owner talks in of a team
// profile, and a thread's specialist watch; a shift's specialist, a plain
// session and a private agent's are refused, before anything is read or
// kept (ADR-0072).
func TestTeamWatchGoesByRole(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	thread := homeThread(t, h, "d1")
	for name, session := range map[string]int64{"the supervisor": h.super.ID, "an owner's session": agent.ID, "a thread's specialist": thread.Session} {
		if text, err := h.server.TeamWatch(ctx, TeamCall{Session: session, Task: 1, CallID: "w1"}, watchUntil(team.UntilMerged)); err != nil || !strings.HasPrefix(text, "Watching W") {
			t.Errorf("%s: %q %v", name, text, err)
		}
	}
	shift := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(3)})
	h.addProfiles(map[string]string{"chani.md": chani})
	private, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		session int64
		says    string
	}{
		"a shift's specialist": {shift.Session, "not a shift's"},
		"a private agent":      {private.ID, "not this session's"},
		"a plain session":      {plain.ID, "started from a profile"},
	} {
		call := TeamCall{Session: tc.session, Task: 1, CallID: "w1"}
		if text, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %q %v", name, text, err)
		}
		if _, err := h.store.WatchByKey(ctx, call.key()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: a refused add kept a watch: %v", name, err)
		}
	}
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w9"}, team.WatchCall{Action: "wait"}); err == nil {
		t.Error("an unknown action passed")
	}
}

// An add reads the pull request on GitHub once: one pocketd cannot read
// is refused and keeps nothing; one at its condition already, or past
// reaching it, is answered at once with no watch to check and no note;
// otherwise the watch is kept. The same call run again answers as it did
// and reads nothing; list and cancel act on the session's own (ADR-0072).
func TestAWatchAddReadsThePullRequestOnce(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	call := func(id string) TeamCall { return TeamCall{Session: h.super.ID, Task: 1, CallID: id} }
	text, err := h.server.TeamWatch(ctx, call("w1"), watchUntil(team.UntilChecks))
	if err != nil || !strings.HasPrefix(text, "Watching W1: frostyard/example#7 until its checks are done, for at most 24 hours, until 2026-11-19 22:00 UTC.") {
		t.Fatalf("the add %q %v", text, err)
	}
	if w := h.watchOf(call("w1")); w.State != store.WatchLive || w.GitHubID != 1007 || w.SessionID != h.super.ID || !w.Deadline.Equal(h.clock.Now().Add(24*time.Hour)) {
		t.Fatalf("the watch %+v", w)
	}
	reads := g.pullReads()
	if again, err := h.server.TeamWatch(ctx, call("w1"), team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#9", Until: team.UntilMerged}); err != nil || again != text || g.pullReads() != reads {
		t.Fatalf("the call again %q %v, %d reads", again, err, g.pullReads()-reads)
	}
	if text, err := h.server.TeamWatch(ctx, call("w2"), team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#8", Until: team.UntilMerged}); err == nil || !strings.Contains(err.Error(), "cannot read frostyard/example#8") {
		t.Fatalf("an unreadable pull request: %q %v", text, err)
	}
	if _, err := h.store.WatchByKey(ctx, call("w2").key()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused add kept %v", err)
	}
	for name, tc := range map[string]struct {
		state  string
		merged bool
		ci     string
		until  string
		says   string
		ended  string
	}{
		"merged already":      {"closed", true, pendingRuns, team.UntilMerged, "frostyard/example#7 is there already: merged.", team.WatchMet},
		"never to merge":      {"closed", false, pendingRuns, team.UntilMerged, "frostyard/example#7 was closed without merging, so it will not merge", team.WatchUnreachable},
		"closed already":      {"closed", false, pendingRuns, team.UntilClosed, "is there already: closed without merging.", team.WatchMet},
		"its checks done":     {"open", false, greenRuns, team.UntilChecks, "is there already: CI at aaaaaaaaaaaa green.", team.WatchMet},
		"no checks to finish": {"closed", true, pendingRuns, team.UntilChecks, "is merged, so there are no checks to wait for", team.WatchUnreachable},
	} {
		g.set(func(g *fakeGitHub) { g.state, g.merged, g.ci[headA] = tc.state, tc.merged, tc.ci })
		c := call("at-once " + name)
		text, err := h.server.TeamWatch(ctx, c, watchUntil(tc.until))
		if err != nil || !strings.Contains(text, tc.says) || !strings.Contains(text, "No watch was kept, and no message follows.") {
			t.Errorf("%s: %q %v", name, text, err)
			continue
		}
		w := h.watchOf(c)
		if w.State != store.WatchEnded || w.Outcome != tc.ended {
			t.Errorf("%s: %+v", name, w)
		}
		if _, err := h.store.Note(ctx, w.NoteKey()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: an answer at once filed a note: %v", name, err)
		}
	}
	list, err := h.server.TeamWatch(ctx, call("l1"), team.WatchCall{Action: team.WatchList})
	if err != nil || !strings.Contains(list, "- W1 frostyard/example#7 until its checks are done: watching, until 2026-11-19 22:00 UTC") ||
		!strings.Contains(list, "the condition held") {
		t.Fatalf("the list %q %v", list, err)
	}
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 2, CallID: "c0"}, team.WatchCall{Action: team.WatchCancel, ID: "W1"}); err != nil {
		t.Fatal(err)
	}
	if text, err := h.server.TeamWatch(ctx, call("c1"), team.WatchCall{Action: team.WatchCancel, ID: "W1"}); err != nil || text != "W1 had ended already: cancelled." {
		t.Fatalf("a cancel again %q %v", text, err)
	}
	leto, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: leto.ID, Task: 1, CallID: "c1"}, team.WatchCall{Action: team.WatchCancel, ID: "W1"}); err == nil || !strings.Contains(err.Error(), "no watch W1") {
		t.Fatalf("another session's cancel: %v", err)
	}
}

// A pass ends a watch whose condition holds and files its one note; a
// pass again, and a restarted pocketd's, files no other, and the delivery
// pass places it once (ADR-0072).
func TestAWatchsNoteIsFiledAndPlacedOnce(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilChecks)); err != nil {
		t.Fatal(err)
	}
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if w := h.watchOf(call); w.State != store.WatchLive {
		t.Fatalf("pending CI ended it: %+v", w)
	}
	g.set(func(g *fakeGitHub) { g.ci[headA] = failingRuns })
	h.clock.Advance(90 * time.Minute)
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	w := h.watchOf(call)
	if w.State != store.WatchEnded || w.Outcome != team.WatchMet || w.Seen != "open, CI at aaaaaaaaaaaa failing: verify" {
		t.Fatalf("the watch %+v", w)
	}
	note, err := h.store.Note(ctx, w.NoteKey())
	want := "[W1 · frostyard/example#7 · checks done] frostyard/example#7 (Fix CI): CI at aaaaaaaaaaaa failing: verify. The watch ended; add another with team_watch to go on waiting."
	if err != nil || note.Text != want || note.SessionID != h.super.ID {
		t.Fatalf("the note %+v %v", note, err)
	}
	for i := 0; i < 2; i++ {
		if err := h.server.watchPass(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	again := h.restarted(t)
	if err := again.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := again.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.watchNotesTo(h.super.ID); len(got) != 1 || got[0] != want {
		t.Fatalf("the supervisor got %q", got)
	}
	if note, _ := h.store.Note(ctx, w.NoteKey()); note.SentAt == nil {
		t.Fatal("the note is not noted sent")
	}
}

// A pass reads each watched pull request once, whichever watches name it,
// and CI at its head once; a read that fails leaves every watch of it for
// the next pass (ADR-0072).
func TestAPassReadsEachWatchedPullRequestOnce(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	for i, until := range []string{team.UntilChecks, team.UntilMerged, team.UntilClosed} {
		for _, session := range []int64{h.super.ID, agent.ID} {
			if _, err := h.server.TeamWatch(ctx, TeamCall{Session: session, Task: 1, CallID: "w" + itoa(int64(i))}, watchUntil(until)); err != nil {
				t.Fatal(err)
			}
		}
	}
	reads := g.pullReads()
	countCI := func() int {
		g.mu.Lock()
		defer g.mu.Unlock()
		n := 0
		for _, call := range g.calls {
			if strings.Contains(call, "/check-runs") {
				n++
			}
		}
		return n
	}
	ci := countCI()
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := g.pullReads() - reads; n != 1 {
		t.Fatalf("the pass read the pull request %d times", n)
	}
	if n := countCI() - ci; n != 1 {
		t.Fatalf("the pass read CI %d times", n)
	}
	g.set(func(g *fakeGitHub) { g.failReads, g.state, g.merged = 1, "closed", true })
	if err := h.server.watchPass(ctx); err == nil {
		t.Fatal("a failed read was not reported")
	}
	live, err := h.store.LiveWatches(ctx)
	if err != nil || len(live) != 6 {
		t.Fatalf("a failed read ended watches: %d %v", len(live), err)
	}
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if live, _ := h.store.LiveWatches(ctx); len(live) != 0 {
		t.Fatalf("live after the merge %+v", live)
	}
}

// A watch ends when its pull request can no longer reach its condition,
// and when its time is up, with what pocketd saw last or why it could not
// read it; until then a failed read leaves it (ADR-0072).
func TestAWatchEndsWhenItCanNoLongerHoldOrItsTimeIsUp(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	call := func(id string) TeamCall { return TeamCall{Session: h.super.ID, Task: 1, CallID: id} }
	for id, until := range map[string]string{"merged": team.UntilMerged, "closed": team.UntilClosed, "checks": team.UntilChecks} {
		if _, err := h.server.TeamWatch(ctx, call(id), watchUntil(until)); err != nil {
			t.Fatal(err)
		}
	}
	g.set(func(g *fakeGitHub) { g.state = "closed" })
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	for id, tc := range map[string]struct{ outcome, says string }{
		"merged": {team.WatchUnreachable, "· closed without merging] frostyard/example#7 (Fix CI): It was closed without merging, so it will not merge unless someone reopens it."},
		"closed": {team.WatchMet, "· closed] frostyard/example#7 (Fix CI): It was closed without merging."},
		"checks": {team.WatchUnreachable, "It was closed without merging, so there are no checks to wait for."},
	} {
		w := h.watchOf(call(id))
		note, err := h.store.Note(ctx, w.NoteKey())
		if w.Outcome != tc.outcome || err != nil || !strings.Contains(note.Text, tc.says) {
			t.Errorf("%s: %+v, %q %v", id, w, note.Text, err)
		}
	}

	// Time: a watch of an hour, read in a pass before its end, then not.
	g.set(func(g *fakeGitHub) { g.state = "open" })
	hour := team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#7", Until: team.UntilChecks, Hours: hours(1)}
	if text, err := h.server.TeamWatch(ctx, call("hour"), hour); err != nil || !strings.Contains(text, "for at most 1 hour,") {
		t.Fatalf("an hour's watch %q %v", text, err)
	}
	merge := team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#7", Until: team.UntilMerged, Hours: hours(2)}
	if _, err := h.server.TeamWatch(ctx, call("two"), merge); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(30 * time.Minute)
	g.set(func(g *fakeGitHub) { g.failReads = 1 })
	if err := h.server.watchPass(ctx); err == nil {
		t.Fatal("a failed read was not reported")
	}
	if w := h.watchOf(call("hour")); w.State != store.WatchLive {
		t.Fatalf("a failed read before the end ended it: %+v", w)
	}
	h.clock.Advance(31 * time.Minute)
	g.set(func(g *fakeGitHub) { g.failReads = 1 })
	_ = h.server.watchPass(ctx)
	w := h.watchOf(call("hour"))
	note, err := h.store.Note(ctx, w.NoteKey())
	if w.Outcome != team.WatchExpired || err != nil || !strings.Contains(note.Text, "[W4 · frostyard/example#7 · time is up]") ||
		!strings.Contains(note.Text, "pocketd watched it for 61m for its checks to be done, and could not read it at the end: read frostyard/example#7 on GitHub: gh: HTTP 502. The watch ended") {
		t.Fatalf("an hour's end %+v, %q %v", w, note.Text, err)
	}
	if w := h.watchOf(call("two")); w.State != store.WatchLive {
		t.Fatalf("a watch of two hours ended after one: %+v", w)
	}
	h.clock.Advance(time.Hour)
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	w = h.watchOf(call("two"))
	if note, _ := h.store.Note(ctx, w.NoteKey()); w.Outcome != team.WatchExpired || !strings.Contains(note.Text, "for it to merge, and it is still open.") {
		t.Fatalf("two hours' end %+v, %q", w, note.Text)
	}
}

// A pass a stop cut short ends no watch, also one past its deadline: a
// read the stop cancelled is no answer (ADR-0072).
func TestAStoppedPassEndsNoWatch(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(context.Background(), call, watchUntil(team.UntilChecks)); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(25 * time.Hour)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.server.watchPass(stopped); !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped pass: %v", err)
	}
	if w := h.watchOf(call); w.State != store.WatchLive {
		t.Fatalf("a stopped pass ended %+v", w)
	}
	if err := h.server.watchPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := h.watchOf(call); w.Outcome != team.WatchExpired {
		t.Fatalf("the next pass left %+v", w)
	}
}

// A watch's note starts a run: it goes only to an idle session, never
// into its queue, and waits while the session is at work (ADR-0072).
func TestAWatchsNoteWaitsForAnIdleSession(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{h.super.ID: true}
	h.engine.mu.Unlock()
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.watchNotesTo(h.super.ID); len(got) != 0 {
		t.Fatalf("a busy session got %q", got)
	}
	h.engine.mu.Lock()
	h.engine.busy = nil
	h.engine.mu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.watchNotesTo(h.super.ID); len(got) != 1 || !strings.Contains(got[0], "[W1 · frostyard/example#7 · merged]") {
		t.Fatalf("the idle session got %q", got)
	}
}

// A thread's close ends its specialist's watches with no note, and drops
// one a watch filed that has not gone: the closed thread's specialist
// gets none, and a pass after files none (ADR-0072).
func TestAThreadsCloseEndsItsWatchesWithNoNote(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	thread := homeThread(t, h, "d1")
	live := TeamCall{Session: thread.Session, Task: 1, CallID: "w1"}
	met := TeamCall{Session: thread.Session, Task: 1, CallID: "w2"}
	if _, err := h.server.TeamWatch(ctx, live, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWatch(ctx, met, watchUntil(team.UntilChecks)); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{thread.Session: true}
	h.engine.mu.Unlock()
	g.set(func(g *fakeGitHub) { g.ci[headA] = greenRuns })
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if w := h.watchOf(met); w.Outcome != team.WatchMet {
		t.Fatalf("the checks' watch %+v", w)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, Task: 2, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if w := h.watchOf(live); w.State != store.WatchEnded || w.Outcome != team.WatchThreadClosed {
		t.Fatalf("the merge's watch %+v", w)
	}
	if note, _ := h.store.Note(ctx, h.watchOf(met).NoteKey()); note.DroppedAt == nil {
		t.Fatalf("the checks' note %+v", note)
	}
	h.engine.mu.Lock()
	h.engine.busy = nil
	h.engine.mu.Unlock()
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.watchNotesTo(thread.Session); len(got) != 0 {
		t.Fatalf("the closed thread's specialist got %q", got)
	}
	// Right before a submission, a note to a closed thread's specialist
	// is refused, whatever the store held when it was read.
	if err := h.server.watchNoteGoes(ctx, thread.Session); !errors.Is(err, errWatchClosed) {
		t.Fatalf("a closed thread's specialist: %v", err)
	}
	if err := h.server.watchNoteGoes(ctx, h.super.ID); err != nil {
		t.Fatalf("the supervisor: %v", err)
	}
}

// With no pull rounds nothing would check a watch: StartPulls(0) ends each
// live one with its note, and an add after it is refused (ADR-0072).
func TestNoPullRoundsKeepNoWatch(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	h.server.StartPulls(0)
	w := h.watchOf(call)
	if note, err := h.store.Note(ctx, w.NoteKey()); w.Outcome != team.WatchUnchecked || err != nil || !strings.Contains(note.Text, "(-pr-every 0)") {
		t.Fatalf("the watch %+v, its note %q %v", w, note.Text, err)
	}
	if text, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w2"}, watchUntil(team.UntilMerged)); err == nil || err.Error() != team.WatchesOff {
		t.Fatalf("an add with no rounds: %q %v", text, err)
	}
}

// Each pull round checks the watches, after its reads (ADR-0072).
func TestAPullRoundChecksTheWatches(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	var rounds []func()
	h.server.pullTimer = func(_ time.Duration, f func()) func() {
		rounds = append(rounds, f)
		return func() {}
	}
	h.server.StartPulls(5 * time.Minute)
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	text, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged))
	if err != nil || !strings.Contains(text, "pocketd checks it every 5m") {
		t.Fatalf("the add %q %v", text, err)
	}
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	rounds[0]()
	if w := h.watchOf(call); w.Outcome != team.WatchMet {
		t.Fatalf("the round left %+v", w)
	}
}

// A building thread's brief says how to wait for CI with team_watch, and
// its specialist's session watches; a shift's says how its rounds go, and
// its session gets no team_watch (ADR-0072).
func TestABriefSaysHowToWaitForCI(t *testing.T) {
	h := newTeamHarness(t)
	thread := homeThread(t, h, "d1")
	shift := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(3)})
	if brief := h.sentTo(thread.Session); len(brief) == 0 || !strings.Contains(brief[0], team.WatchGuide) {
		t.Fatalf("a thread's brief %q", brief)
	}
	if brief := h.sentTo(shift.Session); len(brief) == 0 || strings.Contains(brief[0], team.WatchGuide) || !strings.Contains(brief[0], "This message is round 1") {
		t.Fatalf("a shift's brief %q", brief)
	}
	shifts := map[int64]bool{}
	for _, spec := range h.engine.specs {
		if spec.Thread != 0 {
			shifts[spec.Thread] = spec.Shift
		}
	}
	if shifts[thread.ID] || !shifts[shift.ID] {
		t.Fatalf("the specs' shifts %v", shifts)
	}
}

// At start, every session that watches now and was made before team_watch
// gets it, but a standing one; a shift's specialist and a private agent's
// session get none (ADR-0072).
func TestOlderSessionsGetTeamWatchAtStart(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	thread := homeThread(t, h, "d1")
	shift := shiftThread(t, h, "s1", team.Shift{Stop: "the tests pass", MaxRounds: rounds(3)})
	h.addProfiles(map[string]string{"chani.md": chani})
	private, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	given := h.engine.watchTool
	h.engine.mu.Unlock()
	for name, tc := range map[string]struct {
		session int64
		gets    bool
	}{
		"the supervisor":        {h.super.ID, true},
		"an owner's session":    {agent.ID, true},
		"a thread's specialist": {thread.Session, true},
		"a shift's specialist":  {shift.Session, false},
		"a private agent":       {private.ID, false},
	} {
		if given[tc.session] != tc.gets {
			t.Errorf("%s: %v", name, given[tc.session])
		}
	}
}
