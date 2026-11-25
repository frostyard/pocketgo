package nsgoclient

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
	_ "github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// The suite's clock is the fake pocketd's: no test reads the wall clock for
// a launch's end or for a time the board says.
func TestMain(m *testing.M) {
	timeNow = func() time.Time { return pocketdtest.Now }
	nsgotest.Main(m)
}

func TestConversation(t *testing.T) {
	service := pocketdtest.New()
	service.AddSession(proto.Session{ID: 3, Title: "Release checklist", Cwd: "/workspace", Profile: "maintainer", Model: proto.ModelRef{Provider: "fixture", ID: "reviewer"}, State: proto.SessionIdle},
		proto.Entry{ID: 1, Kind: proto.EntryUser, Text: "What should we check before tagging the release?"},
		proto.Entry{ID: 2, Kind: proto.EntryAssistant, Thinking: "Reading the release notes.", Text: "The release checklist is ready.\n\n- Verify the desktop build.\n- Exercise the browser client.\n- Record the results in the release notes."})
	app := signedInApp(t, service, 1230, 800)
	app.WaitFor("text:Release checklist", 5*time.Second)
	app.Press("text:Release checklist")
	app.WaitFor("text:What should we check before tagging the release?", 5*time.Second)
	app.Screenshot("conversation")
	app.Type("label:Message", "The browser build is ready.")
	app.Press("label:Send message")
	app.WaitUntil(5*time.Second, func() bool { return len(service.RequestsTo("POST", "/v1/sessions/3/messages")) == 1 })
	app.WaitFor("text:The browser build is ready.", 5*time.Second)
	app.Type("label:Message", "Draft while inspecting")
	composer := app.Find("label:Message").ID
	app.Press("label:Session details")
	app.WaitFor("text:Pin session", 3*time.Second)
	app.Find("label:Resize right panel")
	if input := app.Find("label:Message"); input.ID != composer || input.Text != "Draft while inspecting" {
		t.Fatal("opening session details replaced the conversation or its draft")
	}
	app.Screenshot("conversation-details")
	app.Press("label:Session details")
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Pin session") })
	if app.Find("label:Message").ID != composer {
		t.Fatal("closing session details replaced the conversation")
	}
}

func signedInApp(t *testing.T, service *pocketdtest.Server, w, h core.Points, opts ...nsgotest.Option) *nsgotest.App {
	t.Helper()
	return signedInOver(t, service.Transport(), w, h, opts...)
}

// signedInOver is signedInApp over transport, which a test puts before
// pocketd's test server to hold a request back before pocketd has it.
func signedInOver(t *testing.T, transport http.RoundTripper, w, h core.Points, opts ...nsgotest.Option) *nsgotest.App {
	t.Helper()
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		// Use the same appearance on both hosts for screenshot comparisons.
		cx.SetAppearance(core.AppearanceLight)
		return newClient(cx, transport)
	}, append(opts, nsgotest.Size(w, h))...)
	app.Clear("label:Service URL")
	app.Type("label:Service URL", pocketdtest.URL)
	app.Type("label:Device token", pocketdtest.Token)
	app.Press("text:Connect")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	return app
}
func TestQuestion(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work", State: proto.SessionWaiting})
	s.PutQuestion(proto.Question{ID: "q4", SessionID: 4, CallID: "call-4", Question: "Which build?", Options: []string{"Native", "Browser"}, Multiple: true})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("text:Open conversation", 5*time.Second)
	app.Press("text:Open conversation")
	app.WaitFor("text:Native", 5*time.Second)
	app.Press("text:Native")
	app.Press("text:Browser")
	app.Press("text:Submit choices")
	app.WaitUntil(5*time.Second, func() bool { q, _ := s.Question("q4"); return q.State == proto.QuestionAnswered })
	app.WaitFor("label:Message", 5*time.Second)
}

func TestPasteAttachments(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work", State: proto.SessionIdle})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("text:Editor", 5*time.Second)
	app.Press("text:Editor")
	app.WaitFor("label:Message", 5*time.Second)
	app.Paste("label:Message", core.PasteEvent{Files: []core.PastedFile{{Name: "notes.txt", MIME: "text/plain", Data: []byte("Hello from a file")}}})
	app.WaitUntil(5*time.Second, func() bool { return len(s.Uploads()) == 1 })
	app.Press("text:Send attachments")
	app.WaitUntil(5*time.Second, func() bool { return len(s.RequestsTo("POST", "/v1/sessions/4/messages")) == 1 })
}

func TestCompactWorkspace(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 1, Title: "Review layout", State: proto.SessionIdle})
	s.SetPrompts(proto.Prompts{Prompts: []proto.Prompt{{Name: "review", Text: "Review the layout."}}})
	app := signedInApp(t, s, 390, 730)
	app.Press("label:Navigation")
	app.WaitFor("text:Review layout", 5*time.Second)
	app.Press("text:Review layout")
	app.WaitFor("label:Message", 5*time.Second)
	app.Press("label:Saved prompts")
	promptID := app.WaitFor("text:/review", 5*time.Second).ID
	// A store refresh can land between pointer down and pointer up (#159).
	app.Press("label:Refresh")
	if app.Find("text:/review").ID != promptID {
		t.Fatal("refresh replaced the saved prompt under the pointer")
	}
	app.Press("text:/review")
	if got := app.Find("label:Message").Text; got != "Review the layout." {
		t.Fatalf("draft = %q", got)
	}
	app.Screenshot("compact-conversation")
	app.Press("label:Session details")
	app.WaitFor("text:Pin session", 3*time.Second)
	app.Press("label:Session details")
	if got := app.Find("label:Message").Text; got != "Review the layout." {
		t.Fatalf("draft after closing details = %q", got)
	}
	app.Press("label:Navigation")
	app.Press("text:Work board")
	app.WaitFor("text:No thread is on the desk", 5*time.Second)
}

func TestFilesAndSearch(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 1, Title: "Release", State: proto.SessionIdle}, proto.Entry{ID: 7, Kind: proto.EntryAssistant, Text: "Ship the changeset."})
	s.SetFileRoots(proto.FileRoots{Roots: []proto.FileRoot{{ID: "agent", Name: "Agent", Path: "/agent"}}})
	s.SetFileFolder(proto.FileFolder{Root: "agent", Entries: []proto.FileEntry{{Name: "example.md", Kind: "file"}}})
	s.SetFileContent(proto.FileContent{Root: "agent", Path: "example.md", Kind: "markdown", Text: "# Release notes\n\nExample file contents."})
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Files")
	app.WaitFor("text:/agent", 5*time.Second)
	app.WaitFor("text:example.md", 5*time.Second)
	app.Press("text:example.md")
	app.WaitFor("text:Example file contents.", 5*time.Second)
	app.Press("label:Search")
	app.Type("label:Search all sessions", "changeset")
	app.Press("text:Search sessions")
	app.WaitFor("text:Ship the changeset.", 5*time.Second)
	app.Press("text:Ship the changeset.")
	app.WaitFor("label:Message", 5*time.Second)
}

func TestRemoteTerminal(t *testing.T) {
	s := pocketdtest.New()
	s.PrintOnStart("Fixture shell ready\r\n")
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Terminal")
	app.Press("text:New terminal")
	app.WaitFor("id:remote-terminal-screen", 10*time.Second)
	terminals := s.Terminals()
	if len(terminals) != 1 {
		t.Fatal("expected one remote shell")
	}
	app.Type("label:Terminal input", "a")
	app.WaitUntil(5*time.Second, func() bool { return strings.Contains(s.TerminalKeys(terminals[0].ID), "a") })
	app.Press("text:End terminal")
	app.WaitUntil(5*time.Second, func() bool { return len(s.Terminals()) == 0 })
}

// TestServerOutputCannotReachTheClipboard checks, in the running client,
// that a shell on the service host neither writes this device's clipboard
// nor reads it (OSC 52). The emulator's answer to a cursor report, which
// follows each sequence, shows that it has read past the sequence.
func TestServerOutputCannotReachTheClipboard(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Terminal")
	app.Press("text:New terminal")
	app.WaitFor("id:remote-terminal-screen", 10*time.Second)
	id := s.Terminals()[0].ID
	app.Clipboard = "local-secret"
	changes := app.ClipboardChangeCount
	answered := func(n int) func() bool {
		return func() bool { return strings.Count(s.TerminalKeys(id), "R") >= n }
	}
	const report = "\x1b[6n" // the emulator answers ESC [ row ; column R
	s.PrintTerminal(id, "\x1b]52;c;ZnJvbS1zZXJ2ZXI=\x1b\\"+report)
	app.WaitUntil(5*time.Second, answered(1))
	if app.Clipboard != "local-secret" || app.ClipboardChangeCount != changes {
		t.Errorf("the shell wrote the clipboard: %q, %d changes", app.Clipboard, app.ClipboardChangeCount-changes)
	}
	s.PrintTerminal(id, "\x1b]52;c;?\x07"+report)
	app.WaitUntil(5*time.Second, answered(2))
	// base64 of "local-secret", as an answer to the query would carry it.
	if keys := s.TerminalKeys(id); strings.Contains(keys, "bG9jYWwtc2VjcmV0") || strings.Contains(keys, "]52") {
		t.Errorf("the shell read the clipboard: the client sent %q", keys)
	}
}

func TestSessionCreationAndApproval(t *testing.T) {
	s := pocketdtest.New()
	model := proto.ModelRef{Provider: "test", ID: "default"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Default"})
	s.AddProfile(proto.Profile{ID: "maintainer", Name: "Maintainer", Role: proto.RoleAgent, Private: true, Cwd: "/workspace", Runs: model})
	s.AddSession(proto.Session{ID: 3, Title: "Existing", State: proto.SessionIdle})
	s.AddApproval(proto.Approval{ID: "a1", SessionID: 3, CallID: "tool-1", Tool: "write", State: proto.ApprovalPending, Arguments: []byte(`{"path":"notes.txt"}`)})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("text:write · notes.txt", 5*time.Second)
	app.Type("label:Decision reason", "Use the draft folder.")
	app.Press("text:Deny")
	app.WaitUntil(5*time.Second, func() bool {
		reqs := s.RequestsTo("POST", "/v1/approvals/a1")
		return len(reqs) == 1 && strings.Contains(string(reqs[0].Body), "Use the draft folder.")
	})
	app.Press("label:New session")
	app.WaitFor("text:No profile", 5*time.Second)
	app.Press("text:No profile")
	app.WaitFor("text:Maintainer · agent", 5*time.Second)
	app.Press("text:Maintainer · agent")
	app.Type("label:Title", "Port acceptance")
	app.Press("text:Start session")
	app.WaitFor("label:Message", 5*time.Second)
	app.Press("label:Session details")
	app.WaitFor("label:Session title", 5*time.Second)
	app.Clear("label:Session title")
	app.Type("label:Session title", "Accepted")
	app.Press("text:Rename")
	app.WaitUntil(5*time.Second, func() bool { v, ok := s.Session(4); return ok && v.Session.Title == "Accepted" })
}
