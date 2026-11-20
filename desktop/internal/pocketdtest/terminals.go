package pocketdtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's terminals (ADR-0025, protocol rule 56) in memory: a shell's
// output is what a test prints to it, and its keys and sizes are what the
// client sent. A poll waits until a test prints, the shell exits or ends,
// or the client gives up.

// HomeFolder is the service user's home, where a shell starts by default.
const HomeFolder = "/home/team"

type fakeShell struct {
	info    proto.Terminal
	out     []byte
	keys    []byte
	seqs    map[string]int64
	sizes   [][2]int
	gone    bool
	changed chan struct{}
}

func (sh *fakeShell) wake() {
	close(sh.changed)
	sh.changed = make(chan struct{})
}

// ReplyToKeys has every shell print what reply returns for the keys it
// takes, as a shell answers a command.
func (s *Server) ReplyToKeys(reply func(id string, keys []byte) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shellReply = reply
}

// AddTerminal adds a shell, listed and running, without an event.
func (s *Server) AddTerminal(t proto.Terminal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shells = append(s.shells, &fakeShell{info: t, seqs: map[string]int64{}, changed: make(chan struct{})})
}

// PrintTerminal adds output to shell id, as its shell prints.
func (s *Server) PrintTerminal(id, output string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh := s.shellLocked(id); sh != nil {
		sh.out = append(sh.out, output...)
		sh.wake()
	}
}

// ExitTerminal has shell id exit with code: it stays listed, exited, and
// its event goes out.
func (s *Server) ExitTerminal(id string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh := s.shellLocked(id); sh != nil {
		sh.info.Exited, sh.info.ExitCode = true, &code
		sh.wake()
		s.emitTerminalsLocked()
	}
}

// EndTerminal ends shell id, as pocketd's timers or another client do: it
// leaves the list, with its event.
func (s *Server) EndTerminal(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endShellLocked(id)
}

// TerminalKeys are the keys shell id took, each input once.
func (s *Server) TerminalKeys(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh := s.shellLocked(id); sh != nil {
		return string(sh.keys)
	}
	return ""
}

// TerminalSizes are the sizes shell id was told, rows by cols, in order.
func (s *Server) TerminalSizes(id string) [][2]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh := s.shellLocked(id); sh != nil {
		return slices.Clone(sh.sizes)
	}
	return nil
}

// Terminals are the shells listed.
func (s *Server) Terminals() []proto.Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalsLocked()
}

func (s *Server) terminalsLocked() []proto.Terminal {
	out := []proto.Terminal{}
	for _, sh := range s.shells {
		out = append(out, sh.info)
	}
	return out
}

func (s *Server) shellLocked(id string) *fakeShell {
	for _, sh := range s.shells {
		if sh.info.ID == id {
			return sh
		}
	}
	return nil
}

func (s *Server) emitTerminalsLocked() {
	s.emitLocked(proto.Event{Type: proto.EventTerminals, Terminals: s.terminalsLocked()})
}

func (s *Server) endShellLocked(id string) bool {
	i := slices.IndexFunc(s.shells, func(sh *fakeShell) bool { return sh.info.ID == id })
	if i < 0 {
		return false
	}
	sh := s.shells[i]
	sh.gone = true
	sh.wake()
	s.shells = slices.Delete(s.shells, i, i+1)
	s.emitTerminalsLocked()
	return true
}

func (s *Server) terminalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathTerminals, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Terminals())
	})
	mux.HandleFunc("POST "+proto.PathTerminals, s.startTerminal)
	mux.HandleFunc("DELETE "+proto.PathTerminal, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.endShellLocked(r.PathValue("id")) {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such terminal")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	})
	mux.HandleFunc("GET "+proto.PathTerminalOutput, s.terminalOutput)
	mux.HandleFunc("POST "+proto.PathTerminalInput, s.terminalInput)
	mux.HandleFunc("POST "+proto.PathTerminalResize, func(w http.ResponseWriter, r *http.Request) {
		var request proto.ResizeTerminal
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Rows < 2 || request.Cols < 2 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		sh := s.shellLocked(r.PathValue("id"))
		switch {
		case sh == nil:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such terminal")
		case sh.info.Exited:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "this terminal has ended")
		default:
			sh.sizes = append(sh.sizes, [2]int{request.Rows, request.Cols})
			writeJSON(w, http.StatusOK, struct{}{})
		}
	})
}

func (s *Server) startTerminal(w http.ResponseWriter, r *http.Request) {
	var request proto.StartTerminal
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cwd := HomeFolder
	switch request.Folder {
	case proto.TerminalSession:
		sess := s.sessions[request.Session]
		if sess == nil {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", request.Session))
			return
		}
		cwd = sess.detail.Session.Cwd
	case proto.TerminalPath:
		cwd = request.Path
	}
	live := 0
	for _, sh := range s.shells {
		if !sh.info.Exited {
			live++
		}
	}
	if live >= proto.MaxTerminals {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "pocketd runs 4 terminals at once: end one first")
		return
	}
	s.nextShell++
	for s.shellLocked("s"+strconv.Itoa(s.nextShell)) != nil {
		s.nextShell++
	}
	id := "s" + strconv.Itoa(s.nextShell)
	rows, cols := request.Rows, request.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	s.shells = append(s.shells, &fakeShell{info: proto.Terminal{ID: id, Title: path.Base(cwd), Cwd: cwd, StartedAt: Now},
		seqs: map[string]int64{}, sizes: [][2]int{{rows, cols}}, changed: make(chan struct{})})
	s.emitTerminalsLocked()
	started := proto.TerminalStarted{ID: id}
	s.rememberLocked(r, request.RequestID, started)
	writeJSON(w, http.StatusCreated, started)
}

func (s *Server) terminalOutput(w http.ResponseWriter, r *http.Request) {
	from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if err != nil || from < 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "give from")
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	sh := s.shellLocked(id)
	if sh == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such terminal")
		return
	}
	wait := r.URL.Query().Get("wait") != "0"
	for {
		if sh.gone {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such terminal")
			return
		}
		offset := min(from, int64(len(sh.out)))
		if int64(len(sh.out)) > offset || sh.info.Exited || !wait {
			out := proto.TerminalOutput{Data: slices.Clone(sh.out[offset:]), From: offset, To: int64(len(sh.out)),
				Exited: sh.info.Exited, ExitCode: sh.info.ExitCode}
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, out)
			return
		}
		changed := sh.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
		s.mu.Lock()
	}
}

func (s *Server) terminalInput(w http.ResponseWriter, r *http.Request) {
	var request proto.TerminalInput
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Tab == "" || request.Seq < 1 || len(request.Data) > proto.MaxTerminalInput {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh := s.shellLocked(r.PathValue("id"))
	switch {
	case sh == nil:
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such terminal")
		return
	case sh.info.Exited:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "this terminal has ended")
		return
	}
	if request.Seq > sh.seqs[request.Tab] {
		sh.seqs[request.Tab] = request.Seq
		sh.keys = append(sh.keys, request.Data...)
		if s.shellReply != nil {
			if out := s.shellReply(sh.info.ID, request.Data); out != "" {
				sh.out = append(sh.out, out...)
				sh.wake()
			}
		}
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
