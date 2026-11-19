// Package pocketdtest is a fake pocketd for the desktop client's tests: the
// endpoints of protocol version 1 (docs/specs/protocol.md) over in-memory
// state, served by httptest on loopback. Tests shape its state, emit events
// to its streams, drop the streams or restart it, and read the requests it
// got. Only tests import it.
package pocketdtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Token is the device token the server takes.
const Token = "pgd_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Now is the server's clock, which never moves.
var Now = time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)

// Request is a request the server got.
type Request struct {
	Method, Path string
	// LastEventID is the stream request's Last-Event-ID header.
	LastEventID string
	Body        []byte
}

// Server is the fake pocketd.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	token     string
	models    []proto.Model
	sessions  map[int64]*proto.SessionDetail
	approvals []proto.Approval
	nextID    int64
	submitted int64
	seq       int64
	kept      []proto.Event
	streams   map[chan proto.Event]struct{}
	requests  []Request
	replies   map[string][]byte // request id → the first answer's body
	failures  map[string]int    // "METHOD path" → status to answer with once
}

// New starts a server and stops it when t ends.
func New(t testing.TB) *Server {
	s := &Server{
		token:    Token,
		sessions: map[int64]*proto.SessionDetail{},
		nextID:   1,
		seq:      Now.UnixMilli() * 1000,
		streams:  map[chan proto.Event]struct{}{},
		replies:  map[string][]byte{},
		failures: map[string]int{},
	}
	s.Server = httptest.NewServer(s.handler())
	t.Cleanup(func() {
		s.DropStreams()
		s.CloseClientConnections()
		s.Close()
	})
	return s
}

// AddModel adds a model GET /v1/models lists.
func (s *Server) AddModel(m proto.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append(s.models, m)
}

// AddSession adds a session with its transcript, without an event.
func (s *Server) AddSession(session proto.Session, entries ...proto.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.State == "" {
		session.State = proto.SessionIdle
	}
	s.sessions[session.ID] = &proto.SessionDetail{Session: session, Entries: entries, Live: proto.Live{SessionID: session.ID}}
	s.nextID = max(s.nextID, session.ID+1)
}

// AddApproval adds an approval, or replaces the one with its id, without
// an event.
func (s *Server) AddApproval(a proto.Approval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.approvals, func(x proto.Approval) bool { return x.ID == a.ID }); i >= 0 {
		s.approvals[i] = a
		return
	}
	s.approvals = append(s.approvals, a)
}

// SetLive sets a session's live state, without an event.
func (s *Server) SetLive(live proto.Live) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.sessions[live.SessionID]; d != nil {
		d.Live = live
	}
}

// Revoke makes the server refuse the token, as a revoked device's.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
}

// FailOnce makes the next request of method on p answer status, with an
// Error body, without doing anything.
func (s *Server) FailOnce(method, p string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method+" "+p] = status
}

// Emit numbers event, applies its payload to the server's state, keeps it
// for Last-Event-ID and sends it to every open stream.
func (s *Server) Emit(event proto.Event) proto.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emitLocked(event)
}

func (s *Server) emitLocked(event proto.Event) proto.Event {
	s.seq++
	event.Seq = s.seq
	switch {
	case event.Session != nil:
		if d := s.sessions[event.Session.ID]; d != nil {
			d.Session = *event.Session
		} else {
			s.sessions[event.Session.ID] = &proto.SessionDetail{Session: *event.Session, Live: proto.Live{SessionID: event.Session.ID}}
		}
	case event.Entry != nil:
		if d := s.sessions[event.Entry.SessionID]; d != nil {
			d.Entries = append(d.Entries, *event.Entry)
		}
	case event.Live != nil:
		if d := s.sessions[event.Live.SessionID]; d != nil {
			d.Live = *event.Live
		}
	case event.Approval != nil:
		i := slices.IndexFunc(s.approvals, func(a proto.Approval) bool { return a.ID == event.Approval.ID })
		if i >= 0 {
			s.approvals[i] = *event.Approval
		} else {
			s.approvals = append(s.approvals, *event.Approval)
		}
	}
	s.kept = append(s.kept, event)
	for ch := range s.streams {
		ch <- event
	}
	return event
}

// Seq is the newest event's seq.
func (s *Server) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Streams is how many event streams are open.
func (s *Server) Streams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// DropStreams ends every open stream, as a network failure would.
func (s *Server) DropStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.streams {
		close(ch)
		delete(s.streams, ch)
	}
}

// Restart ends every stream and forgets the kept events, as pocketd
// restarting does: a client's Last-Event-ID is then answered with resync.
// Seq numbers keep growing (protocol rule 17).
func (s *Server) Restart() {
	s.DropStreams()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kept = nil
	s.seq += 1000
}

// Requests are the requests the server got, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// RequestsTo are the requests of method on p.
func (s *Server) RequestsTo(method, p string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == p {
			out = append(out, r)
		}
	}
	return out
}

// Session returns the server's copy of session id.
func (s *Server) Session(id int64) (proto.SessionDetail, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.sessions[id]
	if d == nil {
		return proto.SessionDetail{}, false
	}
	return *d, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code proto.ErrorCode, message string) {
	writeJSON(w, status, proto.Error{Code: code, Message: message})
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathHello, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, proto.Hello{Protocol: proto.Version, Server: "test", Device: "laptop", Now: Now})
	})
	mux.HandleFunc("GET "+proto.PathModels, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		models := slices.Clone(s.models)
		s.mu.Unlock()
		if models == nil {
			models = []proto.Model{}
		}
		writeJSON(w, http.StatusOK, models)
	})
	mux.HandleFunc("GET "+proto.PathSessions, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := make([]proto.Session, 0, len(s.sessions))
		for _, d := range s.sessions {
			list = append(list, d.Session)
		}
		s.mu.Unlock()
		sort.Slice(list, func(i, j int) bool {
			if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
				return list[i].UpdatedAt.After(list[j].UpdatedAt)
			}
			return list[i].ID > list[j].ID
		})
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathSessions, s.createSession)
	mux.HandleFunc("GET "+proto.PathSession, func(w http.ResponseWriter, r *http.Request) {
		d, ok := s.Session(pathID(r))
		if !ok {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
			return
		}
		if d.Entries == nil {
			d.Entries = []proto.Entry{}
		}
		writeJSON(w, http.StatusOK, d)
	})
	mux.HandleFunc("POST "+proto.PathMessages, s.send)
	mux.HandleFunc("POST "+proto.PathAbort, s.abort)
	mux.HandleFunc("GET "+proto.PathApprovals, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := []proto.Approval{}
		for _, a := range s.approvals {
			if a.State == proto.ApprovalPending {
				list = append(list, a)
			}
		}
		s.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool { return list[i].RequestedAt.Before(list[j].RequestedAt) })
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathApproval, s.decide)
	mux.HandleFunc("GET "+proto.PathEvents, s.events)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, LastEventID: r.Header.Get("Last-Event-ID"), Body: body})
		token := s.token
		failure := s.failures[r.Method+" "+r.URL.Path]
		delete(s.failures, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		if r.Header.Get(proto.HeaderProtocol) != strconv.Itoa(proto.Version) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeProtocol, "send Pocket-Protocol: 1")
			return
		}
		if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
			writeError(w, http.StatusUnauthorized, proto.ErrorCodeUnauthorized, "unknown token")
			return
		}
		if failure != 0 {
			writeError(w, failure, proto.ErrorCodeInternal, "made to fail")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		mux.ServeHTTP(w, r)
	})
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// replay answers a request id seen before with its first answer, and
// reports whether it did.
func (s *Server) replay(w http.ResponseWriter, id string) bool {
	s.mu.Lock()
	body, ok := s.replies[id]
	s.mu.Unlock()
	if ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
	return ok
}

func (s *Server) remember(id string, v any) {
	body, _ := json.Marshal(v)
	s.mu.Lock()
	s.replies[id] = append(body, '\n')
	s.mu.Unlock()
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var request proto.CreateSession
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, request.RequestID) {
		return
	}
	s.mu.Lock()
	known := slices.ContainsFunc(s.models, func(m proto.Model) bool { return m.ModelRef == request.Model })
	s.mu.Unlock()
	if !path.IsAbs(request.Cwd) || !known {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "cwd must be absolute and the model one of /v1/models")
		return
	}
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	title := request.Title
	if title == "" {
		title = fmt.Sprintf("Session %d", id)
	}
	session := proto.Session{ID: id, Title: title, Cwd: path.Clean(request.Cwd), Model: request.Model, Thinking: request.Thinking,
		State: proto.SessionIdle, CreatedAt: Now, UpdatedAt: Now}
	s.emitLocked(proto.Event{Type: proto.EventSession, Session: &session})
	s.mu.Unlock()
	s.remember(request.RequestID, session)
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var request proto.SendMessage
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" || strings.TrimSpace(request.Text) == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, request.RequestID) {
		return
	}
	id := pathID(r)
	s.mu.Lock()
	d := s.sessions[id]
	if d == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	}
	s.submitted++
	answer := proto.SentMessage{SubmissionID: s.submitted}
	entryID := int64(1)
	if n := len(d.Entries); n > 0 {
		entryID = d.Entries[n-1].ID + 1
	}
	s.emitLocked(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: entryID, SessionID: id, Kind: proto.EntryUser, Text: request.Text, At: Now}})
	s.mu.Unlock()
	s.remember(request.RequestID, answer)
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	var request proto.Abort
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	id := pathID(r)
	s.mu.Lock()
	if d := s.sessions[id]; d != nil && d.Live.Busy {
		s.emitLocked(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: id}})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var request proto.Decide
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, request.RequestID) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	i := slices.IndexFunc(s.approvals, func(a proto.Approval) bool { return a.ID == id })
	if i < 0 {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such approval")
		return
	}
	a := s.approvals[i]
	if a.State != proto.ApprovalPending {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "already decided")
		return
	}
	a.State, a.Reason, a.DecidedBy = proto.ApprovalDenied, request.Reason, "laptop"
	if request.Allow {
		a.State = proto.ApprovalAllowed
	}
	decided := Now
	a.DecidedAt = &decided
	s.emitLocked(proto.Event{Type: proto.EventApproval, Approval: &a})
	s.mu.Unlock()
	s.remember(request.RequestID, a)
	writeJSON(w, http.StatusOK, a)
}

// events serves GET /v1/events as the service's hub does: the kept events
// after Last-Event-ID, or one resync when they are gone, then the events
// emitted while the stream is open.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var last int64
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		var err error
		if last, err = strconv.ParseInt(h, 10, 64); err != nil || last < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad Last-Event-ID")
			return
		}
	}
	ch := make(chan proto.Event, 1024)
	s.mu.Lock()
	var backlog []proto.Event
	switch {
	case last == 0 || last == s.seq:
	case last > s.seq || len(s.kept) == 0 || last < s.kept[0].Seq-1:
		backlog = []proto.Event{{Seq: s.seq, Type: proto.EventResync}}
	default:
		for _, ev := range s.kept {
			if ev.Seq > last {
				backlog = append(backlog, ev)
			}
		}
	}
	s.streams[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, ch)
		s.mu.Unlock()
	}()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, ev := range backlog {
		_ = proto.WriteEvent(w, ev)
	}
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if proto.WriteEvent(w, ev) != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
