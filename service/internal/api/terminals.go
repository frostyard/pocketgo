package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/terminal"
)

// The owner's terminals (ADR-0025, protocol rule 56): login shells of the
// service user on the service host, outside the agents' sandbox, behind
// the device token alone (guard). service/internal/terminal runs them;
// these handlers read the requests and name the folder a shell starts in.

// startTerminalKind is a start's request record (rule 5).
const startTerminalKind = "startTerminal"

// publishTerminals sends the whole list after each change of it.
func (s *Server) publishTerminals(list []proto.Terminal) {
	s.emit(proto.Event{Type: proto.EventTerminals, Terminals: list})
}

func (s *Server) terminalList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.terms.List())
}

// startTerminal starts a shell once per request id: a retry answers the
// shell the first call started, 200 instead of 201.
func (s *Server) startTerminal(w http.ResponseWriter, r *http.Request) {
	var request proto.StartTerminal
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	switch result, existed, err := s.store.LookupRequest(r.Context(), key, startTerminalKind); {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "look the request up", err)
		return
	case existed:
		writeJSON(w, http.StatusOK, proto.TerminalStarted{ID: result})
		return
	}
	cwd, ok := s.terminalFolder(w, r, request)
	if !ok {
		return
	}
	rows, cols, err := terminal.StartSize(request.Rows, request.Cols)
	if err != nil {
		terminalError(w, s, err)
		return
	}
	info, err := s.terms.Start(cwd, rows, cols)
	if err != nil {
		terminalError(w, s, err)
		return
	}
	if s.afterTerminalStart != nil {
		s.afterTerminalStart()
	}
	// The shell runs only with its request recorded, so a retry finds it,
	// whether or not the client still waits: its login ran already.
	if err := s.store.RecordRequest(context.WithoutCancel(r.Context()), key, startTerminalKind, info.ID); err != nil {
		_ = s.terms.End(info.ID)
		s.internal(w, "record the request", err)
		return
	}
	writeJSON(w, http.StatusCreated, proto.TerminalStarted{ID: info.ID})
}

// terminalFolder is where a new shell starts: the service user's home, a
// session's folder, or an absolute path to a folder that exists. Each
// takes only its own field.
func (s *Server) terminalFolder(w http.ResponseWriter, r *http.Request, request proto.StartTerminal) (string, bool) {
	bad := func(format string, args ...any) (string, bool) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf(format, args...))
		return "", false
	}
	switch request.Folder {
	case "", proto.TerminalHome:
		if request.Session != 0 || request.Path != "" {
			return bad("a shell in the home folder takes no session or path")
		}
		return s.terms.Home(), true
	case proto.TerminalSession:
		if request.Path != "" {
			return bad("a shell in a session's folder takes no path")
		}
		row, err := s.store.Session(r.Context(), request.Session)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", request.Session))
			return "", false
		} else if err != nil {
			s.internal(w, "read session", err)
			return "", false
		}
		return row.Cwd, true
	case proto.TerminalPath:
		if request.Session != 0 {
			return bad("a shell in a path takes no session")
		}
		if !filepath.IsAbs(request.Path) {
			return bad("give an absolute path to a folder that exists")
		}
		path := filepath.Clean(request.Path)
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return bad("give an absolute path to a folder that exists")
		}
		return path, true
	}
	return bad(`give folder as "home", "session" or "path"`)
}

func (s *Server) endTerminal(w http.ResponseWriter, r *http.Request) {
	if err := s.terms.End(r.PathValue("id")); err != nil {
		terminalError(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// terminalOutput is the long poll: from, the bytes the client has, and
// ack, those it drew (from when left out), and tab, the client's name.
func (s *Server) terminalOutput(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset := func(name, value string) (int64, bool) {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "give "+name+" as a whole number of bytes")
			return 0, false
		}
		return n, true
	}
	from, ok := offset("from", q.Get("from"))
	if !ok {
		return
	}
	ack := from
	if q.Has("ack") {
		if ack, ok = offset("ack", q.Get("ack")); !ok {
			return
		}
	}
	out, err := s.terms.Poll(r.Context(), r.PathValue("id"), q.Get("tab"), from, ack)
	if err != nil {
		terminalError(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) terminalInput(w http.ResponseWriter, r *http.Request) {
	var request proto.TerminalInput
	if !decode(w, r, &request) {
		return
	}
	if err := s.terms.Input(r.Context(), r.PathValue("id"), request.Tab, request.Seq, request.Data); err != nil {
		terminalError(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) resizeTerminal(w http.ResponseWriter, r *http.Request) {
	var request proto.ResizeTerminal
	if !decode(w, r, &request) {
		return
	}
	if err := s.terms.Resize(r.PathValue("id"), request.Rows, request.Cols); err != nil {
		terminalError(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// terminalError answers a terminal's failure: an unknown or ended shell is
// not_found, a rule broken bad_request, a shell's state or pocketd's
// conflict.
func terminalError(w http.ResponseWriter, s *Server, err error) {
	var b *terminal.BadRequest
	switch {
	case errors.As(err, &b):
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, b.Why)
	case errors.Is(err, terminal.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such terminal")
	case errors.Is(err, terminal.ErrFull), errors.Is(err, terminal.ErrEnded), errors.Is(err, terminal.ErrTooManyTabs), errors.Is(err, terminal.ErrClosed):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error())
	default:
		s.internal(w, "run the terminal", err)
	}
}
