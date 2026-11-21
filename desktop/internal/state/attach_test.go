package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// A file attached in the composer shows as a chip at once, uploading,
// then uploaded with what pocketd answered; its upload names it and its
// type, and a message waits until every chip uploaded (ADR-0029).
func TestAnAttachedFileUploadsBeforeTheMessageGoes(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathUploads, 1))
	h.store.Attach(1, "shot.png", BytesSource([]byte("\x89PNG\r\n\x1a\nnot really")))
	got := h.store.Attachments(1)
	if len(got) != 1 || got[0].Name != "shot.png" || !got[0].Uploading || h.store.AttachmentsReady(1) || h.store.MaySend(1) {
		t.Fatalf("chips %+v, ready %v", got, h.store.AttachmentsReady(1))
	}
	var refused error
	h.store.Send(1, "look", func(err error) { refused = err })
	if refused == nil || !strings.Contains(refused.Error(), "shot.png is still uploading") {
		t.Fatalf("a message went while its file uploaded: %v", refused)
	}
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, proto.Path(proto.PathUploads, 1)) })
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.Attachments(1)[0].Uploading })
	got = h.store.Attachments(1)
	if got[0].Err != "" || got[0].Upload.ID == 0 || !h.store.AttachmentsReady(1) || !h.store.MaySend(1) {
		t.Fatalf("chips %+v", got)
	}
	ups := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathUploads, 1))
	q, _ := url.ParseQuery(ups[0].Query)
	if len(ups) != 1 || q.Get("name") != "shot.png" || q.Get("requestId") == "" || string(ups[0].Body) != "\x89PNG\r\n\x1a\nnot really" ||
		h.srv.Uploads()[0].MimeType != "image/png" {
		t.Fatalf("the upload %+v, %+v", ups, h.srv.Uploads())
	}

	// The message carries the file, and the chip leaves once pocketd took
	// it; files alone make a message.
	var sent bool
	h.store.Send(1, "", func(err error) { sent = err == nil })
	h.loop.RunUntil(t, func() bool { return !h.store.Sending(1).Busy })
	var body proto.SendMessage
	msgs := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))
	if !sent || len(msgs) != 1 || json.Unmarshal(msgs[0].Body, &body) != nil || len(body.Attachments) != 1 || body.Attachments[0] != got[0].Upload.ID {
		t.Fatalf("sent %v: %+v", sent, msgs)
	}
	if left := h.store.Attachments(1); len(left) != 0 {
		t.Fatalf("chips left after the message went: %+v", left)
	}
	detail, _ := h.srv.Session(1)
	if last := detail.Entries[len(detail.Entries)-1]; !strings.HasPrefix(last.Text, proto.AttachmentsHeading) {
		t.Fatalf("the message is %q", last.Text)
	}
}

// A chip whose upload failed holds the message until the owner removes
// it, and removing a chip stops its upload and leaves the message free;
// a message tried again with other files is a new request.
func TestAFailedUploadHoldsTheMessageUntilRemoved(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.srv.FailOnce(http.MethodPost, proto.Path(proto.PathUploads, 1), http.StatusInternalServerError)
	h.store.Attach(1, "a.txt", BytesSource([]byte("a")))
	h.loop.RunUntil(t, func() bool { return !h.store.Attachments(1)[0].Uploading })
	failed := h.store.Attachments(1)[0]
	if failed.Err == "" || h.store.AttachmentsReady(1) {
		t.Fatalf("chip %+v", failed)
	}
	var refused error
	h.store.Send(1, "hi", func(err error) { refused = err })
	if refused == nil || !strings.Contains(refused.Error(), "a.txt did not upload") {
		t.Fatalf("a message went with a failed file: %v", refused)
	}
	h.store.RemoveAttachment(1, failed.Key)
	if len(h.store.Attachments(1)) != 0 || !h.store.MaySend(1) {
		t.Fatalf("chips %+v after removing", h.store.Attachments(1))
	}

	// A chip removed while it uploads is stopped, and its answer shows
	// nowhere.
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathUploads, 1))
	h.store.Attach(1, "b.txt", BytesSource([]byte("b")))
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, proto.Path(proto.PathUploads, 1)) })
	h.store.RemoveAttachment(1, h.store.Attachments(1)[0].Key)
	release()
	h.loop.RunUntil(t, func() bool { return h.store.InFlight() == 0 })
	if len(h.store.Attachments(1)) != 0 {
		t.Fatalf("a removed chip came back: %+v", h.store.Attachments(1))
	}

	// One text with two sets of files is two requests.
	p := proto.Path(proto.PathMessages, 1)
	h.store.Attach(1, "c.txt", BytesSource([]byte("c")))
	h.loop.RunUntil(t, func() bool { return h.store.AttachmentsReady(1) })
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	done := false
	h.store.Send(1, "same", func(error) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	h.store.RemoveAttachment(1, h.store.Attachments(1)[0].Key)
	done = false
	h.store.Send(1, "same", func(error) { done = true })
	h.loop.RunUntil(t, func() bool { return done })
	if ids := sentIDs(t, h, p); len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("request ids %q", ids)
	}
}

// A file attaches only as a regular file of at most 50 MB, opened without
// waiting on a named pipe; its type is its extension's, else its bytes'.
func TestAFileSourceIsARegularFileWithinTheLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, size, err := FileSource(path).Open()
	if err != nil || size != 5 {
		t.Fatalf("FileSource = %d, %v", size, err)
	}
	if b, _ := io.ReadAll(r); string(b) != "hello" {
		t.Fatalf("read %q", b)
	}
	_ = r.Close()
	if _, _, err := FileSource(dir).Open(); err == nil {
		t.Fatal("a folder attached")
	}
	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(proto.MaxUpload + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, _, err := FileSource(big).Open(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a file over 50 MB: %v", err)
	}
	for _, c := range []struct{ name, head, want string }{
		{"a.png", "", "image/png"},
		{"a.pdf", "", "application/pdf"},
		{"noext", "\x89PNG\r\n\x1a\n", "image/png"},
		{"noext", "plain words", "text/plain; charset=utf-8"},
	} {
		if got := UploadType(c.name, []byte(c.head)); got != c.want {
			t.Errorf("UploadType(%q, %q) = %q, want %q", c.name, c.head, got, c.want)
		}
	}
}

// The owner's message shows its words, and its files as pocketd named
// them, with the images the model got; a message without the heading, or
// queued, shows as it is written (ADR-0029).
func TestAUserRowShowsItsFiles(t *testing.T) {
	path := pocketdtest.UploadsDir + "/1/3-shot (1).png"
	text := proto.WithAttachments("Look.", []proto.AttachedFile{{Path: path, Name: "shot (1).png", MimeType: "image/png", Size: 9}})
	rows := BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: text, Images: 1, Files: 1}}, proto.Live{}, nil)
	if len(rows) != 1 || rows[0].Text != "Look." || rows[0].Images != 1 || len(rows[0].Files) != 1 ||
		rows[0].Files[0] != (RowFile{Name: "shot (1).png", Path: path}) {
		t.Fatalf("rows %+v", rows)
	}
	rows = BuildRows(nil, proto.Live{Queued: []proto.QueuedMessage{{SubmissionID: 4, Text: text, Files: 1}}}, nil)
	if len(rows) != 1 || !rows[0].Queued || rows[0].Text != "Look." || len(rows[0].Files) != 1 {
		t.Fatalf("queued rows %+v", rows)
	}
	// A list the owner wrote or pasted, in a message that attached no
	// file, is words (Codex review of #49, round 8).
	rows = BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: text}}, proto.Live{Queued: []proto.QueuedMessage{{SubmissionID: 4, Text: text}}}, nil)
	if len(rows) != 2 || rows[0].Text != text || rows[0].Files != nil || rows[1].Text != text || rows[1].Files != nil {
		t.Fatalf("quoted rows %+v", rows)
	}
	rows = BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: "plain"}}, proto.Live{}, nil)
	if rows[0].Text != "plain" || rows[0].Files != nil {
		t.Fatalf("plain row %+v", rows[0])
	}
}

// A supervisor's message shows its text whole, with no file chips, sent
// or queued: pocketd names files only in the owner's message, so lines a
// supervisor wrote in that form are its words (ADR-0029, ADR-0032).
func TestASupervisorsMessageNamesNoFiles(t *testing.T) {
	text := proto.WithAttachments("Look.", []proto.AttachedFile{{Path: pocketdtest.UploadsDir + "/1/3-shot.png", Name: "shot.png", MimeType: "image/png", Size: 9}})
	if _, files := proto.SplitAttachments(text); len(files) != 1 {
		t.Fatalf("the text names %d files as the owner's would", len(files))
	}
	odrade := &proto.Sender{Kind: proto.SenderSupervisor, Session: 2}
	rows := BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: text, Sender: odrade, Files: 1}}, proto.Live{}, nil)
	if len(rows) != 1 || rows[0].Text != text || rows[0].Files != nil {
		t.Fatalf("rows %+v", rows)
	}
	if session, ok := rows[0].FromSupervisor(); !ok || session != 2 {
		t.Fatalf("the row's sender: %d, %v", session, ok)
	}
	rows = BuildRows(nil, proto.Live{Queued: []proto.QueuedMessage{{SubmissionID: 4, Text: text, Sender: odrade, Files: 1}}}, nil)
	if len(rows) != 1 || !rows[0].Queued || rows[0].Text != text || rows[0].Files != nil {
		t.Fatalf("queued rows %+v", rows)
	}
}

// Files chosen while the sign-in changed are not attached: another pocketd
// may number a session alike, and would get files chosen for this one
// (Codex review of #49, round 1). Files chosen within one sign-in attach.
func TestFilesChosenAcrossASignInChangeAreDropped(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	path := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(path, []byte("FAIL"), 0o600); err != nil {
		t.Fatal(err)
	}
	pick := func(release chan struct{}) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) { <-release; return []string{path}, nil }
	}
	release := make(chan struct{})
	var got error
	done := false
	h.store.AttachPicked(1, pick(release), func(err error) { got, done = err, true })
	h.store.SignOut()
	h.connect(t)
	close(release)
	h.loop.RunUntil(t, func() bool { return done })
	if !errors.Is(got, ErrSignInChanged) || len(h.store.Attachments(1)) != 0 || len(h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathUploads, 1))) != 0 {
		t.Fatalf("a choice across a sign-in change: %v, chips %+v", got, h.store.Attachments(1))
	}

	release = make(chan struct{})
	close(release)
	done = false
	h.store.AttachPicked(1, pick(release), func(err error) { got, done = err, true })
	h.loop.RunUntil(t, func() bool { return done && h.store.AttachmentsReady(1) && h.store.InFlight() == 0 })
	if got != nil || len(h.store.Attachments(1)) != 1 || h.store.Attachments(1)[0].Name != "build.log" {
		t.Fatalf("a choice within one sign-in: %v, chips %+v", got, h.store.Attachments(1))
	}
}

// A message's request id is kept for its text and its files told apart:
// a text that ends as another's files would read is another message
// (Codex review of #49, round 1).
func TestAMessagesTextAndFilesAreToldApart(t *testing.T) {
	if sendAction("x", []int64{7}) == sendAction("x\x007", nil) || sendAction("x,7", nil) == sendAction("x", []int64{7}) {
		t.Fatal("two messages share an action")
	}
	if sendAction("x", []int64{7, 8}) == sendAction("x", []int64{8, 7}) || sendAction("x", nil) == sendAction("x", []int64{7}) {
		t.Fatal("two orders, or a message with and without files, share one")
	}
}

// A chip whose upload's answer was lost uploads again with its request
// id, so pocketd answers the upload it stored, and keeps one copy (Codex
// review of #49, round 2).
func TestAFailedUploadIsTriedAgainWithItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	path := proto.Path(proto.PathUploads, 1)
	h.srv.LoseAnswerOnce(http.MethodPost, path)
	h.store.Attach(1, "a.txt", BytesSource([]byte("a")))
	h.loop.RunUntil(t, func() bool { return !h.store.Attachments(1)[0].Uploading })
	chip := h.store.Attachments(1)[0]
	if chip.Err == "" {
		t.Fatalf("chip %+v", chip)
	}
	h.store.RetryAttachment(1, chip.Key)
	h.loop.RunUntil(t, func() bool { return !h.store.Attachments(1)[0].Uploading })
	chip = h.store.Attachments(1)[0]
	ups := h.srv.Uploads()
	reqs := h.srv.RequestsTo(http.MethodPost, path)
	first, _ := url.ParseQuery(reqs[0].Query)
	second, _ := url.ParseQuery(reqs[1].Query)
	if chip.Err != "" || len(ups) != 1 || chip.Upload.ID != ups[0].ID || len(reqs) != 2 || first.Get("requestId") != second.Get("requestId") {
		t.Fatalf("chip %+v, uploads %+v, requests %+v", chip, ups, reqs)
	}
}

// The fake pocketd takes an upload's whole body, as pocketd does, and
// refuses one over proto.MaxUpload (Codex review of #49, round 2).
func TestTheFakeTakesAWholeUpload(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Attach(1, "big.bin", BytesSource(make([]byte, 2<<20)))
	h.loop.RunUntil(t, func() bool { return h.store.AttachmentsReady(1) && h.store.InFlight() == 0 })
	if ups := h.srv.Uploads(); len(ups) != 1 || ups[0].Size != 2<<20 {
		t.Fatalf("the fake stored %+v", ups)
	}
}

// An attached file is copied as it is attached, and every try uploads
// those bytes under one request id: a file changed since, even to bytes of
// its size at its time, is not what a retry sends, so pocketd never answers
// one upload for other bytes (Codex review of #49, rounds 3 and 4).
func TestARetrySendsTheBytesFirstAttached(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	file := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(file, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	path := proto.Path(proto.PathUploads, 1)
	h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
	h.store.AttachFiles(1, []string{file})
	h.loop.RunUntil(t, func() bool { a := h.store.Attachments(1); return len(a) == 1 && !a[0].Uploading })
	if err := os.WriteFile(file, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	h.store.RetryAttachment(1, h.store.Attachments(1)[0].Key)
	h.loop.RunUntil(t, func() bool { return !h.store.Attachments(1)[0].Uploading && h.store.InFlight() == 0 })
	reqs := h.srv.RequestsTo(http.MethodPost, path)
	if len(reqs) != 2 {
		t.Fatalf("%d uploads sent", len(reqs))
	}
	first, _ := url.ParseQuery(reqs[0].Query)
	second, _ := url.ParseQuery(reqs[1].Query)
	if first.Get("requestId") != second.Get("requestId") || string(reqs[0].Body) != "AAAA" || string(reqs[1].Body) != "AAAA" {
		t.Fatalf("sent %q as %s, then %q as %s", reqs[0].Body, first.Get("requestId"), reqs[1].Body, second.Get("requestId"))
	}
	if ups := h.srv.Uploads(); len(ups) != 1 || h.store.Attachments(1)[0].Upload.ID != ups[0].ID {
		t.Fatalf("uploads %+v, chip %+v", ups, h.store.Attachments(1)[0])
	}
	entries, err := os.ReadDir(h.store.tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the snapshot is left: %v, %v", entries, err)
	}
}

// A file is copied to its end, whatever size it said as it was opened: one
// that grew as it was attached uploads whole, not cut at the size it had
// (Codex review of #49, round 5).
func TestAFileThatGrewIsCopiedWhole(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Attach(1, "grown.log", Source{Open: func() (io.ReadCloser, int64, error) {
		return io.NopCloser(strings.NewReader("AAAABBBB")), 4, nil
	}})
	h.loop.RunUntil(t, func() bool { return h.store.AttachmentsReady(1) && h.store.InFlight() == 0 })
	reqs := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathUploads, 1))
	if len(reqs) != 1 || string(reqs[0].Body) != "AAAABBBB" {
		t.Fatalf("uploaded %+v", reqs)
	}
	if ups := h.srv.Uploads(); len(ups) != 1 || ups[0].Size != 8 {
		t.Fatalf("the fake stored %+v", ups)
	}
}

// blockingReader tells reading as it is first read, then waits for
// release, and gives all of data with its end in that one read, so a copy
// of it ends whole, whatever stopped meanwhile; Close tells closed.
type blockingReader struct {
	reading, release, closed chan struct{}
	once                     bool
	data                     []byte
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.reading)
		<-b.release
		return copy(p, b.data), io.EOF
	}
	return 0, io.EOF
}

func (b *blockingReader) Close() error { close(b.closed); return nil }

// Closing the store lets the attached files' copies go: one kept for a
// retry at once, and one still being written as its writer ends, whose
// answer to the UI thread never runs once the window closed. No copy has a
// name on disk at any time, so none outlives the store, and none is made
// after it (Codex review of #49, rounds 5 to 7).
func TestClosingTheStoreRemovesTheCopies(t *testing.T) {
	h := newHarness(t)
	// A closed window runs nothing posted to it.
	var closed atomic.Bool
	post := h.store.opts.Post
	h.store.opts.Post = func(fn func()) {
		if !closed.Load() {
			post(fn)
		}
	}
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	path := proto.Path(proto.PathUploads, 1)
	h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
	h.store.Attach(1, "failed.txt", BytesSource([]byte("kept for a retry")))
	h.loop.RunUntil(t, func() bool { a := h.store.Attachments(1); return len(a) == 1 && a[0].Err != "" })
	live := func() int {
		h.store.snaps.mu.Lock()
		defer h.store.snaps.mu.Unlock()
		return len(h.store.snaps.files)
	}
	named := func() int {
		entries, err := os.ReadDir(h.store.tempDir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	// An upload of the copy still being written would wait here.
	defer h.srv.HoldAnswerOnce(http.MethodPost, path)()
	slow := &blockingReader{reading: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), data: []byte("slow")}
	h.store.Attach(1, "slow.txt", Source{Open: func() (io.ReadCloser, int64, error) { return slow, 4, nil }})
	<-slow.reading
	if n, on := live(), named(); n != 2 || on != 0 {
		t.Fatalf("%d copies before Close, %d of them named on disk", n, on)
	}
	closed.Store(true)
	h.store.Close()
	if n := live(); n != 1 {
		t.Fatalf("%d copies right after Close, want the one still being written", n)
	}
	close(slow.release)
	<-slow.closed // the copy ended
	if n, on := live(), named(); n != 0 || on != 0 {
		t.Fatalf("%d copies once the copy being written ended, %d named", n, on)
	}
	if _, err := h.store.snaps.create(h.store.tempDir); err == nil {
		t.Fatal("a copy was made after Close")
	}
}

// A copy dropped as an upload still reads it, by its chip's removal, a
// sign-out or Close, stays readable to that upload and goes as it ends; a
// copy dropped opens no more (Codex review of #49, rounds 6 and 7).
func TestACopyInUseGoesAsItsLastHandleCloses(t *testing.T) {
	for _, close := range []bool{false, true} {
		var snaps snapshots
		sf, err := snaps.create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sf.file.Write([]byte("copy")); err != nil {
			t.Fatal(err)
		}
		snaps.release(sf)
		r, err := snaps.reader(sf, 4)
		if err != nil {
			t.Fatal(err)
		}
		if close {
			snaps.removeAll(true)
		} else {
			snaps.drop(sf)
		}
		if _, err := snaps.reader(sf, 4); err == nil {
			t.Fatalf("closed %v: a dropped copy opened again", close)
		}
		if data, err := io.ReadAll(r); err != nil || string(data) != "copy" {
			t.Fatalf("closed %v: the upload under way read %q, %v", close, data, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := sf.file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closed %v: the copy stayed open once its last reader ended: %v", close, err)
		}
	}
}

// A copy has no name on disk from the moment it is made: a crash of the
// desktop leaves nothing of the owner's file behind (Codex review of #49,
// round 7).
func TestACopyHasNoNameOnDisk(t *testing.T) {
	dir := t.TempDir()
	var snaps snapshots
	sf, err := snaps.create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the copy is named on disk: %v, %v", entries, err)
	}
	if _, err := sf.file.Write([]byte("copy")); err != nil {
		t.Fatal(err)
	}
	snaps.release(sf)
	r, err := snaps.reader(sf, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if data, err := io.ReadAll(r); err != nil || string(data) != "copy" {
		t.Fatalf("the copy reads %q, %v", data, err)
	}
}

// Pasted bytes over the limit never fit: the chip fails at once, keeps
// none of them, and offers no Try Again; a file over the limit may
// shrink, so its chip does (Codex review of #49, round 7).
func TestPastedBytesOverTheLimitAreKeptByNothing(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Attach(1, PastedImageName, BytesSource(make([]byte, proto.MaxUpload+1)))
	got := h.store.Attachments(1)
	if len(got) != 1 || got[0].Err != ErrTooLarge.Error() || got[0].Uploading || got[0].Retry {
		t.Fatalf("chips %+v", got)
	}
	if h.store.attachments[1][0].open.Open != nil {
		t.Fatal("the chip keeps the pasted bytes")
	}
	h.store.RetryAttachment(1, got[0].Key)
	if a := h.store.Attachments(1); a[0].Uploading || h.store.InFlight() != 0 {
		t.Fatalf("Try Again of bytes that never fit: %+v", a)
	}
}

// A pasted image's bytes are let go once its copy is made: every try
// uploads the copy, so a chip keeps no image in memory (Codex review of
// #49, round 6).
func TestAPastedImageIsLetGoOnceCopied(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	defer h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathUploads, 1))()
	h.store.Attach(1, PastedImageName, BytesSource([]byte("\x89PNG\r\n\x1a\npixels")))
	h.loop.RunUntil(t, func() bool { a := h.store.attachments[1]; return len(a) == 1 && a[0].snap != nil })
	if h.store.attachments[1][0].open.Open != nil {
		t.Fatal("the chip keeps what its copy was made from")
	}
}

// A message's request id is chosen from its text and files in time linear
// in their size: a long message with many files is not copied once a file
// (Codex review of #49, round 6).
func TestASendActionIsBuiltInOneBuffer(t *testing.T) {
	text := strings.Repeat("x", 256<<10)
	ids := make([]int64, 200)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if allocs := testing.AllocsPerRun(5, func() { _ = sendAction(text, ids) }); allocs > 2 {
		t.Fatalf("a send action took %v allocations", allocs)
	}
	if got := sendAction("a,1", []int64{2, 30}); got != "3:a,1,2,30" {
		t.Fatalf("sendAction = %q", got)
	}
}

// A queued message's preview cut short shows its text whole with its mark,
// never file chips: it may end within its files' lines, and a list cut
// short must not look whole (Codex review of #49, round 7).
func TestACutQueuedPreviewNamesNoFiles(t *testing.T) {
	text := proto.WithAttachments("Look.", []proto.AttachedFile{{Path: pocketdtest.UploadsDir + "/1/3-shot.png", Name: "shot.png", MimeType: "image/png", Size: 9}})
	rows := BuildRows(nil, proto.Live{Queued: []proto.QueuedMessage{{SubmissionID: 4, Text: text, Cut: true, Files: 1}}}, nil)
	if len(rows) != 1 || rows[0].Text != text+" …" || rows[0].Files != nil || !rows[0].Queued {
		t.Fatalf("rows %+v", rows)
	}
}

// gatedSource is a source that is held open, blocked in its first read,
// until release closes; open counts the sources open, most the most open
// at once.
type gatedSource struct {
	release    chan struct{}
	open, most atomic.Int32
}

func (g *gatedSource) source(data string) Source {
	return Source{Open: func() (io.ReadCloser, int64, error) {
		n := g.open.Add(1)
		for {
			m := g.most.Load()
			if n <= m || g.most.CompareAndSwap(m, n) {
				break
			}
		}
		return &gatedReader{g: g, r: strings.NewReader(data)}, int64(len(data)), nil
	}}
}

type gatedReader struct {
	g    *gatedSource
	r    io.Reader
	once sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	<-r.g.release
	return r.r.Read(p)
}

func (r *gatedReader) Close() error {
	r.once.Do(func() { r.g.open.Add(-1) })
	return nil
}

// Files attached at once take turns: at most MaxAttachWork are copied and
// uploaded at a time, each holding its files open, while the others wait
// their turn, their chips shown uploading; every one goes in the end
// (Codex review of #49, round 8).
func TestAttachedFilesTakeTurns(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	g := &gatedSource{release: make(chan struct{})}
	for i := range MaxAttachWork + 2 {
		h.store.Attach(1, fmt.Sprintf("f%d.txt", i), g.source(fmt.Sprintf("file %d", i)))
	}
	h.loop.RunUntil(t, func() bool { return g.open.Load() == MaxAttachWork })
	h.loop.Flush()
	if len(h.store.attachLine) != 2 || h.store.attachHeld != MaxAttachWork {
		t.Fatalf("%d waiting, %d running", len(h.store.attachLine), h.store.attachHeld)
	}
	for _, chip := range h.store.Attachments(1) {
		if !chip.Uploading {
			t.Fatalf("a chip waiting its turn shows %+v", chip)
		}
	}
	close(g.release)
	h.loop.RunUntil(t, func() bool { return h.store.AttachmentsReady(1) && h.store.InFlight() == 0 })
	if ups := h.srv.Uploads(); len(ups) != MaxAttachWork+2 || g.most.Load() > MaxAttachWork {
		t.Fatalf("%d uploads, %d sources open at most", len(ups), g.most.Load())
	}
}

// A failed upload keeps its copy, and its slot, for Try Again: files
// attached while every slot is so held wait their turn, open nothing,
// and may be removed with what they hold; a Try Again of a kept copy goes
// ahead, and its slot, once uploaded, goes to the file that waited
// (Codex review of #49, round 9).
func TestAKeptCopyHoldsItsTurn(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	path := proto.Path(proto.PathUploads, 1)
	for i := range MaxAttachWork {
		h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
		h.store.Attach(1, fmt.Sprintf("failed%d.txt", i), BytesSource([]byte("kept")))
		h.loop.RunUntil(t, func() bool { a := h.store.Attachments(1); return len(a) == i+1 && a[i].Err != "" })
	}
	var opened atomic.Int32
	waiting := Source{Open: func() (io.ReadCloser, int64, error) {
		opened.Add(1)
		return io.NopCloser(strings.NewReader("next")), 4, nil
	}}
	h.store.Attach(1, "next.txt", waiting)
	h.store.Attach(1, PastedImageName, BytesSource([]byte("pasted bytes")))
	h.loop.Flush()
	chips := h.store.Attachments(1)
	if len(h.store.attachLine) != 2 || opened.Load() != 0 || !chips[MaxAttachWork].Uploading {
		t.Fatalf("%d waiting, %d opened, chips %+v", len(h.store.attachLine), opened.Load(), chips)
	}
	pasted := h.store.attachments[1][MaxAttachWork+1]
	h.store.RemoveAttachment(1, chips[MaxAttachWork+1].Key)
	if len(h.store.attachLine) != 1 || pasted.open.Open != nil {
		t.Fatalf("the removed chip still waits: %d waiting, its source kept %v", len(h.store.attachLine), pasted.open.Open != nil)
	}
	h.store.RetryAttachment(1, chips[0].Key)
	h.loop.RunUntil(t, func() bool {
		return opened.Load() == 1 && h.store.InFlight() == 0 && !h.store.Attachments(1)[MaxAttachWork].Uploading
	})
	if a := h.store.Attachments(1); a[0].Upload.ID == 0 || a[MaxAttachWork].Upload.ID == 0 {
		t.Fatalf("chips %+v", a)
	}
}
