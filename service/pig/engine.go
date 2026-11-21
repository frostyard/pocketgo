// Package pig is the only package that imports PiG (ADR-0004; depguard in
// .golangci.yml enforces it). It runs agents on PiG's durable harness and
// speaks proto types to the rest of the service, so a PiG API change lands
// here and nowhere else.
package pig

import (
	"context"
	"encoding/base64"
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
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// DurableFile is the name of the Durable store in the data folder.
const DurableFile = "durable.sqlite"

// Approver decides tool calls that need the owner.
type Approver interface {
	Await(ctx context.Context, call approvals.Call) (approvals.Decision, error)
	// Asked reports whether a call asked for an approval already.
	Asked(ctx context.Context, taskID int64, callID string) (bool, error)
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
	// sign-ins. Commands cannot reach it either: the Sandbox hides it.
	Protected []string
	// Sandbox runs every command a tool starts (ADR-0012). Open refuses to
	// run without one unless Unsandboxed says so, which only tests do.
	Sandbox     *Sandbox
	Unsandboxed bool
	// GH is the gh program, for tests; "" is one only root can change,
	// in a system folder (trustedGH): the PATH is never searched.
	GH string
	// Git is the git program, for tests; "" is one only root can change,
	// in a system folder (trustedGit): the PATH is never searched.
	Git string
	// Own is what the owner's providers file adds (ADR-0023), checked:
	// their own providers, registered beside PiG's, and models added to
	// PiG's own providers.
	Own profiles.Own
}

// Engine runs sessions on a durable harness. It is safe for concurrent use.
type Engine struct {
	opts    Options
	harness harness.Harness
	models  *ai.Models
	// auth holds the sign-ins models runs on; nil, as in tests, holds no
	// API key.
	auth *ai.AuthStorage
	// notes say what of the owner's models was not added (ADR-0023).
	notes    []string
	coding   *durable.Extension
	approval *durable.Extension
	// teamExt holds the team tools, which call team (ADR-0014).
	teamExt   *durable.Extension
	team      Team
	mu        sync.Mutex
	watches   map[int64]*watcher
	closeOnce sync.Once
	protected []string
	commands  map[string]string

	// settings are the harness's settings (compactionSettings), fixed
	// once it opens.
	settings *harness.HarnessSettings

	// storage is the durable store the harness runs on. The engine reads
	// only submissions from it, which the harness has no read for.
	storage durable.Storage
	// retry runs f once, later, unless cancel is called first: a watch
	// that failed to read view tries again (advance). Tests replace it,
	// and look at the view.
	retry func(view harness.ConversationView, f func()) (cancel func())
	// betweenReads runs in Transcript between its two reads; tests change
	// the conversation there. Nil does nothing.
	betweenReads func()
}

// retryDelay is how long a watch waits before it reads a failed update
// again.
const retryDelay = time.Second

// Open opens the durable store in opts.DataDir and signs in from
// opts.AuthPath. No task runs until Resume: wire the callbacks' receivers
// first.
func Open(ctx context.Context, opts Options) (*Engine, error) {
	models, auth, notes, err := signedIn(opts.AuthPath, opts.Own)
	if err != nil {
		return nil, err
	}
	e, err := open(ctx, opts, models)
	if err != nil {
		return nil, err
	}
	e.auth, e.notes = auth, notes
	return e, nil
}

// signedIn is the model catalog of every built-in provider and of the
// owner's own, with the models they add (ADR-0023), on the credentials of
// authPath as keptCredentials reads them, and those sign-ins. notes say
// what of the owner's models was not added, and why.
func signedIn(authPath string, own profiles.Own) (*ai.Models, *ai.AuthStorage, []string, error) {
	auth, err := ai.NewAuthStorage(authPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sign-ins %s: %w", authPath, err)
	}
	models := ai.CreateModels(ai.CreateModelsOptions{Credentials: keptCredentials{AuthStorage: auth, own: ownIDs(own.Providers)}, AuthContext: storedOnly()})
	for _, provider := range ai.BuiltinProviders() {
		if provider != nil {
			models.SetProvider(provider)
		}
	}
	for _, p := range own.Providers {
		models.SetProvider(ownProvider(p))
	}
	return models, auth, addModels(models, own.Added), nil
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
// opening the durable store, as Engine.Models lists them: a subscription's
// and an API key's alike (ADR-0022).
//
// notes say what of the owner's models was not added, and why.
func ListModels(ctx context.Context, authPath string, own profiles.Own) (list []proto.Model, notes []string, err error) {
	catalog, auth, notes, err := signedIn(authPath, own)
	if err != nil {
		return nil, nil, err
	}
	list, err = available(ctx, catalog, auth, own.Providers)
	return list, notes, err
}

// keyKept says whether provider's stored sign-in is one pocketd runs on: a
// subscription's, or a key of a provider pocketd keeps one for
// (APIKeyProviders). A key written by hand for another provider, as a
// Pi-format sign-ins file may hold, may need an adapter this build does not
// link: it lists nothing (Codex review of #29, round 5, and of #33). A
// sign-in that cannot be read counts as such a key.
func keyKept(auth *ai.AuthStorage, provider string, own []profiles.Provider) bool {
	// An owner's provider resolves its own key (ownKey).
	if _, ok := ownIDs(own)[provider]; ok || auth == nil {
		return true
	}
	cred, ok, err := auth.GetRaw(provider)
	if err != nil {
		return false
	}
	return !ok || cred.Type != ai.CredentialAPIKey || TakesAPIKey(provider)
}

func open(ctx context.Context, opts Options, models *ai.Models) (*Engine, error) {
	if opts.NeedsApproval == nil || opts.Approver == nil {
		return nil, errors.New("pig: an Approver and NeedsApproval are required")
	}
	if opts.Sandbox == nil && !opts.Unsandboxed {
		return nil, errors.New("pig: commands run in a Sandbox (ADR-0012); only a test sets Unsandboxed")
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
	protected := slices.Clone(opts.Protected)
	if opts.Sandbox != nil {
		// The file tools refuse what the sandbox protects from commands.
		protected = append(protected, opts.Sandbox.Guarded()...)
	}
	for _, dir := range protected {
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
	e.teamExt = e.teamExtension()
	if err := registry.Install(e.teamExt); err != nil {
		return nil, err
	}
	store, err := sqlitenode.OpenNodeSqliteStorage(filepath.Join(opts.DataDir, DurableFile), sqlitenode.NodeSqliteStorageOptions{})
	if err != nil {
		return nil, fmt.Errorf("durable store: %w", err)
	}
	// Each model's summary has room (summaryReserve), for every session;
	// the catalog is fixed once the engine opens.
	e.settings = compactionSettings(models)
	h, err := harness.OpenHarness(ctx, store, harness.HarnessOptions{
		Models:   models,
		Registry: registry,
		Settings: func() *harness.HarnessSettings { return e.settings },
		Env: func(ctx context.Context, target harness.EnvTarget) (env.ExecutionEnv, error) {
			if target.Cwd == nil || *target.Cwd == "" {
				return nil, errors.New("the session has no working directory")
			}
			local := envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: *target.Cwd})
			return &guardedEnv{ExecutionEnv: local, protected: e.protected, commands: e.commands, sandbox: opts.Sandbox}, nil
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
	needs := e.opts.NeedsApproval(call.Name)
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
		if !needs {
			// A call that asked under an earlier rule, before an upgrade,
			// still waits for the owner's answer.
			asked, err := e.opts.Approver.Asked(ctx, int64(api.TaskId()), call.ID)
			if err != nil || !asked {
				return nil, err
			}
		}
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

// Models lists the models with a sign-in, by provider and id: a
// subscription's or an API key's (ADR-0022). A session, a profile, a
// cross-review and a thread's worker choose among them.
func (e *Engine) Models(ctx context.Context) ([]proto.Model, error) {
	return available(ctx, e.models, e.auth, e.opts.Own.Providers)
}

// Notes say what of the owner's models was not added at Open, and why
// (ADR-0023).
func (e *Engine) Notes() []string { return slices.Clone(e.notes) }

// available lists the models with a credential pocketd runs on (keyKept).
func available(ctx context.Context, models *ai.Models, auth *ai.AuthStorage, own []profiles.Provider) ([]proto.Model, error) {
	list, err := models.GetAvailable(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]proto.Model, 0, len(list))
	for _, m := range list {
		if !keyKept(auth, m.ProviderID(), own) {
			continue
		}
		levels := thinkingLevels(m)
		// ProviderID reads ProviderMeta first, where the built-in catalog
		// names the provider; its models have no Provider value.
		out = append(out, proto.Model{
			ModelRef:       proto.ModelRef{Provider: m.ProviderID(), ID: m.ID},
			Name:           m.DisplayName,
			Reasoning:      slices.ContainsFunc(levels, func(level string) bool { return level != string(ai.ThinkingOff) }),
			ThinkingLevels: levels,
			ContextWindow:  m.Capabilities.ContextWindow,
			// A message's attached images reach a model that takes
			// images (ADR-0029).
			Images: slices.Contains(m.Input, ai.ModelInputImage),
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
// approvalInstructions is what instructions said while bash, write and
// edit waited for the owner (before ADR-0012). A session created then
// still carries it, until refreshInstructions replaces it.
func approvalInstructions(cwd string) string {
	return "You are a coding agent working for the owner in " + cwd + ". " +
		"Use the tools to read and change files and to run commands there. " +
		"Calls that change files or run commands wait for the owner's approval; a denied call returns the owner's reason. " +
		"Answer briefly, and say what you changed."
}

// refreshInstructions gives a session whose instructions are still the
// generated ones that promised approvals the current instructions, so its
// agent does not wait on a checkpoint that no longer comes. Instructions
// anyone changed stay as they are.
func (e *Engine) refreshInstructions(ctx context.Context, id int64) error {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return err
	}
	if agent.Instructions == nil || agent.Cwd == nil || *agent.Instructions != approvalInstructions(*agent.Cwd) {
		return nil
	}
	return conversation.Configure(ctx, harness.AgentChange{Instructions: harness.SetTo(instructions(*agent.Cwd))})
}

// sessionInstructions are a session's instructions: pocketd's preamble,
// then, for a session started from a profile, the agent's identity, and
// for a supervisor the roster it delegates to.
func sessionInstructions(spec api.SessionSpec) string {
	text := instructions(spec.Cwd)
	if spec.Worker {
		// A worker takes no profile's identity: it works for its thread's
		// specialist (ADR-0021).
		return text + "\n\n" + team.WorkerInstructions
	}
	if spec.Profile == nil {
		return text
	}
	text += "\n\nYou are " + spec.Profile.Name + ", the team's " + string(spec.Profile.Role) + ". Your profile follows.\n\n" + spec.Profile.Identity
	if spec.Roster != "" {
		text += "\n\n" + spec.Roster
	}
	// The lessons last: pocketd owns the instructions from their marker on
	// (SetLessons).
	return text + spec.Lessons
}

// SetLessons makes section the end of session id's instructions, in
// place of the section pocketd wrote there (team.WithoutLessons), and
// changes nothing before it: instructions without one take section at
// their end. Instructions
// that end with section already stay as they are, so the model's cache of
// them holds.
func (e *Engine) SetLessons(ctx context.Context, id int64, section string) error {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return err
	}
	current := ""
	if agent.Instructions != nil {
		current = *agent.Instructions
	}
	// Only a section pocketd wrote is replaced (team.WithoutLessons): text
	// of an older session's identity that looks like one stays.
	base, _ := team.WithoutLessons(current)
	if base+section == current {
		return nil
	}
	return conversation.Configure(ctx, harness.AgentChange{Instructions: harness.SetTo(base + section)})
}

// Tools are the names of the tools an agent can be given.
func (e *Engine) Tools() []string { return ToolNames() }

// ToolNames are the names of the tools an agent can be given: PiG's coding
// tools. pocketd profiles checks profiles against them without opening
// the engine.
func ToolNames() []string {
	var out []string
	for _, tool := range tools.CodingTools.Tools {
		out = append(out, tool.Name)
	}
	return out
}

// readOnlyTools are the coding tools a cross-review, a consult and a plan's
// lead read with (ADR-0019, ADR-0020): read alone. No shell: in the sandbox
// one could still write other checkouts and reach GitHub as the service
// user; team_diff reads a review's change set instead.
var readOnlyTools = []string{"read"}

// reviewTools are the read-only tools among names, the profile's own: a
// review never gets a tool its profile does not have.
func reviewTools(names []string) []string {
	out := []string{}
	for _, name := range names {
		if slices.Contains(readOnlyTools, name) {
			out = append(out, name)
		}
	}
	return out
}

// workerTools are the coding tools among names, the specialist's own: a
// worker never gets a tool its specialist's profile does not have, nor a
// team tool (ADR-0021).
func workerTools(names []string) []string {
	out := []string{}
	for _, name := range names {
		if slices.Contains(ToolNames(), name) {
			out = append(out, name)
		}
	}
	return out
}

// toolsNamed are the engine's tools with these names, in that order. The
// list is never nil: PiG reads a nil list as no change, which would give a
// profile with no tools every tool.
func (e *Engine) toolsNamed(names []string) ([]*durable.ToolRegistration, error) {
	out := []*durable.ToolRegistration{}
	for _, name := range names {
		i := slices.IndexFunc(e.coding.Tools, func(t *durable.ToolRegistration) bool { return t.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("no tool %q", name)
		}
		out = append(out, e.coding.Tools[i])
	}
	return out, nil
}

func instructions(cwd string) string {
	return "You are a coding agent working for the owner in " + cwd + ". " +
		"Use the tools to read and change files and to run commands there. " +
		"Answer briefly, and say what you changed."
}

// created records, in the commit that creates a conversation, the request
// key that created it, so a retry finds the conversation instead of creating
// a second one, even when the work database never recorded it.
type created struct {
	Conversation int64 `json:"conversation"`
	// What the session was created from, so a retry records it as made
	// (empty in a record from before pocketd kept it).
	Title    string `json:"title,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Route    string `json:"route,omitempty"`
	// Standing says it is its profile's standing session (ADR-0026).
	Standing bool `json:"standing,omitempty"`
}

// Created is the session created for key, if one was.
func (e *Engine) Created(ctx context.Context, key string) (api.Created, bool, error) {
	found, err := durable.Snapshot[created](ctx, e.harness, createdDoc, key)
	if err != nil || found == nil {
		return api.Created{}, false, err
	}
	out := api.Created{ID: found.Conversation, Title: found.Title, Cwd: found.Cwd, Thinking: found.Thinking, Profile: found.Profile, Route: found.Route,
		Standing: found.Standing}
	if found.Provider != "" {
		out.Model = proto.ModelRef{Provider: found.Provider, ID: found.Model}
		return out, true, nil
	}
	// A record from before pocketd kept what a session was created from:
	// the conversation's own agent says, and no profile made it, so the
	// owner named its model. Its title is unknown.
	out.Legacy = true
	conversation, err := e.conversation(ctx, found.Conversation)
	if err != nil {
		return api.Created{}, false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return api.Created{}, false, err
	}
	if agent.Model != nil {
		out.Model = proto.ModelRef{Provider: agent.Model.Provider, ID: agent.Model.ModelId}
	}
	if agent.Cwd != nil {
		out.Cwd = *agent.Cwd
	}
	out.Thinking, out.Route = string(agent.ThinkingLevel), "owner"
	return out, true, nil
}

var createdDoc = durable.DefineDocFamily(durable.DocFamilyDefinition[created, created]{
	Family: true, Kind: "pocketgo.created", Version: 1,
	DocumentSemantics: durable.DocumentSemantics{Scope: durable.ScopeSession},
	Initial:           func(seed created) created { return seed },
	CheckpointWhen:    func(created, []durable.Op, durable.CheckpointInfo) bool { return false },
})

// CreateSession creates a conversation for request key and returns its id,
// which is the session id. The same key returns the same conversation. A
// profile's standing session (spec.StandingKey, ADR-0026) is recorded under
// both keys in the commit that creates it; one made under the standing key
// already is the one returned, and key is recorded for it too, so a retry
// of this request finds it whatever the profile says by then.
func (e *Engine) CreateSession(ctx context.Context, key string, spec api.SessionSpec) (int64, error) {
	if found, err := durable.Snapshot[created](ctx, e.harness, createdDoc, key); err != nil {
		return 0, err
	} else if found != nil {
		return found.Conversation, nil
	}
	if spec.StandingKey != "" {
		found, err := durable.Snapshot[created](ctx, e.harness, createdDoc, spec.StandingKey)
		if err != nil {
			return 0, err
		}
		if found != nil {
			conversation, err := e.conversation(ctx, found.Conversation)
			if err != nil {
				return 0, err
			}
			if _, err := conversation.Commit(ctx, func(tx durable.Tx) (any, error) {
				return durable.TxDoc[created](tx, createdDoc, key, *found)
			}); err != nil {
				return 0, err
			}
			return found.Conversation, nil
		}
	}
	agent, err := e.agentOf(spec)
	if err != nil {
		return 0, err
	}
	conversation, err := e.harness.CreateConversation(ctx, harness.ConversationCreateOptions{
		Agent: agent,
		Init: func(tx durable.Tx, id durable.ConversationId) error {
			record := created{Conversation: int64(id), Title: spec.Title, Cwd: spec.Cwd, Provider: spec.Model.Provider, Model: spec.Model.ID,
				Thinking: spec.Thinking, Route: spec.Route, Standing: spec.StandingKey != ""}
			if spec.Profile != nil {
				record.Profile = spec.Profile.ID
			}
			if _, err := durable.TxDoc[created](tx, createdDoc, key, record); err != nil {
				return err
			}
			if spec.StandingKey != "" {
				if _, err := durable.TxDoc[created](tx, createdDoc, spec.StandingKey, record); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		return 0, err
	}
	return int64(conversation.Id()), nil
}

// agentOf is the agent of a session made from spec: its model, its
// working directory, its instructions, and its tools by its profile and its
// role.
func (e *Engine) agentOf(spec api.SessionSpec) (*harness.AgentChange, error) {
	extensions, tools, err := e.toolsOf(spec)
	if err != nil {
		return nil, err
	}
	agent := &harness.AgentChange{
		Model:        harness.SetTo(durable.ModelRef{Provider: spec.Model.Provider, ModelId: spec.Model.ID}),
		Extensions:   harness.SetTo(harness.ExtensionChange{List: extensions}),
		Cwd:          harness.SetTo(spec.Cwd),
		Instructions: harness.SetTo(sessionInstructions(spec)),
	}
	if spec.Thinking != "" {
		agent.ThinkingLevel = harness.SetTo(ai.ModelThinkingLevel(spec.Thinking))
	}
	if tools != nil {
		agent.Tools = harness.SetTo(harness.ToolChange{List: tools})
	}
	return agent, nil
}

// toolsOf are the extensions and the tools of a session made from spec, by
// its profile and its role; nil tools are every tool of the extensions.
func (e *Engine) toolsOf(spec api.SessionSpec) ([]*durable.Extension, []*durable.ToolRegistration, error) {
	extensions := []*durable.Extension{e.coding, e.approval}
	if spec.Worker {
		// A worker gets the coding tools of its specialist's profile, and
		// no team tool (ADR-0021).
		names := ToolNames()
		if spec.WorkerTools != nil {
			names = workerTools(spec.WorkerTools)
		}
		list, err := e.toolsNamed(names)
		return extensions, list, err
	}
	if role := roleTools(spec); len(role) > 0 {
		// The profile's tools, or every coding tool, and the team tools of
		// the session's role (ADR-0014), exactly: the team extension holds
		// all three, and only these are offered.
		names := ToolNames()
		if spec.Profile != nil && spec.Profile.Tools != nil {
			names = spec.Profile.Tools
		}
		// A cross-review, a consult and a plan's lead read and change
		// nothing: the lead writes its plan with team_plan alone, and
		// builds nothing (ADR-0019, ADR-0020).
		if spec.Review || spec.Consult != 0 || spec.Plan != 0 {
			names = reviewTools(names)
		}
		list, err := e.toolsNamed(names)
		if err != nil {
			return nil, nil, err
		}
		return append(extensions, e.teamExt), append(list, e.teamTools(role)...), nil
	}
	if spec.Profile != nil && spec.Profile.Tools != nil {
		// Exactly the profile's tools (ADR-0013).
		list, err := e.toolsNamed(spec.Profile.Tools)
		return extensions, list, err
	}
	return extensions, nil, nil
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

// Send submits the owner's message: text, and images after it
// (ADR-0029). key makes the submission idempotent: the same key returns
// the same submission.
func (e *Engine) Send(ctx context.Context, id int64, key, text string, images ...api.Image) (int64, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return 0, err
	}
	submission, err := conversation.Submit(ctx, durable.SubmissionDraft{RequestId: &key, Type: durable.SubmissionTypeInput, Content: userContent(text, images)})
	if err != nil {
		return 0, err
	}
	return int64(submission.Id()), nil
}

// userContent is a user message of text and images: the text alone, as
// every message before attachments, when there is no image.
func userContent(text string, images []api.Image) ai.UserContent {
	if len(images) == 0 {
		return ai.UserText(text)
	}
	blocks := ai.UserContentBlocks{ai.TextContent{Text: text}}
	for _, image := range images {
		blocks = append(blocks, ai.ImageContent{Data: base64.StdEncoding.EncodeToString(image.Data), MimeType: image.MimeType})
	}
	return blocks
}

// SendIdle is Send with PiG's WhenBusyReject: the submission is refused,
// with nothing written, while the session has a run (api.ErrSessionBusy).
func (e *Engine) SendIdle(ctx context.Context, id int64, key, text string) (int64, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return 0, err
	}
	submission, err := conversation.Submit(ctx, durable.SubmissionDraft{RequestId: &key, Type: durable.SubmissionTypeInput, Content: ai.UserText(text),
		WhenBusy: durable.WhenBusyReject})
	var busy *durable.ConversationBusy
	if errors.As(err, &busy) {
		return 0, api.ErrSessionBusy
	}
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
	return e.TranscriptAfter(ctx, id, 0)
}

// TranscriptAfter is Transcript's entries after entry after, with the live
// state of their revision: a worker's replies are read on from its cursor,
// never its whole history again (ADR-0021; Codex review of #29, round 4).
func (e *Engine) TranscriptAfter(ctx context.Context, id, after int64) ([]proto.Entry, proto.Live, error) {
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
	live := viewLive(id, view, &queueCache{})
	if e.betweenReads != nil {
		e.betweenReads()
	}
	var newest int64
	for _, record := range view.Entries {
		newest = max(newest, int64(record.Id))
	}
	if newest <= after {
		return nil, live, nil
	}
	records, err := entriesBetween(ctx, conversation, after, newest)
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
	queue    queueCache // the queue as the last live state read it
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
	if err := e.refreshInstructions(ctx, id); err != nil {
		return err
	}
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
	live := viewLive(id, first, &w.queue)
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
	live := viewLive(id, view, &w.queue)
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
