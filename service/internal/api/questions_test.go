package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// The question tool (ADR-0047): the supervisor's and a private agent's
// session put a question to the owner, and the call waits for the answer.

// questionHarness is a team harness with a private agent, chani, the
// owner's message as what started each session's run, and a question
// timer the test fires.
type questionHarness struct {
	*teamHarness
	timerMu sync.Mutex
	timers  []questionTimer
}

type questionTimer struct {
	d         time.Duration
	f         func()
	cancelled *bool
}

func newQuestionHarness(t *testing.T) *questionHarness {
	t.Helper()
	h := &questionHarness{teamHarness: newTeamHarness(t)}
	if err := os.WriteFile(filepath.Join(h.profileDir, "chani.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\nquestionMinutes: 45\n---\n# Chani\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.engine.starters = map[int64]string{h.super.ID: "u:" + h.device + ":m1"}
	h.server.questionTimer = func(d time.Duration, f func()) func() {
		h.timerMu.Lock()
		defer h.timerMu.Unlock()
		cancelled := new(bool)
		h.timers = append(h.timers, questionTimer{d: d, f: f, cancelled: cancelled})
		return func() {
			h.timerMu.Lock()
			defer h.timerMu.Unlock()
			*cancelled = true
		}
	}
	return h
}

// lastTimer is the newest question timer that is not cancelled.
func (h *questionHarness) lastTimer(t *testing.T) questionTimer {
	t.Helper()
	h.timerMu.Lock()
	defer h.timerMu.Unlock()
	for i := len(h.timers) - 1; i >= 0; i-- {
		if !*h.timers[i].cancelled {
			return h.timers[i]
		}
	}
	t.Fatal("no question timer is armed")
	return questionTimer{}
}

// questionCall is a question call that runs in the background.
type questionCall struct {
	done chan struct{}
	text string
	err  error
}

// ask runs the question call in the background and returns it once its
// question waits in the session, as the session's event says, or once
// the call ended first (a refusal).
func (h *questionHarness) ask(t *testing.T, ctx context.Context, call TeamCall, q team.Question) (*questionCall, *proto.Question) {
	t.Helper()
	_, backlog, events, cancel := h.hub.Subscribe(0)
	defer cancel()
	c := &questionCall{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		c.text, c.err = h.server.Question(ctx, call, q)
	}()
	waiting := func(e proto.Event) *proto.Question {
		if e.Type == proto.EventSession && e.Session.ID == call.Session && e.Session.Question != nil && e.Session.Question.CallID == call.CallID {
			return e.Session.Question
		}
		return nil
	}
	for _, e := range backlog {
		if q := waiting(e); q != nil {
			return c, q
		}
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-events:
			if q := waiting(e); q != nil {
				return c, q
			}
		case <-c.done:
			return c, nil
		case <-deadline:
			t.Fatal("no question came")
		}
	}
}

// result is the call's result once it ended.
func (c *questionCall) result(t *testing.T) (string, error) {
	t.Helper()
	select {
	case <-c.done:
		return c.text, c.err
	case <-time.After(10 * time.Second):
		t.Fatal("the question call still waits")
		return "", nil
	}
}

// In the supervisor's session, in a run the owner's message started, a
// question shows on the session (state waiting) and in its detail, never
// among the asks; the owner's answer is the call's result, in the same
// call; the session waits no more; another answer is conflict, the same
// request again answers as it did, and the asks never list it.
func TestAQuestionWaitsForTheOwnersAnswer(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: h.super.ID, Task: 7, CallID: "q1"}
	c, q := h.ask(t, ctx, call, team.Question{Question: " Which database? ", Options: []string{"Postgres", "SQLite"}, Placeholder: "Or name another"})
	if q == nil {
		_, err := c.result(t)
		t.Fatalf("refused: %v", err)
	}
	if q.ID != store.QuestionID(7, "q1") || q.Question != "Which database?" || len(q.Options) != 2 || q.State != proto.QuestionWaiting ||
		q.Placeholder != "Or name another" || !q.Deadline.Equal(h.clock.Now().Add(team.DefaultQuestionTimeout)) {
		t.Fatalf("question %+v", q)
	}
	sessions, err := h.client.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID == h.super.ID && (s.State != proto.SessionWaiting || s.Question == nil || s.Question.ID != q.ID) {
			t.Fatalf("the session while its question waits: %+v", s)
		}
	}
	detail, err := h.client.Session(ctx, h.super.ID)
	if err != nil || detail.Session.Question == nil || detail.Session.Question.ID != q.ID {
		t.Fatalf("the session's detail: %+v, %v", detail.Session, err)
	}
	if asks, err := h.client.Asks(ctx); err != nil || len(asks) != 0 {
		t.Fatalf("asks %+v, %v", asks, err)
	}
	answered, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a1", Option: []int{0}})
	if err != nil || answered.State != proto.QuestionAnswered || answered.Answer != "Postgres" || len(answered.Option) != 1 || answered.SettledBy != "laptop" {
		t.Fatalf("answer %+v, %v", answered, err)
	}
	if text, err := c.result(t); err != nil || text != `{"answer":"Postgres","option":0}` {
		t.Fatalf("the call's result %q, %v", text, err)
	}
	detail, err = h.client.Session(ctx, h.super.ID)
	if err != nil || detail.Session.Question != nil || detail.Session.State == proto.SessionWaiting {
		t.Fatalf("the session once answered: %+v, %v", detail.Session, err)
	}
	// The same request again answers as it did; another answer, from
	// another device too, is conflict: the first answer stands.
	if again, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a1", Option: []int{0}}); err != nil || again.Answer != "Postgres" {
		t.Fatalf("the same answer again: %+v, %v", again, err)
	}
	_, err = h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a2", Answer: "SQLite"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "answered already, on laptop") {
		t.Errorf("the conflict says %v", err)
	}
	_, token, err := auth.CreateDevice(ctx, h.store, "phone")
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := proto.NewClient(base, token, h.http)
	_, err = phone.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a1", Answer: "SQLite"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	// A call run again answers as it did, waiting for nothing.
	if text, err := h.server.Question(ctx, call, team.Question{Question: "Which database?"}); err != nil || text != `{"answer":"Postgres","option":0}` {
		t.Fatalf("the call run again: %q, %v", text, err)
	}
}

// Any device answers a waiting question, and the first answer stands:
// the phone's answer reaches the call, and the laptop's later one is
// conflict. An answer in words, with options of a multiple-choice
// question, keeps both; a skip carries neither.
func TestAnyDeviceAnswersAndTheFirstAnswerStands(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	_, token, err := auth.CreateDevice(ctx, h.store, "phone")
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := proto.NewClient(base, token, h.http)
	c, q := h.ask(t, ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "q1"}, team.Question{Question: "Which?", Options: []string{"a", "b", "c"}, Multiple: true})
	if q == nil {
		t.Fatal("refused")
	}
	if _, err := phone.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "p1", Answer: "a and c, carefully", Option: []int{2, 0}}); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "l1", Skipped: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if text, err := c.result(t); err != nil || text != `{"answer":"a and c, carefully","option":[0,2]}` {
		t.Fatalf("the call's result %q, %v", text, err)
	}

	c, q = h.ask(t, ctx, TeamCall{Session: h.super.ID, Task: 8, CallID: "q2"}, team.Question{Question: "Go on?"})
	if q == nil {
		t.Fatal("refused")
	}
	for name, body := range map[string]proto.AnswerQuestion{
		"no answer":              {RequestID: "b1"},
		"an option of none":      {RequestID: "b2", Option: []int{0}},
		"a skip with an answer":  {RequestID: "b3", Skipped: true, Answer: "yes"},
		"an answer of spaces":    {RequestID: "b4", Answer: "   "},
		"an answer far too long": {RequestID: "b5", Answer: strings.Repeat("x", proto.MaxAnswer+1)},
	} {
		_, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, body)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = h.client.AnswerQuestion(ctx, h.super.ID+1000, q.ID, proto.AnswerQuestion{RequestID: "n1", Answer: "yes"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	if _, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "s1", Skipped: true}); err != nil {
		t.Fatal(err)
	}
	if text, err := c.result(t); err != nil || text != `{"skipped":true,"reason":"owner"}` {
		t.Fatalf("a skip's result %q, %v", text, err)
	}
}

// Only the supervisor's and a private agent's session ask, and the API
// refuses a call from any other, before anything is kept: an agent's, a
// thread's specialist's, a worker's and a plain session's. A private
// agent's session asks, with its profile's timeout.
func TestOnlyTheSupervisorAndAPrivateAgentAsk(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, session := range map[string]int64{"an agent": agent.ID, "a plain session": plain.ID, "a thread's specialist": thread.Session} {
		h.engine.starters[session] = "u:" + h.device + ":m-" + name
		text, err := h.server.Question(ctx, TeamCall{Session: session, Task: 3, CallID: "q-" + name}, team.Question{Question: "Go on?"})
		if err == nil || !strings.Contains(err.Error(), "question is the supervisor's and a private agent's alone") {
			t.Errorf("%s: %q, %v", name, text, err)
		}
		if _, err := h.store.WaitingQuestion(ctx, session); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s kept a question: %v", name, err)
		}
	}
	if _, err := h.server.Question(ctx, TeamCall{Session: thread.Session, Task: 3, CallID: "q-asks"}, team.Question{Question: "Go on?"}); err == nil ||
		!strings.Contains(err.Error(), "as asks, in its report with team_message") {
		t.Errorf("a thread's specialist is not pointed to asks: %v", err)
	}

	chani, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.starters[chani.ID] = "u:" + h.device + ":m2"
	c, q := h.ask(t, ctx, TeamCall{Session: chani.ID, Task: 4, CallID: "q1"}, team.Question{Question: "Tea or coffee?", Options: []string{"Tea", "Coffee"}})
	if q == nil {
		_, err := c.result(t)
		t.Fatalf("a private agent was refused: %v", err)
	}
	if !q.Deadline.Equal(h.clock.Now().Add(45 * time.Minute)) {
		t.Errorf("a private agent's question waits until %v", q.Deadline)
	}
	if _, err := h.client.AnswerQuestion(ctx, chani.ID, q.ID, proto.AnswerQuestion{RequestID: "a1", Answer: "Tea, please"}); err != nil {
		t.Fatal(err)
	}
	if text, err := c.result(t); err != nil || text != `{"answer":"Tea, please"}` {
		t.Fatalf("a private agent's result %q, %v", text, err)
	}
}

// A run the owner's own message did not start asks no one: a call in a
// run a schedule, a note, a report or a supervisor's message started, or
// one no submission started, is refused at once with the issue's words,
// and keeps nothing; a session with no run is refused too.
func TestNobodyWatchingARunNotTheOwnersRefusesAQuestion(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	for name, key := range map[string]string{
		"a schedule":            "schedule:3f2a:1",
		"a note":                "plan:abc:approved",
		"a thread's report":     "report:T1:2",
		"a supervisor's send":   "team-send:4:abc",
		"a launch":              "launch:x:told",
		"no submission":         "",
		"a key that looks like": "uu:" + h.device + ":m1",
	} {
		h.engine.starters[h.super.ID] = key
		text, err := h.server.Question(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "q-" + name}, team.Question{Question: "Go on?"})
		if !errors.Is(err, team.ErrNobodyWatching) || err.Error() != "nobody is watching this run; ask in prose, in your summary" {
			t.Errorf("%s: %q, %v", name, text, err)
		}
	}
	delete(h.engine.starters, h.super.ID)
	if _, err := h.server.Question(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "q-idle"}, team.Question{Question: "Go on?"}); !errors.Is(err, team.ErrNobodyWatching) {
		t.Errorf("no run: %v", err)
	}
	h.engine.starterErr = errors.New("the store is down")
	if _, err := h.server.Question(ctx, TeamCall{Session: h.super.ID, Task: 5, CallID: "q-err"}, team.Question{Question: "Go on?"}); err == nil || errors.Is(err, team.ErrNobodyWatching) {
		t.Errorf("a failed read: %v", err)
	}
	if _, err := h.store.WaitingQuestion(ctx, h.super.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused question was kept: %v", err)
	}
}

// A session has one waiting question at a time: a second call is refused
// while the first waits, and asks once it is settled. A question that
// breaks a rule is refused before anything is kept.
func TestOneQuestionWaitsAtATime(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	c, q := h.ask(t, ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "q1"}, team.Question{Question: "First?"})
	if q == nil {
		t.Fatal("refused")
	}
	if _, err := h.server.Question(ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "q2"}, team.Question{Question: "Second?"}); !errors.Is(err, team.ErrQuestionWaits) {
		t.Fatalf("a second question: %v", err)
	}
	if _, err := h.server.Question(ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "q3"}, team.Question{Question: "Bad?", Options: []string{"only one"}}); err == nil || errors.Is(err, team.ErrQuestionWaits) {
		t.Fatalf("a question that breaks a rule: %v", err)
	}
	if _, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a1", Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.result(t); err != nil {
		t.Fatal(err)
	}
	c, q = h.ask(t, ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "q2"}, team.Question{Question: "Second?"})
	if q == nil {
		_, err := c.result(t)
		t.Fatalf("the second question once the first was answered: %v", err)
	}
	if _, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a2", Skipped: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.result(t); err != nil {
		t.Fatal(err)
	}
}

// A question with no answer by its deadline is skipped, reason timeout,
// and the agent goes on; one whose deadline passed while the service
// stopped is skipped as its call runs again.
func TestAQuestionWithNoAnswerTimesOut(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	c, q := h.ask(t, ctx, TeamCall{Session: h.super.ID, Task: 7, CallID: "q1"}, team.Question{Question: "Still there?"})
	if q == nil {
		t.Fatal("refused")
	}
	timer := h.lastTimer(t)
	if timer.d != team.DefaultQuestionTimeout {
		t.Fatalf("the timer is armed for %v", timer.d)
	}
	h.clock.Advance(timer.d)
	timer.f()
	if text, err := c.result(t); err != nil || text != `{"skipped":true,"reason":"timeout"}` {
		t.Fatalf("a timed-out question's result %q, %v", text, err)
	}
	if row, err := h.store.Question(ctx, q.ID); err != nil || row.State != store.QuestionSkipped || row.SettledBy != "pocketd" {
		t.Fatalf("the question %+v, %v", row, err)
	}
	_, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "late", Answer: "yes"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "no answer came in time") {
		t.Errorf("a late answer's conflict says %v", err)
	}

	// The deadline passes while no call waits: the call run again skips it.
	cancelled, stop := context.WithCancel(ctx)
	c, q = h.ask(t, cancelled, TeamCall{Session: h.super.ID, Task: 8, CallID: "q2"}, team.Question{Question: "And now?"})
	if q == nil {
		t.Fatal("refused")
	}
	stop()
	if _, err := c.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped wait: %v", err)
	}
	if row, err := h.store.Question(ctx, q.ID); err != nil || row.State != store.QuestionWaiting {
		t.Fatalf("a stopped wait settled its question: %+v, %v", row, err)
	}
	h.clock.Advance(team.DefaultQuestionTimeout)
	if text, err := h.server.Question(ctx, TeamCall{Session: h.super.ID, Task: 8, CallID: "q2"}, team.Question{Question: "And now?"}); err != nil || text != `{"skipped":true,"reason":"timeout"}` {
		t.Fatalf("the call run again past its deadline: %q, %v", text, err)
	}
}

// A restart shows the waiting question again and keeps a decision made:
// a new server over the same work database lists the question on its
// session, and the call run again waits for it; an answer made while no
// call waited is the call's result at once.
func TestARestartShowsTheWaitingQuestionAgain(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	cancelled, stop := context.WithCancel(ctx)
	call := TeamCall{Session: h.super.ID, Task: 7, CallID: "q1"}
	c, q := h.ask(t, cancelled, call, team.Question{Question: "Which?", Options: []string{"a", "b"}})
	if q == nil {
		t.Fatal("refused")
	}
	stop() // pocketd stops: the task stops where it is
	if _, err := c.result(t); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	restarted := New(h.store, h.hub, "test", h.clock.Now, nil)
	restarted.SetApprovals(approvals.New(h.store, restarted.PublishApproval))
	restarted.SetEngine(h.engine)
	restarted.questionTimer = h.server.questionTimer
	t.Cleanup(restarted.Stop)
	if err := restarted.RetireQuestions(ctx); err != nil { // its task lives: it stays
		t.Fatal(err)
	}
	h.server = restarted
	h.http.Transport = memTransport{restarted.Handler()}
	sessions, err := h.client.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, s := range sessions {
		shown = shown || s.ID == h.super.ID && s.Question != nil && s.Question.ID == q.ID && s.State == proto.SessionWaiting
	}
	if !shown {
		t.Fatalf("after the restart, the sessions %+v", sessions)
	}
	// The call runs again: same question, waiting again.
	c, again := h.ask(t, ctx, call, team.Question{Question: "Which?", Options: []string{"a", "b"}})
	if again == nil || again.ID != q.ID {
		t.Fatalf("the call run again asked %+v", again)
	}
	if _, err := h.client.AnswerQuestion(ctx, h.super.ID, q.ID, proto.AnswerQuestion{RequestID: "a1", Option: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if text, err := c.result(t); err != nil || text != `{"answer":"b","option":1}` {
		t.Fatalf("the result after the restart %q, %v", text, err)
	}
}

// An abort settles a waiting question as skipped, reason aborted, once
// its task ended; a question whose task still runs keeps waiting, and a
// start skips one whose task ended while the service stopped.
func TestAnAbortSkipsTheWaitingQuestion(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	cancelled, stop := context.WithCancel(ctx)
	c, q := h.ask(t, cancelled, TeamCall{Session: h.super.ID, Task: 7, CallID: "q1"}, team.Question{Question: "Wait?"})
	if q == nil {
		t.Fatal("refused")
	}
	// The abort ends the call's wait as PiG cancels its task.
	h.engine.runFirst = 9
	h.engine.onAbort = func(int64) { stop() }
	h.engine.ended[7] = true
	if err := h.client.Abort(ctx, h.super.ID, proto.Abort{RequestID: "ab1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the aborted call: %v", err)
	}
	row, err := h.store.Question(ctx, q.ID)
	if err != nil || row.State != store.QuestionSkipped || row.Reason != proto.SkippedAborted || row.SettledBy != "pocketd" {
		t.Fatalf("the aborted question %+v, %v", row, err)
	}
	detail, err := h.client.Session(ctx, h.super.ID)
	if err != nil || detail.Session.Question != nil {
		t.Fatalf("the session after the abort: %+v, %v", detail.Session, err)
	}

	// At start: a question whose task ended is skipped, one whose task
	// lives stays.
	if _, _, err := h.store.PutQuestion(ctx, store.QuestionRow{SessionID: h.super.ID, TaskID: 11, CallID: "q2", Question: "Live?", Deadline: h.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.RetireQuestions(ctx); err != nil {
		t.Fatal(err)
	}
	if row, err := h.store.WaitingQuestion(ctx, h.super.ID); err != nil || row.TaskID != 11 {
		t.Fatalf("a live task's question: %+v, %v", row, err)
	}
	h.engine.ended[11] = true
	if err := h.server.RetireQuestions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.WaitingQuestion(ctx, h.super.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an ended task's question still waits: %v", err)
	}
}

// At start, a supervisor's and a private agent's session made before the
// question tool gets it, but a standing one, which takes it with its
// profile; an agent's, a plain session and a thread's specialist do not.
func TestOlderSessionsThatAskGetTheQuestionTool(t *testing.T) {
	h := newQuestionHarness(t)
	ctx := context.Background()
	chani, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "agent", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]bool{h.super.ID: true, chani.ID: true, agent.ID: false, plain.ID: false, thread.Session: false} {
		if h.engine.questionTool[id] != want {
			t.Errorf("session %d given the question tool: %v, want %v", id, h.engine.questionTool[id], want)
		}
	}
}
