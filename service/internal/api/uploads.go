package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/uploads"
)

// Image is an image a message gives the model (ADR-0029).
type Image = uploads.Image

// SetUploads gives the server the uploads folder (ADR-0029); without one,
// an upload fails.
func (s *Server) SetUploads(f *uploads.Folder) { s.uploads = f }

// noComposer says why session id takes no message, and so no upload, from
// the owner: a thread's worker's and a cross-review's (rules 46 and 47).
// "" for a session that takes them.
func (s *Server) noComposer(ctx context.Context, id int64) (string, error) {
	if thread, err := s.store.ThreadOfWorker(ctx, id); err == nil {
		return fmt.Sprintf("session %d is thread T%d's worker: its specialist directs it, and it takes no upload", id, thread.ID), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if thread, err := s.store.ThreadOfSession(ctx, id); err == nil && thread.ReviewPull != 0 {
		return fmt.Sprintf("thread T%d is a cross-review pocketd runs: its session takes no upload", thread.ID), nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return "", nil
}

// upload stores a file the owner attaches to a message to the session
// (rule 10a): the body is the file, its name and the request id are in
// the query string, its type is Content-Type. The file and its record go
// together: the upload is claimed pending before its file is written and
// stored once it is, so one that fails, or that a stop of pocketd cuts
// off, leaves neither once it is dropped (Codex review of #49, round 1).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	requestID := query.Get("requestId")
	if !validRequestID(w, requestID) {
		return
	}
	if r.ContentLength > proto.MaxUpload {
		tooLarge(w)
		return
	}
	if !s.knownSession(w, r, id) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if why, err := s.noComposer(ctx, id); err != nil {
		s.internal(w, "read the session's thread", err)
		return
	} else if why != "" {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, why)
		return
	}
	if s.uploads == nil {
		s.internal(w, "store the upload", errors.New("pocketd has no uploads folder"))
		return
	}
	key := requestKey(r, requestID)
	defer s.keys.lock(key)()
	kind := "upload:" + strconv.FormatInt(id, 10)
	// The name the file and its record take is one safe name (Codex review
	// of #49, round 1).
	name := uploads.SafeName(query.Get("name"))
	row, pending, existed, err := s.store.ClaimUpload(ctx, key, kind, store.UploadRow{Session: id, File: s.uploads.NewFile(name), Name: name})
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "claim the upload", err)
		return
	case existed && !pending:
		// A retry answers the upload the first call stored (rule 5).
		writeJSON(w, http.StatusOK, s.wireUpload(row))
		return
	case existed:
		// A try a stop cut off, or whose drop failed, left its claim: its
		// file goes, and this try starts afresh.
		if err := s.dropUpload(ctx, store.PendingUpload{Key: key, Session: row.Session, File: row.File}); err != nil {
			s.internal(w, "drop the upload a stop cut off", err)
			return
		}
		if row, _, _, err = s.store.ClaimUpload(ctx, key, kind, store.UploadRow{Session: id, File: s.uploads.NewFile(name), Name: name}); err != nil {
			s.internal(w, "claim the upload", err)
			return
		}
	}
	if s.beforeUploadWrite != nil {
		s.beforeUploadWrite(id, row.File)
	}
	body := &bodyReader{r: r.Body}
	size, err := s.uploads.Write(id, row.File, body)
	if err != nil {
		// Write removed what it made, and made nothing at a name taken:
		// only the claim goes (Codex review of #49, round 2).
		if dropErr := s.store.DropUpload(ctx, key, row.File); dropErr != nil {
			s.log.Warn("drop an upload that failed", "session", id, "file", row.File, "err", dropErr)
		}
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(body.err, &tooBig):
			tooLarge(w)
		case body.err != nil:
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the upload stopped before it ended")
		default:
			s.internal(w, "store the upload", err)
		}
		return
	}
	if s.afterUploadWrite != nil && s.afterUploadWrite() {
		// A test's stop of pocketd, between the file and its record.
		s.internal(w, "store the upload", errors.New("stopped"))
		return
	}
	stored, err := s.finishUpload(ctx, key, size, uploads.Mime(name, r.Header.Get("Content-Type")))
	if err != nil {
		// The file claimed goes, by its name in the claim (Codex review of
		// #49, round 2).
		if dropErr := s.dropUpload(ctx, store.PendingUpload{Key: key, Session: id, File: row.File}); dropErr != nil {
			s.log.Warn("drop an upload that was not recorded", "session", id, "file", row.File, "err", dropErr)
		}
		s.internal(w, "record the upload", err)
		return
	}
	writeJSON(w, http.StatusCreated, s.wireUpload(stored))
}

// dropUpload removes a pending upload's file, then its claim: one whose
// file may still be there keeps its claim, for the next try or start. A
// folder that is no longer the uploads folder's own holds no file of
// pocketd's to remove.
func (s *Server) dropUpload(ctx context.Context, p store.PendingUpload) error {
	if err := s.uploads.Remove(p.Session, p.File); err != nil && !errors.Is(err, uploads.ErrNotItsFolder) && !errors.Is(err, uploads.ErrNotOurs) {
		return err
	}
	return s.store.DropUpload(ctx, p.Key, p.File)
}

// dropLeftUploads drops the uploads a stop of pocketd left pending, their
// files included, once: the first delivery pass does (uploadsDue), before
// it takes releaseMu. Each is dropped under its request's key, and only
// while the key's pending upload is still the one the pass found: a retry
// of that request, which serving does not hold back, may have dropped it
// and claimed another file (Codex review of #49, round 2).
func (s *Server) dropLeftUploads(ctx context.Context) error {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	if !s.uploadsDue || s.uploads == nil {
		s.uploadsDue = false
		return nil
	}
	left, err := s.store.PendingUploads(ctx, s.startedAt)
	if err != nil {
		return err
	}
	if s.afterUploadsRead != nil {
		s.afterUploadsRead()
	}
	var failures []error
	for _, p := range left {
		if err := s.dropLeftUpload(ctx, p); err != nil {
			failures = append(failures, fmt.Errorf("drop the upload %s: %w", p.File, err))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	s.uploadsDue = false
	return nil
}

// dropLeftUpload drops p under its request's key, if the key's pending
// upload is still p's file.
func (s *Server) dropLeftUpload(ctx context.Context, p store.PendingUpload) error {
	defer s.keys.lock(p.Key)()
	now, err := s.store.PendingUploadOf(ctx, p.Key)
	if errors.Is(err, store.ErrNotFound) || err == nil && now.File != p.File {
		return nil
	} else if err != nil {
		return err
	}
	return s.dropUpload(ctx, p)
}

// bodyReader keeps the error of a read of the request's body: a body cut
// off, or over its bound, is the client's doing, not the service's.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

func tooLarge(w http.ResponseWriter) {
	writeError(w, http.StatusRequestEntityTooLarge, proto.ErrorCodeBadRequest, fmt.Sprintf("a file has at most %d bytes", proto.MaxUpload))
}

func (s *Server) wireUpload(row store.UploadRow) proto.Upload {
	return proto.Upload{ID: row.ID, SessionID: row.Session, Name: row.Name, MimeType: row.Mime, Size: row.Size, Path: s.uploads.Path(row.Session, row.File)}
}

// messageOf is the message session id gets for text with the uploads
// rows: text, then a line for each file (proto.WithAttachments), and the
// images the model gets with it, when it takes images (rule 10a). An image
// that cannot go is named with a note why.
func (s *Server) messageOf(ctx context.Context, id int64, text string, rows []store.UploadRow) (string, []Image, error) {
	if len(rows) == 0 {
		return text, nil, nil
	}
	if s.uploads == nil {
		return "", nil, errors.New("pocketd has no uploads folder")
	}
	takes := false
	for _, row := range rows {
		if uploads.ForModel(row.Mime, row.Size) {
			var err error
			if takes, err = s.takesImages(ctx, id); err != nil {
				return "", nil, err
			}
			break
		}
	}
	files := make([]proto.AttachedFile, 0, len(rows))
	var images []Image
	for _, row := range rows {
		f := proto.AttachedFile{Path: s.uploads.Path(id, row.File), Name: row.Name, MimeType: row.Mime, Size: row.Size}
		if takes && uploads.ForModel(row.Mime, row.Size) {
			data, err := s.uploads.Read(id, row.File, row.Size)
			if err != nil {
				f.Note = uploads.NotShown(unreadable(err))
			} else if image, note, err := uploads.PrepareImage(data); err != nil {
				f.Note = uploads.NotShown(err)
			} else {
				images = append(images, image)
				f.Note = note
			}
		}
		files = append(files, f)
	}
	return proto.WithAttachments(text, files), images, nil
}

// unreadable says why an upload's file could not be read, in words for
// its note.
func unreadable(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("its file is gone")
	case errors.Is(err, uploads.ErrGrown):
		return errors.New("its file is larger than it was uploaded")
	case errors.Is(err, uploads.ErrNotRegular):
		return errors.New("its file is no regular file")
	case errors.Is(err, uploads.ErrNotItsFolder):
		return errors.New("a link stands in its folder's place")
	}
	return errors.New("its file could not be read")
}

// takesImages says whether session id's model takes images: the model it
// runs now, as the models list it (Model.Images).
func (s *Server) takesImages(ctx context.Context, id int64) (bool, error) {
	choice, err := s.engine.SessionModel(ctx, id)
	if err != nil {
		return false, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range models {
		if m.ModelRef == choice.Model {
			return m.Images, nil
		}
	}
	return false, nil
}
