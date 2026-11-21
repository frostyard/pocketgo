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
// together: one that fails leaves neither.
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
	result, existed, err := s.store.LookupRequest(ctx, key, kind)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case existed:
		// A retry answers the upload the first call stored (rule 5).
		upload, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			s.internal(w, "read request", fmt.Errorf("request %s records no upload (%q)", kind, result))
			return
		}
		row, err := s.store.Upload(ctx, upload)
		if err != nil {
			s.internal(w, "read the upload", err)
			return
		}
		writeJSON(w, http.StatusOK, s.wireUpload(row))
		return
	}
	name := uploads.SafeName(query.Get("name"))
	body := &bodyReader{r: r.Body}
	file, size, err := s.uploads.Store(id, name, body)
	if err != nil {
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
	row, _, err := s.store.RecordUpload(ctx, key, kind, store.UploadRow{Session: id, File: file, Name: name,
		Mime: uploads.Mime(name, r.Header.Get("Content-Type")), Size: size})
	if err != nil {
		// Not recorded: the file goes too, so an upload leaves both or
		// neither.
		if removeErr := s.uploads.Remove(id, file); removeErr != nil {
			s.log.Warn("remove an upload that was not recorded", "session", id, "file", file, "err", removeErr)
		}
		if errors.Is(err, store.ErrRequestReused) {
			reused(w)
			return
		}
		s.internal(w, "record the upload", err)
		return
	}
	writeJSON(w, http.StatusCreated, s.wireUpload(row))
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
