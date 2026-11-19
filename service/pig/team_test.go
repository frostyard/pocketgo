package pig

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

func agentTools(t *testing.T, e *Engine, id int64) []string {
	t.Helper()
	conversation, err := e.conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tool := range agent.Tools {
		out = append(out, tool.Name)
	}
	return out
}

// The team tools go by role (ADR-0014): a supervisor delegates and closes,
// a thread's specialist reports, and any other session gets none.
func TestTeamToolsGoByRole(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Tools: []string{"read", "bash"}}
	var supervisor int64
	for name, tc := range map[string]struct {
		spec api.SessionSpec
		want []string
	}{
		"a plain session":       {api.SessionSpec{}, []string{"read", "write", "edit", "bash"}},
		"an agent":              {api.SessionSpec{Profile: vera}, []string{"read", "bash"}},
		"a supervisor":          {api.SessionSpec{Profile: odrade, Roster: "Delegate to vera."}, []string{"read", "write", "edit", "bash", toolDelegate, toolClose}},
		"a thread's specialist": {api.SessionSpec{Profile: vera, Thread: 3}, []string{"read", "bash", toolMessage}},
	} {
		tc.spec.Cwd, tc.spec.Model = f.work, fauxRef
		id, err := e.CreateSession(ctx, name, tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, tc.want) {
			t.Errorf("%s: tools %q, want %q", name, got, tc.want)
		}
		if tc.spec.Roster != "" {
			supervisor = id
		}
	}
	// The roster is in the supervisor's instructions.
	conversation, _ := e.conversation(ctx, supervisor)
	agent, _ := conversation.Agent(ctx)
	if agent.Instructions == nil || !strings.HasSuffix(*agent.Instructions, "Delegate to vera.") {
		t.Errorf("a supervisor's instructions %v", agent.Instructions)
	}
}

// fakeTeam answers the team tools and records their calls.
type fakeTeam struct {
	calls []api.TeamCall
	d     []team.Delegation
	fail  error
}

func (f *fakeTeam) TeamDelegate(_ context.Context, call api.TeamCall, d team.Delegation) (string, error) {
	f.calls, f.d = append(f.calls, call), append(f.d, d)
	return "Delegated as T1.", f.fail
}

func (f *fakeTeam) TeamMessage(context.Context, api.TeamCall, team.Report) (string, error) {
	return "", errors.New("not here")
}

func (f *fakeTeam) TeamClose(context.Context, api.TeamCall, int64) (string, error) {
	return "", errors.New("not here")
}

// A team tool passes the calling session and its call to the team; the
// team's text is the result, and its error an error result the agent reads.
func TestTeamToolsCallTheTeam(t *testing.T) {
	f := newFixture(t)
	delegate := map[string]any{"agent": "vera", "task": "Fix CI", "repository": "frostyard/example"}
	e := f.open(fauxModels(toolTurn(toolDelegate, delegate, "d1"), answer("ok"), toolTurn(toolDelegate, delegate, "d2"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}})
	if err != nil {
		t.Fatal(err)
	}
	result := func(text string) *proto.ToolResult {
		t.Helper()
		if _, err := e.Send(ctx, id, text, text); err != nil {
			t.Fatal(err)
		}
		if err := e.WaitIdle(ctx, id); err != nil {
			t.Fatal(err)
		}
		entries, _, err := e.Transcript(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].ToolResult != nil {
				return entries[i].ToolResult
			}
		}
		t.Fatal("no tool result")
		return nil
	}
	if got := result("delegate"); got.IsError || got.Text != "Delegated as T1." {
		t.Fatalf("result %+v", got)
	}
	if len(fake.calls) != 1 || fake.calls[0].Session != id || fake.calls[0].CallID != "d1" || fake.calls[0].Task == 0 ||
		fake.d[0] != (team.Delegation{Agent: "vera", Task: "Fix CI", Repository: "frostyard/example"}) {
		t.Fatalf("calls %+v %+v", fake.calls, fake.d)
	}
	fake.fail = errors.New("the desk is full")
	if got := result("again"); !got.IsError || !strings.Contains(got.Text, "the desk is full") {
		t.Fatalf("a refusal %+v", got)
	}
	// Each call is its own tool task: a reused call id stays apart.
	if len(fake.calls) != 2 || fake.calls[1].Task == fake.calls[0].Task {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// Git runs in the sandbox: its command line is bwrap's, and without a
// sandbox it runs only where a test said Unsandboxed.
func TestGitRunsInTheSandbox(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Sandbox: &Sandbox{Bwrap: fake, Shell: "/bin/sh"}}, commands: map[string]string{"PATH": os.Getenv("PATH")}}
	if _, err := e.Git(context.Background(), "/src/example", "worktree", "add", "-b", "vera/x-t1", "/src/example--vera-x-t1", "HEAD"); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(argv); !strings.HasPrefix(got, "--dev-bind\n/\n/\n") || !strings.HasSuffix(got, "--\ngit\n-C\n/src/example\nworktree\nadd\n-b\nvera/x-t1\n/src/example--vera-x-t1\nHEAD\n") {
		t.Fatalf("bwrap argv:\n%s", got)
	}
	if _, err := (&Engine{}).Git(context.Background(), dir, "status"); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("git without a sandbox: %v", err)
	}
}

// pocketd keeps a bounded part of what git and a checkout's hooks print,
// however much they print, and so does a failure's text.
func TestGitKeepsBoundedOutput(t *testing.T) {
	dir := t.TempDir()
	noisy := filepath.Join(dir, "bwrap")
	script := "#!/bin/sh\nhead -c 3000000 /dev/zero | tr '\\0' o\nhead -c 3000000 /dev/zero | tr '\\0' e >&2\nexit $1\n"
	if err := os.WriteFile(noisy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// The fake bwrap's first argument is --dev-bind: exit takes it as a
	// non-number and fails, as a failing hook would.
	e := &Engine{opts: Options{Sandbox: &Sandbox{Bwrap: noisy, Shell: "/bin/sh"}}, commands: map[string]string{"PATH": os.Getenv("PATH")}}
	_, err := e.Git(context.Background(), dir, "worktree", "add")
	if err == nil {
		t.Fatal("a failing command succeeded")
	}
	if n := len(err.Error()); n > gitDiagnostics+200 || !strings.Contains(err.Error(), "more bytes") {
		t.Fatalf("an error of %d bytes: %.200s", n, err)
	}
	quiet := filepath.Join(dir, "ok")
	if err := os.WriteFile(quiet, []byte("#!/bin/sh\nhead -c 3000000 /dev/zero | tr '\\0' o\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.opts.Sandbox.Bwrap = quiet
	out, err := e.Git(context.Background(), dir, "status")
	if err != nil || len(out) > gitOutput+100 {
		t.Fatalf("output of %d bytes, %v", len(out), err)
	}
}

// lastText is the text of the transcript's last message, and whether it
// is a tool result.
func lastText(transcript ai.TranscriptContext) (string, bool) {
	messages := transcript.Messages()
	if len(messages) == 0 {
		return "", false
	}
	switch m := messages[len(messages)-1].(type) {
	case ai.ToolResultMessage:
		return m.ToolName, true
	case ai.UserMessage:
		switch content := m.Content.(type) {
		case ai.UserText:
			return string(content), false
		case ai.UserContentBlocks:
			var b strings.Builder
			for _, block := range content {
				if text, ok := block.(ai.TextContent); ok {
					b.WriteString(text.Text)
				}
			}
			return b.String(), false
		}
	}
	return "", false
}

// teamModel plays both agents of a delegation by what each was last told:
// the supervisor delegates when the owner asks, the specialist reports
// with an ask when it gets its brief, and both acknowledge anything else.
func teamModel(transcript ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
	text, isResult := lastText(transcript)
	var response ai.FauxResponse
	switch {
	case isResult:
		response = ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("Done with " + text + ".")}}
	case strings.Contains(text, "Ask Vera"):
		response = ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxToolCall(toolDelegate,
			map[string]any{"agent": "vera", "task": "Fix CI", "repository": "frostyard/example"}, &ai.FauxToolCallOptions{ID: "d1"})}, StopReason: "toolUse"}
	case strings.Contains(text, "delegated this to you in thread"):
		response = ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxToolCall(toolMessage,
			map[string]any{"message": "PR #3 is open.", "status": "done", "asks": []any{map[string]any{"question": "Squash?", "options": []any{"yes", "no"}}}},
			&ai.FauxToolCallOptions{ID: "m1"})}, StopReason: "toolUse"}
	default:
		response = ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("Noted: " + text)}}
	}
	return response.AssistantMessage(), nil
}

// TestDelegationEndToEndOverHTTP wires the service as pocketd serve does
// and drives a delegation through the protocol (ADR-0014): the owner asks
// the supervisor, which delegates; the specialist starts in a worktree,
// reports with an ask; the report reaches the supervisor, the ask the
// Inbox; the owner answers, and the answer reaches the specialist.
func TestDelegationEndToEndOverHTTP(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	dataDir, profileDir, home := t.TempDir(), t.TempDir(), t.TempDir()
	checkout := filepath.Join(t.TempDir(), "example")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", checkout},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	for name, text := range map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":   "---\nmodel: faux/faux-1\nrepositories:\n  frostyard/example: " + checkout + "\n---\n# Vera\n",
	} {
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(ctx, filepath.Join(dataDir, "work.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(1024, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(s, hub, "e2e", nil, nil)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	server.SetProfiles(profiles.Folder{Dir: profileDir, Home: home})
	steps := make([]ai.FauxResponseStep, 16)
	for i := range steps {
		steps[i] = ai.FauxFactoryStep(teamModel)
	}
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approvals.NeedsApproval, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(steps...))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	isolateGit(engine)
	server.SetEngine(engine)
	engine.SetTeam(server)
	engine.Resume()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, _ := proto.NewClient(httpServer.URL, token, nil)

	streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream := make(chan proto.Event, 1024)
	go func() {
		_ = client.Events(streamCtx, 0, func(e proto.Event) error { stream <- e; return nil })
	}()
	// Two sessions run at once: events are kept, and each wait looks at
	// every one seen so far.
	var seen []proto.Event
	await := func(what string, match func(proto.Event) bool) proto.Event {
		t.Helper()
		for i := 0; ; i++ {
			for i >= len(seen) {
				select {
				case e := <-stream:
					seen = append(seen, e)
				case <-streamCtx.Done():
					t.Fatalf("no %s event", what)
				}
			}
			if match(seen[i]) {
				return seen[i]
			}
		}
	}
	await("hello", func(e proto.Event) bool { return e.Type == proto.EventHello })
	supervisor, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "odrade"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(ctx, supervisor.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Ask Vera to fix CI."}); err != nil {
		t.Fatal(err)
	}
	// The thread takes its session before the brief; a fast specialist
	// may report before the thread's open event, so the wait is for the
	// session.
	open := await("thread attached", func(e proto.Event) bool { return e.Type == proto.EventThread && e.Thread.Session != 0 }).Thread
	if open.Agent != "vera" || open.Supervisor != supervisor.ID || open.Branch != "vera/fix-ci-t1" || open.Cwd != checkout+"--vera-fix-ci-t1" {
		t.Fatalf("thread %+v", open)
	}
	ask := await("ask", func(e proto.Event) bool { return e.Type == proto.EventAsk && e.Ask.State == proto.AskOpen }).Ask
	if ask.SessionID != open.Session || ask.Question != "Squash?" {
		t.Fatalf("ask %+v", ask)
	}
	await("the report in the supervisor's session", func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.SessionID == supervisor.ID && e.Entry.Kind == proto.EntryUser &&
			e.Entry.Text == "[T1 · Vera · done] PR #3 is open.\n\nAsks waiting for the owner: A1"
	})
	await("thread done", func(e proto.Event) bool { return e.Type == proto.EventThread && e.Thread.State == proto.ThreadDone })
	inbox, err := client.Asks(ctx)
	if err != nil || len(inbox) != 1 || inbox[0].ID != ask.ID {
		t.Fatalf("inbox %+v %v", inbox, err)
	}
	if _, err := client.AnswerAsk(ctx, ask.ID, proto.AnswerAsk{RequestID: proto.NewRequestID(), Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	await("the answer in the specialist's session", func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.SessionID == open.Session && e.Entry.Kind == proto.EntryUser &&
			e.Entry.Text == `The owner answered A1 ("Squash?"): yes`
	})
	await("the specialist's acknowledgement", func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.SessionID == open.Session && e.Entry.Kind == proto.EntryAssistant &&
			strings.HasPrefix(e.Entry.Text, "Noted: The owner answered A1")
	})
	threads, err := client.Threads(ctx)
	if err != nil || len(threads) != 1 || threads[0].State != proto.ThreadDone || threads[0].Report != "PR #3 is open." {
		t.Fatalf("threads %+v %v", threads, err)
	}
	if got := agentTools(t, engine, open.Session); !slices.Contains(got, toolMessage) || slices.Contains(got, toolDelegate) {
		t.Fatalf("the specialist's tools %q", got)
	}
}

// isolateGit keeps the developer's git configuration (signing, global
// hooks) out of the git a test engine runs.
func isolateGit(e *Engine) {
	if e.commands == nil {
		e.commands = map[string]string{}
	}
	e.commands["GIT_CONFIG_GLOBAL"] = "/dev/null"
	e.commands["GIT_CONFIG_NOSYSTEM"] = "1"
}

// gitEnv is the test's environment without git's repository variables
// (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE and the rest) and without the
// developer's git configuration, so a fixture never works on another
// repository.
func gitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}
