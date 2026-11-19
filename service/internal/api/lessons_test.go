package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// duncanSession starts a session from duncan's profile, as the owner does.
func duncanSession(t *testing.T, h *teamHarness, request string) int64 {
	t.Helper()
	s, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: request, Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// lessonEvents are the states the lesson events of lesson id carried.
func (h *teamHarness) lessonEvents(id int64) []proto.LessonState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []proto.LessonState
	for _, e := range h.events {
		if e.Type == proto.EventLesson && e.Lesson.ID == id {
			out = append(out, e.Lesson.State)
		}
	}
	return out
}

// team_memory keeps the calling agent's lessons: a new one waits for the
// owner, a call run again saves nothing twice, and the agent changes or
// drops one that waits at once.
func TestTeamMemoryKeepsTheAgentsLessons(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	call := TeamCall{Session: duncan, Task: 2, CallID: "m1"}
	text, err := h.server.TeamMemory(ctx, call, team.Memory{Action: team.MemoryAdd, Text: "Run  the tests\nbefore a PR."})
	if err != nil || text != team.SavedText(1, "Duncan") {
		t.Fatalf("add: %q, %v", text, err)
	}
	if again, err := h.server.TeamMemory(ctx, call, team.Memory{Action: team.MemoryAdd, Text: "Run the tests before a PR."}); err != nil || again != text {
		t.Fatalf("the call run again: %q, %v", again, err)
	}
	lesson, _ := h.store.Lesson(ctx, 1)
	if lesson.Text != "Run the tests before a PR." || lesson.State != store.LessonPending || lesson.Writer != "duncan" || lesson.SessionID != duncan {
		t.Fatalf("lesson %+v", lesson)
	}
	if got := h.lessonEvents(1); len(got) != 1 || got[0] != proto.LessonPending {
		t.Fatalf("events %v", got)
	}
	list, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, Task: 2, CallID: "m2"}, team.Memory{Action: team.MemoryList})
	if err != nil || !strings.Contains(list, "[L1] Run the tests before a PR. (waiting for the owner's approval)") {
		t.Fatalf("list: %q, %v", list, err)
	}
	if got, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, Task: 2, CallID: "m3"}, team.Memory{Action: team.MemoryUpdate, ID: "L1", Text: "Run every test."}); err != nil || got != team.ChangedText(1) {
		t.Fatalf("update: %q, %v", got, err)
	}
	if got, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, Task: 2, CallID: "m4"}, team.Memory{Action: team.MemoryRemove, ID: "[L1]"}); err != nil || got != team.DiscardedText(1) {
		t.Fatalf("remove: %q, %v", got, err)
	}
	// The call run again answers as it did, and changes nothing.
	if got, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, Task: 2, CallID: "m4"}, team.Memory{Action: team.MemoryRemove, ID: "L1"}); err != nil || got != team.DiscardedText(1) {
		t.Fatalf("remove run again: %q, %v", got, err)
	}
	// A new call on a lesson that is gone says so.
	if got, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, Task: 2, CallID: "m5"}, team.Memory{Action: team.MemoryRemove, ID: "L1"}); err != nil || got != team.GoneText(1) {
		t.Fatalf("remove of a gone lesson: %q, %v", got, err)
	}
	if got := h.lessonEvents(1); len(got) != 3 || got[2] != proto.LessonDiscarded {
		t.Fatalf("events %v", got)
	}
}

// Only a session started from a profile keeps lessons, only its own
// agent's, but for a supervisor's add or list of another's.
func TestTeamMemoryChecksWhoKeepsWhat(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "p", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: plain.ID, CallID: "m"}, team.Memory{Action: team.MemoryAdd, Text: "x"}); err == nil || !strings.Contains(err.Error(), "started from a profile") {
		t.Fatalf("a plain session: %v", err)
	}
	duncan := duncanSession(t, h, "d1")
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "m"}, team.Memory{Action: team.MemoryAdd, Text: "x", Agent: "vera"}); err == nil || !strings.Contains(err.Error(), "only the supervisor") {
		t.Fatalf("an agent for another: %v", err)
	}
	got, err := h.server.TeamMemory(ctx, TeamCall{Session: h.super.ID, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "Ask before a rewrite.", Agent: "duncan"})
	if err != nil || got != team.SavedText(1, "Duncan") {
		t.Fatalf("a supervisor for duncan: %q, %v", got, err)
	}
	if lesson, _ := h.store.Lesson(ctx, 1); lesson.Agent != "duncan" || lesson.Writer != "odrade" {
		t.Fatalf("lesson %+v", lesson)
	}
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: h.super.ID, CallID: "m2"}, team.Memory{Action: team.MemoryAdd, Text: "x", Agent: "nobody"}); err == nil {
		t.Fatal("a lesson for an agent the team does not have")
	}
	// Another agent's lesson is not one this agent changes.
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: h.super.ID, CallID: "m3"}, team.Memory{Action: team.MemoryRemove, ID: "L1"}); err == nil || !strings.Contains(err.Error(), "no lesson L1 for Odrade") {
		t.Fatalf("odrade's remove of duncan's lesson: %v", err)
	}
	if list, err := h.server.TeamMemory(ctx, TeamCall{Session: h.super.ID, CallID: "m4"}, team.Memory{Action: team.MemoryList, Agent: "duncan"}); err != nil || !strings.Contains(list, "[L1] Ask before a rewrite.") {
		t.Fatalf("a supervisor's list of duncan's: %q, %v", list, err)
	}
	// A writer has at most MaxPending lessons waiting for one agent:
	// odrade's waiting lesson for duncan is not duncan's.
	for i := 1; i <= team.MaxPending+1; i++ {
		_, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "a" + strconv.Itoa(i)}, team.Memory{Action: team.MemoryAdd, Text: "lesson " + strconv.Itoa(i)})
		if i <= team.MaxPending && err != nil {
			t.Fatalf("lesson %d: %v", i, err)
		}
		if i == team.MaxPending+1 && (err == nil || !strings.Contains(err.Error(), "wait for the owner's approval already")) {
			t.Fatalf("a writer's waiting lesson past the limit: %v", err)
		}
	}
}

// The owner approves exactly what they were shown: a lesson that changed
// since is a conflict. An approval puts it in force in every session of
// its agent, and in the agent's new sessions.
func TestTheOwnerApprovesWhatTheyWereShown(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "Run the tests."}); err != nil {
		t.Fatal(err)
	}
	waiting, err := h.client.Lessons(ctx)
	if err != nil || len(waiting) != 1 || !waiting[0].Waits() || waiting[0].Agent != "duncan" {
		t.Fatalf("lessons %+v, %v", waiting, err)
	}
	_, err = h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "Run the tests twice."})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	approved, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "Run the tests."})
	if err != nil || approved.State != proto.LessonActive {
		t.Fatalf("approve %+v, %v", approved, err)
	}
	if again, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "Run the tests."}); err != nil || again.State != proto.LessonActive {
		t.Fatalf("the request again: %+v, %v", again, err)
	}
	_, err = h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r2", Decision: proto.LessonApprove, Text: "Run the tests."})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	// The agent's running session gets it once a pass runs.
	h.runScheduled(5)
	h.engine.mu.Lock()
	section := h.engine.lessons[duncan]
	h.engine.mu.Unlock()
	if !strings.Contains(section, "- [L1] Run the tests.") {
		t.Fatalf("duncan's session's lessons:\n%s", section)
	}
	// A new session of the agent starts with it.
	duncanSession(t, h, "d2")
	h.engine.mu.Lock()
	spec := h.engine.specs[len(h.engine.specs)-1]
	h.engine.mu.Unlock()
	if !strings.Contains(spec.Lessons, "- [L1] Run the tests.") {
		t.Fatalf("a new session's lessons:\n%s", spec.Lessons)
	}
	if got := h.lessonEvents(1); len(got) != 2 || got[1] != proto.LessonActive {
		t.Fatalf("events %v", got)
	}
}

// A change to a lesson in force waits, and the lesson stays in force until
// the owner approves the change: new text, or forgetting it. Discard drops
// a change, and forget removes a lesson at once.
func TestChangesToALessonInForceWaitForTheOwner(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	memory := func(call string, m team.Memory) string {
		t.Helper()
		got, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: call}, m)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	decide := func(request string, d proto.DecideLesson) proto.Lesson {
		t.Helper()
		d.RequestID = request
		got, err := h.client.DecideLesson(ctx, 1, d)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	section := func() string {
		t.Helper()
		h.runScheduled(5)
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		return h.engine.lessons[duncan]
	}
	memory("m1", team.Memory{Action: team.MemoryAdd, Text: "a"})
	decide("r1", proto.DecideLesson{Decision: proto.LessonApprove, Text: "a"})
	if got := memory("m2", team.Memory{Action: team.MemoryUpdate, ID: "L1", Text: "b"}); got != team.ProposedText(1) {
		t.Fatalf("update: %q", got)
	}
	if !strings.Contains(section(), "- [L1] a\n") {
		t.Fatal("a change took effect before the owner approved it")
	}
	if got := decide("r2", proto.DecideLesson{Decision: proto.LessonApprove, Text: "b"}); got.Text != "b" || got.Change != nil {
		t.Fatalf("approve the change: %+v", got)
	}
	if !strings.Contains(section(), "- [L1] b\n") {
		t.Fatal("the approved change did not reach the session")
	}
	memory("m3", team.Memory{Action: team.MemoryUpdate, ID: "L1", Text: "c"})
	if got := decide("r3", proto.DecideLesson{Decision: proto.LessonDiscard}); got.Text != "b" || got.Change != nil || got.State != proto.LessonActive {
		t.Fatalf("discard the change: %+v", got)
	}
	if got := memory("m4", team.Memory{Action: team.MemoryRemove, ID: "L1"}); got != team.ForgetText(1) {
		t.Fatalf("remove: %q", got)
	}
	_, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r4", Decision: proto.LessonApprove, Text: "b"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if got := decide("r4", proto.DecideLesson{Decision: proto.LessonApprove, Forget: true}); got.State != proto.LessonForgotten {
		t.Fatalf("approve forgetting: %+v", got)
	}
	if s := section(); strings.Contains(s, "[L1]") || !strings.Contains(s, "none yet") {
		t.Fatalf("a forgotten lesson stayed:\n%s", s)
	}
	// The owner forgets a lesson in force at once.
	memory("m5", team.Memory{Action: team.MemoryAdd, Text: "d"})
	if _, err := h.client.DecideLesson(ctx, 2, proto.DecideLesson{RequestID: "r5", Decision: proto.LessonApprove, Text: "d"}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.client.DecideLesson(ctx, 2, proto.DecideLesson{RequestID: "r6", Decision: proto.LessonForget}); err != nil || got.State != proto.LessonForgotten {
		t.Fatalf("forget: %+v, %v", got, err)
	}
	if lessons, _ := h.client.Lessons(ctx); len(lessons) != 0 {
		t.Fatalf("lessons gone stay listed: %+v", lessons)
	}
}

// A decision is checked before anything is decided.
func TestADecisionNamesWhatItDecides(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	for _, d := range []proto.DecideLesson{
		{RequestID: "r", Decision: proto.LessonApprove},
		{RequestID: "r", Decision: proto.LessonApprove, Text: "a", Forget: true},
		{RequestID: "r", Decision: proto.LessonDiscard, Text: "a"},
		{RequestID: "r", Decision: proto.LessonForget, Forget: true},
		{RequestID: "r", Decision: "keep"},
	} {
		_, err := h.client.DecideLesson(ctx, 1, d)
		asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	}
	_, err := h.client.DecideLesson(ctx, 9, proto.DecideLesson{RequestID: "r", Decision: proto.LessonForget})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}

// Lessons that could not reach a session are tried again by the next
// pass, and the first pass after a start gives every agent's sessions
// theirs.
func TestLessonsReachEverySessionInTheEnd(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.lessonsErr = errors.New("durable store is down")
	h.engine.mu.Unlock()
	if _, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(1)
	h.engine.mu.Lock()
	h.engine.lessonsErr = nil
	h.engine.mu.Unlock()
	h.runScheduled(5)
	h.engine.mu.Lock()
	got := h.engine.lessons[duncan]
	h.engine.lessons = map[int64]string{}
	h.engine.mu.Unlock()
	if !strings.Contains(got, "- [L1] a") {
		t.Fatalf("after a failure, the session's lessons:\n%s", got)
	}
	// At start, every session started from a profile gets its agent's.
	h.server.RefreshAllLessons()
	h.server.StartDelivery()
	h.runScheduled(5)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !strings.Contains(h.engine.lessons[duncan], "- [L1] a") || !strings.Contains(h.engine.lessons[h.super.ID], "none yet") {
		t.Fatalf("at start: %v", h.engine.lessons)
	}
}

// A lesson approved while a session of its agent was being made reaches
// that session too: the session's instructions read the lessons before
// the approval's refresh looked for the agent's sessions, which did not
// list it yet (Codex review of #18, round 5: through session creation).
func TestALessonApprovedWhileASessionStartsReachesIt(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(5)
	// A second session's creation stops once it read the lessons (none
	// in force), before the engine makes it and pocketd records it.
	read, release := make(chan struct{}), make(chan struct{})
	h.engine.mu.Lock()
	h.engine.onCreate = func(SessionSpec) {
		h.engine.mu.Lock()
		h.engine.onCreate = nil
		h.engine.mu.Unlock()
		close(read)
		<-release
	}
	h.engine.mu.Unlock()
	created := make(chan proto.Session, 1)
	go func() {
		s, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "d2", Profile: "duncan"})
		if err != nil {
			t.Error(err)
		}
		created <- s
	}()
	<-read
	// The approval and its refresh run while the session is unrecorded.
	if _, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(5)
	close(release)
	second := <-created
	h.runScheduled(5)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !strings.Contains(h.engine.lessons[second.ID], "- [L1] a") {
		t.Fatalf("the session made meanwhile:\n%s", h.engine.lessons[second.ID])
	}
}

// A team_memory call that saved or changed a lesson answers as it did when
// it runs again, whatever the profiles say now: a profile removed since
// the call does not turn its done work into a failure (Codex review of
// #18, round 1).
func TestAMemoryCallRunAgainAnswersAsItDid(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	add := TeamCall{Session: duncan, Task: 2, CallID: "m1"}
	first, err := h.server.TeamMemory(ctx, add, team.Memory{Action: team.MemoryAdd, Text: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	update := TeamCall{Session: duncan, Task: 2, CallID: "m2"}
	proposed, err := h.server.TeamMemory(ctx, update, team.Memory{Action: team.MemoryUpdate, ID: "L1", Text: "b"})
	if err != nil || proposed != team.ProposedText(1) {
		t.Fatalf("update: %q, %v", proposed, err)
	}
	// The owner discards the change; then the profiles go.
	if _, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r2", Decision: proto.LessonDiscard}); err != nil {
		t.Fatal(err)
	}
	h.writeProfiles(map[string]string{"odrade.md": "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n"})
	// The agent is named by its id once its profile is gone.
	if again, err := h.server.TeamMemory(ctx, add, team.Memory{Action: team.MemoryAdd, Text: "a"}); err != nil || again != team.SavedText(1, "duncan") {
		t.Fatalf("the add run again: %q, %v (first %q)", again, err, first)
	}
	if again, err := h.server.TeamMemory(ctx, update, team.Memory{Action: team.MemoryUpdate, ID: "L1", Text: "b"}); err != nil || again != proposed {
		t.Fatalf("the update run again: %q, %v", again, err)
	}
	if lesson, _ := h.store.Lesson(ctx, 1); lesson.Waits() {
		t.Fatalf("the discarded change came back: %+v", lesson)
	}
}

// An approval names the text exactly: white space around it is another
// text (Codex review of #18, round 1).
func TestAnApprovalComparesTheTextAsSent(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "Run tests"}); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: " Run tests\n"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// A flush of what waits (DeliverPending) waits for a lessons refresh a
// scheduled pass has under way: it returns only once the sessions have
// their lessons, and two refreshes never write over each other (Codex
// review of #18, round 1).
func TestAFlushWaitsForTheRefreshUnderWay(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	duncan := duncanSession(t, h, "d1")
	if _, err := h.server.TeamMemory(ctx, TeamCall{Session: duncan, CallID: "m1"}, team.Memory{Action: team.MemoryAdd, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	h.runScheduled(5)
	if _, err := h.client.DecideLesson(ctx, 1, proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	inRefresh, release := make(chan struct{}), make(chan struct{})
	h.engine.mu.Lock()
	h.engine.onSetLessons = func() {
		h.engine.mu.Lock()
		h.engine.onSetLessons = nil
		h.engine.mu.Unlock()
		close(inRefresh)
		<-release
	}
	h.engine.mu.Unlock()
	h.retryMu.Lock()
	pass := h.scheduled[len(h.scheduled)-1]
	h.scheduled = nil
	h.retryMu.Unlock()
	passDone := make(chan struct{})
	go func() { pass(); close(passDone) }()
	<-inRefresh
	flushed := make(chan struct{})
	go func() {
		_ = h.server.DeliverPending(ctx)
		close(flushed)
	}()
	for !refreshWaits() {
		select {
		case <-flushed:
			// The pass goes on, so the test's cleanup does not wait on it.
			close(release)
			<-passDone
			t.Fatal("the flush returned while a refresh was under way")
		default:
			runtime.Gosched()
		}
	}
	close(release)
	<-passDone
	<-flushed
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !strings.Contains(h.engine.lessons[duncan], "- [L1] a") {
		t.Fatalf("after the flush:\n%s", h.engine.lessons[duncan])
	}
}

// refreshWaits says whether a goroutine waits to refresh lessons while
// another refreshes.
func refreshWaits() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "(*Server).refreshLessons") && strings.Contains(g, "sync.(*Mutex).Lock") {
			return true
		}
	}
	return false
}

// A specialist's lessons section is written from the profile that passed
// the last checks: a profile changed after them is not read again (Codex
// review of #18, round 3).
func TestASpecialistsLessonsComeFromTheCheckedProfile(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.server.afterRecheck = func() {
		h.server.afterRecheck = nil
		if err := os.WriteFile(filepath.Join(h.profileDir, "duncan.md"), []byte("---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Someone Else\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "x"}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	spec := h.engine.specs[len(h.engine.specs)-1]
	h.engine.mu.Unlock()
	if spec.Lessons != team.LessonsSection(nil) || strings.Contains(spec.Lessons, "Someone Else") {
		t.Fatalf("the specialist's lessons:\n%s", spec.Lessons)
	}
	// A delivery pass after the change leaves the section as it is: it
	// depends on the lessons alone (Codex review of #18, round 4).
	h.server.RefreshAllLessons()
	h.server.StartDelivery()
	h.runScheduled(5)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.lessons) == 0 {
		t.Fatal("no pass refreshed the sessions' lessons")
	}
	for id, section := range h.engine.lessons {
		if section != team.LessonsSection(nil) {
			t.Fatalf("session %d's lessons after a pass:\n%s", id, section)
		}
	}
}
