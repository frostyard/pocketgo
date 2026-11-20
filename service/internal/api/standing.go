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

// A profile's standing session (ADR-0025, rules 7c and 7d) is the one
// session its agent keeps for good: New Session opens it, pocketd compacts
// it early at its profile's compactAt, it follows its profile when it is
// idle, and Start Over gives it a fresh context in the same session.

// standingKey is the engine's request key of profile's standing session:
// every request that opens it, and a retry after any failure, finds the
// one conversation.
func standingKey(profile string) string { return "standing:" + profile }

// validStanding checks a CreateSession for a standing profile (rule 7c):
// the profile alone, since its standing session has its own folder, model
// and title. What a new one needs is checked only when there is none to
// open (newStanding).
func validStanding(w http.ResponseWriter, request proto.CreateSession, profile *profiles.Profile) (created, bool) {
	if request.Cwd != "" || request.Model != (proto.ModelRef{}) || request.Thinking != "" || request.Title != "" {
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
	bad := func(message string) (SessionSpec, bool) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, message)
		return SessionSpec{}, false
	}
	if err := s.checkTools(p); err != nil {
		return bad(err.Error())
	}
	models, err := s.engine.Models(r.Context())
	if err != nil {
		s.internal(w, "list models", err)
		return SessionSpec{}, false
	}
	spec, err := s.standingSpec(p, p.Start(), models)
	if errors.Is(err, errInternal) {
		s.internal(w, "read profiles", err)
		return SessionSpec{}, false
	} else if err != nil {
		return bad(err.Error())
	}
	if !dirExists(spec.Cwd) {
		return bad("profile " + p.ID + "'s folder " + spec.Cwd + " is not an existing directory on the service host")
	}
	rows, err := s.store.ActiveLessons(r.Context(), p.ID)
	if err != nil {
		s.internal(w, "read lessons", err)
		return SessionSpec{}, false
	}
	spec.Lessons = sectionOf(rows)
	spec.Title = team.StandingTitle(p.Name)
	return spec, true
}

// errInternal marks a failure of pocketd, not of the request.
var errInternal = errors.New("internal")

// standingSpec is what profile p's standing session in cwd runs now: the
// router's model for the profile, never the owner's, at its default
// thinking level, and a supervisor's roster. The lessons section is not
// in it.
func (s *Server) standingSpec(p profiles.Profile, cwd string, models []proto.Model) (SessionSpec, error) {
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return SessionSpec{}, err
	}
	if err := profiles.CheckCompactAt(p, router.Window(models, choice.Model)); err != nil {
		return SessionSpec{}, err
	}
	spec := SessionSpec{Cwd: cwd, Model: choice.Model, Profile: &p, Route: string(choice.Rule)}
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
// says when idle (refreshStanding); else 201 with one made under the
// profile's key (standingKey), recorded with the request and as the
// profile's standing session in one transaction. A conversation made for
// the profile whose record failed is recorded as it was made (rule 23).
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
		s.refreshStanding(ctx, id)
		s.writeSession(w, r, id, http.StatusOK)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "read the standing session", err)
		return
	}
	engineKey := standingKey(valid.profile)
	made, exists, err := s.engine.Created(ctx, engineKey)
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
	id, err := s.engine.CreateSession(ctx, engineKey, spec)
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
func (s *Server) refreshStanding(ctx context.Context, id int64) *profiles.Profile {
	p, err := s.reconfigureStanding(ctx, id)
	if err != nil {
		s.log.Warn("refresh a standing session", "session", id, "err", err)
	}
	return p
}

// reconfigureStanding gives standing session id, while it has no run, its
// profile's identity, roster, tools and model as they are now (ADR-0025):
// the engine changes what differs, in one transaction that finds it idle,
// and keeps the lessons section, which only a delivery pass writes; the
// work database then records the model. It runs under refreshMu, so a
// lessons refresh never writes over it. It returns the profile it read,
// nil for a session that is no standing one.
func (s *Server) reconfigureStanding(ctx context.Context, id int64) (*profiles.Profile, error) {
	name, err := s.store.StandingProfile(ctx, id)
	if errors.Is(err, store.ErrNotFound) || s.profiles == nil {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	p, err := s.profiles.Find(name)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	if err := s.checkTools(p); err != nil {
		return &p, err
	}
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return &p, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return &p, err
	}
	spec, err := s.standingSpec(p, row.Cwd, models)
	if err != nil {
		return &p, err
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if _, busy, err := s.engine.Reconfigure(ctx, id, spec); err != nil || busy {
		return &p, err
	}
	if row.Provider != spec.Model.Provider || row.Model != spec.Model.ID || row.Route != spec.Route {
		if err := s.store.SetSessionModel(ctx, id, spec.Model.Provider, spec.Model.ID, spec.Route); err != nil {
			return &p, err
		}
		s.publishSession(id, true)
	}
	return &p, nil
}

// standingRunEnded follows a run of a standing session that ended: the
// session takes what its profile says now, then pocketd compacts it if its
// context reached its profile's compactAt (ADR-0025). Neither runs for
// any other session.
func (s *Server) standingRunEnded(id int64) {
	ctx := s.passCtx
	p := s.refreshStanding(ctx, id)
	if p == nil || p.CompactAt == 0 {
		return
	}
	if _, err := s.engine.CompactIfOver(ctx, id, p.CompactAt); err != nil {
		s.log.Warn("compact a standing session", "session", id, "err", err)
	}
}

// startOver is POST /v1/sessions/{id}/start-over (rule 7d): a standing
// session that has no run takes what its profile says now, then its
// context starts afresh, in the same session, so its threads report there
// still. Its request is recorded once the reset is in, and a retry
// answers as the first did.
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
	s.refreshStanding(ctx, id)
	if err := s.engine.StartOver(ctx, id, key); errors.Is(err, ErrSessionBusy) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d has a run, or messages wait for one: start over once it is idle", id))
		return
	} else if err != nil {
		s.internal(w, "start over", err)
		return
	}
	if err := s.store.RecordRequest(ctx, key, kind, "started-over"); err != nil {
		s.internal(w, "record request", err)
		return
	}
	s.writeSession(w, r, id, http.StatusOK)
}
