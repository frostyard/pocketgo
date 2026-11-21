package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Attachment is a file the owner attaches to their next message to a
// session, as its chip in the composer shows it (ADR-0029): its name, and
// whether it uploads, failed to (Err), or uploaded (Upload).
type Attachment struct {
	// Key is the chip's identity, which no other chip of the store has.
	Key       int64
	Name      string
	Uploading bool
	Err       string
	Upload    proto.Upload
}

// attachment is a chip, what it uploads from, and the stop of its upload.
type attachment struct {
	Attachment
	open   Source
	cancel context.CancelFunc
}

// Source is what a file uploads from: Open gives its bytes and their
// number, and Version what tells one content of it from another, so a
// retry of a source that changed is a new request (Codex review of #49,
// round 3).
type Source struct {
	Open    func() (io.ReadCloser, int64, error)
	Version func() string
}

// UploadTimeout bounds one upload: a file of 50 MB takes a while.
const UploadTimeout = 10 * time.Minute

// PastedImageName is the name a pasted image uploads as, as pocket's.
const PastedImageName = "pasted-image.png"

// ErrTooLarge is a file over the upload limit.
var ErrTooLarge = fmt.Errorf("a file has at most %d MB", proto.MaxUpload>>20)

// BytesSource is data as a source, as a pasted image: it never changes.
func BytesSource(data []byte) Source {
	return Source{
		Open: func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
		},
		Version: func() string { return "" },
	}
}

// FileSource is the file at path as a source: a regular file of at most
// proto.MaxUpload bytes, opened without waiting on a named pipe. Its
// version is its size and its time of change as the file says now.
func FileSource(path string) Source {
	return Source{
		Open: func() (io.ReadCloser, int64, error) {
			f, err := openUploadFile(path)
			if err != nil {
				return nil, 0, err
			}
			info, err := f.Stat()
			switch {
			case err != nil:
			case !info.Mode().IsRegular():
				err = errors.New("only a regular file can be attached")
			case info.Size() > proto.MaxUpload:
				err = ErrTooLarge
			}
			if err != nil {
				_ = f.Close()
				return nil, 0, err
			}
			return f, info.Size(), nil
		},
		Version: func() string {
			info, err := os.Stat(path)
			if err != nil {
				return ""
			}
			return strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
		},
	}
}

// UploadType is the type a file named name, whose bytes start with head,
// uploads as: its extension's, else what its first bytes say, as pocket
// does. pocketd checks and keeps it (rule 10a).
func UploadType(name string, head []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

// Attach uploads a file to session id for the owner's next message, read
// from open: its chip shows at once, uploading, then uploaded or failed.
// The upload's request id is its chip's own.
func (s *Store) Attach(id int64, name string, open Source) {
	s.nextAttach++
	a := &attachment{Attachment: Attachment{Key: s.nextAttach, Name: name}, open: open}
	s.attachments[id] = append(s.attachments[id], a)
	s.log(LevelCommand, "Attach %s to session %d", name, id)
	s.upload(id, a)
}

// RetryAttachment uploads chip key of session id again, after its upload
// failed: from the same source, with the same request id, so an upload
// pocketd stored whose answer was lost is the one answered again, never a
// second copy (Codex review of #49, round 2).
func (s *Store) RetryAttachment(id, key int64) {
	for _, a := range s.attachments[id] {
		if a.Key == key && !a.Uploading && a.Err != "" {
			s.log(LevelCommand, "Attach %s to session %d again", a.Name, id)
			s.upload(id, a)
		}
	}
}

// upload sends chip a of session id to pocketd, under its chip's request
// id, which it keeps until the upload succeeded.
func (s *Store) upload(id int64, a *attachment) {
	a.Uploading, a.Err = true, ""
	name, open := a.Name, a.open.Open
	key := fmt.Sprintf("upload:%d:%d", id, a.Key)
	// The request id is kept for the name and the source's version: a
	// file that changed since its try uploads as a new request.
	requestID := s.ids.For(key, name+"\x00"+a.open.Version())
	life, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	var got proto.Upload
	s.doWithin("Uploading "+name, UploadTimeout, func(ctx context.Context, c *proto.Client) error {
		ctx, stop := context.WithCancel(ctx)
		defer stop()
		defer context.AfterFunc(life, stop)()
		if err := life.Err(); err != nil {
			return err
		}
		r, size, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		if size < 0 || size > proto.MaxUpload {
			return ErrTooLarge
		}
		head := make([]byte, min(size, 512))
		n, err := io.ReadFull(r, head)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		got, err = c.Upload(ctx, id, requestID, name, UploadType(name, head[:n]), io.MultiReader(bytes.NewReader(head[:n]), r), size)
		return err
	}, func(err error) {
		cancel()
		if !slices.Contains(s.attachments[id], a) {
			return // removed while it uploaded
		}
		a.Uploading = false
		if err != nil {
			a.Err = err.Error()
			return
		}
		a.Upload = got
		s.ids.Done(key)
	})
}

// AttachFiles attaches the files at paths to session id's next message,
// each as its base name (FileSource).
func (s *Store) AttachFiles(id int64, paths []string) {
	for _, path := range paths {
		s.Attach(id, filepath.Base(path), FileSource(path))
	}
}

// ErrSignInChanged is a choice of files whose sign-in ended while the
// chooser was open: another pocketd may number a session alike, so its
// files are not attached.
var ErrSignInChanged = errors.New("the sign-in changed while the files were chosen: attach them again")

// AttachPicked asks pick, the native file chooser, for files to attach to
// session id's next message, off the UI thread, and attaches those chosen
// (AttachFiles) only while the sign-in that asked is the store's: files
// chosen for one pocketd never go to another (Codex review of #49, round
// 1). done runs on the UI thread with pick's error, or ErrSignInChanged.
func (s *Store) AttachPicked(id int64, pick func(context.Context) ([]string, error), done func(error)) {
	gen := s.gen
	go func() {
		paths, err := pick(context.Background())
		s.opts.Post(func() {
			if err == nil && gen != s.gen {
				err = ErrSignInChanged
			}
			if err == nil {
				s.AttachFiles(id, paths)
			}
			done(err)
		})
	}()
}

// Attachments are session id's chips, in the order they were attached.
func (s *Store) Attachments(id int64) []Attachment {
	out := make([]Attachment, 0, len(s.attachments[id]))
	for _, a := range s.attachments[id] {
		out = append(out, a.Attachment)
	}
	return out
}

// RemoveAttachment drops chip key of session id, and stops its upload.
func (s *Store) RemoveAttachment(id, key int64) {
	s.attachments[id] = slices.DeleteFunc(s.attachments[id], func(a *attachment) bool {
		if a.Key != key {
			return false
		}
		if a.cancel != nil {
			a.cancel()
		}
		s.ids.Done(fmt.Sprintf("upload:%d:%d", id, key))
		return true
	})
}

// AttachmentsReady says whether every chip of session id uploaded: a
// message waits while one uploads, and while one failed, until the owner
// removes it, as pocket's Send does.
func (s *Store) AttachmentsReady(id int64) bool {
	for _, a := range s.attachments[id] {
		if a.Uploading || a.Err != "" {
			return false
		}
	}
	return true
}

// attachmentsBlock says why a message to session id waits for its chips,
// "" when none holds it.
func (s *Store) attachmentsBlock(id int64) string {
	for _, a := range s.attachments[id] {
		switch {
		case a.Uploading:
			return a.Name + " is still uploading"
		case a.Err != "":
			return a.Name + " did not upload: remove it, or attach it again"
		}
	}
	return ""
}
