package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// deepseekFlash is the team's default worker model, on an API key.
var deepseekFlash = proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}

// workerHarness is a team whose thread T1, Vera's, can have a worker on
// the DeepSeek API.
func workerHarness(t *testing.T) (*teamHarness, store.ThreadRow) {
	t.Helper()
	h := newTeamHarness(t)
	h.engine.mu.Lock()
	h.engine.workerModels = []proto.Model{{ModelRef: deepseekFlash, Name: "DeepSeek V4.1 Flash"}}
	h.engine.mu.Unlock()
	return h, veraThread(t, h)
}

// The worker's spec, as the engine was asked to create it.
func workerSpec(t *testing.T, h *teamHarness) SessionSpec {
	t.Helper()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, spec := range h.engine.specs {
		if spec.Worker {
			return spec
		}
	}
	t.Fatal("no worker was created")
	return SessionSpec{}
}

// A thread's specialist starts its worker with its first team_worker
// call: on the team's default worker model, in the thread's folder, with
// the message as its brief; a later call goes to the same worker
// (ADR-0021).
func TestAThreadsSpecialistStartsItsWorker(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}
	answer, err := h.server.TeamWorker(ctx, call, team.WorkerMessage{Message: "Fix the flaky test in ci_test.go."})
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t, h)
	if spec.Model != deepseekFlash || spec.Route != "worker-default" || spec.Cwd != thread.Cwd || spec.Thread != thread.ID || spec.Profile != nil {
		t.Fatalf("worker spec %+v", spec)
	}
	got, err := h.store.Thread(ctx, thread.ID)
	if err != nil || got.WorkerSession == 0 {
		t.Fatalf("thread %+v, %v", got, err)
	}
	if !strings.Contains(answer, "session "+strconv.FormatInt(got.WorkerSession, 10)) || !strings.Contains(answer, "deepseek/deepseek-flash") {
		t.Fatalf("answer %q", answer)
	}
	h.engine.mu.Lock()
	sent, to := h.engine.texts[workerMessagePrefix+call.key()+":report"], h.engine.to[workerMessagePrefix+call.key()+":report"]
	created := h.engine.created
	h.engine.mu.Unlock()
	if sent != "Fix the flaky test in ci_test.go." || to != got.WorkerSession {
		t.Fatalf("the brief %q went to %d", sent, to)
	}
	// The same call run again sends nothing twice; another goes to the same
	// worker.
	if _, err := h.server.TeamWorker(ctx, call, team.WorkerMessage{Message: "Fix the flaky test in ci_test.go."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 8, CallID: "w2"}, team.WorkerMessage{Message: "Also run make ci."}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	again := h.engine.created
	followUp := h.engine.to[workerMessagePrefix+"team:"+strconv.FormatInt(thread.Session, 10)+":8:w2:report"]
	h.engine.mu.Unlock()
	if again != created || followUp != got.WorkerSession {
		t.Fatalf("sessions %d then %d; the follow-up went to %d", created, again, followUp)
	}
}

// A profile's worker field chooses its threads' worker model (ADR-0021).
func TestAProfilesWorkerFieldChoosesTheModel(t *testing.T) {
	h, thread := workerHarness(t)
	h.writeProfiles(map[string]string{
		"vera.md": "---\nmodel: faux/faux-1\nworker: deepseek/deepseek-v4-pro\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Vera\n",
	})
	pro := proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4-pro"}
	h.engine.mu.Lock()
	h.engine.workerModels = append(h.engine.workerModels, proto.Model{ModelRef: pro})
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWorker(context.Background(), TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Go."}); err != nil {
		t.Fatal(err)
	}
	if spec := workerSpec(t, h); spec.Model != pro || spec.Route != "worker" {
		t.Fatalf("worker spec %+v", spec)
	}
}

// team_worker refuses a caller that is no thread's specialist, a
// cross-review, a closed thread, an empty message, and a worker model
// pocketd cannot run, whose answer tells the specialist to stop and tell
// the owner (ADR-0021).
func TestTeamWorkerRefuses(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: h.super.ID, Task: 1, CallID: "w"}, team.WorkerMessage{Message: "Go."}); err == nil {
		t.Error("a supervisor got a worker")
	}
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 1, CallID: "w"}, team.WorkerMessage{Message: "  "}); err == nil {
		t.Error("an empty message went")
	}
	h.engine.mu.Lock()
	h.engine.workerModels = nil
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 1, CallID: "w"}, team.WorkerMessage{Message: "Go."}); err == nil ||
		!strings.Contains(err.Error(), "tell the owner") {
		t.Errorf("no worker model: %v", err)
	}
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 2, CallID: "w"}, team.WorkerMessage{Message: "Go."}); err == nil {
		t.Error("a closed thread got a worker")
	}
}

// startedWorker is thread's worker, started with a first message.
func startedWorker(t *testing.T, h *teamHarness, thread store.ThreadRow) int64 {
	t.Helper()
	if _, err := h.server.TeamWorker(context.Background(), TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Go."}); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.Thread(context.Background(), thread.ID)
	if err != nil || got.WorkerSession == 0 {
		t.Fatalf("thread %+v, %v", got, err)
	}
	return got.WorkerSession
}

// The reply a worker's run ended with reaches its specialist once, as a
// message that names the thread (ADR-0021).
func TestAWorkersReplyReachesItsSpecialistOnce(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Changed ci_test.go; make ci passes."},
	}}
	h.engine.mu.Unlock()
	ended := make(chan int64, 2)
	h.server.afterRunEnded = func(id int64) { ended <- id }
	for range 2 {
		h.server.PublishLive(proto.Live{SessionID: worker, Busy: true})
		h.server.PublishLive(proto.Live{SessionID: worker, Busy: false})
		<-ended
	}
	h.engine.mu.Lock()
	var replies []string
	for key, text := range h.engine.texts {
		if strings.HasPrefix(key, "worker-reply:") && h.engine.to[key] == thread.Session {
			replies = append(replies, text)
		}
	}
	h.engine.mu.Unlock()
	if len(replies) != 1 || replies[0] != "[T1 · worker] Changed ci_test.go; make ci passes." {
		t.Fatalf("replies to the specialist %q", replies)
	}
}

// The owner sends a worker nothing: its specialist directs it (ADR-0021).
func TestAWorkerTakesNoMessageFromTheOwner(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	var pe *proto.Error
	_, err := h.client.Send(context.Background(), worker, proto.SendMessage{RequestID: "o1", Text: "Stop that."})
	if !errors.As(err, &pe) || pe.Status != http.StatusConflict {
		t.Fatalf("the owner's message to a worker: %v", err)
	}
}

// A thread's close stops its worker's run, and a reply that comes after
// reaches no one (ADR-0021).
func TestAClosedThreadsWorkerStops(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.runFirst = 9
	h.engine.mu.Unlock()
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	aborted := append([]int64(nil), h.engine.aborted...)
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 1, Kind: proto.EntryUser, Text: "Go."}, {ID: 2, Kind: proto.EntryAssistant, Text: "Done."}}}
	h.engine.mu.Unlock()
	if len(aborted) == 0 || aborted[len(aborted)-1] != worker {
		t.Fatalf("aborted %v, want the worker %d", aborted, worker)
	}
	ended := make(chan int64, 1)
	h.server.afterRunEnded = func(id int64) { ended <- id }
	h.server.PublishLive(proto.Live{SessionID: worker, Busy: true})
	h.server.PublishLive(proto.Live{SessionID: worker, Busy: false})
	<-ended
	if unsent, _ := h.store.UnsentReportsOf(ctx, thread.ID); len(unsent) != 0 {
		t.Fatalf("a closed thread's worker reply was kept: %+v", unsent)
	}
}

// The service's start sends a worker's reply that a stop left unsent
// (ADR-0021).
func TestTheStartSendsAWorkersLeftReply(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 3, Kind: proto.EntryUser, Text: "Go."}, {ID: 4, Kind: proto.EntryAssistant, Text: "All done."}}}
	h.engine.mu.Unlock()
	h.server.reviewAsync = false // the start's passes run in the call
	h.server.StartPulls(0)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for key, text := range h.engine.texts {
		if strings.HasPrefix(key, "worker-reply:") && text == "[T1 · worker] All done." {
			return
		}
	}
	t.Fatalf("no reply after the start: %v", h.engine.texts)
}

// A cross-review has no worker: it reads its change set and files its
// verdict, whatever tools reach it (ADR-0019, ADR-0021).
func TestACrossReviewHasNoWorker(t *testing.T) {
	h := newReviewHarness(t)
	h.engine.mu.Lock()
	h.engine.workerModels = []proto.Model{{ModelRef: deepseekFlash}}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWorker(context.Background(), TeamCall{Session: h.teg.Session, Task: 1, CallID: "w"}, team.WorkerMessage{Message: "Review it for me."}); err == nil ||
		!strings.Contains(err.Error(), "cross-review has no worker") {
		t.Fatalf("a review's worker: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, spec := range h.engine.specs {
		if spec.Worker {
			t.Fatal("a worker was created for a review")
		}
	}
}

// endRun has the worker's run end, and waits for the service to follow it.
func endRun(t *testing.T, h *teamHarness, worker int64) {
	t.Helper()
	ended := make(chan int64, 1)
	h.server.afterRunEnded = func(id int64) { ended <- id }
	h.server.PublishLive(proto.Live{SessionID: worker, Busy: true})
	h.server.PublishLive(proto.Live{SessionID: worker, Busy: false})
	<-ended
	h.server.afterRunEnded = nil
}

// repliesTo are the worker replies the engine got for session, in no
// order.
func repliesTo(h *teamHarness, session int64) []string {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	var out []string
	for key, text := range h.engine.texts {
		if strings.HasPrefix(key, workerReplyPrefix) && h.engine.to[key] == session {
			out = append(out, text)
		}
	}
	slices.Sort(out)
	return out
}

// Two runs back to back leave no idle moment between them: each reply
// reaches the specialist all the same, and a run that ended with no reply
// says so (Codex review of #29, round 1).
func TestEachWorkerRunsReplyReachesTheSpecialist(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Reading.", StopReason: "toolUse"},
		{ID: 3, Kind: proto.EntryToolResult},
		{ID: 4, Kind: proto.EntryAssistant, Text: "First done.", StopReason: "stop"},
		{ID: 5, Kind: proto.EntryUser, Text: "Fix it."},
		{ID: 6, Kind: proto.EntryAssistant, Text: "Second done.", StopReason: "stop"},
		{ID: 7, Kind: proto.EntryUser, Text: "And this."},
	}}
	h.engine.mu.Unlock()
	endRun(t, h, worker)
	want := []string{"[T1 · worker] First done.", "[T1 · worker] Second done.", "[T1 · worker] The worker's run ended with no reply: no reason was given."}
	if got := repliesTo(h, thread.Session); !slices.Equal(got, want) {
		t.Fatalf("replies %q, want %q", got, want)
	}
}

// A profile pocketd cannot read starts no worker, rather than one with
// every tool (Codex review of #29, round 1).
func TestAnUnreadProfileStartsNoWorker(t *testing.T) {
	h, thread := workerHarness(t)
	if err := os.Remove(filepath.Join(h.profileDir, "vera.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWorker(context.Background(), TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Go."}); err == nil ||
		!strings.Contains(err.Error(), "profile") {
		t.Fatalf("a worker with no profile: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, spec := range h.engine.specs {
		if spec.Worker {
			t.Fatalf("a worker started: %+v", spec)
		}
	}
}

// A message to a closed thread's worker is refused under the lock a close
// holds as it commits: no run starts after a close (Codex review of #29,
// round 1).
func TestNoWorkerRunStartsAfterAClose(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.server.queueWorkerMessage(ctx, thread.ID, worker, workerMessagePrefix+"late", "Go on."); err == nil {
		t.Fatal("a message was kept for a closed thread's worker")
	}
	if unsent, _ := h.store.UnsentReportsOf(ctx, thread.ID); len(unsent) != 0 {
		t.Fatalf("it waits: %+v", unsent)
	}
}

// The service's start stops a closed thread's worker that a stop left
// running (Codex review of #29, round 1).
func TestTheStartStopsAClosedThreadsWorker(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.runFirst = 9
	h.engine.mu.Unlock()
	h.server.reviewAsync = false
	h.server.StartPulls(0)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !slices.Contains(h.engine.aborted, worker) {
		t.Fatalf("aborted %v, want the worker %d", h.engine.aborted, worker)
	}
}

// A worker an earlier run made is recorded as it was made, whatever the
// profile says now (Codex review of #29, round 1).
func TestAWorkerIsRecordedAsItWasMade(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	made := proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4-pro"}
	if _, err := h.engine.CreateSession(ctx, workerKey(thread), SessionSpec{Cwd: thread.Cwd, Model: made, Route: "worker", Thread: thread.ID, Worker: true}); err != nil {
		t.Fatal(err)
	}
	worker := startedWorker(t, h, thread)
	row, err := h.store.Session(ctx, worker)
	if err != nil || row.Provider+"/"+row.Model != made.String() || row.Route != "worker" {
		t.Fatalf("the worker's row %+v, %v", row, err)
	}
}

// Every message to a worker watches it first: one whose watch did not
// start still reaches the event stream (Codex review of #29, round 1).
func TestEachMessageToAWorkerWatchesIt(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	delete(h.engine.watching, worker)
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWorker(context.Background(), TeamCall{Session: thread.Session, Task: 8, CallID: "w2"}, team.WorkerMessage{Message: "Again."}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.watching[worker] {
		t.Fatal("the worker is not watched")
	}
}

// A worker's replies that could not be read when its run ended are read
// again by the delivery pass (Codex review of #29, round 1).
func TestAWorkerReplyThatFailedIsTriedAgain(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcriptErr = errors.New("the store is busy")
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 1, Kind: proto.EntryUser, Text: "Go."}, {ID: 2, Kind: proto.EntryAssistant, Text: "Done."}}}
	h.engine.mu.Unlock()
	endRun(t, h, worker)
	if got := repliesTo(h, thread.Session); len(got) != 0 {
		t.Fatalf("a reply went though its transcript could not be read: %q", got)
	}
	if done, err := h.server.deliverAll(context.Background()); err != nil || !done {
		t.Fatalf("the delivery pass: %v, %v", done, err)
	}
	if got := repliesTo(h, thread.Session); !slices.Equal(got, []string{"[T1 · worker] Done."}) {
		t.Fatalf("replies after the delivery pass %q", got)
	}
}

// A worker's reply still queued when its thread closes is withdrawn: it
// goes to no one (Codex review of #29, round 1).
func TestAClosedThreadsQueuedReplyIsWithdrawn(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 1, Kind: proto.EntryUser, Text: "Go."}, {ID: 2, Kind: proto.EntryAssistant, Text: "Done."}}}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasPrefix(key, workerReplyPrefix) {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	endRun(t, h, worker)
	if unsent, _ := h.store.UnsentReportsOf(ctx, thread.ID); len(unsent) != 1 {
		t.Fatalf("the reply is not waiting: %+v", unsent)
	}
	if _, err := h.server.TeamClose(ctx, TeamCall{Session: h.super.ID, CallID: "close"}, thread.ID); err != nil {
		t.Fatal(err)
	}
	if unsent, _ := h.store.UnsentReportsOf(ctx, thread.ID); len(unsent) != 0 {
		t.Fatalf("the closed thread's reply still waits: %+v", unsent)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	withdrawn := false
	for _, key := range h.engine.withdrawals {
		withdrawn = withdrawn || strings.HasPrefix(key, workerReplyPrefix)
	}
	if !withdrawn {
		t.Fatalf("withdrawals %q", h.engine.withdrawals)
	}
}

// A supervisor that takes no message holds up its own reports alone: the
// specialist's worker reply goes all the same (Codex review of #29,
// round 1).
func TestEachRecipientTakesItsReportsInOrder(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.failTo = map[int64]bool{h.super.ID: true}
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 1, Kind: proto.EntryUser, Text: "Go."}, {ID: 2, Kind: proto.EntryAssistant, Text: "Done."}}}
	h.engine.mu.Unlock()
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 9, CallID: "m1"}, team.Report{Message: "Working.", Status: team.StatusWorking}); err != nil {
		t.Fatal(err)
	}
	endRun(t, h, worker)
	if got := repliesTo(h, thread.Session); !slices.Equal(got, []string{"[T1 · worker] Done."}) {
		t.Fatalf("the specialist's replies %q", got)
	}
}

// A run stopped in a tool call, with no reply of the model's, says it
// ended with none and why; a run still at work, as the transcript itself
// says, says nothing yet (Codex review of #29, round 2).
func TestAWorkerRunStoppedInAToolSaysSo(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Running the tests.", StopReason: "toolUse"},
		{ID: 3, Kind: proto.EntryToolResult},
	}
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: entries}
	h.engine.transcriptLive = map[int64]proto.Live{worker: {Busy: true}}
	h.engine.mu.Unlock()
	endRun(t, h, worker)
	if got := repliesTo(h, thread.Session); len(got) != 0 {
		t.Fatalf("a run at work replied: %q", got)
	}
	h.engine.mu.Lock()
	h.engine.transcriptLive = map[int64]proto.Live{worker: {Failure: "aborted by the owner"}}
	h.engine.mu.Unlock()
	endRun(t, h, worker)
	if got := repliesTo(h, thread.Session); !slices.Equal(got, []string{"[T1 · worker] The worker's run ended with no reply: aborted by the owner."}) {
		t.Fatalf("replies %q", got)
	}
}

// A message that starts the worker's next run sends the reply of the run
// before, though the worker never went idle between them (Codex review of
// #29, round 2).
func TestANewRunSendsTheReplyBefore(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "First done.", StopReason: "stop"},
		{ID: 3, Kind: proto.EntryUser, Text: "Fix it."},
	}}
	h.engine.transcriptLive = map[int64]proto.Live{worker: {Busy: true}}
	h.engine.mu.Unlock()
	started := make(chan int64, 1)
	h.server.afterRunStarted = func(id int64) {
		if id == worker {
			started <- id
		}
	}
	h.server.PublishEntry(proto.Entry{ID: 3, SessionID: worker, Kind: proto.EntryUser, Text: "Fix it."})
	<-started
	if got := repliesTo(h, thread.Session); !slices.Equal(got, []string{"[T1 · worker] First done."}) {
		t.Fatalf("replies %q", got)
	}
}

// A thread's worker starts one at a time: two first calls of a specialist,
// which PiG runs side by side, start one (Codex review of #29, round 2).
func TestAWorkerStartsOneAtATime(t *testing.T) {
	h, thread := workerHarness(t)
	held := false
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Worker {
			if h.server.workerStartMu.TryLock() {
				h.server.workerStartMu.Unlock()
			} else {
				held = true
			}
		}
	}
	startedWorker(t, h, thread)
	if !held {
		t.Fatal("the worker was made outside workerStartMu")
	}
}

// A merged thread's close holds outboxMu, as a message to its worker
// does, and withdraws its worker's queued replies before it commits
// (Codex review of #29, round 2).
func TestAMergedThreadsCloseWithdrawsRepliesFirst(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "open", false)})
	if _, err := h.server.TeamMessage(ctx, TeamCall{Session: thread.Session, Task: 5, CallID: "m1"},
		team.Report{Message: "PR #3.", Status: team.StatusWorking, PullRequests: []team.PullRef{{Number: 3}}}); err != nil {
		t.Fatal(err)
	}
	worker := startedWorker(t, h, thread)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 1, Kind: proto.EntryUser, Text: "Go."}, {ID: 2, Kind: proto.EntryAssistant, Text: "Done."}}}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasPrefix(key, workerReplyPrefix) {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	endRun(t, h, worker)
	var locked, open []bool
	h.engine.onWithdraw = func(key string) {
		if !strings.HasPrefix(key, workerReplyPrefix) {
			return
		}
		if h.server.outboxMu.TryLock() {
			h.server.outboxMu.Unlock()
			locked = append(locked, false)
		} else {
			locked = append(locked, true)
		}
		now, _ := h.store.Thread(ctx, thread.ID)
		open = append(open, now.State != store.ThreadClosed)
	}
	answerGH(h, map[string]string{"repos/frostyard/example/pulls/3": githubPull("frostyard/example", 3, thread.Branch, "frostyard/example", "closed", true)})
	if _, err := h.server.RefreshPulls(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Thread(ctx, thread.ID); got.State != store.ThreadClosed {
		t.Fatalf("the merged thread %+v", got)
	}
	if len(locked) == 0 || slices.Contains(locked, false) || slices.Contains(open, false) {
		t.Fatalf("withdrawals: under outboxMu %v, before the close %v", locked, open)
	}
	if unsent, _ := h.store.UnsentReportsOf(ctx, thread.ID); len(unsent) != 0 {
		t.Fatalf("the closed thread's reply still waits: %+v", unsent)
	}
}

// A session made before workers on a model that serves workers alone runs
// no more: the owner's message to it is refused, and the service does not
// start while one is there (Codex review of #29, rounds 3 and 4).
func TestASessionOnAWorkersModelRunsNoMore(t *testing.T) {
	h, _ := workerHarness(t)
	ctx := context.Background()
	if _, err := h.store.InsertSession(ctx, store.SessionRow{ID: 900, Title: "old", Cwd: "/", Provider: "deepseek", Model: "deepseek-flash", Route: "owner"}); err != nil {
		t.Fatal(err)
	}
	var pe *proto.Error
	if _, err := h.client.Send(ctx, 900, proto.SendMessage{RequestID: "o1", Text: "Go on."}); !errors.As(err, &pe) || pe.Status != http.StatusConflict ||
		!strings.Contains(pe.Message, "workers alone") {
		t.Fatalf("the owner's message: %v", err)
	}
	// The service refuses to start, before PiG resumes a run, while such a
	// session is there (Codex review of #29, round 4); a worker's own
	// session on it is fine.
	if err := h.server.CheckWorkerOnlySessions(ctx); err == nil || !strings.Contains(err.Error(), "session 900") {
		t.Fatalf("the start's check: %v", err)
	}
	if _, err := h.store.InsertSession(ctx, store.SessionRow{ID: 901, Title: "T1 · worker", Cwd: "/", Provider: "deepseek", Model: "deepseek-flash", Route: "worker-default"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.CheckWorkerOnlySessions(ctx); err == nil || strings.Contains(err.Error(), "session 901") {
		t.Fatalf("the start's check names a worker: %v", err)
	}
}

// A message to a worker that a failed run left in its queue goes again:
// it counts as sent only once it is in the worker's transcript (Codex
// review of #29, round 3).
func TestAStrandedWorkerMessageGoesAgain(t *testing.T) {
	h, thread := workerHarness(t)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.mu.Unlock()
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasPrefix(key, workerMessagePrefix) {
			h.engine.mu.Lock()
			h.engine.queued[int64(len(h.engine.sent)+100)] = true
			h.engine.mu.Unlock()
		}
	}
	call := TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWorker(context.Background(), call, team.WorkerMessage{Message: "Go."}); err != nil {
		t.Fatal(err)
	}
	h.engine.onSend = nil
	if _, err := h.server.deliverAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if _, sent := h.engine.sent[workerMessagePrefix+call.key()+":report:1"]; !sent {
		t.Fatalf("the stranded message did not go again: %v", h.engine.order)
	}
}

// The message that starts a worker is kept for it under workerStartMu, so
// it is first in the worker's queue (Codex review of #29, round 3).
func TestTheBriefIsFirstInTheWorkersQueue(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Brief."}); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.Thread(ctx, thread.ID)
	h.engine.mu.Lock()
	delete(h.engine.watching, got.WorkerSession)
	h.engine.mu.Unlock()
	watchedUnderLock := false
	h.engine.onLive = func(live proto.Live) {
		if live.SessionID == got.WorkerSession {
			if h.server.workerStartMu.TryLock() {
				h.server.workerStartMu.Unlock()
			} else {
				watchedUnderLock = true
			}
		}
	}
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 8, CallID: "w2"}, team.WorkerMessage{Message: "Fix."}); err != nil {
		t.Fatal(err)
	}
	if !watchedUnderLock {
		t.Fatal("the message was kept outside workerStartMu")
	}
}

// A pass moves the worker's cursor on to the last run that ended, so the
// next keeps no reply of the runs before again (Codex review of #29,
// round 3).
func TestAWorkersCursorMovesOn(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "First."},
		{ID: 5, Kind: proto.EntryUser, Text: "Fix."},
		{ID: 6, Kind: proto.EntryAssistant, Text: "Second."},
	}}
	h.engine.mu.Unlock()
	endRun(t, h, worker)
	got, err := h.store.Thread(ctx, thread.ID)
	if err != nil || got.WorkerCursor != 5 {
		t.Fatalf("the cursor %d, %v", got.WorkerCursor, err)
	}
	if replies, cursor := workerReplies(h.engine.transcripts[worker], proto.Live{}, 5); len(replies) != 0 || cursor != 5 {
		t.Fatalf("after the cursor: %+v, %d", replies, cursor)
	}
	// The next pass reads the transcript from the cursor on (Codex review of
	// #29, round 4).
	endRun(t, h, worker)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if n := len(h.engine.readAfter); n == 0 || h.engine.readAfter[n-1] != 5 {
		t.Fatalf("reads after %v", h.engine.readAfter)
	}
}

// A specialist's session an earlier run made, on a model that now serves
// workers alone, gets no brief: a key would run it (Codex review of #29,
// round 5).
func TestARecoveredSessionOnAKeyGetsNoBrief(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.workerModels = []proto.Model{{ModelRef: deepseekFlash}}
	h.engine.mu.Unlock()
	thread, _, err := h.store.CreateThread(ctx, store.ThreadRow{Key: "old", Title: "old", Agent: "duncan", SupervisorSession: h.super.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.CreateSession(ctx, "thread:"+thread.Owner, SessionSpec{Cwd: h.home, Model: deepseekFlash, Route: "profile"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.startThread(ctx, thread); err == nil || !strings.Contains(err.Error(), "workers alone") {
		t.Fatalf("a recovered session on a key: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for key := range h.engine.texts {
		if strings.HasSuffix(key, ":brief") && strings.Contains(key, thread.Owner) {
			t.Fatal("its brief went")
		}
	}
}

// A worker's brief is kept with it, and goes, though its watch fails: the
// call does not fail with the brief kept, so the specialist sends no task
// twice (Codex review of #29, round 5).
func TestABriefGoesThoughTheWatchFails(t *testing.T) {
	h, thread := workerHarness(t)
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the watch is down")
	h.engine.mu.Unlock()
	call := TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWorker(context.Background(), call, team.WorkerMessage{Message: "Brief."}); err != nil {
		t.Fatalf("the first call: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.texts[workerMessagePrefix+call.key()+":report"] != "Brief." {
		t.Fatalf("the brief did not go: %v", h.engine.order)
	}
}

// A worker whose record a close refused is not watched: no watch outlives
// it (Codex review of #29, round 5).
func TestARefusedWorkerIsNotWatched(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	var made int64
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Worker {
			if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := h.server.TeamWorker(ctx, TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Brief."}); err == nil {
		t.Fatal("a worker started for a thread closed meanwhile")
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	made = h.engine.byKey[workerKey(thread)]
	if made == 0 || h.engine.watching[made] {
		t.Fatalf("the refused worker %d is watched: %v", made, h.engine.watching)
	}
}

// A worker whose first watch failed is watched by the next delivery pass,
// so the end of its run is seen (Codex review of #29, round 6).
func TestAWorkerWhoseWatchFailedIsWatchedAgain(t *testing.T) {
	h, thread := workerHarness(t)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.watchErr = errors.New("the watch is down")
	h.engine.mu.Unlock()
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.watchErr = nil
	watched := h.engine.watching[worker]
	h.engine.mu.Unlock()
	if watched {
		t.Fatal("watched though its watch failed")
	}
	if _, err := h.server.deliverAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !h.engine.watching[worker] {
		t.Fatal("the delivery pass did not watch the worker again")
	}
}

// A run that was aborted or failed after it wrote something says it was
// cut short: its text is no finished reply (Codex review of #29, round 6).
func TestAWorkerRunCutShortSaysSo(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Half the tests pass.", StopReason: "aborted"},
	}}
	h.engine.mu.Unlock()
	endRun(t, h, worker)
	want := []string{"[T1 · worker] The worker's run ended before it finished (aborted). What it had written:\n\nHalf the tests pass."}
	if got := repliesTo(h, thread.Session); !slices.Equal(got, want) {
		t.Fatalf("replies %q, want %q", got, want)
	}
}

// A team_worker call run again after its message was kept answers as the
// first did, though the thread closed since: it is no new call (Codex
// review of #29, round 6).
func TestAReplayedWorkerCallAnswersAsTheFirst(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}
	first, err := h.server.TeamWorker(ctx, call, team.WorkerMessage{Message: "Go."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CloseThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	sent := len(h.engine.order)
	h.engine.mu.Unlock()
	// It answers as the first, the start of the worker on its model
	// included (Codex review of #29, round 7).
	if again, err := h.server.TeamWorker(ctx, call, team.WorkerMessage{Message: "Go."}); err != nil || again != first {
		t.Fatalf("the call run again answered %q, %v; the first %q", again, err, first)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.order) != sent {
		t.Fatalf("the call run again sent %v", h.engine.order[sent:])
	}
}

// A worker starts only from a profile whose tools pocketd has, as any
// start does (Codex review of #29, round 6).
func TestAWorkerChecksItsProfilesTools(t *testing.T) {
	h, thread := workerHarness(t)
	path := filepath.Join(h.profileDir, "vera.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(body), "---\n", "---\ntools: [read, frobnicate]\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWorker(context.Background(), TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}, team.WorkerMessage{Message: "Go."}); err == nil ||
		!strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("a worker with an unknown tool: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, spec := range h.engine.specs {
		if spec.Worker {
			t.Fatalf("a worker started: %+v", spec)
		}
	}
}

// A session an earlier try of the owner's request made, on a model that
// now serves workers alone, is not recorded: a key would run it (Codex
// review of #29, round 6).
func TestARecoveredOwnerSessionOnAKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.workerModels = []proto.Model{{ModelRef: deepseekFlash}}
	h.engine.mu.Unlock()
	device, err := auth.Authenticate(ctx, h.store, "Bearer "+h.token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.CreateSession(ctx, "u:"+device.ID+":r1", SessionSpec{Title: "old", Cwd: h.work, Model: deepseekFlash, Route: "owner"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Cwd: h.work, Model: model})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if rows, _ := h.store.Sessions(ctx); len(rows) != 0 {
		t.Fatalf("recorded %+v", rows)
	}
}

// A worker whose run ended before its watch began replies all the same:
// the brief was visible to a delivery pass before the watch (Codex review
// of #29, round 7).
func TestAWorkerThatEndedBeforeItsWatchReplies(t *testing.T) {
	h, thread := workerHarness(t)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.onSend = func(to int64, key string) {
		if strings.HasPrefix(key, workerMessagePrefix) {
			h.engine.mu.Lock()
			h.engine.transcripts = map[int64][]proto.Entry{to: {
				{ID: 1, Kind: proto.EntryUser, Text: "Go."},
				{ID: 2, Kind: proto.EntryAssistant, Text: "Done before the watch.", StopReason: "stop"},
			}}
			h.engine.mu.Unlock()
		}
	}
	startedWorker(t, h, thread)
	h.engine.onSend = nil
	if _, err := h.server.deliverAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := repliesTo(h, thread.Session), []string{"[T1 · worker] Done before the watch."}; !slices.Equal(got, want) {
		t.Fatalf("replies %q, want %q", got, want)
	}
}

// An entry the protocol does not model after the answer, as PiG's
// compaction, leaves the answer the run's reply (Codex review of #29,
// round 7).
func TestACompactionAfterTheAnswerKeepsTheReply(t *testing.T) {
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Go."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Done.", StopReason: "stop"},
		{ID: 3, Kind: proto.EntryOther, Raw: "compaction"},
	}
	out, cursor := workerReplies(entries, proto.Live{}, 0)
	if len(out) != 1 || out[0].text != "Done." || out[0].entry != 2 || cursor != 1 {
		t.Fatalf("replies %+v, cursor %d", out, cursor)
	}
}

// A run stopped at the model's output limit says it was cut short (Codex
// review of #29, round 7).
func TestAWorkerAnswerAtTheOutputLimitSaysSo(t *testing.T) {
	got := replyText(proto.Entry{Kind: proto.EntryAssistant, Text: "Half of it.", StopReason: "length"})
	if want := "The worker's run ended before it finished (it reached the model's output limit). What it had written:\n\nHalf of it."; got != want {
		t.Fatalf("reply %q, want %q", got, want)
	}
}

// A new message to a worker whose model the service cannot run now is
// refused, as a start is; the call that started it still answers as it
// did (Codex review of #29, round 7).
func TestAWorkersModelIsCheckedForEachNewMessage(t *testing.T) {
	h, thread := workerHarness(t)
	ctx := context.Background()
	first := TeamCall{Session: thread.Session, Task: 7, CallID: "w1"}
	answer, err := h.server.TeamWorker(ctx, first, team.WorkerMessage{Message: "Go."})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.workerModels = nil
	h.engine.mu.Unlock()
	next := TeamCall{Session: thread.Session, Task: 8, CallID: "w2"}
	if _, err := h.server.TeamWorker(ctx, next, team.WorkerMessage{Message: "Fix."}); err == nil || !strings.Contains(err.Error(), "tell the owner") {
		t.Fatalf("a message to a worker on a model gone: %v", err)
	}
	if kept, err := h.store.ReportFiled(ctx, workerMessagePrefix+next.key()); err != nil || kept {
		t.Fatalf("the message was kept: %v, %v", kept, err)
	}
	if again, err := h.server.TeamWorker(ctx, first, team.WorkerMessage{Message: "Go."}); err != nil || again != answer {
		t.Fatalf("the first call run again: %q, %v", again, err)
	}
}
