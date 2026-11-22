package pocketdtest

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"golang.org/x/image/draw"
)

// The fake fits an image as pocketd's uploads.PrepareImage does, halving
// it with a 2×2 box while a side is more than twice its target before the
// last bilinear step, so a fitted image's charge is the bytes pocketd's
// would take, a photo on its side included (Codex review of #63, round
// 7). The reference below is pocketd's procedure, written out again.
func TestTheFakeShrinksAsPocketdDoes(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	img := image.NewRGBA(image.Rect(0, 0, 4500, 120))
	for i := range img.Pix {
		img.Pix[i] = byte(r.IntN(256))
		if i%4 == 3 {
			img.Pix[i] = 0xff
		}
	}
	// pocketd: 4500×120 fits 2000×53; a side over twice its target halves
	// first, to 2250×60, then bilinear to 2000×53.
	half := image.NewRGBA(image.Rect(0, 0, 2250, 60))
	draw.ApproxBiLinear.Scale(half, half.Bounds(), img, img.Bounds(), draw.Src, nil)
	fitted := image.NewRGBA(image.Rect(0, 0, 2000, 53))
	draw.ApproxBiLinear.Scale(fitted, fitted.Bounds(), half, half.Bounds(), draw.Src, nil)
	var p, j bytes.Buffer
	if err := png.Encode(&p, fitted); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, fitted, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodedLen(min(p.Len(), j.Len()))
	if got := fitImage(img, 1); got != want {
		t.Fatalf("the fake charges %d, pocketd's fitting takes %d", got, want)
	}
	// On its side, the same: shrunk as stored, then turned.
	if got := fitImage(proto.Upright(img, 8), 6); got != want {
		t.Fatalf("the fake charges a photo on its side %d, pocketd's fitting takes %d", got, want)
	}
}

// The fake names a worker's starter as pocketd does (protocol rule 23a):
// in GET /v1/sessions, in the session's detail and in its session events,
// so the desktop's tests nest a worker over the protocol.
func TestTheFakeNamesAWorkersStarter(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 2, Title: "T7 · Fix CI", Profile: "duncan", UpdatedAt: Now})
	worker := proto.Session{ID: 6, Title: "T7 · worker", Route: "worker-default", StartedBy: 2, UpdatedAt: Now}
	s.AddSession(worker)
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := client.Sessions(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("sessions %+v, %v", list, err)
	}
	for _, got := range list {
		if want := map[int64]int64{2: 0, 6: 2}[got.ID]; got.StartedBy != want {
			t.Errorf("GET /v1/sessions has session %d started by %d, want %d", got.ID, got.StartedBy, want)
		}
	}
	if detail, err := client.Session(ctx, 6); err != nil || detail.Session.StartedBy != 2 {
		t.Errorf("the worker's detail is started by %d (%v), want 2", detail.Session.StartedBy, err)
	}
	from := s.Seq()
	worker.State = proto.SessionRunning
	s.Emit(proto.Event{Type: proto.EventSession, Session: &worker})
	stop := errors.New("stop")
	err = client.Events(ctx, from, func(e proto.Event) error {
		if e.Type != proto.EventSession {
			return nil
		}
		if e.Session.ID != 6 || e.Session.StartedBy != 2 {
			t.Errorf("the session event is %+v, want the worker started by 2", e.Session)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("the stream ended with %v before the worker's event", err)
	}
}

// A lesson decision run again with its request id answers with the lesson
// as it is now and decides nothing, as the service's store does: a change
// proposed since stays (Codex review of #18, round 3).
func TestALessonDecisionReplayAnswersTheLessonAsItIsNow(t *testing.T) {
	s := New()
	inForce := proto.Lesson{ID: 1, Agent: "vera", Text: "a", State: proto.LessonActive, Change: &proto.LessonChange{Text: "b"}}
	s.AddLesson(inForce)
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "b"}
	if l, err := client.DecideLesson(ctx, 1, request); err != nil || l.Text != "b" || l.Change != nil {
		t.Fatalf("approve: %+v, %v", l, err)
	}
	again := inForce
	again.Text, again.Change = "b", &proto.LessonChange{Text: "c"}
	s.AddLesson(again)
	l, err := client.DecideLesson(ctx, 1, request)
	if err != nil || l.Change == nil || l.Change.Text != "c" {
		t.Fatalf("the request again: %+v, %v", l, err)
	}
	if l, _ := client.Lessons(ctx); len(l) != 1 || l[0].Change == nil || l[0].Change.Text != "c" {
		t.Fatalf("the replay decided again: %+v", l)
	}
}

// A decision of the wrong shape is refused as the service refuses it,
// before anything is decided (Codex review of #18, round 5).
func TestALessonDecisionOfTheWrongShapeIsRefused(t *testing.T) {
	s := New()
	s.AddLesson(proto.Lesson{ID: 1, Agent: "vera", Text: "a", State: proto.LessonActive, Change: &proto.LessonChange{Forget: true}})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []proto.DecideLesson{
		{RequestID: "r1", Decision: proto.LessonDiscard, Text: "a"},
		{RequestID: "r2", Decision: proto.LessonApprove, Text: "a", Forget: true},
		{RequestID: "r3", Decision: proto.LessonApprove},
		{RequestID: "r4", Decision: proto.LessonForget, Forget: true},
		{RequestID: "r5", Decision: "keep"},
	} {
		_, err := client.DecideLesson(context.Background(), 1, d)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%+v: %v", d, err)
		}
	}
	if l, _ := client.Lessons(context.Background()); len(l) != 1 || l[0].Change == nil {
		t.Fatalf("a refused decision decided: %+v", l)
	}
}

// A retried approval answers with the plan as it is now, as pocketd's does,
// not the first answer (Codex review of #25, round 7).
func TestARetriedApprovalAnswersThePlanAsItIs(t *testing.T) {
	s := New()
	rev := strings.Repeat("a", 64)
	s.AddPlan(proto.Plan{ID: 1, State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev}})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := proto.ApprovePlan{RequestID: "r1", Rev: rev, Graph: 1}
	if got, err := client.ApprovePlan(ctx, 1, request); err != nil || got.State != proto.PlanApproved {
		t.Fatalf("approve %+v %v", got, err)
	}
	building := proto.Plan{ID: 1, State: proto.PlanBuilding, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev, Approved: true}}
	s.AddPlan(building)
	if got, err := client.ApprovePlan(ctx, 1, request); err != nil || got.State != proto.PlanBuilding {
		t.Fatalf("a retry %+v %v", got, err)
	}
}

// An approval looks its request id up and claims it under one hold of the
// lock, as pocketd claims it in the approval's transaction: two requests
// of one id never both miss the record (Codex review of #25, round 8).
func TestAnApprovalClaimsItsRequestIDAtomically(t *testing.T) {
	s := New()
	rev := strings.Repeat("a", 64)
	s.AddPlan(proto.Plan{ID: 1, State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev}})
	gap := false
	s.afterReplayLookup = func() {
		// Held by the approval, the lock cannot be taken here.
		if s.mu.TryLock() {
			gap = true
			s.mu.Unlock()
		}
	}
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApprovePlan(context.Background(), 1, proto.ApprovePlan{RequestID: "r1", Rev: rev, Graph: 1}); err != nil {
		t.Fatal(err)
	}
	if gap {
		t.Fatal("the lock was let go between the request id's lookup and its claim")
	}
}

// A shift's stop or resume tried again answers with the thread as it is
// now and changes nothing, as pocketd's does (protocol rule 55), not the
// first answer; a stop files the blocked report pocketd files, and a
// thread still starting takes no change (Codex review of #37).
func TestAShiftChangeTriedAgainAnswersTheThreadAsItIs(t *testing.T) {
	s := New()
	s.AddThread(proto.Thread{ID: 4, Agent: "vera", Session: 9, State: proto.ThreadOpen,
		Shift: &proto.Shift{State: proto.ShiftRunning, Round: 3, MaxRounds: 8}})
	s.AddThread(proto.Thread{ID: 5, Agent: "vera", State: proto.ThreadStarting,
		Shift: &proto.Shift{State: proto.ShiftRunning, Round: 1, MaxRounds: 8}})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stopped, err := client.StopShift(ctx, 4, proto.StopShift{RequestID: "s1"})
	if err != nil || stopped.Shift.State != proto.ShiftEnded || stopped.State != proto.ThreadBlocked {
		t.Fatalf("stop %+v %v", stopped, err)
	}
	resumed, err := client.ResumeShift(ctx, 4, proto.ResumeShift{RequestID: "r1"})
	if err != nil || resumed.Shift.State != proto.ShiftRunning || resumed.Shift.Round != 4 || !resumed.UpdatedAt.After(stopped.UpdatedAt) {
		t.Fatalf("resume %+v %v", resumed, err)
	}
	again, err := client.StopShift(ctx, 4, proto.StopShift{RequestID: "s1"})
	if err != nil || again.Shift.State != proto.ShiftRunning || again.Shift.Round != 4 {
		t.Fatalf("the stop tried again: %+v %v, want the running shift as it is", again, err)
	}
	var pe *proto.Error
	if _, err := client.StopShift(ctx, 5, proto.StopShift{RequestID: "s2"}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("a starting thread's stop: %v, want 409", err)
	}

	// A shift stopped with rounds left whose thread then closed takes no
	// resume; a resume claimed before still answers with the thread as it
	// is (Codex review of #37, round 2).
	if _, err := client.StopShift(ctx, 4, proto.StopShift{RequestID: "s3"}); err != nil {
		t.Fatal(err)
	}
	before, err := client.Threads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := client.CloseThread(ctx, 4, proto.CloseThread{RequestID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	// A close moves updatedAt on as every change does, though the thread
	// changed later than Now: the client keeps the later copy.
	if i := slices.IndexFunc(before, func(th proto.Thread) bool { return th.ID == 4 }); i < 0 || !closed.UpdatedAt.After(before[i].UpdatedAt) {
		t.Fatalf("the close's updatedAt %v did not move on from the thread's", closed.UpdatedAt)
	}
	if _, err := client.ResumeShift(ctx, 4, proto.ResumeShift{RequestID: "r2"}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeConflict {
		t.Fatalf("a closed thread's resume: %v, want 409", err)
	}
	if got, err := client.ResumeShift(ctx, 4, proto.ResumeShift{RequestID: "r1"}); err != nil || got.State != proto.ThreadClosed {
		t.Fatalf("a claimed resume tried again on the closed thread: %+v %v", got, err)
	}
}

// A model change run again with its request id answers the session as it
// is now and changes nothing again, as pocketd does (rule 7e): a later
// change stays. The id with another body, or for another session, is
// conflict (Codex review of #48, round 1).
func TestAModelChangeTriedAgainAnswersTheSessionAsItIs(t *testing.T) {
	s := New()
	opus := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, ThinkingLevels: []string{"off", "high"}}
	sol := proto.Model{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}, ThinkingLevels: []string{"off", "high"}}
	s.SetModels([]proto.Model{opus, sol})
	s.AddSession(proto.Session{ID: 1, Title: "Plan", Model: opus.ModelRef, UpdatedAt: Now})
	s.AddSession(proto.Session{ID: 2, Title: "Fix", Model: opus.ModelRef, UpdatedAt: Now})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := proto.ChangeModel{RequestID: "m1", Model: &sol.ModelRef}
	if got, err := client.ChangeModel(ctx, 1, first); err != nil || got.Model != sol.ModelRef {
		t.Fatalf("the change: %+v, %v", got, err)
	}
	if _, err := client.ChangeModel(ctx, 1, proto.ChangeModel{RequestID: "m2", Thinking: "high"}); err != nil {
		t.Fatal(err)
	}
	again, err := client.ChangeModel(ctx, 1, first)
	if err != nil || again.Model != sol.ModelRef || again.Thinking != "high" {
		t.Fatalf("the change again: %+v, %v, want the session as the later change left it", again, err)
	}
	if detail, _ := s.Session(1); detail.Session.Thinking != "high" {
		t.Fatalf("the replay changed the session: %+v", detail.Session)
	}
	for _, c := range []struct {
		id      int64
		request proto.ChangeModel
	}{
		{1, proto.ChangeModel{RequestID: "m1", Model: &opus.ModelRef}},
		{1, proto.ChangeModel{RequestID: "m1", Model: &sol.ModelRef, Thinking: "high"}},
		{2, first},
	} {
		var e *proto.Error
		if _, err := client.ChangeModel(ctx, c.id, c.request); !errors.As(err, &e) || e.Status != http.StatusConflict {
			t.Errorf("session %d with %+v: %v, want conflict", c.id, c.request, err)
		}
	}
}

// An upload looks its request id up and stores its file under one hold of
// the lock, as pocketd holds a request's key: two tries of one id never
// both miss the record (Codex review of #49, round 5).
func TestAnUploadClaimsItsRequestIDAtomically(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	gap := false
	s.afterReplayLookup = func() {
		// Held by the upload, the lock cannot be taken here.
		if s.mu.TryLock() {
			gap = true
			s.mu.Unlock()
		}
	}
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("note")), nil }
	if _, err := client.Upload(context.Background(), 1, "u1", "a.txt", "text/plain", open, 4); err != nil {
		t.Fatal(err)
	}
	if gap {
		t.Fatal("the lock was let go between the request id's lookup and the upload's record")
	}
}

// The fake counts a message's images as pocketd does, whatever model the
// session runs: a PNG it reads, and not an SVG, nor a PNG over 5 MiB, nor
// bytes that are no image of their type (Codex review of #49, round 6).
func TestTheFakeCountsAMessagesImagesAsPocketdDoes(t *testing.T) {
	s := New()
	blind := proto.ModelRef{Provider: "faux", ID: "blind"}
	s.AddModel(proto.Model{ModelRef: blind, Name: "Blind"})
	s.AddSession(proto.Session{ID: 1, Model: blind})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	var shot bytes.Buffer
	if err := png.Encode(&shot, image.NewGray(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	big := append(slices.Clone(shot.Bytes()), make([]byte, 5<<20)...)
	var ids []int64
	for i, f := range []struct {
		name, mime string
		data       []byte
	}{
		{"shot.png", "image/png", shot.Bytes()},
		{"logo.svg", "image/svg+xml", []byte("<svg/>")},
		{"big.png", "image/png", big},
		{"fake.png", "image/png", []byte("\x89PNG\r\n\x1a\nnot really")},
		// A header whose image is cut off, and a WebP's signature alone,
		// decode no image, as pocketd finds (Codex review of #49, round 9).
		{"cut.png", "image/png", shot.Bytes()[:shot.Len()-12]},
		{"empty.webp", "image/webp", []byte("RIFF\x04\x00\x00\x00WEBP")},
	} {
		open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.data)), nil }
		up, err := client.Upload(context.Background(), 1, "u"+strconv.Itoa(i), f.name, f.mime, open, int64(len(f.data)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.ID)
	}
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: ids}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if len(entries) != 1 || entries[0].Images != 1 {
		t.Fatalf("entries %+v", entries)
	}
}

// An upload whose body was cut off is refused, as pocketd refuses it, and
// stores nothing: the fake never takes part of a file for the whole
// (Codex review of #49, round 7).
func TestTheFakeRefusesAnUploadCutOff(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	request, err := http.NewRequest(http.MethodPost, URL+proto.Path(proto.PathUploads, 1)+"?name=a.txt&requestId=u1",
		io.MultiReader(strings.NewReader("half"), errorReader{errors.New("the connection went")}))
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = -1
	request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
	request.Header.Set("Authorization", "Bearer "+Token)
	response, err := (&http.Client{Transport: s.Transport()}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || len(s.Uploads()) != 0 {
		t.Fatalf("a body cut off: %d, uploads %+v", response.StatusCode, s.Uploads())
	}
}

// The fake keeps an upload's name and type as pocketd does: a comma and
// what else a safe name holds not made "_", a type of its extension for
// application/octet-stream; so a message names it in a line the desktop
// reads back, and its entry counts it (Codex review of #49, round 8).
func TestTheFakeNamesAndTypesAnUploadAsPocketdDoes(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("%PDF")), nil }
	up, err := client.Upload(context.Background(), 1, "u1", "report, final.pdf", "application/octet-stream", open, 4)
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != proto.SafeUploadName("report, final.pdf") || up.Name != "report_ final.pdf" || up.MimeType != "application/pdf" {
		t.Fatalf("the fake kept %+v", up)
	}
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: "Read it.", Attachments: []int64{up.ID}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if len(entries) != 1 || entries[0].Files != 1 {
		t.Fatalf("entries %+v", entries)
	}
	if _, files := proto.SplitAttachments(entries[0].Text); len(files) != 1 || files[0].Name != up.Name {
		t.Fatalf("the message names %+v", files)
	}
}

// A queued message's entry, once placed, keeps the images and files the
// message carried, as pocketd's does (Codex review of #49, round 10).
func TestTheFakePlacesAQueuedMessageWithItsFiles(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	var shot bytes.Buffer
	if err := png.Encode(&shot, image.NewGray(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(shot.Bytes())), nil }
	up, err := client.Upload(context.Background(), 1, "u1", "shot.png", "image/png", open, int64(shot.Len()))
	if err != nil {
		t.Fatal(err)
	}
	s.QueueMessages(true)
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: []int64{up.ID}}); err != nil {
		t.Fatal(err)
	}
	s.PlaceQueued(1)
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if len(entries) != 1 || entries[0].Files != 1 || entries[0].Images != 1 {
		t.Fatalf("entries %+v", entries)
	}
}

// The fake keeps a message's images within pocketd's budget, in the order
// the message names them: seven images of near 4 MiB of base64 give six
// (Codex review of #49, round 10).
func TestTheFakeKeepsAMessagesImagesWithinTheirBudget(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	noise := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	r := rand.New(rand.NewPCG(5, 6))
	for i := range noise.Pix {
		noise.Pix[i] = byte(r.IntN(256))
		if i%4 == 3 {
			noise.Pix[i] = 0xff
		}
	}
	var big bytes.Buffer
	if err := png.Encode(&big, noise); err != nil {
		t.Fatal(err)
	}
	cost := base64.StdEncoding.EncodedLen(big.Len())
	if cost*6 > maxMessageImages || cost*7 <= maxMessageImages || cost >= maxImageBase64 {
		t.Fatalf("an image takes %d bytes of base64", cost)
	}
	var ids []int64
	for i := range 7 {
		open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(big.Bytes())), nil }
		up, err := client.Upload(context.Background(), 1, "u"+strconv.Itoa(i), "noise.png", "image/png", open, int64(big.Len()))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.ID)
	}
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: ids}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if len(entries) != 1 || entries[0].Images != 6 || entries[0].Files != 7 {
		t.Fatalf("entries %+v", entries)
	}
}

// The fake charges an image pocketd fits, as a GIF's first frame, what
// the fitted image takes, not pocketd's bound: six small GIFs all go
// (Codex review of #49, round 11).
func TestTheFakeChargesAFittedImageItsSize(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	var small bytes.Buffer
	frame := image.NewPaletted(image.Rect(0, 0, 4, 3), color.Palette{color.Black, color.White})
	if err := gif.Encode(&small, frame, nil); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := range 6 {
		open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(small.Bytes())), nil }
		up, err := client.Upload(context.Background(), 1, "u"+strconv.Itoa(i), "dot.gif", "image/gif", open, int64(small.Len()))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.ID)
	}
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: ids}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if len(entries) != 1 || entries[0].Images != 6 {
		t.Fatalf("entries %+v", entries)
	}
}

// A queued message's preview is cut as pocketd cuts it, and its entry,
// once placed, has its whole text: a long message with files shows cut
// while it waits and whole, with its files, once placed (Codex review of
// #49, round 12).
func TestTheFakeCutsAQueuedMessagesPreview(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("note")), nil }
	up, err := client.Upload(context.Background(), 1, "u1", "notes.txt", "text/plain", open, 4)
	if err != nil {
		t.Fatal(err)
	}
	s.QueueMessages(true)
	text := strings.Repeat("é", proto.MaxQueuedText)
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: text, Attachments: []int64{up.ID}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	queued := slices.Clone(s.sessions[1].detail.Live.Queued)
	s.mu.Unlock()
	if len(queued) != 1 || !queued[0].Cut || len(queued[0].Text) > proto.MaxQueuedText || !utf8.ValidString(queued[0].Text) || queued[0].Files != 1 {
		t.Fatalf("queued %d bytes, cut %v", len(queued[0].Text), queued[0].Cut)
	}
	s.PlaceQueued(1)
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if _, files := proto.SplitAttachments(entries[0].Text); len(entries) != 1 || len(files) != 1 || entries[0].Files != 1 {
		t.Fatalf("the placed entry has %d bytes and %d files", len(entries[0].Text), len(files))
	}
}

// The fake charges an image pocketd fits what pocketd finally sends, made
// smaller as it is until it fits, not pocketd's bound: six transparent
// images that each fit once smaller all go (Codex review of #49, round
// 12).
func TestTheFakeChargesWhatAFittedImageEndsAs(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 1})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	noise := image.NewNRGBA(image.Rect(0, 0, 1000, 1000))
	r := rand.New(rand.NewPCG(7, 8))
	for i := range noise.Pix {
		noise.Pix[i] = byte(r.IntN(256))
	}
	var big bytes.Buffer
	if err := png.Encode(&big, noise); err != nil {
		t.Fatal(err)
	}
	if cost := base64.StdEncoding.EncodedLen(big.Len()); cost < maxImageBase64 || big.Len() > 5<<20 {
		t.Fatalf("an image of %d bytes, %d of base64", big.Len(), cost)
	}
	var ids []int64
	for i := range 6 {
		open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(big.Bytes())), nil }
		up, err := client.Upload(context.Background(), 1, "u"+strconv.Itoa(i), "noise.png", "image/png", open, int64(big.Len()))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.ID)
	}
	if _, err := client.Send(context.Background(), 1, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: ids}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entries := s.sessions[1].detail.Entries
	s.mu.Unlock()
	if len(entries) != 1 || entries[0].Images != 6 {
		t.Fatalf("entries %+v", entries[0].Images)
	}
}

// The fake charges a JPEG on its side what pocketd sends of it: the image
// turned upright by its EXIF orientation and fitted, though it would fit
// as it is, since pocketd never sends it as it is; an upright one its own
// bytes (rule 16a).
func TestTheFakeChargesATurnedPhotoAsPocketdSendsIt(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 30))
	r := rand.New(rand.NewPCG(5, 6))
	for i := range img.Pix {
		img.Pix[i] = byte(r.IntN(256))
		if i%4 == 3 {
			img.Pix[i] = 0xff
		}
	}
	var stored bytes.Buffer
	if err := jpeg.Encode(&stored, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	// A big-endian EXIF segment whose one entry is orientation 6.
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := append([]byte{0xFF, 0xE1, 0, byte(len(payload) + 2)}, payload...)
	turned := append(append(append([]byte{}, stored.Bytes()[:2]...), segment...), stored.Bytes()[2:]...)
	if got := imageCost("image/jpeg", stored.Bytes()); got != base64.StdEncoding.EncodedLen(stored.Len()) {
		t.Fatalf("an upright photo is charged %d, not its own %d", got, base64.StdEncoding.EncodedLen(stored.Len()))
	}
	decoded, err := jpeg.Decode(bytes.NewReader(turned))
	if err != nil {
		t.Fatal(err)
	}
	want := fitImage(proto.Upright(decoded, 6), 1) // it fits: turned, then fitted at its size
	if got := imageCost("image/jpeg", turned); got != want || got == base64.StdEncoding.EncodedLen(len(turned)) {
		t.Fatalf("a photo on its side is charged %d, want %d (its own bytes take %d)", got, want, base64.StdEncoding.EncodedLen(len(turned)))
	}
}

// The fake shrinks a large photo on its side before it turns it, as
// pocketd does, so its memory stays near the stored image's (Codex review
// of #63, round 1).
func TestTheFakeTurnsALargePhotoAfterItShrinks(t *testing.T) {
	var stored bytes.Buffer
	if err := jpeg.Encode(&stored, image.NewGray(image.Rect(0, 0, 4000, 3000)), nil); err != nil {
		t.Fatal(err)
	}
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := append([]byte{0xFF, 0xE1, 0, byte(len(payload) + 2)}, payload...)
	turned := append(append(append([]byte{}, stored.Bytes()[:2]...), segment...), stored.Bytes()[2:]...)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	cost := imageCost("image/jpeg", turned)
	runtime.ReadMemStats(&after)
	if cost <= 0 || cost >= maxImageBase64 {
		t.Fatalf("the photo is charged %d", cost)
	}
	// The decoded photo is 12 MB; turned before it shrinks, it would take
	// 96 MB more.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 50<<20 {
		t.Fatalf("charging the photo allocated %d bytes", allocated)
	}
}
