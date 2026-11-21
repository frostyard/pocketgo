package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// dirExists says whether path is an existing directory on the service
// host.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// A profile's standing session (ADR-0026, rules 7c and 7d) is the one
// session its agent keeps for good: New Session opens it, pocketd compacts
// it early at its profile's compactAt, it follows its profile when it is
// idle, and Start Over gives it a fresh context in the same session.

// standingKey is the engine's key of profile's standing session: every
// request that opens it finds the one conversation, and the engine records
// it under the key of the request that made it too, so a retry of that
// request finds it whatever the profile says by then (rule 23).
func standingKey(profile string) string { return "standing:" + profile }

// errStandingProfile marks a standing session whose profile cannot be
// applied to it now: gone, invalid, unable to start, or private with a
// home that is not the session's folder.
var errStandingProfile = errors.New("its profile cannot be applied")

// errInternal marks a failure of pocketd, not of the request.
var errInternal = errors.New("internal")

// validStanding checks a CreateSession for a standing profile (rule 7c):
// the profile alone, since its standing session has its own folder, model
// and title. What a new one needs is checked only when there is none to
// open (newStanding).
func validStanding(w http.ResponseWriter, request proto.CreateSession, profile *profiles.Profile) (created, bool) {
	if request.Cwd != "" || request.Model != (proto.ModelRef{}) || request.Thinking != "" || request.Title != "" ||
		request.Repository != "" || request.Worktree {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest,
			"profile "+profile.ID+" keeps a standing session, which has its own folder, model and title: name the profile alone")
		return created{}, false
	}
	return created{spec: SessionSpec{Profile: profile}, profile: profile.ID, standing: true}, true
}

// newStanding is the spec of profile p's new standing session: the profile
// starts (profiles.Startable), its folder exists, its model runs, with a
// compactAt below that model's window, and it has its lessons and its
// agent's name as its title.
func (s *Server) newStanding(w http.ResponseWriter, r *http.Request, p profiles.Profile) (SessionSpec, bool) {
	spec, err := s.newStandingSpec(r.Context(), p)
	switch {
	case errors.Is(err, errInternal):
		s.internal(w, "start the standing session", err)
		return SessionSpec{}, false
	case err != nil:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return SessionSpec{}, false
	}
	return spec, true
}

// newStandingSpec is profile p's new standing session, as newStanding
// checks it: a rule p breaks is an error of its own, and a failure of
// pocketd's is errInternal.
func (s *Server) newStandingSpec(ctx context.Context, p profiles.Profile) (SessionSpec, error) {
	if err := s.checkTools(p); err != nil {
		return SessionSpec{}, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("%w: list models: %w", errInternal, err)
	}
	spec, err := s.standingSpec(p, p.Start(), models, nil)
	if err != nil {
		return SessionSpec{}, err
	}
	if err := profiles.CheckStandingFolder(p); err != nil {
		return SessionSpec{}, err
	}
	rows, err := s.store.ActiveLessons(ctx, p.ID)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("%w: read lessons: %w", errInternal, err)
	}
	spec.Lessons = sectionOf(rows)
	spec.Title = team.StandingTitle(p.Name)
	return spec, nil
}

// standingCreator is the created_by of a standing session pocketd opened
// itself, for a schedule (ADR-0033).
const standingCreator = "schedule"

// openStandingFor opens profile id's standing session when it has none,
// as New Session would (ADR-0033; rule 7c): made under the profile's key,
// standingKey, which is its request's key too, so a try again after a stop
// finds the conversation made, and recorded as the profile's standing
// session in the transaction that records it. It runs under createMu, as
// every session's start does, from the profile as it is then, read last
// (Codex review of #57, round 2): it must still be valid and standing, and
// a supervisor's for a gather (standingWhy). why says which rule it
// breaks, with no session; a profile pocketd cannot read now is errNotNow,
// and another failure of pocketd's errInternal; a rule its start breaks is
// an error of its own.
func (s *Server) openStandingFor(ctx context.Context, id string, gather bool) (session int64, why string, err error) {
	if s.beforeCreateLock != nil {
		s.beforeCreateLock()
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if session, err := s.store.StandingSession(ctx, id); err == nil {
		return session, "", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, "", fmt.Errorf("%w: %w", errInternal, err)
	}
	p, err := s.profiles.Find(id)
	if profiles.Unreadable(err) {
		return 0, "", fmt.Errorf("%w: %v", errNotNow, err)
	} else if err != nil {
		return 0, fmt.Sprintf("no valid profile %s: %v", id, err), nil
	}
	if why := standingWhy(p, gather); why != "" {
		return 0, why, nil
	}
	session, err = s.createStanding(ctx, p)
	return session, "", err
}

// standingWhy is why profile p's standing session takes no schedule, ""
// when it does: p keeps no standing session, or, for a gather, is not a
// supervisor's (only a supervisor keeps the board its gather fills).
func standingWhy(p profiles.Profile, gather bool) string {
	switch {
	case !p.Standing:
		return fmt.Sprintf("profile %s keeps no standing session (session: standing)", p.ID)
	case gather && p.Role != profiles.RoleSupervisor:
		return fmt.Sprintf("profile %s is not a supervisor: only a supervisor gathers proposals", p.ID)
	}
	return ""
}

// createStanding creates and records profile p's standing session, under
// createMu (openStandingFor).
func (s *Server) createStanding(ctx context.Context, p profiles.Profile) (int64, error) {
	key := standingKey(p.ID)
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errInternal, err)
	}
	var spec SessionSpec
	if exists && made.Model != (proto.ModelRef{}) {
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route, Profile: &p}
	} else if spec, err = s.newStandingSpec(ctx, p); err != nil {
		return 0, err
	}
	spec.StandingKey = key
	id, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errInternal, err)
	}
	if err := s.recordSession(ctx, key, id, spec, p.ID, standingCreator, true); err != nil {
		return 0, fmt.Errorf("%w: %w", errInternal, err)
	}
	return id, nil
}

// standingSpec is what profile p's standing session in cwd runs now: the
// router's model for the profile at its default thinking level, or, while
// the owner has a pick for the session (ADR-0028), the pick's model at its
// level; with a compactAt below that model's window, and a supervisor's
// roster. The lessons section is not in it. A pick whose model pocketd no
// longer runs cannot be applied, as a profile's: the owner picks again, or
// starts over. The pick's model and level are checked together, as a
// change of them is (router.Change): a level the model no longer takes
// gives way to the model's default, as a new model's does (Codex review of
// #48, round 1).
func (s *Server) standingSpec(p profiles.Profile, cwd string, models []proto.Model, pick *ModelChoice) (SessionSpec, error) {
	var choice router.Choice
	var thinking string
	var err error
	if pick != nil {
		choice, thinking, err = router.Change(pick.Model, pick.Thinking, nil, "", models)
	} else {
		choice, err = router.Choose(router.Request{Profile: &p}, models)
	}
	if err != nil {
		return SessionSpec{}, err
	}
	if err := profiles.CheckCompactAt(p, router.Window(models, choice.Model)); err != nil {
		return SessionSpec{}, err
	}
	spec := SessionSpec{Cwd: cwd, Model: choice.Model, Thinking: thinking, Profile: &p, Route: string(choice.Rule), StandingKey: standingKey(p.ID)}
	if p.Role == profiles.RoleSupervisor {
		if spec.Roster, err = s.roster(models); err != nil {
			return SessionSpec{}, fmt.Errorf("%w: %w", errInternal, err)
		}
	}
	return spec, nil
}

// openStanding answers a valid CreateSession for a standing profile with
// its standing session (rule 7c): 200 with the one it has, whatever its
// profile's model or folder are now, which first takes what its profile
// says when idle (refreshStanding); else 201 with one made for request
// key, under the profile's key too (standingKey), and recorded with the
// request and as the profile's standing session in one transaction. A
// conversation another request made for the profile, whose record failed,
// is recorded as it was made (rule 23).
func (s *Server) openStanding(w http.ResponseWriter, r *http.Request, key string, valid created) {
	ctx := context.WithoutCancel(r.Context())
	if id, err := s.store.StandingSession(ctx, valid.profile); err == nil {
		if err := s.store.RecordRequest(ctx, key, "createSession", strconv.FormatInt(id, 10)); err != nil {
			s.internal(w, "record request", err)
			return
		}
		if err := s.engine.Watch(ctx, id); err != nil {
			s.internal(w, "watch session", err)
			return
		}
		_ = s.refreshStanding(ctx, id)
		s.writeSession(w, r, id, http.StatusOK)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "read the standing session", err)
		return
	}
	made, exists, err := s.engine.Created(ctx, standingKey(valid.profile))
	if err != nil {
		s.internal(w, "read created session", err)
		return
	}
	var spec SessionSpec
	if exists && made.Model != (proto.ModelRef{}) {
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route}
	} else {
		var ok bool
		if spec, ok = s.newStanding(w, r, *valid.spec.Profile); !ok {
			return
		}
	}
	spec.StandingKey = standingKey(valid.profile)
	id, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		s.internal(w, "create session", err)
		return
	}
	if err := s.recordSession(ctx, key, id, spec, valid.profile, deviceOf(r).ID, true); err != nil {
		s.internal(w, "record session", err)
		return
	}
	s.writeSession(w, r, id, http.StatusCreated)
}

// refreshStanding gives standing session id what its profile says now
// (reconfigureStanding), and logs what failed: the session keeps what it
// had, and the next idle moment tries again.
func (s *Server) refreshStanding(ctx context.Context, id int64) error {
	_, err := s.reconfigureStanding(ctx, id)
	if err != nil && !errors.Is(err, errStandingDeferred) {
		s.log.Warn("refresh a standing session", "session", id, "err", err)
	}
	return err
}

// reconfigureStanding is reconfigureStandingLocked under refreshMu, for a
// standing session alone: any other session never waits on refreshMu,
// which a lessons refresh or a compaction may hold a while (Codex review
// of #43, round 3).
func (s *Server) reconfigureStanding(ctx context.Context, id int64) (compactAt int, err error) {
	if standing, err := s.isStanding(ctx, id); err != nil || !standing {
		return 0, err
	}
	s.lockRefresh()
	defer s.refreshMu.Unlock()
	return s.reconfigureStandingLocked(ctx, id)
}

// isStanding says whether session id is a profile's standing session.
func (s *Server) isStanding(ctx context.Context, id int64) (bool, error) {
	_, err := s.store.StandingProfile(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// lockRefresh takes refreshMu for a standing session (onRefreshLock sees
// it, in tests).
func (s *Server) lockRefresh() {
	if s.onRefreshLock != nil {
		s.onRefreshLock()
	}
	s.refreshMu.Lock()
}

// reconfigureStandingLocked gives standing session id, while it has no
// run, its profile's identity, roster, tools and model as they are now
// (ADR-0026): the engine changes what differs, in one transaction that
// finds no run, and keeps the lessons section, which only a delivery pass
// writes; the work database then records the model. refreshMu is held, so
// the reads that decide it and its writes never cross another refresh, and
// a lessons refresh never writes over one. A profile that cannot be applied
// is errStandingProfile, and changes nothing (standingTarget). A session
// with a run keeps what it has: errStandingDeferred, when its profile
// changed. compactAt is the profile's threshold once the profile is
// applied, 0 otherwise and for a session that is no standing one.
func (s *Server) reconfigureStandingLocked(ctx context.Context, id int64) (compactAt int, err error) {
	t, err := s.standingTarget(ctx, id, true)
	if err != nil || t == nil {
		return 0, err
	}
	switch outcome, err := s.engine.Reconfigure(ctx, id, t.spec); {
	case err != nil:
		return 0, err
	case outcome == ReconfigureDeferred:
		return 0, errStandingDeferred
	}
	if err := s.recordStandingModel(ctx, t); err != nil {
		return 0, err
	}
	return t.profile.CompactAt, nil
}

// standingPass looks after every standing session once, when one is due
// (standingDue): the first delivery pass after start, since a run that
// ended while pocketd stopped, or whose busy state never reached
// PublishLive, left its session's refresh and compaction undone
// (standingRunEnded, whose transactions never compact twice). A list that
// fails to read is due again.
func (s *Server) standingPass(ctx context.Context) error {
	s.standingMu.Lock()
	due := s.standingDue
	s.standingDue = false
	s.standingMu.Unlock()
	if !due {
		return nil
	}
	ids, err := s.store.StandingSessions(ctx)
	if err != nil {
		s.standingMu.Lock()
		s.standingDue = true
		s.standingMu.Unlock()
		return fmt.Errorf("read the standing sessions: %w", err)
	}
	for _, id := range ids {
		s.standingRunEnded(id)
	}
	return nil
}

// standing is what a standing session runs now, as its profile says: the
// profile, the session's row and its spec.
type standing struct {
	profile profiles.Profile
	row     store.SessionRow
	spec    SessionSpec
}

// standingTarget reads what standing session id runs now, refreshMu held:
// nil for a session that is no standing one. withPick takes the owner's
// pick for it, when it has one, in place of its profile's model
// (ADR-0028); Start Over takes the profile's. A profile that cannot be
// applied to it is errStandingProfile: gone, invalid, unable to start,
// private with a home that is not the session's folder, or unreadable
// because pocketd has no profiles.
func (s *Server) standingTarget(ctx context.Context, id int64, withPick bool) (*standing, error) {
	name, err := s.store.StandingProfile(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if s.profiles == nil {
		return nil, fmt.Errorf("%w: pocketd has no profiles, so profile %s cannot be read", errStandingProfile, name)
	}
	p, err := s.profiles.Find(name)
	if profiles.Unreadable(err) {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	} else if err != nil {
		return nil, fmt.Errorf("%w: %w", errStandingProfile, err)
	}
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return nil, err
	}
	if s.afterStandingRead != nil {
		s.afterStandingRead()
	}
	if err := s.checkTools(p); err != nil {
		return nil, fmt.Errorf("%w: %w", errStandingProfile, err)
	}
	// The session keeps its folder: a private agent works only in its
	// home, so a private profile whose home is elsewhere is not applied
	// there (rule 7b).
	if p.Private && row.Cwd != p.Home {
		return nil, fmt.Errorf("%w: profile %s is private and works only in its home, %s, and its standing session works in %s", errStandingProfile, p.ID, p.Home, row.Cwd)
	}
	var pick *ModelChoice
	if withPick {
		found, ok, err := s.engine.StandingPick(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			pick = &found
		}
	}
	spec, err := s.standingSpec(p, row.Cwd, models, pick)
	if errors.Is(err, errInternal) {
		return nil, err
	} else if err != nil {
		return nil, fmt.Errorf("%w: %w", errStandingProfile, err)
	}
	return &standing{profile: p, row: row, spec: spec}, nil
}

// recordStandingModel records the model the session runs now, as PiG has
// it, whatever the engine did this time: a record an earlier refresh or
// Start Over failed to write, after PiG took the model, is written now
// (Codex review of #43, round 5). Its route is the owner's while PiG keeps
// a pick of the model it runs (ADR-0028), else the spec's when PiG runs
// the spec's model, else the one recorded; its level is the one PiG runs,
// so a Start Over tried again after a pick of the profile's own model
// records the pick (Codex review of #48, round 2). It publishes the
// session when they changed.
func (s *Server) recordStandingModel(ctx context.Context, t *standing) error {
	runs, err := s.engine.SessionModel(ctx, t.row.ID)
	if err != nil {
		return err
	}
	pick, picked, err := s.engine.StandingPick(ctx, t.row.ID)
	if err != nil {
		return err
	}
	// The level PiG runs, spelled as want spells it when PiG reads the
	// two the same.
	level := func(want string) string {
		if router.SameLevel(want, runs.Thinking) {
			return want
		}
		return runs.Thinking
	}
	row, route, hint := t.row, t.row.Route, t.row.Thinking
	if runs.Model == t.spec.Model {
		route, hint = t.spec.Route, t.spec.Thinking
	}
	if picked && pick.Model == runs.Model {
		route = string(router.RuleOwner)
		if runs.Model != t.spec.Model {
			hint = pick.Thinking
		}
	}
	thinking := level(hint)
	if row.Provider == runs.Model.Provider && row.Model == runs.Model.ID && row.Route == route && row.Thinking == thinking {
		return nil
	}
	if err := s.store.SetSessionModel(ctx, row.ID, runs.Model.Provider, runs.Model.ID, thinking, route); err != nil {
		return err
	}
	s.publishSession(row.ID, true)
	return nil
}

// errStandingDeferred is a standing session whose profile changed while it
// had a run: the change waits for its next idle moment.
var errStandingDeferred = errors.New("the session has a run: its profile's change waits until it is idle")

// standingRunEnded follows a run of a standing session that ended: the
// session takes what its profile says now, then pocketd compacts it if its
// context reached its profile's compactAt (ADR-0026). Both run under
// refreshMu, so the threshold is the one of the profile just applied, and
// no refresh comes between; a refresh that did not apply leaves PiG's own
// threshold alone. Neither runs for any other session.
func (s *Server) standingRunEnded(id int64) {
	ctx := s.passCtx
	if standing, err := s.isStanding(ctx, id); err != nil || !standing {
		if err != nil {
			s.log.Warn("read the standing session", "session", id, "err", err)
		}
		return
	}
	s.lockRefresh()
	defer s.refreshMu.Unlock()
	compactAt, err := s.reconfigureStandingLocked(ctx, id)
	if err != nil {
		if !errors.Is(err, errStandingDeferred) {
			s.log.Warn("refresh a standing session", "session", id, "err", err)
		}
		return
	}
	if compactAt == 0 {
		return
	}
	if _, err := s.engine.CompactIfOver(ctx, id, compactAt); err != nil {
		s.log.Warn("compact a standing session", "session", id, "err", err)
	}
}

// messageWent says whether session id's message of request key went
// already: its record has its result, or PiG has its submission, admitted
// by a try whose record a stop of pocketd lost. A lookup that fails is an
// error: whether it went is unknown.
func (s *Server) messageWent(ctx context.Context, id int64, key string, existed bool, result string) (bool, error) {
	if !existed {
		return false, nil
	}
	if result != "" {
		return true, nil
	}
	where, err := s.engine.Placed(ctx, id, key)
	return where != PlacementAbsent, err
}

// standingBeforeMessage gives a standing session its profile before the
// owner's message starts a run in it (rule 7c), and refuses the message
// when that failed: a run with the profile's old model or tools would not
// be what the owner set. So does a session at work whose profile changed
// since its run began: the message would queue, and PiG may start the run
// that takes it as the current one ends, before pocketd can apply the
// profile (Codex review of #43, round 4). Another session, and a retry of
// a message that went, pass.
func (s *Server) standingBeforeMessage(ctx context.Context, w http.ResponseWriter, id int64) bool {
	_, err := s.reconfigureStanding(ctx, id)
	switch {
	case errors.Is(err, errStandingDeferred):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict,
			fmt.Sprintf("session %d is at work, and its profile changed since its run began: send once the run ends, when the session takes the profile", id))
		return false
	case errors.Is(err, errStandingProfile):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d is a standing session, and %v", id, err))
		return false
	case err != nil:
		s.internal(w, "refresh the standing session", err)
		return false
	}
	return true
}

// startOver is POST /v1/sessions/{id}/start-over (rule 7d): a standing
// session that has no run takes what its profile says now, then its
// context starts afresh, in the same session, so its threads report there
// still. Its request is claimed, with its target, before the reset, and
// completed once the reset is in: a retry answers as the first did, and
// the request id never resets another session. A claim whose reset is
// known not to have happened is freed.
func (s *Server) startOver(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.StartOver
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind := "startOver:" + strconv.FormatInt(id, 10)
	switch result, existed, err := s.store.LookupRequest(r.Context(), key, kind); {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case existed && result != "":
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	if !s.knownSession(w, r, id) {
		return
	}
	if _, err := s.store.StandingProfile(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("session %d is no standing session: only a standing session starts over", id))
		return
	} else if err != nil {
		s.internal(w, "read the standing session", err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// The session is watched before its reset, so the reset's entry
	// reaches the event stream; a watch that fails consumes nothing.
	if err := s.engine.Watch(ctx, id); err != nil {
		s.internal(w, "watch session", err)
		return
	}
	// This request's own claim, which a failed release left for a delivery
	// pass to free, is this request's again, as a message's is.
	s.releaseMu.Lock()
	_, pending := s.unreleased[key]
	delete(s.unreleased, key)
	s.releaseMu.Unlock()
	if _, _, err := s.store.ClaimRequest(ctx, key, kind); err != nil {
		if pending {
			s.releaseLater(key, kind)
		}
		if errors.Is(err, store.ErrRequestReused) {
			reused(w)
			return
		}
		s.internal(w, "claim request", err)
		return
	}
	if err := s.startOverWithProfile(ctx, id, key); err != nil {
		// The claim is freed only when no reset of it is in: one that is
		// stays the request's, so its id never resets another session.
		freed := s.releaseStartOver(ctx, id, key, kind)
		switch {
		case errors.Is(err, ErrSessionBusy) && freed:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d has a run, or messages wait for one: start over once it is idle", id))
		case errors.Is(err, ErrSessionBusy):
			writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal,
				fmt.Sprintf("session %d has a run, and the requestId could not be freed yet: pocketd frees it later, or start over again with it", id))
		default:
			s.internal(w, "start over", err)
		}
		return
	}
	if err := s.store.CompleteRequest(ctx, key, "started-over"); err != nil {
		s.internal(w, "record request", err)
		return
	}
	s.writeSession(w, r, id, http.StatusOK)
}

// startOverWithProfile resets standing session id for request key, and
// gives it what its profile says now, in the one engine transaction that
// finds it idle with nothing queued: a Start Over refused changes nothing
// (Codex review of #43, round 4). A profile that cannot be applied leaves
// the agent as it is, and the reset goes all the same.
func (s *Server) startOverWithProfile(ctx context.Context, id int64, key string) error {
	s.lockRefresh()
	defer s.refreshMu.Unlock()
	// Start Over takes the profile's model, never the owner's pick, which
	// the engine drops in the reset's commit (ADR-0028): a retry whose
	// reset is in drops none, so a pick made since stays (Codex review of
	// #48, round 1).
	t, err := s.standingTarget(ctx, id, false)
	if err != nil {
		s.log.Warn("refresh a standing session that starts over", "session", id, "err", err)
		t = nil
	}
	var spec *SessionSpec
	if t != nil {
		spec = &t.spec
	}
	if _, err := s.engine.StartOver(ctx, id, key, spec); err != nil {
		return err
	}
	// The model PiG runs is recorded before the request completes: a
	// retry, whose reset PiG has already, records it all the same.
	if t != nil {
		return s.recordStandingModel(ctx, t)
	}
	return nil
}

// releaseStartOver frees the claim of a Start Over of session id that
// failed, once PiG says no reset of its key is in, and says whether it did.
// One it cannot free now, or cannot tell, a delivery pass frees later
// (releasePending), never one whose reset is in.
func (s *Server) releaseStartOver(ctx context.Context, id int64, key, kind string) bool {
	where, err := s.engine.Placed(ctx, id, key)
	switch {
	case err != nil:
		s.log.Warn("look up a Start Over that failed", "err", err)
		s.releaseLater(key, kind)
		return false
	case where != PlacementAbsent:
		return false
	}
	if err := s.releaseRequest(ctx, key, kind); err != nil {
		s.log.Warn("free a request id", "kind", kind, "err", err)
		s.releaseLater(key, kind)
		return false
	}
	return true
}

// releaseLater has a delivery pass free a claim of key (releasePending).
func (s *Server) releaseLater(key, kind string) {
	s.releaseMu.Lock()
	s.unreleased[key] = kind
	s.releaseMu.Unlock()
	s.deliverLater()
}
