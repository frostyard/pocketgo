package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
)

var fauxRef = proto.ModelRef{Provider: "faux", ID: "faux-1"}

// approveChanges is Phase 1's approval rule: calls that change files or run
// commands wait. pocketd waits for none (ADR-0012), but the approval
// machinery stays for later gates, and these tests keep exercising it.
func approveChanges(tool string) bool { return tool == "bash" || tool == "write" || tool == "edit" }

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
	return f.openWith(models, approveChanges)
}

// openWith is open with the approval rule needs, its commands unsandboxed.
func (f *fixture) openWith(models *ai.Models, needs func(string) bool) *Engine {
	f.t.Helper()
	e, err := open(context.Background(), Options{
		DataDir:       f.dataDir,
		Protected:     []string{f.dataDir},
		Approver:      f.approve,
		NeedsApproval: needs,
		Unsandboxed:   true,
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
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	// The resumed task asks again, announcing the same approval.
	if again := f.nextApproval(); again.ID != before.ID {
		t.Fatalf("the resumed call asked for %s, not %s", again.ID, before.ID)
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
		toolTurn("read", map[string]any{"path": "file://" + secret}, "r3"), answer("three"),
	))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	for i, text := range []string{"read the secret", "read the link", "read the file URL"} {
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
	id, _ := e.CreateSession(context.Background(), t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
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

func TestCommandsGetOnlyTheAllowedEnvironment(t *testing.T) {
	t.Setenv("POCKETGO_TEST_SECRET", "s3cret-value")
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "echo \"[$POCKETGO_TEST_SECRET][$HOME]\""}, "b1"), answer("done")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if _, err := e.Send(ctx, id, "u:dev:env", "env"); err != nil {
		t.Fatal(err)
	}
	request := f.nextApproval()
	if _, _, err := f.approve.Decide(ctx, "k", request.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, _, _ := e.Transcript(ctx, id)
	var output string
	for _, entry := range entries {
		if entry.ToolResult != nil {
			output = entry.ToolResult.Text
		}
	}
	home, _ := os.UserHomeDir()
	if strings.Contains(output, "s3cret-value") || !strings.Contains(output, "[]["+home+"]") {
		t.Fatalf("command output %q", output)
	}
}

// The built-in catalog's models carry their provider in ProviderMeta, not
// as a Provider value: the list names it anyway, so a model chosen from it
// is one the service can create a session with. The deployed service once
// listed every model with an empty provider, and the desktop refused them.
func TestListedModelsNameTheirProvider(t *testing.T) {
	auth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(auth, []byte(`{"anthropic":{"type":"api_key","key":"sk-ant-stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := ListModels(context.Background(), auth)
	if err != nil || len(models) == 0 {
		t.Fatalf("models of a stored sign-in %+v %v", models, err)
	}
	for _, m := range models {
		if m.Provider != "anthropic" || m.ID == "" {
			t.Fatalf("model %+v: want provider anthropic and an id", m)
		}
	}
}

func TestAnAmbientAPIKeyListsNoModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	models, err := ListModels(context.Background(), filepath.Join(t.TempDir(), "auth.json"))
	if err != nil || len(models) != 0 {
		t.Fatalf("models from environment keys %+v %v", models, err)
	}
}

func TestCreateSessionIsIdempotentByKey(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	first, err := e.CreateSession(ctx, "u:dev:same", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.CreateSession(ctx, "u:dev:same", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil || again != first {
		t.Fatalf("the same key created %d then %d (%v)", first, again, err)
	}
	other, _ := e.CreateSession(ctx, "u:dev:other", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if other == first {
		t.Fatal("another key returned the same conversation")
	}
}

// TestUserEntriesCarryTheirSubmission checks that a user entry names the
// submission that placed it (Entry.SubmissionID), in the transcript and as
// the watch reports it: a message placed at once, and one queued while the
// run waited for an approval and placed when that run ended.
func TestUserEntriesCarryTheirSubmission(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "true"}, "b1"), answer("one"), answer("two")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	first, err := e.Send(ctx, id, "u:dev:first", "first")
	if err != nil {
		t.Fatal(err)
	}
	request := f.nextApproval() // the run waits: the next message queues
	second, err := e.Send(ctx, id, "u:dev:second", "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	check := func(where string, entries []proto.Entry) {
		t.Helper()
		var users []proto.Entry
		for _, entry := range entries {
			if entry.Kind == proto.EntryUser {
				users = append(users, entry)
			} else if entry.SubmissionID != 0 {
				t.Errorf("%s: a %s entry names submission %d", where, entry.Kind, entry.SubmissionID)
			}
		}
		if len(users) != 2 || users[0].Text != "first" || users[0].SubmissionID != first || users[1].Text != "second" || users[1].SubmissionID != second {
			t.Fatalf("%s: user entries %+v, want submissions %d and %d", where, users, first, second)
		}
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	check("transcript", entries)
	f.mu.Lock()
	watched := append([]proto.Entry(nil), f.entries...)
	f.mu.Unlock()
	check("watch", watched)
}

// TestModelsListTheirThinkingLevels checks Model.ThinkingLevels against
// PiG's reading of a model's catalog entry: a model that does not think
// takes only off; one that does takes off to high, and xhigh and max only
// when its level map names them; a level its map sets to nil is gone.
func TestModelsListTheirThinkingLevels(t *testing.T) {
	name := "effort"
	thinking := ai.ModelCapabilities{MaxThinking: ai.ThinkingLevelHigh}
	cases := []struct {
		what  string
		model *ai.Model
		want  string
	}{
		{"no reasoning", &ai.Model{ID: "plain"}, "off"},
		{"reasoning", &ai.Model{ID: "r", Capabilities: thinking}, "off,minimal,low,medium,high"},
		{"xhigh mapped", &ai.Model{ID: "x", Capabilities: thinking, ThinkingLevelMap: ai.ThinkingLevelMap{ai.ThinkingXHigh: &name}}, "off,minimal,low,medium,high,xhigh"},
		{"minimal removed", &ai.Model{ID: "m", Capabilities: thinking, ThinkingLevelMap: ai.ThinkingLevelMap{ai.ThinkingMinimal: nil}}, "off,low,medium,high"},
	}
	for _, c := range cases {
		if got := strings.Join(thinkingLevels(c.model), ","); got != c.want {
			t.Errorf("%s: %s, want %s", c.what, got, c.want)
		}
	}
	faux := ai.NewFauxProvider(ai.FauxConfig{Models: []ai.FauxModelDefinition{{ID: "plain"}, {ID: "thinker", Reasoning: true}}})
	models := ai.CreateModels()
	models.SetProvider(faux.Provider())
	listed, err := available(context.Background(), models)
	if err != nil || len(listed) != 2 {
		t.Fatalf("models %+v %v", listed, err)
	}
	for _, m := range listed {
		switch m.ID {
		case "plain":
			if m.Reasoning || strings.Join(m.ThinkingLevels, ",") != "off" {
				t.Errorf("plain %+v", m)
			}
		case "thinker":
			if !m.Reasoning || strings.Join(m.ThinkingLevels, ",") != "off,minimal,low,medium,high" {
				t.Errorf("thinker %+v", m)
			}
		}
	}
}

// queuedIDs are the submissions a live state holds queued.
func queuedIDs(live proto.Live) []int64 {
	var out []int64
	for _, q := range live.Queued {
		out = append(out, q.SubmissionID)
	}
	return out
}

// TestAQueuedMessageStaysQueuedAfterAFailedRun queues a message behind a
// run that then fails: the session is idle with the run's failure, and the
// message is still queued (Live.Queued), since PiG keeps it for the next
// run. Its entry is not written. The next message's run takes it first.
func TestAQueuedMessageStaysQueuedAfterAFailedRun(t *testing.T) {
	f := newFixture(t)
	failed := ai.FauxStaticStep(ai.FauxResponse{StopReason: "error", ErrorMessage: "the provider is down"})
	faux := ai.NewFauxProvider(ai.FauxConfig{})
	models := ai.CreateModels()
	models.SetProvider(faux.Provider())
	faux.SetResponses([]ai.FauxResponseStep{toolTurn("bash", map[string]any{"command": "true"}, "b1"), failed})
	e := f.open(models)
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:first", "first"); err != nil {
		t.Fatal(err)
	}
	request := f.nextApproval()
	second, err := e.Send(ctx, id, "u:dev:second", "second")
	if err != nil {
		t.Fatal(err)
	}
	_, live, err := e.Transcript(ctx, id)
	if err != nil || !slices.Equal(queuedIDs(live), []int64{second}) || live.Queued[0].Text != "second" {
		t.Fatalf("while the run waits: queued %+v, %v", live.Queued, err)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, live, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if live.Busy || live.Failure == "" || !slices.Equal(queuedIDs(live), []int64{second}) {
		t.Fatalf("after the failed run: live %+v", live)
	}
	for _, entry := range entries {
		if entry.SubmissionID == second {
			t.Fatalf("the queued message has an entry: %+v", entry)
		}
	}
	f.mu.Lock()
	last := f.lives[len(f.lives)-1]
	f.mu.Unlock()
	if !slices.Equal(queuedIDs(last), []int64{second}) {
		t.Fatalf("the watch's last live state queues %v", queuedIDs(last))
	}
	// An abort through the API finds no run to stop: the queue stays
	// (rule 16a), it does not withdraw it.
	client := serveAPI(t, f, e, id)
	if err := client.Abort(ctx, id, proto.Abort{RequestID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, live, err := e.Transcript(ctx, id); err != nil || !slices.Equal(queuedIDs(live), []int64{second}) {
		t.Fatalf("after an abort of the idle session: queued %+v, %v", live.Queued, err)
	}
	faux.SetResponses([]ai.FauxResponseStep{answer("both done")})
	third, err := e.Send(ctx, id, "u:dev:third", "third")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, live, err = e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var placed []int64
	for _, entry := range entries {
		if entry.Kind == proto.EntryUser {
			placed = append(placed, entry.SubmissionID)
		}
	}
	if len(placed) != 3 || placed[1] != second || placed[2] != third || len(live.Queued) != 0 {
		t.Fatalf("after the next message: user entries of submissions %v (want ..., %d, %d), queued %+v", placed, second, third, live.Queued)
	}
}

// inbox is a pi.inbox document of items, as the view holds it.
func inbox(t *testing.T, items ...string) durable.JsonObject {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(`{"items":[`+strings.Join(items, ",")+`]}`), &value); err != nil {
		t.Fatal(err)
	}
	doc, err := durable.ToJsonObject(value)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestALiveStateListsABoundedQueue queues more messages, and longer ones,
// than a live state carries: it lists the oldest, each cut at a character,
// and counts the rest.
func TestALiveStateListsABoundedQueue(t *testing.T) {
	long := strings.Repeat("é", proto.MaxQueuedText) // two bytes each
	var items []string
	for i := range proto.MaxQueuedListed + 3 {
		items = append(items, fmt.Sprintf(`{"id":%d,"mode":"followUp","content":%q}`, i+1, long))
	}
	items = append(items, `{"id":999,"mode":"write","entry":{"kind":"note"}}`)
	queued, more := (&queueCache{}).read(inbox(t, items...))
	if len(queued) != proto.MaxQueuedListed || more != 3 || queued[0].SubmissionID != 1 {
		t.Fatalf("listed %d, more %d, first %d", len(queued), more, queued[0].SubmissionID)
	}
	q := queued[0]
	if !q.Cut || len(q.Text) > proto.MaxQueuedText || !utf8.ValidString(q.Text) || !strings.HasPrefix(long, q.Text) {
		t.Fatalf("a long message: cut %v, %d bytes, valid %v", q.Cut, len(q.Text), utf8.ValidString(q.Text))
	}
	data, _ := json.Marshal(proto.Event{Type: proto.EventLive, Live: &proto.Live{Queued: queued, QueuedMore: more}})
	if len(data) > 1<<20 {
		t.Fatalf("a live event of %d bytes", len(data))
	}
	short, more := (&queueCache{}).read(inbox(t, `{"id":5,"mode":"steer","content":[{"type":"text","text":"now"}]}`))
	if len(short) != 1 || short[0].Text != "now" || short[0].Cut || more != 0 {
		t.Fatalf("a short queue: %+v %d", short, more)
	}
	if none, more := (&queueCache{}).read(nil); none != nil || more != 0 {
		t.Fatalf("no inbox: %+v %d", none, more)
	}
}

// TestTheQueueIsReadOncePerSubmission reads the queue of three live
// states: the same submissions give the same slice, shared by both live
// states, and a new one reads only its own content; the text of a known
// submission is not read again, even if the document changed it.
func TestTheQueueIsReadOncePerSubmission(t *testing.T) {
	var c queueCache
	first, _ := c.read(inbox(t, `{"id":1,"mode":"followUp","content":"one"}`, `{"id":2,"mode":"followUp","content":"two"}`))
	same, _ := c.read(inbox(t, `{"id":1,"mode":"followUp","content":"one"}`, `{"id":2,"mode":"followUp","content":"two"}`))
	if unsafe.SliceData(first) != unsafe.SliceData(same) {
		t.Fatal("an unchanged queue was read again")
	}
	grown, _ := c.read(inbox(t, `{"id":1,"mode":"followUp","content":"not read"}`, `{"id":2,"mode":"followUp","content":"two"}`, `{"id":3,"mode":"steer","content":"three"}`))
	if len(grown) != 3 || grown[0].Text != "one" || grown[2].Text != "three" {
		t.Fatalf("a grown queue: %+v", grown)
	}
	placed, _ := c.read(inbox(t, `{"id":3,"mode":"steer","content":"three"}`))
	if len(placed) != 1 || placed[0].SubmissionID != 3 {
		t.Fatalf("after two were placed: %+v", placed)
	}
}

// flakyStorage fails the scans of submissions while failing is set.
type flakyStorage struct {
	durable.Storage
	mu      sync.Mutex
	failing bool
}

func (s *flakyStorage) ScanSubmissions(ctx context.Context, query durable.SubmissionQuery, limit int, cursor durable.Cursor) (durable.Page[durable.SubmissionRecord, durable.Cursor], error) {
	s.mu.Lock()
	failing := s.failing
	s.mu.Unlock()
	if failing {
		return durable.Page[durable.SubmissionRecord, durable.Cursor]{}, errors.New("the disk is busy")
	}
	return s.Storage.ScanSubmissions(ctx, query, limit, cursor)
}

// TestAFailedReadIsTriedAgainWithoutAnotherCommit fails every read of the
// submissions while a run goes to its end: nothing goes out, not even the
// idle live state. The retry, with no commit after it, reports the
// entries, then the live state.
func TestAFailedReadIsTriedAgainWithoutAnotherCommit(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(answer("done")))
	defer func() { _ = e.Close(context.Background()) }()
	storage := &flakyStorage{Storage: e.storage, failing: true}
	e.storage = storage
	type retry struct {
		view harness.ConversationView
		fn   func()
	}
	retries := make(chan retry, 64)
	e.retry = func(view harness.ConversationView, fn func()) func() { retries <- retry{view, fn}; return func() {} }
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	sent, err := e.Send(ctx, id, "u:dev:x", "hi")
	if err != nil {
		t.Fatal(err)
	}
	// Take the retries as the watch schedules them, up to the one of the
	// run's end: no commit is left after it to report the entries.
	var scheduled []retry
	for len(scheduled) == 0 || viewLive(id, scheduled[len(scheduled)-1].view, &queueCache{}).Busy {
		select {
		case r := <-retries:
			scheduled = append(scheduled, r)
		case <-time.After(20 * time.Second):
			t.Fatal("the watch never had the run's end")
		}
	}
	f.mu.Lock()
	reported, lives := len(f.entries), len(f.lives)
	f.mu.Unlock()
	if reported != 0 || lives != 1 {
		t.Fatalf("while the reads fail: %d entries, %d live states (want only the watch's first)", reported, lives)
	}
	storage.mu.Lock()
	storage.failing = false
	storage.mu.Unlock()
	// Run every retry scheduled, in order: those of older calls stand
	// down, and the latest reports.
	for _, r := range scheduled {
		r.fn()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := kinds(f.entries); got != "user,assistant" || f.entries[0].SubmissionID != sent {
		t.Fatalf("after the retry: entries %s %+v", got, f.entries)
	}
	if final := f.lives[len(f.lives)-1]; final.Busy {
		t.Fatalf("the last live state is still busy: %+v", final)
	}
}

// TestACutPreviewHoldsOnlyItsOwnBytes cuts a long message for the queue:
// the preview is a copy, not a slice that keeps the whole message alive in
// every live event the hub keeps.
func TestACutPreviewHoldsOnlyItsOwnBytes(t *testing.T) {
	long := strings.Repeat("x", proto.MaxMessageSize)
	cut, ok := cutText(long, proto.MaxQueuedText)
	if !ok || len(cut) != proto.MaxQueuedText {
		t.Fatalf("cut %d bytes, %v", len(cut), ok)
	}
	if unsafe.StringData(cut) == unsafe.StringData(long) {
		t.Fatal("the preview shares the whole message's memory")
	}
}

// serveAPI serves the protocol over engine e and the fixture's stores, as
// pocketd serve does, with session id recorded in the work database, and
// returns a client of it.
func serveAPI(t *testing.T, f *fixture, e *Engine, id int64) *proto.Client {
	t.Helper()
	ctx := context.Background()
	hub, err := events.NewHub(64, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(f.store, hub, "test", nil, nil)
	server.SetApprovals(f.approve)
	server.SetEngine(e)
	device, token, err := auth.CreateDevice(ctx, f.store, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateSession(ctx, "k:"+t.Name(), store.SessionRow{ID: id, Title: "t", Cwd: f.work, Provider: fauxRef.Provider, Model: fauxRef.ID, CreatedBy: device.ID}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() { server.Stop(); httpServer.Close() })
	client, err := proto.NewClient(httpServer.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestADetailReadsItsLiveStateAndEntriesAtOneRevision lets the run place a
// queued message between the detail's two reads: the message shows once,
// queued or placed, never in neither (rule 16a).
func TestADetailReadsItsLiveStateAndEntriesAtOneRevision(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "true"}, "b1"), answer("one"), answer("two")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if _, err := e.Send(ctx, id, "u:dev:first", "first"); err != nil {
		t.Fatal(err)
	}
	request := f.nextApproval()
	second, err := e.Send(ctx, id, "u:dev:second", "second")
	if err != nil {
		t.Fatal(err)
	}
	// Between the reads, the run goes on: it places the queued message and
	// ends.
	e.betweenReads = func() {
		e.betweenReads = nil
		if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, true, "laptop", ""); err != nil {
			t.Error(err)
		}
		waitIdle(t, e, id)
	}
	entries, live, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	shown := 0
	for _, entry := range entries {
		if entry.SubmissionID == second {
			shown++
		}
	}
	shown += len(slices.DeleteFunc(queuedIDs(live), func(q int64) bool { return q != second }))
	if shown != 1 {
		t.Fatalf("the queued message shows %d times: entries %s, queued %v", shown, kinds(entries), queuedIDs(live))
	}
}

// A session from a profile gets the profile's identity after pocketd's
// preamble, and exactly the profile's tools (ADR-0013).
func TestASessionFromAProfileHasItsIdentityAndTools(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	vera := &profiles.Profile{ID: "vera", Name: "Vera", Role: profiles.RoleAgent, Identity: "# Vera\n\nShe tidies the example repository.",
		Tools: []string{"read", "bash"}}
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: vera})
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
	if agent.Instructions == nil || !strings.HasPrefix(*agent.Instructions, instructions(f.work)) ||
		!strings.Contains(*agent.Instructions, "You are Vera, the team's agent.") || !strings.HasSuffix(*agent.Instructions, "She tidies the example repository.") {
		t.Errorf("instructions %v", agent.Instructions)
	}
	var tools []string
	for _, tool := range agent.Tools {
		tools = append(tools, tool.Name)
	}
	if !slices.Equal(tools, []string{"read", "bash"}) {
		t.Errorf("tools %q, want read and bash", tools)
	}
	if !slices.Equal(e.Tools(), []string{"read", "write", "edit", "bash"}) {
		t.Errorf("the engine's tools %q", e.Tools())
	}
	if _, err := e.CreateSession(ctx, t.Name()+"-bad", api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "x", Tools: []string{"teleport"}}}); err == nil {
		t.Error("a profile with an unknown tool created a session")
	}
}

// A profile with no tools gets no tools: PiG reads a nil list as no
// change, so the engine passes an empty one.
func TestAProfileWithNoToolsGetsNone(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef,
		Profile: &profiles.Profile{ID: "mute", Name: "Mute", Tools: []string{}}})
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
	if len(agent.Tools) != 0 {
		var names []string
		for _, tool := range agent.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("a profile with no tools got %q", names)
	}
	// The engine keeps what the session was created from, for a retry.
	made, ok, err := e.Created(ctx, t.Name())
	if err != nil || !ok || made.ID != id || made.Profile != "mute" || made.Model != fauxRef || made.Cwd != f.work {
		t.Fatalf("Created %+v, %v, %v", made, ok, err)
	}
}

// A create record from before pocketd kept what a session was created
// from holds only the conversation: Created reads the rest from the
// conversation's own agent, as a session whose owner named its model.
func TestCreatedRecoversALegacyRecord(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	key := t.Name()
	conversation, err := e.harness.CreateConversation(ctx, harness.ConversationCreateOptions{
		Agent: &harness.AgentChange{
			Model:         harness.SetTo(durable.ModelRef{Provider: fauxRef.Provider, ModelId: fauxRef.ID}),
			Cwd:           harness.SetTo(f.work),
			ThinkingLevel: harness.SetTo(ai.ModelThinkingLevel("high")),
		},
		Init: func(tx durable.Tx, id durable.ConversationId) error {
			_, err := durable.TxDoc[created](tx, createdDoc, key, created{Conversation: int64(id)})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	made, ok, err := e.Created(ctx, key)
	if err != nil || !ok || !made.Legacy || made.ID != int64(conversation.Id()) || made.Model != fauxRef || made.Cwd != f.work ||
		made.Thinking != "high" || made.Route != "owner" || made.Profile != "" {
		t.Fatalf("Created %+v, %v, %v", made, ok, err)
	}
}
