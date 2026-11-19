package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// teamHarness is a harness with a team: odrade supervises; vera works on a
// git checkout, duncan in his home; the clerk cannot start; nogit names a
// repository that is no checkout. It keeps every event the server emits.
type teamHarness struct {
	*harness
	checkout   string
	home       string
	profileDir string
	mu         sync.Mutex
	events     []proto.Event
	super      proto.Session
}

func newTeamHarness(t *testing.T) *teamHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	h := &teamHarness{harness: newHarness(t)}
	h.checkout = filepath.Join(t.TempDir(), "example")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", h.checkout},
		{"-C", h.checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	h.profileDir, h.home = h.writeProfiles(map[string]string{
		"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"leto.md":   "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Leto\n",
		"vera.md":   "---\nmodel: faux/faux-1\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
		"duncan.md": "---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan\n",
		"clerk.md":  "---\nrole: clerk\nmodel: github-copilot/gpt-6-luna\nhome: ~\n---\n# Clerk\n",
		"nogit.md":  "---\nmodel: faux/faux-1\nrepositories:\n  frostyard/nogit: " + t.TempDir() + "\n---\n# Nogit\n",
	})
	emit := h.server.emit
	h.server.emit = func(e proto.Event) {
		h.mu.Lock()
		h.events = append(h.events, e)
		h.mu.Unlock()
		emit(e)
	}
	var err error
	h.super, err = h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "super", Profile: "odrade"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// threadStates are the states the thread events of thread id carried.
func (h *teamHarness) threadStates(id int64) []proto.ThreadState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []proto.ThreadState
	for _, e := range h.events {
		if e.Type == proto.EventThread && e.Thread.ID == id {
			out = append(out, e.Thread.State)
		}
	}
	return out
}

// A supervisor's session names whom it can delegate to, in code.
func TestASupervisorSessionGetsTheRoster(t *testing.T) {
	h := newTeamHarness(t)
	spec := h.engine.specs[0]
	for _, want := range []string{"- duncan (Duncan): works in its home", "- vera (Vera): repositories frostyard/example", "at most 12 threads"} {
		if !strings.Contains(spec.Roster, want) {
			t.Errorf("roster lacks %q:\n%s", want, spec.Roster)
		}
	}
	if strings.Contains(spec.Roster, "clerk") || strings.Contains(spec.Roster, "leto") {
		t.Errorf("roster lists one that takes no delegation:\n%s", spec.Roster)
	}
	plain, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "plain", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	if h.engine.specs[len(h.engine.specs)-1].Roster != "" {
		t.Errorf("session %d of an agent got a roster", plain.ID)
	}
}

// team_delegate makes the worktree, starts the specialist's session from
// its profile in a thread, and sends the brief; the same call again finds
// the same thread (ADR-0014).
func TestDelegateStartsAThreadOnce(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, CallID: "c1"}
	d := team.Delegation{Agent: "vera", Task: "Fix the flaky test.\nIt times out.", Repository: "frostyard/example"}
	text, err := h.server.TeamDelegate(ctx, call, d)
	if err != nil {
		t.Fatal(err)
	}
	path, branch := h.checkout+"--vera-fix-the-flaky-test-t1", "vera/fix-the-flaky-test-t1"
	if !strings.Contains(text, "thread T1: Fix the flaky test.") || !strings.Contains(text, branch) {
		t.Errorf("text %q", text)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil || thread.State != store.ThreadOpen || thread.Cwd != path || thread.Branch != branch || thread.Session == 0 {
		t.Fatalf("thread %+v %v", thread, err)
	}
	if head, err := h.engine.Git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != branch {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	spec := h.engine.specs[len(h.engine.specs)-1]
	if spec.Thread != 1 || spec.Profile == nil || spec.Profile.ID != "vera" || spec.Cwd != path || spec.Model != model || spec.Route != "profile" {
		t.Fatalf("specialist's spec %+v", spec)
	}
	row, err := h.store.Session(ctx, thread.Session)
	if err != nil || row.Profile != "vera" || row.CreatedBy != teamCreator || row.Title != "T1 · Fix the flaky test." {
		t.Fatalf("specialist's session %+v %v", row, err)
	}
	brief := h.engine.texts["thread:1:brief"]
	if h.engine.to["thread:1:brief"] != thread.Session || !strings.Contains(brief, "Odrade, the team's supervisor") || !strings.Contains(brief, branch) {
		t.Fatalf("brief to %d: %q", h.engine.to["thread:1:brief"], brief)
	}
	// Delegated, given its session, then opened once the brief went.
	if got := h.threadStates(1); !slices.Equal(got, []proto.ThreadState{proto.ThreadStarting, proto.ThreadStarting, proto.ThreadOpen}) {
		t.Fatalf("thread events %v", got)
	}
	created := h.engine.created
	again, err := h.server.TeamDelegate(ctx, call, d)
	if err != nil || again != text || h.engine.created != created {
		t.Fatalf("a replay: %q %v, %d sessions after %d", again, err, h.engine.created, created)
	}
	if threads, _ := h.store.Threads(ctx); len(threads) != 1 {
		t.Fatalf("a replay made %d threads", len(threads))
	}
	// Without a repository, the specialist works in its home.
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	if home, _ := h.store.Thread(ctx, 2); home.Cwd != h.home || home.Branch != "" || home.State != store.ThreadOpen {
		t.Fatalf("a home thread %+v", home)
	}
}

func TestDelegateRefuses(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		session int64
		d       team.Delegation
		want    string
	}{
		"an agent delegates": {agent.ID, team.Delegation{Agent: "vera", Task: "x"}, "only a supervisor"},
		"a plain session":    {plain.ID, team.Delegation{Agent: "vera", Task: "x"}, "only a supervisor"},
		"to a supervisor":    {h.super.ID, team.Delegation{Agent: "leto", Task: "x"}, "supervisor and takes no"},
		"to one that can't":  {h.super.ID, team.Delegation{Agent: "clerk", Task: "x"}, "github-copilot/gpt-6-luna"},
		"to no one":          {h.super.ID, team.Delegation{Agent: "nobody", Task: "x"}, "- vera (Vera)"},
		"another repository": {h.super.ID, team.Delegation{Agent: "vera", Task: "x", Repository: "a/b"}, "frostyard/example"},
	} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: tc.session, CallID: name}, tc.d); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
	if threads, _ := h.store.Threads(ctx); len(threads) != 0 {
		t.Fatalf("refusals made threads: %+v", threads)
	}
	h.server.SetDeskCap(1)
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "first"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "second"}, team.Delegation{Agent: "vera", Task: "x"}); err == nil ||
		!strings.Contains(err.Error(), "desk is full") {
		t.Fatalf("a full desk: %v", err)
	}
}

// A thread that cannot start leaves the desk with why, and a replay of
// the call says the same.
func TestADelegationThatCannotStartLeavesTheDesk(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, CallID: "c1"}
	d := team.Delegation{Agent: "nogit", Task: "x", Repository: "frostyard/nogit"}
	_, err := h.server.TeamDelegate(ctx, call, d)
	if err == nil || !strings.Contains(err.Error(), "could not start") || !strings.Contains(err.Error(), "not a git checkout") {
		t.Fatalf("delegate: %v", err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if thread.State != store.ThreadClosed || !strings.Contains(thread.Report, "not a git checkout") {
		t.Fatalf("thread %+v", thread)
	}
	if n, _ := h.store.OpenThreads(ctx); n != 0 {
		t.Fatalf("%d threads on the desk", n)
	}
	if _, err := h.server.TeamDelegate(ctx, call, d); err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Fatalf("a replay: %v", err)
	}
	if got := h.threadStates(1); len(got) != 2 || got[1] != proto.ThreadClosed {
		t.Fatalf("thread events %v", got)
	}
}

// team_message files the report, puts the asks in the Inbox and sends the
// report to the supervisor; the owner's answer goes to the specialist once.
func TestReportAsksAndAnswers(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"},
		team.Delegation{Agent: "vera", Task: "Fix CI", Repository: "frostyard/example"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: h.super.ID, CallID: "m0"}, team.Report{Message: "x", Status: team.StatusDone}); err == nil ||
		!strings.Contains(err.Error(), "works in no thread") {
		t.Fatalf("a report from the supervisor: %v", err)
	}
	call := TeamCall{Session: thread.Session, CallID: "m1"}
	report := team.Report{Message: "PR #3 is open.", Status: team.StatusDone,
		Asks: []team.Ask{{Question: "Squash?", Options: []string{" yes ", "no"}}, {Question: "Anything else?"}}}
	text, err := h.server.TeamMessage(ctx, call, report)
	if err != nil || !strings.Contains(text, "A1, A2 wait for the owner") {
		t.Fatalf("report %q %v", text, err)
	}
	key := call.key() + ":report"
	if h.engine.to[key] != h.super.ID || h.engine.texts[key] != "[T1 · Vera · done] PR #3 is open.\n\nAsks waiting for the owner: A1, A2" {
		t.Fatalf("report to %d: %q", h.engine.to[key], h.engine.texts[key])
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadDone || got.Report != "PR #3 is open." {
		t.Fatalf("thread %+v", got)
	}
	if again, err := h.server.TeamMessage(ctx, call, report); err != nil || again != text {
		t.Fatalf("a replay %q %v", again, err)
	}
	asks, err := h.client.Asks(ctx)
	if err != nil || len(asks) != 2 || asks[0].Question != "Squash?" || asks[0].Options[0] != "yes" || asks[0].SessionID != thread.Session || asks[1].Options != nil {
		t.Fatalf("asks %+v %v", asks, err)
	}
	// The owner answers A1; the answer goes to the specialist's session.
	answered, err := h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: " yes "})
	if err != nil || answered.State != proto.AskAnswered || answered.Answer != "yes" || answered.AnsweredBy != "laptop" {
		t.Fatalf("answer %+v %v", answered, err)
	}
	if got := h.engine.texts[answerKey(h, 1)]; h.engine.to[answerKey(h, 1)] != thread.Session || got != `The owner answered A1 ("Squash?"): yes` {
		t.Fatalf("answer to %d: %q", h.engine.to[answerKey(h, 1)], got)
	}
	if _, err := h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a1", Answer: "yes"}); err != nil {
		t.Fatalf("a retry: %v", err)
	}
	_, err = h.client.AnswerAsk(ctx, asks[0].ID, proto.AnswerAsk{RequestID: "a2", Answer: "no"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.AnswerAsk(ctx, 99, proto.AnswerAsk{RequestID: "a3", Answer: "no"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	_, err = h.client.AnswerAsk(ctx, asks[1].ID, proto.AnswerAsk{RequestID: "a4", Answer: "  "})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if open, _ := h.client.Asks(ctx); len(open) != 1 || open[0].ID != asks[1].ID {
		t.Fatalf("open asks %+v", open)
	}
}

// An answer that could not be sent now is the service's to send: the owner
// gets the answered ask, and the service tries again until it goes,
// without a restart; one left at a stop goes at the next start.
func TestAnAnswerThatWasNotSentIsSentLater(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m1"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which one?"}}}); err != nil {
		t.Fatal(err)
	}
	h.engine.sendErr = errors.New("durable store is down")
	answered, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "the first"})
	if err != nil || answered.State != proto.AskAnswered {
		t.Fatalf("answer %+v %v", answered, err)
	}
	if len(later) != 1 {
		t.Fatalf("%d retries waiting, want 1", len(later))
	}
	// A retry that fails waits again.
	later[0]()
	if len(later) != 2 {
		t.Fatalf("a failed retry did not wait again: %d", len(later))
	}
	h.engine.sendErr = nil
	later[1]()
	if got := h.engine.texts[answerKey(h, 1)]; got != `The owner answered A1 ("Which one?"): the first` {
		t.Fatalf("answer %q", got)
	}
	if waiting, _ := h.store.UndeliveredAnswers(ctx); len(waiting) != 0 {
		t.Fatalf("still waiting: %+v", waiting)
	}
	if err := h.server.DeliverPending(ctx); err != nil || len(later) != 2 {
		t.Fatalf("nothing left to send: %v, %d retries", err, len(later))
	}
}

// A model may reuse a call id in a later turn: a call of another tool task
// is a call of its own, not a replay (ADR-0014).
func TestAReusedCallIDIsANewCall(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "toolu_1"}, team.Delegation{Agent: "duncan", Task: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 9, CallID: "toolu_1"}, team.Delegation{Agent: "duncan", Task: "second"}); err != nil {
		t.Fatal(err)
	}
	threads, _ := h.store.Threads(ctx)
	if len(threads) != 2 {
		t.Fatalf("threads %+v", threads)
	}
	thread, _ := h.store.Thread(ctx, 1)
	for _, report := range []struct {
		task    int64
		message string
	}{{5, "first"}, {9, "second"}} {
		if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: report.task, CallID: "toolu_2"},
			team.Report{Message: report.message, Status: team.StatusWorking}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := h.store.Thread(ctx, 1); got.Report != "second" {
		t.Fatalf("the second report was taken for a replay: %+v", got)
	}
}

// A thread recorded under one profile starts its specialist only from a
// profile that still takes the delegation (team.CheckTarget).
func TestAThreadStartsOnlyFromAProfileThatStillTakesIt(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, CallID: "c1"}
	d := team.Delegation{Agent: "duncan", Task: "x"}
	// The thread is recorded, then pocketd stops before it starts; the
	// owner makes duncan private meanwhile.
	if _, err := h.server.newThread(ctx, call, call.key(), d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := h.engine.created
	if _, err := h.server.TeamDelegate(ctx, call, d); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("a private profile started: %v", err)
	}
	if h.engine.created != created {
		t.Fatal("a session started from a profile that takes no delegation")
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
}

// A specialist whose session exists is finished without its profile, and a
// pocketd without profiles does not fail on it.
func TestAStartedSpecialistNeedsNoProfile(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, CallID: "c1"}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	created := h.engine.created
	// Without profiles the agent is named by its id.
	h.server.SetProfiles(nil)
	if again, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "x"}); err != nil ||
		!strings.Contains(again, "Delegated to duncan as thread T1") || h.engine.created != created {
		t.Fatalf("a replay without profiles: %q %v", again, err)
	}
}

// A thread the owner closes while it starts gets no brief, and leaves the
// desk; the supervisor hears why.
func TestAThreadClosedWhileItStartsGetsNoBrief(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.beforeAttach = func(id int64) {
		if _, err := h.client.CloseThread(ctx, id, proto.CloseThread{RequestID: "k1"}); err != nil {
			t.Error(err)
		}
	}
	_, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"})
	if err == nil || !strings.Contains(err.Error(), "closed while it started") {
		t.Fatalf("delegate: %v", err)
	}
	if _, sent := h.engine.texts["thread:1:brief"]; sent {
		t.Fatal("a closed thread's specialist got its brief")
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadClosed || got.Session != 0 {
		t.Fatalf("thread %+v", got)
	}
}

// A brief that cannot be sent leaves the thread off the desk, never open
// with an idle specialist.
func TestABriefThatFailsLeavesTheDesk(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.beforeAttach = func(int64) { h.engine.sendErr = errors.New("durable store is down") }
	_, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"})
	if err == nil || !strings.Contains(err.Error(), "could not start") || !strings.Contains(err.Error(), "send the brief") {
		t.Fatalf("delegate: %v", err)
	}
	if n, _ := h.store.OpenThreads(ctx); n != 0 {
		t.Fatalf("%d threads on the desk", n)
	}
}

// An aborted delegation starts no specialist.
func TestAnAbortedDelegationStartsNoSpecialist(t *testing.T) {
	h := newTeamHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	created := h.engine.created
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"},
		team.Delegation{Agent: "vera", Task: "x", Repository: "frostyard/example"}); err == nil {
		t.Fatal("an aborted delegation went on")
	}
	if h.engine.created != created {
		t.Fatal("an aborted delegation started a specialist")
	}
	if got, _ := h.store.Thread(context.Background(), 1); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
}

// Thread and ask events leave under the change that made them (workMu), so
// a client never sees an older state of a row after a newer one.
func TestThreadAndAskEventsLeaveWithTheirChange(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	emit := h.server.emit
	var loose []string
	h.server.emit = func(e proto.Event) {
		if e.Type == proto.EventThread || e.Type == proto.EventAsk {
			if h.server.workMu.TryLock() {
				h.server.workMu.Unlock()
				loose = append(loose, string(e.Type))
			}
		}
		emit(e)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m1"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "this"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatal(err)
	}
	if len(loose) != 0 {
		t.Fatalf("events sent outside their change: %v", loose)
	}
}

func TestCloseThreads(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	for _, id := range []string{"c1", "c2"} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: id}, team.Delegation{Agent: "duncan", Task: id}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "leto", Profile: "leto"})
	if err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	for name, session := range map[string]int64{"the specialist": thread.Session, "another supervisor": other.ID} {
		if _, err := h.server.TeamClose(ctx, TeamCall{Session: session, CallID: "x"}, 1); err == nil {
			t.Errorf("%s closed a thread", name)
		}
	}
	// Another session of the same supervisor may close it.
	again, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "odrade-2", Profile: "odrade"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := h.server.TeamClose(ctx, TeamCall{Session: again.ID, CallID: "x"}, 1)
	if err != nil || text != "Closed thread T1: its place on the desk is free." {
		t.Fatalf("close %q %v", text, err)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "late"}, team.Report{Message: "x", Status: team.StatusDone}); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("a report on a closed thread: %v", err)
	}
	// The owner closes the other over the protocol, once per request.
	closed, err := h.client.CloseThread(ctx, 2, proto.CloseThread{RequestID: "k1"})
	if err != nil || closed.State != proto.ThreadClosed {
		t.Fatalf("owner close %+v %v", closed, err)
	}
	if _, err := h.client.CloseThread(ctx, 2, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatalf("a retry: %v", err)
	}
	_, err = h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	// A request id used for another thread closes nothing.
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c3"}, team.Delegation{Agent: "duncan", Task: "c3"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.CloseThread(ctx, 3, proto.CloseThread{RequestID: "k1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if got, _ := h.store.Thread(ctx, 3); got.State == store.ThreadClosed {
		t.Fatal("a reused request id closed another thread")
	}
	_, err = h.client.CloseThread(ctx, 9, proto.CloseThread{RequestID: "k2"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	threads, err := h.client.Threads(ctx)
	if err != nil || len(threads) != 3 || threads[1].State != proto.ThreadClosed || threads[2].State != proto.ThreadClosed {
		t.Fatalf("threads %+v %v", threads, err)
	}
}

// The thread and ask paths answer a wrong method, not "no route".
func TestThreadPathsKnowTheirMethods(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: "4", "Authorization": "Bearer " + h.token}
	for _, path := range []string{proto.PathThreads, proto.PathAsks} {
		if response, _ := h.raw("POST", path, "{}", headers); response.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", path, response.StatusCode)
		}
	}
	if response, e := h.raw("POST", "/v1/asks/x", `{"requestId":"r","answer":"y"}`, headers); response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(e.Message, "ask id") {
		t.Errorf("a bad ask id: %d %+v", response.StatusCode, e)
	}
}

// The brief goes under workMu with the thread's attachment and opening, so
// no close falls between them; and a thread closed after its session was
// attached never gets its brief, also when its call runs again.
func TestAClosedThreadNeverGetsItsBrief(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	held := false
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			if h.server.workMu.TryLock() {
				h.server.workMu.Unlock()
			} else {
				held = true
			}
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("the brief went outside workMu")
	}
	// The brief fails once its session is attached: the thread closes,
	// session and all; the call run again sends no brief.
	h.server.beforeAttach = func(int64) { h.engine.sendErr = errors.New("durable store is down") }
	call := TeamCall{Session: h.super.ID, CallID: "c2"}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "y"}); err == nil {
		t.Fatal("a failed brief succeeded")
	}
	h.server.beforeAttach, h.engine.sendErr = nil, nil
	thread, _ := h.store.Thread(ctx, 2)
	if thread.State != store.ThreadClosed || thread.Session == 0 {
		t.Fatalf("thread %+v", thread)
	}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "y"}); err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("a run again: %v", err)
	}
	if _, sent := h.engine.texts["thread:2:brief"]; sent {
		t.Fatal("a closed thread's specialist got its brief")
	}
}

// A delegation cut off by pocketd's stop stays starting, and finishes when
// PiG runs the call again; an owner's abort closes it (another test).
func TestADelegationCutOffByAStopFinishesAfterIt(t *testing.T) {
	h := newTeamHarness(t)
	call := TeamCall{Session: h.super.ID, CallID: "c1"}
	d := team.Delegation{Agent: "vera", Task: "x", Repository: "frostyard/example"}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	h.server.Stop()
	if _, err := h.server.TeamDelegate(stopped, call, d); err == nil {
		t.Fatal("a delegation cut off by the stop succeeded")
	}
	ctx := context.Background()
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadStarting {
		t.Fatalf("thread %+v", got)
	}
	if _, err := h.server.TeamDelegate(ctx, call, d); err != nil {
		t.Fatalf("the call run again: %v", err)
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadOpen {
		t.Fatalf("thread %+v", got)
	}
}

// A filed report whose message cannot reach the supervisor now is kept and
// sent later, by the service; the specialist is told it is filed, and the
// call run again files nothing twice.
func TestAFiledReportReachesTheSupervisorLater(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	h.engine.failTo = map[int64]bool{h.super.ID: true}
	call := TeamCall{Session: thread.Session, CallID: "m1"}
	report := team.Report{Message: "Done.", Status: team.StatusDone, Asks: []team.Ask{{Question: "Merge?"}}}
	text, err := h.server.TeamMessage(ctx, call, report)
	if err != nil || !strings.Contains(text, "A1 wait") {
		t.Fatalf("report %q %v", text, err)
	}
	if again, err := h.server.TeamMessage(ctx, call, report); err != nil || again != text {
		t.Fatalf("a run again %q %v", again, err)
	}
	if open, _ := h.store.OpenAsks(ctx); len(open) != 1 {
		t.Fatalf("asks %+v", open)
	}
	if len(later) == 0 {
		t.Fatal("no retry waits")
	}
	h.engine.failTo = nil
	later[len(later)-1]()
	key := call.key() + ":report"
	if h.engine.to[key] != h.super.ID || !strings.HasPrefix(h.engine.texts[key], "[T1 · Duncan · done] Done.") {
		t.Fatalf("report to %d: %q", h.engine.to[key], h.engine.texts[key])
	}
	if unsent, _ := h.store.UnsentReports(ctx); len(unsent) != 0 {
		t.Fatalf("unsent %+v", unsent)
	}
}

// One answer that cannot go does not hold back the others: a pass tries
// every pending answer.
func TestOneAnswerThatCannotGoHoldsNoOther(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	var sessions []int64
	for _, id := range []string{"c1", "c2"} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: id}, team.Delegation{Agent: "duncan", Task: id}); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(1); i <= 2; i++ {
		thread, _ := h.store.Thread(ctx, i)
		sessions = append(sessions, thread.Session)
		if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
			team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Both sends fail; both answers wait.
	h.engine.failTo = map[int64]bool{sessions[0]: true, sessions[1]: true}
	for _, id := range []int64{1, 2} {
		if _, err := h.client.AnswerAsk(ctx, id, proto.AnswerAsk{RequestID: "a" + strconv.FormatInt(id, 10), Answer: "this"}); err != nil {
			t.Fatal(err)
		}
	}
	if waiting, _ := h.store.UndeliveredAnswers(ctx); len(waiting) != 2 {
		t.Fatalf("waiting %+v", waiting)
	}
	// The first session still takes no message; the second answer goes.
	h.engine.failTo = map[int64]bool{sessions[0]: true}
	if err := h.server.DeliverPending(ctx); err == nil {
		t.Fatal("a failed delivery was not reported")
	}
	if waiting, _ := h.store.UndeliveredAnswers(ctx); len(waiting) != 1 || waiting[0].ID != 1 {
		t.Fatalf("waiting %+v", waiting)
	}
	if _, sent := h.engine.texts[answerKey(h, 2)]; !sent {
		t.Fatal("the second answer waited behind the first")
	}
}

// A thread's worktree is marked with the thread's random owner, so a fresh
// data folder that numbers its calls the same never takes over a worktree
// an earlier one made.
func TestAWorktreeFromAnotherDataFolderIsNotTaken(t *testing.T) {
	first := newTeamHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: first.super.ID, CallID: "c1"}
	d := team.Delegation{Agent: "vera", Task: "Fix CI", Repository: "frostyard/example"}
	if _, err := first.server.TeamDelegate(ctx, call, d); err != nil {
		t.Fatal(err)
	}
	second := newTeamHarness(t)
	vera := "---\nmodel: faux/faux-1\nrepositories:\n  frostyard/example: " + first.checkout + "\n---\n# Vera\n"
	if err := os.WriteFile(filepath.Join(second.profileDir, "vera.md"), []byte(vera), 0o600); err != nil {
		t.Fatal(err)
	}
	if second.super.ID != first.super.ID {
		t.Fatalf("the harnesses number sessions apart: %d, %d", first.super.ID, second.super.ID)
	}
	if _, err := second.server.TeamDelegate(ctx, call, d); err == nil || !strings.Contains(err.Error(), "did not make it") {
		t.Fatalf("another data folder's worktree was taken: %v", err)
	}
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

// A thread whose brief went stays on the desk when its opening fails to
// be recorded: the specialist works, and its report moves the thread on.
func TestAThreadWhoseBriefWentNeverCloses(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.openThread = func(context.Context, int64) (store.ThreadRow, error) {
		return store.ThreadRow{}, errors.New("database is locked")
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if thread.State != store.ThreadStarting || thread.Session == 0 {
		t.Fatalf("thread %+v", thread)
	}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m1"}, team.Report{Message: "Done.", Status: team.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadDone {
		t.Fatalf("thread %+v", got)
	}
}

// The specialist's session starts from its profile as it is once the
// worktree is made: a profile made private meanwhile starts nothing.
func TestAProfileChangedWhileTheWorktreeIsMadeStartsNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	private := "---\nmodel: faux/faux-1\nprivate: true\nhome: ~\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n"
	h.engine.onGit = func(args []string) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			if err := os.WriteFile(filepath.Join(h.profileDir, "vera.md"), []byte(private), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	created := h.engine.created
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"},
		team.Delegation{Agent: "vera", Task: "x", Repository: "frostyard/example"}); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("delegate: %v", err)
	}
	if h.engine.created != created {
		t.Fatal("a session started from the old profile")
	}
}

// team_close is recorded with its call's key: the call run again after the
// supervisor's profile changed answers as the first did.
func TestACloseRunAgainAnswersAsBefore(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	call := TeamCall{Session: h.super.ID, Task: 7, CallID: "k1"}
	first, err := h.server.TeamClose(ctx, call, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.profileDir, "odrade.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := h.server.TeamClose(ctx, call, 1); err != nil || again != first {
		t.Fatalf("the close run again: %q %v", again, err)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, Task: 8, CallID: "k2"}, 1); err == nil {
		t.Fatal("a new close by a demoted supervisor succeeded")
	}
}

// A thread's reports reach the supervisor in the order they were filed,
// also when an older one had to wait.
func TestReportsReachTheSupervisorInOrder(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	h.engine.failTo = map[int64]bool{h.super.ID: true}
	working := TeamCall{Session: thread.Session, Task: 1, CallID: "m"}
	if _, err := h.server.TeamMessage(ctx, working, team.Report{Message: "Halfway.", Status: team.StatusWorking}); err != nil {
		t.Fatal(err)
	}
	h.engine.failTo = nil
	done := TeamCall{Session: thread.Session, Task: 2, CallID: "m"}
	if _, err := h.server.TeamMessage(ctx, done, team.Report{Message: "Done.", Status: team.StatusDone}); err != nil {
		t.Fatal(err)
	}
	var reports []string
	for _, key := range h.engine.order {
		if strings.HasSuffix(key, ":report") {
			reports = append(reports, key)
		}
	}
	if !slices.Equal(reports, []string{working.key() + ":report", done.key() + ":report"}) {
		t.Fatalf("reports sent %q", reports)
	}
}

// One delivery pass waits or runs at a time, through its whole scan: a
// pass asked for while one runs is scheduled only once it finished; the
// first pass at start is not waited for.
func TestDeliveryPassesRunOneAtATime(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	h.engine.failTo = map[int64]bool{thread.Session: true}
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "this"}); err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 {
		t.Fatalf("%d passes waiting, want 1", len(later))
	}
	h.engine.failTo = nil
	// While the pass sends, another is asked for: none is scheduled
	// until the pass finished.
	scheduledDuring := -1
	h.engine.onSend = func(int64, string) {
		h.engine.onSend = nil
		h.server.StartDelivery()
		scheduledDuring = len(later)
	}
	later[0]()
	if scheduledDuring != 1 {
		t.Fatalf("a pass was scheduled while one ran: %d waiting", scheduledDuring)
	}
	if len(later) != 2 {
		t.Fatalf("the pass asked for meanwhile did not follow: %d", len(later))
	}
	later[1]()
	if len(later) != 2 {
		t.Fatalf("a pass with nothing left asked for another: %d", len(later))
	}
}

// An answer counts as delivered only once its entry is in the transcript:
// one the busy specialist queued waits, and one an abort withdrew is sent
// again under a key of its own.
func TestAnAnswerCountsOnlyOncePlaced(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	// The specialist is busy: every new submission queues.
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.onSend = func(int64, string) {
		h.engine.mu.Lock()
		h.engine.queued[int64(len(h.engine.sent)+100)] = true
		h.engine.mu.Unlock()
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "this"}); err != nil {
		t.Fatal(err)
	}
	first := h.engine.sent[answerKey(h, 1)]
	if waiting, _ := h.store.UndeliveredAnswers(ctx); len(waiting) != 1 || waiting[0].Submission != first {
		t.Fatalf("a queued answer counts as delivered: %+v", waiting)
	}
	// An abort withdraws it: the next pass sends it again, and this time
	// it is placed.
	h.engine.onSend = nil
	h.engine.withdrawn[first] = true
	later[len(later)-1]()
	second, sent := h.engine.sent[answerKey(h, 1)+":1"]
	if !sent || h.engine.texts[answerKey(h, 1)+":1"] != `The owner answered A1 ("Which?"): this` {
		t.Fatalf("the withdrawn answer was not sent again: %v", h.engine.order)
	}
	if waiting, _ := h.store.UndeliveredAnswers(ctx); len(waiting) != 0 {
		t.Fatalf("still waiting after submission %d was placed: %+v", second, waiting)
	}
}

// A start run again whose brief went before never closes its thread, also
// when the run fails: the specialist works, and its reports move it on.
func TestAStartWhoseBriefWentNeverCloses(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.openThread = func(context.Context, int64) (store.ThreadRow, error) {
		return store.ThreadRow{}, errors.New("database is locked")
	}
	call := TeamCall{Session: h.super.ID, CallID: "c1"}
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	h.engine.createdErr = errors.New("durable store is down")
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	if _, err := h.server.TeamDelegate(ctx, call, team.Delegation{Agent: "duncan", Task: "x"}); err == nil || !strings.Contains(err.Error(), "may have started") {
		t.Fatalf("a failed run again: %v", err)
	}
	if len(later) == 0 {
		t.Fatal("no delivery pass follows the brief")
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadStarting || got.Session == 0 {
		t.Fatalf("thread %+v", got)
	}
}

// A thread that could not start, whose close failed, closes once its
// delegating call's task ended, in a delivery pass, also after a restart:
// it never keeps a place on the desk.
func TestAThreadWhoseCloseFailedClosesLater(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	fail := h.server.closeFailed
	h.server.closeFailed = func(context.Context, int64, string) (store.ThreadRow, error) {
		return store.ThreadRow{}, errors.New("database is locked")
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 33, CallID: "c1"},
		team.Delegation{Agent: "nogit", Task: "x", Repository: "frostyard/nogit"}); err == nil {
		t.Fatal("a delegation to no checkout started")
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadStarting || len(later) != 1 {
		t.Fatalf("thread %+v, %d passes", got, len(later))
	}
	h.server.closeFailed = fail
	// While the call's task runs, the pass leaves the thread, and looks
	// again.
	later[0]()
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadStarting || len(later) != 2 {
		t.Fatalf("thread %+v, %d passes", got, len(later))
	}
	h.engine.mu.Lock()
	h.engine.ended[33] = true
	h.engine.mu.Unlock()
	later[1]()
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadClosed || !strings.Contains(got.Report, "ended before the thread started") {
		t.Fatalf("thread %+v", got)
	}
	// A server that starts again finds nothing left to follow.
	if done, err := h.server.deliverAll(ctx); err != nil || !done {
		t.Fatalf("after: %v %v", done, err)
	}
}

// A brief a busy specialist queued opens the thread and is followed: one
// an abort withdrew is sent again, and the thread notes it once placed.
func TestABriefIsFollowedUntilPlaced(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if thread.State != store.ThreadOpen || thread.Briefed || thread.BriefSubmission == 0 || len(later) != 1 {
		t.Fatalf("thread %+v, %d passes", thread, len(later))
	}
	h.engine.onSend = nil
	h.engine.withdrawn[thread.BriefSubmission] = true
	later[0]()
	if _, sent := h.engine.sent["thread:1:brief:1"]; !sent {
		t.Fatalf("the withdrawn brief was not sent again: %v", h.engine.order)
	}
	if got, _ := h.store.Thread(ctx, 1); !got.Briefed {
		t.Fatalf("thread %+v", got)
	}
}

// A home thread's specialist starts only in its profile's home as it is
// when its session starts.
func TestAHomeChangedWhileStartingStartsNothing(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	moved := t.TempDir()
	h.server.afterFolder = func() {
		if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: "+moved+"\n---\n# Duncan\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	created := h.engine.created
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err == nil ||
		!strings.Contains(err.Error(), "home changed") {
		t.Fatalf("delegate: %v", err)
	}
	if h.engine.created != created {
		t.Fatal("a session started in the old home")
	}
}

// Every message pocketd owes a session is placed under outboxMu, from the
// owner's answer, a report and a delivery pass alike, so two callers never
// advance one message's attempt twice.
func TestMessagesArePlacedUnderTheOutboxLock(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var loose []string
	h.engine.onSend = func(_ int64, key string) {
		if h.server.outboxMu.TryLock() {
			h.server.outboxMu.Unlock()
			loose = append(loose, key)
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "this"}); err != nil {
		t.Fatal(err)
	}
	if len(loose) != 0 {
		t.Fatalf("sent outside outboxMu: %v", loose)
	}
}

// A message queued on an idle session (a failed run's queue, which PiG
// keeps until the next input) is withdrawn while still queued and sent
// again as a new input; a busy session's queue is left to its run.
func TestAStrandedMessageIsSentAgain(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	h.engine.queued = map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if key == answerKey(h, 1) {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	// The session is busy: the answer waits in its queue.
	h.engine.runFirst = 7
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "this"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil || len(h.engine.withdrawals) != 0 {
		t.Fatalf("a busy session's queue was touched: %q, %v", h.engine.withdrawals, err)
	}
	// Its run failed and left it idle: the answer goes again, and lands.
	h.engine.runFirst = 0
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.engine.withdrawals, []string{answerKey(h, 1)}) {
		t.Fatalf("withdrawals %q", h.engine.withdrawals)
	}
	if _, sent := h.engine.sent[answerKey(h, 1)+":1"]; !sent {
		t.Fatalf("the answer was not sent again: %q", h.engine.order)
	}
	if waiting, _ := h.store.UndeliveredAnswers(ctx); len(waiting) != 0 {
		t.Fatalf("still waiting %+v", waiting)
	}
}

// A brief sent again by a delivery pass goes under workMu with a fresh read
// of its thread, so no close falls between them; a closed thread's brief
// is not sent again.
func TestABriefResentAfterACloseNeverGoes(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	h.engine.withdrawn[thread.BriefSubmission] = true
	held := true
	h.engine.onSend = func(_ int64, key string) {
		if strings.Contains(key, ":brief") && h.server.workMu.TryLock() {
			h.server.workMu.Unlock()
			held = false
		}
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, sent := h.engine.sent["thread:1:brief:1"]; !sent || !held {
		t.Fatalf("the brief went again: %v, under workMu: %v", sent, held)
	}
	// Withdrawn again, then closed: the next pass sends nothing.
	again, _ := h.store.Thread(ctx, 1)
	h.engine.withdrawn[again.BriefSubmission] = true
	if _, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, sent := h.engine.sent["thread:1:brief:2"]; sent {
		t.Fatal("a closed thread's brief went again")
	}
}

// A brief whose fate the durable store cannot tell is neither sent nor its
// thread closed until it can.
func TestAnInconclusiveBriefLookupWaits(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	lookup := errors.New("durable store is busy")
	h.server.beforeAttach = func(int64) { h.engine.placedErr = lookup }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, Task: 9, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err == nil {
		t.Fatal("a start whose brief could not be looked up succeeded")
	}
	h.server.beforeAttach = nil
	h.engine.mu.Lock()
	h.engine.ended[9] = true
	h.engine.mu.Unlock()
	sent := len(h.engine.sent)
	if _, err := h.server.deliverAll(ctx); err == nil {
		t.Fatal("an inconclusive lookup was taken for an answer")
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadStarting || len(h.engine.sent) != sent {
		t.Fatalf("thread %+v, %d sends after %d", got, len(h.engine.sent), sent)
	}
	// The store can tell again: no brief ever went, so the thread closes.
	h.engine.placedErr = nil
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
}

// Stop ends a delivery pass's work: the context its sends run under is
// cancelled, and a pass after Stop does nothing.
func TestStopEndsTheDeliveryPasses(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	var later []func()
	h.server.retry = func(f func()) func() { later = append(later, f); return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	h.engine.failTo = map[int64]bool{thread.Session: true}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "this"}); err != nil {
		t.Fatal(err)
	}
	h.engine.failTo = nil
	later[len(later)-1]()
	h.engine.mu.Lock()
	passCtx := h.engine.lastCtx
	h.engine.mu.Unlock()
	h.server.Stop()
	if passCtx == nil || passCtx.Err() == nil {
		t.Fatal("Stop left the context of the delivery pass's sends running")
	}
	sends := len(h.engine.order)
	h.server.deliverPass()
	if len(h.engine.order) != sends {
		t.Fatal("a pass after Stop sent")
	}
}

// The brief's first submission takes the delegating call's context: an
// abort that comes while pocketd looks the brief up sends none, and the
// thread leaves the desk.
func TestAnAbortDuringTheBriefSendsNone(t *testing.T) {
	h := newTeamHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.engine.onPlaced = func(key string) {
		if key == "thread:1:brief" {
			h.engine.onPlaced = nil
			cancel()
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err == nil {
		t.Fatal("an aborted delegation succeeded")
	}
	if _, sent := h.engine.sent["thread:1:brief"]; sent {
		t.Fatal("an aborted delegation sent its brief")
	}
	if got, _ := h.store.Thread(context.Background(), 1); got.State != store.ThreadClosed {
		t.Fatalf("thread %+v", got)
	}
}

// answerKey is the key the answer to ask id goes under: its own key's.
func answerKey(h *teamHarness, id int64) string {
	h.t.Helper()
	ask, err := h.store.Ask(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return ask.Key + ":answer:" + digest(ask.Answer)
}

// Closing a thread whose brief still waits in its specialist's queue
// withdraws the brief: the specialist never starts the closed work. A
// thread closed while its folder was made gets no session at all.
func TestClosingAThreadWithdrawsItsQueuedBrief(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.queued = map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.engine.withdrawals, []string{"thread:1:brief"}) {
		t.Fatalf("withdrawals %q", h.engine.withdrawals)
	}
	// Closed while its folder is made: no specialist session starts.
	created := h.engine.created
	h.server.beforeAttach = func(id int64) {
		if _, err := h.client.CloseThread(ctx, id, proto.CloseThread{RequestID: "k2"}); err != nil {
			t.Error(err)
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c2"}, team.Delegation{Agent: "duncan", Task: "y"}); err == nil {
		t.Fatal("a thread closed while it started started")
	}
	if h.engine.created != created {
		t.Fatal("a closed thread's specialist session started")
	}
}

// A thread's conversation is keyed by its random owner: a work database
// restored from a backup, which numbers a new thread as an old one, never
// finds the old thread's conversation.
func TestASpecialistSessionIsKeyedByItsThreadsOwner(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, found := h.engine.made["thread:"+thread.Owner]; !found {
		t.Fatalf("no conversation under the owner's key: %v", h.engine.made)
	}
	if _, found := h.engine.made["thread:1"]; found {
		t.Fatal("a conversation under the thread's number")
	}
}

// A queued brief is withdrawn before its thread closes: a close whose
// withdrawal fails does not happen, and the next try closes it.
func TestAThreadClosesOnlyOnceItsBriefIsWithdrawn(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.queued = map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	h.engine.withdrawErr = errors.New("durable store is down")
	_, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if got, _ := h.store.Thread(ctx, 1); got.State == store.ThreadClosed {
		t.Fatal("a thread closed while its brief stayed queued")
	}
	h.engine.withdrawErr = nil
	h.engine.onWithdraw = func(string) {
		if got, _ := h.store.Thread(ctx, 1); got.State == store.ThreadClosed {
			t.Error("the thread closed before its brief was withdrawn")
		}
	}
	if _, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, 1); got.State != store.ThreadClosed || !slices.Equal(h.engine.withdrawals, []string{"thread:1:brief"}) {
		t.Fatalf("thread %+v, withdrawals %q", got, h.engine.withdrawals)
	}
}

// A new specialist's session starts from its profile as it is once the
// locks are held, and an abort that came while pocketd waited for them
// starts nothing.
func TestTheSpecialistIsCheckedOnceTheLocksAreHeld(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	created := h.engine.created
	h.server.beforeAttach = func(int64) {
		if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err == nil ||
		!strings.Contains(err.Error(), "private") {
		t.Fatalf("a profile made private meanwhile: %v", err)
	}
	if h.engine.created != created {
		t.Fatal("a session started from the old profile")
	}
	// duncan takes delegations again; the supervisor aborts while pocketd
	// waits for the locks.
	if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aborted, cancel := context.WithCancel(ctx)
	h.server.beforeAttach = func(int64) { cancel() }
	if _, err := h.server.TeamDelegate(aborted, TeamCall{Session: h.super.ID, CallID: "c3"}, team.Delegation{Agent: "duncan", Task: "x"}); err == nil {
		t.Fatal("an aborted delegation started")
	}
	if h.engine.created != created {
		t.Fatal("an aborted delegation started a session")
	}
}

// An answer's key names its content: the placement of an earlier answer to
// the same ask, which a restored work database lets the owner answer
// again, never counts for a new one.
func TestAnAnswerKeyNamesItsContent(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, CallID: "m"},
		team.Report{Message: "?", Status: team.StatusBlocked, Asks: []team.Ask{{Question: "Which?"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.AnswerAsk(ctx, 1, proto.AnswerAsk{RequestID: "a1", Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	ask, _ := h.store.Ask(ctx, 1)
	again := ask
	again.Answer, again.Delivered, again.Attempt = "no", false, 0
	h.server.outboxMu.Lock()
	placed, err := h.server.deliverRow(ctx, again)
	h.server.outboxMu.Unlock()
	if err != nil || !placed {
		t.Fatal(placed, err)
	}
	if got := h.engine.texts[ask.Key+":answer:"+digest("no")]; got != `The owner answered A1 ("Which?"): no` {
		t.Fatalf("the new answer did not go: %q", h.engine.order)
	}
}

// A close refused for its request id touches no queue: the thread's
// queued brief stays where it is.
func TestARefusedCloseLeavesTheQueue(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.queued = map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: id}, team.Delegation{Agent: "duncan", Task: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatal(err)
	}
	withdrawals := len(h.engine.withdrawals)
	_, err := h.client.CloseThread(ctx, 2, proto.CloseThread{RequestID: "k1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if len(h.engine.withdrawals) != withdrawals {
		t.Fatalf("a refused close withdrew %q", h.engine.withdrawals[withdrawals:])
	}
	thread, _ := h.store.Thread(ctx, 2)
	if !h.engine.queued[thread.BriefSubmission] {
		t.Fatal("a refused close took the brief out of the queue")
	}
}

// A message whose attempt the durable store does not have is not sent
// while an earlier attempt still lives: a placed one is its delivery, a
// queued one is followed.
func TestAnAbsentAttemptChecksTheEarlierOnes(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	thread, _ := h.store.Thread(ctx, 1)
	sends := len(h.engine.order)
	// The work database says attempt 2; the durable store has only the
	// first, placed.
	m := outgoing{session: thread.Session, key: "thread:1:brief", text: thread.Brief, attempt: 2}
	h.server.outboxMu.Lock()
	placed, err := h.server.place(ctx, m, func(int64) error { return nil }, func(int) (bool, error) { return true, nil })
	h.server.outboxMu.Unlock()
	if err != nil || !placed || len(h.engine.order) != sends {
		t.Fatalf("placed %v %v; %d sends", placed, err, len(h.engine.order)-sends)
	}
}

// The close's request id is reserved before any withdrawal, so another
// request that holds it is refused with the queue untouched.
func TestACloseReservesItsRequestBeforeTheQueue(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.queued = map[int64]bool{}
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasSuffix(key, ":brief") {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	key := "u:" + func() string { d, _ := h.store.Devices(ctx); return d[0].ID }() + ":k1"
	reserved := false
	h.engine.onWithdraw = func(string) {
		_, reserved, _ = h.store.LookupRequest(ctx, key, "closeThread:1")
	}
	if _, err := h.client.CloseThread(ctx, 1, proto.CloseThread{RequestID: "k1"}); err != nil {
		t.Fatal(err)
	}
	if !reserved {
		t.Fatal("the brief was withdrawn before the close's request id was reserved")
	}
}
