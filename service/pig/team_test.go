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
	"github.com/frostyard/pocketgo/service/internal/worktree"
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

// The team tools go by role (ADR-0014, ADR-0015): a supervisor delegates
// and closes, a thread's specialist reports, every session started from a
// profile keeps lessons, and a plain session gets none.
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
		"an agent":              {api.SessionSpec{Profile: vera}, []string{"read", "bash", toolMemory}},
		"a supervisor":          {api.SessionSpec{Profile: odrade, Roster: "Delegate to vera."}, []string{"read", "write", "edit", "bash", toolDelegate, toolClose, toolMemory}},
		"a thread's specialist": {api.SessionSpec{Profile: vera, Thread: 3}, []string{"read", "bash", toolMessage, toolMemory}},
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
	calls  []api.TeamCall
	d      []team.Delegation
	memory []team.Memory
	fail   error
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

func (f *fakeTeam) TeamMemory(_ context.Context, call api.TeamCall, m team.Memory) (string, error) {
	f.calls, f.memory = append(f.calls, call), append(f.memory, m)
	return "Saved L1.", f.fail
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
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RECORD\"\necho 0 >&3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Sandbox: &Sandbox{Bwrap: fake, Shell: "/bin/sh"}}, commands: map[string]string{"PATH": os.Getenv("PATH"), "RECORD": record}}
	if _, err := e.Git(context.Background(), "/src/example", "worktree", "add", "-b", "vera/x-t1", "/src/example--vera-x-t1", "HEAD"); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(argv); !strings.HasPrefix(got, "--dev-bind\n/\n/\n") || !strings.HasSuffix(got, "--\n"+shell()+"\n-c\n"+gitScript+"\nsh\n-C\n/src/example\nworktree\nadd\n-b\nvera/x-t1\n/src/example--vera-x-t1\nHEAD\n") {
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
	// Output past the bound is an error, never a cut answer.
	long := filepath.Join(dir, "ok")
	if err := os.WriteFile(long, []byte("#!/bin/sh\nhead -c 3000000 /dev/zero | tr '\\0' o\necho 0 >&3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.opts.Sandbox.Bwrap = long
	if out, err := e.Git(context.Background(), dir, "worktree", "list", "--porcelain"); err == nil || !strings.Contains(err.Error(), "more than") || out != "" {
		t.Fatalf("a cut answer: %d bytes, %v", len(out), err)
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
	defer server.Stop() // no delivery pass outlives the test
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
	if open.Agent != "vera" || open.Supervisor != supervisor.ID || !strings.HasPrefix(open.Branch, "vera/fix-ci-t1-") ||
		open.Cwd != checkout+"--"+strings.ReplaceAll(open.Branch, "/", "-") {
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

// Placed tells, by request key, a message in the transcript from one
// queued behind a busy run, one an abort withdrew and one never sent.
// pocketd counts an answer, a report or a brief delivered only when
// placed.
func TestPlacedFollowsASubmission(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "true"}, "b1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	other, _ := e.CreateSession(ctx, t.Name()+"-other", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "k:first", "first"); err != nil {
		t.Fatal(err)
	}
	f.nextApproval() // the run waits: the session is busy
	if busy, err := e.Busy(ctx, id); err != nil || !busy {
		t.Fatalf("a running session: busy %v, %v", busy, err)
	}
	if _, err := e.Send(ctx, id, "k:second", "second"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]api.Placement{"k:first": api.PlacementPlaced, "k:second": api.PlacementQueued, "k:never": api.PlacementAbsent} {
		if got, err := e.Placed(ctx, id, key); err != nil || got != want {
			t.Fatalf("%s: %v %v, want %v", key, got, err, want)
		}
	}
	// The key proves the message: another session's same key is not it.
	if got, err := e.Placed(ctx, other, "k:first"); err != nil || got != api.PlacementAbsent {
		t.Fatalf("another session's key: %v %v", got, err)
	}
	if err := e.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	if got, err := e.Placed(ctx, id, "k:second"); err != nil || got != api.PlacementWithdrawn {
		t.Fatalf("after the abort: %v %v", got, err)
	}
	if busy, err := e.Busy(ctx, id); err != nil || busy {
		t.Fatalf("an idle session: busy %v, %v", busy, err)
	}
}

// A message a failed run left queued, with the session idle, is withdrawn
// while still queued and sent again as a new input: its run starts from
// its own entry, so an abort aimed at the failed run never matches it.
func TestAStrandedMessageIsWithdrawnAndSentAgain(t *testing.T) {
	f := newFixture(t)
	failed := ai.FauxStaticStep(ai.FauxResponse{StopReason: "error", ErrorMessage: "the provider is down"})
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "true"}, "b1"), failed,
		toolTurn("bash", map[string]any{"command": "true"}, "b2"), answer("done")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "k:first", "first"); err != nil {
		t.Fatal(err)
	}
	failedFirst, busy, err := e.RunIdentity(ctx, id)
	if err != nil || !busy {
		t.Fatalf("the first run: %d %v %v", failedFirst, busy, err)
	}
	request := f.nextApproval()
	if _, err := e.Send(ctx, id, "ask:1:answer", "The owner answered A1"); err != nil {
		t.Fatal(err)
	}
	if withdrawn, err := e.Withdraw(ctx, id, "k:first"); err != nil || withdrawn {
		t.Fatalf("a placed message was withdrawn: %v %v", withdrawn, err)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	if got, _ := e.Placed(ctx, id, "ask:1:answer"); got != api.PlacementQueued {
		t.Fatalf("after the failed run: %v", got)
	}
	if withdrawn, err := e.Withdraw(ctx, id, "ask:1:answer"); err != nil || !withdrawn {
		t.Fatalf("withdraw: %v %v", withdrawn, err)
	}
	if got, _ := e.Placed(ctx, id, "ask:1:answer"); got != api.PlacementWithdrawn {
		t.Fatalf("after the withdrawal: %v", got)
	}
	if _, err := e.Send(ctx, id, "ask:1:answer:1", "The owner answered A1"); err != nil {
		t.Fatal(err)
	}
	f.nextApproval() // the new run waits: it is busy
	if got, _ := e.Placed(ctx, id, "ask:1:answer:1"); got != api.PlacementPlaced {
		t.Fatalf("the answer sent again: %v", got)
	}
	if first, busy, err := e.RunIdentity(ctx, id); err != nil || !busy || first == failedFirst {
		t.Fatalf("the new run is %d (busy %v, %v); the failed run was %d", first, busy, err, failedFirst)
	}
}

// git's answers keep their spaces: a checkout whose name ends in one is a
// checkout, and its worktree is made.
func TestGitKeepsSpacesInPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := &Engine{opts: Options{Unsandboxed: true}, commands: allowedEnv()}
	isolateGit(e)
	checkout := filepath.Join(t.TempDir(), "example ")
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
	if err := worktree.Add(context.Background(), worktree.Tools{Git: e.Git, Kind: e.Kind, Read: e.Read, Same: e.Same}, checkout, checkout+"--vera-fix-t1", "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
}

// The sandbox's checks get a tool command's environment, never pocketd's:
// a provider key in pocketd's environment never reaches them.
func TestTheSandboxChecksGetNoServiceEnvironment(t *testing.T) {
	t.Setenv("POCKETD_TEST_PROVIDER_KEY", "sk-not-for-agents")
	e := &Engine{opts: Options{Unsandboxed: true}, commands: allowedEnv()}
	cmd, err := e.sandboxed(context.Background(), []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "POCKETD_TEST_PROVIDER_KEY=") {
			t.Fatalf("the check's environment carries %q", kv)
		}
	}
}

// A sandbox that fails to start is an error, never an answer: the
// existence check says present or absent only when the probe said so.
func TestABrokenSandboxIsNoAnswer(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Sandbox: &Sandbox{Bwrap: broken, Shell: "/bin/sh"}}, commands: map[string]string{"PATH": os.Getenv("PATH")}}
	if _, err := e.Exists(context.Background(), "/anything"); err == nil {
		t.Fatal("a sandbox that did not start answered")
	}
	if _, _, err := e.Read(context.Background(), "/anything"); err == nil {
		t.Fatal("a sandbox that did not start read")
	}
}

// A shell that cannot start git (not found, not executable) is no answer
// from git: its 127 or 126 is not a worktree.GitExit, so a check that
// could not run never reads as one that said no (Codex review round 25).
func TestGitThatCannotStartIsNoAnswer(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{opts: Options{Unsandboxed: true}, commands: map[string]string{"PATH": dir}}
	_, err := e.Git(context.Background(), dir, "rev-parse", "--show-toplevel")
	var exit *worktree.GitExit
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("git not found: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("not a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = e.Git(context.Background(), dir, "rev-parse", "--show-toplevel")
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("git not executable: %v", err)
	}
}

// git ended by a signal (the out-of-memory killer, say) is no answer from
// git: the shell's 128+n is not a worktree.GitExit (Codex review round 26).
func TestGitEndedBySignalIsNoAnswer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Unsandboxed: true}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	_, err := e.Git(context.Background(), dir, "rev-parse", "--show-toplevel")
	var exit *worktree.GitExit
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("git killed: %v", err)
	}
	// git's own fatal answer is one.
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 128\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = e.Git(context.Background(), dir, "rev-parse", "--show-toplevel")
	if !errors.As(err, &exit) || exit.Status != 128 {
		t.Fatalf("git's fatal answer: %v", err)
	}
}

// A session's lessons are the end of its instructions, from the marker
// pocketd owns: SetLessons replaces that part, keeps the rest, and leaves
// instructions that end so already as they are (ADR-0015).
func TestSetLessonsReplacesOnlyTheLessons(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Identity: "# Vera\nShe tests.", Tools: []string{"read"}}
	first := team.LessonsSection(nil)
	id, err := e.CreateSession(ctx, "s1", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera, Lessons: first})
	if err != nil {
		t.Fatal(err)
	}
	instructions := func() string {
		conversation, _ := e.conversation(ctx, id)
		agent, _ := conversation.Agent(ctx)
		return *agent.Instructions
	}
	before := instructions()
	if !strings.HasSuffix(before, first) || !strings.Contains(before, "She tests.") {
		t.Fatalf("instructions:\n%s", before)
	}
	second := team.LessonsSection([]team.Lesson{{ID: 4, Text: "Run the tests."}})
	if err := e.SetLessons(ctx, id, second); err != nil {
		t.Fatal(err)
	}
	after := instructions()
	if after != strings.TrimSuffix(before, first)+second || strings.Count(after, profiles.LessonsMarker) != 1 {
		t.Fatalf("instructions after:\n%s", after)
	}
	// The same section again changes nothing.
	if err := e.SetLessons(ctx, id, second); err != nil {
		t.Fatal(err)
	}
	if again := instructions(); again != after {
		t.Fatalf("the same lessons changed the instructions:\n%s", again)
	}
	// A plain session's instructions take a section at their end.
	plain, err := e.CreateSession(ctx, "s2", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetLessons(ctx, plain, second); err != nil {
		t.Fatal(err)
	}
	conversation, _ := e.conversation(ctx, plain)
	plainAgent, _ := conversation.Agent(ctx)
	if !strings.HasSuffix(*plainAgent.Instructions, second) {
		t.Fatalf("a plain session's instructions:\n%s", *plainAgent.Instructions)
	}
}

// team_memory reaches the team with its arguments, keyed by the call, from
// an agent's session (ADR-0015).
func TestTeamMemoryCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	args := map[string]any{"action": "add", "text": "Run the tests."}
	e := f.open(fauxModels(toolTurn(toolMemory, args, "m1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "remember", "remember"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0].Session != id || fake.calls[0].CallID != "m1" ||
		fake.memory[0] != (team.Memory{Action: team.MemoryAdd, Text: "Run the tests."}) {
		t.Fatalf("calls %+v %+v", fake.calls, fake.memory)
	}
}

// A lesson goes from an agent's team_memory call, through the owner's
// approval over HTTP, into the instructions of the agent's running
// session, with the real engine (ADR-0015).
func TestALessonEndToEndOverHTTP(t *testing.T) {
	ctx := context.Background()
	dataDir, profileDir, home := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan\n\nHe tends the garden.\n"), 0o600); err != nil {
		t.Fatal(err)
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
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approvals.NeedsApproval, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(toolTurn(toolMemory, map[string]any{"action": "add", "text": "Water the roses at dawn."}, "m1"), answer("Saved; it waits for the owner.")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	server.SetEngine(engine)
	engine.SetTeam(server)
	engine.Resume()
	defer server.Stop()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, _ := proto.NewClient(httpServer.URL, token, nil)

	session, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	instructions := func() string {
		t.Helper()
		conversation, err := engine.conversation(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		agent, err := conversation.Agent(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return *agent.Instructions
	}
	if text := instructions(); !strings.Contains(text, profiles.LessonsMarker) || !strings.Contains(text, "none yet") || !strings.Contains(text, "He tends the garden.") {
		t.Fatalf("a new session's instructions:\n%s", text)
	}
	if _, err := client.Send(ctx, session.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Remember the roses."}); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	lessons, err := client.Lessons(ctx)
	if err != nil || len(lessons) != 1 || lessons[0].State != proto.LessonPending || lessons[0].Text != "Water the roses at dawn." {
		t.Fatalf("lessons %+v, %v", lessons, err)
	}
	if strings.Contains(instructions(), "roses") {
		t.Fatal("a lesson reached the session before the owner approved it")
	}
	if _, err := client.DecideLesson(ctx, lessons[0].ID, proto.DecideLesson{RequestID: proto.NewRequestID(), Decision: proto.LessonApprove, Text: lessons[0].Text}); err != nil {
		t.Fatal(err)
	}
	if err := server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	text := instructions()
	if !strings.Contains(text, "- [L1] Water the roses at dawn.") || !strings.Contains(text, "He tends the garden.") || strings.Count(text, profiles.LessonsMarker) != 1 {
		t.Fatalf("instructions after the approval:\n%s", text)
	}
}

// A session made before lessons, whose identity holds a heading like the
// start of the lessons section, keeps all of its instructions: the first
// refresh adds the section at their end (Codex review of #18, round 2).
func TestSetLessonsKeepsAnOlderSessionsLookAlikeHeading(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	old := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleSupervisor, Tools: []string{"read"},
		Identity: "# Vera\n\n" + profiles.LessonsMarker + " (her own notes)\n\nShe tests."}
	id, err := e.CreateSession(ctx, "s1", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: old, Roster: "Delegate to duncan."})
	if err != nil {
		t.Fatal(err)
	}
	conversation, _ := e.conversation(ctx, id)
	agent, _ := conversation.Agent(ctx)
	before := *agent.Instructions
	section := team.LessonsSection([]team.Lesson{{ID: 1, Text: "Run the tests."}})
	if err := e.SetLessons(ctx, id, section); err != nil {
		t.Fatal(err)
	}
	agent, _ = conversation.Agent(ctx)
	if *agent.Instructions != before+section {
		t.Fatalf("instructions after the first refresh:\n%s", *agent.Instructions)
	}
}

// gh runs with gh's prompts, pager and colors off, and its answer comes
// back whole; a failure keeps what gh said (ADR-0016).
func TestGHRunsQuietlyAndKeepsItsError(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = fail ]; then echo 'HTTP 404: Not Found' >&2; exit 1; fi\n" +
		"echo \"prompt=$GH_PROMPT_DISABLED pager=$GH_PAGER color=$NO_COLOR args=$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Unsandboxed: true, GH: filepath.Join(dir, "gh")}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	out, err := e.GH(context.Background(), "api", "repos/frostyard/x/pulls/3")
	if err != nil || out != "prompt=1 pager=cat color=1 args=api repos/frostyard/x/pulls/3\n" {
		t.Fatalf("gh: %q, %v", out, err)
	}
	if _, err := e.GH(context.Background(), "fail"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("a failing gh: %v", err)
	}
	if _, err := (&Engine{opts: Options{GH: filepath.Join(dir, "gh")}}).GH(context.Background(), "api"); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("gh without a sandbox: %v", err)
	}
}

// gh is never found on the PATH: a gh the service user can change, as one
// in a folder of its own, is not GitHub's answer (Codex review of #20,
// round 1).
func TestGHIsATrustedProgram(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "gh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"state\":\"closed\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := &Engine{opts: Options{Unsandboxed: true}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	out, err := e.GH(context.Background(), "api", "repos/frostyard/x/pulls/3")
	if err == nil && strings.Contains(out, "closed") {
		t.Fatalf("the gh on the PATH answered: %q", out)
	}
	if program, err := trustedGH(); err == nil && strings.HasPrefix(program, dir) {
		t.Fatalf("trustedGH found %s", program)
	}
}
