// Package pig is the only package that imports PiG (ADR-0004; depguard in
// .golangci.yml enforces it). It runs agents on PiG's durable harness and
// speaks proto types to the rest of the service, so a PiG API change lands
// here and nowhere else.
package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/MichaelKinsy/PiG/durable/harness"
	sqlitenode "github.com/MichaelKinsy/PiG/durable/storage/sqlite/node"
	"github.com/MichaelKinsy/PiG/durable/tools"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
)

// DurableFile is the name of the Durable store in the data folder.
const DurableFile = "durable.sqlite"

// Approver decides tool calls that need the owner.
type Approver interface {
	Await(ctx context.Context, call approvals.Call) (approvals.Decision, error)
}

// Options configure an Engine.
type Options struct {
	// DataDir holds DurableFile.
	DataDir string
	// AuthPath is the auth.json of the subscription sign-ins (Pi's format).
	AuthPath string
	// Approver decides the calls NeedsApproval selects.
	Approver      Approver
	NeedsApproval func(tool string) bool
	// OnEntry and OnLive receive every written entry and every change of a
	// watched session's live state, and OnLive a watched session's live state
	// when the watch starts. They must not block.
	OnEntry func(proto.Entry)
	OnLive  func(proto.Live)
	// Protected are folders whose files read, write and edit refuse, whatever
	// the session's working directory: the data folder, which holds the
	// sign-ins. Bash can still reach them; every bash call waits for the owner.
	Protected []string
}

// Engine runs sessions on a durable harness. It is safe for concurrent use.
type Engine struct {
	opts      Options
	harness   harness.Harness
	models    *ai.Models
	coding    *durable.Extension
	approval  *durable.Extension
	mu        sync.Mutex
	watches   map[int64]*watcher
	closeOnce sync.Once
	protected []string
	commands  map[string]string

	// storage is the durable store the harness runs on. The engine reads
	// only submissions from it, which the harness has no read for.
	storage durable.Storage
	// retry runs f once, later, unless cancel is called first: a watch
	// that failed to read view tries again (advance). Tests replace it,
	// and look at the view.
	retry func(view harness.ConversationView, f func()) (cancel func())
}

// retryDelay is how long a watch waits before it reads a failed update
// again.
const retryDelay = time.Second

// Open opens the durable store in opts.DataDir and signs in from
// opts.AuthPath. No task runs until Resume: wire the callbacks' receivers
// first.
func Open(ctx context.Context, opts Options) (*Engine, error) {
	models, err := signedIn(opts.AuthPath)
	if err != nil {
		return nil, err
	}
	return open(ctx, opts, models)
}

// signedIn is the model catalog of every built-in provider, with the
// credentials of authPath.
func signedIn(authPath string) (*ai.Models, error) {
	auth, err := ai.NewAuthStorage(authPath)
	if err != nil {
		return nil, fmt.Errorf("sign-ins %s: %w", authPath, err)
	}
	models := ai.CreateModels(ai.CreateModelsOptions{Credentials: auth, AuthContext: storedOnly()})
	for _, provider := range ai.BuiltinProviders() {
		if provider != nil {
			models.SetProvider(provider)
		}
	}
	return models, nil
}

// storedOnly is an auth context with no environment and no credential
// files: the stored sign-ins are the only credentials, so an API key in the
// service's environment can neither list a model nor pay for a session.
func storedOnly() *ai.AuthContext {
	return &ai.AuthContext{
		Env:        func(string) (string, bool) { return "", false },
		FileExists: func(string) bool { return false },
	}
}

// ListModels lists the models the sign-ins in authPath can run, without
// opening the durable store.
func ListModels(ctx context.Context, authPath string) ([]proto.Model, error) {
	models, err := signedIn(authPath)
	if err != nil {
		return nil, err
	}
	return available(ctx, models)
}

func open(ctx context.Context, opts Options, models *ai.Models) (*Engine, error) {
	if opts.NeedsApproval == nil || opts.Approver == nil {
		return nil, errors.New("pig: an Approver and NeedsApproval are required")
	}
	if opts.OnEntry == nil {
		opts.OnEntry = func(proto.Entry) {}
	}
	if opts.OnLive == nil {
		opts.OnLive = func(proto.Live) {}
	}
	e := &Engine{opts: opts, models: models, watches: map[int64]*watcher{}, commands: allowedEnv(),
		retry: func(_ harness.ConversationView, f func()) func() {
			t := time.AfterFunc(retryDelay, f)
			return func() { t.Stop() }
		}}
	for _, dir := range opts.Protected {
		resolved, err := resolvePath(dir)
		if err != nil {
			return nil, fmt.Errorf("protected folder %s: %w", dir, err)
		}
		e.protected = append(e.protected, resolved)
	}
	e.coding = tools.CreateCodingTools(nil)
	e.approval = &durable.Extension{
		Name:  "pocketgo-approval",
		Hooks: []durable.HookRegistration{harness.Hook(harness.ToolTask, &harness.ToolHooks{BeforeTool: e.beforeTool})},
	}
	registry := harness.CreateRegistry()
	if err := registry.Install(e.coding); err != nil {
		return nil, err
	}
	if err := registry.Install(e.approval); err != nil {
		return nil, err
	}
	store, err := sqlitenode.OpenNodeSqliteStorage(filepath.Join(opts.DataDir, DurableFile), sqlitenode.NodeSqliteStorageOptions{})
	if err != nil {
		return nil, fmt.Errorf("durable store: %w", err)
	}
	h, err := harness.OpenHarness(ctx, store, harness.HarnessOptions{
		Models:   models,
		Registry: registry,
		Env: func(ctx context.Context, target harness.EnvTarget) (env.ExecutionEnv, error) {
			if target.Cwd == nil || *target.Cwd == "" {
				return nil, errors.New("the session has no working directory")
			}
			local := envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: *target.Cwd})
			return &guardedEnv{ExecutionEnv: local, protected: e.protected, commands: e.commands}, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("durable harness: %w", err)
	}
	e.harness, e.storage = h, store
	return e, nil
}

// Resume lets tasks run, the ones that were running when the service stopped
// first. Call it once the callbacks' receivers are wired.
func (e *Engine) Resume() { e.harness.Resume() }

// TaskEnded reports whether a task is terminal or gone.
func (e *Engine) TaskEnded(ctx context.Context, id int64) (bool, error) {
	record, err := e.harness.GetTask(ctx, durable.TaskId(id))
	if err != nil {
		return false, err
	}
	return record == nil || record.State.Status == durable.TaskTerminal, nil
}

// approvalMemo is the durable memo that pins a call's decision inside its
// task, so a replay of the task reuses it.
func approvalMemo(callID string) string { return "pocketgo/approval/" + callID }

type memoDecision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

func (e *Engine) beforeTool(ctx context.Context, call harness.ToolHookCall, api harness.HookApi) (*harness.BeforeToolResult, error) {
	if !e.opts.NeedsApproval(call.Name) {
		return nil, nil
	}
	var decision approvals.Decision
	if value, ok, err := api.Memo(ctx, approvalMemo(call.ID)); err != nil {
		return nil, err
	} else if ok {
		var memo memoDecision
		data, _ := json.Marshal(value)
		if err := json.Unmarshal(data, &memo); err != nil {
			return nil, fmt.Errorf("approval memo: %w", err)
		}
		decision = approvals.Decision{Allow: memo.Allow, Reason: memo.Reason}
	} else {
		arguments, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, err
		}
		decision, err = e.opts.Approver.Await(ctx, approvals.Call{
			SessionID: int64(api.ConversationId()), TaskID: int64(api.TaskId()),
			CallID: call.ID, Tool: call.Name, Arguments: arguments,
		})
		if err != nil {
			return nil, err
		}
		if _, err := api.MemoCandidate(ctx, approvalMemo(call.ID), map[string]any{"allow": decision.Allow, "reason": decision.Reason}); err != nil {
			return nil, err
		}
	}
	if decision.Allow {
		return nil, nil
	}
	message := "The owner denied this call."
	if decision.Reason != "" {
		message += " Reason: " + decision.Reason
	}
	return &harness.BeforeToolResult{Block: &message}, nil
}

// Close stops every watch and closes the harness. Running tasks stop where
// they are and resume at the next Open.
func (e *Engine) Close(ctx context.Context) error {
	var err error
	e.closeOnce.Do(func() {
		e.mu.Lock()
		for _, w := range e.watches {
			w.stop()
			_, _ = w.handle.Stop()
		}
		e.watches = map[int64]*watcher{}
		e.mu.Unlock()
		err = e.harness.Close(ctx)
	})
	return err
}

// Models lists the models whose provider has a credential, by provider and id.
func (e *Engine) Models(ctx context.Context) ([]proto.Model, error) {
	return available(ctx, e.models)
}

func available(ctx context.Context, models *ai.Models) ([]proto.Model, error) {
	list, err := models.GetAvailable(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]proto.Model, 0, len(list))
	for _, m := range list {
		levels := thinkingLevels(m)
		out = append(out, proto.Model{
			ModelRef:       proto.ModelRef{Provider: providerID(m), ID: m.ID},
			Name:           m.DisplayName,
			Reasoning:      slices.ContainsFunc(levels, func(level string) bool { return level != string(ai.ThinkingOff) }),
			ThinkingLevels: levels,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// thinkingLevels are the levels a session on m takes, lowest first, as PiG
// reads them from the model's catalog entry (its reasoning flag, maximum
// level and level map). A provider clamps any other level to one of these,
// so the service refuses the others (protocol rule 8). Never empty: a model
// that does not think takes "off".
func thinkingLevels(m *ai.Model) []string {
	supported := ai.GetSupportedThinkingLevels(m)
	out := make([]string, 0, len(supported))
	for _, level := range supported {
		out = append(out, string(level))
	}
	if len(out) == 0 {
		out = append(out, string(ai.ThinkingOff))
	}
	return out
}

// instructions is the agent's system prompt for a session in cwd.
func instructions(cwd string) string {
	return "You are a coding agent working for the owner in " + cwd + ". " +
		"Use the tools to read and change files and to run commands there. " +
		"Calls that change files or run commands wait for the owner's approval; a denied call returns the owner's reason. " +
		"Answer briefly, and say what you changed."
}

// created records, in the commit that creates a conversation, the request
// key that created it, so a retry finds the conversation instead of creating
// a second one, even when the work database never recorded it.
type created struct {
	Conversation int64 `json:"conversation"`
}

var createdDoc = durable.DefineDocFamily(durable.DocFamilyDefinition[created, created]{
	Family: true, Kind: "pocketgo.created", Version: 1,
	DocumentSemantics: durable.DocumentSemantics{Scope: durable.ScopeSession},
	Initial:           func(seed created) created { return seed },
	CheckpointWhen:    func(created, []durable.Op, durable.CheckpointInfo) bool { return false },
})

// CreateSession creates a conversation for request key and returns its id,
// which is the session id. The same key returns the same conversation.
func (e *Engine) CreateSession(ctx context.Context, key, cwd string, model proto.ModelRef, thinking string) (int64, error) {
	if found, err := durable.Snapshot[created](ctx, e.harness, createdDoc, key); err != nil {
		return 0, err
	} else if found != nil {
		return found.Conversation, nil
	}
	agent := &harness.AgentChange{
		Model:        harness.SetTo(durable.ModelRef{Provider: model.Provider, ModelId: model.ID}),
		Extensions:   harness.SetTo(harness.ExtensionChange{List: []*durable.Extension{e.coding, e.approval}}),
		Cwd:          harness.SetTo(cwd),
		Instructions: harness.SetTo(instructions(cwd)),
	}
	if thinking != "" {
		agent.ThinkingLevel = harness.SetTo(ai.ModelThinkingLevel(thinking))
	}
	conversation, err := e.harness.CreateConversation(ctx, harness.ConversationCreateOptions{
		Agent: agent,
		Init: func(tx durable.Tx, id durable.ConversationId) error {
			_, err := durable.TxDoc[created](tx, createdDoc, key, created{Conversation: int64(id)})
			return err
		},
	})
	if err != nil {
		return 0, err
	}
	return int64(conversation.Id()), nil
}

// ErrNoSession is returned for a session id with no conversation.
var ErrNoSession = errors.New("no such session")

func (e *Engine) conversation(ctx context.Context, id int64) (harness.Conversation, error) {
	conversation, err := e.harness.Conversation(ctx, durable.ConversationId(id))
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrNoSession
	}
	return conversation, nil
}

// Send submits the owner's message. key makes the submission idempotent: the
// same key returns the same submission.
func (e *Engine) Send(ctx context.Context, id int64, key, text string) (int64, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return 0, err
	}
	submission, err := conversation.Submit(ctx, durable.SubmissionDraft{RequestId: &key, Type: durable.SubmissionTypeInput, Content: ai.UserText(text)})
	if err != nil {
		return 0, err
	}
	return int64(submission.Id()), nil
}

// Abort stops the session's run.
func (e *Engine) Abort(ctx context.Context, id int64) error {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return err
	}
	return conversation.Abort(ctx, nil)
}

// Transcript returns every entry of the session, by ascending id, and its
// live state.
func (e *Engine) Transcript(ctx context.Context, id int64) ([]proto.Entry, proto.Live, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return nil, proto.Live{}, err
	}
	// The live state and the entries are of one revision: the view's, read
	// first, and its entries through the newest one it has. Otherwise a run
	// that placed a queued message between the two reads would leave it in
	// neither (protocol rule 16a).
	watch, err := conversation.Watch(ctx, nil)
	if err != nil {
		return nil, proto.Live{}, err
	}
	view := watch.Value()
	_, _ = watch.Stop()
	live := viewLive(id, view)
	var newest int64
	for _, record := range view.Entries {
		newest = max(newest, int64(record.Id))
	}
	if newest == 0 {
		return nil, live, nil
	}
	records, err := entriesBetween(ctx, conversation, 0, newest)
	if err != nil {
		return nil, proto.Live{}, err
	}
	entries, err := e.wireEntries(ctx, id, records)
	if err != nil {
		return nil, proto.Live{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, live, nil
}

// wireEntries converts records of session id, and gives each user entry
// the submission that placed it.
func (e *Engine) wireEntries(ctx context.Context, id int64, records []durable.EntryRecord) ([]proto.Entry, error) {
	users := map[int64]bool{}
	for _, record := range records {
		if record.Kind == durable.UserEntry.Kind {
			users[int64(record.Id)] = true
		}
	}
	placed, err := e.placedBy(ctx, id, users)
	if err != nil {
		return nil, err
	}
	out := make([]proto.Entry, 0, len(records))
	for _, record := range records {
		entry := wireEntry(record)
		entry.SubmissionID = placed[entry.ID]
		out = append(out, entry)
	}
	return out, nil
}

// placedBy finds the submissions that placed the entries of session id in
// want, by entry id: a placed input records its entry, which the harness
// writes in the same commit. It reads the session's submissions newest
// first and stops once it found every entry, so a new message costs a page.
func (e *Engine) placedBy(ctx context.Context, id int64, want map[int64]bool) (map[int64]int64, error) {
	found := map[int64]int64{}
	if len(want) == 0 {
		return found, nil
	}
	conversation, order := durable.ConversationId(id), durable.ScanDescending
	query := durable.SubmissionQuery{ConversationId: &conversation, Order: &order}
	var cursor durable.Cursor
	for {
		page, err := e.storage.ScanSubmissions(ctx, query, 200, cursor)
		if err != nil {
			return nil, fmt.Errorf("submissions of session %d: %w", id, err)
		}
		for _, record := range page.Items {
			if record.Type == durable.SubmissionTypeInput && record.Entry != nil && want[int64(*record.Entry)] {
				found[int64(*record.Entry)] = int64(record.Id)
			}
		}
		if len(found) == len(want) || page.Next == nil {
			return found, nil
		}
		cursor = *page.Next
	}
}

// entriesBetween reads the entries with after < id <= through.
func entriesBetween(ctx context.Context, conversation harness.Conversation, after, through int64) ([]durable.EntryRecord, error) {
	minID, maxID := durable.EntryId(after+1), durable.EntryId(through)
	var out []durable.EntryRecord
	var cursor durable.Cursor
	for {
		page, err := conversation.Entries(ctx, durable.EntryQuery{MinEntryId: &minID, MaxEntryId: &maxID}, 200, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Next == nil {
			return out, nil
		}
		cursor = *page.Next
	}
}

// RunIdentity names the session's current run by its first entry; busy is
// false when no run is in progress.
func (e *Engine) RunIdentity(ctx context.Context, id int64) (first int64, busy bool, err error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return 0, false, err
	}
	watch, err := conversation.Watch(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _, _ = watch.Stop() }()
	value, ok := watch.Value().Docs.Get("pi.live")
	if !ok {
		return 0, false, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return 0, false, err
	}
	var live struct {
		Run *struct {
			First *int64 `json:"first"`
		} `json:"run"`
	}
	if err := json.Unmarshal(data, &live); err != nil || live.Run == nil {
		return 0, false, err
	}
	if live.Run.First != nil {
		first = *live.Run.First
	}
	return first, true, nil
}

// WaitIdle returns when the session has no run and no live task.
func (e *Engine) WaitIdle(ctx context.Context, id int64) error {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return err
	}
	return conversation.WaitForIdle(ctx)
}

// Usage sums the session's token and cost ledger.
func (e *Engine) Usage(ctx context.Context, id int64) (proto.Usage, error) {
	state, err := durable.Snapshot[harness.UsageState](ctx, e.harness, harness.UsageDoc, durable.ConversationId(id))
	if err != nil {
		return proto.Usage{}, err
	}
	return wireUsageState(state), nil
}

type watcher struct {
	handle       harness.ConversationWatch
	conversation harness.Conversation

	// mu orders the watch's calls and their retries (advance).
	mu       sync.Mutex
	lastID   int64
	lastLive []byte
	calls    uint64 // the watch calls so far; a retry of an older one stands down
	cancel   func() // the retry waiting, or nil
	stopped  bool
}

// stop ends the watcher's retries, as the engine closes.
func (w *watcher) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
}

// Watch follows a session and reports its live state now, then its new
// entries and live changes, to OnEntry and OnLive until Close; ctx bounds
// only the start. Watching a watched session does nothing.
func (e *Engine) Watch(ctx context.Context, id int64) error {
	initial, err := e.watch(ctx, id)
	if err != nil || initial == nil {
		return err
	}
	e.opts.OnLive(*initial)
	return nil
}

func (e *Engine) watch(ctx context.Context, id int64) (*proto.Live, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.watches[id]; ok {
		return nil, nil
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return nil, err
	}
	// The watch lives until Close, not until the caller's request ends.
	handle, err := conversation.Watch(context.WithoutCancel(ctx), nil)
	if err != nil {
		return nil, err
	}
	w := &watcher{handle: handle, conversation: conversation}
	first := handle.Value()
	for _, record := range first.Entries {
		w.lastID = max(w.lastID, int64(record.Id))
	}
	live := viewLive(id, first)
	w.lastLive, _ = json.Marshal(live)
	e.watches[id] = w
	handle.Start(func(ctx context.Context, call durable.WatchCall[harness.ConversationView]) error {
		e.advance(ctx, id, w, call.Value)
		return nil
	})
	return &live, nil
}

// advance runs on the watch's own goroutine, one call at a time: it reports
// the view's new entries, then its live state. The view's entries are a
// bounded window: when it starts past the newest entry already reported,
// the entries in between are read from the store first, so no entry is
// skipped.
//
// When a read fails, nothing goes out: the cursor stays before the new
// entries, and the live state that follows them waits too. The same view is
// read again after retryDelay, unless a newer call comes first, so the
// entries do not wait for another commit, which may never come.
func (e *Engine) advance(ctx context.Context, id int64, w *watcher, view harness.ConversationView) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	e.advanceLocked(ctx, id, w, view, w.calls)
}

func (e *Engine) advanceLocked(ctx context.Context, id int64, w *watcher, view harness.ConversationView, call uint64) {
	if w.stopped || call != w.calls {
		return
	}
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	entries, err := e.newEntries(ctx, id, w, view)
	if err != nil {
		w.cancel = e.retry(view, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			e.advanceLocked(context.Background(), id, w, view, call)
		})
		return
	}
	for _, entry := range entries {
		w.lastID = entry.ID
		e.opts.OnEntry(entry)
	}
	live := viewLive(id, view)
	if data, err := json.Marshal(live); err == nil && string(data) != string(w.lastLive) {
		w.lastLive = data
		e.opts.OnLive(live)
	}
}

// newEntries are the entries of view after the watcher's cursor, in order,
// read in full from the store where the view's window does not reach.
func (e *Engine) newEntries(ctx context.Context, id int64, w *watcher, view harness.ConversationView) ([]proto.Entry, error) {
	records := append([]durable.EntryRecord(nil), view.Entries...)
	if view.Before != nil && int64(*view.Before) > w.lastID {
		missed, err := entriesBetween(ctx, w.conversation, w.lastID, int64(*view.Before))
		if err != nil {
			return nil, err
		}
		records = append(records, missed...)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Id < records[j].Id })
	records = slices.DeleteFunc(records, func(record durable.EntryRecord) bool { return int64(record.Id) <= w.lastID })
	records = slices.CompactFunc(records, func(a, b durable.EntryRecord) bool { return a.Id == b.Id })
	return e.wireEntries(ctx, id, records)
}

// providerID is the id of a model's provider; a model without one has "".
func providerID(m *ai.Model) string {
	if m.Provider == nil {
		return ""
	}
	return m.Provider.ID()
}
