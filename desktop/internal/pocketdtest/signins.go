package pocketdtest

import (
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

// signInFake is the fake's sign-ins: the rows GET /v1/signins answers and
// the number of the next flow. The flow a start's or a cancel's request id
// named is kept with the other requests' (Server.replies), so a request id
// used for another request is conflict, as pocketd answers (Codex review
// of #123, round 1).
type signInFake struct {
	rows []proto.SignIn
	next int
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
	s.signInLocked("")
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
		// The account's models replace the provider's earlier ones, as
		// pocketd lists them (Codex review of #123, round 2).
		s.models = append(slices.DeleteFunc(s.models, func(m proto.Model) bool { return m.Provider == provider }), models...)
	}
	row.Flow = &flow
	s.emitLocked(proto.Event{Type: proto.EventSignIn, SignIn: new(*row)})
	return true
}

// ForgetSignInFlows forgets every flow, as a restart of pocketd does,
// without an event: a request id that started one is then not_found.
func (s *Server) ForgetSignInFlows() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signInLocked("")
	for i := range s.signin.rows {
		s.signin.rows[i].Flow = nil
	}
}

// replaySignInLocked answers a start's or a cancel's request id seen
// before: conflict for another request, and for the same one the flow it
// named as it is now, not_found once it is gone. It says whether it
// answered.
func (s *Server) replaySignInLocked(w http.ResponseWriter, op, id string) bool {
	prior, ok := s.replies[id]
	switch {
	case !ok:
		return false
	case prior.op != op:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
	default:
		if i := s.flowLocked(string(prior.body)); i >= 0 {
			writeJSON(w, http.StatusOK, *s.signin.rows[i].Flow)
		} else {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "the device flow this requestId started is gone")
		}
	}
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
		// The provider's models go with its sign-in, as pocketd lists
		// them (Codex review of #123, round 1).
		s.models = slices.DeleteFunc(s.models, func(m proto.Model) bool { return m.Provider == row.Provider })
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
		// The body is read as pocketd reads it (Codex review of #123,
		// round 3).
		var request proto.CancelSignIn
		if !decodeStrict(w, r, &request, &request.RequestID) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		op := r.Method + " " + r.URL.Path
		if s.replaySignInLocked(w, op, request.RequestID) {
			return
		}
		i := s.flowLocked(r.PathValue("id"))
		if i < 0 {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such flow")
			return
		}
		s.replies[request.RequestID] = reply{op: op, body: []byte(r.PathValue("id"))}
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
	if !decodeStrict(w, r, &request, &request.RequestID) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op := r.Method + " " + r.URL.Path
	if s.replaySignInLocked(w, op, request.RequestID) {
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
	if row.Flow != nil && !row.Flow.State.Ended() {
		s.replies[request.RequestID] = reply{op: op, body: []byte(row.Flow.ID)}
		writeJSON(w, http.StatusOK, *row.Flow)
		return
	}
	s.signin.next++
	row.Flow = &proto.SignInFlow{ID: fmt.Sprintf("flow-%d", s.signin.next), Provider: row.Provider, State: proto.FlowWaiting,
		UserCode: UserCode, VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
		StartedAt: Now, ExpiresAt: Now.Add(15 * time.Minute)}
	s.replies[request.RequestID] = reply{op: op, body: []byte(row.Flow.ID)}
	s.emitLocked(proto.Event{Type: proto.EventSignIn, SignIn: new(*row)})
	writeJSON(w, http.StatusCreated, *row.Flow)
}
