package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/store"
)

var model = proto.ModelRef{Provider: "faux", ID: "faux-1"}

// fakeEngine is an in-memory Engine.
type fakeEngine struct {
	mu       sync.Mutex
	next     int64
	created  int
	sent     map[string]int64
	aborted  []int64
	watching map[int64]bool
}

func (f *fakeEngine) Models(context.Context) ([]proto.Model, error) {
	return []proto.Model{{ModelRef: model, Name: "Faux"}}, nil
}

func (f *fakeEngine) CreateSession(context.Context, string, proto.ModelRef, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.created++
	return f.next, nil
}

func (f *fakeEngine) Watch(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watching[id] = true
	return nil
}

func (f *fakeEngine) Send(_ context.Context, _ int64, key, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.sent[key]; ok {
		return id, nil
	}
	f.sent[key] = int64(len(f.sent) + 100)
	return f.sent[key], nil
}

func (f *fakeEngine) Abort(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, id)
	return nil
}

func (f *fakeEngine) Transcript(_ context.Context, id int64) ([]proto.Entry, proto.Live, error) {
	return []proto.Entry{{ID: 1, SessionID: id, Kind: proto.EntryUser, Text: "hi"}}, proto.Live{SessionID: id}, nil
}

func (f *fakeEngine) Usage(context.Context, int64) (proto.Usage, error) {
	return proto.Usage{Output: 3}, nil
}

type harness struct {
	t      *testing.T
	hub    *events.Hub
	server *Server
	engine *fakeEngine
	store  *store.Store
	http   *httptest.Server
	client *proto.Client
	token  string
	work   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(64, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), func(floor, block int64) (int64, error) { return s.ReserveSeq(context.Background(), floor, block) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := New(s, hub, "test", nil, nil)
	server.SetApprovals(approvals.New(s, server.PublishApproval))
	engine := &fakeEngine{sent: map[string]int64{}, watching: map[int64]bool{}}
	server.SetEngine(engine)
	h := &harness{t: t, hub: hub, server: server, engine: engine, store: s, token: token, work: t.TempDir()}
	h.http = httptest.NewServer(server.Handler())
	t.Cleanup(h.http.Close)
	h.client, _ = proto.NewClient(h.http.URL, token, nil)
	return h
}

// raw sends a request with full control of headers and body.
func (h *harness) raw(method, path, body string, headers map[string]string) (*http.Response, proto.Error) {
	h.t.Helper()
	request, _ := http.NewRequest(method, h.http.URL+path, strings.NewReader(body))
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var e proto.Error
	_ = json.NewDecoder(response.Body).Decode(&e)
	return response, e
}

func asError(t *testing.T, err error, status int, code proto.ErrorCode) {
	t.Helper()
	var e *proto.Error
	if !errors.As(err, &e) || e.Status != status || e.Code != code {
		t.Fatalf("error = %v (want %d %s)", err, status, code)
	}
}

func TestProtocolHeaderAndTokenAreRequired(t *testing.T) {
	h := newHarness(t)
	response, e := h.raw("GET", proto.PathHello, "", map[string]string{"Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("no protocol header: %d %+v", response.StatusCode, e)
	}
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: "2", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("protocol 2: %d %+v", response.StatusCode, e)
	}
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + auth.NewToken()})
	if response.StatusCode != http.StatusUnauthorized || e.Code != proto.ErrorCodeUnauthorized {
		t.Fatalf("unknown token: %d %+v", response.StatusCode, e)
	}
	hello, err := h.client.Hello(context.Background())
	if err != nil || hello.Protocol != proto.Version || hello.Device != "laptop" || hello.Server != "test" {
		t.Fatalf("hello %+v, %v", hello, err)
	}
}

func TestCreateSessionValidates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	good := proto.CreateSession{RequestID: "r1", Cwd: h.work, Model: model}
	cases := map[string]func(*proto.CreateSession){
		"no request id":    func(c *proto.CreateSession) { c.RequestID = "" },
		"long request id":  func(c *proto.CreateSession) { c.RequestID = strings.Repeat("x", proto.MaxRequestIDSize+1) },
		"relative cwd":     func(c *proto.CreateSession) { c.Cwd = "work" },
		"missing cwd":      func(c *proto.CreateSession) { c.Cwd = filepath.Join(h.work, "nope") },
		"unknown model":    func(c *proto.CreateSession) { c.Model = proto.ModelRef{Provider: "x", ID: "y"} },
		"unknown thinking": func(c *proto.CreateSession) { c.Thinking = "maximum" },
	}
	for name, mutate := range cases {
		request := good
		mutate(&request)
		_, err := h.client.CreateSession(ctx, request)
		var e *proto.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	response, e := h.raw("POST", proto.PathSessions, `{"requestId":"r","cwd":"`+h.work+`","model":{"provider":"faux","id":"faux-1"},"extra":1}`,
		map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, "unknown field") {
		t.Fatalf("unknown field: %d %+v", response.StatusCode, e)
	}
	if h.engine.created != 0 {
		t.Fatalf("an invalid request created %d sessions", h.engine.created)
	}
}

func TestCreateSessionIsIdempotentAndWatched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	request := proto.CreateSession{RequestID: "r1", Cwd: h.work + "/", Model: model, Thinking: "high"}
	first, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "Session 1" || first.Cwd != h.work || first.Thinking != "high" || first.State != proto.SessionIdle || first.Usage.Output != 3 {
		t.Fatalf("session %+v", first)
	}
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != first.ID || h.engine.created != 1 {
		t.Fatalf("retry %+v %v (created %d)", again, err, h.engine.created)
	}
	if !h.engine.watching[first.ID] {
		t.Fatal("the new session is not watched")
	}
	sessions, err := h.client.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != first.ID {
		t.Fatalf("sessions %+v %v", sessions, err)
	}
	detail, err := h.client.Session(ctx, first.ID)
	if err != nil || len(detail.Entries) != 1 || detail.Session.ID != first.ID {
		t.Fatalf("detail %+v %v", detail, err)
	}
	_, err = h.client.Session(ctx, 99)
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}

func TestSendAndAbort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	sent, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "hello"})
	if err != nil || sent.SubmissionID == 0 {
		t.Fatalf("send %+v %v", sent, err)
	}
	if again, _ := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "hello"}); again != sent {
		t.Fatalf("retry %+v != %+v", again, sent)
	}
	if _, ok := h.engine.sent["u:"+mustDevice(t, h).ID+":m1"]; !ok {
		t.Fatalf("request key not scoped to the device: %v", h.engine.sent)
	}
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m2", Text: "   "})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	_, err = h.client.Send(ctx, 42, proto.SendMessage{RequestID: "m3", Text: "x"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a1"}); err != nil || len(h.engine.aborted) != 1 {
		t.Fatalf("abort %v %v", err, h.engine.aborted)
	}
	// A retry of an abort whose answer was lost does not abort again.
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a1"}); err != nil || len(h.engine.aborted) != 1 {
		t.Fatalf("abort retry %v %v", err, h.engine.aborted)
	}
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "a1", Text: "x"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

func TestAbortRetiresPendingApprovals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	if _, _, err := h.store.RequestApproval(ctx, store.ApprovalRow{SessionID: session.ID, TaskID: 3, CallID: "b", Tool: "bash", Arguments: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a"}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := h.client.Approvals(ctx); len(pending) != 0 {
		t.Fatalf("pending after abort %+v", pending)
	}
	if s, _ := h.client.Session(ctx, session.ID); s.Session.State != proto.SessionIdle {
		t.Fatalf("state after abort %s", s.Session.State)
	}
}

func TestCreateRetryReplaysBeforeRevalidatingAndRestoresTheWatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	work := filepath.Join(h.work, "gone")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	request := proto.CreateSession{RequestID: "r", Cwd: work, Model: model}
	first, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	delete(h.engine.watching, first.ID)
	h.engine.mu.Unlock()
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != first.ID {
		t.Fatalf("retry after the directory went away: %+v %v", again, err)
	}
	if !h.engine.watching[first.ID] {
		t.Fatal("the retry did not restore the watch")
	}
}

func TestEventStreamsEndOnRevocationAndStop(t *testing.T) {
	h := newHarness(t)
	h.server.streamCheck = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- h.client.Events(ctx, 0, func(proto.Event) error { return nil }) }()
	for h.hub.Subscribers() == 0 {
		runtime.Gosched()
	}
	if err := h.store.RevokeDevice(context.Background(), "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; !errors.Is(err, proto.ErrStreamEnded) {
		t.Fatalf("stream of a revoked device ended with %v", err)
	}

	other := newHarness(t)
	go func() { ended <- other.client.Events(ctx, 0, func(proto.Event) error { return nil }) }()
	for other.hub.Subscribers() == 0 {
		runtime.Gosched()
	}
	other.server.Stop()
	if err := <-ended; !errors.Is(err, proto.ErrStreamEnded) {
		t.Fatalf("stream after Stop ended with %v", err)
	}
}

func mustDevice(t *testing.T, h *harness) store.Device {
	t.Helper()
	d, err := h.store.DeviceByTokenHash(context.Background(), auth.Hash(h.token))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApprovalsDecideOnceAndRetriesReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	row, _, err := h.store.RequestApproval(ctx, store.ApprovalRow{SessionID: session.ID, TaskID: 5, CallID: "w1", Tool: "write", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := h.client.Approvals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != row.ID {
		t.Fatalf("pending %+v %v", pending, err)
	}
	decided, err := h.client.Decide(ctx, row.ID, proto.Decide{RequestID: "d1", Allow: true})
	if err != nil || decided.State != proto.ApprovalAllowed || decided.DecidedBy != "laptop" {
		t.Fatalf("decide %+v %v", decided, err)
	}
	retry, err := h.client.Decide(ctx, row.ID, proto.Decide{RequestID: "d1", Allow: true})
	if err != nil || retry.ID != decided.ID || retry.State != decided.State {
		t.Fatalf("retry %+v %v", retry, err)
	}
	_, err = h.client.Decide(ctx, row.ID, proto.Decide{RequestID: "d2", Allow: false})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.Decide(ctx, "anope", proto.Decide{RequestID: "d3", Allow: true})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	// A request id used for one request is refused for another.
	_, err = h.client.Decide(ctx, "anope", proto.Decide{RequestID: "d1", Allow: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "d1", Cwd: h.work, Model: model})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	response, e := h.raw("POST", proto.Path(proto.PathApproval, row.ID), `{"requestId":"d9"}`, map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, "allow") {
		t.Fatalf("a decision without allow: %d %+v", response.StatusCode, e)
	}
}

// collect reads events until it has n or the timeout passes.
func collect(t *testing.T, hub *events.Hub, client *proto.Client, last int64, n int, during func()) []proto.Event {
	t.Helper()
	before := hub.Subscribers()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []proto.Event
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- client.Events(ctx, last, func(e proto.Event) error {
			got = append(got, e)
			if len(got) == n {
				return context.Canceled
			}
			return nil
		})
	}()
	go func() {
		for hub.Subscribers() == before {
			runtime.Gosched()
		}
		close(started)
	}()
	<-started
	if during != nil {
		during()
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("events ended with %v after %d events", err, len(got))
	}
	return got
}

func TestEventsCarrySessionsLiveAndApprovals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var session proto.Session
	got := collect(t, h.hub, h.client, 0, 4, func() {
		session, _ = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
		h.server.PublishLive(proto.Live{SessionID: session.ID, Busy: true})
		h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	})
	types := []proto.EventType{}
	for _, e := range got {
		types = append(types, e.Type)
	}
	want := []proto.EventType{proto.EventSession, proto.EventLive, proto.EventSession, proto.EventEntry}
	for i, w := range want {
		if i >= len(types) || types[i] != w {
			t.Fatalf("event types %v, want prefix %v", types, want)
		}
	}
	if got[2].Session.State != proto.SessionRunning {
		t.Fatalf("a busy live makes the session running: %+v", got[2].Session)
	}
	// Reconnecting after the first event replays the rest.
	replay := collect(t, h.hub, h.client, got[0].Seq, 3, nil)
	if replay[0].Seq != got[1].Seq || replay[2].Seq != got[3].Seq {
		t.Fatalf("replay seqs %d..%d, want %d..%d", replay[0].Seq, replay[2].Seq, got[1].Seq, got[3].Seq)
	}
}

func TestAPendingApprovalMakesTheSessionWaiting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	manager := h.server.approvals
	done := make(chan struct{})
	go func() {
		_, _ = manager.Await(ctx, approvals.Call{SessionID: session.ID, TaskID: 1, CallID: "b1", Tool: "bash"})
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, _ := h.client.Session(ctx, session.ID)
		if s.Session.State == proto.SessionWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never became waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pending, _ := h.client.Approvals(ctx)
	if _, _, err := manager.Decide(ctx, "k", pending[0].ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	<-done
	if s, _ := h.client.Session(ctx, session.ID); s.Session.State != proto.SessionIdle {
		t.Fatalf("state after the decision %s", s.Session.State)
	}
}

func TestBadLastEventID(t *testing.T) {
	h := newHarness(t)
	response, e := h.raw("GET", proto.PathEvents, "", map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + h.token, "Last-Event-ID": "x"})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("%d %+v", response.StatusCode, e)
	}
}

func TestBodyLimits(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + h.token}
	valid := `{"requestId":"r","cwd":"` + h.work + `","model":{"provider":"faux","id":"faux-1"}}`
	for name, body := range map[string]string{
		"trailing data":  valid + `{}`,
		"trailing close": valid + `}`,
		"big trailer":    valid + " " + strings.Repeat("x", maxBody),
		"over 1 MiB":     `{"requestId":"r","cwd":"` + strings.Repeat("a", maxBody) + `"}`,
		"long title":     `{"requestId":"r","cwd":"` + h.work + `","model":{"provider":"faux","id":"faux-1"},"title":"` + strings.Repeat("t", 201) + `"}`,
		"not an object":  `[]`,
	} {
		response, e := h.raw("POST", proto.PathSessions, body, headers)
		if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s: %d %+v", name, response.StatusCode, e)
		}
	}
	if h.engine.created != 0 {
		t.Fatalf("a rejected body created %d sessions", h.engine.created)
	}
	response, _ := h.raw("POST", proto.PathSessions, valid, headers)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("valid body: %d", response.StatusCode)
	}
	response, _ = h.raw("POST", proto.PathSessions, valid, headers)
	if response.StatusCode != http.StatusOK || h.engine.created != 1 {
		t.Fatalf("retry: %d, created %d", response.StatusCode, h.engine.created)
	}
}
