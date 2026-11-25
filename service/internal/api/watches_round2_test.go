package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// An add stopped while it waited for its admission keeps no watch: its
// last check comes after the lock, and its transaction takes the call's
// context (Codex review of #173, round 2).
func TestAnAddStoppedWhileItWaitsForAdmissionKeepsNoWatch(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	call, stop := context.WithCancel(context.Background())
	defer stop()
	// The call is stopped after its reads, as it waits behind another add.
	h.server.afterWatchRead = func() {
		h.server.afterWatchRead = nil
		stop()
	}
	tc := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if text, err := h.server.TeamWatch(call, tc, watchUntil(team.UntilMerged)); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("the stopped add %q %v", text, err)
	}
	if _, err := h.store.WatchByKey(context.Background(), tc.key()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stopped add kept a watch: %v", err)
	}
}

// With no pull rounds, a sweep of the live watches that failed runs again
// until it ends them with their notes, and no add is admitted meanwhile
// (Codex review of #173, round 2).
func TestAFailedSweepOfUncheckedWatchesRunsAgain(t *testing.T) {
	h := newTeamHarness(t)
	watchGitHub(h)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_watch_end BEFORE UPDATE ON watches BEGIN SELECT RAISE(ABORT, 'the disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(100)
	h.server.StartPulls(0)
	if w := h.watchOf(call); w.State != store.WatchLive {
		t.Fatalf("a failed sweep ended %+v", w)
	}
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w2"}, watchUntil(team.UntilMerged)); err == nil || err.Error() != team.WatchesOff {
		t.Fatalf("an add while the sweep waits: %v", err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_watch_end`); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(100)
	w := h.watchOf(call)
	if note, err := h.store.Note(ctx, w.NoteKey()); w.Outcome != team.WatchUnchecked || err != nil || !strings.Contains(note.Text, "-pr-every 0") {
		t.Fatalf("after the sweep ran again %+v, its note %q %v", w, note.Text, err)
	}
}

// A watch's note wakes the delivery pass as it is filed, before the pass
// reads the next watched pull request (Codex review of #173, round 2).
func TestAWatchsNoteWakesDeliveryBeforeTheNextRead(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	other := githubPull("frostyard/example", 8, "vera/other", "frostyard/example", "open", false)
	scheduledAtRead := -1
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if strings.HasSuffix(strings.Join(args, " "), "repos/frostyard/example/pulls/8") {
			if scheduledAtRead == -2 {
				h.retryMu.Lock()
				scheduledAtRead = len(h.scheduled)
				h.retryMu.Unlock()
			}
			return other, nil
		}
		return g.gh(ctx, args)
	}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w7"}, watchUntil(team.UntilMerged)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWatch(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w8"},
		team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#8", Until: team.UntilMerged}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(100)
	g.set(func(g *fakeGitHub) { g.state, g.merged = "closed", true })
	scheduledAtRead = -2
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	if scheduledAtRead < 1 {
		t.Fatalf("the delivery pass was asked for after the next read: %d scheduled when #8 was read", scheduledAtRead)
	}
}

// The watches' passes spend no more GitHub requests in any hour than the
// watches' budget, however many pages CI has, and each watch is read in
// turn: 64 watches of heads with ten pages each of check runs and
// statuses, every 5 minutes for 3 hours (Codex review of #173, round 2).
func TestWatchReadsStayWithinTheirBudget(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	for n := int64(1); n <= team.MaxWatches; n++ {
		if _, _, err := h.store.AddWatch(ctx, store.WatchRow{Key: fmt.Sprint("w", n), SessionID: h.super.ID, Repository: "frostyard/example", Number: n,
			GitHubID: 1000 + n, Until: team.UntilChecks, Hours: team.MaxWatchHours, Deadline: h.clock.Now().Add(team.MaxWatchHours * time.Hour)},
			team.MaxWatches, team.MaxWatches, func(store.WatchRow) string { return "" }); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	read := map[int64]bool{}
	h.engine.mu.Lock()
	h.engine.onGH = func(_ context.Context, args []string) (string, error) {
		calls++
		call := strings.Join(args, " ")
		page := 1
		if i := strings.Index(call, "&page="); i >= 0 {
			_, _ = fmt.Sscanf(call[i+len("&page="):], "%d", &page)
		}
		switch {
		case strings.Contains(call, "/check-runs"):
			runs := make([]string, 100)
			for i := range runs {
				runs[i] = fmt.Sprintf(`{"id":%d,"name":"job %d","status":"in_progress","conclusion":null}`, page*1000+i, page*1000+i)
			}
			return `{"total_count":1000,"check_runs":[` + strings.Join(runs, ",") + `]}`, nil
		case strings.Contains(call, "/status"):
			statuses := make([]string, 100)
			for i := range statuses {
				statuses[i] = fmt.Sprintf(`{"context":"status %d","state":"pending"}`, page*1000+i)
			}
			return `{"total_count":1000,"statuses":[` + strings.Join(statuses, ",") + `]}`, nil
		}
		var n int64
		if _, err := fmt.Sscanf(call[strings.LastIndex(call, "/")+1:], "%d", &n); err != nil {
			return "", errors.New("gh: HTTP 404")
		}
		read[n] = true
		return strings.Replace(githubPull("frostyard/example", n, "vera/fix", "frostyard/example", "open", false), strings.Repeat("a", 40), fmt.Sprintf("%040x", n), 1), nil
	}
	h.engine.mu.Unlock()
	var perPass []int
	for pass := 0; pass < 36; pass++ {
		before := calls
		_ = h.server.watchPass(ctx)
		perPass = append(perPass, calls-before)
		h.clock.Advance(5 * time.Minute)
	}
	for start := 0; start+12 <= len(perPass); start++ {
		hour := 0
		for _, n := range perPass[start : start+12] {
			hour += n
		}
		if hour > team.WatchReadBudget {
			t.Fatalf("the hour from pass %d spent %d requests, over %d: %v", start, hour, team.WatchReadBudget, perPass)
		}
	}
	if len(read) != team.MaxWatches {
		t.Fatalf("%d of %d watches were read in 3 hours", len(read), team.MaxWatches)
	}
	if live, _ := h.store.LiveWatches(ctx); len(live) != team.MaxWatches {
		t.Fatalf("a budget's wait ended watches: %d live", len(live))
	}
}

// CI pocketd could not read says why in the watch's note at its end, not
// that its checks still run (Codex review of #173, round 2).
func TestAnExpiredWatchSaysWhyItsCIWasUnread(t *testing.T) {
	h := newTeamHarness(t)
	g := watchGitHub(h)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.onGH = func(ctx context.Context, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "/check-runs") {
			return "", errors.New("gh: Resource not accessible by integration (HTTP 403)")
		}
		return g.gh(ctx, args)
	}
	h.engine.mu.Unlock()
	call := TeamCall{Session: h.super.ID, Task: 1, CallID: "w1"}
	if _, err := h.server.TeamWatch(ctx, call, team.WatchCall{Action: team.WatchAdd, Pull: "frostyard/example#7", Until: team.UntilChecks, Hours: hours(1)}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(61 * time.Minute)
	if err := h.server.watchPass(ctx); err != nil {
		t.Fatal(err)
	}
	w := h.watchOf(call)
	note, err := h.store.Note(ctx, w.NoteKey())
	if w.Outcome != team.WatchExpired || err != nil || !strings.Contains(note.Text, "it is still open, CI unread: read the checks of frostyard/example at aaaaaaaaaaaa on GitHub: gh: Resource not accessible by integration (HTTP 403)") {
		t.Fatalf("the expiry %+v, %q %v", w, note.Text, err)
	}
}
