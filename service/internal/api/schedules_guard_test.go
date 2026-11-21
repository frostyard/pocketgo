package api

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// The guards of Codex's reviews of #51 (ADR-0030).

// setClock moves the harness's clock to t.
func (h *harness) setClock(t time.Time) {
	h.clock.mu.Lock()
	h.clock.now = t
	h.clock.mu.Unlock()
}

// An owner's scheduled message to a standing session waits while the
// session's profile cannot be applied, as the owner's own message is
// refused then, and goes once it can (round 1).
func TestAScheduledMessageWaitsForItsStandingProfile(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	row := h.scheduleRow("evening")
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{h.odrade: true}
	h.engine.mu.Unlock()
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	// Odrade's profile breaks before the message reaches her.
	path := filepath.Join(h.dir, "odrade.md")
	if err := os.WriteFile(path, []byte("---\nrole: supervisor\nmodel: nope\n---\n# Odrade\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.failTo = nil
	h.engine.mu.Unlock()
	h.server.deliverLater()
	h.runScheduled(20)
	if got := h.sentTo(h.odrade, "schedule:"+row.Owner); len(got) != 0 {
		t.Fatalf("sent while the profile cannot be applied: %q", got)
	}
	if err := os.WriteFile(path, []byte(odrade), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.deliverLater()
	h.runScheduled(20)
	if got := h.sentTo(h.odrade, "schedule:"+row.Owner); len(got) != 1 {
		t.Fatalf("sent once the profile applies: %q", got)
	}
}

// A repeat whose last message is in the transcript, though pocketd stopped
// before it noted so, goes out at its next time, never skipped, whichever
// pass runs first after the start (round 1).
func TestASkipFindsTheLastMessageInTheTranscript(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	row := h.scheduleRow("evening")
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{h.odrade: true}
	h.engine.mu.Unlock()
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	// PiG placed it, and pocketd stopped before it noted it sent.
	h.engine.mu.Lock()
	h.engine.failTo = nil
	h.engine.mu.Unlock()
	if _, err := h.engine.Send(context.Background(), h.odrade, row.NoteKey(0), schedule.Message("Run the evening gather.")); err != nil {
		t.Fatal(err)
	}
	// The schedules pass runs before any delivery pass.
	h.setClock(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC))
	h.fireScheduleWakes()
	if got := h.scheduleRow("evening"); got.Runs != 2 || got.Skipped != 0 || got.LastOutcome != store.OutcomeSent {
		t.Fatalf("the next time %+v", got)
	}
	if note, err := h.store.Note(context.Background(), row.NoteKey(0)); err != nil || note.SentAt == nil {
		t.Fatalf("the first note %+v %v", note, err)
	}
}

// A service zone whose name no zone database has, as one TZ names by its
// file, still sends a schedule's times, every one (round 1).
func TestAServiceZoneByItsFileKeepsItsSchedules(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	h.server.SetZone(time.FixedZone("/usr/share/zoneinfo/Europe/Berlin", 3600))
	h.server.StartSchedules()
	h.runSchedulePasses()
	if _, err := h.server.TeamSchedule(context.Background(), TeamCall{Session: h.odrade, Task: 1, CallID: "s1"}, schedule.Call{Action: schedule.ActionAdd, When: "every day 23:00", Message: "Look."}); err != nil {
		t.Fatal(err)
	}
	h.runSchedulePasses()
	h.at(time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC))
	row := h.scheduleRow("S1")
	if row.Runs != 1 || row.State != store.ScheduleActive || row.NextAt == nil || !row.NextAt.Equal(time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("after its time %+v", row)
	}
}

// leto is a supervisor that keeps no standing session.
const leto = "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Leto\n"

// A supervisor's session started before team_schedule gets it at start
// (round 1); a session of any other profile does not, and a standing one
// takes it at its own refresh, never from the upgrade, which could cross
// that refresh (round 2).
func TestOlderSupervisorSessionsGetTheScheduleTool(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := os.WriteFile(filepath.Join(h.dir, "leto.md"), []byte(leto), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "vera", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	older, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.scheduleTool[older.ID] || h.engine.scheduleTool[plain.ID] || h.engine.scheduleTool[h.odrade] {
		t.Fatalf("team_schedule went to %v", h.engine.scheduleTool)
	}
}

// unreadableProfiles is a profile folder whose reads of some profiles fail for
// a while, as a read the disk refuses.
type unreadableProfiles struct {
	profiles.Folder
	mu   sync.Mutex
	fail map[string]bool
}

func (f *unreadableProfiles) Find(id string) (profiles.Profile, error) {
	f.mu.Lock()
	failing := f.fail[id]
	f.mu.Unlock()
	if failing {
		return profiles.Profile{}, &fs.PathError{Op: "read", Path: id + ".md", Err: syscall.EIO}
	}
	return f.Folder.Find(id)
}

// A supervisor's profile that cannot be read at start gets its sessions
// team_schedule once it can, by a later pass (round 2).
func TestAScheduleToolUpgradeIsTriedAgain(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	if err := os.WriteFile(filepath.Join(h.dir, "leto.md"), []byte(leto), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	older, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, fail: map[string]bool{"leto": true}}
	h.server.SetProfiles(flaky)
	_ = h.server.UpgradeTeamTools(ctx)
	h.engine.mu.Lock()
	upgraded := h.engine.scheduleTool[older.ID]
	h.engine.mu.Unlock()
	if upgraded {
		t.Fatal("upgraded with no profile read")
	}
	flaky.mu.Lock()
	flaky.fail = nil
	flaky.mu.Unlock()
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.scheduleTool[older.ID] {
		t.Fatalf("not upgraded once the profile reads: %v", h.engine.scheduleTool)
	}
}

// A change of only a when's case or spaces changes nothing: a time that
// went out once does not go out again (round 2).
func TestACaseOnlyEditKeepsTheSchedule(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [{"id": "once", "when": "in 30m", "profile": "odrade", "message": "x"}]}`, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.at(time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC))
	h.writeSchedules(`{"schedules": [{"id": "once", "when": "IN  30M", "profile": "odrade", "message": "x"}]}`)
	h.at(time.Date(2026, 11, 18, 22, 31, 0, 0, time.UTC))
	h.at(time.Date(2026, 11, 18, 23, 1, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 {
		t.Fatalf("the time went out again: %q", got)
	}
}

// team_schedule's list reads the owner's file as it is now, with what
// pocketd records for it, never a new file beside an old record (round 1).
func TestTheToolListsTheFileAsItIsNow(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.writeSchedules(`{"schedules": [{"id": "evening", "when": "tomorrow 9:00", "profile": "odrade", "message": "Once."}]}`)
	list, err := h.server.TeamSchedule(context.Background(), TeamCall{Session: h.odrade, Task: 1, CallID: "l1"}, schedule.Call{Action: schedule.ActionList})
	if err != nil {
		t.Fatal(err)
	}
	if want := "evening (the owner's): once, next Thu 19 Nov 09:00 UTC · Once."; !strings.Contains(list, want) {
		t.Fatalf("the list lacks %q:\n%s", want, list)
	}
}

// An entry that breaks a rule after it went out keeps its counts and its
// last time in the list, with its problem and no next time (round 1).
func TestABrokenEntryKeepsItsHistory(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	h.writeSchedules(`{"schedules": [{"id": "evening", "when": "every 5m", "profile": "odrade", "message": "Run the evening gather."}]}`)
	list, err := h.client.Schedules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := list.Schedules[0]
	if got.Runs != 1 || got.Last == nil || got.Last.Outcome != proto.ScheduleSent || got.Problem == "" || got.Next != nil || got.Session != h.odrade {
		t.Fatalf("the broken entry %+v", got)
	}
}

// A pass arms its wake from the clock when it ends: a time that comes
// while the pass runs is woken for at it, or at once once it passed
// (round 1).
func TestTheWakeCountsFromTheEndOfThePass(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	if _, err := h.server.TeamSchedule(context.Background(), TeamCall{Session: h.odrade, Task: 1, CallID: "s1"}, schedule.Call{Action: schedule.ActionAdd, When: "in 1m", Message: "x"}); err != nil {
		t.Fatal(err)
	}
	h.runSchedulePasses()
	for _, tc := range []struct{ during, want time.Time }{
		{time.Date(2026, 11, 18, 22, 0, 50, 0, time.UTC), time.Date(2026, 11, 18, 22, 1, 0, 0, time.UTC)},
		{time.Date(2026, 11, 18, 22, 1, 10, 0, time.UTC), time.Date(2026, 11, 18, 22, 1, 10, 0, time.UTC)},
	} {
		h.setClock(time.Date(2026, 11, 18, 22, 0, 30, 0, time.UTC))
		h.server.afterScheduleFire = func() { h.setClock(tc.during) }
		h.server.scheduleSoon()
		h.runSchedulePasses()
		h.server.afterScheduleFire = nil
		if wake := h.nextScheduleWake(); !wake.Equal(tc.want) {
			t.Errorf("a pass that ends at %s wakes at %s, want %s", tc.during.Format("15:04:05"), wake.Format("15:04:05"), tc.want.Format("15:04:05"))
		}
	}
}

// A list too long for one event goes as an event with no list, which asks
// the client to fetch it; the client reads every event (round 1).
func TestALongScheduleListGoesAsAFetch(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	ctx := context.Background()
	text := strings.Repeat("x", schedule.MaxText)
	for i := range 300 {
		if _, _, err := h.store.AddSchedule(ctx, store.ScheduleRow{Key: "k" + strconv.Itoa(i), SessionID: int64(1000 + i/schedule.MaxPerSession), Spec: "every day 23:00",
			Zone: "UTC", Text: text, FirstAt: time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)}, schedule.MaxPerSession); err != nil {
			t.Fatal(err)
		}
	}
	list, err := h.client.Schedules(ctx)
	if err != nil || len(list.Schedules) != 300 {
		t.Fatalf("the list: %d %v", len(list.Schedules), err)
	}
	h.eventsMu.Lock()
	var event *proto.Event
	for i := range h.events {
		if h.events[i].Type == proto.EventSchedules {
			event = &h.events[i]
		}
	}
	h.eventsMu.Unlock()
	if event == nil || event.Schedules != nil {
		t.Fatalf("the event %+v", event)
	}
	var stream bytes.Buffer
	if err := proto.WriteEvent(&stream, *event); err != nil {
		t.Fatal(err)
	}
	read := 0
	if err := proto.ReadEvents(&stream, func(e proto.Event) error { read++; return nil }); !errors.Is(err, proto.ErrStreamEnded) || read != 1 {
		t.Fatalf("read %d events: %v", read, err)
	}
}

// An owner's entry that breaks a rule before its id keeps its record: once
// fixed, a time that went out once does not go out again (round 1).
func TestABrokenEntryKeepsItsRecordWhereverItsIDStands(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [{"id": "once", "when": "in 30m", "profile": "odrade", "message": "x"}]}`, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.at(time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 {
		t.Fatalf("sent %q", got)
	}
	h.writeSchedules(`{"schedules": [{"note": "x", "id": "once", "when": "in 30m", "profile": "odrade", "message": "x"}]}`)
	h.at(time.Date(2026, 11, 18, 22, 31, 0, 0, time.UTC))
	h.writeSchedules(`{"schedules": [{"id": "once", "when": "in 30m", "profile": "odrade", "message": "x"}]}`)
	h.at(time.Date(2026, 11, 18, 23, 30, 0, 0, time.UTC))
	h.at(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 {
		t.Fatalf("the time went out again: %q", got)
	}
}
