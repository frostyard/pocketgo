package pocketdtest

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"  // GIFs decode as pocketd decodes them
	_ "image/jpeg" // and JPEGs
	_ "image/png"  // and PNGs
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	_ "golang.org/x/image/webp" // and WebPs
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
	// The name and type pocketd keeps (Codex review of #49, round 8).
	name := proto.SafeUploadName(r.URL.Query().Get("name"))
	mimeType := proto.UploadMime(name, r.Header.Get("Content-Type"))
	s.nextUpload++
	up := proto.Upload{ID: s.nextUpload, SessionID: id, Name: name, MimeType: mimeType, Size: int64(len(data)),
		Path: fmt.Sprintf("%s/%d/%d-%s", UploadsDir, id, s.nextUpload, name)}
	s.uploads = append(s.uploads, up)
	if s.asImage == nil {
		s.asImage = map[int64]bool{}
	}
	s.asImage[up.ID] = goesAsImage(mimeType, data)
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
// many images the message carries, whatever model the session runs, as
// pocketd counts them (rule 10a; Codex review of #49, round 6); ok is
// false when one is no upload of the session. s.mu is held.
func (s *Server) attachedLocked(session int64, attachments []int64) (files []proto.AttachedFile, images int, ok bool) {
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
		if s.asImage[up.ID] {
			images++
		}
	}
	return files, images, true
}

// goesAsImage says whether a message carries an upload of mimeType and
// data as an image, as pocketd's rule 10a says: a PNG, JPEG, GIF or WebP
// type, at most 5,242,880 bytes, whose bytes are an image of those
// formats, of at most 50,000,000 pixels, that decodes whole (a GIF its
// first frame), as pocketd decodes it (Codex review of #49, round 9).
func goesAsImage(mimeType string, data []byte) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return false
	}
	if len(data) > 5<<20 {
		return false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif" && format != "webp") ||
		config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 50_000_000 {
		return false
	}
	_, _, err = image.Decode(bytes.NewReader(data))
	return err == nil
}
