package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// scheduleHarness is a standing harness (Odrade keeps a standing session,
// Vera plain ones) with the owner's schedules file, the events it emits,
// and Odrade's standing session open unless closed says not to.
type scheduleHarness struct {
	*harness
	dir      string
	odrade   int64
	eventsMu sync.Mutex
	events   []proto.Event
}

func newScheduleHarness(t *testing.T, file string, open bool) *scheduleHarness {
	t.Helper()
	h, dir, _ := standingHarness(t)
	sh := &scheduleHarness{harness: h, dir: dir}
	emit := h.server.emit
	h.server.emit = func(e proto.Event) {
		sh.eventsMu.Lock()
		sh.events = append(sh.events, e)
		sh.eventsMu.Unlock()
		emit(e)
	}
	if file != "" {
		sh.writeSchedules(file)
	}
	if open {
		session, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "odrade", Profile: "odrade"})
		if err != nil {
			t.Fatal(err)
		}
		sh.odrade = session.ID
	}
	return sh
}

func (h *scheduleHarness) writeSchedules(text string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, profiles.SchedulesFile), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// at moves the clock to t, fires the schedules' wakes due then, and runs
// the delivery passes they asked for.
func (h *scheduleHarness) at(t time.Time) {
	h.t.Helper()
	h.clock.mu.Lock()
	h.clock.now = t
	h.clock.mu.Unlock()
	h.fireScheduleWakes()
	h.runScheduled(20)
}

// lastSchedules is the last schedules event's list.
func (h *scheduleHarness) lastSchedules() *proto.Schedules {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	for i := len(h.events) - 1; i >= 0; i-- {
		if h.events[i].Type == proto.EventSchedules {
			return h.events[i].Schedules
		}
	}
	return nil
}

// sentTo are the texts sent to session whose key starts with prefix, in
// the order sent.
func (h *harness) sentTo(session int64, prefix string) []string {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	var out []string
	for _, key := range h.engine.order {
		if strings.HasPrefix(key, prefix) && h.engine.to[key] == session {
			out = append(out, h.engine.texts[key])
		}
	}
	return out
}

func (h *harness) scheduleRow(name string) store.ScheduleRow {
	h.t.Helper()
	if id, err := schedule.ParseID(name); err == nil {
		row, err := h.store.Schedule(context.Background(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		return row
	}
	rows, err := h.store.Schedules(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == name || schedule.ID(row.ID) == name {
			return row
		}
	}
	h.t.Fatalf("no schedule %s in %+v", name, rows)
	return store.ScheduleRow{}
}

const evening = `{"schedules": [{"id": "evening", "when": "every day 23:00", "profile": "odrade", "message": "Run the evening gather."}]}`

// The owner's schedule goes to its profile's standing session at its time,
// as "[scheduled] <message>", through the outbox, once a time, and on
// again the next day (ADR-0030).
func TestTheOwnersScheduleGoesOutAtItsTime(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	row := h.scheduleRow("evening")
	if row.NextAt == nil || !row.NextAt.Equal(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)) || row.Runs != 0 {
		t.Fatalf("the first time %+v", row)
	}
	// Nothing before its time; the pass checks again every minute.
	if wake := h.nextScheduleWake(); !wake.Equal(time.Date(2026, 11, 18, 22, 1, 0, 0, time.UTC)) {
		t.Fatalf("the wake %v", wake)
	}
	h.at(time.Date(2026, 11, 18, 22, 59, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 0 {
		t.Fatalf("sent before its time: %q", got)
	}
	if wake := h.nextScheduleWake(); !wake.Equal(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("the wake at the time %v", wake)
	}
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"+row.Owner+":0"); len(got) != 1 || got[0] != "[scheduled] Run the evening gather." {
		t.Fatalf("sent %q", got)
	}
	note, err := h.store.Note(context.Background(), row.NoteKey(0))
	if err != nil || note.SentAt == nil {
		t.Fatalf("the note %+v %v", note, err)
	}
	row = h.scheduleRow("evening")
	if row.Runs != 1 || !row.NextAt.Equal(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)) || row.LastOutcome != store.OutcomeSent {
		t.Fatalf("after its time %+v", row)
	}
	// A pass again at the same time sends nothing more.
	h.server.scheduleSoon()
	h.runSchedulePasses()
	h.runScheduled(20)
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 {
		t.Fatalf("a pass again sent %q", got)
	}
	// The schedules event says what went out, and when it goes next.
	list := h.lastSchedules()
	if list == nil || len(list.Schedules) != 1 || list.Zone != "UTC" {
		t.Fatalf("the event %+v", list)
	}
	if got := list.Schedules[0]; got.ID != "evening" || got.Source != proto.ScheduleOwner || got.Session != h.odrade || got.Runs != 1 || got.Repeat != "every day at 23:00" ||
		got.Next == nil || !got.Next.Equal(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)) || got.Last == nil || got.Last.Outcome != proto.ScheduleSent || got.Problem != "" {
		t.Fatalf("the event's schedule %+v", got)
	}
	h.at(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 2 {
		t.Fatalf("the next day sent %q", got)
	}
}

// A time missed while pocketd stopped goes out once, with no time before
// it, and the next is the first of its beat after now (ADR-0030).
func TestAScheduleMissedWhileStoppedGoesOutOnce(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [{"id": "nudge", "when": "every 2h", "profile": "odrade", "message": "Look at the desk."}]}`, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	// Three days later, as after a stop: one message.
	later := time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC)
	h.at(later)
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 || got[0] != "[scheduled] Look at the desk." {
		t.Fatalf("sent %q", got)
	}
	// It keeps its beat: every 2 hours from 00:00.
	if row := h.scheduleRow("nudge"); row.Runs != 1 || !row.NextAt.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after the gap %+v", row)
	}
}

// A repeat whose last message still waits for the session skips its time,
// counted, and sends no other until that one is placed (ADR-0030).
func TestARepeatSkipsWhileItsLastMessageWaits(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	row := h.scheduleRow("evening")
	// The session is at work: the message waits in its queue.
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{h.odrade: true}
	h.engine.queued = map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.mu.Lock()
	h.engine.queued[int64(len(h.engine.sent)+100)] = true
	h.engine.mu.Unlock()
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	if note, _ := h.store.Note(context.Background(), row.NoteKey(0)); note.SentAt != nil {
		t.Fatalf("a queued message counts as sent: %+v", note)
	}
	h.at(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC))
	row = h.scheduleRow("evening")
	if row.Runs != 1 || row.Skipped != 1 || row.LastOutcome != store.OutcomeSkipped || !row.NextAt.Equal(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("the skipped time %+v", row)
	}
	if got := h.sentTo(h.odrade, "schedule:"+row.Owner+":1"); len(got) != 0 {
		t.Fatalf("a second message went: %q", got)
	}
	if list := h.lastSchedules(); list == nil || list.Schedules[0].Skipped != 1 || list.Schedules[0].Last.Outcome != proto.ScheduleSkipped {
		t.Fatalf("the event %+v", list)
	}
	// Once the session took it, the next time goes.
	h.engine.mu.Lock()
	h.engine.queued = nil
	h.engine.busy = nil
	h.engine.mu.Unlock()
	h.runScheduled(20)
	h.server.deliverLater()
	h.runScheduled(20)
	h.at(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"+row.Owner+":1"); len(got) != 1 {
		t.Fatalf("the next message: %q", got)
	}
	if row = h.scheduleRow("evening"); row.Runs != 2 || row.Skipped != 1 {
		t.Fatalf("after it went %+v", row)
	}
}

// A schedule whose profile keeps no standing session, or has none open
// yet, misses its time, says why, and goes on to the next (ADR-0030).
func TestAScheduleWithNoStandingSessionMissesItsTime(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [
		{"id": "evening", "when": "every day 23:00", "profile": "odrade", "message": "gather"},
		{"id": "vera", "when": "every day 23:00", "profile": "vera", "message": "x"},
		{"id": "nobody", "when": "every day 23:00", "profile": "nobody", "message": "x"}
	]}`, false)
	h.server.StartSchedules()
	h.runSchedulePasses()
	h.at(time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC))
	for name, want := range map[string]string{
		"evening": "Odrade has no standing session yet: open it with New Session",
		"vera":    "profile vera keeps no standing session",
		"nobody":  "no valid profile nobody",
	} {
		row := h.scheduleRow(name)
		if row.LastOutcome != store.OutcomeMissed || !strings.Contains(row.LastWhy, want) || row.Runs != 0 || !row.NextAt.Equal(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: %+v", name, row)
		}
	}
	if got := h.engine.order; len(got) != 0 {
		t.Fatalf("sent %q", got)
	}
	// The list says why each does not go out.
	list, err := h.client.Schedules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Schedules[0]; got.Problem != "Odrade has no standing session yet: open it with New Session" || got.Last.Outcome != proto.ScheduleMissed {
		t.Fatalf("evening %+v", got)
	}
	// Once Odrade's session is open, the next time goes to it.
	session, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "odrade", Profile: "odrade"})
	if err != nil {
		t.Fatal(err)
	}
	h.at(time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC))
	if got := h.sentTo(session.ID, "schedule:"); len(got) != 1 || got[0] != "[scheduled] gather" {
		t.Fatalf("sent %q", got)
	}
}

// The owner's file is read at each pass: a new entry starts at its first
// time after now, a changed when starts again, an entry that breaks a rule
// goes out never, and one gone from the file goes (ADR-0030).
func TestTheOwnersSchedulesFollowTheFile(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	h.server.StartSchedules()
	h.runSchedulePasses()
	first := h.scheduleRow("evening")
	// The owner moves it to 22:30, in New York: the first 22:30 there after
	// now (17:00 there).
	h.writeSchedules(`{"schedules": [{"id": "evening", "when": "every day 22:30", "zone": "America/New_York", "profile": "odrade", "message": "Run the evening gather."}]}`)
	h.at(time.Date(2026, 11, 18, 22, 1, 0, 0, time.UTC))
	row := h.scheduleRow("evening")
	if row.ID != first.ID || row.Zone != "America/New_York" || !row.NextAt.Equal(time.Date(2026, 11, 19, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("moved %+v", row)
	}
	// A when that breaks a rule keeps the record and goes out never.
	h.writeSchedules(`{"schedules": [{"id": "evening", "when": "every 5m", "profile": "odrade", "message": "x"}]}`)
	h.at(time.Date(2026, 11, 19, 3, 30, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 0 {
		t.Fatalf("a broken entry sent %q", got)
	}
	if got := h.scheduleRow("evening"); got.ID != first.ID {
		t.Fatalf("the record %+v", got)
	}
	list := h.lastSchedules()
	if list == nil || !strings.Contains(list.Schedules[0].Problem, "at most every 10 minutes") || list.Schedules[0].Next != nil {
		t.Fatalf("the event %+v", list)
	}
	// Fixed as it was, the time due goes out once.
	h.writeSchedules(`{"schedules": [{"id": "evening", "when": "every day 22:30", "zone": "America/New_York", "profile": "odrade", "message": "Run the evening gather."}]}`)
	h.at(time.Date(2026, 11, 19, 3, 31, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 {
		t.Fatalf("the fixed entry sent %q", got)
	}
	// A file that cannot be read sends none of it, and keeps every record.
	h.writeSchedules(`{"schedules": 3}`)
	h.at(time.Date(2026, 11, 20, 3, 30, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 {
		t.Fatalf("a broken file sent %q", got)
	}
	list = h.lastSchedules()
	if list == nil || !strings.Contains(list.Problem, "schedules is no list") || len(list.Schedules) != 1 || list.Schedules[0].ID != "evening" || list.Schedules[0].Problem == "" {
		t.Fatalf("a broken file's list %+v", list)
	}
	// An entry gone from the file goes.
	h.writeSchedules(`{"schedules": []}`)
	h.at(time.Date(2026, 11, 20, 3, 31, 0, 0, time.UTC))
	if rows, _ := h.store.Schedules(context.Background()); len(rows) != 0 {
		t.Fatalf("rows %+v", rows)
	}
	if list := h.lastSchedules(); list == nil || len(list.Schedules) != 0 {
		t.Fatalf("the empty list %+v", list)
	}
}

// team_schedule keeps a supervisor's own reminders and repeats: an add is
// recorded once per call and answers as it did, list shows them, cancel
// ends one, and nobody else keeps schedules (ADR-0030).
func TestTheSupervisorKeepsItsOwnSchedules(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	ctx := context.Background()
	call := TeamCall{Session: h.odrade, Task: 7, CallID: "s1"}
	add := schedule.Call{Action: schedule.ActionAdd, When: "in 30m", Message: "Look at the deploy again."}
	text, err := h.server.TeamSchedule(ctx, call, add)
	if err != nil || text != `Scheduled S1: once, Wed 18 Nov 22:30 UTC. It comes to this session as a message that starts with "[scheduled]"; cancel it with team_schedule cancel S1.` {
		t.Fatalf("add: %q %v", text, err)
	}
	// The same call again, later, answers as the first did and adds none.
	h.clock.Advance(time.Hour)
	if again, err := h.server.TeamSchedule(ctx, call, add); err != nil || again != text {
		t.Fatalf("again: %q %v", again, err)
	}
	if rows, _ := h.store.SessionSchedules(ctx, h.odrade); len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 7, CallID: "s2"}, schedule.Call{Action: schedule.ActionAdd, When: "every weekday 8:00", Message: "Read the desk."}); err != nil {
		t.Fatal(err)
	}
	list, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 8, CallID: "l1"}, schedule.Call{Action: schedule.ActionList})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"evening (the owner's): every day at 23:00, next Thu 19 Nov 23:00 UTC · Run the evening gather.",
		"S1: once, next Wed 18 Nov 22:30 UTC · Look at the deploy again.",
		"S2: weekdays at 08:00, next Thu 19 Nov 08:00 UTC · Read the desk.",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("the list lacks %q:\n%s", want, list)
		}
	}
	// A cancel ends one, and answers the same again; the owner's are
	// changed only in their file; another session's is none of this one's.
	cancel := schedule.Call{Action: schedule.ActionCancel, ID: "S1"}
	if got, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 8, CallID: "c1"}, cancel); err != nil || got != "Cancelled S1: it sends nothing more." {
		t.Fatalf("cancel: %q %v", got, err)
	}
	if got, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 8, CallID: "c1"}, cancel); err != nil || got != "Cancelled S1: it sends nothing more." {
		t.Fatalf("cancel again: %q %v", got, err)
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 8, CallID: "c2"}, schedule.Call{Action: schedule.ActionCancel, ID: "evening"}); err == nil ||
		!strings.Contains(err.Error(), "the owner's schedules change only in their schedules file") {
		t.Fatalf("cancel the owner's: %v", err)
	}
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "vera", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: vera.ID, Task: 1, CallID: "v1"}, add); err == nil || !strings.Contains(err.Error(), "only a supervisor keeps schedules") {
		t.Fatalf("an agent's add: %v", err)
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: vera.ID, Task: 1, CallID: "v2"}, schedule.Call{Action: schedule.ActionCancel, ID: "S2"}); err == nil {
		t.Fatal("an agent cancelled the supervisor's schedule")
	}
	// What breaks a rule is refused, and records nothing.
	for name, c := range map[string]schedule.Call{
		"too often":    {Action: schedule.ActionAdd, When: "every 5m", Message: "x"},
		"no time":      {Action: schedule.ActionAdd, When: "soon", Message: "x"},
		"too long":     {Action: schedule.ActionAdd, When: "in 1h", Message: strings.Repeat("x", schedule.MaxText+1)},
		"no message":   {Action: schedule.ActionAdd, When: "in 1h", Message: " "},
		"passed today": {Action: schedule.ActionAdd, When: "today 9:00", Message: "x"},
	} {
		if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 9, CallID: name}, c); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if rows, _ := h.store.SessionSchedules(ctx, h.odrade); len(rows) != 1 {
		t.Fatalf("rows after refusals %+v", rows)
	}
}

// A session keeps at most schedule.MaxPerSession schedules that go out
// (ADR-0030).
func TestASupervisorsSchedulesAreBounded(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	ctx := context.Background()
	for i := range schedule.MaxPerSession {
		if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "a" + string(rune('A'+i))}, schedule.Call{Action: schedule.ActionAdd, When: "in 1h", Message: "x"}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "over"}, schedule.Call{Action: schedule.ActionAdd, When: "in 1h", Message: "x"}); err == nil ||
		err.Error() != "a session keeps at most 50 schedules that go out: cancel one first" {
		t.Fatalf("the 51st: %v", err)
	}
}

// A supervisor's own schedule goes to its session at its time, through the
// outbox; a cancelled one goes no more (ADR-0030).
func TestASupervisorsScheduleGoesToItsSession(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	ctx := context.Background()
	h.server.StartSchedules()
	h.runSchedulePasses()
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "s1"}, schedule.Call{Action: schedule.ActionAdd, When: "in 30m", Message: "Look again."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "s2"}, schedule.Call{Action: schedule.ActionAdd, When: "in 20m", Message: "Never."}); err != nil {
		t.Fatal(err)
	}
	// The add asked for a pass, which wakes at the soonest time.
	if n := h.runSchedulePasses(); n == 0 {
		t.Fatal("the add ran no pass")
	}
	if wake := h.nextScheduleWake(); !wake.Equal(time.Date(2026, 11, 18, 22, 1, 0, 0, time.UTC)) {
		t.Fatalf("the wake %v", wake)
	}
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "c2"}, schedule.Call{Action: schedule.ActionCancel, ID: "S2"}); err != nil {
		t.Fatal(err)
	}
	h.at(time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC))
	if got := h.sentTo(h.odrade, "schedule:"); len(got) != 1 || got[0] != "[scheduled] Look again." {
		t.Fatalf("sent %q", got)
	}
	if row := h.scheduleRow("S1"); row.State != store.ScheduleDone || row.NextAt != nil || row.Runs != 1 {
		t.Fatalf("a time that went out once %+v", row)
	}
	// A cancel of one that went out says so.
	if got, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "c1"}, schedule.Call{Action: schedule.ActionCancel, ID: "S1"}); err != nil ||
		!strings.Contains(got, "went out already") {
		t.Fatalf("cancel after: %q %v", got, err)
	}
}

// No pass runs before StartSchedules, which the service calls after
// engine.Resume (ADR-0030).
func TestSchedulesWaitForTheirStart(t *testing.T) {
	h := newScheduleHarness(t, evening, true)
	if _, err := h.server.TeamSchedule(context.Background(), TeamCall{Session: h.odrade, Task: 1, CallID: "s1"}, schedule.Call{Action: schedule.ActionAdd, When: "in 30m", Message: "x"}); err != nil {
		t.Fatal(err)
	}
	if n := h.runSchedulePasses(); n != 0 {
		t.Fatalf("%d passes ran before the start", n)
	}
	h.server.StartSchedules()
	if n := h.runSchedulePasses(); n != 1 {
		t.Fatalf("%d passes ran at the start", n)
	}
}

// GET /v1/schedules lists the owner's schedules in their file's order,
// each with when it goes next or why it does not, then the supervisors'
// own, with the service's zone (ADR-0030).
func TestGetSchedulesListsThem(t *testing.T) {
	h := newScheduleHarness(t, `{"schedules": [
		{"id": "evening", "when": "every mon,thu 19:00", "zone": "Europe/Berlin", "profile": "odrade", "message": "gather"},
		{"id": "vera", "when": "every day 23:00", "profile": "vera", "message": "x"},
		{"id": "fast", "when": "every 5m", "profile": "odrade", "message": "x"},
		{"when": "every day 8:00", "profile": "odrade", "message": "x"}
	]}`, true)
	ctx := context.Background()
	if _, err := h.server.TeamSchedule(ctx, TeamCall{Session: h.odrade, Task: 1, CallID: "s1"}, schedule.Call{Action: schedule.ActionAdd, When: "every 2h", Message: "Look."}); err != nil {
		t.Fatal(err)
	}
	list, err := h.client.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Zone != "UTC" || list.Problem != "" || len(list.Schedules) != 5 {
		t.Fatalf("list %+v", list)
	}
	evening := list.Schedules[0]
	if evening.ID != "evening" || evening.Source != proto.ScheduleOwner || evening.Profile != "odrade" || evening.Session != h.odrade || evening.Zone != "Europe/Berlin" ||
		evening.Repeat != "mon, thu at 19:00" || evening.Next == nil || !evening.Next.Equal(time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC)) || evening.Problem != "" {
		t.Errorf("evening %+v", evening)
	}
	if vera := list.Schedules[1]; vera.Problem != "profile vera keeps no standing session (session: standing)" || vera.Session != 0 {
		t.Errorf("vera %+v", vera)
	}
	if fast := list.Schedules[2]; !strings.Contains(fast.Problem, "at most every 10 minutes") || fast.Next != nil {
		t.Errorf("fast %+v", fast)
	}
	if noID := list.Schedules[3]; noID.ID != "schedule 4" || !strings.Contains(noID.Problem, "has no id") {
		t.Errorf("no id %+v", noID)
	}
	own := list.Schedules[4]
	if own.Source != proto.ScheduleSession || own.Session != h.odrade || own.Repeat != "every 2 hours" || own.Message != "Look." || own.Next == nil ||
		!own.Next.Equal(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)) || !strings.HasPrefix(own.ID, "S") {
		t.Errorf("own %+v", own)
	}
	// The list went out as an event too.
	if event := h.lastSchedules(); event == nil || len(event.Schedules) != 5 {
		t.Fatalf("the event %+v", event)
	}
	// With no profiles, none of the owner's goes out, and the list says so.
	h.server.SetProfiles(nil)
	list, err = h.client.Schedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Problem != "pocketd runs without profiles" || list.Schedules[0].ID != "evening" || list.Schedules[0].Problem != "pocketd runs without profiles" {
		t.Fatalf("without profiles %+v", list)
	}
}
