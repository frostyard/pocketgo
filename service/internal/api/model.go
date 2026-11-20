package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// changeModel serves POST /v1/sessions/{id}/model (rule 7e, ADR-0028): the
// owner's model for a session, its thinking level, or both, as the
// composer changes them. router.Change decides what the session runs; the
// engine applies it, at once while the session is idle, else from its
// run's next round; the work database records it before the answer. A
// standing session keeps it as the owner's pick, which holds in place of
// its profile's model until Start Over; refreshMu is held, so no refresh
// of its profile comes between. A retry answers the session as it is,
// and changes nothing again.
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
	if standing {
		s.lockRefresh()
		defer s.refreshMu.Unlock()
	}
	row, err := s.store.Session(ctx, id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		s.internal(w, "list models", err)
		return
	}
	choice, thinking, err := router.Change(proto.ModelRef{Provider: row.Provider, ID: row.Model}, row.Thinking, request.Model, request.Thinking, models)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	if standing {
		// The pick must leave the profile one pocketd can apply: its
		// compactAt below the model's window (ADR-0026).
		if p, ok, err := s.standingProfile(ctx, id); err != nil {
			s.internal(w, "read the standing profile", err)
			return
		} else if ok {
			if err := profiles.CheckCompactAt(p, router.Window(models, choice.Model)); err != nil {
				writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
				return
			}
		}
	}
	if _, _, err := s.store.ClaimRequest(ctx, key, kind); err != nil {
		if errors.Is(err, store.ErrRequestReused) {
			reused(w)
			return
		}
		s.internal(w, "claim request", err)
		return
	}
	if err := s.engine.SetModel(ctx, id, choice.Model, thinking); err != nil {
		s.internal(w, "change the model", err)
		return
	}
	pick := store.Pick{Provider: choice.Model.Provider, Model: choice.Model.ID, Thinking: thinking}
	if _, err := s.store.ChangeModel(ctx, id, pick, string(choice.Rule)); err != nil {
		s.internal(w, "record the model", err)
		return
	}
	if err := s.store.CompleteRequest(ctx, key, "changed"); err != nil {
		s.internal(w, "record request", err)
		return
	}
	s.publishSession(id, true)
	s.writeSession(w, r, id, http.StatusOK)
}

// modelKind names a change of session id's model as its request does, so
// a requestId reused with another body, or for another session, is
// refused.
func modelKind(id int64, request proto.ChangeModel) string {
	model := "-"
	if request.Model != nil {
		model = request.Model.Provider + "/" + request.Model.ID
	}
	return "model:" + strconv.FormatInt(id, 10) + ":" + model + ":" + request.Thinking
}

// standingProfile is the profile of standing session id, as pocketd reads
// it now; false when it cannot be read, as without profiles: a refresh
// then applies nothing either.
func (s *Server) standingProfile(ctx context.Context, id int64) (profiles.Profile, bool, error) {
	name, err := s.store.StandingProfile(ctx, id)
	if err != nil {
		return profiles.Profile{}, false, err
	}
	if s.profiles == nil {
		return profiles.Profile{}, false, nil
	}
	p, err := s.profiles.Find(name)
	if err != nil {
		return profiles.Profile{}, false, nil
	}
	return p, true, nil
}
