package api

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// eyes is a model that takes images (Model.Images).
var eyes = proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-eyes"}, Name: "Faux Eyes", ThinkingLevels: []string{"off"}, Images: true}

// pngOf is a w×h PNG.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (h *harness) upload(session int64, requestID, name, contentType string, data []byte) (proto.Upload, error) {
	return h.client.Upload(context.Background(), session, requestID, name, contentType, bytes.NewReader(data), int64(len(data)))
}

// uploadedFiles are the files of session's uploads folder.
func (h *harness) uploadedFiles(session int64) []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.server.uploads.Base(), strconv.FormatInt(session, 10)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// An upload stores its body as a file of the session's uploads folder,
// under its safe name, and answers where it is; a retry answers the same
// upload and stores no other, and its request id is the upload's alone
// (rules 5 and 10a).
func TestAnUploadIsStoredOnceARequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	other, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c2", Cwd: h.work, Model: model})

	up, err := h.upload(session.ID, "u1", "../../a, b (1).txt", "Text/Plain; charset=utf-8", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if up.ID == 0 || up.SessionID != session.ID || up.Name != "a_ b (1).txt" || up.MimeType != "text/plain" || up.Size != 5 ||
		!strings.HasPrefix(up.Path, filepath.Join(h.server.uploads.Base(), strconv.FormatInt(session.ID, 10))+"/") ||
		!strings.HasSuffix(up.Path, "-a_ b (1).txt") {
		t.Fatalf("upload %+v", up)
	}
	if b, err := os.ReadFile(up.Path); err != nil || string(b) != "hello" {
		t.Fatalf("the file holds %q, %v", b, err)
	}
	again, err := h.upload(session.ID, "u1", "other.txt", "", []byte("other"))
	if err != nil || again != up {
		t.Fatalf("a retry answered %+v, %v (want %+v)", again, err, up)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 1 {
		t.Fatalf("a retry stored another file: %q", files)
	}
	_, err = h.upload(other.ID, "u1", "x.txt", "", []byte("x"))
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "u1", Text: "hi"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)

	// The type is the name's when none is declared, and an empty file is
	// a file.
	empty, err := h.upload(session.ID, "u2", "shot.PNG", "", nil)
	if err != nil || empty.MimeType != "image/png" || empty.Size != 0 {
		t.Fatalf("empty upload %+v, %v", empty, err)
	}
	_, err = h.upload(session.ID, "", "x.txt", "", []byte("x"))
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	_, err = h.upload(42, "u3", "x.txt", "", []byte("x"))
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}

// An upload over 50 MiB is 413, by its Content-Length or as it is read,
// and one cut off is bad_request: neither leaves a file or a record, so
// the request id is free for the upload tried again (rule 10a).
func TestAnUploadThatFailsLeavesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	post := func(requestID string, body io.Reader, length int64) *http.Response {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, base+proto.Path(proto.PathUploads, session.ID)+"?name=big.bin&requestId="+requestID, body)
		request.ContentLength = length
		request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		request.Header.Set("Authorization", "Bearer "+h.token)
		response, err := h.http.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response
	}
	if r := post("big", strings.NewReader("x"), proto.MaxUpload+1); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a Content-Length over the limit: %d", r.StatusCode)
	}
	// A body of unknown length that runs past the limit.
	if r := post("big", io.LimitReader(zeros{}, proto.MaxUpload+1), -1); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the limit: %d", r.StatusCode)
	}
	if r := post("cut", io.MultiReader(strings.NewReader("half"), cutOff{}), -1); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body cut off: %d", r.StatusCode)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 0 {
		t.Fatalf("failed uploads left %q", files)
	}
	if _, err := h.upload(session.ID, "cut", "whole.txt", "", []byte("whole")); err != nil {
		t.Fatalf("the upload tried again: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type cutOff struct{}

func (cutOff) Read([]byte) (int, error) { return 0, errors.New("the connection went") }

// A message names each attachment by its path, name, type and size, after
// its text, as proto writes it; its images reach a model that takes
// images, and only such a model. With an attachment the text may be
// empty; an attachment must be an upload of the session (rule 10a).
func TestAMessageCarriesItsAttachments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	blind, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	seeing, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c2", Cwd: h.work, Model: eyes.ModelRef})
	shot := pngOf(t, 4, 3)

	log, err := h.upload(blind.ID, "u1", "build.log", "", []byte("FAIL"))
	if err != nil {
		t.Fatal(err)
	}
	pic, err := h.upload(blind.ID, "u2", "shot.png", "image/png", shot)
	if err != nil {
		t.Fatal(err)
	}
	device := "u:" + mustDevice(t, h).ID + ":"
	if _, err := h.client.Send(ctx, blind.ID, proto.SendMessage{RequestID: "m1", Text: "Why?", Attachments: []int64{pic.ID, log.ID, pic.ID}}); err != nil {
		t.Fatal(err)
	}
	want := proto.WithAttachments("Why?", []proto.AttachedFile{
		{Path: pic.Path, Name: "shot.png", MimeType: "image/png", Size: int64(len(shot))},
		{Path: log.Path, Name: "build.log", MimeType: "text/plain", Size: 4},
	})
	if got := h.engine.texts[device+"m1"]; got != want {
		t.Fatalf("the message is\n%q\nwant\n%q", got, want)
	}
	if images := h.engine.images[device+"m1"]; len(images) != 0 {
		t.Fatalf("a model that takes no images got %d", len(images))
	}

	// The model that takes images gets the image, as it is, after the text;
	// files alone make the message.
	pic2, err := h.upload(seeing.ID, "u3", "shot.png", "image/png", shot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, seeing.ID, proto.SendMessage{RequestID: "m2", Attachments: []int64{pic2.ID}}); err != nil {
		t.Fatal(err)
	}
	if got, want := h.engine.texts[device+"m2"], proto.AttachmentsHeading+"\n- "+pic2.Path+" (shot.png, image/png, "+strconv.Itoa(len(shot))+" bytes)"; got != want {
		t.Fatalf("files alone: %q, want %q", got, want)
	}
	if images := h.engine.images[device+"m2"]; len(images) != 1 || images[0].MimeType != "image/png" || !bytes.Equal(images[0].Data, shot) {
		t.Fatalf("the model got %+v", images)
	}

	// Another session's upload, an id that is none, and no text without
	// an attachment are refused, and send nothing.
	for _, bad := range []proto.SendMessage{
		{RequestID: "m3", Text: "x", Attachments: []int64{pic2.ID}},
		{RequestID: "m4", Text: "x", Attachments: []int64{999}},
		{RequestID: "m5", Text: "  "},
	} {
		_, err := h.client.Send(ctx, blind.ID, bad)
		asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
		if _, sent := h.engine.texts[device+bad.RequestID]; sent {
			t.Fatalf("%s went", bad.RequestID)
		}
	}
}

// An image the model cannot get goes without its image, its line saying
// why: one whose file an agent swapped for a link out of its folder is
// never read through it, and one that is no image is not shown (rule
// 10a).
func TestAnImageThatCannotGoIsNamedWithWhy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: eyes.ModelRef})
	device := "u:" + mustDevice(t, h).ID + ":"

	pic, err := h.upload(session.ID, "u1", "shot.png", "image/png", pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(secret, []byte(`{"anthropic":"sk-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pic.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, pic.Path); err != nil {
		t.Fatal(err)
	}
	fake, err := h.upload(session.ID, "u2", "fake.png", "image/png", []byte("not a png at all"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: []int64{pic.ID, fake.ID}}); err != nil {
		t.Fatal(err)
	}
	text := h.engine.texts[device+"m1"]
	words, files := proto.SplitAttachments(text)
	if words != "See." || len(files) != 2 ||
		files[0].Note != "[Image: not shown to you: its file is no regular file.]" ||
		files[1].Note != "[Image: not shown to you: it is not an image pocketd can decode.]" {
		t.Fatalf("the message %q read as %q, %+v", text, words, files)
	}
	if len(h.engine.images[device+"m1"]) != 0 || strings.Contains(text, "sk-secret") {
		t.Fatalf("the model got %+v with %q", h.engine.images[device+"m1"], text)
	}
}

// A worker's session and a cross-review's take no upload, as they take no
// message (rule 10a).
func TestAWorkerOrAReviewTakesNoUpload(t *testing.T) {
	wh, thread := workerHarness(t)
	worker := startedWorker(t, wh, thread)
	_, err := wh.upload(worker, "u", "x.txt", "", []byte("x"))
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if files := wh.uploadedFiles(worker); len(files) != 0 {
		t.Fatalf("the worker's upload left %q", files)
	}

	rh := newReviewHarness(t)
	_, err = rh.upload(rh.teg.Session, "u", "x.txt", "", []byte("x"))
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// The images of a message go to the model they were chosen for: a change
// of the session's model from another device waits until the message
// went (Codex review of #49, round 1).
func TestImagesGoToTheModelTheyWereChosenFor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: eyes.ModelRef})
	pic, err := h.upload(session.ID, "u1", "shot.png", "image/png", pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	// As the message reads the session's model, another device changes it
	// to one that takes no images; the message waits for nothing more than
	// the change to be on its way, or done.
	var armed atomic.Bool
	armed.Store(true)
	changed := make(chan error, 1)
	var sentTo proto.ModelRef
	var mu sync.Mutex
	h.engine.mu.Lock()
	h.engine.onSend = func(id int64, _ string) {
		if id == session.ID {
			choice, _ := h.engine.sessionModel(ctx, id)
			mu.Lock()
			sentTo = choice.Model
			mu.Unlock()
		}
	}
	h.engine.standing.afterSessionModel = func(int64) {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err := h.client.ChangeModel(ctx, session.ID, proto.ChangeModel{RequestID: "m", Model: &model})
			changed <- err
		}()
		waiting := func() bool {
			h.server.modelLocks.mu.Lock()
			defer h.server.modelLocks.mu.Unlock()
			l := h.server.modelLocks.held[strconv.FormatInt(session.ID, 10)]
			return l != nil && l.users == 2
		}
		for !waiting() {
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	}
	h.engine.mu.Unlock()
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "s1", Text: "See.", Attachments: []int64{pic.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	images := h.engine.images["u:"+mustDevice(t, h).ID+":s1"]
	mu.Lock()
	defer mu.Unlock()
	if len(images) != 1 || sentTo != eyes.ModelRef {
		t.Fatalf("%d images went while the session ran %+v", len(images), sentTo)
	}
}

// A retry of a message that went answers as it did, without reading its
// files or the session's model again (Codex review of #49, round 1).
func TestARetryOfAMessageThatWentReadsNothingAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: eyes.ModelRef})
	pic, err := h.upload(session.ID, "u1", "shot.png", "image/png", pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	message := proto.SendMessage{RequestID: "s1", Text: "See.", Attachments: []int64{pic.ID}}
	sent, err := h.client.Send(ctx, session.ID, message)
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = errors.New("the models cannot be read")
	h.engine.mu.Unlock()
	if err := os.Remove(pic.Path); err != nil {
		t.Fatal(err)
	}
	again, err := h.client.Send(ctx, session.ID, message)
	if err != nil || again != sent {
		t.Fatalf("the retry answered %+v, %v (want %+v)", again, err, sent)
	}
}

// An upload a stop of pocketd cut off between its file and its record is
// dropped, its file with it: by the upload tried again, which keeps one
// file, and by the next start's first delivery pass (Codex review of #49,
// round 1).
func TestAnUploadAStopCutOffIsDropped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.server.afterUploadWrite = func() bool { return true }
	for _, id := range []string{"u1", "u2"} {
		_, err := h.upload(session.ID, id, id+".txt", "", []byte("cut "+id))
		asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 2 {
		t.Fatalf("the stopped uploads left %q", files)
	}
	h.server.afterUploadWrite = nil

	// u1 is tried again: its cut file goes, and its whole one stays.
	up, err := h.upload(session.ID, "u1", "u1.txt", "", []byte("whole u1"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(up.Path); err != nil || string(b) != "whole u1" {
		t.Fatalf("the upload holds %q, %v", b, err)
	}

	// u2 is never tried again: the next start drops it.
	h.server.releaseMu.Lock()
	h.server.startedAt, h.server.uploadsDue = h.clock.Now().Add(time.Minute), true
	h.server.releaseMu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 1 || filepath.Join(filepath.Dir(up.Path), files[0]) != up.Path {
		t.Fatalf("the uploads folder holds %q, want %s alone", files, up.Path)
	}
	if _, found, err := h.store.LookupRequest(ctx, "u:"+mustDevice(t, h).ID+":u2", "upload:"+strconv.FormatInt(session.ID, 10)); err != nil || found {
		t.Fatalf("u2's request id is held: %v, %v", found, err)
	}
}

// A name cut to its safe length keeps its end in the file's name, so the
// message names it as its record does, and the desktop reads it back
// (Codex review of #49, round 1).
func TestALongDotNameIsTheFilesEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	up, err := h.upload(session.ID, "u1", "."+strings.Repeat("x", 130), "", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(up.Path, "-"+up.Name) {
		t.Fatalf("the file %s does not end in -%s", up.Path, up.Name)
	}
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "s1", Attachments: []int64{up.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, files := proto.SplitAttachments(h.engine.texts["u:"+mustDevice(t, h).ID+":s1"]); len(files) != 1 || files[0].Name != up.Name {
		t.Fatalf("the message names %+v", files)
	}
}
