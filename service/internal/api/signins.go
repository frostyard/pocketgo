package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/signins"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// SignInStore is the service's one credential store (ADR-0005), as the
// engine keeps it: service/pig implements it. A sign-in a client makes
// (ADR-0062) is written through the store the engine refreshes a
// subscription's tokens through, never through a second writer of the
// sign-ins file.
type SignInStore interface {
	// StoredSignIn is what the store holds for provider, no secret in it.
	StoredSignIn(ctx context.Context, provider string) (signins.Stored, error)
	// KeepSignIn keeps provider's sign-in from token, an approved device
	// flow's GitHub token, and says which credential it kept
	// (signins.Stored.Identity). No error carries token.
	KeepSignIn(ctx context.Context, provider, token string) (identity string, err error)
	// SignOut takes provider's sign-in out of the store.
	SignOut(ctx context.Context, provider string) error
}

// signInService is the sign-ins a client makes (SetSignIns).
type signInService struct {
	creds SignInStore
	flows *signins.Flows
	// pubMu orders the signin events: each provider's sign-in is read and
	// published under it, so the last one carries every change made
	// before it was read. last are the states published, by provider:
	// one whose store read failed goes out with its last state.
	pubMu sync.Mutex
	last  map[string]proto.SignInState
	// record replaces store.RecordRequest in tests.
	record func(ctx context.Context, key, kind, flow string) error
}

// The request kinds of the sign-ins (rule 69): a start, by provider, and
// a cancel.
const (
	startSignInKind  = "signin/"
	cancelSignInKind = "signin-cancel"
)

// signInRead bounds a signin event's read of the store.
const signInRead = 10 * time.Second

// recordSignIn records a start's or a cancel's request with its flow
// (store.RecordRequest); tests make it fail.
func (s *Server) recordSignIn(ctx context.Context, key, kind, flow string) error {
	if s.signIns.record != nil {
		return s.signIns.record(ctx, key, kind, flow)
	}
	return s.store.RecordRequest(ctx, key, kind, flow)
}

// SetSignIns wires the sign-ins a client makes (ADR-0062): creds is the
// engine's credential store, and o the device flows' GitHub and clock,
// whose Keep and Changed the server sets. Call it before serving.
func (s *Server) SetSignIns(creds SignInStore, o signins.Options) {
	svc := &signInService{creds: creds, last: map[string]proto.SignInState{}}
	o.Keep = s.keepSignIn
	o.Changed = s.publishSignIn
	if o.Now == nil {
		o.Now = s.now
	}
	svc.flows = signins.New(o)
	s.signIns = svc
}

// keepSignIn is the flows' Keep: the sign-in goes through the engine's
// credential store, then its account goes beside it, with the identity
// of the credential kept. An account that cannot be kept is logged: the
// sign-in holds without it.
func (s *Server) keepSignIn(ctx context.Context, provider, token, account string) error {
	identity, err := s.signIns.creds.KeepSignIn(ctx, provider, token)
	if err != nil {
		return err
	}
	if err := s.store.KeepSignInAccount(ctx, provider, identity, account); err != nil {
		s.log.Error("keep a sign-in's account", "provider", provider, "err", err)
	}
	return nil
}

// signInRow is provider p's sign-in as GET /v1/signins lists it.
func (s *Server) signInRow(ctx context.Context, p signins.Provider) (proto.SignIn, error) {
	row := proto.SignIn{Provider: p.ID, Name: p.Name, DeviceFlow: p.Device}
	if flow, ok := s.signIns.flows.Latest(p.ID); ok {
		row.Flow = &flow
	}
	stored, err := s.signIns.creds.StoredSignIn(ctx, p.ID)
	if err != nil {
		return row, fmt.Errorf("read the sign-in of %s: %w", p.ID, err)
	}
	identity, account, _, err := s.store.SignInAccount(ctx, p.ID)
	if err != nil {
		return row, fmt.Errorf("read the account of %s: %w", p.ID, err)
	}
	row.State, row.Account = signins.State(stored), signins.Account(stored, identity, account)
	return row, nil
}

// publishSignIn publishes provider's sign-in as it is now: the flows'
// Changed, and a sign-out's.
func (s *Server) publishSignIn(provider string) {
	p, ok := signins.Find(provider)
	if !ok || s.signIns == nil {
		return
	}
	svc := s.signIns
	svc.pubMu.Lock()
	defer svc.pubMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), signInRead)
	defer cancel()
	row, err := s.signInRow(ctx, p)
	if err != nil {
		s.log.Error("read a sign-in to publish", "err", err)
		row.State, row.Account = svc.last[provider], ""
		if row.State == "" {
			row.State = proto.SignInNone
		}
	}
	svc.last[provider] = row.State
	s.emit(proto.Event{Type: proto.EventSignIn, SignIn: &row})
}

// signInsReady answers 500 while the service runs without sign-ins.
func (s *Server) signInsReady(w http.ResponseWriter) bool {
	if s.signIns == nil {
		writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, "the service runs without sign-ins")
		return false
	}
	return true
}

// signInProvider is the path's provider, one pocketd signs in to: 404 for
// another. device asks for one a client signs in to: 400 for another.
func signInProvider(w http.ResponseWriter, r *http.Request, device bool) (signins.Provider, bool) {
	p, ok := signins.Find(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("pocketd signs in to no provider %q", r.PathValue("id")))
		return p, false
	}
	if device && !p.Device {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest,
			fmt.Sprintf("pocketd signs in to %s on the service host alone: pocketd login %s", p.Name, p.ID))
		return p, false
	}
	return p, true
}

// listSignIns is GET /v1/signins: each provider pocketd signs in to, in
// signins.Providers' order, with its state, its account and its newest
// flow; never a token (rule 69).
func (s *Server) listSignIns(w http.ResponseWriter, r *http.Request) {
	if !s.signInsReady(w) {
		return
	}
	out := make([]proto.SignIn, 0, len(signins.Providers))
	for _, p := range signins.Providers {
		row, err := s.signInRow(r.Context(), p)
		if err != nil {
			s.internal(w, "read the sign-ins", err)
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// startSignIn is POST /v1/signins/{id}: provider's device flow
// starts, or its running one answers (rule 69). The flows take the
// request's key with the flow before it polls, and the request is recorded
// with it, so a retry answers that flow as it is and never starts another,
// whether the record was written or the client went (Codex review of #123,
// round 1).
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.signInsReady(w) {
		return
	}
	p, ok := signInProvider(w, r, false)
	if !ok {
		return
	}
	var request proto.StartSignIn
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if _, ok := signInProvider(w, r, true); !ok {
		return
	}
	key, kind := requestKey(r, request.RequestID), startSignInKind+p.ID
	defer s.keys.lock(key)()
	switch result, existed, err := s.store.LookupRequest(r.Context(), key, kind); {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "look the request up", err)
		return
	case existed:
		if flow, ok := s.signIns.flows.Flow(result); ok {
			writeJSON(w, http.StatusOK, flow)
			return
		}
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "the device flow this requestId started is gone: start another with a new requestId")
		return
	}
	flow, started, err := s.signIns.flows.Start(p.ID, key)
	switch {
	case errors.Is(err, signins.ErrStopped):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the service is stopping: sign in once it is back")
		return
	case errors.Is(err, signins.ErrGone):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "the device flow this requestId started is gone: start another with a new requestId")
		return
	case err != nil:
		s.internal(w, "start the device flow", err)
		return
	}
	// The record is written whatever became of the client. One that fails
	// leaves the key with its flow in the flows: a retry in this run
	// answers that flow.
	if err := s.recordSignIn(context.WithoutCancel(r.Context()), key, kind, flow.ID); err != nil {
		s.internal(w, "record the request", err)
		return
	}
	status := http.StatusOK
	if started {
		status = http.StatusCreated
	}
	writeJSON(w, status, flow)
}

// signInFlow is GET /v1/signins/flows/{id}: a provider's newest flow as it
// is now.
func (s *Server) signInFlow(w http.ResponseWriter, r *http.Request) {
	if !s.signInsReady(w) {
		return
	}
	flow, ok := s.signIns.flows.Flow(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no device flow "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

// cancelSignIn is POST /v1/signins/flows/{id}/cancel: a waiting flow ends
// canceled, and the flow answers as it is then (rule 69). The request id
// is recorded with its flow before the cancel, so it never cancels another
// flow, and a cancel whose record failed did nothing (Codex review of
// #123, round 1).
func (s *Server) cancelSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.signInsReady(w) {
		return
	}
	var request proto.CancelSignIn
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	id := r.PathValue("id")
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	switch result, existed, err := s.store.LookupRequest(r.Context(), key, cancelSignInKind); {
	case errors.Is(err, store.ErrRequestReused), existed && result != id:
		reused(w)
		return
	case err != nil:
		s.internal(w, "look the request up", err)
		return
	}
	if _, ok := s.signIns.flows.Flow(id); !ok {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no device flow "+id)
		return
	}
	if err := s.recordSignIn(context.WithoutCancel(r.Context()), key, cancelSignInKind, id); err != nil {
		s.internal(w, "record the request", err)
		return
	}
	flow, ok := s.signIns.flows.Cancel(id)
	if !ok {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no device flow "+id)
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

// signOut is DELETE /v1/signins/{id}: the provider's waiting flow
// is canceled and its sign-in taken out of the engine's credential store,
// with no approved flow's keep between, and its account forgotten (rule
// 69). It answers the provider's sign-in as it is then.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if !s.signInsReady(w) {
		return
	}
	p, ok := signInProvider(w, r, true)
	if !ok {
		return
	}
	err := s.signIns.flows.SignOut(r.Context(), p.ID, func(ctx context.Context) error {
		if err := s.signIns.creds.SignOut(ctx, p.ID); err != nil {
			return err
		}
		if err := s.store.ForgetSignInAccount(ctx, p.ID); err != nil {
			s.log.Error("forget a sign-in's account", "provider", p.ID, "err", err)
		}
		return nil
	})
	if err != nil {
		s.internal(w, "sign out", err)
		return
	}
	s.publishSignIn(p.ID)
	row, err := s.signInRow(r.Context(), p)
	if err != nil {
		s.internal(w, "read the sign-in", err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}
