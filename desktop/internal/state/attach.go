package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
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
// Its file is copied once, as it is attached, into a snapshot of the
// desktop's own (snap), with its bytes' digest and its type: every try
// uploads those bytes, under a request id kept for them, whatever the file
// becomes (Codex review of #49, round 4).
type attachment struct {
	Attachment
	open   Source
	snap   string
	size   int64
	digest string
	mime   string
	cancel context.CancelFunc
}

// Source is what a file uploads from: Open gives its bytes and their
// number.
type Source struct {
	Open func() (io.ReadCloser, int64, error)
}

// UploadTimeout bounds one upload: a file of 50 MB takes a while.
const UploadTimeout = 10 * time.Minute

// PastedImageName is the name a pasted image uploads as, as pocket's.
const PastedImageName = "pasted-image.png"

// ErrTooLarge is a file over the upload limit.
var ErrTooLarge = fmt.Errorf("a file has at most %d MB", proto.MaxUpload>>20)

// BytesSource is data as a source, as a pasted image.
func BytesSource(data []byte) Source {
	return Source{Open: func() (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}}
}

// FileSource is the file at path as a source: a regular file of at most
// proto.MaxUpload bytes, opened without waiting on a named pipe.
func FileSource(path string) Source {
	return Source{Open: func() (io.ReadCloser, int64, error) {
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
	}}
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

// snapshot is a copy of what src gives, in a file of dir, with its size,
// its bytes' SHA-256 and the type a file named name of them uploads as.
// The file is read to its end, whatever size it said: one written to as it
// is attached, or that says it is empty, is copied whole, at most
// proto.MaxUpload bytes (Codex review of #49, round 5).
func snapshot(src Source, dir, name string) (path string, size int64, digest, mimeType string, err error) {
	r, said, err := src.Open()
	if err != nil {
		return "", 0, "", "", err
	}
	defer func() { _ = r.Close() }()
	if said > proto.MaxUpload {
		return "", 0, "", "", ErrTooLarge
	}
	out, err := os.CreateTemp(dir, "pocketgo-attachment-*")
	if err != nil {
		return "", 0, "", "", err
	}
	head := &headWriter{}
	sum := sha256.New()
	size, err = io.Copy(io.MultiWriter(out, sum, head), io.LimitReader(r, proto.MaxUpload+1))
	if err == nil && size > proto.MaxUpload {
		err = ErrTooLarge
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(out.Name())
		return "", 0, "", "", err
	}
	return out.Name(), size, hex.EncodeToString(sum.Sum(nil)), UploadType(name, head.buf), nil
}

// headWriter keeps the first 512 bytes written to it, which tell a file's
// type.
type headWriter struct{ buf []byte }

func (h *headWriter) Write(p []byte) (int, error) {
	if room := 512 - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Attach uploads a file to session id for the owner's next message, read
// from open: its chip shows at once, uploading, then uploaded or failed.
// Its bytes are copied first, off the UI thread, and the upload's request
// id is its chip's own, kept for those bytes.
func (s *Store) Attach(id int64, name string, open Source) {
	s.nextAttach++
	a := &attachment{Attachment: Attachment{Key: s.nextAttach, Name: name}, open: open}
	s.attachments[id] = append(s.attachments[id], a)
	s.log(LevelCommand, "Attach %s to session %d", name, id)
	s.prepare(id, a)
}

// prepare copies chip a's file into its snapshot, off the UI thread, and
// uploads it, while the chip is there and the sign-in is the one that
// asked.
func (s *Store) prepare(id int64, a *attachment) {
	a.Uploading, a.Err = true, ""
	gen, dir, name, src := s.gen, s.tempDir, a.Name, a.open
	life, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go func() {
		path, size, digest, mimeType, err := snapshot(src, dir, name)
		if err == nil && !s.snaps.keep(path) {
			return // the store closed: keep removed it
		}
		s.opts.Post(func() {
			if gen != s.gen || life.Err() != nil || !slices.Contains(s.attachments[id], a) {
				if path != "" {
					s.snaps.drop(path)
				}
				return
			}
			if err != nil {
				a.Uploading, a.Err = false, err.Error()
				return
			}
			a.snap, a.size, a.digest, a.mime = path, size, digest, mimeType
			s.upload(id, a)
		})
	}()
}

// RetryAttachment uploads chip key of session id again, after its upload
// failed: the bytes it copied, with the same request id, so an upload
// pocketd stored whose answer was lost is the one answered again, never a
// second copy (Codex review of #49, rounds 2 and 4). A chip whose copy
// failed copies its file again.
func (s *Store) RetryAttachment(id, key int64) {
	for _, a := range s.attachments[id] {
		if a.Key == key && !a.Uploading && a.Err != "" {
			s.log(LevelCommand, "Attach %s to session %d again", a.Name, id)
			if a.snap == "" {
				s.prepare(id, a)
			} else {
				s.upload(id, a)
			}
		}
	}
}

// upload sends chip a's snapshot to pocketd, under its chip's request id,
// kept for its name and its bytes' digest until the upload succeeded; the
// snapshot goes then.
func (s *Store) upload(id int64, a *attachment) {
	a.Uploading, a.Err = true, ""
	name, snap, size, mimeType := a.Name, a.snap, a.size, a.mime
	key := fmt.Sprintf("upload:%d:%d", id, a.Key)
	requestID := s.ids.For(key, name+"\x00"+a.digest)
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
		open := func() (io.ReadCloser, error) { return os.Open(snap) }
		var err error
		got, err = c.Upload(ctx, id, requestID, name, mimeType, open, size)
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
		s.snaps.drop(a.snap)
		a.snap = ""
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

// RemoveAttachment drops chip key of session id, stops its upload, and
// removes its snapshot.
func (s *Store) RemoveAttachment(id, key int64) {
	s.attachments[id] = slices.DeleteFunc(s.attachments[id], func(a *attachment) bool {
		if a.Key != key {
			return false
		}
		if a.cancel != nil {
			a.cancel()
		}
		if a.snap != "" {
			s.snaps.drop(a.snap)
			a.snap = ""
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

// snapshots are the attached files' copies on disk, which Close and a
// sign-out remove. A copy is made off the UI thread, so its mutex, not the
// UI thread, guards them: a copy that ends after Close removes itself
// (Codex review of #49, round 5).
type snapshots struct {
	mu     sync.Mutex
	paths  map[string]bool
	closed bool
}

// keep notes path, and says whether it may stay: once the store closed, it
// is removed at once.
func (s *snapshots) keep(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = os.Remove(path)
		return false
	}
	if s.paths == nil {
		s.paths = map[string]bool{}
	}
	s.paths[path] = true
	return true
}

// drop removes path.
func (s *snapshots) drop(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.paths, path)
	_ = os.Remove(path)
}

// removeAll removes every copy, and every later one at once when closed.
func (s *snapshots) removeAll(closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path := range s.paths {
		_ = os.Remove(path)
	}
	s.paths = nil
	s.closed = s.closed || closed
}
