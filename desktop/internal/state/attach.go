package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// proto.MaxUpload bytes (Codex review of #49, round 5). The copy is one of
// snaps from the moment it is made, so Close removes it while it is still
// being written, and the copy stops as life ends (Codex review of #49,
// round 6).
func snapshot(life context.Context, snaps *snapshots, src Source, dir, name string) (path string, size int64, digest, mimeType string, err error) {
	r, said, err := src.Open()
	if err != nil {
		return "", 0, "", "", err
	}
	defer func() { _ = r.Close() }()
	if said > proto.MaxUpload {
		return "", 0, "", "", ErrTooLarge
	}
	out, err := snaps.create(dir)
	if err != nil {
		return "", 0, "", "", err
	}
	head := &headWriter{}
	sum := sha256.New()
	size, err = io.Copy(io.MultiWriter(out, sum, head), io.LimitReader(lifeReader{life, r}, proto.MaxUpload+1))
	if err == nil && size > proto.MaxUpload {
		err = ErrTooLarge
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		snaps.drop(out.Name())
		return "", 0, "", "", err
	}
	return out.Name(), size, hex.EncodeToString(sum.Sum(nil)), UploadType(name, head.buf), nil
}

// lifeReader reads r until life ends.
type lifeReader struct {
	life context.Context
	r    io.Reader
}

func (l lifeReader) Read(p []byte) (int, error) {
	if err := l.life.Err(); err != nil {
		return 0, err
	}
	return l.r.Read(p)
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
		path, size, digest, mimeType, err := snapshot(life, &s.snaps, src, dir, name)
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
			// Every try uploads the copy: what it was copied from, a pasted
			// image's bytes included, is let go (Codex review of #49, round
			// 6).
			a.snap, a.size, a.digest, a.mime, a.open = path, size, digest, mimeType, Source{}
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
		open := func() (io.ReadCloser, error) { return s.snaps.open(snap) }
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
// sign-out remove. A copy is made and read off the UI thread, so its
// mutex, not the UI thread, guards them. Each is one of them from the
// moment it is made, with the handles open on it: a copy dropped goes at
// once, and one the system keeps while a handle is open goes as its last
// handle closes. Once the store closed, no copy is made (Codex review of
// #49, rounds 5 and 6).
type snapshots struct {
	mu     sync.Mutex
	files  map[string]*snapFile
	closed bool
	// remove removes the copy at path with open handles on it (os.Remove);
	// tests refuse one in use, as Windows does without delete sharing.
	remove func(path string, open int) error
}

// snapFile is a copy: the handles open on it, and whether it was dropped.
type snapFile struct {
	open    int
	dropped bool
}

// errStoreClosed is a copy asked for once the store closed.
var errStoreClosed = errors.New("the window closed")

// create makes a copy in dir, open for writing.
func (s *snapshots) create(dir string) (*snapHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errStoreClosed
	}
	f, err := createSnapshot(dir)
	if err != nil {
		return nil, err
	}
	if s.files == nil {
		s.files = map[string]*snapFile{}
	}
	s.files[f.Name()] = &snapFile{open: 1}
	return &snapHandle{File: f, snaps: s}, nil
}

// open opens the copy at path for reading, while it is one not dropped.
func (s *snapshots) open(path string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[path]
	if f == nil || f.dropped {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	in, err := openSnapshot(path)
	if err != nil {
		return nil, err
	}
	f.open++
	return &snapHandle{File: in, snaps: s}, nil
}

// snapHandle is a handle open on a copy; Close lets the copy go once it
// was dropped and no other handle is open.
type snapHandle struct {
	*os.File
	snaps *snapshots
	once  sync.Once
}

func (h *snapHandle) Close() error {
	err := os.ErrClosed
	h.once.Do(func() {
		err = h.File.Close()
		h.snaps.release(h.Name())
	})
	return err
}

// release notes a handle on path closed, and removes the copy when it was
// dropped.
func (s *snapshots) release(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[path]; f != nil {
		f.open--
		if f.dropped {
			s.removeLocked(path, f)
		}
	}
}

// drop removes the copy at path, or, while the system keeps it, as its
// last handle closes.
func (s *snapshots) drop(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[path]; f != nil {
		f.dropped = true
		s.removeLocked(path, f)
	}
}

// removeAll drops every copy, and makes none after it when closed.
func (s *snapshots) removeAll(closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, f := range s.files {
		f.dropped = true
		s.removeLocked(path, f)
	}
	s.closed = s.closed || closed
}

// removeLocked removes the copy at path, which is forgotten once it is
// gone; one the system kept is tried again as a handle closes. s.mu is
// held.
func (s *snapshots) removeLocked(path string, f *snapFile) {
	remove := s.remove
	if remove == nil {
		remove = func(path string, _ int) error { return os.Remove(path) }
	}
	if err := remove(path, f.open); err == nil || errors.Is(err, fs.ErrNotExist) {
		delete(s.files, path)
	}
}
