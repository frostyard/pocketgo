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
	// Retry says Try Again can help a chip that failed: it has a copy to
	// upload, or a source to copy (RetryAttachment).
	Retry bool
}

// attachment is a chip, what it uploads from, and the stop of its upload.
// Its file is copied once, as it is attached, into a snapshot of the
// desktop's own (snap), with its bytes' digest and its type: every try
// uploads those bytes, under a request id kept for them, whatever the file
// becomes (Codex review of #49, round 4).
type attachment struct {
	Attachment
	open   Source
	snap   *snapFile
	size   int64
	digest string
	mime   string
	cancel context.CancelFunc
	// held says the chip holds one of MaxAttachWork slots, and active
	// that its turn runs; dropAnswer settles its upload's answer as
	// dropped, with a sign-in that ended.
	held, active bool
	dropAnswer   func()
}

// Source is what a file uploads from: Open gives its bytes and their
// number.
type Source struct {
	Open func() (io.ReadCloser, int64, error)
	// fixed is the number of bytes of a source that never changes, as
	// pasted bytes (BytesSource); -1 or 0 for one that may, as a file.
	fixed int64
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
	}, fixed: int64(len(data))}
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
func snapshot(life context.Context, snaps *snapshots, src Source, dir, name string) (snap *snapFile, size int64, digest, mimeType string, err error) {
	r, said, err := src.Open()
	if err != nil {
		return nil, 0, "", "", err
	}
	defer func() { _ = r.Close() }()
	if said > proto.MaxUpload {
		return nil, 0, "", "", ErrTooLarge
	}
	out, err := snaps.create(dir)
	if err != nil {
		return nil, 0, "", "", err
	}
	head := &headWriter{}
	sum := sha256.New()
	size, err = io.Copy(io.MultiWriter(out.file, sum, head), io.LimitReader(lifeReader{life, r}, proto.MaxUpload+1))
	if err == nil && size > proto.MaxUpload {
		err = ErrTooLarge
	}
	snaps.release(out) // written
	if err != nil {
		snaps.drop(out)
		return nil, 0, "", "", err
	}
	return out, size, hex.EncodeToString(sum.Sum(nil)), UploadType(name, head.buf), nil
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
	// Bytes that never change and are over the limit never fit: the chip
	// fails at once and keeps none of them, and offers no Try Again (Codex
	// review of #49, round 7).
	if open.fixed > proto.MaxUpload {
		a.open, a.Err = Source{}, ErrTooLarge.Error()
		return
	}
	s.runAttach(id, a, s.prepare)
}

// prepare copies chip a's file into its snapshot, off the UI thread, and
// uploads it, while the chip is there and the sign-in is the one that
// asked.
func (s *Store) prepare(id int64, a *attachment, done func()) {
	a.Uploading, a.Err = true, ""
	gen, dir, name, src := s.gen, s.tempDir, a.Name, a.open
	life, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go func() {
		snap, size, digest, mimeType, err := snapshot(life, &s.snaps, src, dir, name)
		s.opts.Post(func() {
			if gen != s.gen || life.Err() != nil || !slices.Contains(s.attachments[id], a) {
				if snap != nil {
					s.snaps.drop(snap)
				}
				done()
				return
			}
			if err != nil {
				a.Uploading, a.Err = false, err.Error()
				// A paste's bytes are kept for no Try Again: the owner
				// pastes again, so a full or unwritable temporary folder
				// keeps none of them (Codex review of #49, round 12).
				if a.open.fixed > 0 {
					a.open = Source{}
					a.Err += ": paste it again"
				}
				done()
				return
			}
			// Every try uploads the copy: what it was copied from, a pasted
			// image's bytes included, is let go (Codex review of #49, round
			// 6).
			a.snap, a.size, a.digest, a.mime, a.open = snap, size, digest, mimeType, Source{}
			s.upload(id, a, done)
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
			if a.snap == nil {
				if a.open.Open == nil {
					continue // nothing it could upload
				}
				s.runAttach(id, a, s.prepare)
			} else {
				s.runAttach(id, a, s.upload)
			}
		}
	}
}

// upload sends chip a's snapshot to pocketd, under its chip's request id,
// kept for its name and its bytes' digest until the upload succeeded; the
// snapshot goes then.
func (s *Store) upload(id int64, a *attachment, done func()) {
	a.Uploading, a.Err = true, ""
	name, snap, size, mimeType := a.Name, a.snap, a.size, a.mime
	key := fmt.Sprintf("upload:%d:%d", id, a.Key)
	requestID := s.ids.For(key, name+"\x00"+a.digest)
	life, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	var got proto.Upload
	// The turn ends once its upload let its file go and its answer is
	// settled: taken, or dropped with its sign-in, a 401's sign-out
	// included (dropAnswer), which doWithin's answer never tells (Codex
	// review of #49, rounds 11 and 12).
	ioDone, answered := false, false
	end := func() {
		if ioDone && answered {
			done()
		}
	}
	a.dropAnswer = func() { answered = true; end() }
	s.doWithin("Uploading "+name, UploadTimeout, func(ctx context.Context, c *proto.Client) error {
		defer s.opts.Post(func() { ioDone = true; end() })
		ctx, stop := context.WithCancel(ctx)
		defer stop()
		defer context.AfterFunc(life, stop)()
		if err := life.Err(); err != nil {
			return err
		}
		open := func() (io.ReadCloser, error) { return s.snaps.reader(snap, size) }
		var err error
		got, err = c.Upload(ctx, id, requestID, name, mimeType, open, size)
		return err
	}, func(err error) {
		cancel()
		defer func() {
			a.dropAnswer = nil
			answered = true
			end()
		}()
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
		a.snap = nil
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
		chip := a.Attachment
		chip.Retry = a.Err != "" && !a.Uploading && (a.snap != nil || a.open.Open != nil)
		out = append(out, chip)
	}
	return out
}

// RemoveAttachment drops chip key of session id, stops its upload, and
// removes its snapshot.
func (s *Store) RemoveAttachment(id, key int64) {
	var removed []*attachment
	s.attachments[id] = slices.DeleteFunc(s.attachments[id], func(a *attachment) bool {
		if a.Key != key {
			return false
		}
		if a.cancel != nil {
			a.cancel()
		}
		if a.snap != nil {
			s.snaps.drop(a.snap)
			a.snap = nil
		}
		a.open = Source{}
		// A turn under way keeps its slot until its copy or upload ends,
		// which may hold files open a while yet (Codex review of #49,
		// round 10); a copy kept for Try Again frees it now.
		if !a.active {
			s.releaseAttach(a)
		}
		s.ids.Done(fmt.Sprintf("upload:%d:%d", id, key))
		removed = append(removed, a)
		return true
	})
	// A chip removed while it waited leaves the line, with what it would
	// have uploaded (Codex review of #49, round 9).
	s.attachLine = slices.DeleteFunc(s.attachLine, func(u attachUnit) bool {
		if slices.Contains(removed, u.a) {
			s.attachWaiting -= u.fixed
			return true
		}
		return false
	})
	s.nextAttachWork()
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

// MaxAttachWork is how many attached files hold a file open at once: one
// being copied or uploaded, and one whose upload failed, which keeps its
// copy for Try Again. A large choice of files waits its turn, its chips
// shown uploading, rather than open them all; a Try Again of a kept copy,
// which holds its slot, and a removal always go ahead (Codex review of
// #49, rounds 8 and 9).
const MaxAttachWork = 4

// attachUnit is a file's turn: its copy and upload, or its upload alone.
type attachUnit struct {
	id   int64
	a    *attachment
	work func(id int64, a *attachment, done func())
	// fixed are the pasted bytes it keeps waiting (Source.fixed).
	fixed int64
}

// runAttach gives attachment a of session id its turn: work starts at once
// when a holds a slot or one is free, else waits in line; its chip shows
// it uploading either way. work calls done once, as its turn ends, after
// it set a's state.
func (s *Store) runAttach(id int64, a *attachment, work func(id int64, a *attachment, done func())) {
	// Pasted bytes wait in memory: those waiting their turn take at most
	// PastedWaitingLimit, past which a paste fails at once and keeps
	// none of its bytes (Codex review of #49, round 11).
	if waits := !a.held && s.attachHeld >= MaxAttachWork; waits && a.open.fixed > 0 && s.attachWaiting+a.open.fixed > PastedWaitingLimit {
		a.open, a.Uploading, a.Err = Source{}, false, ErrTooMuchWaiting.Error()
		return
	}
	a.Uploading, a.Err = true, ""
	s.attachLine = append(s.attachLine, attachUnit{id: id, a: a, work: work, fixed: a.open.fixed})
	s.attachWaiting += a.open.fixed
	s.nextAttachWork()
}

// PastedWaitingLimit is the most bytes pasted files waiting their turn
// keep in memory, twice a file's limit.
var PastedWaitingLimit int64 = 2 * proto.MaxUpload

// ErrTooMuchWaiting is a paste while pasted files waiting their turn hold
// PastedWaitingLimit.
var ErrTooMuchWaiting = errors.New("too many pasted files wait to upload: remove some, or paste again once they have uploaded")

// nextAttachWork starts the turns in line, in order, while slots are free,
// and every turn whose file holds its slot.
func (s *Store) nextAttachWork() {
	for i := 0; i < len(s.attachLine); {
		u := s.attachLine[i]
		if !u.a.held && s.attachHeld >= MaxAttachWork {
			i++
			continue
		}
		// slices.Delete clears the slot it frees, so the line keeps no
		// file it let go (Codex review of #49, round 9).
		s.attachLine = slices.Delete(s.attachLine, i, i+1)
		s.attachWaiting -= u.fixed
		if !u.a.held {
			u.a.held = true
			s.attachHeld++
		}
		u.a.active = true
		gen, ended := s.attachGen, false
		u.work(u.id, u.a, func() {
			if ended {
				return
			}
			ended = true
			u.a.active = false
			// A turn of an earlier sign-in kept its slot while its files
			// were open: it frees it now (Codex review of #49, round 11).
			if gen != s.attachGen {
				if u.a.held {
					u.a.held = false
					s.attachHeld--
				}
				s.nextAttachWork()
				return
			}
			// A copy kept for Try Again keeps its slot.
			if u.a.snap == nil {
				s.releaseAttach(u.a)
			}
			s.nextAttachWork()
		})
		i = 0 // the turn may have changed the line
	}
}

// releaseAttach frees the slot a holds.
func (s *Store) releaseAttach(a *attachment) {
	if a.held {
		a.held = false
		s.attachHeld--
	}
}

// snapshots are the attached files' copies. A copy is a file with no name
// on disk from the moment it is made (createSnapshot), so nothing of it
// outlives the process, a crash included; it is read through its one open
// handle (snapshots.reader), and closed once it is dropped and nothing
// writes or reads it. A copy is made and read off the UI thread, so the
// mutex, not the UI thread, guards them. Once the store closed, no copy is
// made (Codex review of #49, rounds 5 to 7).
type snapshots struct {
	mu     sync.Mutex
	files  map[*snapFile]bool
	closed bool
}

// snapFile is a copy: its open file, how many write or read it, and
// whether it was dropped.
type snapFile struct {
	file    *snapshotBacking
	using   int
	dropped bool
}

// errStoreClosed is a copy asked for once the store closed.
var errStoreClosed = errors.New("the window closed")

// create makes a copy in dir, for its writer, who releases it once
// written.
func (s *snapshots) create(dir string) (*snapFile, error) {
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
		s.files = map[*snapFile]bool{}
	}
	sf := &snapFile{file: f, using: 1}
	s.files[sf] = true
	return sf, nil
}

// reader reads the first size bytes of copy sf, while it is one not
// dropped; its Close releases it.
func (s *snapshots) reader(sf *snapFile, size int64) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.files[sf] || sf.dropped {
		return nil, errors.New("the attached file's copy is gone")
	}
	sf.using++
	return &snapReader{SectionReader: io.NewSectionReader(sf.file, 0, size), snaps: s, sf: sf}, nil
}

// snapReader reads a copy; Close releases it.
type snapReader struct {
	*io.SectionReader
	snaps *snapshots
	sf    *snapFile
	once  sync.Once
}

func (r *snapReader) Close() error {
	r.once.Do(func() { r.snaps.release(r.sf) })
	return nil
}

// release notes a writer or reader of sf is done, and closes it when it
// was dropped and nothing else uses it.
func (s *snapshots) release(sf *snapFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sf.using--
	if sf.dropped && sf.using == 0 {
		s.closeLocked(sf)
	}
}

// drop lets copy sf go: at once, or as its last writer or reader is done.
func (s *snapshots) drop(sf *snapFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files[sf] {
		sf.dropped = true
		if sf.using == 0 {
			s.closeLocked(sf)
		}
	}
}

// removeAll drops every copy, and makes none after it when closed.
func (s *snapshots) removeAll(closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sf := range s.files {
		sf.dropped = true
		if sf.using == 0 {
			s.closeLocked(sf)
		}
	}
	s.closed = s.closed || closed
}

// closeLocked closes copy sf, which goes with its handle. s.mu is held.
func (s *snapshots) closeLocked(sf *snapFile) {
	_ = sf.file.Close()
	delete(s.files, sf)
}
