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
	// CreateSession is idempotent by key: the same key returns the same id.
	CreateSession(ctx context.Context, key, cwd string, model proto.ModelRef, thinking string) (int64, error)
	Watch(ctx context.Context, id int64) error
	Send(ctx context.Context, id int64, key, text string) (int64, error)
	Abort(ctx context.Context, id int64) error
	Transcript(ctx context.Context, id int64) ([]proto.Entry, proto.Live, error)
	Usage(ctx context.Context, id int64) (proto.Usage, error)
	// RunIdentity names the session's current run by its first entry.
	RunIdentity(ctx context.Context, id int64) (first int64, busy bool, err error)
	WaitIdle(ctx context.Context, id int64) error
	TaskEnded(ctx context.Context, taskID int64) (bool, error)
}

// abortSettle bounds how long an abort waits for its run to settle before
// it retires the run's approvals.
const abortSettle = 15 * time.Second

// writeTimeout bounds one event write, so a client that stops reading
// cannot hold a stream past revocation or shutdown.
const writeTimeout = 10 * time.Second

// thinkingLevels are the accepted values of CreateSession.Thinking.
var thinkingLevels = map[string]bool{"": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}

// maxBody bounds every request body.
const maxBody = 1 << 20

// defaultStreamCheck is how often an event stream sends a keepalive comment
// and checks that its device is still signed in.
const defaultStreamCheck = 10 * time.Second

// Server is the HTTP API. Create it with New, finish the wiring with
// SetApprovals and SetEngine before serving, and serve Handler.
type Server struct {
	store     *store.Store
	hub       *events.Hub
	approvals *approvals.Manager
	version   string
	now       func() time.Time
	log       *slog.Logger
	engine    Engine

	createMu sync.Mutex
	liveMu   sync.Mutex
	live     map[int64]proto.Live
	states   map[int64]proto.SessionState
	stopOnce sync.Once
	stop     chan struct{}
	// streamCheck is defaultStreamCheck; tests shorten it.
	streamCheck time.Duration
}

// New returns a server.
func New(s *store.Store, hub *events.Hub, version string, now func() time.Time, log *slog.Logger) *Server {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{store: s, hub: hub, version: version, now: now, log: log,
		live: map[int64]proto.Live{}, states: map[int64]proto.SessionState{}, stop: make(chan struct{}), streamCheck: defaultStreamCheck}
}

// SetApprovals and SetEngine finish the wiring; both happen before serving
// and before the engine resumes tasks.
func (s *Server) SetApprovals(m *approvals.Manager) { s.approvals = m }
func (s *Server) SetEngine(e Engine)                { s.engine = e }

// Stop ends every open event stream; call it before shutting the HTTP
// server down, so the shutdown does not wait on streams.
func (s *Server) Stop() { s.stopOnce.Do(func() { close(s.stop) }) }

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

// state is a session's state (rule 11): waiting while an approval of it is
// pending, running while its run is busy, idle otherwise.
func (s *Server) state(waiting bool, id int64) proto.SessionState {
	if waiting {
		return proto.SessionWaiting
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
	waiting, err := s.store.HasPendingApproval(ctx, id)
	if err != nil {
		s.log.Warn("pending approvals", "session", id, "err", err)
	}
	state := s.state(waiting, id)
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
	waiting, err := s.store.HasPendingApproval(ctx, id)
	if err != nil {
		return proto.Session{}, err
	}
	return s.wireSession(ctx, row, waiting), nil
}

func (s *Server) wireSession(ctx context.Context, row store.SessionRow, waiting bool) proto.Session {
	usage, err := s.engine.Usage(ctx, row.ID)
	if err != nil {
		s.log.Warn("session usage", "session", row.ID, "err", err)
	}
	return proto.Session{
		ID: row.ID, Title: row.Title, Cwd: row.Cwd, Model: proto.ModelRef{Provider: row.Provider, ID: row.Model},
		Thinking: row.Thinking, State: s.state(waiting, row.ID), Usage: usage, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// WatchAll starts watching every stored session; the service calls it once
// at start, before the engine resumes tasks.
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
	// Paths without a method, to tell a wrong method from an unknown path.
	paths := http.NewServeMux()
	for _, path := range []string{proto.PathHello, proto.PathModels, proto.PathSessions, proto.PathSession,
		proto.PathMessages, proto.PathAbort, proto.PathApprovals, proto.PathApproval, proto.PathEvents} {
		paths.HandleFunc(path, func(http.ResponseWriter, *http.Request) {})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := paths.Handler(r); pattern != "" {
			writeError(w, http.StatusMethodNotAllowed, proto.ErrorCodeBadRequest, r.Method+" is not allowed on "+r.URL.Path)
			return
		}
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no route "+r.Method+" "+r.URL.Path)
	})
	return s.guard(mux)
}

// guard checks the protocol version, then the device token (rule 1).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(proto.HeaderProtocol) != strconv.Itoa(proto.Version) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeProtocol,
				fmt.Sprintf("this service speaks protocol %d; send it in the %s header", proto.Version, proto.HeaderProtocol))
			return
		}
		device, err := auth.Authenticate(r.Context(), s.store, r.Header.Get("Authorization"))
		if errors.Is(err, auth.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, proto.ErrorCodeUnauthorized, "a valid device token is required")
			return
		} else if err != nil {
			s.internal(w, "authenticate", err)
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

// decode reads a strict JSON body (rule 3): one object, no unknown fields,
// nothing after it, at most maxBody bytes.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: "+err.Error())
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: data after the JSON object")
		return false
	}
	return true
}

// requestKey scopes a request id to the device that sent it (rule 4).
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

// reused answers a request id reused for another request.
func reused(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, proto.ErrorCodeConflict, store.ErrRequestReused.Error())
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
	waiting, err := s.store.PendingSessions(r.Context())
	if err != nil {
		s.internal(w, "list approvals", err)
		return
	}
	out := make([]proto.Session, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.wireSession(r.Context(), row, waiting[row.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

// validCreate checks a new CreateSession against rules 6 to 9 and returns
// the cleaned working directory.
func (s *Server) validCreate(w http.ResponseWriter, r *http.Request, request proto.CreateSession) (string, bool) {
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
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	key := requestKey(r, request.RequestID)
	s.createMu.Lock()
	defer s.createMu.Unlock()
	// A retry replays the first outcome before the current prerequisites are
	// checked again (rule 5): the directory or model may be gone by now.
	result, existed, err := s.store.LookupRequest(r.Context(), key, "createSession")
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case existed && result != "":
		id, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			s.internal(w, "stored request", err)
			return
		}
		// The first call may have ended before its watch started.
		if err := s.engine.Watch(r.Context(), id); err != nil {
			s.internal(w, "watch session", err)
			return
		}
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	cwd, ok := s.validCreate(w, r, request)
	if !ok {
		return
	}
	// Finish what starts here even if the client goes away. The engine's
	// create is idempotent by key and the work database records the session
	// with its request in one transaction, so a retry after any failure
	// finds the same conversation instead of creating another.
	ctx := context.WithoutCancel(r.Context())
	id, err := s.engine.CreateSession(ctx, key, cwd, request.Model, request.Thinking)
	if err != nil {
		s.internal(w, "create session", err)
		return
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = "Session " + strconv.FormatInt(id, 10)
	}
	if _, err := s.store.CreateSession(ctx, key, store.SessionRow{
		ID: id, Title: title, Cwd: cwd, Provider: request.Model.Provider, Model: request.Model.ID,
		Thinking: request.Thinking, CreatedBy: deviceOf(r).ID,
	}); err != nil {
		s.internal(w, "record session", err)
		return
	}
	session, err := s.session(ctx, id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	// One creation event, before the watch reports the first live state.
	s.liveMu.Lock()
	s.states[id] = session.State
	s.liveMu.Unlock()
	s.hub.Publish(proto.Event{Type: proto.EventSession, Session: &session})
	if err := s.engine.Watch(ctx, id); err != nil {
		s.internal(w, "watch session", err)
		return
	}
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
	key := requestKey(r, request.RequestID)
	kind := "send:" + strconv.FormatInt(id, 10)
	// The submission itself is idempotent by key in the Durable store. The
	// record, written once the submission is admitted, refuses the key for
	// any other request; a failed send leaves none.
	if _, _, err := s.store.LookupRequest(r.Context(), key, kind); errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "read request", err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	submission, err := s.engine.Send(ctx, id, key, request.Text)
	if err != nil {
		s.internal(w, "send message", err)
		return
	}
	if err := s.store.RecordRequest(ctx, key, kind, strconv.FormatInt(submission, 10)); err != nil {
		s.internal(w, "record request", err)
		return
	}
	writeJSON(w, http.StatusOK, proto.SentMessage{SubmissionID: submission})
}

// abortDone is the result of a settled abort.
const abortDone = "done"

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.Abort
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) || !s.knownSession(w, r, id) {
		return
	}
	key := requestKey(r, request.RequestID)
	kind := "abort:" + strconv.FormatInt(id, 10)
	result, existed, err := s.store.LookupRequest(r.Context(), key, kind)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case result == abortDone:
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// The record names the run this abort targets, by its first entry,
	// before anything acts: a retry, or a retry after a restart, finishes
	// this abort and never stops a run started since.
	var target int64
	if existed {
		target, _ = strconv.ParseInt(strings.TrimPrefix(result, "run:"), 10, 64)
	} else {
		first, busy, err := s.engine.RunIdentity(ctx, id)
		if err != nil {
			s.internal(w, "read the run", err)
			return
		}
		if busy {
			target = first
		}
		if _, _, err := s.store.ClaimRequest(ctx, key, kind); err != nil {
			s.internal(w, "record request", err)
			return
		}
		if err := s.store.CompleteRequest(ctx, key, "run:"+strconv.FormatInt(target, 10)); err != nil {
			s.internal(w, "record request", err)
			return
		}
	}
	if current, busy, err := s.engine.RunIdentity(ctx, id); err != nil {
		s.internal(w, "read the run", err)
		return
	} else if busy && current == target {
		if err := s.engine.Abort(ctx, id); err != nil {
			if !existed {
				_ = s.store.DeleteRequest(ctx, key) // a failed abort leaves no record
			}
			s.internal(w, "abort", err)
			return
		}
	}
	settle, cancel := context.WithTimeout(ctx, abortSettle)
	defer cancel()
	if err := s.engine.WaitIdle(settle, id); err != nil {
		s.internal(w, "abort (the run did not settle; retry with the same requestId)", err)
		return
	}
	// The aborted run's calls will never run: their approvals are retired.
	// Only ended tasks count, so a run started meanwhile keeps its own.
	if err := s.approvals.RetireEnded(ctx, id, s.engine.TaskEnded); err != nil {
		s.internal(w, "retire approvals", err)
		return
	}
	if err := s.store.CompleteRequest(ctx, key, abortDone); err != nil {
		s.internal(w, "record request", err)
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

// decideBody is Decide as it arrives: Allow is a pointer so that a missing
// or null decision is refused instead of read as a denial.
type decideBody struct {
	RequestID string `json:"requestId"`
	Allow     *bool  `json:"allow"`
	Reason    string `json:"reason"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var request decideBody
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if request.Allow == nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "allow must be true or false")
		return
	}
	if len(request.Reason) > 2000 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "reason must be at most 2000 bytes")
		return
	}
	id := r.PathValue("id")
	approval, _, err := s.approvals.Decide(context.WithoutCancel(r.Context()), requestKey(r, request.RequestID), id, *request.Allow, deviceOf(r).Name, request.Reason)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no approval "+id)
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "approval "+id+" is already decided")
	case err != nil:
		s.internal(w, "decide approval", err)
	default:
		writeJSON(w, http.StatusOK, approval)
	}
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
	token := r.Header.Get("Authorization")
	controller := http.NewResponseController(w)
	// write sends one event or comment within writeTimeout, so a client
	// that stops reading cannot hold the stream.
	write := func(send func() error) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(writeTimeout))
		if send() != nil {
			return false
		}
		return controller.Flush() == nil
	}
	backlog, stream, cancel := s.hub.Subscribe(last)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for _, event := range backlog {
		select {
		case <-s.stop:
			return
		case <-r.Context().Done():
			return
		default:
		}
		if !write(func() error { return proto.WriteEvent(w, event) }) {
			return
		}
	}
	flusher.Flush()
	ticker := time.NewTicker(s.streamCheck)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			// A device revoked meanwhile, from any process, loses its stream.
			if _, err := auth.Authenticate(r.Context(), s.store, token); err != nil {
				return
			}
			if !write(func() error { _, err := io.WriteString(w, ": keepalive\n\n"); return err }) {
				return
			}
		case event, open := <-stream:
			if !open {
				return // fell behind: the client reconnects with Last-Event-ID
			}
			if !write(func() error { return proto.WriteEvent(w, event) }) {
				return
			}
		}
	}
}
