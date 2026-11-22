package pig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store/storetest"
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

// The team tools go by role (ADR-0014, ADR-0015, ADR-0019): a supervisor
// delegates and closes, a thread's specialist reports, a cross-review
// files its verdict with read-only tools, every other session started
// from a profile keeps lessons, and a plain session gets none.
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
		"a plain session": {api.SessionSpec{}, []string{"read", "write", "edit", "bash"}},
		"an agent":        {api.SessionSpec{Profile: vera}, []string{"read", "bash", toolMemory}},
		"a supervisor":    {api.SessionSpec{Profile: odrade, Roster: "Delegate to vera."}, []string{"read", "write", "edit", "bash", toolDelegate, toolClose, toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolInbox, toolMemory, toolQuestion}},
		// Only a cross-review pocketd started files a verdict, with read-only
		// tools, whatever its profile allows (ADR-0019).
		// A thread's specialist has a worker, which has its coding tools
		// and no team tool (ADR-0021).
		"a thread's specialist": {api.SessionSpec{Profile: vera, Thread: 3}, []string{"read", "bash", toolMessage, toolWorker, toolMemory}},
		"a worker":              {api.SessionSpec{Thread: 3, Worker: true, WorkerTools: []string{"read", "bash", toolMessage}}, []string{"read", "bash"}},
		"a worker of all tools": {api.SessionSpec{Thread: 3, Worker: true}, []string{"read", "write", "edit", "bash"}},
		// It reads GitHub with GET alone too, for what a pin bump brings in,
		// and still has no shell (ADR-0057).
		"a cross-review": {api.SessionSpec{Profile: &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent}, Thread: 4, Review: true},
			[]string{"read", toolVerdict, toolDiff, toolGitHub}},
		"a cross-review of a profile with bash": {api.SessionSpec{Profile: vera, Thread: 7, Review: true}, []string{"read", toolVerdict, toolDiff, toolGitHub}},
		// A plan's lead reads files and GitHub, and builds nothing: no
		// bash, write or edit, whatever its profile (Codex review of #25,
		// round 2), and no worker, which would build for it (ADR-0020,
		// ADR-0036).
		"a plan's lead": {api.SessionSpec{Profile: vera, Thread: 4, Plan: 1}, []string{"read", toolMessage, toolPlan, toolGitHub, toolMemory}},
		"an all-tools plan's lead": {api.SessionSpec{Profile: &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}, Thread: 6, Plan: 3},
			[]string{"read", toolMessage, toolPlan, toolGitHub, toolMemory}},
		// A consult reads files and GitHub, and files its finding: nothing
		// else, whatever its profile or its other roles (ADR-0020,
		// ADR-0036).
		"a consult":            {api.SessionSpec{Profile: vera, Consult: 2}, []string{"read", toolFinding, toolGitHub}},
		"an all-tools consult": {api.SessionSpec{Profile: &profiles.Profile{ID: "irulan", Role: profiles.RoleAgent}, Consult: 3}, []string{"read", toolFinding, toolGitHub}},
		"a consult with no read": {api.SessionSpec{Profile: &profiles.Profile{ID: "teg", Role: profiles.RoleAgent, Tools: []string{"bash"}}, Consult: 4},
			[]string{toolFinding, toolGitHub}},
		"a consult of a supervisor's kind": {api.SessionSpec{Profile: odrade, Thread: 5, Plan: 2, Consult: 5}, []string{"read", toolFinding, toolGitHub}},
		// A gather's agent reads files and GitHub, and files its proposals:
		// nothing else, whatever its profile (ADR-0033). team_github is
		// its, a consult's, a plan's lead's and a cross-review's: no other
		// role here has it (ADR-0036, ADR-0057).
		"a gather's agent":            {api.SessionSpec{Profile: vera, Gather: 2}, []string{"read", toolPropose, toolGitHub}},
		"an all-tools gather's agent": {api.SessionSpec{Profile: &profiles.Profile{ID: "irulan", Role: profiles.RoleAgent}, Gather: 3}, []string{"read", toolPropose, toolGitHub}},
		"a gather's agent with no read": {api.SessionSpec{Profile: &profiles.Profile{ID: "teg", Role: profiles.RoleAgent, Tools: []string{"bash"}}, Gather: 4},
			[]string{toolPropose, toolGitHub}},
		"a gather's agent of a supervisor's kind": {api.SessionSpec{Profile: odrade, Gather: 5}, []string{"read", toolPropose, toolGitHub}},
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
	// worker are the team_worker messages, in order.
	worker   []string
	calls    []api.TeamCall
	d        []team.Delegation
	memory   []team.Memory
	verdict  []team.Verdict
	plans    []team.PlanCall
	findings []team.Finding
	// sends are the team_send calls, in order.
	sends []team.Send
	// schedules are the team_schedule calls, in order; scheduleResult is
	// what TeamSchedule answers, "scheduled" when empty. own are the
	// schedule calls (ADR-0060), which Schedule answers the same way.
	schedules      []schedule.Call
	own            []schedule.Call
	scheduleResult string
	// gathers, boards and proposals are the team_gather, team_board and
	// team_propose calls, in order; boardResult is what TeamBoard answers,
	// "board" when empty.
	gathers     []team.Gather
	boards      []team.BoardCall
	proposals   []team.ProposeCall
	boardResult string
	// reads are the team_github calls, in order; readResult is what
	// TeamGitHub answers, "{}" when empty.
	reads      []team.GitHubRead
	readResult string
	// launches are the team_launch calls, in order.
	launches []team.LaunchCall
	// inboxes are the team_inbox calls, in order; inboxResult is what
	// TeamInbox answers, "inbox" when empty.
	inboxes     []api.TeamCall
	inboxResult string
	fail        error
	// diff is what TeamDiff answers, a short patch when "".
	diff string
	// planResult is team_plan's answer, "Wrote plan P1." when empty.
	planResult string
	// refuse is what TeamAllows answers; allowed are the sessions it was
	// asked about, in order.
	refuse  error
	allowed []int64
	// questions are the question calls, in order, under mu: a call that
	// waits runs beside the test.
	mu             sync.Mutex
	questions      []team.Question
	questionResult string
	questionWait   chan struct{}
	// questionCalled, when set, gets a note at each question call.
	questionCalled chan struct{}
}

func (f *fakeTeam) TeamAllows(_ context.Context, session int64) error {
	f.allowed = append(f.allowed, session)
	return f.refuse
}

func (f *fakeTeam) TeamPlan(_ context.Context, call api.TeamCall, c team.PlanCall) (string, error) {
	f.calls, f.plans = append(f.calls, call), append(f.plans, c)
	if f.planResult != "" {
		return f.planResult, f.fail
	}
	return "Wrote plan P1.", f.fail
}

func (f *fakeTeam) TeamFinding(_ context.Context, call api.TeamCall, finding team.Finding) (string, error) {
	f.calls, f.findings = append(f.calls, call), append(f.findings, finding)
	return "Filed K1.", f.fail
}

func (f *fakeTeam) TeamVerdict(_ context.Context, call api.TeamCall, v team.Verdict) (string, error) {
	f.calls, f.verdict = append(f.calls, call), append(f.verdict, v)
	return "Recorded R1.", f.fail
}

func (f *fakeTeam) TeamGather(_ context.Context, call api.TeamCall, g team.Gather) (string, error) {
	f.calls = append(f.calls, call)
	f.gathers = append(f.gathers, g)
	return "gathering", f.fail
}

func (f *fakeTeam) TeamBoard(_ context.Context, call api.TeamCall, c team.BoardCall) (string, error) {
	f.calls = append(f.calls, call)
	f.boards = append(f.boards, c)
	if f.boardResult != "" {
		return f.boardResult, f.fail
	}
	return "board", f.fail
}

func (f *fakeTeam) TeamLaunch(_ context.Context, call api.TeamCall, c team.LaunchCall) (string, error) {
	f.calls = append(f.calls, call)
	f.launches = append(f.launches, c)
	return "launched", f.fail
}

// Question records the question calls and answers questionResult, `{"answer":"yes"}`
// when empty; questionWait, when set, holds the call until it closes or
// ctx ends, as a question that waits for the owner (ADR-0048).
func (f *fakeTeam) Question(ctx context.Context, call api.TeamCall, q team.Question) (string, error) {
	f.mu.Lock()
	f.calls, f.questions = append(f.calls, call), append(f.questions, q)
	wait := f.questionWait
	f.mu.Unlock()
	if f.questionCalled != nil {
		select {
		case f.questionCalled <- struct{}{}:
		default:
		}
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.questionResult != "" {
		return f.questionResult, f.fail
	}
	return `{"answer":"yes"}`, f.fail
}

func (f *fakeTeam) TeamInbox(_ context.Context, call api.TeamCall) (string, error) {
	f.calls = append(f.calls, call)
	f.inboxes = append(f.inboxes, call)
	if f.inboxResult != "" {
		return f.inboxResult, f.fail
	}
	return "inbox", f.fail
}

func (f *fakeTeam) TeamPropose(_ context.Context, call api.TeamCall, c team.ProposeCall) (string, error) {
	f.calls = append(f.calls, call)
	f.proposals = append(f.proposals, c)
	return "proposed", f.fail
}

func (f *fakeTeam) TeamGitHub(_ context.Context, call api.TeamCall, r team.GitHubRead) (string, error) {
	f.calls = append(f.calls, call)
	f.reads = append(f.reads, r)
	if f.readResult != "" {
		return f.readResult, f.fail
	}
	return "{}", f.fail
}

func (f *fakeTeam) TeamSchedule(_ context.Context, call api.TeamCall, c schedule.Call) (string, error) {
	f.calls = append(f.calls, call)
	f.schedules = append(f.schedules, c)
	if f.scheduleResult != "" {
		return f.scheduleResult, f.fail
	}
	return "scheduled", f.fail
}

func (f *fakeTeam) Schedule(_ context.Context, call api.TeamCall, c schedule.Call) (string, error) {
	f.calls = append(f.calls, call)
	f.own = append(f.own, c)
	if f.scheduleResult != "" {
		return f.scheduleResult, f.fail
	}
	return "scheduled", f.fail
}

func (f *fakeTeam) TeamSend(_ context.Context, call api.TeamCall, m team.Send) (string, error) {
	f.calls, f.sends = append(f.calls, call), append(f.sends, m)
	return "Delivered to thread T5.", f.fail
}

func (f *fakeTeam) TeamWorker(_ context.Context, call api.TeamCall, w team.WorkerMessage) (string, error) {
	f.calls = append(f.calls, call)
	f.worker = append(f.worker, w.Message)
	return "worker " + w.Message, f.fail
}

func (f *fakeTeam) TeamDiff(_ context.Context, call api.TeamCall, d team.DiffRequest) (string, error) {
	f.calls = append(f.calls, call)
	if f.diff != "" {
		return f.diff, f.fail
	}
	return "diff --git a/x b/x", f.fail
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
	program, err := trustedGit()
	if err != nil {
		t.Skip(err)
	}
	if got := string(argv); !strings.HasPrefix(got, "--dev-bind\n/\n/\n") || !strings.HasSuffix(got, "--\n"+shell()+"\n-c\n"+gitScript+"\nsh\n"+program+"\n-C\n/src/example\nworktree\nadd\n-b\nvera/x-t1\n/src/example--vera-x-t1\nHEAD\n") {
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
	s := storetest.Open(t, filepath.Join(dataDir, "work.sqlite"), nil)
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
	if _, err := worktree.Add(context.Background(), worktree.Tools{Git: e.Git, Kind: e.Kind, Read: e.Read, Same: e.Same}, checkout, checkout+"--vera-fix-t1", "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
}

// MakeDir makes a folder once: a folder, a file or a link there already is
// no folder made, and a folder it cannot make is an error, never an
// answer (ADR-0027; Codex review of #46, round 4).
func TestMakeDirMakesAFolderOnce(t *testing.T) {
	e := &Engine{opts: Options{Unsandboxed: true}, commands: allowedEnv()}
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "chairlift--owner-session-qlz6tr")
	if made, err := e.MakeDir(ctx, path); err != nil || !made {
		t.Fatalf("a new folder: %v %v", made, err)
	}
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		t.Fatalf("the folder: %v %v", info, err)
	}
	if made, err := e.MakeDir(ctx, path); err != nil || made {
		t.Fatalf("the folder again: %v %v", made, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if made, err := e.MakeDir(ctx, link); err != nil || made {
		t.Fatalf("a dangling link: %v %v", made, err)
	}
	if _, err := e.MakeDir(ctx, filepath.Join(dir, "no", "parent")); err == nil {
		t.Fatal("a folder without its parent answered")
	}
	// The mkdir on the PATH is never run: a fake one there that says it
	// made a folder that was there is not believed (Codex review of #46,
	// round 5).
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "mkdir"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.commands = map[string]string{"PATH": fake + ":" + e.commands["PATH"]}
	if made, err := e.MakeDir(ctx, path); err != nil || made {
		t.Fatalf("with a fake mkdir on the PATH, the folder there again: %v %v", made, err)
	}
}

// Real resolves a folder as the sandbox does, every link on its way
// followed, and says when a path names no folder (ADR-0027; Codex review
// of #46, round 5).
func TestRealResolvesAFoldersLinks(t *testing.T) {
	e := &Engine{opts: Options{Unsandboxed: true}, commands: allowedEnv()}
	ctx := context.Background()
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := e.Real(ctx, filepath.Join(dir, "link")); err != nil || !ok || got != filepath.Join(real, "a") {
		t.Fatalf("Real through a link = %q %v %v, want %q", got, ok, err, filepath.Join(real, "a"))
	}
	if _, ok, err := e.Real(ctx, filepath.Join(dir, "absent")); err != nil || ok {
		t.Fatalf("Real of no folder = %v %v", ok, err)
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
	e := &Engine{opts: Options{Unsandboxed: true, Git: filepath.Join(dir, "git")}, commands: map[string]string{"PATH": dir}}
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
	e := &Engine{opts: Options{Unsandboxed: true, Git: filepath.Join(dir, "git")}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
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

// A supervisor's team_schedule call reaches the team, keyed by its
// session, its task and its call's id (ADR-0030).
func TestTeamScheduleCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	args := map[string]any{"action": "add", "when": "every day 19:00", "message": "Look at the desk."}
	e := f.open(fauxModels(toolTurn(toolSchedule, args, "s1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "remind", "remind me"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	want := schedule.Call{Action: schedule.ActionAdd, When: "every day 19:00", Message: "Look at the desk."}
	if len(fake.calls) != 1 || fake.calls[0].Session != id || fake.calls[0].CallID != "s1" || fake.calls[0].Task == 0 || fake.schedules[0] != want {
		t.Fatalf("calls %+v %+v", fake.calls, fake.schedules)
	}
}

// team_gather, team_board and team_propose reach the team, keyed by the
// calling session, its task and the call's id (ADR-0033), a proposal's ci
// included (ADR-0047).
func TestGatherToolsCallTheTeam(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(
		toolTurn(toolGather, map[string]any{"skill": "grooming", "agents": []any{"duncan"}, "note": "Evening."}, "g1"), answer("ok"),
		toolTurn(toolBoard, map[string]any{"action": "order", "rows": []any{3, 1}}, "b1"), answer("ok"),
		toolTurn(toolBoard, map[string]any{"action": "edit", "row": 3, "rev": 1, "ci": true}, "b2"), answer("ok"),
		toolTurn(toolPropose, map[string]any{"action": "add", "title": "Fix A", "task": "Do it.", "stop_when": "Done.", "max_rounds": 5, "ci": true}, "p1"), answer("ok"),
	))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	odrade, err := e.CreateSession(ctx, "odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"gather", "order", "edit"} {
		if _, err := e.Send(ctx, odrade, text, text); err != nil {
			t.Fatal(err)
		}
		if err := e.WaitIdle(ctx, odrade); err != nil {
			t.Fatal(err)
		}
	}
	asked, err := e.CreateSession(ctx, "ask", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}, Gather: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, asked, "propose", "propose"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, asked); err != nil {
		t.Fatal(err)
	}
	if len(fake.gathers) != 1 || fake.gathers[0].Skill != "grooming" || fake.gathers[0].Note != "Evening." || len(fake.gathers[0].Agents) != 1 {
		t.Fatalf("gathers %+v", fake.gathers)
	}
	if len(fake.boards) != 2 || fake.boards[0].Action != team.BoardOrder || !slices.Equal(fake.boards[0].Rows, []int64{3, 1}) ||
		fake.boards[1].Action != team.BoardEdit || fake.boards[1].CI == nil || !*fake.boards[1].CI {
		t.Fatalf("boards %+v", fake.boards)
	}
	if len(fake.proposals) != 1 || fake.proposals[0].Action != team.ProposeAdd || *fake.proposals[0].Title != "Fix A" || *fake.proposals[0].MaxRounds != 5 || fake.proposals[0].CI == nil || !*fake.proposals[0].CI {
		t.Fatalf("proposals %+v", fake.proposals)
	}
	if len(fake.calls) != 4 || fake.calls[0].CallID != "g1" || fake.calls[0].Session != odrade || fake.calls[3].Session != asked || fake.calls[3].CallID != "p1" {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// A supervisor's team_send call reaches the team, keyed by its session,
// its task and its call's id, with the thread and the text (ADR-0032).
func TestTeamSendCallsTheTeam(t *testing.T) {
	f := newFixture(t)
	args := map[string]any{"thread": 5, "text": "core#281 took ADR-0088: renumber yours."}
	e := f.open(fauxModels(toolTurn(toolSend, args, "t1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "tell", "tell T5"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	want := team.Send{Thread: 5, Text: "core#281 took ADR-0088: renumber yours."}
	if len(fake.calls) != 1 || fake.calls[0].Session != id || fake.calls[0].CallID != "t1" || fake.calls[0].Task == 0 || len(fake.sends) != 1 || fake.sends[0] != want {
		t.Fatalf("calls %+v %+v", fake.calls, fake.sends)
	}
}

// A full page of team_schedule's list reaches the supervisor whole: PiG
// cuts no part of it (Codex review of #51, round 3).
func TestAScheduleListPageReachesTheSupervisorWhole(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolSchedule, map[string]any{"action": "list", "page": 2}, "l1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	var lines []schedule.Line
	for i := range 200 {
		lines = append(lines, schedule.Line{ID: schedule.ID(int64(i + 1)), When: "once", Zone: time.UTC, Text: strings.Repeat("é", 150)})
	}
	page, err := schedule.ListText(schedule.TeamTool, lines, 2)
	if err != nil || len(page) < schedule.MaxPageBytes-512 || !strings.Contains(page, "team_schedule list with page 3") {
		t.Fatalf("the page: %d bytes, %v", len(page), err)
	}
	fake := &fakeTeam{scheduleResult: page}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "list", "list"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil {
			if entry.ToolResult.Text != page || fake.schedules[0].Page != 2 {
				t.Fatalf("the page came through with %d of its %d bytes, page %d asked", len(entry.ToolResult.Text), len(page), fake.schedules[0].Page)
			}
			return
		}
	}
	t.Fatal("no tool result")
}

// A proposal read with team_board reaches the supervisor whole, at the
// bounds of its fields: each character four bytes, or each field nearly
// all line ends, the title's too (trimming takes only those at its ends).
// PiG cuts no part of it (Codex review of #57, rounds 2 and 3).
func TestABoardReadReachesTheSupervisorWhole(t *testing.T) {
	wide := func(n int) string { return strings.Repeat("\U0001F600", n) }
	tall := func(n int) string { return "x" + strings.Repeat("\n", n-2) + "x" }
	read := func(title, task, stop, note string) string {
		return "#3 rev 1 · duncan · home · grooming · 20 rounds · G1 · " + title + "\n\nTask:\n" + task + "\n\nStop when:\n" + stop + "\n\nNote: " + note
	}
	for name, text := range map[string]string{
		"wide": read(wide(team.MaxTitle), wide(team.MaxTask), wide(team.MaxProposalStop), wide(team.MaxProposalNote)),
		"tall": read(tall(team.MaxTitle), tall(team.MaxTask), tall(team.MaxProposalStop), tall(team.MaxProposalNote)),
	} {
		if got := boardReadThroughPiG(t, text); got != text {
			t.Errorf("%s: the read came through with %d of its %d bytes, %d of its %d lines", name, len(got), len(text), strings.Count(got, "\n")+1, strings.Count(text, "\n")+1)
		}
	}
}

// boardReadThroughPiG is what a supervisor's session gets from a
// team_board read the team answers with text.
func boardReadThroughPiG(t *testing.T, text string) string {
	t.Helper()
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolBoard, map[string]any{"action": "read", "row": 3}, "r1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	e.SetTeam(&fakeTeam{boardResult: text})
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "read", "read"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil {
			return entry.ToolResult.Text
		}
	}
	t.Fatal("no tool result")
	return ""
}

// A supervisor's session started before team_schedule gets it after its
// team_close, once; an agent's session never does (Codex review of #51,
// round 1).
func TestSetSupervisorToolsUpgradesAnOlderSupervisor(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}
	id, err := e.CreateSession(ctx, "older", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade})
	if err != nil {
		t.Fatal(err)
	}
	// As a session made before team_schedule was.
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var older []*durable.ToolRegistration
	for _, tool := range agent.Tools {
		if !slices.Contains(laterSupervisorTools, tool.Name) {
			older = append(older, tool)
		}
	}
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: older})}); err != nil {
		t.Fatal(err)
	}
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || !changed {
		t.Fatalf("the upgrade: %v %v", changed, err)
	}
	want := []string{"read", "write", "edit", "bash", toolDelegate, toolClose, toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolInbox, toolMemory, toolQuestion}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools %q, want %q", got, want)
	}
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || changed {
		t.Fatalf("again: %v %v", changed, err)
	}
	vera, err := e.CreateSession(ctx, "agent", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := e.SetSupervisorTools(ctx, vera); err != nil || changed {
		t.Fatalf("an agent's session: %v %v", changed, err)
	}
}

// A supervisor's session started after team_schedule and before team_send
// gets team_send after its team_schedule, once, and keeps every other tool
// where it is (ADR-0032).
func TestSetSupervisorToolsUpgradesASupervisorWithItsSchedules(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}
	id, err := e.CreateSession(ctx, "older", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var older []*durable.ToolRegistration
	for _, tool := range agent.Tools {
		if tool.Name != toolSend {
			older = append(older, tool)
		}
	}
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: older})}); err != nil {
		t.Fatal(err)
	}
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || !changed {
		t.Fatalf("the upgrade: %v %v", changed, err)
	}
	want := []string{"read", "write", "edit", "bash", toolDelegate, toolClose, toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolInbox, toolMemory, toolQuestion}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools %q, want %q", got, want)
	}
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || changed {
		t.Fatalf("again: %v %v", changed, err)
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
	s := storetest.Open(t, filepath.Join(dataDir, "work.sqlite"), nil)
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

// gh runs with gh's prompts, pager and colors off, an empty configuration
// folder of pocketd's, the host github.com and the service user's token,
// and its answer comes back whole; a failure keeps what gh said
// (ADR-0016).
func TestGHRunsQuietlyAndKeepsItsError(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo tok-123; exit 0; fi\n" +
		"if [ \"$1\" = fail ]; then echo 'HTTP 404: Not Found' >&2; exit 1; fi\n" +
		"echo \"prompt=$GH_PROMPT_DISABLED pager=$GH_PAGER color=$NO_COLOR config=$GH_CONFIG_DIR host=$GH_HOST token=$GH_TOKEN args=$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Unsandboxed: true, GH: filepath.Join(dir, "gh"), DataDir: data}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	out, err := e.GH(context.Background(), "api", "repos/frostyard/x/pulls/3")
	if want := "prompt=1 pager=cat color=1 config=" + data + " host=github.com token=tok-123 args=api repos/frostyard/x/pulls/3\n"; err != nil || out != want {
		t.Fatalf("gh: %q, %v", out, err)
	}
	if _, err := e.GH(context.Background(), "fail"); err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "tok-123") {
		t.Fatalf("a failing gh: %v", err)
	}
	if _, err := (&Engine{opts: Options{GH: filepath.Join(dir, "gh")}}).GH(context.Background(), "api"); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("gh without a sandbox: %v", err)
	}
}

// gh is never found on the PATH, and a gh the service user can change,
// as one in a folder of its own, is never run: its answer would stand for
// GitHub's (Codex review of #20, rounds 1 and 2). No real gh runs here.
func TestGHIsATrustedProgram(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "gh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	saved := ghPaths
	defer func() { ghPaths = saved }()
	ghPaths = []string{fake, filepath.Join(dir, "missing", "gh")}
	if _, err := trustedGH(); err == nil || !strings.Contains(err.Error(), "other than root") {
		t.Fatalf("a gh the service user owns: %v", err)
	}
	e := &Engine{opts: Options{Unsandboxed: true}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	if _, err := e.GH(context.Background(), "api", "repos/frostyard/x/pulls/3"); err == nil {
		t.Fatal("an untrusted gh answered")
	}
	ghPaths = []string{filepath.Join(dir, "missing", "gh")}
	if _, err := e.GH(context.Background(), "api"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("no gh installed: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the gh on the PATH ran")
	}
}

// git is never found on the PATH, and a git the service user can change
// is never run: its answers are a review's evidence and guard what
// pocketd hands an agent (Codex review of #26, round 9).
func TestGitIsATrustedProgram(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := gitPaths
	defer func() { gitPaths = saved }()
	gitPaths = []string{fake, filepath.Join(dir, "missing", "git")}
	if _, err := trustedGit(); err == nil || !strings.Contains(err.Error(), "other than root") {
		t.Fatalf("a git the service user owns: %v", err)
	}
	e := &Engine{opts: Options{Unsandboxed: true}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	if _, err := e.Git(context.Background(), dir, "version"); err == nil || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("an untrusted git answered: %v", err)
	}
	gitPaths = saved
	if _, err := trustedGit(); err != nil {
		t.Skip(err)
	}
	if out, err := e.Git(context.Background(), dir, "version"); err != nil || !strings.HasPrefix(out, "git version") {
		t.Fatalf("the system's git, with a fake one first on the PATH: %q, %v", out, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the git on the PATH ran")
	}
}

// A thread's session started before team_verdict existed gets it, right
// after team_message, once; its other tools stay, and a session that does
// not report gets none (ADR-0017, Codex review of #23, round 2).
func TestAnOlderSpecialistSessionGetsTheVerdictTool(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Tools: []string{"read", "bash"}}
	specialist, err := e.CreateSession(ctx, "specialist", api.SessionSpec{Profile: vera, Thread: 3, Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := e.CreateSession(ctx, "agent", api.SessionSpec{Profile: vera, Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	// As a session of version 6 has it: without team_verdict.
	conversation, err := e.conversation(ctx, specialist)
	if err != nil {
		t.Fatal(err)
	}
	old, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var list []*durable.ToolRegistration
	for _, tool := range old.Tools {
		if tool.Name != toolVerdict {
			list = append(list, tool)
		}
	}
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: list})}); err != nil {
		t.Fatal(err)
	}
	if got := agentTools(t, e, specialist); slices.Contains(got, toolVerdict) {
		t.Fatalf("the old session already has it: %q", got)
	}
	if added, err := e.SetVerdictTool(ctx, specialist, true); err != nil || !added {
		t.Fatalf("added %v, %v", added, err)
	}
	if got, want := agentTools(t, e, specialist), []string{"read", "bash", toolMessage, toolVerdict, toolWorker, toolMemory}; !slices.Equal(got, want) {
		t.Errorf("tools %q, want %q", got, want)
	}
	if added, err := e.SetVerdictTool(ctx, specialist, true); err != nil || added {
		t.Errorf("a second time: added %v, %v", added, err)
	}
	// A session that is no review's loses it, and nothing else (ADR-0019).
	if removed, err := e.SetVerdictTool(ctx, specialist, false); err != nil || !removed {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if got, want := agentTools(t, e, specialist), []string{"read", "bash", toolMessage, toolWorker, toolMemory}; !slices.Equal(got, want) {
		t.Errorf("tools after the removal %q, want %q", got, want)
	}
	if removed, err := e.SetVerdictTool(ctx, agent, false); err != nil || removed {
		t.Errorf("a session without it: removed %v, %v", removed, err)
	}
}

// A page of git's output holds exactly the bytes asked for, and stops git
// at the first byte past them, saying more follows; no size of the whole
// is needed (Codex review of #26, rounds 2 and 3).
func TestGitPageWindow(t *testing.T) {
	w := &window{from: 5, keep: 4}
	var err error
	for _, chunk := range []string{"012", "3456", "789", "abc"} {
		if _, err = w.Write([]byte(chunk)); err != nil {
			break
		}
	}
	if string(w.buf) != "5678" || !w.more || !errors.Is(err, errPageFull) {
		t.Fatalf("page %q, more %v, err %v", w.buf, w.more, err)
	}
	whole := &window{from: 0, keep: 4}
	if _, err := whole.Write([]byte("0123")); err != nil || whole.more || string(whole.buf) != "0123" {
		t.Fatalf("an answer that fits: %q, more %v, err %v", whole.buf, whole.more, err)
	}
}

// A whole team_diff page reaches the reviewer through PiG, its footer and
// next offset included, however many short lines it holds (Codex review of
// #26, round 3).
func TestATeamDiffPageReachesTheReviewerWhole(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolDiff, map[string]any{}, "d1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	page := strings.Repeat("+\n", 3000) + "[bytes 0 to 6000; ask again with offset 6000 for the rest]"
	fake := &fakeTeam{diff: page}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Thread: 4, Review: true,
		Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "review", "review"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil {
			if !strings.Contains(entry.ToolResult.Text, "ask again with offset 6000") {
				t.Fatalf("the page was cut: %d bytes reached the reviewer, ending %q", len(entry.ToolResult.Text),
					entry.ToolResult.Text[max(0, len(entry.ToolResult.Text)-80):])
			}
			return
		}
	}
	t.Fatal("no tool result")
}

// A page never ends inside a UTF-8 character (Codex review of #26, round
// 4).
func TestAPageEndsAtAWholeCharacter(t *testing.T) {
	b := []byte("ab\u00e9") // é is 2 bytes
	if got := wholeRunes(b[:3]); got != 2 {
		t.Fatalf("a page cut inside é keeps %d bytes, want 2", got)
	}
	if got := wholeRunes(b); got != len(b) {
		t.Fatalf("a whole page keeps %d bytes, want %d", got, len(b))
	}
	euro := []byte("\u20ac") // € is 3 bytes
	if got := wholeRunes(euro[:2]); got != 0 {
		t.Fatalf("a page cut inside € keeps %d bytes, want 0", got)
	}
}

// GitEach's splitter gives every record of a long output, keeping one at a
// time, and refuses a record past its bound (Codex review of #26, round 5).
func TestGitEachSplitsALongOutput(t *testing.T) {
	var got int
	s := &splitter{sep: 0, each: func(string) error { got++; return nil }}
	record := strings.Repeat("x", 100) + "\x00"
	for range 2000 { // 200 KB, past Git's bound
		if _, err := s.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
	}
	if got != 2000 || len(s.buf) != 0 {
		t.Fatalf("%d records, %d bytes kept", got, len(s.buf))
	}
	long := &splitter{sep: 0, each: func(string) error { return nil }}
	if _, err := long.Write([]byte(strings.Repeat("x", gitOutput+1))); err == nil {
		t.Fatal("a record past the bound was kept")
	}
}

// A worker works for its thread's specialist: its instructions are the
// worker's, with no profile's identity in them (ADR-0021).
func TestAWorkersInstructions(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "worker:x", api.SessionSpec{Cwd: f.work, Model: fauxRef, Thread: 3, Worker: true,
		Profile: &profiles.Profile{ID: "vera", Name: "Vera", Identity: "I am Vera, the frontier."}})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Instructions == nil || !strings.HasSuffix(*agent.Instructions, team.WorkerInstructions) || strings.Contains(*agent.Instructions, "I am Vera") {
		t.Fatalf("a worker's instructions:\n%v", agent.Instructions)
	}
}

// team_plan, team_finding and team_delegate's plan reach the team with
// their arguments as the model gave them (ADR-0020).
func TestPlanToolsCallTheTeam(t *testing.T) {
	f := newFixture(t)
	write := map[string]any{"action": "write", "text": "# Plan", "rev": strings.Repeat("a", 64), "asks": []any{3}}
	tasks := map[string]any{"action": "tasks", "tasks": []any{map[string]any{"id": "t1", "title": "Keys", "agent": "vera",
		"repository": "frostyard/example", "acceptance": []any{"Keys exist"}, "deps": []any{}}}}
	consult := map[string]any{"action": "consult", "agents": []any{"irulan"}, "question": "Holds?", "ask": 3}
	finding := map[string]any{"stance": "objects", "summary": "It does not."}
	start := map[string]any{"agent": "vera", "task": "Plan the signing", "plan": map[string]any{"start": true}}
	deliver := map[string]any{"agent": "vera", "task": "Build t1", "repository": "frostyard/example", "plan": map[string]any{"id": 1, "task": "t1"}}
	e := f.open(fauxModels(
		toolTurn(toolPlan, write, "p1"), answer("ok"), toolTurn(toolPlan, tasks, "p2"), answer("ok"), toolTurn(toolPlan, consult, "p3"), answer("ok"),
		toolTurn(toolFinding, finding, "f1"), answer("ok"),
		toolTurn(toolDelegate, start, "d1"), answer("ok"), toolTurn(toolDelegate, deliver, "d2"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{}
	e.SetTeam(fake)
	ctx := context.Background()
	vera := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}
	lead, err := e.CreateSession(ctx, "lead", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera, Thread: 4, Plan: 1})
	if err != nil {
		t.Fatal(err)
	}
	expert, err := e.CreateSession(ctx, "expert", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera, Consult: 1})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := e.CreateSession(ctx, "supervisor", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}})
	if err != nil {
		t.Fatal(err)
	}
	run := func(id int64, text string) {
		t.Helper()
		if _, err := e.Send(ctx, id, text, text); err != nil {
			t.Fatal(err)
		}
		if err := e.WaitIdle(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{"write", "tasks", "consult"} {
		run(lead, text)
	}
	run(expert, "finding")
	run(supervisor, "start")
	run(supervisor, "deliver")
	if len(fake.plans) != 3 {
		t.Fatalf("plan calls %+v", fake.plans)
	}
	if got := fake.plans[0]; got.Action != team.PlanWrite || got.Text != "# Plan" || got.Rev != strings.Repeat("a", 64) || !slices.Equal(got.Asks, []int64{3}) {
		t.Errorf("write %+v", got)
	}
	if got := fake.plans[1]; got.Action != team.PlanTasks || len(got.Tasks) != 1 || got.Tasks[0].Repository != "frostyard/example" || got.Tasks[0].Acceptance[0] != "Keys exist" {
		t.Errorf("tasks %+v", got)
	}
	if got := fake.plans[2]; got.Action != team.PlanConsult || !slices.Equal(got.Agents, []string{"irulan"}) || got.Question != "Holds?" || got.Ask != 3 {
		t.Errorf("consult %+v", got)
	}
	if len(fake.findings) != 1 || fake.findings[0] != (team.Finding{Stance: "objects", Summary: "It does not."}) {
		t.Errorf("findings %+v", fake.findings)
	}
	if len(fake.d) != 2 || fake.d[0].Plan == nil || !fake.d[0].Plan.Start || fake.d[1].Plan == nil || *fake.d[1].Plan != (team.PlanRef{ID: 1, Task: "t1"}) {
		t.Errorf("delegations %+v", fake.d)
	}
	for i, want := range []int64{lead, lead, lead, expert, supervisor, supervisor} {
		if fake.calls[i].Session != want {
			t.Errorf("call %d from session %d, want %d", i, fake.calls[i].Session, want)
		}
	}
}

// lastResult is the text of the transcript's last message when it is a
// tool result, else "".
func lastResult(transcript ai.TranscriptContext) string {
	messages := transcript.Messages()
	if len(messages) == 0 {
		return ""
	}
	m, ok := messages[len(messages)-1].(ai.ToolResultMessage)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, block := range m.Content {
		if text, ok := block.(ai.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// planText and planTextV2 are the plan the scripted lead writes, before
// and after the owner's answer.
const (
	planText   = "# Signing\n\nSign the ISO."
	planTextV2 = "# Signing\n\nSign the ISO with the TPM."
)

// planModel plays the four agents of a plan by what each was last told
// (ADR-0020): the supervisor starts the plan and delivers its ready task;
// the lead writes, consults, asks, writes the answer in, drafts the task
// and calls ready; the consult files its finding; the task's specialist
// reports it done.
func planModel(transcript ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
	call := func(tool string, args map[string]any, id string) (ai.AssistantMessage, error) {
		return ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxToolCall(tool, args, &ai.FauxToolCallOptions{ID: id})}, StopReason: "toolUse"}.AssistantMessage(), nil
	}
	say := func(text string) (ai.AssistantMessage, error) {
		return ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText(text)}}.AssistantMessage(), nil
	}
	if result := lastResult(transcript); result != "" {
		switch {
		case strings.HasPrefix(result, "Wrote plan P1: it is at rev "+team.ShortRev(team.Rev(planTextV2))):
			return call(toolPlan, map[string]any{"action": "tasks", "tasks": []any{map[string]any{"id": "t1", "title": "Sign the ISO", "agent": "vera",
				"repository": "frostyard/example", "acceptance": []any{"The ISO is signed"}}}}, "p-tasks")
		case strings.HasPrefix(result, "Wrote plan P1"):
			return call(toolPlan, map[string]any{"action": "consult", "agents": []any{"irulan"}, "question": "Do TPM keys survive a re-image?"}, "p-consult")
		case strings.HasPrefix(result, "Drafted tasks"):
			return call(toolPlan, map[string]any{"action": "ready"}, "p-ready")
		}
		return say("Done: " + result)
	}
	text, _ := lastText(transcript)
	switch {
	case strings.HasPrefix(text, "Plan the signing"):
		return call(toolDelegate, map[string]any{"agent": "duncan", "task": "Plan the signing of the ISO.", "title": "Signing",
			"plan": map[string]any{"start": true}}, "d-plan")
	case strings.Contains(text, "lead plan P1 in thread"):
		return call(toolPlan, map[string]any{"action": "write", "text": planText}, "p-write")
	case strings.Contains(text, "consults you as K1"):
		return call(toolFinding, map[string]any{"stance": "supports", "summary": "They survive."}, "f1")
	case strings.Contains(text, "[K1 · Irulan · consult on P1] supports"):
		return call(toolMessage, map[string]any{"message": "One decision.", "status": "working",
			"asks": []any{map[string]any{"question": "Which key?", "options": []any{"TPM", "File"}}}}, "m-ask")
	case strings.Contains(text, "The owner answered A1"):
		return call(toolPlan, map[string]any{"action": "write", "text": planTextV2, "rev": team.Rev(planText), "asks": []any{1}}, "p-write-2")
	case strings.Contains(text, "[P1 · approved]"):
		return call(toolDelegate, map[string]any{"agent": "vera", "task": "Sign the ISO.", "repository": "frostyard/example",
			"plan": map[string]any{"id": 1, "task": "t1"}}, "d-t1")
	case strings.Contains(text, "task t1 of plan P1"):
		return call(toolMessage, map[string]any{"message": "Signed; no change was needed.", "status": "done"}, "m-done")
	}
	return say("Noted: " + text)
}

// TestAPlanEndToEndOverHTTP wires the service as pocketd serve does and
// drives a plan through the protocol (ADR-0020): the owner asks the
// supervisor for a plan; the lead writes it, consults an expert, whose
// finding comes back, asks the owner, writes the answer in, drafts the
// task and calls ready. The owner approves it over the protocol; the
// supervisor is told once and delivers the task; its thread's close makes
// the plan done.
func TestAPlanEndToEndOverHTTP(t *testing.T) {
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
		"duncan.md": "---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan\n",
		"irulan.md": "---\nmodel: faux/faux-1\nhome: ~\n---\n# Irulan\n",
		"vera.md":   "---\nmodel: faux/faux-1\nrepositories:\n  frostyard/example: " + checkout + "\n---\n# Vera\n",
	} {
		if err := os.WriteFile(filepath.Join(profileDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := storetest.Open(t, filepath.Join(dataDir, "work.sqlite"), nil)
	defer func() { _ = s.Close() }()
	_, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(4096, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(s, hub, "e2e", nil, nil)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	server.SetProfiles(profiles.Folder{Dir: profileDir, Home: home})
	steps := make([]ai.FauxResponseStep, 64)
	for i := range steps {
		steps[i] = ai.FauxFactoryStep(planModel)
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
	defer server.Stop()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, _ := proto.NewClient(httpServer.URL, token, nil)

	streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream := make(chan proto.Event, 4096)
	go func() {
		_ = client.Events(streamCtx, 0, func(e proto.Event) error { stream <- e; return nil })
	}()
	var seen []proto.Event
	await := func(what string, match func(proto.Event) bool) proto.Event {
		t.Helper()
		for i := 0; ; i++ {
			for i >= len(seen) {
				select {
				case e := <-stream:
					seen = append(seen, e)
				case <-streamCtx.Done():
					for _, e := range seen {
						t.Log(summarize(e))
					}
					t.Fatalf("no %s event", what)
				}
			}
			if match(seen[i]) {
				return seen[i]
			}
		}
	}
	entryIn := func(session int64, kind proto.EntryKind, part string) func(proto.Event) bool {
		return func(e proto.Event) bool {
			return e.Type == proto.EventEntry && e.Entry.SessionID == session && e.Entry.Kind == kind && strings.Contains(e.Entry.Text, part)
		}
	}
	await("hello", func(e proto.Event) bool { return e.Type == proto.EventHello })
	supervisor, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "odrade"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(ctx, supervisor.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Plan the signing of the ISO."}); err != nil {
		t.Fatal(err)
	}
	planThread := await("the plan thread attached", func(e proto.Event) bool {
		return e.Type == proto.EventThread && e.Thread.Session != 0 && e.Thread.Plan == 1
	}).Thread
	if planThread.Agent != "duncan" || planThread.PlanTask != "" || planThread.Cwd != home {
		t.Fatalf("plan thread %+v", planThread)
	}
	ask := await("the lead's ask", func(e proto.Event) bool { return e.Type == proto.EventAsk && e.Ask.State == proto.AskOpen }).Ask
	if ask.SessionID != planThread.Session || ask.Question != "Which key?" {
		t.Fatalf("ask %+v", ask)
	}
	// The consult ran from irulan's profile, read-only, and its finding
	// reached the lead before the ask.
	plans, err := client.Plans(ctx)
	if err != nil || len(plans) != 1 || len(plans[0].Consults) != 1 || plans[0].Consults[0].State != proto.ConsultAnswered || plans[0].Consults[0].Stance != "supports" {
		t.Fatalf("plans %+v %v", plans, err)
	}
	if got := agentTools(t, engine, plans[0].Consults[0].Session); !slices.Equal(got, []string{"read", toolFinding, toolGitHub}) {
		t.Fatalf("the consult's tools %q", got)
	}
	if got := agentTools(t, engine, planThread.Session); !slices.Contains(got, toolPlan) || !slices.Contains(got, toolGitHub) || slices.Contains(got, toolDelegate) {
		t.Fatalf("the lead's tools %q", got)
	}
	if !slices.Equal(plans[0].Waiting, []string{"A1 open"}) {
		t.Fatalf("waiting %v", plans[0].Waiting)
	}
	if _, err := client.AnswerAsk(ctx, ask.ID, proto.AnswerAsk{RequestID: proto.NewRequestID(), Answer: "TPM"}); err != nil {
		t.Fatal(err)
	}
	review := await("the plan in review", func(e proto.Event) bool { return e.Type == proto.EventPlan && e.Plan.State == proto.PlanReview }).Plan
	if review.Rev != team.Rev(planTextV2) || review.Graph == nil || review.Graph.Version != 1 || len(review.Waiting) != 0 {
		t.Fatalf("review %+v", review)
	}
	approved, err := client.ApprovePlan(ctx, review.ID, proto.ApprovePlan{RequestID: proto.NewRequestID(), Rev: review.Rev, Graph: review.Graph.Version})
	if err != nil || approved.State != proto.PlanApproved {
		t.Fatalf("approve %+v %v", approved, err)
	}
	await("the approval in the supervisor's session", entryIn(supervisor.ID, proto.EntryUser, "[P1 · approved]"))
	task := await("the task's thread", func(e proto.Event) bool {
		return e.Type == proto.EventThread && e.Thread.PlanTask == "t1" && e.Thread.Session != 0
	}).Thread
	if task.Agent != "vera" || task.Plan != 1 || !strings.HasPrefix(task.Branch, "vera/p1-t1-sign-the-iso-t2-") {
		t.Fatalf("task thread %+v", task)
	}
	await("the task's report", entryIn(supervisor.ID, proto.EntryUser, "[T2 · Vera · done]"))
	if _, err := client.CloseThread(ctx, task.ID, proto.CloseThread{RequestID: proto.NewRequestID()}); err != nil {
		t.Fatal(err)
	}
	await("the plan done", func(e proto.Event) bool { return e.Type == proto.EventPlan && e.Plan.State == proto.PlanDone })
	await("the plan's end in the supervisor's session", entryIn(supervisor.ID, proto.EntryUser, "[P1 · done]"))
	threads, err := client.Threads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if th.State != proto.ThreadClosed {
			t.Errorf("thread %+v is not closed", th)
		}
	}
	// Each note reached the supervisor once.
	detail, err := client.Session(ctx, supervisor.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range detail.Entries {
		for _, part := range []string{"[P1 · approved]", "[P1 · done]", "[T2 · Vera · done]"} {
			if entry.Kind == proto.EntryUser && strings.Contains(entry.Text, part) {
				counts[part]++
			}
		}
	}
	for part, n := range counts {
		if n != 1 {
			t.Errorf("the supervisor got %q %d times", part, n)
		}
	}
	if len(counts) != 3 {
		t.Errorf("the supervisor's notes %v", counts)
	}
}

// A read's page at its bounds reaches the lead whole through PiG: PiG keeps
// 50 KiB or 2,000 lines of a tool result, and a page is far below both
// (Codex review of #25, round 5).
func TestAReadPageReachesTheLeadWhole(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolPlan, map[string]any{"action": "read", "page": 2}, "r1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	page := strings.Repeat(strings.Repeat("é", 20)+"\n", team.MaxPageLines)
	for len(page) < team.MaxPageBytes-41 {
		page += "x"
	}
	fake := &fakeTeam{planResult: "Page 2 of 3 of plan P1's read, at rev abc1234.\n\n" + page + "\n\n(The read goes on: team_plan read with page 3.)"}
	e.SetTeam(fake)
	ctx := context.Background()
	lead, err := e.CreateSession(ctx, "lead", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}, Thread: 4, Plan: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, lead, "read", "read"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, lead); err != nil {
		t.Fatal(err)
	}
	entries, _, err := e.Transcript(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil {
			if entry.ToolResult.Text != fake.planResult {
				t.Fatalf("the page came through with %d of its %d bytes", len(entry.ToolResult.Text), len(fake.planResult))
			}
			if fake.plans[0].Page != 2 {
				t.Fatalf("the page asked %d", fake.plans[0].Page)
			}
			return
		}
	}
	t.Fatal("no tool result")
}

// A supervisor's team_delegate carries the plan rule in its description,
// which a session reads from the live registration: a supervisor whose
// instructions were written before plans has the rule too (Codex review
// of #25, round 8).
func TestTeamDelegateCarriesThePlanRule(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor}
	// Instructions of before plans: a roster with no plan rule.
	id, err := e.CreateSession(ctx, "old-supervisor", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade, Roster: "Delegate to vera."})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range agent.Tools {
		if tool.Name == toolDelegate {
			if !strings.Contains(tool.Description, team.PlanRoster()) {
				t.Fatalf("team_delegate's description lacks the plan rule: %q", tool.Description)
			}
			return
		}
	}
	t.Fatal("the supervisor has no team_delegate")
}

// A probe the cancelled context killed fails with the cancel, so a start a
// stop cut short is finished after the restart; one that died on its own
// while the context lives does not (Codex review of #58, round 7).
func TestAProbeTheCancelKilledSaysTheCancel(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "go")
	if err := mkfifo(fifo); err != nil {
		t.Skip(err)
	}
	fake := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nread x < \"$FIFO\"\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Sandbox: &Sandbox{Bwrap: fake, Shell: "/bin/sh"}}, commands: map[string]string{"PATH": os.Getenv("PATH"), "FIFO": fifo}}
	run := func(cancelFirst bool) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := e.Dir(ctx, dir)
			done <- err
		}()
		// Opening the pipe waits until the probe runs and reads it.
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cancelFirst {
			cancel()
		}
		_, _ = f.WriteString("go\n")
		_ = f.Close()
		return <-done
	}
	if err := run(true); !errors.Is(err, context.Canceled) {
		t.Errorf("a probe the cancel killed: %v, want context.Canceled", err)
	}
	if err := run(false); err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("a probe that died on its own: %v, want its own failure", err)
	}
}

// team_propose and team_board take a proposal's ci as a boolean (ADR-0047).
func TestProposalToolsHaveBooleanCI(t *testing.T) {
	for name, schema := range map[string]string{"team_propose": proposeParameters, "team_board": boardParameters} {
		var got struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema), &got); err != nil {
			t.Fatal(err)
		}
		if got.Properties["ci"].Type != "boolean" {
			t.Fatalf("%s has no boolean ci", name)
		}
	}
}
