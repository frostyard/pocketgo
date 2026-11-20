package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
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
	sent, to := h.engine.texts[call.key()+":worker"], h.engine.to[call.key()+":worker"]
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
	followUp := h.engine.to["team:"+strconv.FormatInt(thread.Session, 10)+":8:w2:worker"]
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
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 2, Kind: proto.EntryAssistant, Text: "Done."}}}
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

// The models the owner is offered leave out a worker-only model
// (ADR-0021).
func TestTheModelsListLeavesOutWorkerOnlyModels(t *testing.T) {
	h := newTeamHarness(t)
	mai := proto.ModelRef{Provider: "github-copilot", ID: "mai-code-1.1-flash"}
	h.engine.mu.Lock()
	h.engine.more = append(h.engine.more, proto.Model{ModelRef: mai})
	h.engine.mu.Unlock()
	models, err := h.client.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.ModelRef == mai {
			t.Fatalf("the owner is offered %s", mai)
		}
	}
}

// The service's start sends a worker's reply that a stop left unsent
// (ADR-0021).
func TestTheStartSendsAWorkersLeftReply(t *testing.T) {
	h, thread := workerHarness(t)
	worker := startedWorker(t, h, thread)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{worker: {{ID: 4, Kind: proto.EntryAssistant, Text: "All done."}}}
	h.engine.mu.Unlock()
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
