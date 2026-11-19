package app

import (
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"
	"github.com/frostyard/frostui/frostuitest"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// never is Options.After for tests that never let the stream's backoff
// end on its own.
func never(time.Duration) <-chan time.Time { return nil }

// errNoSecret is the memory keyring's NotFound.
var errNoSecret = errors.New("no such secret")

// memRing is a keyring in memory; deleteErr makes Delete fail, as a locked
// keyring does.
type memRing struct {
	mu        sync.Mutex
	secrets   map[string][]byte
	deleteErr error
}

func (r *memRing) Set(key string, value []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets[key] = value
	return nil
}

func (r *memRing) Get(key string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.secrets[key]
	if !ok {
		return nil, errNoSecret
	}
	return v, nil
}

func (r *memRing) Delete(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteErr != nil {
		return r.deleteErr
	}
	delete(r.secrets, key)
	return nil
}

// testApp is an app on a test loop, its config in a temporary folder, its
// token in a keyring in memory, and a fake pocketd served in memory.
type testApp struct {
	a     *App
	loop  *frostuitest.Loop
	srv   *pocketdtest.Server
	ring  *memRing
	creds config.Credentials
	store *config.Store
	tt    *ui.Tester
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	frostui.SetIcons(frostui.NewIcons(nil))
	t.Cleanup(func() { frostui.SetIcons(nil) })
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ring := &memRing{secrets: map[string][]byte{}}
	x := &testApp{loop: frostuitest.NewLoop(), srv: pocketdtest.New(), store: store, ring: ring,
		creds: config.Credentials{Ring: ring, NotFound: errNoSecret, Dir: store.Dir()}}
	x.srv.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5", Reasoning: true})
	x.srv.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-5.5"}, Name: "GPT-5.5", Reasoning: true})
	x.a = x.newApp(t)
	x.tt = ui.NewTesterWith(x.a.View, 1280, 820, ui.TesterOptions{Dark: os.Getenv("POCKETGO_DARK") != ""})
	return x
}

// newApp is an app over the test app's settings, keyring and pocketd, as
// after starting the program again.
func (x *testApp) newApp(t *testing.T) *App {
	t.Helper()
	a, err := New(Options{Version: "test", Post: x.loop.Post, Store: x.store, Creds: x.creds, After: never, Transport: x.srv.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// connect signs the app in to the fake pocketd and waits for its lists.
func (x *testApp) connect(t *testing.T) {
	t.Helper()
	if err := x.a.st.Connect(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connected && x.a.st.Hello() != nil })
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
}

// until runs the loop until cond holds, then draws.
func (x *testApp) until(t *testing.T, cond func() bool) {
	t.Helper()
	x.loop.RunUntil(t, cond)
	x.settle()
}

// settle draws until what was posted and what animates is done.
func (x *testApp) settle() {
	for range 6 {
		x.tt.Advance(200 * time.Millisecond)
		x.loop.Flush()
		x.tt.Frame()
	}
}

func (x *testApp) click(t *testing.T, label string) {
	t.Helper()
	if err := x.tt.Click(label); err != nil {
		t.Fatalf("click %q: %v (texts %q)", label, err, x.tt.Texts())
	}
	x.settle()
}

func (x *testApp) hasAll(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if !x.tt.HasText(s) {
			t.Errorf("missing %q in %q", s, x.tt.Texts())
		}
	}
}

// snapshot writes the last frame as a PNG in POCKETGO_SNAPSHOT_DIR, when
// it is set.
func (x *testApp) snapshot(t *testing.T, name string) {
	t.Helper()
	dir := os.Getenv("POCKETGO_SNAPSHOT_DIR")
	if dir == "" {
		return
	}
	if os.Getenv("POCKETGO_DARK") != "" {
		name += "-dark"
	}
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, x.tt.Image()); err != nil {
		t.Fatal(err)
	}
}

func at(d time.Duration) time.Time { return pocketdtest.Now.Add(d) }

// seed gives the fake pocketd three sessions, one of each state, and two
// approvals waiting.
func (x *testApp) seed() {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 1, Title: "Fix the flaky CI in nsl", Cwd: "/home/team/projects/nsl", Model: model, Thinking: "high",
		State: proto.SessionWaiting, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "The **arm64** job fails about once a day. Find out why and fix it.", At: at(-10 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant, Thinking: "Start with the job's recent logs.",
			Text:      "I'll look at the last failing runs first.",
			ToolCalls: []proto.ToolCall{{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"gh run list --workflow ci.yml --status failure --limit 5"}`)}},
			At:        at(-9 * time.Minute)},
		proto.Entry{ID: 3, SessionID: 1, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c1", Name: "bash",
			Text: "completed  failure  ci  main  arm64 (timeout)  12m\ncompleted  failure  ci  main  arm64 (timeout)  11m"}, At: at(-9 * time.Minute)},
	)
	x.srv.AddSession(proto.Session{ID: 2, Title: "Desk dashboard: weekly metrics", Cwd: "/home/team/projects/pi-pocket-team", Model: model,
		State: proto.SessionRunning, UpdatedAt: at(-2 * time.Minute)})
	x.srv.AddSession(proto.Session{ID: 3, Title: "Draft the release notes", Cwd: "/home/team/projects/frostread", Model: model,
		State: proto.SessionIdle, UpdatedAt: at(-time.Hour)})
	x.srv.AddSession(proto.Session{ID: 4, Title: "Snow image fails to boot", Cwd: "/home/team/projects/snow", Model: model,
		State: proto.SessionWaiting, UpdatedAt: at(-3 * time.Minute)})
	x.srv.AddApproval(proto.Approval{ID: "a1", SessionID: 1, CallID: "w1", Tool: "write",
		Arguments: json.RawMessage(`{"path":".github/workflows/ci.yml","content":"timeout-minutes: 30\n"}`), State: proto.ApprovalPending, RequestedAt: at(-time.Minute)})
	x.srv.AddApproval(proto.Approval{ID: "a2", SessionID: 4, CallID: "b9", Tool: "bash",
		Arguments: json.RawMessage(`{"command":"sudo bootc switch ghcr.io/frostyard/snow:testing"}`), State: proto.ApprovalPending, RequestedAt: at(-30 * time.Second)})
}

func TestSignInSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.settle()
	x.hasAll(t, "Connect to pocketd", "Service URL", "Device token", "Connect")
	if field, ok := x.tt.AccessByRole(ui.RoleTextField, "Device token"); !ok || !field.Password {
		t.Errorf("the token field is not masked: %+v, found %v", field, ok)
	}
	x.snapshot(t, "signin")

	// Plain HTTP to a host outside the tailnet waits for the owner's word,
	// and nothing leaves the device meanwhile.
	x.a.signIn.server, x.a.signIn.token = "http://192.0.2.10:8080", pocketdtest.Token
	x.settle()
	x.click(t, "Connect")
	x.hasAll(t, "Connect Anyway", "Cancel")
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "clear text") {
		t.Errorf("no warning in %q", x.tt.Texts())
	}
	x.snapshot(t, "signin-http-warning")
	if n := len(x.srv.Requests()); n != 0 {
		t.Errorf("%d requests left before the owner confirmed", n)
	}
	x.click(t, "Cancel")

	// A wrong token says so.
	x.a.signIn.server, x.a.signIn.token = x.srv.URL, "pgd_wrong"
	x.settle()
	x.click(t, "Connect")
	x.until(t, func() bool { return x.a.signIn.err != "" })
	if !strings.Contains(x.a.signIn.err, "refused the device token") {
		t.Errorf("error %q", x.a.signIn.err)
	}

	// The right one signs in, keeps the token and remembers the service.
	x.a.signIn.token = pocketdtest.Token
	x.settle()
	x.click(t, "Connect")
	x.until(t, func() bool { return x.a.st.SignedIn() && x.a.st.Conn.State == state.Connected })
	if tok, err := x.creds.Token(x.srv.URL); err != nil || tok != pocketdtest.Token {
		t.Errorf("kept token %q, %v", tok, err)
	}
	if files, _ := filepath.Glob(filepath.Join(x.store.Dir(), "token-*")); len(files) != 0 {
		t.Errorf("the token went to %v though the keyring works", files)
	}
	if st, _ := x.store.Load(); st.Server != x.srv.URL {
		t.Errorf("remembered service %q", st.Server)
	}
	if x.a.signIn.token != "" {
		t.Error("the form still holds the token")
	}
	x.hasAll(t, "Inbox", "Nothing waits for you")
}

func TestSessionsAndInboxSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.connect(t)
	x.hasAll(t, "Inbox", "SESSIONS", "Fix the flaky CI in nsl", "Desk dashboard: weekly metrics", "Draft the release notes",
		"Snow image fails to boot", "waiting", "running", "idle", "write waits for your approval", "bash waits for your approval",
		".github/workflows/ci.yml", "Allow", "Deny", "Open Session", "2 tool calls wait for you", "2 sessions wait", "1 session runs")
	x.snapshot(t, "sessions-inbox")

	// Allow the oldest from the Inbox: it leaves the lists.
	x.click(t, "Allow")
	x.until(t, func() bool { return len(x.a.st.Data.Approvals()) == 1 })
	var body proto.Decide
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathApproval, "a1"))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || !body.Allow || body.RequestID == "" {
		t.Fatalf("the decision sent %+v", reqs)
	}
	x.hasAll(t, "1 tool call waits for you")
	if x.tt.HasText("write waits for your approval") {
		t.Error("the allowed call still shows")
	}

	// Open Session goes to the session of the card.
	x.click(t, "Open Session")
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	if x.a.place != placeSession || x.a.st.Data.OpenID() != 4 {
		t.Errorf("place %v, session %d; want session 4", x.a.place, x.a.st.Data.OpenID())
	}
}

func TestSessionWithAnApprovalSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.connect(t)
	x.click(t, "Fix the flaky CI in nsl, waiting")
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	// The run streams its reply and asks to write a file.
	x.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 1, Busy: true,
		Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "Both failures are **timeouts** on the arm64 runner. I'll raise the job's limit to 30 minutes:",
			ToolCalls: []proto.ToolCall{{ID: "w1", Name: "write", Arguments: json.RawMessage(`{"path":".github/workflows/ci.yml","content":"timeout-minutes: 30\n"}`)}}},
		Tools: []proto.LiveTool{{CallID: "w1", Name: "write", Status: proto.ToolPending}}}})
	x.until(t, func() bool { return x.a.st.Data.Open().Live.Partial != nil })
	x.settle()
	x.hasAll(t, "Fix the flaky CI in nsl", "You", "The arm64 job fails about once a day. Find out why and fix it.",
		"Thinking", "I'll look at the last failing runs first.", "bash", "done", "write", "waiting for you",
		"write waits for your approval", "Allow", "Deny", "Abort", "Send")
	x.snapshot(t, "session-approval")

	// A tool call opens to show its arguments and result.
	x.click(t, "bash gh run list --workflow ci.yml --status failure --limit 5")
	x.hasAll(t, "Arguments", "Result")

	// Deny with a reason: the card goes, and pocketd has the reason.
	*x.a.reasons["a1"] = "Raise it to 20 minutes, not 30"
	x.settle()
	x.click(t, "Deny")
	x.until(t, func() bool { return len(x.a.st.Data.ApprovalsFor(1)) == 0 })
	var body proto.Decide
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathApproval, "a1"))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Allow || body.Reason != "Raise it to 20 minutes, not 30" {
		t.Fatalf("the denial sent %+v", reqs)
	}

	// Abort stops the run.
	x.click(t, "Abort")
	x.until(t, func() bool { return !x.a.st.Data.Open().Live.Busy })
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathAbort, 1))); n != 1 {
		t.Errorf("%d aborts sent", n)
	}

	// Enter sends the draft; it clears once pocketd took it, and the
	// message shows from its entry event.
	*x.a.composer.draft(1) = "Use 20 minutes instead."
	x.settle()
	x.click(t, "Send")
	x.until(t, func() bool { return len(x.a.st.Data.Open().Entries) == 4 })
	if got := *x.a.composer.draft(1); got != "" {
		t.Errorf("the draft is still %q", got)
	}
	x.hasAll(t, "Use 20 minutes instead.")
}

func TestStreamingReplySnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.connect(t)
	x.click(t, "Draft the release notes, idle")
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 1, SessionID: 3, Kind: proto.EntryUser, Text: "Draft the v0.6.0 notes from the merged PRs.", At: at(0)}})
	x.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 3, Busy: true,
		Partial: &proto.Entry{Kind: proto.EntryAssistant, Thinking: "List the merged PRs since v0.5.0.",
			ToolCalls: []proto.ToolCall{{ID: "r1", Name: "bash", Arguments: json.RawMessage(`{"command":"gh pr list --state merged --base main"}`)}}},
		Tools: []proto.LiveTool{{CallID: "r1", Name: "bash", Status: proto.ToolRunning}}}})
	x.until(t, func() bool { o := x.a.st.Data.Open(); return len(o.Entries) == 1 && o.Live.Busy })
	x.hasAll(t, "Draft the v0.6.0 notes from the merged PRs.", "Thinking", "bash", "running", "Working…")
	x.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 2, SessionID: 3, Kind: proto.EntryAssistant, Thinking: "List the merged PRs since v0.5.0.",
		ToolCalls: []proto.ToolCall{{ID: "r1", Name: "bash", Arguments: json.RawMessage(`{"command":"gh pr list --state merged --base main"}`)}}, At: at(time.Second)}})
	x.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 3, SessionID: 3, Kind: proto.EntryToolResult,
		ToolResult: &proto.ToolResult{CallID: "r1", Name: "bash", Text: "#212 Search inside books\n#215 Faster library scans"}, At: at(2 * time.Second)}})
	x.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 3, Busy: true,
		Partial: &proto.Entry{Kind: proto.EntryAssistant, Text: "## frostread v0.6.0\n\n- **Search inside books** (#212)\n- Faster library scans (#215)"}}})
	x.until(t, func() bool {
		o := x.a.st.Data.Open()
		return len(o.Entries) == 3 && o.Live.Partial != nil && o.Live.Partial.Text != ""
	})
	x.settle()
	x.hasAll(t, "frostread v0.6.0", "Search inside books", "done")
	if x.tt.HasText("Working…") {
		t.Error("the working line shows while words stream in")
	}
	x.snapshot(t, "session-streaming")
	// The run fails: its failure shows above the composer.
	x.srv.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 3, Failure: "the provider is overloaded"}})
	x.until(t, func() bool { return x.a.st.Data.Open().Live.Failure != "" })
	x.hasAll(t, "The run failed: the provider is overloaded")
}

func TestNewSessionStartsAndOpens(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.connect(t)
	x.click(t, "New Session")
	x.hasAll(t, "New session", "Working directory", "Model", "Claude Opus 5.5 · anthropic", "Thinking", "Start Session")
	f := x.a.newSession
	f.cwd = "projects/demo"
	x.settle()
	x.click(t, "Start Session")
	if !strings.Contains(f.err, "absolute path") {
		t.Errorf("a relative directory says %q", f.err)
	}
	f.cwd, f.title, f.thinking = "/home/team/projects/demo", "Try the new client", "low"
	x.settle()
	x.click(t, "Start Session")
	x.until(t, func() bool {
		return x.a.place == placeSession && x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded
	})
	s, ok := x.a.st.Data.Session(x.a.st.Data.OpenID())
	if !ok || s.Title != "Try the new client" || s.Thinking != "low" || s.Model.ID != "claude-opus-5-5" {
		t.Errorf("opened %+v", s)
	}
	if f.open {
		t.Error("the dialog stayed open")
	}
	x.hasAll(t, "Try the new client", "A fresh start")
}

func TestAKeptSignInConnectsAtStart(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	if err := x.creds.SetToken(x.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	x.a.settings.Server, x.a.settings.LastSession = x.srv.URL, 3
	// The first try fails: the connecting screen says why and offers Retry.
	x.srv.FailOnce(http.MethodGet, proto.PathEvents, http.StatusBadGateway)
	x.a.Start()
	x.until(t, func() bool { return x.a.st.Conn.Reason != "" })
	x.hasAll(t, "Retry")
	x.click(t, "Retry")
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connected && x.a.st.InFlight() == 0 })
	if x.a.place != placeSession || x.a.st.Data.OpenID() != 3 {
		t.Errorf("the last session did not open: place %v, session %d", x.a.place, x.a.st.Data.OpenID())
	}
	x.hasAll(t, "Draft the release notes")
	// Revoked: the next request signs out, and the sign-in screen says why.
	x.srv.Revoke()
	x.a.st.Refresh()
	x.until(t, func() bool { return !x.a.st.SignedIn() })
	x.hasAll(t, "Connect to pocketd")
	if !strings.Contains(x.a.signIn.err, "no longer takes this device's token") {
		t.Errorf("error %q", x.a.signIn.err)
	}
	if tok, _ := x.creds.Token(x.srv.URL); tok != "" {
		t.Error("the refused token is still kept")
	}
}

// TestAnInboxThatFailedToLoadSaysSo fails the approvals' first fetch: the
// Inbox offers to try again and never says that nothing waits.
func TestAnInboxThatFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.srv.FailOnce(http.MethodGet, proto.PathApprovals, http.StatusBadGateway)
	x.connect(t)
	x.hasAll(t, "Try Again", "The approvals are not loaded")
	if x.tt.HasText("Nothing waits for you") {
		t.Fatal("the Inbox says nothing waits though its approvals did not load")
	}
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "Loading the approvals failed") {
		t.Errorf("no failure in %q", x.tt.Texts())
	}
	x.click(t, "Try Again")
	x.until(t, func() bool { return len(x.a.st.Data.Approvals()) == 2 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "write waits for your approval", "2 tool calls wait for you")
	if x.tt.HasText("Try Again") {
		t.Error("the failure still shows after the approvals loaded")
	}
}

// TestSigningOutEndsTheAutomaticSignInEvenIfTheKeyringKeepsTheToken has a
// locked keyring refuse to delete the token: the owner sees it, and the
// next start does not sign in with the token left behind.
func TestSigningOutEndsTheAutomaticSignInEvenIfTheKeyringKeepsTheToken(t *testing.T) {
	x := newTestApp(t)
	x.a.signIn.server, x.a.signIn.token = x.srv.URL, pocketdtest.Token
	x.settle()
	x.click(t, "Connect")
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connected })
	if st, _ := x.store.Load(); st.Server != x.srv.URL {
		t.Fatalf("the sign-in is not kept: %+v", st)
	}
	x.ring.deleteErr = errors.New("the keyring is locked")
	x.a.confirmSignOut()
	x.settle()
	x.click(t, "Sign Out")
	x.hasAll(t, "Connect to pocketd")
	if !strings.Contains(x.a.signIn.err, "the keyring is locked") {
		t.Errorf("the sign-in screen says %q; want the keyring's failure", x.a.signIn.err)
	}
	st, _ := x.store.Load()
	if st.Server != "" || st.LastServer != x.srv.URL {
		t.Errorf("settings %+v: want no automatic sign-in, and the form to offer the service", st)
	}
	if tok, _ := x.creds.Token(x.srv.URL); tok == "" {
		t.Fatal("the test's keyring lost the token: it should have kept it")
	}
	// The keyring unlocks; the program starts again.
	x.ring.deleteErr = nil
	again := x.newApp(t)
	again.Start()
	x.loop.Flush()
	if again.st.SignedIn() {
		t.Error("the next start signed in with the token the owner signed out of")
	}
	if again.signIn.server != x.srv.URL {
		t.Errorf("the form offers %q", again.signIn.server)
	}
}
