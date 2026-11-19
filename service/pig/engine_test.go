package pig

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/store"
)

var fauxRef = proto.ModelRef{Provider: "faux", ID: "faux-1"}

func fauxModels(steps ...ai.FauxResponseStep) *ai.Models {
	faux := ai.NewFauxProvider(ai.FauxConfig{})
	models := ai.CreateModels()
	models.SetProvider(faux.Provider())
	faux.SetResponses(steps)
	return models
}

func answer(text string) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText(text)}})
}

func toolTurn(name string, args map[string]any, id string) ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxToolCall(name, args, &ai.FauxToolCallOptions{ID: id})}, StopReason: "toolUse"})
}

// fixture is an engine over a real work store and approvals manager, as the
// service wires it.
type fixture struct {
	t        *testing.T
	dataDir  string
	work     string
	store    *store.Store
	approve  *approvals.Manager
	requests chan proto.Approval
	mu       sync.Mutex
	entries  []proto.Entry
	lives    []proto.Live
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dataDir: t.TempDir(), work: t.TempDir(), requests: make(chan proto.Approval, 16)}
	s, err := store.Open(context.Background(), filepath.Join(f.dataDir, "work.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.store = s
	f.approve = approvals.New(s, func(a proto.Approval) {
		if a.State == proto.ApprovalPending {
			f.requests <- a
		}
	})
	return f
}

func (f *fixture) open(models *ai.Models) *Engine {
	f.t.Helper()
	e, err := open(context.Background(), Options{
		DataDir:       f.dataDir,
		Protected:     []string{f.dataDir},
		Approver:      f.approve,
		NeedsApproval: approvals.NeedsApproval,
		OnEntry:       func(entry proto.Entry) { f.mu.Lock(); f.entries = append(f.entries, entry); f.mu.Unlock() },
		OnLive:        func(live proto.Live) { f.mu.Lock(); f.lives = append(f.lives, live); f.mu.Unlock() },
	}, models)
	if err != nil {
		f.t.Fatal(err)
	}
	e.Resume()
	return e
}

func (f *fixture) nextApproval() proto.Approval {
	f.t.Helper()
	select {
	case a := <-f.requests:
		return a
	case <-time.After(10 * time.Second):
		f.t.Fatal("no approval was requested")
		return proto.Approval{}
	}
}

func waitIdle(t *testing.T, e *Engine, id int64) {
	t.Helper()
	conversation, err := e.conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conversation.WaitForIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func kinds(entries []proto.Entry) string {
	var out []string
	for _, e := range entries {
		if e.Kind != proto.EntrySystem {
			out = append(out, string(e.Kind))
		}
	}
	return strings.Join(out, ",")
}

func TestApprovedWriteRunsAndTheTranscriptConverts(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn("write", map[string]any{"path": "hello.txt", "content": "hi\n"}, "w1"), answer("Wrote hello.txt.")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, f.work, fauxRef, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:r1", "write hello"); err != nil {
		t.Fatal(err)
	}
	request := f.nextApproval()
	if request.Tool != "write" || request.SessionID != id || !strings.Contains(string(request.Arguments), "hello.txt") {
		t.Fatalf("approval %+v", request)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	if data, err := os.ReadFile(filepath.Join(f.work, "hello.txt")); err != nil || string(data) != "hi\n" {
		t.Fatalf("file %q, %v", data, err)
	}
	entries, live, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(entries); got != "user,assistant,toolResult,assistant" {
		t.Fatalf("transcript kinds %s", got)
	}
	if entries[1].Kind != proto.EntrySystem && (len(entries[1].ToolCalls) == 0 && len(entries[2].ToolCalls) == 0) {
		t.Fatalf("no tool call in %+v", entries)
	}
	last := entries[len(entries)-1]
	if last.Text != "Wrote hello.txt." || last.Model == nil || *last.Model != fauxRef || last.Usage == nil {
		t.Fatalf("answer entry %+v", last)
	}
	if live.Busy {
		t.Fatalf("live after the run %+v", live)
	}
	usage, err := e.Usage(ctx, id)
	if err != nil || usage.Output == 0 {
		t.Fatalf("usage %+v, %v", usage, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := kinds(f.entries); got != "user,assistant,toolResult,assistant" {
		t.Fatalf("watched entries %s", got)
	}
	sawBusy := false
	for _, l := range f.lives {
		sawBusy = sawBusy || l.Busy
	}
	if !sawBusy || f.lives[len(f.lives)-1].Busy {
		t.Fatalf("live changes %+v", f.lives)
	}
}

func TestDeniedCallIsBlockedWithTheReason(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "touch x"}, "b1"), answer("Understood.")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, f.work, fauxRef, "")
	if _, err := e.Send(ctx, id, "u:dev:r1", "touch x"); err != nil {
		t.Fatal(err)
	}
	request := f.nextApproval()
	if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, false, "laptop", "not in this repo"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	if _, err := os.Stat(filepath.Join(f.work, "x")); !os.IsNotExist(err) {
		t.Fatalf("the denied command ran: %v", err)
	}
	entries, _, _ := e.Transcript(ctx, id)
	var result *proto.ToolResult
	for _, entry := range entries {
		if entry.ToolResult != nil {
			result = entry.ToolResult
		}
	}
	if result == nil || !result.IsError || !strings.Contains(result.Text, "not in this repo") {
		t.Fatalf("tool result %+v", result)
	}
}

func TestReadNeedsNoApproval(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.work, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := f.open(fauxModels(toolTurn("read", map[string]any{"path": "a.txt"}, "r1"), answer("It says alpha.")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, f.work, fauxRef, "")
	if _, err := e.Send(ctx, id, "u:dev:r1", "read a.txt"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	select {
	case a := <-f.requests:
		t.Fatalf("read asked for approval: %+v", a)
	default:
	}
	entries, _, _ := e.Transcript(ctx, id)
	if got := kinds(entries); got != "user,assistant,toolResult,assistant" {
		t.Fatalf("kinds %s", got)
	}
}

func TestSendIsIdempotentPerKey(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(answer("one")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, f.work, fauxRef, "")
	first, err := e.Send(ctx, id, "u:dev:same", "hi")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Send(ctx, id, "u:dev:same", "hi")
	if err != nil || second != first {
		t.Fatalf("the same key submitted twice: %d, %d, %v", first, second, err)
	}
	waitIdle(t, e, id)
	entries, _, _ := e.Transcript(ctx, id)
	if got := kinds(entries); got != "user,assistant" {
		t.Fatalf("kinds %s", got)
	}
}

func TestUnknownSession(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	if _, err := e.Send(context.Background(), 999, "k", "hi"); err != ErrNoSession {
		t.Fatalf("Send to an unknown session: %v", err)
	}
}

// The Phase 1 promise: a run that waits for an approval when the service
// stops resumes after a restart, asks for the same approval, and finishes
// once the owner decides, without repeating the tool call.
func TestAWaitingApprovalSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.open(fauxModels(toolTurn("write", map[string]any{"path": "after.txt", "content": "resumed\n"}, "w1")))
	id, err := e.CreateSession(ctx, f.work, fauxRef, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:r1", "write after.txt"); err != nil {
		t.Fatal(err)
	}
	before := f.nextApproval()
	closeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := e.Close(closeCtx); err != nil {
		t.Fatalf("close while waiting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "after.txt")); !os.IsNotExist(err) {
		t.Fatalf("the call ran before its approval: %v", err)
	}

	reopened := f.open(fauxModels(answer("Done after the restart.")))
	defer func() { _ = reopened.Close(context.Background()) }()
	if err := reopened.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	pending, err := f.approve.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != before.ID {
		t.Fatalf("pending after the restart %+v, %v", pending, err)
	}
	// The resumed task asks again: wait until its hook waits, then decide.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, live, _ := reopened.Transcript(ctx, id); live.Busy && len(live.Tools) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not resume")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+before.ID, before.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, reopened, id)
	if data, err := os.ReadFile(filepath.Join(f.work, "after.txt")); err != nil || string(data) != "resumed\n" {
		t.Fatalf("file after the restart %q, %v", data, err)
	}
	entries, _, _ := reopened.Transcript(ctx, id)
	if got := kinds(entries); got != "user,assistant,toolResult,assistant" {
		t.Fatalf("transcript after the restart %s", got)
	}
	calls := 0
	for _, entry := range entries {
		calls += len(entry.ToolCalls)
	}
	if calls != 1 || entries[len(entries)-1].Text != "Done after the restart." {
		t.Fatalf("tool calls %d, entries %+v", calls, entries)
	}
}

func TestPathToolsCannotReachTheDataFolder(t *testing.T) {
	f := newFixture(t)
	secret := filepath.Join(f.dataDir, "auth.json")
	if err := os.WriteFile(secret, []byte(`{"anthropic":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.work, "innocent.json")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	e := f.open(fauxModels(
		toolTurn("read", map[string]any{"path": secret}, "r1"), answer("one"),
		toolTurn("read", map[string]any{"path": "innocent.json"}, "r2"), answer("two"),
	))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, f.work, fauxRef, "")
	for i, text := range []string{"read the secret", "read the link"} {
		if _, err := e.Send(ctx, id, "u:dev:"+text, text); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, e, id)
		entries, _, _ := e.Transcript(ctx, id)
		var results []*proto.ToolResult
		for _, entry := range entries {
			if entry.ToolResult != nil {
				results = append(results, entry.ToolResult)
			}
		}
		if len(results) != i+1 {
			t.Fatalf("results %+v", results)
		}
		got := results[i]
		if !got.IsError || strings.Contains(got.Text, "secret") || !strings.Contains(got.Text, "belongs to pocketd") {
			t.Fatalf("call %d result %+v", i, got)
		}
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/data", "/data", true},
		{"/data/auth.json", "/data", true},
		{"/database", "/data", false},
		{"/other/../data/x", "/data", true},
		{"/", "/data", false},
	} {
		if got := within(filepath.Clean(c.path), c.dir); got != c.want {
			t.Errorf("within(%q, %q) = %v", c.path, c.dir, got)
		}
	}
}

func TestWatchReportsTheInitialLiveState(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	id, _ := e.CreateSession(context.Background(), f.work, fauxRef, "")
	if err := e.Watch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lives) != 1 || f.lives[0].SessionID != id || f.lives[0].Busy {
		t.Fatalf("lives %+v", f.lives)
	}
}

func TestAdvanceBackfillsEntriesOutsideTheWindow(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(answer("a"), answer("b"), answer("c")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, f.work, fauxRef, "")
	for _, text := range []string{"one", "two", "three"} {
		if _, err := e.Send(ctx, id, "u:dev:"+text, text); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, e, id)
	}
	conversation, _ := e.conversation(ctx, id)
	page, err := conversation.Entries(ctx, durable.EntryQuery{}, 100, nil)
	if err != nil || len(page.Items) < 4 {
		t.Fatalf("entries %d %v", len(page.Items), err)
	}
	records := append([]durable.EntryRecord(nil), page.Items...)
	sort.Slice(records, func(i, j int) bool { return records[i].Id < records[j].Id })
	// The view holds only the newest entry; its window starts after the
	// first; the watcher last reported the first.
	newest := records[len(records)-1]
	before := records[len(records)-2].Id
	w := &watcher{conversation: conversation, lastID: int64(records[0].Id)}
	var got []int64
	e.opts.OnEntry = func(entry proto.Entry) { got = append(got, entry.ID) }
	e.advance(ctx, id, w, harness.ConversationView{Entries: []durable.EntryRecord{newest}, Before: &before})
	if len(got) != len(records)-1 || got[0] != int64(records[1].Id) || got[len(got)-1] != int64(newest.Id) {
		t.Fatalf("reported %v, want ids %d..%d", got, records[1].Id, newest.Id)
	}
}

func TestTaskEnded(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	if ended, err := e.TaskEnded(ctx, 999999); err != nil || !ended {
		t.Fatalf("a missing task: %v %v", ended, err)
	}
	id, _ := e.CreateSession(ctx, f.work, fauxRef, "")
	if _, err := e.Send(ctx, id, "u:dev:x", "x"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, _, _ := e.Transcript(ctx, id)
	conversation, _ := e.conversation(ctx, id)
	page, _ := conversation.Entries(ctx, durable.EntryQuery{}, 10, nil)
	var task int64
	for _, record := range page.Items {
		if record.ByTaskId != nil {
			task = int64(*record.ByTaskId)
		}
	}
	if task == 0 {
		t.Skipf("no entry names its task in %d entries", len(entries))
	}
	if ended, err := e.TaskEnded(ctx, task); err != nil || !ended {
		t.Fatalf("a finished task: %v %v", ended, err)
	}
}

func TestRunFailureCarriesItsDetail(t *testing.T) {
	live := wireLive(3, []byte(`{"runFailure":{"taskId":1,"inputs":[],"reason":"model_error","detail":"401 unauthorized"}}`))
	if live.Failure != "model_error: 401 unauthorized" {
		t.Fatalf("failure %q", live.Failure)
	}
	if live := wireLive(3, []byte(`{"runFailure":{"taskId":1,"inputs":[],"reason":"interrupted"}}`)); live.Failure != "interrupted" {
		t.Fatalf("failure without detail %q", live.Failure)
	}
}

func TestListModelsLeavesTheDurableStoreAlone(t *testing.T) {
	dir := t.TempDir()
	models, err := ListModels(context.Background(), filepath.Join(dir, "auth.json"))
	if err != nil || len(models) != 0 {
		t.Fatalf("models without sign-ins %+v %v", models, err)
	}
	if _, err := os.Stat(filepath.Join(dir, DurableFile)); !os.IsNotExist(err) {
		t.Fatalf("ListModels opened the durable store: %v", err)
	}
}
