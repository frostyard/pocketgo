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
		// Published again: a stop after the record may have kept the
		// first try's event from going (Codex review of #48, round 2).
		s.publishSession(id, true)
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	if !s.knownSession(w, r, id) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// A worker's session and a cross-review's take no message from a
	// client (rules 46 and 47), and no model from the owner either: a
	// review's model is of another family than its author's.
	if thread, err := s.store.ThreadOfWorker(ctx, id); err == nil {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d is thread T%d's worker: its specialist directs it", id, thread.ID))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "read the session's thread", err)
		return
	}
	if thread, err := s.store.ThreadOfSession(ctx, id); err == nil && thread.ReviewPull != 0 {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d is a cross-review pocketd runs: its model is the reviewer's", thread.ID))
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "read the session's thread", err)
		return
	}
	standing, err := s.isStanding(ctx, id)
	if err != nil {
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
	var change ModelChoice
	in := false
	if existed {
		if change, in, err = s.engine.ModelChange(ctx, id, key); err != nil {
			s.internal(w, "look up the model change", err)
			return
		}
	}
	if !in {
		if change, ok = s.applyModel(ctx, w, id, key, kind, request, standing, existed); !ok {
			return
		}
	}
	if err := s.recordModel(ctx, id, key, change, standing); err != nil {
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
// stays.
func (s *Server) applyModel(ctx context.Context, w http.ResponseWriter, id int64, key, kind string, request proto.ChangeModel, standing, existed bool) (ModelChoice, bool) {
	refused := func(status int, code proto.ErrorCode, message string) (ModelChoice, bool) {
		if existed {
			s.releaseModel(ctx, id, key, kind)
		}
		writeError(w, status, code, message)
		return ModelChoice{}, false
	}
	failed := func(what string, err error) (ModelChoice, bool) {
		if existed {
			s.releaseModel(ctx, id, key, kind)
		}
		s.internal(w, what, err)
		return ModelChoice{}, false
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
		if p, ok, err := s.standingProfile(ctx, id); errors.Is(err, errStandingProfile) {
			return refused(http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d is a standing session, and %v", id, err))
		} else if err != nil {
			return failed("read the standing profile", err)
		} else if ok {
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
			return ModelChoice{}, false
		}
		s.internal(w, "claim request", err)
		return ModelChoice{}, false
	}
	change := ModelChoice{Model: choice.Model, Thinking: thinking}
	if err := s.engine.SetModel(ctx, id, key, change, standing); err != nil {
		// Freed when PiG has no change of it: a failure before the commit,
		// as a conversation that would not open, changed nothing (Codex
		// review of #48, round 1).
		s.releaseModel(ctx, id, key, kind)
		s.internal(w, "change the model", err)
		return ModelChoice{}, false
	}
	return change, true
}

// recordModel records session id's change of request key, change, as PiG
// runs it now, with the request's completion in one transaction
// (store.ChangeModel). A change a later one replaced, or a Start Over
// dropped, between a try and its retry, is not what the session runs, and
// the work database recorded what came after it: the request completes
// alone (Codex review of #48, round 1).
func (s *Server) recordModel(ctx context.Context, id int64, key string, change ModelChoice, standing bool) error {
	runs, err := s.engine.SessionModel(ctx, id)
	if err != nil {
		return err
	}
	current := runs.Model == change.Model && router.SameLevel(runs.Thinking, change.Thinking)
	if current && standing {
		pick, ok, err := s.engine.StandingPick(ctx, id)
		if err != nil {
			return err
		}
		current = ok && pick == change
	}
	if !current {
		return s.store.CompleteRequest(ctx, key, modelChanged)
	}
	pick := store.Pick{Provider: change.Model.Provider, Model: change.Model.ID, Thinking: change.Thinking}
	return s.store.ChangeModel(ctx, id, key, modelChanged, pick, string(router.RuleOwner))
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
	if profiles.Unreadable(err) {
		return profiles.Profile{}, false, fmt.Errorf("profile %s: %w", name, err)
	} else if err != nil {
		return profiles.Profile{}, false, nil
	}
	return p, true, nil
}
