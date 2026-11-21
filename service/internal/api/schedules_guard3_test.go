package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// The guards of Codex's third review of #51 (ADR-0030).

// team_schedule's list comes in pages a tool result keeps whole, and its
// last schedule is reached by the pages that follow.
func TestTheToolsListComesInPages(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	ctx := context.Background()
	text := strings.Repeat("y", schedule.MaxText)
	var last string
	for i := range 14 {
		got, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "a" + strconv.Itoa(i)}, schedule.Call{Action: schedule.ActionAdd, When: "in 1h", Message: text})
		if err != nil {
			t.Fatal(err)
		}
		last = strings.Fields(got)[1]
	}
	last = strings.TrimSuffix(last, ":")
	found := false
	for page := 1; page <= 14; page++ {
		got, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 2, CallID: "l" + strconv.Itoa(page)}, schedule.Call{Action: schedule.ActionList, Page: page})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) > schedule.MaxPageBytes+512 {
			t.Fatalf("page %d holds %d bytes", page, len(got))
		}
		found = found || strings.Contains(got, "\n"+last+": ")
		if !strings.Contains(got, "team_schedule list with page") {
			break
		}
	}
	if !found {
		t.Fatalf("no page names %s", last)
	}
}

// A schedules event carries no list, whatever the list holds: the client
// fetches it, and the events pocketd keeps for clients that come back stay
// small.
func TestASchedulesEventCarriesNoList(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	ctx := context.Background()
	text := strings.Repeat("x", schedule.MaxText)
	for i := range 100 {
		if _, _, err := h.store.AddSchedule(ctx, store.ScheduleRow{Key: "k" + strconv.Itoa(i), SessionID: int64(1000 + i/schedule.MaxPerSession), Spec: "every day 23:00",
			Zone: "UTC", Text: text, FirstAt: time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)}, schedule.MaxPerSession); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.client.Schedules(ctx); err != nil {
		t.Fatal(err)
	}
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	n := 0
	for _, e := range h.events {
		if e.Type != proto.EventSchedules {
			continue
		}
		n++
		encoded, err := json.Marshal(e)
		if err != nil || len(encoded) > 256 {
			t.Fatalf("a schedules event of %d bytes: %v", len(encoded), err)
		}
	}
	if n == 0 {
		t.Fatal("no schedules event")
	}
}

// A profile the owner's schedule names as a symbolic link is refused for
// good, as a profile read only as a regular file is: its time is missed,
// never held for a later pass.
func TestALinkedProfileMissesTheTime(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	path := filepath.Join(h.dir, "odrade.md")
	copied := filepath.Join(t.TempDir(), "odrade.md")
	if err := os.WriteFile(copied, []byte(odrade), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(copied, path); err != nil {
		t.Fatal(err)
	}
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	row := h.scheduleRow("evening")
	if row.LastOutcome != store.OutcomeMissed || !strings.Contains(row.LastWhy, "no valid profile odrade") || !row.NextAt.Equal(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("a linked profile %+v", row)
	}
}

// The upgrade of the supervisors' tools is tried again by the schedules
// passes, also when the start's upgrade of the threads' tools failed
// before it.
func TestAFailedStartUpgradeLeavesTheScheduleToolsDue(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(context.Background()); err == nil {
		t.Fatal("an upgrade with no work database succeeded")
	}
	h.server.scheduleMu.Lock()
	defer h.server.scheduleMu.Unlock()
	if !h.server.toolsDue {
		t.Fatal("no pass tries the schedule tools' upgrade again")
	}
}
