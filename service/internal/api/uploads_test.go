package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/uploads"
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
	return h.client.Upload(context.Background(), session, requestID, name, contentType, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, int64(len(data)))
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
// its text, as proto writes it, and carries its images, whatever model the
// session runs as it goes. With an attachment the text may be empty; an
// attachment must be an upload of the session (rule 10a).
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
	// The message carries its image whatever the model now: the one that
	// reads it, when it runs, gets it as an image when it takes images
	// (Codex review of #49, round 5).
	if images := h.engine.images[device+"m1"]; len(images) != 1 || !bytes.Equal(images[0].Data, shot) {
		t.Fatalf("the message carries %d images", len(images))
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

// An upload a stop of pocketd cut off is settled by the request tried
// again, or by the next start's first delivery pass: finished when its
// file has its name, else dropped, and nothing appears where nothing was
// named (Codex review of #49, rounds 1 and 3).
func TestAnUploadAStopCutOffIsSettled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	stops := map[string]string{"u1": "linked", "u2": "staged", "u3": "linked"}
	var current string
	h.server.uploadStop = func(step string, _ int64, _ string) bool { return stops[current] == step }
	for _, id := range []string{"u1", "u2", "u3"} {
		current = id
		_, err := h.upload(session.ID, id, id+".txt", "", []byte("first "+id))
		asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	}
	h.server.uploadStop = nil
	if files := h.uploadedFiles(session.ID); len(files) != 2 {
		t.Fatalf("the stopped uploads named %q", files)
	}

	// u1 is tried again: its file was named, so it is the upload.
	up, err := h.upload(session.ID, "u1", "u1.txt", "", []byte("second u1"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(up.Path); err != nil || string(b) != "first u1" {
		t.Fatalf("the upload holds %q, %v", b, err)
	}

	// The next start settles u2, dropped, and u3, finished.
	h.server.uploadsMu.Lock()
	h.server.run, h.server.uploadsDue = "the next run", true
	h.server.uploadsMu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 2 {
		t.Fatalf("the uploads folder holds %q", files)
	}
	kind := "upload:" + strconv.FormatInt(session.ID, 10)
	device := "u:" + mustDevice(t, h).ID + ":"
	if _, found, err := h.store.LookupRequest(ctx, device+"u2", kind); err != nil || found {
		t.Fatalf("u2's request id is held: %v, %v", found, err)
	}
	if result, found, err := h.store.LookupRequest(ctx, device+"u3", kind); err != nil || !found || result == "" {
		t.Fatalf("u3 is not finished: %q, %v, %v", result, found, err)
	}
}

// A left upload whose name holds anything but the file pocketd made is
// dropped, and what its name holds stays: pocketd removes nothing in the
// uploads folder, an entry an agent put there least of all (Codex review
// of #49, round 3).
func TestALeftUploadNeverRemovesAnAgentsFile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	stops := map[string]string{"u1": "staged", "u2": "linked"}
	var current string
	paths := map[string]string{}
	h.server.uploadStop = func(step string, id int64, file string) bool {
		paths[current] = h.server.uploads.Path(id, file)
		return stops[current] == step
	}
	for _, id := range []string{"u1", "u2"} {
		current = id
		_, err := h.upload(session.ID, id, id+".txt", "", []byte("pocketd's"))
		asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	}
	h.server.uploadStop = nil
	// An agent puts a file where u1 was to be named, and one in place of
	// u2's.
	for _, id := range []string{"u1", "u2"} {
		_ = os.Remove(paths[id])
		if err := os.WriteFile(paths[id], []byte("an agent's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.server.uploadsMu.Lock()
	h.server.run, h.server.uploadsDue = "the next run", true
	h.server.uploadsMu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u1", "u2"} {
		if b, err := os.ReadFile(paths[id]); err != nil || string(b) != "an agent's" {
			t.Fatalf("%s's name holds %q, %v", id, b, err)
		}
		if _, found, err := h.store.LookupRequest(ctx, "u:"+mustDevice(t, h).ID+":"+id, "upload:"+strconv.FormatInt(session.ID, 10)); err != nil || found {
			t.Fatalf("%s's request id is held: %v, %v", id, found, err)
		}
	}
}

// An upload another run left is settled at the next start even when the
// clock moved back, or did not move at all: runs tell them apart, not
// times (Codex review of #49, round 3).
func TestLeftUploadsAreFoundWhateverTheClock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.server.uploadStop = func(step string, _ int64, _ string) bool { return step == "staged" }
	_, err := h.upload(session.ID, "u1", "u1.txt", "", []byte("cut"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.server.uploadStop = nil
	// The next start, an hour earlier by the clock.
	h.server.uploadsMu.Lock()
	h.server.run, h.server.uploadsDue, h.server.startedAt = "the next run", true, h.clock.Now().Add(-time.Hour)
	h.server.uploadsMu.Unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.store.LookupRequest(ctx, "u:"+mustDevice(t, h).ID+":u1", "upload:"+strconv.FormatInt(session.ID, 10)); err != nil || found {
		t.Fatalf("the left upload's request id is held: %v, %v", found, err)
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

// The first pass after a start settles an upload a stop cut off only
// under its request's key, and leaves one whose key a retry holds to that
// retry: the pass never waits on an upload under way, and never drops the
// retry's claim (Codex review of #49, rounds 2 and 3).
func TestTheFirstPassLeavesARetrysUploadAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.server.uploadStop = func(step string, _ int64, _ string) bool { return step == "staged" }
	_, err := h.upload(session.ID, "u1", "u1.txt", "", []byte("cut"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	key := "u:" + mustDevice(t, h).ID + ":u1"

	// The pass reads the cut upload; the retry then settles it, claims and
	// names its own, and waits, holding the key, while the pass goes on.
	named, release := make(chan struct{}), make(chan struct{})
	h.server.uploadStop = func(step string, _ int64, _ string) bool {
		if step == "linked" {
			close(named)
			<-release
		}
		return false
	}
	retried := make(chan error, 1)
	h.server.afterUploadsRead = func() {
		go func() {
			_, err := h.upload(session.ID, "u1", "u1.txt", "", []byte("whole"))
			retried <- err
		}()
		<-named
	}
	h.server.uploadsMu.Lock()
	h.server.run, h.server.uploadsDue = "the next run", true
	h.server.uploadsMu.Unlock()
	err = h.server.DeliverPending(ctx)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-retried; err != nil {
		t.Fatalf("the retry: %v", err)
	}
	files := h.uploadedFiles(session.ID)
	if len(files) != 1 {
		t.Fatalf("the uploads folder holds %q", files)
	}
	if b, err := os.ReadFile(filepath.Join(h.server.uploads.Base(), strconv.FormatInt(session.ID, 10), files[0])); err != nil || string(b) != "whole" {
		t.Fatalf("the upload holds %q, %v", b, err)
	}
	if result, found, err := h.store.LookupRequest(ctx, key, "upload:"+strconv.FormatInt(session.ID, 10)); err != nil || !found || result == "" {
		t.Fatalf("the retry's record: %q, %v, %v", result, found, err)
	}
}

// An upload whose record fails once its file has its name is finished by
// its request tried again, which answers it: its file is not left
// unrecorded (Codex review of #49, rounds 2 and 3).
func TestAnUploadNotRecordedIsFinishedByItsRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	finish := h.server.finishUpload
	h.server.finishUpload = func(context.Context, string) (store.UploadRow, error) {
		return store.UploadRow{}, errors.New("the disk is full")
	}
	_, err := h.upload(session.ID, "u1", "a.txt", "", []byte("a"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.server.finishUpload = finish
	up, err := h.upload(session.ID, "u1", "a.txt", "", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 1 || filepath.Join(filepath.Dir(up.Path), files[0]) != up.Path {
		t.Fatalf("the uploads folder holds %q, want %s", files, up.Path)
	}
}

// An upload whose new name an entry holds already makes nothing, and
// removes nothing: the entry is not pocketd's (Codex review of #49, round
// 2).
func TestAnUploadNeverRemovesWhatItDidNotMake(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	var planted string
	h.server.uploadStop = func(step string, id int64, file string) bool {
		if step == "staged" {
			planted = h.server.uploads.Path(id, file)
			if err := os.WriteFile(planted, []byte("an agent's"), 0o600); err != nil {
				t.Error(err)
			}
		}
		return false
	}
	_, err := h.upload(session.ID, "u1", "a.txt", "", []byte("mine"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if b, err := os.ReadFile(planted); err != nil || string(b) != "an agent's" {
		t.Fatalf("the entry at the name holds %q, %v", b, err)
	}
	if _, found, err := h.store.LookupRequest(ctx, "u:"+mustDevice(t, h).ID+":u1", "upload:"+strconv.FormatInt(session.ID, 10)); err != nil || found {
		t.Fatalf("its request id is held: %v, %v", found, err)
	}
}

// A message whose files' lines would make its entry larger than an event
// carries is refused, and nothing goes (Codex review of #49, round 3).
func TestAMessageLongerThanAnEntryCarriesIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	up, err := h.upload(session.ID, "u1", "a.txt", "", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	h.server.maxComposed = 100
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "s1", Text: strings.Repeat("w", 60), Attachments: []int64{up.ID}})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if _, sent := h.engine.texts["u:"+mustDevice(t, h).ID+":s1"]; sent {
		t.Fatal("the message went")
	}
}

// A message's images stay within their budget, which a further image
// would pass: it is named alone, with why; and a message whose files'
// lines alone are too long reads no image (Codex review of #49, round 4).
func TestAMessagesImagesStayWithinTheirBudget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: eyes.ModelRef})
	shot := pngOf(t, 4, 3)
	var ups []int64
	for _, id := range []string{"u1", "u2"} {
		up, err := h.upload(session.ID, id, id+".png", "image/png", shot)
		if err != nil {
			t.Fatal(err)
		}
		ups = append(ups, up.ID)
	}
	prepared := 0
	h.server.prepareImage = func(data []byte) (uploads.Image, string, error) {
		prepared++
		return uploads.PrepareImage(data)
	}
	h.server.maxImages = base64.StdEncoding.EncodedLen(len(shot)) + 1
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "s1", Text: "See.", Attachments: ups}); err != nil {
		t.Fatal(err)
	}
	device := "u:" + mustDevice(t, h).ID + ":"
	_, files := proto.SplitAttachments(h.engine.texts[device+"s1"])
	if images := h.engine.images[device+"s1"]; len(images) != 1 || len(files) != 2 || files[0].Note != "" || files[1].Note != uploads.NotShown(uploads.ErrImagesFull) {
		t.Fatalf("%d images went, files %+v", len(images), files)
	}

	prepared = 0
	h.server.maxComposed = 10
	_, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "s2", Text: "See.", Attachments: ups})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if prepared != 0 {
		t.Fatalf("a message too long read %d images", prepared)
	}
}

// An upload left by a stop whose key a request holds as the first pass
// comes is settled by a later pass: the request may not settle it, as one
// that reuses its id for another request does not (Codex review of #49,
// round 4).
func TestASkippedLeftUploadIsSettledLater(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.server.uploadStop = func(step string, _ int64, _ string) bool { return step == "staged" }
	_, err := h.upload(session.ID, "u1", "u1.txt", "", []byte("cut"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.server.uploadStop = nil
	key := "u:" + mustDevice(t, h).ID + ":u1"
	h.server.uploadsMu.Lock()
	h.server.run, h.server.uploadsDue = "the next run", true
	h.server.uploadsMu.Unlock()
	unlock := h.server.keys.lock(key) // a request that holds the key
	_ = h.server.DeliverPending(ctx)
	unlock()
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.store.LookupRequest(ctx, key, "upload:"+strconv.FormatInt(session.ID, 10)); err != nil || found {
		t.Fatalf("the skipped upload's request id is held: %v, %v", found, err)
	}
}

// An upload named whose name a sync did not write keeps its staged claim:
// the request tried again finds its file and finishes it, and makes no
// other (Codex review of #49, round 6).
func TestAnUploadWhoseNameIsNotSyncedIsFinishedByItsRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.server.linkUpload = func(staged *uploads.Staged, file string) error {
		if err := staged.Link(file); err != nil {
			return err
		}
		return fmt.Errorf("%s: %w: the disk went", file, uploads.ErrNotSynced)
	}
	_, err := h.upload(session.ID, "u1", "notes.txt", "", []byte("notes"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.server.linkUpload = (*uploads.Staged).Link
	up, err := h.upload(session.ID, "u1", "notes.txt", "", []byte("notes"))
	if err != nil {
		t.Fatal(err)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 1 || up.Path != h.server.uploads.Path(session.ID, files[0]) {
		t.Fatalf("files %q, upload %+v", files, up)
	}
}

// A retry of a stored upload reads its body to its end under the upload's
// bound before it answers: one of unknown length over it is 413 and one
// cut off bad_request, as the first try's would be (Codex review of #49,
// round 6).
func TestARetryReadsItsBodyUnderTheBound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	if _, err := h.upload(session.ID, "u1", "big.bin", "", []byte("first")); err != nil {
		t.Fatal(err)
	}
	post := func(body io.Reader) int {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, base+proto.Path(proto.PathUploads, session.ID)+"?name=big.bin&requestId=u1", body)
		request.ContentLength = -1
		request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		request.Header.Set("Authorization", "Bearer "+h.token)
		response, err := h.http.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode
	}
	if status := post(io.LimitReader(zeros{}, proto.MaxUpload+1)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a retry over the bound: %d", status)
	}
	if status := post(io.MultiReader(strings.NewReader("half"), cutOff{})); status != http.StatusBadRequest {
		t.Fatalf("a retry cut off: %d", status)
	}
	if status := post(strings.NewReader("first")); status != http.StatusOK {
		t.Fatalf("a whole retry: %d", status)
	}
	if files := h.uploadedFiles(session.ID); len(files) != 1 {
		t.Fatalf("files %q", files)
	}
}

// An upload whose session's folder an agent moved as its file was named
// is not stored at a path that holds nothing: its claim goes, and the
// request tried again stores the file where its path says (Codex review
// of #49, round 7).
func TestAnUploadMovedAsItIsNamedIsStoredAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	folder := filepath.Join(h.server.uploads.Base(), strconv.FormatInt(session.ID, 10))
	h.server.linkUpload = func(staged *uploads.Staged, file string) error {
		if err := staged.Link(file); err != nil {
			return err
		}
		return os.Rename(folder, folder+"-moved")
	}
	_, err := h.upload(session.ID, "u1", "notes.txt", "", []byte("notes"))
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.server.linkUpload = (*uploads.Staged).Link
	up, err := h.upload(session.ID, "u1", "notes.txt", "", []byte("notes"))
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(up.Path); err != nil || string(data) != "notes" {
		t.Fatalf("the upload's path holds %q, %v", data, err)
	}
}
