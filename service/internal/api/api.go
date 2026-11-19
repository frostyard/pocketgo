// Package api serves protocol version 1 (docs/specs/protocol.md) over HTTP:
// REST for commands and Server-Sent Events for live updates.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// Engine runs sessions; service/pig implements it.
type Engine interface {
	Models(ctx context.Context) ([]proto.Model, error)
	CreateSession(ctx context.Context, cwd string, model proto.ModelRef, thinking string) (int64, error)
	Watch(ctx context.Context, id int64) error
	Send(ctx context.Context, id int64, key, text string) (int64, error)
	Abort(ctx context.Context, id int64) error
	Transcript(ctx context.Context, id int64) ([]proto.Entry, proto.Live, error)
	Usage(ctx context.Context, id int64) (proto.Usage, error)
}

// thinkingLevels are the accepted values of CreateSession.Thinking.
var thinkingLevels = map[string]bool{"": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}

// maxBody bounds every request body.
const maxBody = 1 << 20

// keepalive is how often an idle event stream gets a comment line.
const keepalive = 25 * time.Second

// Server is the HTTP API. Create it with New, set its engine with SetEngine,
// and serve Handler.
type Server struct {
	store     *store.Store
	hub       *events.Hub
	approvals *approvals.Manager
	version   string
	now       func() time.Time
	log       *slog.Logger

	engine   Engine
	createMu sync.Mutex
	liveMu   sync.Mutex
	live     map[int64]proto.Live
	states   map[int64]proto.SessionState
}

// New returns a server. Its approvals manager publishes through the server,
// so wire the manager's publish to Server.PublishApproval.
func New(s *store.Store, hub *events.Hub, version string, now func() time.Time, log *slog.Logger) *Server {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{store: s, hub: hub, version: version, now: now, log: log, live: map[int64]proto.Live{}, states: map[int64]proto.SessionState{}}
}

// SetApprovals and SetEngine finish the wiring; both happen before serving.
func (s *Server) SetApprovals(m *approvals.Manager) { s.approvals = m }
func (s *Server) SetEngine(e Engine)                { s.engine = e }

// PublishEntry, PublishLive and PublishApproval are the engine's and the
// approvals manager's callbacks. They never block on a client.
func (s *Server) PublishEntry(entry proto.Entry) {
	s.hub.Publish(proto.Event{Type: proto.EventEntry, Entry: &entry})
	if err := s.store.TouchSession(context.Background(), entry.SessionID); err != nil {
		s.log.Warn("touch session", "session", entry.SessionID, "err", err)
	}
}

func (s *Server) PublishLive(live proto.Live) {
	s.liveMu.Lock()
	s.live[live.SessionID] = live
	s.liveMu.Unlock()
	s.hub.Publish(proto.Event{Type: proto.EventLive, Live: &live})
	s.publishStateChange(live.SessionID)
}

func (s *Server) PublishApproval(approval proto.Approval) {
	s.hub.Publish(proto.Event{Type: proto.EventApproval, Approval: &approval})
	s.publishStateChange(approval.SessionID)
}

// state is a session's state: waiting while an approval of it is pending,
// running while its run is busy, idle otherwise.
func (s *Server) state(ctx context.Context, id int64) proto.SessionState {
	if pending, err := s.store.PendingApprovals(ctx); err == nil {
		for _, a := range pending {
			if a.SessionID == id {
				return proto.SessionWaiting
			}
		}
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live[id].Busy {
		return proto.SessionRunning
	}
	return proto.SessionIdle
}

func (s *Server) publishStateChange(id int64) {
	ctx := context.Background()
	state := s.state(ctx, id)
	s.liveMu.Lock()
	changed := s.states[id] != state
	s.states[id] = state
	s.liveMu.Unlock()
	if !changed {
		return
	}
	session, err := s.session(ctx, id)
	if err != nil {
		return
	}
	s.hub.Publish(proto.Event{Type: proto.EventSession, Session: &session})
}

func (s *Server) session(ctx context.Context, id int64) (proto.Session, error) {
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return proto.Session{}, err
	}
	usage, err := s.engine.Usage(ctx, id)
	if err != nil {
		s.log.Warn("session usage", "session", id, "err", err)
	}
	return proto.Session{
		ID: row.ID, Title: row.Title, Cwd: row.Cwd, Model: proto.ModelRef{Provider: row.Provider, ID: row.Model},
		Thinking: row.Thinking, State: s.state(ctx, id), Usage: usage, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// WatchAll starts watching every stored session; the service calls it once
// at start.
func (s *Server) WatchAll(ctx context.Context) error {
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.engine.Watch(ctx, row.ID); err != nil {
			return fmt.Errorf("watch session %d: %w", row.ID, err)
		}
	}
	return nil
}

type deviceKey struct{}

func deviceOf(r *http.Request) store.Device { return r.Context().Value(deviceKey{}).(store.Device) }

// Handler serves every route of protocol version 1.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathHello, s.hello)
	mux.HandleFunc("GET "+proto.PathModels, s.models)
	mux.HandleFunc("GET "+proto.PathSessions, s.sessions)
	mux.HandleFunc("POST "+proto.PathSessions, s.createSession)
	mux.HandleFunc("GET "+proto.PathSession, s.getSession)
	mux.HandleFunc("POST "+proto.PathMessages, s.send)
	mux.HandleFunc("POST "+proto.PathAbort, s.abort)
	mux.HandleFunc("GET "+proto.PathApprovals, s.pending)
	mux.HandleFunc("POST "+proto.PathApproval, s.decide)
	mux.HandleFunc("GET "+proto.PathEvents, s.events)
	return s.guard(mux)
}

// guard checks the protocol version, then the device token.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(proto.HeaderProtocol) != strconv.Itoa(proto.Version) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeProtocol,
				fmt.Sprintf("this service speaks protocol %d; send it in the %s header", proto.Version, proto.HeaderProtocol))
			return
		}
		device, err := auth.Authenticate(r.Context(), s.store, r.Header.Get("Authorization"))
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				s.log.Error("authenticate", "err", err)
			}
			writeError(w, http.StatusUnauthorized, proto.ErrorCodeUnauthorized, "a valid device token is required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceKey{}, device)))
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code proto.ErrorCode, message string) {
	writeJSON(w, status, proto.Error{Code: code, Message: message})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, what+" failed")
}

// decode reads a strict JSON body: unknown fields and trailing data fail.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: "+err.Error())
		return false
	}
	if decoder.More() {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: trailing data")
		return false
	}
	return true
}

// requestKey scopes a request id to the device that sent it.
func requestKey(r *http.Request, requestID string) string {
	return "u:" + deviceOf(r).ID + ":" + requestID
}

func validRequestID(w http.ResponseWriter, id string) bool {
	if id == "" || len(id) > proto.MaxRequestIDSize {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest,
			fmt.Sprintf("requestId must be 1 to %d bytes", proto.MaxRequestIDSize))
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the session id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.Hello{Protocol: proto.Version, Server: s.version, Device: deviceOf(r).Name, Now: s.now().UTC()})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	models, err := s.engine.Models(r.Context())
	if err != nil {
		s.internal(w, "list models", err)
		return
	}
	if models == nil {
		models = []proto.Model{}
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Sessions(r.Context())
	if err != nil {
		s.internal(w, "list sessions", err)
		return
	}
	out := make([]proto.Session, 0, len(rows))
	for _, row := range rows {
		session, err := s.session(r.Context(), row.ID)
		if err != nil {
			s.internal(w, "read session", err)
			return
		}
		out = append(out, session)
	}
	writeJSON(w, http.StatusOK, out)
}

// validCreate checks a CreateSession against the rules of the spec and
// returns the cleaned working directory.
func (s *Server) validCreate(w http.ResponseWriter, r *http.Request, request proto.CreateSession) (string, bool) {
	if !validRequestID(w, request.RequestID) {
		return "", false
	}
	if !filepath.IsAbs(request.Cwd) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "cwd must be an absolute path")
		return "", false
	}
	cwd := filepath.Clean(request.Cwd)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "cwd must be an existing directory on the service host")
		return "", false
	}
	if !thinkingLevels[request.Thinking] {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "thinking must be one of minimal, low, medium, high, xhigh, or empty")
		return "", false
	}
	if len(request.Title) > 200 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "title must be at most 200 bytes")
		return "", false
	}
	models, err := s.engine.Models(r.Context())
	if err != nil {
		s.internal(w, "list models", err)
		return "", false
	}
	for _, m := range models {
		if m.ModelRef == request.Model {
			return cwd, true
		}
	}
	writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "model "+request.Model.String()+" is not available; GET "+proto.PathModels+" lists the models")
	return "", false
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var request proto.CreateSession
	if !decode(w, r, &request) {
		return
	}
	cwd, ok := s.validCreate(w, r, request)
	if !ok {
		return
	}
	ctx := r.Context()
	key := requestKey(r, request.RequestID)
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if _, result, ok, err := s.store.Request(ctx, key); err != nil {
		s.internal(w, "read request", err)
		return
	} else if ok {
		var id int64
		if err := json.Unmarshal(result, &id); err != nil {
			s.internal(w, "stored request", err)
			return
		}
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	id, err := s.engine.CreateSession(ctx, cwd, request.Model, request.Thinking)
	if err != nil {
		s.internal(w, "create session", err)
		return
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = "Session " + strconv.FormatInt(id, 10)
	}
	if _, err := s.store.InsertSession(ctx, store.SessionRow{
		ID: id, Title: title, Cwd: cwd, Provider: request.Model.Provider, Model: request.Model.ID,
		Thinking: request.Thinking, CreatedBy: deviceOf(r).ID,
	}); err != nil {
		s.internal(w, "record session", err)
		return
	}
	result, _ := json.Marshal(id)
	if err := s.store.SaveRequest(ctx, key, "createSession", result); err != nil {
		s.internal(w, "record request", err)
		return
	}
	if err := s.engine.Watch(ctx, id); err != nil {
		s.internal(w, "watch session", err)
		return
	}
	session, err := s.session(ctx, id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	s.hub.Publish(proto.Event{Type: proto.EventSession, Session: &session})
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, id int64, status int) {
	session, err := s.session(r.Context(), id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	writeJSON(w, status, session)
}

// knownSession writes 404 for a session the store does not have.
func (s *Server) knownSession(w http.ResponseWriter, r *http.Request, id int64) bool {
	if _, err := s.store.Session(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", id))
		return false
	} else if err != nil {
		s.internal(w, "read session", err)
		return false
	}
	return true
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !s.knownSession(w, r, id) {
		return
	}
	session, err := s.session(r.Context(), id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	entries, live, err := s.engine.Transcript(r.Context(), id)
	if err != nil {
		s.internal(w, "read transcript", err)
		return
	}
	if entries == nil {
		entries = []proto.Entry{}
	}
	writeJSON(w, http.StatusOK, proto.SessionDetail{Session: session, Entries: entries, Live: live})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.SendMessage
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if strings.TrimSpace(request.Text) == "" || len(request.Text) > proto.MaxMessageSize {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("text must be 1 to %d bytes and not only spaces", proto.MaxMessageSize))
		return
	}
	if !s.knownSession(w, r, id) {
		return
	}
	submission, err := s.engine.Send(r.Context(), id, requestKey(r, request.RequestID), request.Text)
	if err != nil {
		s.internal(w, "send message", err)
		return
	}
	writeJSON(w, http.StatusOK, proto.SentMessage{SubmissionID: submission})
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.Abort
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) || !s.knownSession(w, r, id) {
		return
	}
	if err := s.engine.Abort(r.Context(), id); err != nil {
		s.internal(w, "abort", err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) pending(w http.ResponseWriter, r *http.Request) {
	pending, err := s.approvals.Pending(r.Context())
	if err != nil {
		s.internal(w, "list approvals", err)
		return
	}
	if pending == nil {
		pending = []proto.Approval{}
	}
	writeJSON(w, http.StatusOK, pending)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var request proto.Decide
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if len(request.Reason) > 2000 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "reason must be at most 2000 bytes")
		return
	}
	ctx := r.Context()
	key := requestKey(r, request.RequestID)
	if _, result, ok, err := s.store.Request(ctx, key); err != nil {
		s.internal(w, "read request", err)
		return
	} else if ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result)
		return
	}
	approval, err := s.approvals.Decide(ctx, r.PathValue("id"), request.Allow, deviceOf(r).Name, request.Reason)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no approval "+r.PathValue("id"))
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "approval "+r.PathValue("id")+" is already decided")
		return
	case err != nil:
		s.internal(w, "decide approval", err)
		return
	}
	result, _ := json.Marshal(approval)
	if err := s.store.SaveRequest(ctx, key, "decide", append(result, '\n')); err != nil {
		s.internal(w, "record request", err)
		return
	}
	writeJSON(w, http.StatusOK, approval)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, "event stream", errors.New("the response writer cannot flush"))
		return
	}
	var last int64
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		parsed, err := strconv.ParseInt(header, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "Last-Event-ID must be a non-negative integer")
			return
		}
		last = parsed
	}
	backlog, stream, cancel := s.hub.Subscribe(last)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for _, event := range backlog {
		if proto.WriteEvent(w, event) != nil {
			return
		}
	}
	flusher.Flush()
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-stream:
			if !open {
				return // fell behind: the client reconnects with Last-Event-ID
			}
			if proto.WriteEvent(w, event) != nil {
				return
			}
			flusher.Flush()
		}
	}
}
