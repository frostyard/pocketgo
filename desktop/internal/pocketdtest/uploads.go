package pocketdtest

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// UploadsDir is where the fake says it keeps the owner's uploads (rule
// 10a); it keeps their bytes in memory.
const UploadsDir = "/home/team/.local/share/pocketgo-uploads"

// uploadRoutes serves the owner's uploads.
func (s *Server) uploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+proto.PathUploads, s.uploadFile)
}

// uploadFile keeps a file the owner attaches to a message, once a
// request id, as pocketd does: a worker's or a cross-review's session
// takes none.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	requestID := r.URL.Query().Get("requestId")
	if requestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "requestId is required")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the upload stopped before it ended")
		return
	}
	id := pathID(r)
	// The request id is looked up under the lock that records its upload,
	// as pocketd holds a request's key: two tries of one id store one
	// upload (Codex review of #49, round 5).
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, seen := s.replies[requestID]
	if s.afterReplayLookup != nil {
		s.afterReplayLookup()
	}
	if seen {
		if prior.op != r.Method+" "+r.URL.Path {
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(prior.body)
		return
	}
	sess := s.sessions[id]
	if sess == nil {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	}
	if route := sess.detail.Session.Route; route == "cross-review" || strings.HasPrefix(route, "worker") {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "this session takes no upload")
		return
	}
	name := path.Base("/" + r.URL.Query().Get("name"))
	if name == "/" {
		name = "upload"
	}
	mimeType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	s.nextUpload++
	up := proto.Upload{ID: s.nextUpload, SessionID: id, Name: name, MimeType: mimeType, Size: int64(len(data)),
		Path: fmt.Sprintf("%s/%d/%d-%s", UploadsDir, id, s.nextUpload, name)}
	s.uploads = append(s.uploads, up)
	s.rememberLocked(r, requestID, up)
	writeJSON(w, http.StatusCreated, up)
}

// Uploads are the files uploaded, oldest first.
func (s *Server) Uploads() []proto.Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.uploads)
}

// attachedLocked are the files of message attachments to session, and how
// many images a model that takes them gets of them; ok is false when one
// is no upload of the session. s.mu is held.
func (s *Server) attachedLocked(session int64, attachments []int64) (files []proto.AttachedFile, images int, ok bool) {
	sess := s.sessions[session]
	takes := slices.ContainsFunc(s.models, func(m proto.Model) bool { return m.ModelRef == sess.detail.Session.Model && m.Images })
	seen := map[int64]bool{}
	for _, id := range attachments {
		if seen[id] {
			continue
		}
		seen[id] = true
		i := slices.IndexFunc(s.uploads, func(u proto.Upload) bool { return u.ID == id && u.SessionID == session })
		if i < 0 {
			return nil, 0, false
		}
		up := s.uploads[i]
		files = append(files, proto.AttachedFile{Path: up.Path, Name: up.Name, MimeType: up.MimeType, Size: up.Size})
		if takes && strings.HasPrefix(up.MimeType, "image/") {
			images++
		}
	}
	return files, images, true
}
