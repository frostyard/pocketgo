package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// modelChanged is the result of a completed change of a session's model.
const modelChanged = "changed"

// changeModel serves POST /v1/sessions/{id}/model (rule 7e, ADR-0028): the
// owner's model for a session, its thinking level, or both, as the
// composer changes them. router.Change decides what the session runs; the
// engine applies it, at once while the session is idle, else from its
// run's next round, in a commit that keeps it under the request's key;
// the work database records it, with the request's completion, before the
// answer. A standing session keeps it as the owner's pick, which holds in
// place of its profile's model until Start Over; refreshMu is held, so no
// refresh of its profile comes between. A retry answers the session as it
// is, and changes nothing again: one whose change PiG has, from a try that
// stopped before its record, records it then (Codex review of #48, round
// 1).
func (s *Server) changeModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.ChangeModel
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if request.Model == nil && request.Thinking == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "name a model, a thinking level, or both")
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind := modelKind(id, request)
	result, existed, err := s.store.LookupRequest(r.Context(), key, kind)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case existed && result != "":
		// The session as it runs now: a later change PiG took, whose
		// record a stop lost, is recorded first (Codex review of #48,
		// round 4); and published again, since a stop after the record
		// may have kept the first try's event from going (round 2).
		if err := s.recordRunning(context.WithoutCancel(r.Context()), id); err != nil {
			s.internal(w, "record the model", err)
			return
		}
		s.publishSession(id, true)
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	// A claim an earlier try left, whose change PiG may not have: a try
	// that stops before it decides leaves it to a delivery pass, which
	// frees it once PiG says it has no change of it (releasePending; Codex
	// review of #48, round 3).
	abandon := func() {
		if existed {
			s.releaseLater(key, kind)
		}
	}
	if !s.knownSession(w, r, id) {
		abandon()
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// A worker's session and a cross-review's take no message from a
	// client (rules 46 and 47), and no model from the owner either: a
	// review's model is of another family than its author's.
	if thread, err := s.store.ThreadOfWorker(ctx, id); err == nil {
		abandon()
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d is thread T%d's worker: its specialist directs it", id, thread.ID))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		abandon()
		s.internal(w, "read the session's thread", err)
		return
	}
	if thread, err := s.store.ThreadOfSession(ctx, id); err == nil && thread.ReviewPull != 0 {
		abandon()
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d is a cross-review pocketd runs: its model is the reviewer's", thread.ID))
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		abandon()
		s.internal(w, "read the session's thread", err)
		return
	}
	standing, err := s.isStanding(ctx, id)
	if err != nil {
		abandon()
		s.internal(w, "read the standing session", err)
		return
	}
	// One change of a session's model at a time, from the read that
	// decides it to its record: two with other request ids, from two
	// devices, never cross, so the work database records the one PiG runs
	// (Codex review of #48, round 1). Only a standing session takes
	// refreshMu too.
	defer s.lockModel(id)()
	if standing {
		s.lockRefresh()
		defer s.refreshMu.Unlock()
	}
	// A claim an earlier try left: PiG may have its change, which then
	// changes nothing again, whatever came since.
	var kept KeptChange
	in := false
	if existed {
		if kept, in, err = s.engine.ModelChange(ctx, id, key); err != nil {
			abandon()
			s.internal(w, "look up the model change", err)
			return
		}
	}
	if !in {
		if kept.Choice, kept.Route, ok = s.applyModel(ctx, w, id, key, kind, request, standing, existed); !ok {
			return
		}
		kept.Latest = true
	}
	if err := s.recordModel(ctx, id, key, kept, standing); err != nil {
		s.internal(w, "record the model", err)
		return
	}
	s.publishSession(id, true)
	s.writeSession(w, r, id, http.StatusOK)
}

// applyModel decides and applies session id's change of request key, kind,
// with its locks held: router.Change checks it against what the session
// runs, as PiG has it, a standing session's profile must take it, and the
// request is claimed before the engine applies it. A claim that changed
// nothing, this try's or an earlier one's (existed), is freed, whatever
// stopped it before PiG's commit (releaseModel); one PiG has a change of
// stays. route is the router's rule the session records for it, decided
// against what PiG ran (router.ChangeRule), which PiG keeps with the
// change.
func (s *Server) applyModel(ctx context.Context, w http.ResponseWriter, id int64, key, kind string, request proto.ChangeModel, standing, existed bool) (change ModelChoice, route string, ok bool) {
	refused := func(status int, code proto.ErrorCode, message string) (ModelChoice, string, bool) {
		if existed {
			s.releaseModel(ctx, id, key, kind)
		}
		writeError(w, status, code, message)
		return ModelChoice{}, "", false
	}
	failed := func(what string, err error) (ModelChoice, string, bool) {
		if existed {
			s.releaseModel(ctx, id, key, kind)
		}
		s.internal(w, what, err)
		return ModelChoice{}, "", false
	}
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return failed("read session", err)
	}
	// What the request leaves as it is, the model or the level, is the
	// one PiG runs: a change PiG took whose record a stop of pocketd lost
	// is not undone by the next (Codex review of #48, round 2). The
	// record's spelling of the level stays when PiG reads it the same.
	runs, err := s.engine.SessionModel(ctx, id)
	if err != nil {
		return failed("read the session's model", err)
	}
	base := ModelChoice{Model: proto.ModelRef{Provider: row.Provider, ID: row.Model}, Thinking: row.Thinking}
	if runs.Model != (proto.ModelRef{}) {
		base.Model = runs.Model
		if !router.SameLevel(base.Thinking, runs.Thinking) {
			base.Thinking = runs.Thinking
		}
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return failed("list models", err)
	}
	choice, thinking, err := router.Change(base.Model, base.Thinking, request.Model, request.Thinking, models)
	if err != nil {
		return refused(http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
	}
	if standing {
		// The pick must leave the profile one pocketd can apply: its
		// compactAt below the model's window (ADR-0026). A profile that
		// cannot be read now is not checked, so nothing is applied (Codex
		// review of #48, round 1); nor is one pocketd runs without
		// profiles for, which takes no message either (round 2).
		if p, found, err := s.standingProfile(ctx, id); errors.Is(err, errStandingProfile) {
			return refused(http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d is a standing session, and %v", id, err))
		} else if err != nil {
			return failed("read the standing profile", err)
		} else if found {
			if err := profiles.CheckCompactAt(p, router.Window(models, choice.Model)); err != nil {
				return refused(http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
			}
		}
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
			return ModelChoice{}, "", false
		}
		s.internal(w, "claim request", err)
		return ModelChoice{}, "", false
	}
	change = ModelChoice{Model: choice.Model, Thinking: thinking}
	route = router.ChangeRule(proto.ModelRef{Provider: row.Provider, ID: row.Model}, base.Model, change.Model, row.Route, standing)
	if err := s.engine.SetModel(ctx, id, key, change, route, standing); err != nil {
		// Freed when PiG has no change of it: a failure before the commit,
		// as a conversation that would not open, changed nothing (Codex
		// review of #48, round 1).
		s.releaseModel(ctx, id, key, kind)
		s.internal(w, "change the model", err)
		return ModelChoice{}, "", false
	}
	return change, route, true
}

// recordModel records session id's change of request key, kept, as PiG
// runs it now, with the request's completion in one transaction
// (store.ChangeModel). A change a later one replaced, or a Start Over
// dropped, between a try and its retry, is not what the session runs
// (Codex review of #48, round 1), even when a later change came back to
// its model and level (round 4): the session records what it runs then
// (running; round 3).
func (s *Server) recordModel(ctx context.Context, id int64, key string, kept KeptChange, standing bool) error {
	r, err := s.running(ctx, id, standing)
	if err != nil {
		return err
	}
	change := kept.Choice
	if kept.Latest && r.runs.Model == change.Model && router.SameLevel(r.runs.Thinking, change.Thinking) &&
		(!standing || r.picked && r.pick == change) {
		return s.store.ChangeModel(ctx, id, key, modelChanged, store.Pick{Provider: change.Model.Provider, Model: change.Model.ID,
			Thinking: change.Thinking}, kept.Route)
	}
	return s.store.ChangeModel(ctx, id, key, modelChanged, r.record, r.route)
}

// runningModel is what session id runs, as PiG has it, and the record
// that names it: record and route.
type runningModel struct {
	runs   ModelChoice
	pick   ModelChoice
	picked bool
	// record is runs, at the record's spelling of the level when PiG reads
	// them the same; route the owner's once its model is not the recorded
	// one, for a session that is no standing one, or while a standing
	// session's pick holds it, else the recorded one; same says the record
	// names it so already.
	record store.Pick
	route  string
	same   bool
}

// running reads what session id runs, and the record that names it
// (runningModel), its model lock held.
func (s *Server) running(ctx context.Context, id int64, standing bool) (runningModel, error) {
	var r runningModel
	var err error
	if r.runs, err = s.engine.SessionModel(ctx, id); err != nil {
		return r, err
	}
	if standing {
		if r.pick, r.picked, err = s.engine.StandingPick(ctx, id); err != nil {
			return r, err
		}
	}
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return r, err
	}
	recorded := proto.ModelRef{Provider: row.Provider, ID: row.Model}
	if r.runs.Model == (proto.ModelRef{}) {
		r.runs.Model = recorded // an agent that names none runs the recorded one
	}
	thinking := r.runs.Thinking
	if router.SameLevel(row.Thinking, thinking) {
		thinking = row.Thinking
	}
	r.record = store.Pick{Provider: r.runs.Model.Provider, Model: r.runs.Model.ID, Thinking: thinking}
	r.route = row.Route
	if standing && r.picked && r.pick.Model == r.runs.Model || !standing && r.runs.Model != recorded {
		r.route = string(router.RuleOwner)
	}
	r.same = r.runs.Model == recorded && thinking == row.Thinking && r.route == row.Route
	return r, nil
}

// recordRunning records what session id runs, as PiG has it, when its
// record names something else (running), under its model lock, and
// refreshMu for a standing session: a change PiG took whose record a stop
// lost (Codex review of #48, round 4).
func (s *Server) recordRunning(ctx context.Context, id int64) error {
	standing, err := s.isStanding(ctx, id)
	if err != nil {
		return err
	}
	defer s.lockModel(id)()
	if standing {
		s.lockRefresh()
		defer s.refreshMu.Unlock()
	}
	r, err := s.running(ctx, id, standing)
	if err != nil || r.same {
		return err
	}
	return s.store.SetSessionModel(ctx, id, r.record.Provider, r.record.Model, r.record.Thinking, r.route)
}

// lockModel holds session id for a change of its model until the returned
// function runs.
func (s *Server) lockModel(id int64) (unlock func()) {
	if s.onModelLock != nil {
		s.onModelLock(id)
	}
	return s.modelLocks.lock(strconv.FormatInt(id, 10))
}

// releaseModel frees the claim of session id's model change of request
// key, kind, once PiG says it has no change of the key: one PiG has took
// effect, and its claim stays, so the request id never changes the model
// again. A lookup or a release that fails leaves it to a delivery pass
// (releasePending).
func (s *Server) releaseModel(ctx context.Context, id int64, key, kind string) {
	if _, in, err := s.engine.ModelChange(ctx, id, key); err != nil {
		s.log.Warn("look up a model change that failed", "err", err)
		s.releaseLater(key, kind)
		return
	} else if in {
		return
	}
	if err := s.releaseRequest(ctx, key, kind); err != nil {
		s.log.Warn("free a request id", "kind", kind, "err", err)
		s.releaseLater(key, kind)
	}
}

// modelKindPrefix begins the kind of every model change's request.
const modelKindPrefix = "model:"

// modelKind names a change of session id's model as its request does, so
// a requestId reused with another body, or for another session, is
// refused. Each field is quoted, so no two bodies name the same kind, as
// a model id with a slash in it would (Codex review of #48, round 1).
func modelKind(id int64, request proto.ChangeModel) string {
	model := "nil"
	if request.Model != nil {
		model = strconv.Quote(request.Model.Provider) + ":" + strconv.Quote(request.Model.ID)
	}
	return modelKindPrefix + strconv.FormatInt(id, 10) + ":" + model + ":" + strconv.Quote(request.Thinking)
}

// modelKindSession is the session of a model change's request kind; false
// for a kind that is none.
func modelKindSession(kind string) (int64, bool) {
	rest, ok := strings.CutPrefix(kind, modelKindPrefix)
	if !ok {
		return 0, false
	}
	session, _, _ := strings.Cut(rest, ":")
	id, err := strconv.ParseInt(session, 10, 64)
	return id, err == nil
}

// standingProfile is the profile of standing session id, as pocketd reads
// it now; false for one gone or invalid: a refresh then applies nothing
// either. A read that failed, and may work later, is an error: the pick is
// not checked against a profile pocketd cannot read (Codex review of #48,
// round 1); so is running without profiles, errStandingProfile (round 2).
func (s *Server) standingProfile(ctx context.Context, id int64) (profiles.Profile, bool, error) {
	name, err := s.store.StandingProfile(ctx, id)
	if err != nil {
		return profiles.Profile{}, false, err
	}
	if s.profiles == nil {
		return profiles.Profile{}, false, fmt.Errorf("%w: pocketd has no profiles, so profile %s cannot be read", errStandingProfile, name)
	}
	p, err := s.profiles.Find(name)
	if errors.Is(err, profiles.ErrFolderChanged) {
		// The folder pocketd protects is not the one there now: none of
		// its profiles is read until pocketd starts again (Codex review of
		// #48, round 3).
		return profiles.Profile{}, false, fmt.Errorf("%w: %w", errStandingProfile, err)
	} else if profiles.Unreadable(err) {
		return profiles.Profile{}, false, fmt.Errorf("profile %s: %w", name, err)
	} else if err != nil {
		return profiles.Profile{}, false, nil
	}
	return p, true, nil
}
