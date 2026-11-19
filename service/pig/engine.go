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
	"sort"
	"strings"
	"sync"

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
}

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
	models := ai.CreateModels(ai.CreateModelsOptions{Credentials: auth})
	for _, provider := range ai.BuiltinProviders() {
		if provider != nil {
			models.SetProvider(provider)
		}
	}
	return models, nil
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
	e := &Engine{opts: opts, models: models, watches: map[int64]*watcher{}}
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
			return envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: *target.Cwd}), nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("durable harness: %w", err)
	}
	e.harness = h
	for _, dir := range opts.Protected {
		resolved, err := resolvePath(dir)
		if err != nil {
			return nil, fmt.Errorf("protected folder %s: %w", dir, err)
		}
		e.protected = append(e.protected, resolved)
	}
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

// pathTools are the tools whose "path" argument names a file.
var pathTools = map[string]bool{"read": true, "write": true, "edit": true}

// guardPath refuses a path tool's call on a file in a protected folder.
func (e *Engine) guardPath(ctx context.Context, call harness.ToolHookCall, api harness.HookApi) (*string, error) {
	if !pathTools[call.Name] || len(e.protected) == 0 {
		return nil, nil
	}
	path, _ := call.Arguments["path"].(string)
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		conversation, err := e.conversation(ctx, int64(api.ConversationId()))
		if err != nil {
			return nil, err
		}
		agent, err := conversation.Agent(ctx)
		if err != nil {
			return nil, err
		}
		if agent.Cwd == nil {
			return nil, errors.New("the session has no working directory")
		}
		path = filepath.Join(*agent.Cwd, path)
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	for _, dir := range e.protected {
		if within(resolved, dir) {
			message := "This path belongs to pocketd and is not available to agents."
			return &message, nil
		}
	}
	return nil, nil
}

func (e *Engine) beforeTool(ctx context.Context, call harness.ToolHookCall, api harness.HookApi) (*harness.BeforeToolResult, error) {
	if block, err := e.guardPath(ctx, call, api); err != nil {
		return nil, err
	} else if block != nil {
		return &harness.BeforeToolResult{Block: block}, nil
	}
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
		out = append(out, proto.Model{
			ModelRef:  proto.ModelRef{Provider: providerID(m), ID: m.ID},
			Name:      m.DisplayName,
			Reasoning: m.Capabilities.MaxThinking != "",
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

// instructions is the agent's system prompt for a session in cwd.
func instructions(cwd string) string {
	return "You are a coding agent working for the owner in " + cwd + ". " +
		"Use the tools to read and change files and to run commands there. " +
		"Calls that change files or run commands wait for the owner's approval; a denied call returns the owner's reason. " +
		"Answer briefly, and say what you changed."
}

// CreateSession creates a conversation and returns its id, which is the
// session id.
func (e *Engine) CreateSession(ctx context.Context, cwd string, model proto.ModelRef, thinking string) (int64, error) {
	agent := &harness.AgentChange{
		Model:        harness.SetTo(durable.ModelRef{Provider: model.Provider, ModelId: model.ID}),
		Extensions:   harness.SetTo(harness.ExtensionChange{List: []*durable.Extension{e.coding, e.approval}}),
		Cwd:          harness.SetTo(cwd),
		Instructions: harness.SetTo(instructions(cwd)),
	}
	if thinking != "" {
		agent.ThinkingLevel = harness.SetTo(ai.ModelThinkingLevel(thinking))
	}
	conversation, err := e.harness.CreateConversation(ctx, harness.ConversationCreateOptions{Agent: agent})
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
	var entries []proto.Entry
	var cursor durable.Cursor
	for {
		page, err := conversation.Entries(ctx, durable.EntryQuery{}, 200, cursor)
		if err != nil {
			return nil, proto.Live{}, err
		}
		for _, record := range page.Items {
			entries = append(entries, wireEntry(record))
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	watch, err := conversation.Watch(ctx, nil)
	if err != nil {
		return nil, proto.Live{}, err
	}
	live := viewLive(id, watch.Value())
	_, _ = watch.Stop()
	return entries, live, nil
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
	lastID       int64
	lastLive     []byte
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

// advance runs on the watch's own goroutine, one call at a time. The view's
// entries are a bounded window: when it starts past the newest entry already
// reported, the entries in between are read from the store first, so no
// entry is skipped.
func (e *Engine) advance(ctx context.Context, id int64, w *watcher, view harness.ConversationView) {
	records := append([]durable.EntryRecord(nil), view.Entries...)
	if view.Before != nil && int64(*view.Before) > w.lastID {
		missed, err := entriesBetween(ctx, w.conversation, w.lastID, int64(*view.Before))
		if err == nil {
			records = append(records, missed...)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Id < records[j].Id })
	for _, record := range records {
		if int64(record.Id) <= w.lastID {
			continue
		}
		w.lastID = int64(record.Id)
		e.opts.OnEntry(wireEntry(record))
	}
	live := viewLive(id, view)
	if data, err := json.Marshal(live); err == nil && string(data) != string(w.lastLive) {
		w.lastLive = data
		e.opts.OnLive(live)
	}
}

// providerID is the id of a model's provider; a model without one has "".
func providerID(m *ai.Model) string {
	if m.Provider == nil {
		return ""
	}
	return m.Provider.ID()
}

// resolvePath is path made absolute, cleaned, with symbolic links resolved
// as far as the path exists, so a link cannot lead around a protected folder.
func resolvePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest), nil
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
