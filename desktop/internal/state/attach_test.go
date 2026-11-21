package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	rows := BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: text, Images: 1}}, proto.Live{}, nil)
	if len(rows) != 1 || rows[0].Text != "Look." || rows[0].Images != 1 || len(rows[0].Files) != 1 ||
		rows[0].Files[0] != (RowFile{Name: "shot (1).png", Path: path}) {
		t.Fatalf("rows %+v", rows)
	}
	rows = BuildRows(nil, proto.Live{Queued: []proto.QueuedMessage{{SubmissionID: 4, Text: text}}}, nil)
	if len(rows) != 1 || !rows[0].Queued || rows[0].Text != "Look." || len(rows[0].Files) != 1 {
		t.Fatalf("queued rows %+v", rows)
	}
	rows = BuildRows([]proto.Entry{{ID: 1, Kind: proto.EntryUser, Text: "plain"}}, proto.Live{}, nil)
	if rows[0].Text != "plain" || rows[0].Files != nil {
		t.Fatalf("plain row %+v", rows[0])
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
// release.
type blockingReader struct {
	reading, release chan struct{}
	once             bool
	r                io.Reader
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.reading)
		<-b.release
	}
	return b.r.Read(p)
}

// Closing the store removes the attached files' copies, one kept for a
// retry and one still being made, whose answer to the UI thread never runs
// once the window closed: none outlives the store (Codex review of #49,
// round 5).
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
	copies := func() int {
		entries, err := os.ReadDir(h.store.tempDir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	// An upload of the copy still being made would wait here.
	defer h.srv.HoldAnswerOnce(http.MethodPost, path)()
	reading, release := make(chan struct{}), make(chan struct{})
	h.store.Attach(1, "slow.txt", Source{Open: func() (io.ReadCloser, int64, error) {
		return io.NopCloser(&blockingReader{reading: reading, release: release, r: strings.NewReader("slow")}), 4, nil
	}})
	<-reading
	if n := copies(); n != 2 {
		t.Fatalf("%d copies before Close, want 2", n)
	}
	closed.Store(true)
	h.store.Close()
	if n := copies(); n != 1 {
		t.Fatalf("%d copies right after Close, want the one still being made", n)
	}
	close(release)
	h.loop.RunUntil(t, func() bool { return copies() == 0 })
}
