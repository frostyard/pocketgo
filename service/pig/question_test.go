package pig

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The question tool (ADR-0048).

// waitForCall waits until fake saw a question call.
func waitForCall(t *testing.T, fake *fakeTeam) {
	t.Helper()
	select {
	case <-fake.questionCalled:
	case <-time.After(20 * time.Second):
		t.Fatal("no question call")
	}
}

// The tool passes the calling session, its task and the call's id, and
// the question as the agent wrote it, to the team, and the team's answer
// is its result, in the same run; while the call waits, the run's starter
// is the request key of the message that started it.
func TestTheQuestionToolWaitsForTheTeamsAnswer(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(
		toolTurn(toolQuestion, map[string]any{"question": "Which database?", "options": []any{"Postgres", "SQLite"}, "placeholder": "or another"}, "q1"),
		answer("Postgres it is."),
	))
	defer func() { _ = e.Close(context.Background()) }()
	wait := make(chan struct{})
	fake := &fakeTeam{questionResult: `{"answer":"Postgres","option":0}`, questionWait: wait, questionCalled: make(chan struct{}, 4)}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}})
	if err != nil {
		t.Fatal(err)
	}
	if key, busy, err := e.RunStarter(ctx, id); err != nil || busy || key != "" {
		t.Fatalf("an idle session's run starter: %q %v %v", key, busy, err)
	}
	if _, err := e.Send(ctx, id, "u:dev:r1", "pick a database"); err != nil {
		t.Fatal(err)
	}
	waitForCall(t, fake)
	if key, busy, err := e.RunStarter(ctx, id); err != nil || !busy || key != "u:dev:r1" {
		t.Fatalf("the run starter while the question waits: %q %v %v", key, busy, err)
	}
	close(wait)
	waitIdle(t, e, id)
	fake.mu.Lock()
	call, q := fake.calls[0], fake.questions[0]
	fake.mu.Unlock()
	if call.Session != id || call.CallID != "q1" || call.Task == 0 {
		t.Fatalf("the call %+v", call)
	}
	if q.Question != "Which database?" || !slices.Equal(q.Options, []string{"Postgres", "SQLite"}) || q.Placeholder != "or another" || q.Multiple {
		t.Fatalf("the question %+v", q)
	}
	if got := lastToolResult(t, e, id); got.IsError || got.Text != `{"answer":"Postgres","option":0}` {
		t.Fatalf("the result %+v", got)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil || entries[len(entries)-1].Text != "Postgres it is." {
		t.Fatalf("the run did not go on with the answer: %+v, %v", entries, err)
	}
	// A refusal is an error result the agent reads.
	fake.questionWait, fake.fail = nil, team.ErrNobodyWatching
	if got := toolText(fake.Question(ctx, call, q)); !*got.IsError {
		t.Fatal("a refusal is no error result")
	}
}

// A run another submission started names its key; one an abort ended
// stops the call's wait with no result, and the task ends.
func TestAnAbortEndsTheQuestionsWait(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolQuestion, map[string]any{"question": "Muster?"}, "q1")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{questionWait: make(chan struct{}), questionCalled: make(chan struct{}, 4)}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "schedule:abc:1", "[scheduled] muster"); err != nil {
		t.Fatal(err)
	}
	waitForCall(t, fake)
	if key, busy, err := e.RunStarter(ctx, id); err != nil || !busy || key != "schedule:abc:1" {
		t.Fatalf("a scheduled run's starter: %q %v %v", key, busy, err)
	}
	fake.mu.Lock()
	task := fake.calls[0].Task
	fake.mu.Unlock()
	if err := e.Abort(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	if ended, err := e.TaskEnded(ctx, task); err != nil || !ended {
		t.Fatalf("the aborted call's task ended: %v %v", ended, err)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil && !entry.ToolResult.IsError && strings.Contains(entry.ToolResult.Text, "answer") {
			t.Fatalf("an aborted question has an answer: %+v", entry.ToolResult)
		}
	}
}

// A session made before the question tool gets it, after its other
// tools: one with a tool list gets the tool and its extension, and a
// session that has it is left as it is.
func TestSetQuestionToolGivesItToAnOlderSession(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Tools: []string{"read"}}
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade})
	if err != nil {
		t.Fatal(err)
	}
	want := agentTools(t, e, id)
	if want[len(want)-1] != toolQuestion {
		t.Fatalf("a new supervisor's tools %q", want)
	}
	// As made before the tool: no question, no extension.
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	older := slices.DeleteFunc(slices.Clone(agent.Tools), func(r *durable.ToolRegistration) bool { return r.Name == toolQuestion })
	extensions := slices.DeleteFunc(slices.Clone(agent.Extensions), func(x *durable.Extension) bool { return x.Name == e.questionExt.Name })
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: older}), Extensions: harness.SetTo(harness.ExtensionChange{List: extensions})}); err != nil {
		t.Fatal(err)
	}
	if changed, err := e.SetQuestionTool(ctx, id); err != nil || !changed {
		t.Fatalf("SetQuestionTool = %v, %v", changed, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("tools %q, want %q", got, want)
	}
	agent, _ = conversation.Agent(ctx)
	if !slices.Contains(extensionNames(agent.Extensions), e.questionExt.Name) {
		t.Fatalf("extensions %q", extensionNames(agent.Extensions))
	}
	if changed, err := e.SetQuestionTool(ctx, id); err != nil || changed {
		t.Fatalf("again: %v, %v", changed, err)
	}
	// A team tool given later goes among the team tools, before the
	// question tool.
	withoutTool(t, e, id, toolInbox)
	if changed, err := e.SetSupervisorTools(ctx, id); err != nil || !changed {
		t.Fatalf("SetSupervisorTools = %v, %v", changed, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, want) {
		t.Fatalf("after a later team tool: %q, want %q", got, want)
	}
}

// A private agent's session made before the tool with no tool at all
// (tools: []) gets the question tool alone: its empty list is a selection
// of none, which a new extension adds nothing to (Codex review of #95,
// round 1).
func TestSetQuestionToolGivesItToASessionOfNoTools(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	chani := &profiles.Profile{ID: "chani", Name: "Chani", Role: profiles.RoleAgent, Private: true, Home: f.work, Tools: []string{}}
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
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
	extensions := slices.DeleteFunc(slices.Clone(agent.Extensions), func(x *durable.Extension) bool { return x.Name == e.questionExt.Name })
	if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: []*durable.ToolRegistration{}}),
		Extensions: harness.SetTo(harness.ExtensionChange{List: extensions})}); err != nil {
		t.Fatal(err)
	}
	if got := agentTools(t, e, id); len(got) != 0 {
		t.Fatalf("the older session's tools %q", got)
	}
	if changed, err := e.SetQuestionTool(ctx, id); err != nil || !changed {
		t.Fatalf("SetQuestionTool = %v, %v", changed, err)
	}
	if got := agentTools(t, e, id); !slices.Equal(got, []string{toolQuestion}) {
		t.Fatalf("tools %q, want the question tool alone", got)
	}
}

// Over HTTP, with pocketd's own team: in a supervisor's session, a run the
// owner's message started shows the question on its session, the owner's
// answer is the tool's result and the run goes on with it, and the asks
// never list it; a scheduled run's question is refused at once; an abort
// skips a waiting question. A private agent's session has the tool, a
// specialist's does not.
func TestAQuestionEndToEndOverHTTP(t *testing.T) {
	ctx := context.Background()
	dataDir, profileDir, home := t.TempDir(), t.TempDir(), t.TempDir()
	for name, text := range map[string]string{
		"odrade.md": "---\nmodel: faux/faux-1\nrole: supervisor\nhome: ~\ntools: [read]\n---\n# Odrade\n",
		"chani.md":  "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\ntools: [read]\n---\n# Chani\n",
		"duncan.md": "---\nmodel: faux/faux-1\nhome: ~\ntools: [read]\n---\n# Duncan\n",
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
	question := map[string]any{"question": "Which database?", "options": []any{"Postgres", "SQLite"}}
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approvals.NeedsApproval, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(
		toolTurn(toolQuestion, question, "q1"), answer("Postgres it is."),
		toolTurn(toolQuestion, question, "q2"), answer("I put it in my summary."),
		toolTurn(toolQuestion, question, "q3"),
	))
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

	odrade, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "odrade"})
	if err != nil {
		t.Fatal(err)
	}
	chani, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	duncan, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentTools(t, engine, chani.ID); !slices.Equal(got, []string{"read", toolQuestion}) {
		t.Fatalf("a private agent's tools %q", got)
	}
	if got := agentTools(t, engine, duncan.ID); slices.Contains(got, toolQuestion) {
		t.Fatalf("an agent's tools %q", got)
	}

	// waitQuestion waits for the session event that shows call's question.
	_, _, stream, cancel := hub.Subscribe(0)
	defer cancel()
	waitQuestion := func(call string) proto.Question {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case e := <-stream:
				if e.Type == proto.EventSession && e.Session.ID == odrade.ID && e.Session.Question != nil && e.Session.Question.CallID == call {
					if e.Session.State != proto.SessionWaiting {
						t.Fatalf("a session whose question waits is %s", e.Session.State)
					}
					return *e.Session.Question
				}
			case <-deadline:
				t.Fatalf("no question %s", call)
			}
		}
	}

	if _, err := client.Send(ctx, odrade.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Pick a database."}); err != nil {
		t.Fatal(err)
	}
	q := waitQuestion("q1")
	if asks, err := client.Asks(ctx); err != nil || len(asks) != 0 {
		t.Fatalf("asks while a question waits: %+v, %v", asks, err)
	}
	if _, err := client.AnswerQuestion(ctx, odrade.ID, q.ID, proto.AnswerQuestion{RequestID: proto.NewRequestID(), Option: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, odrade.ID); err != nil {
		t.Fatal(err)
	}
	if got := lastToolResult(t, engine, odrade.ID); got.IsError || got.Text != `{"answer":"Postgres","option":0}` {
		t.Fatalf("the answered question's result %+v", got)
	}
	entries, _, err := engine.Transcript(ctx, odrade.ID)
	if err != nil || entries[len(entries)-1].Text != "Postgres it is." {
		t.Fatalf("the run after the answer: %v", err)
	}

	// A scheduled run's question is refused at once: nobody watches it.
	if _, err := engine.Send(ctx, odrade.ID, "schedule:abc:1", "[scheduled] muster"); err != nil {
		t.Fatal(err)
	}
	if err := engine.WaitIdle(ctx, odrade.ID); err != nil {
		t.Fatal(err)
	}
	if got := lastToolResult(t, engine, odrade.ID); !got.IsError || got.Text != team.ErrNobodyWatching.Error() {
		t.Fatalf("a scheduled run's question %+v", got)
	}
	if _, err := s.WaitingQuestion(ctx, odrade.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused question was kept: %v", err)
	}

	// An abort skips the waiting question.
	if _, err := client.Send(ctx, odrade.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "Pick again."}); err != nil {
		t.Fatal(err)
	}
	q = waitQuestion("q3")
	if err := client.Abort(ctx, odrade.ID, proto.Abort{RequestID: proto.NewRequestID()}); err != nil {
		t.Fatal(err)
	}
	row, err := s.Question(ctx, q.ID)
	if err != nil || row.State != store.QuestionSkipped || row.Reason != proto.SkippedAborted {
		t.Fatalf("the aborted run's question %+v, %v", row, err)
	}
	detail, err := client.Session(ctx, odrade.ID)
	if err != nil || detail.Session.Question != nil || detail.Session.State == proto.SessionWaiting {
		t.Fatalf("the session after the abort %+v, %v", detail.Session, err)
	}
}
