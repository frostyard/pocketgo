package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestSignInChecksTheTokenFirst(t *testing.T) {
	h := newHarness(t)
	var got error
	done := false
	h.store.SignIn(h.srv.URL, "pgd_wrong", func(err error) { got, done = err, true })
	h.loop.RunUntil(t, func() bool { return done })
	if got == nil || !strings.Contains(got.Error(), "refused the device token") {
		t.Fatalf("a wrong token signed in: %v", got)
	}
	if h.store.SignedIn() || h.srv.Streams() != 0 {
		t.Error("a refused sign-in opened the stream")
	}
	// No service answers: the transport refuses the connection, as a
	// closed port would, without the host's network.
	offline := h.newStore(t, pocketdtest.Hosts{})
	done = false
	offline.SignIn(h.srv.URL, pocketdtest.Token, func(err error) { got, done = err, true })
	h.loop.RunUntil(t, func() bool { return done })
	if got == nil || !strings.Contains(got.Error(), "could not reach pocketd") || !errors.Is(got, pocketdtest.ErrNoHost) {
		t.Errorf("an unreachable service: %v", got)
	}
	done = false
	h.store.SignIn(h.srv.URL, pocketdtest.Token, func(err error) { got, done = err, true })
	h.loop.RunUntil(t, func() bool { return done })
	if got != nil {
		t.Fatal(got)
	}
	h.loop.RunUntil(t, func() bool { return h.store.Conn.State == Connected })
	if h.store.Hello() == nil || h.store.Hello().Device != "laptop" {
		t.Errorf("hello %+v", h.store.Hello())
	}
}

// TestTheTokenNeverFollowsARedirectElsewhere has pocketd redirect the sign-in:
// within its origin the client follows, and to another host or scheme it
// stops before the token leaves (SameOrigin).
func TestTheTokenNeverFollowsARedirectElsewhere(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var reached []string // scheme://host of each sign-in check that arrived with a token
	record := func(r *http.Request) {
		if r.URL.Path == proto.PathHello && r.Header.Get("Authorization") != "" {
			mu.Lock()
			reached = append(reached, r.URL.Scheme+"://"+r.URL.Host)
			mu.Unlock()
		}
	}
	redirectTo := func(target string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			record(r)
			if r.URL.Path == proto.PathHello && r.URL.Query().Get("moved") == "" {
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				return
			}
			h.srv.Handler().ServeHTTP(w, r)
		})
	}
	signIn := func(s *Store, server string) error {
		var got error
		done := false
		s.SignIn(server, pocketdtest.Token, func(err error) { got, done = err, true })
		h.loop.RunUntil(t, func() bool { return done })
		return got
	}
	for _, tc := range []struct {
		name, server, target string
		follows              bool
	}{
		{"same origin", "http://localhost:8788", "/v1/hello?moved=1", true},
		{"another host", "http://localhost:8788", "http://pocketd.example.org/v1/hello?moved=1", false},
		{"another port", "http://localhost:8788", "http://localhost:9999/v1/hello?moved=1", false},
		{"https to http", "https://pocketd.example.ts.net", "http://pocketd.example.ts.net/v1/hello?moved=1", false},
	} {
		reached = nil
		hosts := pocketdtest.Hosts{}
		for _, host := range []string{"localhost:8788", "pocketd.example.org", "localhost:9999", "pocketd.example.ts.net"} {
			hosts[host] = redirectTo(tc.target)
		}
		s := h.newStore(t, hosts)
		err := signIn(s, tc.server)
		s.Close()
		mu.Lock()
		got := slices.Clone(reached)
		mu.Unlock()
		switch {
		case tc.follows && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !tc.follows && (!errors.Is(err, ErrRedirect) || !strings.Contains(err.Error(), "only to the service URL you entered")):
			t.Errorf("%s: signed in through the redirect: %v", tc.name, err)
		case !tc.follows && len(got) != 1:
			t.Errorf("%s: the token reached %v; want only the service URL", tc.name, got)
		}
	}
	if !Fatal(fmt.Errorf("stream: %w", ErrRedirect)) {
		t.Error("a redirected stream would try again forever")
	}
}

// TestARedirectNeverTurnsACommandIntoAGet has pocketd's origin redirect a
// POST with 303: Go would follow it as a GET without the body, and the
// page's 200 would look like the command's success.
func TestARedirectNeverTurnsACommandIntoAGet(t *testing.T) {
	for _, tc := range []struct {
		status int
		ok     bool
	}{{http.StatusSeeOther, false}, {http.StatusFound, false}, {http.StatusMovedPermanently, false}, {http.StatusTemporaryRedirect, true}, {http.StatusPermanentRedirect, true}} {
		var mu sync.Mutex
		var reached []string
		client := HTTPClient(pocketdtest.Hosts{"localhost:8788": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/sessions/1/abort" {
				http.Redirect(w, r, "/moved", tc.status)
				return
			}
			mu.Lock()
			reached = append(reached, r.Method)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		})})
		resp, err := client.Post("http://localhost:8788/v1/sessions/1/abort", "application/json", strings.NewReader(`{"requestId":"r1"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
		mu.Lock()
		got := slices.Clone(reached)
		mu.Unlock()
		switch {
		case tc.ok && (err != nil || len(got) != 1 || got[0] != http.MethodPost):
			t.Errorf("%d: %v, reached %v; want the POST followed", tc.status, err, got)
		case !tc.ok && (!errors.Is(err, ErrRedirect) || len(got) != 0):
			t.Errorf("%d: %v, reached %v; want the redirect refused", tc.status, err, got)
		}
	}
}

// TestTheTokenNeverGoesThroughAProxy sets the proxy variables: the client
// pocketd's requests go through ignores them, so the token takes the route
// the sign-in judged.
func TestTheTokenNeverGoesThroughAProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example.org:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example.org:3128")
	t.Setenv("NO_PROXY", "")
	watch, ok := HTTPClient(nil).Transport.(watchTransport)
	if !ok {
		t.Fatalf("transport %T", HTTPClient(nil).Transport)
	}
	direct, ok := watch.base.(*http.Transport)
	if !ok || direct.Proxy != nil {
		t.Fatalf("the default transport is %T with a proxy function: the token would go to the proxy", watch.base)
	}
	if http.DefaultTransport.(*http.Transport).Proxy == nil {
		t.Error("the test proves nothing: Go's default transport uses no proxy either")
	}
}

func TestConnectLoadsWhatTheWindowShows(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "older", -time.Hour))
	h.srv.AddSession(session(2, "newer", 0))
	h.srv.AddApproval(approval("a1", 1, "c1", 0))
	h.connect(t)
	if got := ids(h.store.Data.Sessions()); len(got) != 2 || got[0] != 2 {
		t.Errorf("sessions %v, want [2 1]", got)
	}
	if got := approvalIDs(h.store.Data.Approvals()); len(got) != 1 || got[0] != "a1" {
		t.Errorf("approvals %v", got)
	}
	if models, _ := h.store.Models(); len(models) != 1 || models[0].ID != "claude-opus-5-5" {
		t.Errorf("models %v", models)
	}
	if h.store.Data.StateOf(1) != proto.SessionWaiting {
		t.Errorf("session 1 is %s, want waiting", h.store.Data.StateOf(1))
	}
}

func TestTheStreamKeepsTheOpenSessionCurrent(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0), proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "hi"})
	h.connect(t)
	h.store.Open(1)
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Loaded })
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true, Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "Hel"}}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Live.Partial != nil })
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant, Text: "Hello"}})
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1}})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Open().Entries) == 2 && !h.store.Data.Open().Live.Busy })
	if h.store.Data.Open().Entries[1].Text != "Hello" {
		t.Errorf("entries %+v", h.store.Data.Open().Entries)
	}
}

// TestTheStreamComesBackFromItsLastEvent drops the stream: the store waits
// as the backoff says, on its clock, and reads the stream again from the
// last event it applied.
func TestTheStreamComesBackFromItsLastEvent(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	last := h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &proto.Session{ID: 1, Title: "renamed", State: proto.SessionIdle, UpdatedAt: pocketdtest.Now}})
	h.loop.RunUntil(t, func() bool { s, _ := h.store.Data.Session(1); return s.Title == "renamed" })

	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	if got := h.clock.Waiting()[0]; got != DefaultBackoff.Min {
		t.Errorf("the first wait is %s, want %s", got, DefaultBackoff.Min)
	}
	if h.store.Conn.State != Reconnecting || !strings.Contains(h.store.Conn.Reason, "trying again in 1s") {
		t.Errorf("conn %+v", h.store.Conn)
	}
	// An event while the stream is down comes back with the next stream.
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &proto.Session{ID: 1, Title: "while down", State: proto.SessionIdle, UpdatedAt: pocketdtest.Now}})
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool {
		s, _ := h.store.Data.Session(1)
		return s.Title == "while down" && h.store.Conn.State == Connected
	})
	streams := h.srv.RequestsTo(http.MethodGet, proto.PathEvents)
	if len(streams) != 2 {
		t.Fatalf("%d stream requests, want 2", len(streams))
	}
	if streams[0].LastEventID != "" || streams[1].LastEventID != strconv.FormatInt(last.Seq, 10) {
		t.Errorf("Last-Event-IDs %q and %q, want none then %d", streams[0].LastEventID, streams[1].LastEventID, last.Seq)
	}
	// Reconnecting fetched the lists again.
	h.settle(t)
	if n := len(h.srv.RequestsTo(http.MethodGet, proto.PathSessions)); n != 2 {
		t.Errorf("%d session list fetches, want 2: one per open stream", n)
	}
}

func TestFailuresInARowWaitLongerUntilTheStreamOpens(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	waitFor := func(want time.Duration) {
		t.Helper()
		h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
		if got := h.clock.Waiting()[0]; got != want {
			t.Fatalf("wait %s, want %s", got, want)
		}
	}
	h.srv.DropStreams()
	waitFor(time.Second)
	for _, want := range []time.Duration{2 * time.Second, 4 * time.Second} {
		h.srv.FailOnce(http.MethodGet, proto.PathEvents, http.StatusInternalServerError)
		h.clock.Fire()
		waitFor(want)
	}
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool { return h.store.Conn.State == Connected && h.srv.Streams() == 1 })
	// The stream opened: the next drop starts from the shortest wait.
	h.srv.DropStreams()
	waitFor(time.Second)
}

// TestARestartedServiceIsLoadedAgain restarts the fake pocketd: the events
// after the store's Last-Event-ID are gone, so it answers resync, and the
// store fetches the lists and the open session again.
func TestARestartedServiceIsLoadedAgain(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0), proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "hi"})
	h.connect(t)
	h.store.Open(1)
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Loaded })
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Live.Busy })

	h.srv.Restart()
	// While it was down, the run asked for an approval and wrote an entry.
	h.srv.AddApproval(approval("a1", 1, "c1", 0))
	h.srv.SetLive(proto.Live{SessionID: 1, Busy: true, Tools: []proto.LiveTool{{CallID: "c1", Name: "bash", Status: proto.ToolPending}}})
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	// The refresh of the stream's opening can bring all of it before the
	// backlog's resync is applied: wait for the resync itself, which starts
	// its own fetches at once, then for their answers.
	resynced := func() bool { return strings.Contains(strings.Join(h.logs, "\n"), "no longer has the events") }
	h.loop.RunUntil(t, func() bool {
		return resynced() && len(h.store.Data.ApprovalsFor(1)) == 1 && len(h.store.Data.Open().Live.Tools) == 1
	})
	h.settle(t)
	stream := h.srv.RequestsTo(http.MethodGet, proto.PathEvents)
	if len(stream) != 2 || stream[1].LastEventID == "" {
		t.Errorf("the second stream did not send Last-Event-ID: %+v", stream)
	}
	if n := len(h.srv.RequestsTo(http.MethodGet, "/v1/sessions/1")); n < 3 {
		t.Errorf("the open session was fetched %d times, want once on opening and again on the reconnect and the resync", n)
	}
	if !strings.Contains(strings.Join(h.logs, "\n"), "no longer has the events") {
		t.Errorf("the resync is not in the log: %q", h.logs)
	}
}

func TestARevokedTokenSignsOut(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	h.srv.Revoke()
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool { return h.signedOut != "" })
	if h.store.SignedIn() || h.store.Conn.State != Offline {
		t.Errorf("still signed in: %+v", h.store.Conn)
	}
	if !strings.Contains(h.signedOut, "pocketd device add") {
		t.Errorf("reason %q", h.signedOut)
	}
}

func TestARequestRefusedWith401SignsOut(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.srv.Revoke()
	h.store.Open(1)
	h.loop.RunUntil(t, func() bool { return h.signedOut != "" })
	if h.store.SignedIn() {
		t.Error("still signed in")
	}
}

func sentIDs(t *testing.T, h *harness, p string) []string {
	t.Helper()
	var out []string
	for _, r := range h.srv.RequestsTo(http.MethodPost, p) {
		var body struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body.RequestID)
	}
	return out
}

func TestAMessageTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	send := func(text string) error {
		var got error
		done := false
		h.store.Send(1, text, func(err error) { got, done = err, true })
		if h.store.Sending(1).Busy != !done {
			t.Errorf("busy %v while done %v", h.store.Sending(1).Busy, done)
		}
		h.loop.RunUntil(t, func() bool { return done })
		return got
	}
	p := proto.Path(proto.PathMessages, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	if err := send("hello"); err == nil {
		t.Fatal("the failure did not reach the composer")
	}
	if h.store.Sending(1).Err == "" {
		t.Error("the failure is not kept for the composer")
	}
	if err := send("hello"); err != nil {
		t.Fatal(err)
	}
	if err := send("hello"); err != nil {
		t.Fatal(err)
	}
	got := sentIDs(t, h, p)
	if len(got) != 3 || got[0] != got[1] || got[1] == got[2] {
		t.Errorf("request ids %q: want the retry to keep the failed one's id, and the next message a new one", got)
	}
	if err := send("   "); err == nil {
		t.Error("an empty message was sent")
	}
	// A message over the limit stays here, and the composer says why.
	if err := send(strings.Repeat("x", proto.MaxMessageSize+1)); err == nil {
		t.Error("a message over the limit was sent")
	}
	if got := h.store.Sending(1).Err; !strings.Contains(got, "more than the") {
		t.Errorf("the composer says %q about a message over the limit", got)
	}
	if n := len(sentIDs(t, h, p)); n != 3 {
		t.Errorf("%d messages reached pocketd, want the 3 valid ones", n)
	}
	h.loop.RunUntil(t, func() bool {
		d, _ := h.srv.Session(1)
		return len(h.store.Data.Sessions()) == 1 && len(d.Entries) == 2
	})
}

func TestDecidingAnApprovalTakesItOffTheLists(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.srv.AddApproval(approval("a1", 1, "c1", 0))
	h.srv.AddApproval(approval("a2", 1, "c2", time.Minute))
	h.connect(t)
	h.store.Decide("a1", false, "  not now ")
	if !h.store.Deciding("a1").Busy {
		t.Error("the decision is not busy while it is on its way")
	}
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Approvals()) == 1 })
	if d, _ := h.srv.Session(1); d.Session.ID != 1 {
		t.Fatal("the session is gone")
	}
	var body proto.Decide
	reqs := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathApproval, "a1"))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Allow || body.Reason != "not now" {
		t.Errorf("the decision sent %s", reqs[0].Body)
	}
	// Decided elsewhere, and the event not here yet: the service says
	// conflict, and the approvals load again.
	other := approval("a2", 1, "c2", time.Minute)
	other.State = proto.ApprovalAllowed
	h.srv.AddApproval(other)
	h.store.Decide("a2", false, "")
	h.loop.RunUntil(t, func() bool { return h.store.Deciding("a2").Err != "" })
	if !strings.Contains(h.store.Deciding("a2").Err, "decided this call already") {
		t.Errorf("a conflict says %q", h.store.Deciding("a2").Err)
	}
	h.settle(t)
	if _, ok := h.store.Data.Approval("a2"); ok {
		t.Error("an approval decided elsewhere is still listed after the reload")
	}
}

func TestADecisionTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.srv.AddApproval(approval("a1", 1, "c1", 0))
	h.connect(t)
	p := proto.Path(proto.PathApproval, "a1")
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	h.store.Decide("a1", true, "ignored for an allow")
	h.loop.RunUntil(t, func() bool { return h.store.Deciding("a1").Err != "" })
	h.store.Decide("a1", true, "")
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Approvals()) == 0 })
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] != got[1] {
		t.Errorf("request ids %q, want the retry to keep the first id", got)
	}
}

func TestCreatingASessionAddsIt(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	var got proto.Session
	var gotErr error
	done := false
	create := func(r proto.CreateSession) {
		done = false
		h.store.CreateSession(r, func(s proto.Session, err error) { got, gotErr, done = s, err, true })
		h.loop.RunUntil(t, func() bool { return done })
	}
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	create(proto.CreateSession{Cwd: "relative", Model: model})
	if gotErr == nil || len(h.srv.RequestsTo(http.MethodPost, proto.PathSessions)) != 0 {
		t.Fatalf("a relative cwd went to the service: %v", gotErr)
	}
	h.srv.FailOnce(http.MethodPost, proto.PathSessions, http.StatusBadGateway)
	request := proto.CreateSession{Cwd: " /home/team/projects/demo/ ", Model: model, Title: " Fix CI ", Thinking: "high"}
	create(request)
	if gotErr == nil || h.store.Creating().Err == "" {
		t.Fatal("the failure did not reach the form")
	}
	create(request)
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if got.Title != "Fix CI" || got.Cwd != "/home/team/projects/demo" || got.Thinking != "high" {
		t.Errorf("session %+v", got)
	}
	if _, ok := h.store.Data.Session(got.ID); !ok {
		t.Error("the new session is not in the list")
	}
	if ids := sentIDs(t, h, proto.PathSessions); len(ids) != 2 || ids[0] != ids[1] {
		t.Errorf("request ids %q, want the retry to keep the first id", ids)
	}
}

// TestAnAbortWhoseAnswerWasLostDoesNotTargetTheNextRun loses the answer of
// an abort that pocketd carried out: the session goes idle, so the next
// press, on the next run, is a new abort, not a retry that pocketd would
// answer without stopping anything.
func TestAnAbortWhoseAnswerWasLostDoesNotTargetTheNextRun(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Open(1)
	p := proto.Path(proto.PathAbort, 1)
	busy := func() bool { d, _ := h.srv.Session(1); return d.Live.Busy }
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Loaded && h.store.Data.Open().Live.Busy })

	h.srv.LoseAnswerOnce(http.MethodPost, p)
	h.store.Abort(1)
	h.loop.RunUntil(t, func() bool { return h.store.Aborting(1).Err != "" && !h.store.Data.Open().Live.Busy })
	if busy() {
		t.Fatal("pocketd did not stop the first run")
	}

	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Live.Busy })
	h.store.Abort(1)
	h.loop.RunUntil(t, func() bool { return !h.store.Aborting(1).Busy && h.store.InFlight() == 0 })
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] == got[1] {
		t.Errorf("request ids %q: the abort of the next run reused the settled abort's id", got)
	}
	if busy() {
		t.Error("the next run still works: its abort was taken for a retry of the first")
	}
}

// TestARunThatEndsWhileTheAbortIsInFlightEndsItsID orders it as the review
// found: the abort stops the run, the session goes idle, another device
// starts a new run, and only then the abort's lost answer comes back. The
// idle seen in between ended the targeted run, so the next press is a new
// abort.
func TestARunThatEndsWhileTheAbortIsInFlightEndsItsID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Open(1)
	p := proto.Path(proto.PathAbort, 1)
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Loaded && h.store.Data.Open().Live.Busy })

	release := h.srv.HoldAndLoseAnswerOnce(http.MethodPost, p)
	h.store.Abort(1)
	h.loop.RunUntil(t, func() bool { return !h.store.Data.Open().Live.Busy }) // the first run stopped
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Live.Busy }) // another device's run
	release()
	h.loop.RunUntil(t, func() bool { return h.store.Aborting(1).Err != "" })

	h.store.Abort(1)
	h.loop.RunUntil(t, func() bool { return !h.store.Aborting(1).Busy && h.store.InFlight() == 0 })
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] == got[1] {
		t.Errorf("request ids %q: the second run's abort reused the first's", got)
	}
	if d, _ := h.srv.Session(1); d.Live.Busy {
		t.Error("the second run still works")
	}
}

func TestAbortStopsTheRun(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Open(1)
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Loaded && h.store.Data.Open().Live.Busy })
	p := proto.Path(proto.PathAbort, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	h.store.Abort(1)
	if !h.store.Aborting(1).Busy {
		t.Error("the abort is not busy while on its way")
	}
	h.loop.RunUntil(t, func() bool { return h.store.Aborting(1).Err != "" })
	h.store.Abort(1)
	h.loop.RunUntil(t, func() bool { return !h.store.Data.Open().Live.Busy && !h.store.Aborting(1).Busy })
	h.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true}})
	h.loop.RunUntil(t, func() bool { return h.store.Data.Open().Live.Busy })
	h.store.Abort(1)
	h.loop.RunUntil(t, func() bool { return !h.store.Data.Open().Live.Busy && !h.store.Aborting(1).Busy })
	if got := sentIDs(t, h, p); len(got) != 3 || got[0] != got[1] || got[1] == got[2] {
		t.Errorf("request ids %q: want the retry to keep the failed one's id, and the next abort a new one", got)
	}
}

func TestSigningOutDropsLateAnswers(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(1, "demo", 0))
	h.connect(t)
	h.store.Open(1)
	h.store.SignOut()
	h.settle(t)
	if h.store.Data.Open() != nil || len(h.store.Data.Sessions()) != 0 {
		t.Error("an answer of the sign-in left reached the store")
	}
	if err := func() error {
		var got error
		h.store.Send(1, "hi", func(err error) { got = err })
		return got
	}(); !errors.Is(err, ErrSignedOut) {
		t.Errorf("a send while signed out: %v", err)
	}
}

func TestAStreamRefusingTheProtocolStops(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	h.store.streamStopped(&proto.Error{Code: proto.ErrorCodeProtocol, Message: "send Pocket-Protocol: 1", Status: 400})
	if !h.store.Conn.Stopped || !strings.Contains(h.store.Conn.Reason, "protocol version") {
		t.Errorf("conn %+v", h.store.Conn)
	}
	h.store.RetryNow()
	if h.store.Conn.Stopped {
		t.Error("RetryNow left the stream stopped")
	}
	h.loop.RunUntil(t, func() bool { return h.store.Conn.State == Connected })
}
