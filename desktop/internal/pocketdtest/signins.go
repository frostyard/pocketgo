package pocketdtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The sign-ins a client makes (ADR-0062, protocol rule 69) in memory: a
// start makes a waiting flow at once, with GitHub's code, and a test ends
// it as GitHub's answer would (EndSignIn).

// UserCode is the code every fake flow shows.
const UserCode = "WDJB-MJHT"

// signInFake is the fake's sign-ins: the rows GET /v1/signins answers, the
// flow each start's request id answered, and the number of the next flow.
type signInFake struct {
	rows   []proto.SignIn
	starts map[string]string
	next   int
}

// DefaultSignIns are the sign-ins of a new fake: GitHub Copilot, which a
// client signs in to, and the others, signed in on the service host.
func DefaultSignIns() []proto.SignIn {
	return []proto.SignIn{
		{Provider: "github-copilot", Name: "GitHub Copilot", State: proto.SignInNone, DeviceFlow: true},
		{Provider: "anthropic", Name: "Anthropic (Claude)", State: proto.SignInSignedIn},
		{Provider: "openai-codex", Name: "OpenAI Codex (ChatGPT)", State: proto.SignInNone},
	}
}

// SetSignIns sets the sign-ins GET /v1/signins answers, without an event.
func (s *Server) SetSignIns(rows []proto.SignIn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signin.rows = slices.Clone(rows)
}

// SignIns are the sign-ins as the fake has them now.
func (s *Server) SignIns() []proto.SignIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.signin.rows)
}

// EndSignIn ends provider's waiting flow in state, as GitHub's answer
// would, and sends its signin event: approved signs the provider in as
// account, and adds models, as the sign-in makes pocketd list them. It
// says whether a flow waited.
func (s *Server) EndSignIn(provider string, state proto.SignInFlowState, account, why string, models ...proto.Model) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.signInLocked(provider)
	if i < 0 || s.signin.rows[i].Flow == nil || s.signin.rows[i].Flow.State.Ended() {
		return false
	}
	row := &s.signin.rows[i]
	flow := *row.Flow
	flow.State, flow.EndedAt, flow.Error = state, Now.Add(time.Minute), why
	if state == proto.FlowApproved {
		flow.Account = account
		row.State, row.Account = proto.SignInSignedIn, account
		s.models = append(s.models, models...)
	}
	row.Flow = &flow
	s.emitLocked(proto.Event{Type: proto.EventSignIn, SignIn: new(*row)})
	return true
}

func (s *Server) signInLocked(provider string) int {
	if s.signin.rows == nil {
		s.signin.rows = DefaultSignIns()
	}
	return slices.IndexFunc(s.signin.rows, func(r proto.SignIn) bool { return r.Provider == provider })
}

// flowLocked is the row and flow of flow id, -1 for none.
func (s *Server) flowLocked(id string) int {
	return slices.IndexFunc(s.signin.rows, func(r proto.SignIn) bool { return r.Flow != nil && r.Flow.ID == id })
}

func (s *Server) signInRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathSignIns, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.signInLocked("")
		writeJSON(w, http.StatusOK, s.signin.rows)
	})
	mux.HandleFunc("POST "+proto.PathSignIn, s.startSignIn)
	mux.HandleFunc("DELETE "+proto.PathSignIn, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		i := s.signInLocked(r.PathValue("id"))
		switch {
		case i < 0:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such provider")
			return
		case !s.signin.rows[i].DeviceFlow:
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "pocketd login "+r.PathValue("id"))
			return
		}
		row := &s.signin.rows[i]
		if row.Flow != nil && !row.Flow.State.Ended() {
			flow := *row.Flow
			flow.State, flow.EndedAt = proto.FlowCanceled, Now
			row.Flow = &flow
		}
		row.State, row.Account = proto.SignInNone, ""
		s.emitLocked(proto.Event{Type: proto.EventSignIn, SignIn: new(*row)})
		writeJSON(w, http.StatusOK, *row)
	})
	mux.HandleFunc("GET "+proto.PathSignInFlow, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		i := s.flowLocked(r.PathValue("id"))
		if i < 0 {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such flow")
			return
		}
		writeJSON(w, http.StatusOK, *s.signin.rows[i].Flow)
	})
	mux.HandleFunc("POST "+proto.PathCancelSignIn, func(w http.ResponseWriter, r *http.Request) {
		var request proto.CancelSignIn
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		i := s.flowLocked(r.PathValue("id"))
		if i < 0 {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such flow")
			return
		}
		row := &s.signin.rows[i]
		if !row.Flow.State.Ended() {
			flow := *row.Flow
			flow.State, flow.EndedAt = proto.FlowCanceled, Now
			row.Flow = &flow
			s.emitLocked(proto.Event{Type: proto.EventSignIn, SignIn: new(*row)})
		}
		writeJSON(w, http.StatusOK, *row.Flow)
	})
}

// startSignIn makes the provider's waiting flow, or answers the one that
// waits; a request id seen before answers its flow as it is now.
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request) {
	var request proto.StartSignIn
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, seen := s.signin.starts[request.RequestID]; seen {
		if i := s.flowLocked(id); i >= 0 {
			writeJSON(w, http.StatusOK, *s.signin.rows[i].Flow)
			return
		}
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "the flow is gone")
		return
	}
	i := s.signInLocked(r.PathValue("id"))
	switch {
	case i < 0:
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such provider")
		return
	case !s.signin.rows[i].DeviceFlow:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "pocketd login "+r.PathValue("id"))
		return
	}
	row := &s.signin.rows[i]
	if s.signin.starts == nil {
		s.signin.starts = map[string]string{}
	}
	if row.Flow != nil && !row.Flow.State.Ended() {
		s.signin.starts[request.RequestID] = row.Flow.ID
		writeJSON(w, http.StatusOK, *row.Flow)
		return
	}
	s.signin.next++
	row.Flow = &proto.SignInFlow{ID: fmt.Sprintf("flow-%d", s.signin.next), Provider: row.Provider, State: proto.FlowWaiting,
		UserCode: UserCode, VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
		StartedAt: Now, ExpiresAt: Now.Add(15 * time.Minute)}
	s.signin.starts[request.RequestID] = row.Flow.ID
	s.emitLocked(proto.Event{Type: proto.EventSignIn, SignIn: new(*row)})
	writeJSON(w, http.StatusCreated, *row.Flow)
}
