package pocketdtest

import (
	"fmt"
	"net/http"

	"github.com/frostyard/pocketgo/proto"
)

// SetChanges gives session c.SessionID the changes GET
// /v1/sessions/{id}/changes answers (ADR-0053). A session given none
// answers that its folder is in no git checkout, as pocketd does.
func (s *Server) SetChanges(c proto.Changes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changes == nil {
		s.changes = map[int64]proto.Changes{}
	}
	s.changes[c.SessionID] = c
}

// SetFileDiff gives the file d.Path of session d.SessionID the diff GET
// /v1/sessions/{id}/changes/file answers; a file given none is not found.
func (s *Server) SetFileDiff(d proto.FileDiff) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diffs == nil {
		s.diffs = map[string]proto.FileDiff{}
	}
	s.diffs[fmt.Sprint(d.SessionID, ":", d.Path)] = d
}

func (s *Server) changesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathChanges, func(w http.ResponseWriter, r *http.Request) {
		id := pathID(r)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.sessions[id] == nil {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no session")
			return
		}
		c, ok := s.changes[id]
		if !ok {
			c = proto.Changes{SessionID: id}
		}
		if c.Files == nil {
			c.Files = []proto.ChangedFile{}
		}
		writeJSON(w, http.StatusOK, c)
	})
	mux.HandleFunc("GET "+proto.PathChangesFile, func(w http.ResponseWriter, r *http.Request) {
		id := pathID(r)
		path := r.URL.Query().Get("path")
		// A path no change can have is refused first, as pocketd does
		// (rule 64).
		if err := proto.CheckChangedPath(path); err != nil {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		d, ok := s.diffs[fmt.Sprint(id, ":", path)]
		if s.sessions[id] == nil || !ok {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no change names "+path)
			return
		}
		writeJSON(w, http.StatusOK, d)
	})
}
