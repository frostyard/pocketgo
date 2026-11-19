package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
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
	onLive   func(proto.Live)
	runFirst int64 // the current run's first entry; 0 when idle
	ended    map[int64]bool
	byKey    map[string]int64
	// specs are the sessions created, in order; more are models served
	// besides faux-1.
	specs []SessionSpec
	more  []proto.Model
	// made are the sessions created, by request key, as Created reads them.
	made     map[string]Created
	usage    proto.Usage
	usageErr error
}

func (f *fakeEngine) Models(context.Context) ([]proto.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.Model{{ModelRef: model, Name: "Faux", Reasoning: true, ThinkingLevels: []string{"off", "minimal", "low", "medium", "high"}}},
		f.more...), nil
}

func (f *fakeEngine) CreateSession(_ context.Context, key string, spec SessionSpec) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byKey[key]; ok {
		return id, nil
	}
	f.next++
	f.created++
	f.byKey[key] = f.next
	f.specs = append(f.specs, spec)
	if f.made == nil {
		f.made = map[string]Created{}
	}
	made := Created{ID: f.next, Cwd: spec.Cwd, Model: spec.Model, Thinking: spec.Thinking, Route: spec.Route}
	if spec.Profile != nil {
		made.Profile = spec.Profile.ID
	}
	f.made[key] = made
	return f.next, nil
}

func (f *fakeEngine) Created(_ context.Context, key string) (Created, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	made, ok := f.made[key]
	return made, ok, nil
}

func (f *fakeEngine) Tools() []string { return []string{"read", "write", "edit", "bash"} }

// Watch reports the initial live state, as the real engine does.
func (f *fakeEngine) Watch(_ context.Context, id int64) error {
	f.mu.Lock()
	already := f.watching[id]
	f.watching[id] = true
	onLive := f.onLive
	f.mu.Unlock()
	if !already && onLive != nil {
		onLive(proto.Live{SessionID: id})
	}
	return nil
}

func (f *fakeEngine) RunIdentity(context.Context, int64) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runFirst, f.runFirst != 0, nil
}

func (f *fakeEngine) WaitIdle(context.Context, int64) error { return nil }

func (f *fakeEngine) TaskEnded(_ context.Context, task int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended[task], nil
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
	f.runFirst = 0
	return nil
}

func (f *fakeEngine) Transcript(_ context.Context, id int64) ([]proto.Entry, proto.Live, error) {
	return []proto.Entry{{ID: 1, SessionID: id, Kind: proto.EntryUser, Text: "hi"}}, proto.Live{SessionID: id}, nil
}

func (f *fakeEngine) Usage(context.Context, int64) (proto.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usage, f.usageErr
}

// clock is the work store's clock in a test; it moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t      *testing.T
	clock  *clock
	hub    *events.Hub
	server *Server
	engine *fakeEngine
	store  *store.Store
	http   *http.Client
	client *proto.Client
	token  string
	work   string
}

const base = "http://pocketd.test"

// memTransport serves requests with a handler in memory, streaming the
// response body through a pipe, so the tests open no socket and SSE works.
type memTransport struct{ handler http.Handler }

func (m memTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, write := io.Pipe()
	response := &pipeResponse{header: http.Header{}, body: write, ready: make(chan struct{})}
	go func() {
		m.handler.ServeHTTP(response, r)
		response.writeHeader(http.StatusOK)
		_ = write.Close()
	}()
	select {
	case <-response.ready:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: response.status, Header: response.header, Body: body, Request: r, ProtoMajor: 1, ProtoMinor: 1}, nil
}

type pipeResponse struct {
	header http.Header
	status int
	body   *io.PipeWriter
	once   sync.Once
	ready  chan struct{}
}

func (p *pipeResponse) Header() http.Header  { return p.header }
func (p *pipeResponse) WriteHeader(code int) { p.writeHeader(code) }
func (p *pipeResponse) writeHeader(code int) {
	p.once.Do(func() { p.status = code; close(p.ready) })
}
func (p *pipeResponse) Write(b []byte) (int, error) {
	p.writeHeader(http.StatusOK)
	return p.body.Write(b)
}
func (p *pipeResponse) Flush() {}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	now := &clock{now: time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)}
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.sqlite"), now.Now)
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
	server := New(s, hub, "test", now.Now, nil)
	server.SetApprovals(approvals.New(s, server.PublishApproval))
	engine := &fakeEngine{sent: map[string]int64{}, byKey: map[string]int64{}, watching: map[int64]bool{}, ended: map[int64]bool{}, onLive: server.PublishLive,
		usage: proto.Usage{Output: 3}}
	server.SetEngine(engine)
	h := &harness{t: t, clock: now, hub: hub, server: server, engine: engine, store: s, token: token, work: t.TempDir()}
	h.http = &http.Client{Transport: memTransport{server.Handler()}}
	h.client, _ = proto.NewClient(base, token, h.http)
	return h
}

// raw sends a request with full control of headers and body.
func (h *harness) raw(method, path, body string, headers map[string]string) (*http.Response, proto.Error) {
	h.t.Helper()
	request, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response, err := h.http.Do(request)
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
	// Version 1 had no hello and none of what came with it: refused.
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("protocol 1: %d %+v", response.StatusCode, e)
	}
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + auth.NewToken()})
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
		// faux-1 lists off to high: a level it does not list is refused,
		// not left for the provider to change quietly (rule 8).
		"thinking the model lacks": func(c *proto.CreateSession) { c.Thinking = "xhigh" },
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
		map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token})
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
	h.engine.runFirst = 7
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a1"}); err != nil || len(h.engine.aborted) != 1 {
		t.Fatalf("abort %v %v", err, h.engine.aborted)
	}
	h.engine.runFirst = 8 // a later run
	// A retry of an abort whose answer was lost does not abort again.
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a1"}); err != nil || len(h.engine.aborted) != 1 {
		t.Fatalf("abort retry %v %v", err, h.engine.aborted)
	}
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "a1", Text: "x"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

func TestAbortRetiresOnlyTheEndedRunsApprovals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	for _, task := range []int64{3, 4} {
		if _, _, err := h.store.RequestApproval(ctx, store.ApprovalRow{SessionID: session.ID, TaskID: task, CallID: "b", Tool: "bash", Arguments: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	h.engine.runFirst = 10
	h.engine.ended[3] = true // task 4 belongs to a run started since
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a"}); err != nil {
		t.Fatal(err)
	}
	pending, _ := h.client.Approvals(ctx)
	if len(pending) != 1 || pending[0].ID != store.ApprovalID(4, "b") {
		t.Fatalf("pending after abort %+v", pending)
	}
}

func TestAbortRetryNeverStopsALaterRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	device := mustDevice(t, h)
	key := "u:" + device.ID + ":a"
	kind := "abort:" + strconv.FormatInt(session.ID, 10)
	// The first call recorded run 10, then the service stopped.
	if _, _, err := h.store.ClaimRequest(ctx, key, kind); err != nil {
		t.Fatal(err)
	}
	_ = h.store.CompleteRequest(ctx, key, "run:10")
	h.engine.runFirst = 20 // the owner started another run
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a"}); err != nil {
		t.Fatal(err)
	}
	if len(h.engine.aborted) != 0 {
		t.Fatalf("the retry aborted run 20: %v", h.engine.aborted)
	}
	if result, _, _ := h.store.LookupRequest(ctx, key, kind); result != "done" {
		t.Fatalf("the retry did not complete the abort: %q", result)
	}
}

func TestAFailedCreateLeavesTheRequestIDFree(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "x", Cwd: filepath.Join(h.work, "missing"), Model: model})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "x", Text: "hi"}); err != nil {
		t.Fatalf("a request id of a failed create is still taken: %v", err)
	}
}

func TestADatabaseFailureIsNotUnauthorized(t *testing.T) {
	h := newHarness(t)
	_ = h.store.Close()
	_, err := h.client.Hello(context.Background())
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
}

func TestUnknownRoutesAnswerWithProtocolErrors(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
	response, e := h.raw("GET", "/v1/nowhere", "", headers)
	if response.StatusCode != http.StatusNotFound || e.Code != proto.ErrorCodeNotFound {
		t.Fatalf("unknown path: %d %+v", response.StatusCode, e)
	}
	response, e = h.raw("DELETE", proto.PathSessions, "", headers)
	if response.StatusCode != http.StatusMethodNotAllowed || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("wrong method: %d %+v", response.StatusCode, e)
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
	response, e := h.raw("POST", proto.Path(proto.PathApproval, row.ID), `{"requestId":"d9"}`, map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token})
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
	if during != nil {
		go func() {
			for hub.Subscribers() == before {
				runtime.Gosched()
			}
			close(started)
		}()
		<-started
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
	got := collect(t, h.hub, h.client, 0, 7, func() {
		session, _ = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
		h.server.PublishLive(proto.Live{SessionID: session.ID, Busy: true})
		h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	})
	types := []proto.EventType{}
	for _, e := range got {
		types = append(types, e.Type)
	}
	// The stream's hello, one session event at creation, then the watch's
	// initial live state; the entry brings its session.
	want := []proto.EventType{proto.EventHello, proto.EventSession, proto.EventLive, proto.EventLive, proto.EventSession, proto.EventEntry, proto.EventSession}
	for i, w := range want {
		if i >= len(types) || types[i] != w {
			t.Fatalf("event types %v, want %v", types, want)
		}
	}
	if got[4].Session.State != proto.SessionRunning {
		t.Fatalf("a busy live makes the session running: %+v", got[4].Session)
	}
	// Reconnecting after the first session event replays the rest, after
	// a hello at that event.
	replay := collect(t, h.hub, h.client, got[1].Seq, 6, nil)
	if replay[0].Type != proto.EventHello || replay[0].Seq != got[1].Seq || replay[1].Seq != got[2].Seq || replay[5].Seq != got[6].Seq {
		t.Fatalf("replay %v, want hello at %d, then %d..%d", replay, got[1].Seq, got[2].Seq, got[6].Seq)
	}
}

// TestEveryStreamStartsWithHello checks rule 18: the first event of every
// stream is hello, at the client's Last-Event-ID, or at the newest event
// without one. A client that opened without Last-Event-ID and reconnects
// from that seq misses nothing, even when no other event came before the
// stream dropped.
func TestEveryStreamStartsWithHello(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	newest := h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq
	first := collect(t, h.hub, h.client, 0, 1, nil)
	if first[0].Type != proto.EventHello || first[0].Seq != newest {
		t.Fatalf("a stream from now starts with %+v, want hello at %d", first[0], newest)
	}
	// The stream dropped after its hello alone; an entry landed meanwhile.
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: session.ID, Kind: proto.EntryUser, Text: "hi"})
	again := collect(t, h.hub, h.client, first[0].Seq, 3, nil)
	if again[0].Type != proto.EventHello || again[0].Seq != first[0].Seq || again[1].Type != proto.EventEntry || again[2].Type != proto.EventSession {
		t.Fatalf("reconnect from the hello: %+v", again)
	}
	// A future Last-Event-ID: resync, with the newest event. The hello's
	// seq resyncs again, also once the service gave out the future id, so
	// a client that drops before the resync gets it again.
	future := again[2].Seq + 1
	gone := collect(t, h.hub, h.client, future, 2, nil)
	if gone[0].Type != proto.EventHello || gone[0].Seq >= future || gone[1].Type != proto.EventResync || gone[1].Seq != again[2].Seq {
		t.Fatalf("a future Last-Event-ID: %+v", gone)
	}
	if h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq != future {
		t.Fatal("the next event did not take the future id")
	}
	back := collect(t, h.hub, h.client, gone[0].Seq, 2, nil)
	if back[1].Type != proto.EventResync {
		t.Fatalf("back from the resync's hello: %+v", back)
	}
}

// TestAnEntrySendsItsSession checks that an entry's event is followed by
// its session's (the session event's rule): updatedAt moved to when the
// entry landed, and the usage the answer added. A client keeps the session
// list in order from events alone.
func TestAnEntrySendsItsSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.clock.Advance(time.Minute)
	h.engine.mu.Lock()
	h.engine.usage = proto.Usage{Output: 40, Total: 50}
	h.engine.mu.Unlock()
	got := collect(t, h.hub, h.client, 0, 3, func() {
		h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	})
	if got[1].Type != proto.EventEntry || got[2].Type != proto.EventSession {
		t.Fatalf("events %+v", got)
	}
	sent := got[2].Session
	if !sent.UpdatedAt.Equal(session.UpdatedAt.Add(time.Minute)) || sent.Usage.Output != 40 || sent.Usage.Total != 50 {
		t.Fatalf("the entry's session %+v, created %+v", sent, session)
	}
}

func TestAPendingApprovalMakesTheSessionWaiting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	_, _, events, cancel := h.hub.Subscribe(0)
	defer cancel()
	manager := h.server.approvals
	done := make(chan struct{})
	go func() {
		_, _ = manager.Await(ctx, approvals.Call{SessionID: session.ID, TaskID: 1, CallID: "b1", Tool: "bash"})
		close(done)
	}()
	var requested proto.Approval
	for e := range events {
		if e.Type == proto.EventApproval {
			requested = *e.Approval
			break
		}
	}
	if s, _ := h.client.Session(ctx, session.ID); s.Session.State != proto.SessionWaiting {
		t.Fatalf("state while waiting %s", s.Session.State)
	}
	if _, _, err := manager.Decide(ctx, "k", requested.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	<-done
	if s, _ := h.client.Session(ctx, session.ID); s.Session.State != proto.SessionIdle {
		t.Fatalf("state after the decision %s", s.Session.State)
	}
}

func TestBadLastEventID(t *testing.T) {
	h := newHarness(t)
	response, e := h.raw("GET", proto.PathEvents, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token, "Last-Event-ID": "x"})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("%d %+v", response.StatusCode, e)
	}
}

func TestBodyLimits(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
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

// TestASessionEventThatFailsIsTriedAgain fails the read of a session after
// its entry: the entry goes out, and the session follows once a read
// succeeds, with no other change to send it (rule 16b). A summary with no
// usage never goes out in its place.
func TestASessionEventThatFailsIsTriedAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	h.engine.mu.Lock()
	h.engine.usageErr = errors.New("the durable store is busy")
	h.engine.mu.Unlock()
	from := h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	h.server.PublishEntry(proto.Entry{ID: 8, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "more"})
	if len(retries) != 1 {
		t.Fatalf("%d retries waiting, want one for the session", len(retries))
	}
	h.engine.mu.Lock()
	h.engine.usageErr, h.engine.usage = nil, proto.Usage{Output: 40}
	h.engine.mu.Unlock()
	(<-retries)()
	got := collect(t, h.hub, h.client, from, 4, nil)
	types := []proto.EventType{got[0].Type, got[1].Type, got[2].Type, got[3].Type}
	if !slices.Equal(types, []proto.EventType{proto.EventHello, proto.EventEntry, proto.EventEntry, proto.EventSession}) || got[3].Session.Usage.Output != 40 {
		t.Fatalf("events %v, session %+v", types, got[3].Session)
	}
	// A stopped server schedules no retry.
	h.engine.mu.Lock()
	h.engine.usageErr = errors.New("still busy")
	h.engine.mu.Unlock()
	h.server.Stop()
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "late"})
	if len(retries) != 0 {
		t.Fatal("a stopped server scheduled a retry")
	}
}

// TestAnEntrysTimeIsWrittenBeforeItsSessionGoesOut fails the write of an
// entry's time: the session does not go out with the time before the
// entry, and once the write succeeds it goes out with the new time.
func TestAnEntrysTimeIsWrittenBeforeItsSessionGoesOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	var fail sync.Map
	fail.Store("touch", true)
	h.server.touch = func(ctx context.Context, id int64, at time.Time) error {
		if v, _ := fail.Load("touch"); v == true {
			return errors.New("database is locked")
		}
		return h.store.TouchSession(ctx, id, at)
	}
	h.clock.Advance(time.Minute)
	from := h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryUser, Text: "hi"})
	if len(retries) != 1 {
		t.Fatalf("%d retries waiting, want one", len(retries))
	}
	fail.Store("touch", false)
	(<-retries)()
	got := collect(t, h.hub, h.client, from, 3, nil)
	if got[1].Type != proto.EventEntry || got[2].Type != proto.EventSession || !got[2].Session.UpdatedAt.Equal(session.UpdatedAt.Add(time.Minute)) {
		t.Fatalf("events %+v", got)
	}
}

// TestACreatedSessionIsFollowedWhenItsAnswerFails fails the read of a new
// session's answer: the session is watched and its event goes out once it
// reads, and a retry of the request answers with it.
func TestACreatedSessionIsFollowedWhenItsAnswerFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	h.engine.mu.Lock()
	h.engine.usageErr = errors.New("the durable store is busy")
	h.engine.mu.Unlock()
	request := proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model}
	_, err := h.client.CreateSession(ctx, request)
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if h.engine.created != 1 || !h.engine.watching[1] {
		t.Fatalf("created %d, watching %v", h.engine.created, h.engine.watching)
	}
	h.engine.mu.Lock()
	h.engine.usageErr = nil
	h.engine.mu.Unlock()
	if len(retries) != 1 {
		t.Fatalf("%d retries waiting, want the creation event's", len(retries))
	}
	got := collect(t, h.hub, h.client, 0, 2, func() { (<-retries)() })
	if got[1].Type != proto.EventSession || got[1].Session.ID != 1 {
		t.Fatalf("events %+v", got)
	}
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != 1 || h.engine.created != 1 {
		t.Fatalf("the retry: %+v %v, created %d", again, err, h.engine.created)
	}
}

// TestEntriesAndSessionsGoOutUnderTheSessionLock checks that an entry, and
// every session event, is published holding sessionMu: no session event
// read before an entry's time is noted can go out after the entry (rule
// 16b).
func TestEntriesAndSessionsGoOutUnderTheSessionLock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	var unlocked []proto.EventType
	h.server.emit = func(e proto.Event) {
		if (e.Type == proto.EventEntry || e.Type == proto.EventSession) && h.server.sessionMu.TryLock() {
			h.server.sessionMu.Unlock()
			unlocked = append(unlocked, e.Type)
		}
		h.hub.Publish(e)
	}
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryUser, Text: "hi"})
	h.server.PublishLive(proto.Live{SessionID: session.ID, Busy: true})
	if len(unlocked) != 0 {
		t.Fatalf("published without the session lock: %v", unlocked)
	}
}

// TestATouchRetriedLaterKeepsItsEntrysTime fails the write of session 1's
// entry time, then session 2 gets a later entry: when the write of session
// 1 succeeds at last, it writes the time of its own entry, so session 2
// stays first.
func TestATouchRetriedLaterKeepsItsEntrysTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	one, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c1", Cwd: h.work, Model: model})
	two, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c2", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	failing := map[int64]bool{one.ID: true}
	h.server.touch = func(ctx context.Context, id int64, at time.Time) error {
		if failing[id] {
			return errors.New("database is locked")
		}
		return h.store.TouchSession(ctx, id, at)
	}
	h.clock.Advance(time.Minute)
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: one.ID, Kind: proto.EntryUser, Text: "first"})
	h.clock.Advance(time.Minute)
	h.server.PublishEntry(proto.Entry{ID: 8, SessionID: two.ID, Kind: proto.EntryUser, Text: "later"})
	h.clock.Advance(time.Minute)
	failing[one.ID] = false
	(<-retries)()
	list, err := h.client.Sessions(ctx)
	if err != nil || len(list) != 2 || list[0].ID != two.ID {
		t.Fatalf("sessions %+v %v: the one with the later entry is not first", list, err)
	}
	if want := one.UpdatedAt.Add(time.Minute); !list[1].UpdatedAt.Equal(want) {
		t.Fatalf("session %d updated at %s, want its entry's %s", one.ID, list[1].UpdatedAt, want)
	}
}

// TestAStaleRetryNeitherPublishesNorEndsTheNextOne lets a retry run after
// it was cancelled, as a timer that fired while the lock was held does:
// it stands down, and leaves the retry that replaced it. After Stop, no
// retry runs.
func TestAStaleRetryNeitherPublishesNorEndsTheNextOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} } // cancel cannot stop it
	fail := func(on bool) {
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		h.engine.usageErr = nil
		if on {
			h.engine.usageErr = errors.New("busy")
		}
	}
	sessions := 0
	h.server.emit = func(e proto.Event) {
		if e.Type == proto.EventSession {
			sessions++
		}
		h.hub.Publish(e)
	}
	fail(true)
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryUser, Text: "a"})
	stale := <-retries
	fail(false)
	h.server.PublishEntry(proto.Entry{ID: 8, SessionID: session.ID, Kind: proto.EntryUser, Text: "b"}) // goes out, cancels stale
	fail(true)
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: session.ID, Kind: proto.EntryUser, Text: "c"})
	next := <-retries
	before := sessions
	fail(false)
	stale()
	if sessions != before {
		t.Fatal("a cancelled retry published")
	}
	h.server.sessionMu.Lock()
	stillThere := h.server.retries[session.ID] != nil
	h.server.sessionMu.Unlock()
	if !stillThere {
		t.Fatal("a cancelled retry ended the one that replaced it")
	}
	h.server.Stop()
	next()
	if sessions != before {
		t.Fatal("a retry ran after Stop")
	}
}

// writeProfiles writes profile files into a folder of the harness's and
// gives the server that folder.
func (h *harness) writeProfiles(files map[string]string) (dir, home string) {
	h.t.Helper()
	dir, home = h.t.TempDir(), h.t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	h.server.SetProfiles(profiles.Folder{Dir: dir, Home: home})
	return dir, home
}

// GET /v1/profiles lists every profile file by id: a valid one with the
// model a session from it runs, through the router, or why none can; an
// invalid one with its problem; never a backup (ADR-0013).
func TestProfilesListsEveryProfileWithItsModel(t *testing.T) {
	h := newHarness(t)
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
	h.writeProfiles(map[string]string{
		"odrade.md":         "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":           "---\nmodel: openai/gpt-6-astra\nrepositories:\n  frostyard/b: /b\n  frostyard/a: /a\ntools: [read]\n---\n# Vera\n",
		"clerk.md":          "---\nrole: clerk\nmodel: github-copilot/gpt-6-luna\n---\n",
		"handy.md":          "---\nmodel: faux/faux-1\ntools: [read, teleport]\n---\n",
		"broken.md":         "---\nmodel: nope\n---\n",
		"odrade.md.bak-123": "---\nmodel: nope\n---\n",
	})
	list, err := h.client.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]proto.Profile{}
	for _, p := range list {
		ids = append(ids, p.ID)
		byID[p.ID] = p
	}
	if want := []string{"broken", "clerk", "handy", "odrade", "vera"}; !slices.Equal(ids, want) {
		t.Fatalf("ids %q, want %q", ids, want)
	}
	if p := byID["odrade"]; p.Name != "Odrade" || p.Role != proto.RoleSupervisor || p.Runs != model || p.Problem != "" || p.Home == "" {
		t.Errorf("odrade %+v", p)
	}
	if p := byID["vera"]; p.Runs != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}) || !slices.Equal(p.Repositories, []string{"frostyard/a", "frostyard/b"}) ||
		!slices.Equal(p.Tools, []string{"read"}) {
		t.Errorf("vera %+v", p)
	}
	if p := byID["clerk"]; p.Runs != (proto.ModelRef{}) || !strings.Contains(p.Problem, "github-copilot/gpt-6-luna") {
		t.Errorf("clerk %+v", p)
	}
	if p := byID["handy"]; !strings.Contains(p.Problem, "teleport") {
		t.Errorf("handy %+v", p)
	}
	if p := byID["broken"]; p.Name != "" || !strings.Contains(p.Problem, "provider/id") {
		t.Errorf("broken %+v", p)
	}
}

// A session from a profile starts in the profile's home with the router's
// model, records its profile and route, and gives the engine the profile;
// the owner's model wins over the profile's.
func TestCreateSessionFromAProfile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
	_, home := h.writeProfiles(map[string]string{
		"vera.md":  "---\nmodel: openai/gpt-6-astra\nhome: ~\ntools: [read, bash]\n---\n# Vera\n\nShe tidies.\n",
		"chani.md": "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Chani\n",
	})
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	alias := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}
	if session.Cwd != home || session.Model != alias || session.Profile != "vera" || session.Route != "profile-alias" {
		t.Fatalf("session %+v", session)
	}
	spec := h.engine.specs[0]
	if spec.Profile == nil || spec.Profile.ID != "vera" || !slices.Equal(spec.Profile.Tools, []string{"read", "bash"}) || spec.Model != alias {
		t.Fatalf("spec %+v", spec)
	}
	owner, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r2", Profile: "vera", Cwd: h.work, Model: model})
	if err != nil || owner.Model != model || owner.Route != "owner" || owner.Cwd != h.work {
		t.Fatalf("the owner's model and folder: %+v, %v", owner, err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r3", Cwd: h.work, Model: model})
	if err != nil || plain.Profile != "" || plain.Route != "owner" {
		t.Fatalf("a plain session: %+v, %v", plain, err)
	}
	if private, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r4", Profile: "chani"}); err != nil || private.Cwd != home {
		t.Fatalf("a private agent in its home: %+v, %v", private, err)
	}
	created := h.engine.created
	for name, request := range map[string]proto.CreateSession{
		"a private agent elsewhere": {RequestID: "b1", Profile: "chani", Cwd: h.work},
		"an unknown profile":        {RequestID: "b2", Profile: "nobody"},
		"a path as a profile":       {RequestID: "b3", Profile: "../vera"},
		"no model and no profile":   {RequestID: "b4", Cwd: h.work},
	} {
		var e *proto.Error
		if _, err := h.client.CreateSession(ctx, request); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if h.engine.created != created {
		t.Fatalf("a refused request created %d sessions", h.engine.created-created)
	}
}

// A conversation made for a request whose record failed is recorded, at
// the retry, as it was made: not re-routed from a profile that changed or
// is gone since.
func TestARetryRecordsTheSessionAsItWasMade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
	dir, home := h.writeProfiles(map[string]string{"vera.md": "---\nmodel: openai/gpt-6-astra\nhome: ~\n---\n"})
	device, err := auth.Authenticate(ctx, h.store, "Bearer "+h.token)
	if err != nil {
		t.Fatal(err)
	}
	// The first try made the conversation, and failed before recording it.
	vera, err := profiles.Find(dir, home, "vera")
	if err != nil {
		t.Fatal(err)
	}
	alias := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}
	if _, err := h.engine.CreateSession(ctx, "u:"+device.ID+":r1", SessionSpec{Cwd: home, Model: alias, Profile: &vera, Route: "profile-alias"}); err != nil {
		t.Fatal(err)
	}
	// Since then the profile was removed.
	if err := os.Remove(filepath.Join(dir, "vera.md")); err != nil {
		t.Fatal(err)
	}
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	if session.Cwd != home || session.Model != alias || session.Profile != "vera" || session.Route != "profile-alias" || h.engine.created != 1 {
		t.Fatalf("the retry recorded %+v (%d created)", session, h.engine.created)
	}
}

// An owner's model does not start a profile whose own model cannot run:
// GET /v1/profiles lists it with a problem.
func TestAnOwnerModelDoesNotStartAProfileThatCannot(t *testing.T) {
	h := newHarness(t)
	h.writeProfiles(map[string]string{"clerk.md": "---\nrole: clerk\nmodel: github-copilot/gpt-6-luna\nhome: ~\n---\n"})
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "clerk", Model: model})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
}
