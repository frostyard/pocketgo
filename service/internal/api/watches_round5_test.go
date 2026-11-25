package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// When the budget holds back only a watch's CI, the pass's read of its
// pull request still answers it: an expiry keeps the title and the open
// state, and says the CI was unread for the budget (Codex review of #173,
// round 5).
func TestAHeldCIReadKeepsThePullRequestsRead(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	merge := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	checks := TeamCall{Session: h.super.ID, Task: 1, CallID: "w2"}
	if _, err := h.server.TeamWatch(ctx, merge, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWatch(ctx, checks, team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#7", Until: team.UntilChecks, Hours: hours(1)}); err != nil {
		t.Fatal(err)
	}
	// Past the checks watch's hour, with 2 requests left: #7's read, and
	// not CI's 20 pages.
	h.clock.Advance(61 * time.Minute)
	for h.server.watchBudget.Take(h.clock.Now()) {
	}
	h.clock.Advance(6 * time.Second)
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if w := h.watchOf(merge); w.Outcome != "" {
		t.Fatalf("the merge's watch %+v", w)
	}
	w := h.watchOf(checks)
	note, err := h.store.Note(ctx, w.NoteKey())
	if w.Outcome != team.WatchExpired || err != nil || !strings.Contains(note.Text, "frostyard/example#7 (Fix CI): ") ||
		!strings.Contains(note.Text, "it is still open, CI unread: "+team.WatchReadsSpent) || strings.Contains(note.Text, "could not read it") {
		t.Fatalf("the checks' expiry %+v, %q %v", w, note.Text, err)
	}
}

// With no pull rounds, the sweep wakes the delivery pass as each watch's
// note is filed, not after the others' ends (Codex review of #173, round
// 5).
func TestTheUncheckedSweepWakesDeliveryAtEachNote(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	for _, id := range []string{"w1", "w2"} {
		if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: id}, watchUntil(team.UntilMerged)); err != nil {
			t.Fatal(err)
		}
	}
	h.runScheduled(100)
	var scheduledAtEnd []int
	h.server.beforeUncheckedEnd = func(int64) {
		h.retryMu.Lock()
		scheduledAtEnd = append(scheduledAtEnd, len(h.scheduled))
		h.retryMu.Unlock()
	}
	h.server.StartPulls(0)
	h.runScheduled(1) // the sweep
	if len(scheduledAtEnd) != 2 || scheduledAtEnd[1] < 1 {
		t.Fatalf("the delivery pass was asked for after the second end: scheduled at each end %v", scheduledAtEnd)
	}
}
