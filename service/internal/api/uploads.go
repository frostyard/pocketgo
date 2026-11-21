package api

import (
	"context"
	"encoding/base64"
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
// the query string, its type is Content-Type. The upload is claimed,
// pending, before its file is written; staged, with what tells its file
// apart, once the file is written whole and unnamed; and stored once the
// file has its name. A failure before the name leaves nothing in the
// uploads folder; a stop of pocketd after it leaves a staged upload that
// the request tried again, or the next start, finishes. pocketd removes
// nothing there (Codex review of #49, rounds 1 to 3).
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
	claim := store.UploadRow{Session: id, Name: name, Mime: uploads.Mime(name, r.Header.Get("Content-Type"))}
	claim.File = s.uploads.NewFile(name)
	row, pending, existed, err := s.store.ClaimUpload(ctx, key, kind, claim, s.run)
	if existed && pending && err == nil {
		// A try a stop cut off left its claim: finished when it named its
		// file, else dropped, and this try claims afresh.
		var finished bool
		if row, finished, err = s.settleLeftUpload(ctx, key); err == nil && !finished {
			claim.File = s.uploads.NewFile(name)
			row, pending, existed, err = s.store.ClaimUpload(ctx, key, kind, claim, s.run)
		} else if err == nil {
			pending = false
		}
	}
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
	}
	body := &bodyReader{r: r.Body}
	staged, err := s.uploads.Stage(id, body)
	if err != nil {
		s.dropClaim(ctx, key, row.File)
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
	defer staged.Close()
	if err := s.store.StageUpload(ctx, key, row.File, staged.Identity.Dev, staged.Identity.Ino, staged.Identity.Size, staged.Identity.Digest); err != nil {
		s.dropClaim(ctx, key, row.File)
		s.internal(w, "record the upload", err)
		return
	}
	if s.uploadStop != nil && s.uploadStop("staged", id, row.File) {
		// A test's stop of pocketd, before the file's name.
		s.internal(w, "store the upload", errors.New("stopped"))
		return
	}
	if err := staged.Link(row.File); err != nil {
		// Nothing was named: an entry at the name stays as it is (Codex
		// review of #49, round 2).
		s.dropClaim(ctx, key, row.File)
		s.internal(w, "name the upload's file", err)
		return
	}
	if s.uploadStop != nil && s.uploadStop("linked", id, row.File) {
		// A test's stop of pocketd, between the name and the record.
		s.internal(w, "store the upload", errors.New("stopped"))
		return
	}
	stored, err := s.finishUpload(ctx, key)
	if err != nil {
		// Named and staged: the request tried again, or the next start,
		// finishes it (Codex review of #49, round 3).
		s.internal(w, "record the upload", err)
		return
	}
	writeJSON(w, http.StatusCreated, s.wireUpload(stored))
}

// dropClaim frees key's pending upload of file, whose file has no name: a
// request that failed leaves no record.
func (s *Server) dropClaim(ctx context.Context, key, file string) {
	if err := s.store.DropUpload(ctx, key, file); err != nil {
		s.log.Warn("free an upload's claim", "file", file, "err", err)
	}
}

// settleLeftUpload settles key's pending upload, which a try a stop cut
// off left: finished, and answered, while its name holds the file it
// staged, pocketd's own, and else dropped, whatever the name holds, which
// pocketd never removes (Codex review of #49, round 3). Its request's key
// is held.
func (s *Server) settleLeftUpload(ctx context.Context, key string) (store.UploadRow, bool, error) {
	p, err := s.store.PendingUploadOf(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return store.UploadRow{}, false, nil
	} else if err != nil {
		return store.UploadRow{}, false, err
	}
	if p.Staged {
		ours, err := s.uploads.Holds(p.Session, p.File, uploads.Identity{Dev: p.Dev, Ino: p.Ino, Size: p.Size, Digest: p.Digest})
		if err != nil {
			return store.UploadRow{}, false, err
		}
		if ours {
			row, err := s.finishUpload(ctx, key)
			return row, err == nil, err
		}
	}
	return store.UploadRow{}, false, s.store.DropUpload(ctx, key, p.File)
}

// settleLeftUploads settles the uploads another run of pocketd left
// pending (settleLeftUpload), and clears what it left in the staging
// folder: the first delivery pass does (uploadsDue), outside releaseMu,
// and the passes after it while one is left. Each is settled under its
// request's key, and one whose key a request holds waits for a later
// pass: the pass never waits on an upload under way, and a request that
// holds the key may not settle it, as one that reuses its id for another
// request does not (Codex review of #49, rounds 2 to 4).
func (s *Server) settleLeftUploads(ctx context.Context) (left bool, err error) {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	if !s.uploadsDue || s.uploads == nil {
		s.uploadsDue = false
		return false, nil
	}
	if err := s.uploads.ClearStaging(); err != nil {
		return true, err
	}
	pending, err := s.store.PendingUploads(ctx, s.run)
	if err != nil {
		return true, err
	}
	if s.afterUploadsRead != nil {
		s.afterUploadsRead()
	}
	var failures []error
	skipped := false
	for _, p := range pending {
		unlock, ok := s.keys.tryLock(p.Key)
		if !ok {
			skipped = true
			continue
		}
		_, _, err := s.settleLeftUpload(ctx, p.Key)
		unlock()
		if err != nil {
			failures = append(failures, fmt.Errorf("settle the upload %s: %w", p.File, err))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return true, err
	}
	s.uploadsDue = skipped
	return skipped, nil
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
// images it carries, which the model that reads it gets as images when it
// takes images (rule 10a). An image that cannot go is named with a note
// why.
func (s *Server) messageOf(id int64, text string, rows []store.UploadRow) (string, []Image, error) {
	if len(rows) == 0 {
		return text, nil, nil
	}
	if s.uploads == nil {
		return "", nil, errors.New("pocketd has no uploads folder")
	}
	files := make([]proto.AttachedFile, 0, len(rows))
	for _, row := range rows {
		files = append(files, proto.AttachedFile{Path: s.uploads.Path(id, row.File), Name: row.Name, MimeType: row.Mime, Size: row.Size})
	}
	// Too long before any note is too long: no image is read for it
	// (Codex review of #49, round 4).
	if n := len(proto.WithAttachments(text, files)); n > s.maxComposed {
		return "", nil, fmt.Errorf("%w: the message with its files' lines has %d bytes, more than %d", errTooLong, n, s.maxComposed)
	}
	var images []Image
	inline := 0
	for i, row := range rows {
		if !uploads.ForModel(row.Mime, row.Size) {
			continue
		}
		data, err := s.uploads.Read(id, row.File, row.Size)
		if err != nil {
			files[i].Note = uploads.NotShown(unreadable(err))
			continue
		}
		image, note, err := s.prepareImage(data)
		switch {
		case err != nil:
			files[i].Note = uploads.NotShown(err)
		case inline+base64.StdEncoding.EncodedLen(len(image.Data)) > s.maxImages:
			// The message's images are held in memory, and copied, until
			// it goes: past the budget, an image is named alone (Codex
			// review of #49, round 4).
			files[i].Note = uploads.NotShown(uploads.ErrImagesFull)
		default:
			inline += base64.StdEncoding.EncodedLen(len(image.Data))
			images = append(images, image)
			files[i].Note = note
		}
	}
	composed := proto.WithAttachments(text, files)
	if len(composed) > s.maxComposed {
		return "", nil, fmt.Errorf("%w: the message with its files' lines has %d bytes, more than %d", errTooLong, len(composed), s.maxComposed)
	}
	return composed, images, nil
}

// errTooLong is a message whose files' lines make it longer than its
// entry may be (proto.MaxComposedMessage).
var errTooLong = errors.New("too long")

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
