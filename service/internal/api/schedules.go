package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Schedules (ADR-0030) send a session a message later, or on repeat: the
// owner's, from schedules.json in the profile folder, to a profile's
// standing session, and a supervisor's own, from team_schedule, to its
// session. A pass, one at a time, reads the owner's file, brings their
// records in step with it, and files each time that is due as a note, in
// the transaction that moves the schedule on (store.FireSchedule); the
// delivery pass places the note, which counts as sent only once
// Engine.Placed finds it in the transcript. A pass runs at start, after
// engine.Resume, at the soonest time a schedule goes out, every
// scheduleCheck, so a change of the owner's file is seen, and after each
// team_schedule call.

// scheduleCheck is how often a pass runs whatever is due: the owner's file
// is read again then.
const scheduleCheck = time.Minute

// SetZone sets the service's time zone: a schedule that names none reads
// its clock times in it (ADR-0030). Call it before StartSchedules.
func (s *Server) SetZone(zone *time.Location) {
	if zone != nil {
		s.zone = zone
	}
}

// StartSchedules starts the schedules' passes. The service calls it once
// at start, after engine.Resume, since a pass files messages a delivery
// pass submits, and does not wait for the first pass: a time missed while
// pocketd stopped goes out once then, with no time before it.
func (s *Server) StartSchedules() {
	s.sessionMu.Lock()
	s.schedulesOn = true
	s.sessionMu.Unlock()
	s.scheduleSoon()
}

// scheduleSoon runs a schedules pass on its own goroutine, once the
// schedules started; one asked for while a pass runs runs after it.
func (s *Server) scheduleSoon() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped || !s.schedulesOn {
		return
	}
	if s.scheduling {
		s.scheduleAgain = true
		return
	}
	s.scheduling = true
	s.goSchedule(func() {
		for {
			s.sessionMu.Lock()
			if s.stopped {
				s.scheduling = false
				s.sessionMu.Unlock()
				return
			}
			s.passes.Add(1)
			s.sessionMu.Unlock()
			s.schedulePass(s.passCtx)
			s.passes.Done()
			s.sessionMu.Lock()
			again := s.scheduleAgain && !s.stopped
			s.scheduleAgain, s.scheduling = false, again
			s.sessionMu.Unlock()
			if !again {
				return
			}
		}
	})
}

// ownerFile is the owner's schedules file as a pass read it.
type ownerFile struct {
	// problem says why none of it goes out: no profiles, or a file that
	// cannot be read or is no schedules file.
	problem string
	entries []profiles.ScheduleEntry
	// fires names the entries that go out: no problem of their own.
	fires map[string]bool
}

// schedulePass reads the owner's file and brings their records in step,
// files each time that is due, publishes the schedules when they changed,
// and wakes at the soonest time one goes out, or after scheduleCheck.
func (s *Server) schedulePass(ctx context.Context) {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	// An upgrade of the supervisors' tools that failed at start, as for a
	// profile that could not be read, goes again (Codex review of #51,
	// round 2).
	if s.toolsDue {
		if err := s.upgradeSupervisorTools(ctx); err != nil {
			s.log.Warn("give team_schedule to older supervisors' sessions", "err", err)
		} else {
			s.toolsDue = false
		}
	}
	now := s.now()
	file, err := s.syncOwners(ctx, now)
	if err != nil {
		s.log.Warn("bring the owner's schedules in step", "err", err)
	}
	filed, err := s.fireDue(ctx, now, file)
	if err != nil {
		s.log.Warn("send the schedules that are due", "err", err)
	}
	if filed {
		s.deliverLater()
	}
	if s.afterScheduleFire != nil {
		s.afterScheduleFire()
	}
	rows, err := s.store.Schedules(ctx)
	if err != nil {
		s.log.Warn("read the schedules", "err", err)
		s.armSchedules(nil, now)
		return
	}
	s.publishSchedulesLocked(ctx, file, rows)
	s.armSchedules(fireable(rows, file), now)
}

// syncOwners reads the owner's file and brings their records in step with
// it (store.SyncOwnerSchedules): a schedule new, or whose when or zone
// changed, goes out first at its first time after now. An entry that
// breaks a rule keeps its record and goes out never while it does; so does
// every record while the file cannot be read.
func (s *Server) syncOwners(ctx context.Context, now time.Time) (ownerFile, error) {
	file := s.readOwners()
	if file.problem != "" {
		return file, nil
	}
	file.fires = map[string]bool{}
	rows, err := s.store.Schedules(ctx)
	if err != nil {
		return ownerFile{problem: "pocketd cannot read its schedules now"}, err
	}
	held := map[string]store.ScheduleRow{}
	for _, row := range rows {
		if row.Source == store.ScheduleOwner {
			held[row.Name] = row
		}
	}
	var sync []store.OwnerSchedule
	var kept []string
	for i, e := range file.entries {
		if e.Problem != "" {
			if e.ID != "" {
				kept = append(kept, e.ID)
			}
			continue
		}
		zone, err := schedule.Zone(e.Zone, s.zone)
		if err != nil {
			file.entries[i].Problem = err.Error()
			kept = append(kept, e.ID)
			continue
		}
		// Compared as schedule.Parse reads it: a change of case or spaces
		// alone changes nothing (Codex review of #51, round 2).
		spec := schedule.Canonical(e.When)
		entry := store.OwnerSchedule{Name: e.ID, Profile: e.Profile, Spec: spec, Zone: zone.String(), Text: e.Message, Gather: gatherJSON(e.Gather)}
		// The first time counts only for a schedule new, or whose when or
		// zone changed: one that went out already, as a today time that has
		// passed since, keeps its record as it is.
		if row, ok := held[e.ID]; !ok || row.Spec != entry.Spec || row.Zone != entry.Zone {
			first, err := e.Spec.First(now, zone)
			if err != nil {
				file.entries[i].Problem = fmt.Sprintf("schedule %d's when: %v", e.N, err)
				kept = append(kept, e.ID)
				continue
			}
			entry.First = first
		}
		sync = append(sync, entry)
		file.fires[e.ID] = true
	}
	if _, err := s.store.SyncOwnerSchedules(ctx, sync, kept); err != nil {
		return ownerFile{problem: "pocketd cannot keep the owner's schedules now"}, err
	}
	return file, nil
}

// fireable are the schedules a pass may send: every session's own, and the
// owner's whose entry goes out.
func fireable(rows []store.ScheduleRow, file ownerFile) []store.ScheduleRow {
	var out []store.ScheduleRow
	for _, row := range rows {
		if row.State != store.ScheduleActive || row.NextAt == nil {
			continue
		}
		if row.Source == store.ScheduleOwner && !file.fires[row.Name] {
			continue
		}
		out = append(out, row)
	}
	return out
}

// errNotNow is a check pocketd cannot make now, as a profile it cannot
// read, or a gather's skill its sandbox could not read: a schedule's time
// waits for a later pass.
var errNotNow = errors.New("pocketd cannot tell now")

// fireDue files each time that is due by now, once (store.FireSchedule),
// and says whether one filed a message. A repeat goes out next at its
// first time after now: a time missed while pocketd stopped goes out
// once, with none before it.
func (s *Server) fireDue(ctx context.Context, now time.Time, file ownerFile) (bool, error) {
	due, err := s.store.DueSchedules(ctx, now)
	if err != nil {
		return false, err
	}
	filed := false
	var failures []error
	for _, row := range fireable(due, file) {
		fire := store.Fire{Now: now, Text: schedule.Message(row.Text)}
		spec, specErr := schedule.Parse(row.Spec)
		switch {
		case specErr != nil:
			// A record this build no longer reads goes out never again.
			fire.Why = fmt.Sprintf("pocketd no longer reads its when %q", row.Spec)
		default:
			if spec.Repeat != nil {
				fire.Next = spec.Repeat.After(*row.NextAt, now, s.scheduleZone(row.Zone))
			}
			// A repeat its last message or gather still holds is skipped
			// before its next action is prepared: no check of that action
			// decides a time already held (Codex review of #57, round 5).
			if row.Runs > 0 && !fire.Next.IsZero() {
				skipped, err := s.skipIfHeld(ctx, row, fire)
				if err != nil {
					failures = append(failures, fmt.Errorf("schedule %d: %w", row.ID, err))
					continue
				}
				if skipped {
					s.log.Info("a schedule skipped a time: its last action still holds it", "schedule", row.ID)
					continue
				}
			}
			// A gather's schedule starts the gather in its note's place
			// (ADR-0033): its skill is read now, and its agents checked,
			// before its session opens; one that cannot start misses its
			// time, and says why. A check pocketd could not make now waits
			// for a later pass (Codex review of #57, round 1).
			var start *store.GatherStart
			if row.Gather != "" {
				var g team.Gather
				err := json.Unmarshal([]byte(row.Gather), &g)
				var gather store.GatherStart
				if err == nil {
					gather, err = s.gatherStart(ctx, g)
				}
				switch {
				case errors.Is(err, errNotNow):
					continue
				case err != nil:
					fire.Why = "its gather cannot start: " + err.Error()
				default:
					gather.Schedule = row.Name
					start = &gather
				}
			}
			if fire.Why == "" {
				session, why, err := s.scheduleTarget(ctx, row)
				if errors.Is(err, errNotNow) {
					continue
				} else if err != nil {
					failures = append(failures, fmt.Errorf("schedule %d: %w", row.ID, err))
					continue
				}
				fire.Session, fire.Why = session, why
				if session != 0 {
					fire.Gather = start
				}
			}
		}
		outcome, err := s.fireOne(ctx, row, fire)
		if err != nil {
			failures = append(failures, fmt.Errorf("schedule %d: %w", row.ID, err))
			continue
		}
		if outcome == store.OutcomeSent && fire.Gather != nil {
			s.publishGatherOf(ctx, row.NoteKey(row.Runs))
		}
		filed = filed || outcome == store.OutcomeSent
		if outcome == store.OutcomeSkipped {
			s.log.Info("a schedule skipped a time: its last message still waits", "schedule", row.ID)
		}
	}
	return filed, errors.Join(failures...)
}

// fireOne files a schedule's time (store.FireSchedule). A repeat whose last
// message's note is not noted sent first has that note's placement looked
// up in the durable store, under outboxMu, which the time's own record
// holds too: a message PiG placed before a stop kept pocketd from noting
// it never skips the next time (Codex review of #51, round 1).
func (s *Server) fireOne(ctx context.Context, row store.ScheduleRow, fire store.Fire) (string, error) {
	if row.Runs == 0 || fire.Next.IsZero() || fire.Session == 0 {
		_, outcome, err := s.store.FireSchedule(ctx, row.ID, *row.NextAt, fire)
		return outcome, err
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if err := s.settleNote(ctx, row.NoteKey(row.Runs-1)); err != nil {
		return "", fmt.Errorf("look up its last message: %w", err)
	}
	_, outcome, err := s.store.FireSchedule(ctx, row.ID, *row.NextAt, fire)
	return outcome, err
}

// skipIfHeld skips a repeat's time its last message or gather still holds
// (store.SkipIfHeld), its last message's place looked up first, under
// outboxMu, as fireOne does.
func (s *Server) skipIfHeld(ctx context.Context, row store.ScheduleRow, fire store.Fire) (bool, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if err := s.settleNote(ctx, row.NoteKey(row.Runs-1)); err != nil {
		return false, fmt.Errorf("look up its last message: %w", err)
	}
	outcome, err := s.store.SkipIfHeld(ctx, row.ID, *row.NextAt, fire.Now, fire.Next)
	return outcome == store.OutcomeSkipped, err
}

// settleNote notes note key sent once the durable store has its message in
// its session's transcript, by any attempt (Engine.Placed); outboxMu is
// held. A note not filed, sent or dropped is left as it is.
func (s *Server) settleNote(ctx context.Context, key string) error {
	note, err := s.store.Note(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil || note.SentAt != nil || note.DroppedAt != nil {
		return err
	}
	for attempt := note.Attempt; attempt >= 0; attempt-- {
		where, err := s.engine.Placed(ctx, note.SessionID, attemptKey(note.Key, attempt))
		if err != nil {
			return err
		}
		if where == PlacementPlaced {
			return s.store.NoteSent(ctx, note.Key)
		}
	}
	return nil
}

// scheduleZone is the zone a schedule's record names: the service's for
// its name, which no zone database may have, as one TZ names by its file,
// and for a name that no longer loads (Codex review of #51, round 1).
func (s *Server) scheduleZone(name string) *time.Location {
	if name == "" || name == s.zone.String() {
		return s.zone
	}
	zone, err := schedule.Zone(name, s.zone)
	if err != nil {
		s.log.Warn("a schedule's zone no longer loads: the service's is taken", "zone", name, "err", err)
		return s.zone
	}
	return zone
}

// scheduleTarget is the session a schedule goes to: a session's own goes
// to its session; the owner's to its profile's standing session, while its
// profile is valid and keeps one (why says otherwise, with no session),
// and is a supervisor's for a gather. A profile pocketd cannot read now is
// errNotNow.
func (s *Server) scheduleTarget(ctx context.Context, row store.ScheduleRow) (int64, string, error) {
	if row.Source == store.ScheduleSession {
		return row.SessionID, "", nil
	}
	return s.scheduleSession(ctx, row.Profile, true, row.Gather != "")
}

// scheduleSession is profile's standing session, or why there is none.
// open opens one the profile has none of yet (openStandingFor), as a
// schedule's time does; a list's read leaves it, with no why. A gather's
// profile must be a supervisor's, as team_gather's caller must: it keeps
// the board its gather fills (Codex review of #57, round 1).
func (s *Server) scheduleSession(ctx context.Context, profile string, open, gather bool) (int64, string, error) {
	if s.profiles == nil {
		return 0, "pocketd runs without profiles", nil
	}
	p, err := s.profiles.Find(profile)
	if err != nil {
		if profiles.Unreadable(err) {
			return 0, "", fmt.Errorf("%w: %v", errNotNow, err)
		}
		return 0, fmt.Sprintf("no valid profile %s: %v", profile, err), nil
	}
	if why := standingWhy(p, gather); why != "" {
		return 0, why, nil
	}
	id, err := s.store.StandingSession(ctx, profile)
	if err == nil {
		// A time goes only to a watched session: a standing session whose
		// watch failed as pocketd opened it has it started again before
		// the time is filed (Codex review of #57, round 2). A list's read
		// starts nothing.
		if open {
			if err := s.engine.Watch(ctx, id); err != nil {
				return 0, "", fmt.Errorf("%w: watch its standing session: %v", errNotNow, err)
			}
		}
		return id, "", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, "", err
	}
	// No standing session yet: pocketd opens it, as New Session would (the
	// owner's choice of 2026-11-21, ADR-0033). One its profile cannot start
	// misses the time, and says why.
	if !open {
		return 0, "", nil // a list's read: the time opens it, so nothing stops it
	}
	id, why, err := s.openStandingFor(ctx, p.ID, gather)
	switch {
	case errors.Is(err, errInternal) || errors.Is(err, errNotNow):
		return 0, "", err
	case err != nil:
		return 0, fmt.Sprintf("pocketd could not open %s's standing session: %v", p.Name, err), nil
	}
	return id, why, nil
}

// armSchedules wakes a pass at the soonest time one of rows goes out, or
// after scheduleCheck, whichever is sooner, counted from the clock as it
// arms, not as the pass began: a time that came while the pass ran wakes
// one at once (Codex review of #51, round 1). A time due already when the
// pass began, which it did not send, as one whose profile could not be
// read, waits for the check.
func (s *Server) armSchedules(rows []store.ScheduleRow, began time.Time) {
	now := s.now()
	wake := scheduleCheck
	for _, row := range rows {
		if !row.NextAt.After(began) {
			continue
		}
		if d := max(row.NextAt.Sub(now), 0); d < wake {
			wake = d
		}
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.scheduleCancel != nil {
		s.scheduleCancel()
		s.scheduleCancel = nil
	}
	if s.stopped {
		return
	}
	s.scheduleCancel = s.scheduleTimer(wake, s.scheduleSoon)
}

// scheduleList is the schedules as the protocol has them: the owner's, in
// their file's order, then every session's own that still goes out.
func (s *Server) scheduleList(ctx context.Context, file ownerFile, rows []store.ScheduleRow) proto.Schedules {
	out := proto.Schedules{Zone: s.zone.String(), Problem: file.problem, Schedules: []proto.Schedule{}}
	owners := map[string]store.ScheduleRow{}
	for _, row := range rows {
		if row.Source == store.ScheduleOwner {
			owners[row.Name] = row
		}
	}
	type targetKey struct {
		profile string
		gather  bool
	}
	targets := map[targetKey]proto.Schedule{}
	// Each skill is read once a listing, its failure too; a gather's start
	// reads it again (Codex review of #57, round 5).
	skills := map[string]error{}
	target := func(profile string, gather bool) proto.Schedule {
		if t, ok := targets[targetKey{profile, gather}]; ok {
			return t
		}
		var t proto.Schedule
		session, why, err := s.scheduleSession(ctx, profile, false, gather)
		switch {
		case err != nil:
			t.Problem = "pocketd cannot tell its session now: " + err.Error()
		case session == 0:
			t.Problem = why
		default:
			t.Session = session
		}
		targets[targetKey{profile, gather}] = t
		return t
	}
	if file.problem != "" {
		// The file cannot be read: its records stay, and go out never while
		// it cannot.
		for _, row := range rows {
			if row.Source == store.ScheduleOwner {
				item := wireSchedule(row, s.zone)
				item.ID, item.Next, item.Problem = row.Name, nil, file.problem
				out.Schedules = append(out.Schedules, item)
			}
		}
	}
	for _, e := range file.entries {
		item := proto.Schedule{ID: e.ID, Source: proto.ScheduleOwner, Profile: e.Profile, When: e.When, Zone: e.Zone, Message: e.Message, Gather: wireScheduleGather(gatherJSON(e.Gather)), Problem: e.Problem}
		if item.ID == "" {
			item.ID = fmt.Sprintf("schedule %d", e.N)
		}
		if item.Zone == "" {
			item.Zone = s.zone.String()
		}
		if e.Spec.Repeat != nil {
			item.Repeat = e.Spec.Repeat.String()
		}
		if row, ok := owners[e.ID]; ok && file.fires[e.ID] {
			item = wireSchedule(row, s.zone)
			item.ID, item.When = e.ID, e.When
		} else if ok {
			// An entry that breaks a rule keeps what its record counted, with
			// its problem and no next time (Codex review of #51, round 1).
			kept := wireSchedule(row, s.zone)
			item.Runs, item.Skipped, item.Last = kept.Runs, kept.Skipped, kept.Last
		} else if item.Problem == "" {
			// An entry no pass has read yet goes out first at its first
			// time from now, as the next pass records it.
			if zone, err := schedule.Zone(e.Zone, s.zone); err != nil {
				item.Problem = err.Error()
			} else if first, err := e.Spec.First(s.now(), zone); err != nil {
				item.Problem = fmt.Sprintf("schedule %d's when: %v", e.N, err)
			} else {
				item.Next = &first
			}
		}
		if e.Profile != "" {
			t := target(e.Profile, e.Gather != nil)
			item.Session = t.Session
			if item.Problem == "" {
				item.Problem = t.Problem
			}
		}
		// A gather whose skill is not in the skills folder misses its time
		// (ADR-0033); the skill is read as the agents' sandbox shows it.
		if e.Gather != nil && item.Problem == "" {
			err, read := skills[e.Gather.Skill]
			if !read {
				_, err = s.loadSkill(ctx, e.Gather.Skill)
				skills[e.Gather.Skill] = err
			}
			if err != nil {
				item.Problem = err.Error()
			}
		}
		out.Schedules = append(out.Schedules, item)
	}
	for _, row := range rows {
		if row.Source == store.ScheduleSession && row.State == store.ScheduleActive {
			out.Schedules = append(out.Schedules, wireSchedule(row, s.zone))
		}
	}
	return out
}

// wireSchedule is a schedule's record as the protocol has it.
func wireSchedule(row store.ScheduleRow, service *time.Location) proto.Schedule {
	item := proto.Schedule{ID: schedule.ID(row.ID), Source: proto.ScheduleSession, Session: row.SessionID, When: row.Spec, Zone: row.Zone,
		Message: row.Text, Gather: wireScheduleGather(row.Gather), Next: row.NextAt, Runs: row.Runs, Skipped: row.Skipped}
	if row.Source == store.ScheduleOwner {
		item.ID, item.Source, item.Session, item.Profile = row.Name, proto.ScheduleOwner, 0, row.Profile
	}
	if spec, err := schedule.Parse(row.Spec); err == nil && spec.Repeat != nil {
		item.Repeat = spec.Repeat.String()
	}
	if row.LastAt != nil {
		item.Last = &proto.ScheduleTime{At: *row.LastAt, Outcome: proto.ScheduleOutcome(row.LastOutcome), Why: row.LastWhy}
	}
	return item
}

// publishSchedulesLocked says the schedules changed when they differ from
// the last ones it said so of, by their digest; scheduleMu is held. The
// event carries no list, which the client fetches: a list may run to many
// megabytes, as the supervisors' schedules count per session, and the hub
// keeps thousands of events for clients that come back (Codex review of
// #51, rounds 1 and 3).
func (s *Server) publishSchedulesLocked(ctx context.Context, file ownerFile, rows []store.ScheduleRow) proto.Schedules {
	list := s.scheduleList(ctx, file, rows)
	encoded, err := json.Marshal(list)
	if err != nil {
		return list
	}
	sum := sha256.Sum256(encoded)
	if sum == s.schedulesSent {
		return list
	}
	s.schedulesSent = sum
	s.emit(proto.Event{Type: proto.EventSchedules})
	return list
}

// listSchedules answers GET /v1/schedules: the owner's file is read and
// their records brought in step first, so the answer shows what goes out
// now; a pass then wakes at what changed.
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	s.scheduleMu.Lock()
	file, err := s.syncOwners(ctx, s.now())
	if err != nil {
		s.log.Warn("bring the owner's schedules in step", "err", err)
	}
	rows, err := s.store.Schedules(ctx)
	if err != nil {
		s.scheduleMu.Unlock()
		s.internal(w, "list schedules", err)
		return
	}
	list := s.publishSchedulesLocked(ctx, file, rows)
	s.scheduleMu.Unlock()
	s.scheduleSoon()
	writeJSON(w, http.StatusOK, list)
}

// TeamSchedule is team_schedule (ADR-0030): a supervisor's own reminders
// and repeats, to the session that calls it. An add is recorded once per
// call (api.TeamCall), and the call run again answers as the first did,
// whatever the rules say by then; a cancel's answer depends only on the
// schedule.
func (s *Server) TeamSchedule(ctx context.Context, call TeamCall, c schedule.Call) (string, error) {
	ctx = context.WithoutCancel(ctx)
	if err := schedule.CheckCall(c); err != nil {
		return "", err
	}
	if c.Action == schedule.ActionAdd {
		if row, err := s.store.ScheduleByKey(ctx, call.key()); err == nil {
			return s.addedText(row), nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return "", errors.New("only a supervisor keeps schedules: this session's profile is not one")
	}
	switch c.Action {
	case schedule.ActionAdd:
		text, err := schedule.CheckText(c.Message)
		if err != nil {
			return "", err
		}
		spec, err := schedule.Parse(c.When)
		if err != nil {
			return "", err
		}
		first, err := spec.First(s.now(), s.zone)
		if err != nil {
			return "", err
		}
		row, _, err := s.store.AddSchedule(ctx, store.ScheduleRow{Key: call.key(), SessionID: call.Session, Spec: strings.Join(strings.Fields(c.When), " "),
			Zone: s.zone.String(), Text: text, FirstAt: first}, schedule.MaxPerSession)
		if errors.Is(err, store.ErrConflict) {
			return "", errors.New(strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": "))
		} else if err != nil {
			return "", err
		}
		s.scheduleSoon()
		return s.addedText(row), nil
	case schedule.ActionCancel:
		id, err := schedule.ParseID(c.ID)
		if err != nil {
			return "", fmt.Errorf("%w; the owner's schedules change only in their schedules file", err)
		}
		row, err := s.store.CancelSchedule(ctx, id, call.Session)
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("this session has no schedule %s", schedule.ID(id))
		} else if err != nil {
			return "", err
		}
		s.scheduleSoon()
		return schedule.CancelledText(schedule.ID(row.ID), row.State == store.ScheduleDone), nil
	}
	return s.scheduleLines(ctx, call.Session, c.Page)
}

// addedText is team_schedule's answer to an add, from the schedule as it
// was made.
func (s *Server) addedText(row store.ScheduleRow) string {
	return schedule.AddedText(schedule.ID(row.ID), row.Spec, row.FirstAt, s.scheduleZone(row.Zone))
}

// scheduleLines is team_schedule's list: the session's own schedules that
// still go out, and, for a standing session, the owner's that go to it, as
// GET /v1/schedules has them: the owner's file is read and their records
// brought in step first, under scheduleMu, so the list never pairs a new
// file with an old record, and a pass then wakes at what changed (Codex
// review of #51, round 1).
func (s *Server) scheduleLines(ctx context.Context, session int64, page int) (string, error) {
	profile, err := s.store.StandingProfile(ctx, session)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	s.scheduleMu.Lock()
	file, err := s.syncOwners(ctx, s.now())
	if err != nil {
		s.log.Warn("bring the owner's schedules in step", "err", err)
	}
	rows, err := s.store.Schedules(ctx)
	if err != nil {
		s.scheduleMu.Unlock()
		return "", err
	}
	list := s.publishSchedulesLocked(ctx, file, rows)
	s.scheduleMu.Unlock()
	s.scheduleSoon()
	var lines []schedule.Line
	for _, item := range list.Schedules {
		mine := item.Source == proto.ScheduleSession && item.Session == session
		owners := item.Source == proto.ScheduleOwner && profile != "" && item.Profile == profile
		if !mine && !owners {
			continue
		}
		zone := s.scheduleZone(item.Zone)
		line := schedule.Line{ID: item.ID, Owners: owners, When: "once", Zone: zone, Skipped: item.Skipped, Text: item.Message, Problem: item.Problem}
		if item.Gather != nil {
			line.Text = schedule.GatherText(item.Gather.Skill, item.Gather.Agents, item.Gather.Note)
		}
		if item.Last != nil && item.Last.Outcome == proto.ScheduleSkipped {
			line.LastSkip = item.Last.Why
		}
		if item.Repeat != "" {
			line.When = item.Repeat
		}
		if item.Next != nil {
			line.Next = *item.Next
		}
		lines = append(lines, line)
	}
	text, err := schedule.ListText(lines, page)
	if err != nil {
		return "", err
	}
	if profile != "" && file.problem != "" {
		text = "None of the owner's schedules goes out: " + file.problem + "\n" + text
	}
	return text, nil
}

// readOwners reads the owner's schedules file as a pass would, and changes
// nothing: the entries with no problem of their own go out.
func (s *Server) readOwners() ownerFile {
	if s.profiles == nil {
		return ownerFile{problem: "pocketd runs without profiles"}
	}
	read, err := s.profiles.Schedules()
	if err != nil {
		return ownerFile{problem: err.Error()}
	}
	file := ownerFile{entries: read.Entries, fires: map[string]bool{}}
	for _, e := range read.Entries {
		file.fires[e.ID] = e.Problem == ""
	}
	return file
}
