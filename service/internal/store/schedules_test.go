package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)

// A session's own schedule is recorded once per key, within its session's
// bound, counted where it is recorded (ADR-0030).
func TestASessionsScheduleIsRecordedOncePerKey(t *testing.T) {
	s, _ := open(t)
	row := ScheduleRow{Key: "team:1:2:c1", SessionID: 1, Spec: "in 30m", Zone: "UTC", Text: "look again", FirstAt: at}
	made, replayed, err := s.AddSchedule(ctx, row, 2)
	if err != nil || replayed || made.ID == 0 || made.Owner == "" || made.Source != ScheduleSession || made.State != ScheduleActive ||
		made.NextAt == nil || !made.NextAt.Equal(at) || !made.FirstAt.Equal(at) {
		t.Fatalf("made %+v %v %v", made, replayed, err)
	}
	again, replayed, err := s.AddSchedule(ctx, row, 2)
	if err != nil || !replayed || again.ID != made.ID || again.Owner != made.Owner {
		t.Fatalf("again %+v %v %v", again, replayed, err)
	}
	if _, _, err := s.AddSchedule(ctx, ScheduleRow{Key: "team:1:2:c2", SessionID: 1, Spec: "in 1h", Zone: "UTC", Text: "x", FirstAt: at}, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddSchedule(ctx, ScheduleRow{Key: "team:1:2:c3", SessionID: 1, Spec: "in 1h", Zone: "UTC", Text: "x", FirstAt: at}, 2); !errors.Is(err, ErrConflict) ||
		!strings.Contains(err.Error(), "at most 2 schedules") {
		t.Fatalf("a third: %v", err)
	}
	// Another session has its own bound; a cancelled one frees a place.
	if _, _, err := s.AddSchedule(ctx, ScheduleRow{Key: "team:2:2:c1", SessionID: 2, Spec: "in 1h", Zone: "UTC", Text: "x", FirstAt: at}, 2); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CancelSchedule(ctx, made.ID, 1); err != nil || got.State != ScheduleCancelled || got.NextAt != nil {
		t.Fatalf("cancelled %+v %v", got, err)
	}
	if _, _, err := s.AddSchedule(ctx, ScheduleRow{Key: "team:1:2:c3", SessionID: 1, Spec: "in 1h", Zone: "UTC", Text: "x", FirstAt: at}, 2); err != nil {
		t.Fatalf("after a cancel: %v", err)
	}
	// A cancel of another session's schedule finds none; one again keeps it
	// cancelled.
	if _, err := s.CancelSchedule(ctx, made.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another session's cancel: %v", err)
	}
	if got, err := s.CancelSchedule(ctx, made.ID, 1); err != nil || got.State != ScheduleCancelled {
		t.Fatalf("a cancel again %+v %v", got, err)
	}
	if got, _ := s.SessionSchedules(ctx, 1); len(got) != 2 {
		t.Fatalf("session 1 keeps %+v", got)
	}
}

// Each time goes out once: it files its note, keyed by the schedule's
// owner and the time's number, with the move to the next time; a time
// another pass moved on does nothing; a repeat whose last message waits
// skips the time and keeps its count; no session misses it.
func TestAScheduleGoesOutOncePerTime(t *testing.T) {
	s, _ := open(t)
	made, _, err := s.AddSchedule(ctx, ScheduleRow{Key: "k", SessionID: 7, Spec: "every 2h", Zone: "UTC", Text: "t", FirstAt: at}, 50)
	if err != nil {
		t.Fatal(err)
	}
	next := at.Add(2 * time.Hour)
	row, outcome, err := s.FireSchedule(ctx, made.ID, at, Fire{Now: at, Session: 7, Text: "[scheduled] t", Next: next})
	if err != nil || outcome != OutcomeSent || row.Runs != 1 || !row.NextAt.Equal(next) || row.LastOutcome != OutcomeSent || !row.LastAt.Equal(at) {
		t.Fatalf("first %+v %q %v", row, outcome, err)
	}
	note, err := s.Note(ctx, made.NoteKey(0))
	if err != nil || note.SessionID != 7 || note.Text != "[scheduled] t" || !strings.HasPrefix(note.Key, "schedule:"+made.Owner+":") {
		t.Fatalf("note %+v %v", note, err)
	}
	// The same time again, as a pass that read it before the first moved
	// it, does nothing.
	if again, outcome, err := s.FireSchedule(ctx, made.ID, at, Fire{Now: at, Session: 7, Text: "[scheduled] t", Next: next}); err != nil || outcome != "" || again.Runs != 1 {
		t.Fatalf("again %+v %q %v", again, outcome, err)
	}
	// The next time, while the first note still waits, is skipped.
	later := next.Add(2 * time.Hour)
	row, outcome, err = s.FireSchedule(ctx, made.ID, next, Fire{Now: next, Session: 7, Text: "[scheduled] t", Next: later})
	if err != nil || outcome != OutcomeSkipped || row.Runs != 1 || row.Skipped != 1 || !row.NextAt.Equal(later) || !strings.Contains(row.LastWhy, "still waits") {
		t.Fatalf("skipped %+v %q %v", row, outcome, err)
	}
	if _, err := s.Note(ctx, made.NoteKey(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a skipped time filed a note: %v", err)
	}
	// Once the first note is placed, the next time files the second.
	if err := s.NoteSent(ctx, made.NoteKey(0)); err != nil {
		t.Fatal(err)
	}
	row, outcome, err = s.FireSchedule(ctx, made.ID, later, Fire{Now: later, Session: 7, Text: "[scheduled] t", Next: later.Add(2 * time.Hour)})
	if err != nil || outcome != OutcomeSent || row.Runs != 2 || row.Skipped != 1 {
		t.Fatalf("second %+v %q %v", row, outcome, err)
	}
	if _, err := s.Note(ctx, made.NoteKey(1)); err != nil {
		t.Fatalf("the second note: %v", err)
	}
	// With no session, a time is missed, files nothing and counts nothing.
	due := *row.NextAt
	row, outcome, err = s.FireSchedule(ctx, made.ID, due, Fire{Now: due, Why: "no standing session", Next: due.Add(time.Hour)})
	if err != nil || outcome != OutcomeMissed || row.Runs != 2 || row.LastWhy != "no standing session" {
		t.Fatalf("missed %+v %q %v", row, outcome, err)
	}
	// A time with no next one is the last: the schedule is done.
	due = *row.NextAt
	if err := s.NoteSent(ctx, made.NoteKey(1)); err != nil {
		t.Fatal(err)
	}
	row, outcome, err = s.FireSchedule(ctx, made.ID, due, Fire{Now: due, Session: 7, Text: "x"})
	if err != nil || outcome != OutcomeSent || row.State != ScheduleDone || row.NextAt != nil {
		t.Fatalf("last %+v %q %v", row, outcome, err)
	}
	// A cancelled schedule goes out no more.
	other, _, _ := s.AddSchedule(ctx, ScheduleRow{Key: "k2", SessionID: 7, Spec: "in 1h", Zone: "UTC", Text: "t", FirstAt: at}, 50)
	if _, err := s.CancelSchedule(ctx, other.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := s.FireSchedule(ctx, other.ID, at, Fire{Now: at, Session: 7, Text: "x"}); err != nil || outcome != "" {
		t.Fatalf("a cancelled schedule: %q %v", outcome, err)
	}
}

// A time that goes out once is never skipped, though the schedule's last
// message, from when it repeated, still waits: only a repeat waits for its
// last message (Codex review of #51, round 1).
func TestATimeThatGoesOutOnceIsNeverSkipped(t *testing.T) {
	s, _ := open(t)
	if _, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{{Name: "e", Profile: "odrade", Spec: "every day 19:00", Zone: "UTC", Text: "t", First: at}}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Schedules(ctx)
	row := rows[0]
	if _, outcome, err := s.FireSchedule(ctx, row.ID, at, Fire{Now: at, Session: 3, Text: "t", Next: at.Add(24 * time.Hour)}); err != nil || outcome != OutcomeSent {
		t.Fatal(outcome, err)
	}
	// The owner makes it a time that goes out once, while its last message
	// still waits.
	once := at.Add(time.Hour)
	if _, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{{Name: "e", Profile: "odrade", Spec: "in 1h", Zone: "UTC", Text: "now", First: once}}, nil); err != nil {
		t.Fatal(err)
	}
	got, outcome, err := s.FireSchedule(ctx, row.ID, once, Fire{Now: once, Session: 3, Text: "now"})
	if err != nil || outcome != OutcomeSent || got.Runs != 2 || got.State != ScheduleDone {
		t.Fatalf("the time that goes out once %+v %q %v", got, outcome, err)
	}
	if note, err := s.Note(ctx, row.NoteKey(1)); err != nil || note.Text != "now" {
		t.Fatalf("its note %+v %v", note, err)
	}
}

// The owner's schedules follow their file: a new one starts at its first
// time, a changed when or zone starts again and keeps its count, a changed
// message or profile is taken for the next time, an entry that breaks a
// rule keeps its record, and one that left the file goes.
func TestTheOwnersSchedulesFollowTheirFile(t *testing.T) {
	s, _ := open(t)
	evening := OwnerSchedule{Name: "evening", Profile: "odrade", Spec: "every day 19:00", Zone: "UTC", Text: "gather", First: at.Add(time.Hour)}
	nudge := OwnerSchedule{Name: "nudge", Profile: "odrade", Spec: "every 2h", Zone: "UTC", Text: "look", First: at.Add(2 * time.Hour)}
	if changed, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{evening, nudge}, nil); err != nil || !changed {
		t.Fatalf("first sync %v %v", changed, err)
	}
	if changed, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{evening, nudge}, nil); err != nil || changed {
		t.Fatalf("the same file again changed %v %v", changed, err)
	}
	rows, _ := s.Schedules(ctx)
	if len(rows) != 2 || rows[0].Name != "evening" || rows[0].Source != ScheduleOwner || rows[0].Key != "owner:evening" || !rows[0].NextAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("rows %+v", rows)
	}
	first := rows[0]
	if _, outcome, err := s.FireSchedule(ctx, first.ID, *first.NextAt, Fire{Now: at.Add(time.Hour), Session: 3, Text: "x", Next: at.Add(25 * time.Hour)}); err != nil || outcome != OutcomeSent {
		t.Fatal(outcome, err)
	}
	// A new message is taken as it is, and keeps the next time.
	evening.Text = "gather now"
	if changed, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{evening, nudge}, nil); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got, _ := s.Schedule(ctx, first.ID); got.Text != "gather now" || !got.NextAt.Equal(at.Add(25*time.Hour)) || got.Runs != 1 {
		t.Fatalf("a new message %+v", got)
	}
	// A new when starts again from its first time, and keeps its count.
	evening.Spec, evening.First = "every day 20:00", at.Add(2*time.Hour)
	if _, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{evening, nudge}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Schedule(ctx, first.ID); got.Spec != "every day 20:00" || !got.NextAt.Equal(at.Add(2*time.Hour)) || got.Runs != 1 || got.Owner != first.Owner {
		t.Fatalf("a new when %+v", got)
	}
	// An entry that breaks a rule keeps its record; one gone from the file
	// goes.
	if _, err := s.SyncOwnerSchedules(ctx, nil, []string{"evening"}); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.Schedules(ctx)
	if len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("after the file changed %+v", rows)
	}
	// Back in the file, a schedule that left it is a new one.
	if _, err := s.SyncOwnerSchedules(ctx, []OwnerSchedule{evening, nudge}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.Schedules(ctx)
	if len(rows) != 2 || rows[1].Name != "nudge" || rows[1].Runs != 0 {
		t.Fatalf("nudge again %+v", rows)
	}
	if next, ok, err := s.NextSchedule(ctx); err != nil || !ok || !next.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("the next time %v %v %v", next, ok, err)
	}
	if due, err := s.DueSchedules(ctx, at.Add(2*time.Hour)); err != nil || len(due) != 2 {
		t.Fatalf("due %+v %v", due, err)
	}
}

// The passes read their partial indexes, never every schedule that ever
// went out.
func TestScheduleQueriesUseTheirIndexes(t *testing.T) {
	s, _ := openFresh(t)
	for query, tc := range map[string]struct {
		args    []any
		indexes []string
	}{
		dueSchedules:          {[]any{5}, []string{"schedules_due"}},
		countSessionSchedules: {[]any{3}, []string{"schedules_session"}},
		sessionSchedules:      {[]any{3}, []string{"schedules_session"}},
		// Every pass lists the schedules and reads the owner's: neither
		// reads the session schedules that went out or were cancelled (Codex
		// review of #51, round 1).
		listSchedules:  {nil, []string{"schedules_owner", "schedules_active"}},
		ownerSchedules: {nil, []string{"schedules_owner"}},
	} {
		plan := planOf(t, s, query, tc.args...)
		for _, index := range tc.indexes {
			if !strings.Contains(plan, index) {
				t.Errorf("%s: plan %q does not use %s", query, plan, index)
			}
		}
		for _, step := range strings.Split(plan, "; ") {
			if step == "SCAN schedules" {
				t.Errorf("%s: plan %q reads the whole table", query, plan)
			}
		}
	}
}
