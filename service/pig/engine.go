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
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
	// Self is the program Engine.ReadUpTo runs in the sandbox as
	// ReadHelper, and loadContext as ContextHelper: pocketd,
	// os.Executable() when empty.
	Self string
	// Instructions is the owner's global AGENTS.md, which every session's
	// instructions hold, and Skills the global skills folder, which every
	// session lists (ADR-0043); "" for none.
	Instructions, Skills string
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
	// MCP are the owner's MCP servers, mcp.json as checked, and
	// MCPProfiles each profile's mcp list, by profile id, as pocketd read
	// them at start (ADR-0063): the servers pocketd connects for the
	// sessions of the profiles that name them. Their credentials are kept
	// in DataDir (MCPAuthFile).
	MCP         profiles.MCPServers
	MCPProfiles map[string][]string
	// OnMCPTools is told each profile whose MCP servers' tools changed,
	// once its extension holds them: its sessions get them by
	// SetMCPTools. It must not block.
	OnMCPTools func(profile string)
	// Log takes what the engine reports as it serves: an MCP server's
	// state (ADR-0063). Nil logs nothing.
	Log *slog.Logger
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
	// teamExt holds the team tools, which call team (ADR-0014),
	// questionExt the question tool, which a private agent's session
	// gets with no team tool (ADR-0048), and scheduleExt a private
	// agent's schedule tool, which is no team tool either (ADR-0060).
	teamExt     *durable.Extension
	questionExt *durable.Extension
	scheduleExt *durable.Extension
	team        Team
	// mcp holds the MCP servers the profiles name (ADR-0063); nil holds
	// none, as when their store could not be read (mcpNote says why).
	mcp       *mcpHub
	mcpNote   string
	mu        sync.Mutex
	watches   map[int64]*watcher
	closeOnce sync.Once
	protected []string
	// readOnly are the folders the file tools read but never change
	// (guardedEnv.readOnly, ADR-0035); the sandbox's shown paths, which
	// since ADR-0045 include the owner's global instructions file.
	readOnly []string
	// readOnlyFiles are the sandbox's files among readOnly (ADR-0045).
	readOnlyFiles []string
	// stopShown ends the sandbox's watch of those folders
	// (Sandbox.watchShown), and waits, until ctx ends, for the look it may
	// be in to end too.
	stopShown func(ctx context.Context) error
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

// shownTicker ticks the sandbox's watch of its shown folders every
// shownEvery; tests tick it themselves.
var shownTicker = func() (<-chan time.Time, func()) {
	t := time.NewTicker(shownEvery)
	return t.C, t.Stop
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
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
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
	if opts.Sandbox != nil {
		// The file tools read the folders the sandbox shows read-only, and
		// refuse to change them, as a command cannot (ADR-0035): at the
		// paths the sandbox resolved and checked, never resolved again, so
		// a link put in one's place since leads nowhere the tools do not
		// watch (Codex review of #63, round 3).
		e.readOnly = slices.Clone(opts.Sandbox.Shown)
		e.readOnlyFiles = slices.Clone(opts.Sandbox.files)
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
	e.questionExt = e.questionExtension()
	if err := registry.Install(e.questionExt); err != nil {
		return nil, err
	}
	e.scheduleExt = e.scheduleExtension()
	if err := registry.Install(e.scheduleExt); err != nil {
		return nil, err
	}
	// The profiles' MCP servers, each profile's tools in an extension of
	// its own, as their servers listed them last (ADR-0063). A store that
	// cannot be read connects none, and pocketd serves without them.
	if len(opts.MCPProfiles) > 0 && len(opts.MCP.Servers) > 0 {
		store, err := openMCPStore(opts.DataDir)
		if err != nil {
			e.mcpNote = "the MCP servers are not connected: " + err.Error()
		} else {
			e.mcp = newMCPHub(store, registry, opts.MCP, opts.MCPProfiles, opts.Log, opts.OnMCPTools)
		}
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
			return &guardedEnv{ExecutionEnv: local, protected: e.protected, readOnly: e.readOnly, files: e.readOnlyFiles, commands: e.commands, sandbox: opts.Sandbox}, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("durable harness: %w", err)
	}
	e.harness, e.storage = h, store
	e.stopShown = func(context.Context) error { return nil }
	if opts.Sandbox != nil && len(opts.Sandbox.Shown) > 0 {
		// A folder replaced while commands run stops them, though no new
		// command starts (Codex review of #63, round 1). Close waits for
		// the watch, which may be stopping commands, so it makes no folder
		// again after Close (round 8).
		watch, stop := context.WithCancel(context.Background())
		ticks, stopTicks := shownTicker()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer stopTicks()
			opts.Sandbox.watchShown(watch, ticks)
		}()
		e.stopShown = func(ctx context.Context) error {
			stop()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("the watch of the shown folders (ADR-0035) did not end: %w", ctx.Err())
			}
		}
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
// they are and resume at the next Open. It succeeds only once the watch of
// the shown folders ended, within ctx: every call waits for it, one tried
// again after a Close that failed so too (Codex review of #63, round 9),
// though the harness closes once.
func (e *Engine) Close(ctx context.Context) error {
	var watchErr error
	if e.stopShown != nil {
		watchErr = e.stopShown(ctx)
	}
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
		// The MCP servers' connections end after the tasks that call
		// them (ADR-0063).
		e.mcp.close(ctx)
	})
	return errors.Join(watchErr, err)
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
// the owner's global AGENTS.md, then, for a session started from a
// profile, the agent's identity, and for a supervisor the roster it
// delegates to, or for a session at the agent's home that starts work
// sessions where it changes a repository (team.WorkGuideFor, ADR-0073),
// then its folder's
// AGENTS.md and the skills it may read,
// then its lessons section (ADR-0043); loaded holds the files' parts. A
// private agent's identity starts with team.PrivateIntro, and has no
// roster and no lessons section (ADR-0042).
func sessionInstructions(spec api.SessionSpec, loaded team.Loaded) string {
	text := instructions(spec.Cwd)
	if spec.Worker {
		// A worker takes no profile's identity: it works for its thread's
		// specialist (ADR-0021).
		return team.Instructions("", text, loaded.Global, team.WorkerInstructions, loaded.Folder, loaded.Skills)
	}
	if spec.Profile == nil {
		return team.Instructions("", text, loaded.Global, loaded.Folder, loaded.Skills)
	}
	if spec.Profile.Private {
		return team.Instructions("", text, loaded.Global, team.PrivateIntro(spec.Profile.Name)+"\n\n"+spec.Profile.Identity, loaded.Folder, loaded.Skills)
	}
	identity := "You are " + spec.Profile.Name + ", the team's " + string(spec.Profile.Role) + ". Your profile follows.\n\n" + spec.Profile.Identity
	// A session at the agent's home that starts work sessions is told
	// where it changes a repository; one the owner opened in a repository
	// is not (team.WorkGuideFor, ADR-0073).
	guide := team.WorkGuideFor(spec.Profile, roleOf(spec), spec.Cwd, spec.Repository)
	// The lessons last: pocketd owns the instructions from their marker on
	// (SetLessons).
	return team.Instructions(spec.Lessons, text, loaded.Global, identity, spec.Roster, guide, loaded.Folder, loaded.Skills)
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
// user; team_diff reads a review's change set instead, and team_github
// reads GitHub with GET alone for a consult, a plan's lead and a review
// (ADR-0036, ADR-0057).
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
	// What its instructions hold of the files pocketd reads is read once,
	// now, and kept with it (ADR-0043).
	loaded := e.loadContext(ctx, spec)
	agent, err := e.agentOf(spec, loaded)
	if err != nil {
		return 0, err
	}
	conversation, err := e.harness.CreateConversation(ctx, harness.ConversationCreateOptions{
		Agent: agent,
		Init: func(tx durable.Tx, id durable.ConversationId) error {
			if err := keepLoaded(tx, int64(id), loaded); err != nil {
				return err
			}
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
// working directory, its instructions with loaded, and its tools by its
// profile and its role.
func (e *Engine) agentOf(spec api.SessionSpec, loaded team.Loaded) (*harness.AgentChange, error) {
	extensions, tools, err := e.toolsOf(spec)
	if err != nil {
		return nil, err
	}
	agent := &harness.AgentChange{
		Model:        harness.SetTo(durable.ModelRef{Provider: spec.Model.Provider, ModelId: spec.Model.ID}),
		Extensions:   harness.SetTo(harness.ExtensionChange{List: extensions}),
		Cwd:          harness.SetTo(spec.Cwd),
		Instructions: harness.SetTo(sessionInstructions(spec, loaded)),
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
	// The MCP tools of the profile's servers, after every other tool, in
	// the profile's own extension, for a session that takes them
	// (ADR-0063): never a worker's, above, a review's, a consult's, a
	// gather's or a plan's lead's.
	mcpExt := e.mcpExtensionOf(spec)
	readOnly := spec.Review || spec.Consult != 0 || spec.Plan != 0 || spec.Gather != 0
	private := spec.Profile != nil && spec.Profile.Private
	// A supervisor's and a private agent's session, working as no thread,
	// review, worker, consult or gather, puts questions to the owner
	// (ADR-0048).
	asks := team.MayQuestion(spec.Profile, roleOf(spec))
	if role := roleTools(spec); len(role) > 0 || private || asks {
		// The profile's tools, or every coding tool, and the team tools of
		// the session's role (ADR-0014), exactly: the team extension holds
		// all three, and only these are offered. A private agent's list is
		// exact too, with no team tool (ADR-0042): a standing session takes
		// a list only when it is one, so a session whose profile had a
		// filter and became private with none gets every coding tool.
		names := ToolNames()
		if spec.Profile != nil && spec.Profile.Tools != nil {
			names = spec.Profile.Tools
		}
		// A cross-review, a consult, a gather's agent and a plan's lead read and change
		// nothing: the lead writes its plan with team_plan alone, and
		// builds nothing (ADR-0019, ADR-0020); a private agent in one of
		// these roles too, with no team tool (ADR-0042).
		if readOnly {
			names = reviewTools(names)
		}
		list, err := e.toolsNamed(names)
		if err != nil {
			return nil, nil, err
		}
		if len(role) > 0 {
			extensions, list = append(extensions, e.teamExt), append(list, e.teamTools(role)...)
		}
		// The question tool comes after the team tools, in its own
		// extension: a private agent gets it with none of them.
		if asks {
			extensions, list = append(extensions, e.questionExt), append(list, e.questionExt.Tools...)
		}
		// A private agent's session, working as no thread, review,
		// worker, consult or gather, keeps its own schedules, with the
		// schedule tool after the question tool, in its own extension: no
		// team tool (ADR-0060). A supervisor keeps team_schedule.
		if team.MaySchedule(spec.Profile, roleOf(spec)) {
			extensions, list = append(extensions, e.scheduleExt), append(list, e.scheduleExt.Tools...)
		}
		if mcpExt != nil {
			extensions, list = append(extensions, mcpExt), append(list, mcpWithin(mcpExt.Tools, len(list))...)
		}
		return extensions, list, nil
	}
	if spec.Profile != nil && spec.Profile.Tools != nil {
		// Exactly the profile's tools (ADR-0013), and its MCP tools.
		list, err := e.toolsNamed(spec.Profile.Tools)
		if err == nil && mcpExt != nil {
			extensions, list = append(extensions, mcpExt), append(list, mcpWithin(mcpExt.Tools, len(list))...)
		}
		return extensions, list, err
	}
	// Every tool of the extensions: the MCP extension's too, whatever its
	// servers list later.
	if mcpExt != nil {
		extensions = append(extensions, mcpExt)
	}
	return extensions, nil, nil
}

// mcpExtensionOf is the extension of the MCP tools a session made from
// spec takes (team.MayUseMCP): its profile's, nil for none.
func (e *Engine) mcpExtensionOf(spec api.SessionSpec) *durable.Extension {
	if spec.Profile == nil || spec.Worker || !team.MayUseMCP(roleOf(spec)) {
		return nil
	}
	return e.mcp.extension(spec.Profile.ID)
}

// MCPNote says why the MCP servers are not connected, "" when they are or
// there are none (ADR-0063).
func (e *Engine) MCPNote() string { return e.mcpNote }

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

// Send submits the owner's message: text, and what it attached after it
// (ADR-0029). key makes the submission idempotent: the same key returns
// the same submission.
func (e *Engine) Send(ctx context.Context, id int64, key, text string, attached ...api.Attached) (int64, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return 0, err
	}
	submission, err := conversation.Submit(ctx, durable.SubmissionDraft{RequestId: &key, Type: durable.SubmissionTypeInput, Content: userContent(text, attached)})
	if err != nil {
		return 0, err
	}
	return int64(submission.Id()), nil
}

// userContent is a user message of text and what it attached: the text
// alone when it attached nothing; else its text, unless it is white space
// alone, then a block of proto.AttachmentsHeading alone and a block of the
// files' lines, then its images. The heading's own block, which no text of
// the owner's ever is, tells the files apart from the owner's words
// (attachedText; Codex review of #49, round 8).
func userContent(text string, attached []api.Attached) ai.UserContent {
	if len(attached) == 0 {
		return ai.UserText(text)
	}
	var blocks ai.UserContentBlocks
	if strings.TrimSpace(text) != "" {
		blocks = append(blocks, ai.TextContent{Text: text})
	}
	for _, a := range attached {
		if a.Lines != "" {
			blocks = append(blocks, ai.TextContent{Text: proto.AttachmentsHeading}, ai.TextContent{Text: a.Lines})
		}
		for _, image := range a.Images {
			blocks = append(blocks, ai.ImageContent{Data: base64.StdEncoding.EncodeToString(image.Data), MimeType: image.MimeType})
		}
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
	watch, err := conversation.Watch(ctx, viewWindow())
	if err != nil {
		return nil, proto.Live{}, err
	}
	view := watch.Value()
	_, _ = watch.Stop()
	queue := &queueCache{sender: e.queuedSender(ctx)}
	live := e.liveOf(id, view, queue)
	// A queued message whose sender could not be read fails the read, so
	// no copy of the session lists its queue without it (Codex review of
	// #56, round 2): the caller reads again.
	if queue.partial {
		return nil, proto.Live{}, errSenderUnread
	}
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
	read, err := entriesBetween(ctx, conversation, after, newest)
	if err != nil {
		return nil, proto.Live{}, err
	}
	entries, err := e.placeEntries(ctx, id, read)
	if err != nil {
		return nil, proto.Live{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, live, nil
}

// placeEntries gives each user entry of session id's entries the
// submission that placed it, and its sender when it is not the owner's
// (ADR-0032, ADR-0065), and each answer's usage its cost only when its provider is
// metered (ADR-0052).
func (e *Engine) placeEntries(ctx context.Context, id int64, entries []proto.Entry) ([]proto.Entry, error) {
	for i := range entries {
		e.priceEntry(&entries[i])
	}
	users := map[int64]bool{}
	for _, entry := range entries {
		if entry.Kind == proto.EntryUser {
			users[entry.ID] = true
		}
	}
	placed, err := e.placedBy(ctx, id, users)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		by := placed[entries[i].ID]
		entries[i].SubmissionID, entries[i].Sender = by.submission, by.sender
	}
	return entries, nil
}

// errSenderUnread is a read of a session whose queued message's sender
// the store could not give (ADR-0032).
var errSenderUnread = errors.New("the sender of a queued message could not be read: read the session again")

// placing is the submission that placed a user entry, and who sent it
// when it is not the owner (senderOf).
type placing struct {
	submission int64
	sender     *proto.Sender
}

// senderOf names who sent a submission of request key that is not the
// owner: a supervisor's message by its key (team.SentBy, ADR-0032), else
// the service's own for every key a device did not make (team.IsOwnerKey,
// ADR-0065); nil for the owner's message and for a submission with no key.
func senderOf(requestKey *string) *proto.Sender {
	if requestKey == nil {
		return nil
	}
	if supervisor, ok := team.SentBy(*requestKey); ok {
		return &proto.Sender{Kind: proto.SenderSupervisor, Session: supervisor}
	}
	if team.IsOwnerKey(*requestKey) {
		return nil
	}
	return &proto.Sender{Kind: proto.SenderService}
}

// queuedSender names who sent a queued submission of id, as a live
// state's queue lists it (queueCache): a submission the store does not
// have is the owner's; a read that fails could not tell.
func (e *Engine) queuedSender(ctx context.Context) func(int64) (*proto.Sender, bool) {
	return func(id int64) (*proto.Sender, bool) {
		record, err := e.storage.Submission(ctx, durable.SubmissionId(id))
		if err != nil {
			return nil, false
		}
		if record == nil {
			return nil, true
		}
		return senderOf(record.RequestId), true
	}
}

// placedBy finds the submissions that placed the entries of session id in
// want, by entry id, each with its sender: a placed input records its
// entry, which the harness writes in the same commit. It reads the
// session's submissions newest first and stops once it found every entry,
// so a new message costs a page.
func (e *Engine) placedBy(ctx context.Context, id int64, want map[int64]bool) (map[int64]placing, error) {
	found := map[int64]placing{}
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
				found[int64(*record.Entry)] = placing{submission: int64(record.Id), sender: senderOf(record.RequestId)}
			}
		}
		if len(found) == len(want) || page.Next == nil {
			return found, nil
		}
		cursor = *page.Next
	}
}

// recordsAtOnce is the most records pocketd holds of a session at once,
// as it reads its transcript (entriesBetween) or watches it (viewWindow):
// a record of the owner's message holds its images, up to
// uploads.MaxMessageImages of them, and an entry keeps none of them, so a
// read holds few records, never a page of hundreds (Codex review of #49,
// round 9).
const recordsAtOnce = 2

// viewWindow is the window of every view pocketd takes of a session: its
// newest records, at most recordsAtOnce of them. The view tells where the
// transcript ends, and entriesBetween reads the entries before it.
func viewWindow() *harness.ConversationWindow {
	least, most := 1, recordsAtOnce
	return &harness.ConversationWindow{Min: &least, Max: &most}
}

// entriesBetween reads the entries with after < id <= through, as
// entries: each page of records is converted, and let go, before the next
// is read, so a transcript's images, which an entry never keeps, are held
// a page at a time (Codex review of #49, round 8). The entries are not
// placed (placeEntries).
func entriesBetween(ctx context.Context, conversation harness.Conversation, after, through int64) ([]proto.Entry, error) {
	minID, maxID := durable.EntryId(after+1), durable.EntryId(through)
	var out []proto.Entry
	var cursor durable.Cursor
	for {
		page, err := conversation.Entries(ctx, durable.EntryQuery{MinEntryId: &minID, MaxEntryId: &maxID}, recordsAtOnce, cursor)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Items {
			out = append(out, wireEntry(record))
		}
		if page.Next == nil {
			return out, nil
		}
		cursor = *page.Next
	}
}

// EntriesAfter is at most most of session id's entries after entry after,
// oldest first, as the search index reads them (ADR-0050): read a few
// records at a time (recordsAtOnce), and not placed, since a search names
// no submission or sender.
func (e *Engine) EntriesAfter(ctx context.Context, id, after int64, most int) ([]proto.Entry, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return nil, err
	}
	minID, order := durable.EntryId(after+1), durable.ScanAscending
	var out []proto.Entry
	var cursor durable.Cursor
	for len(out) < most {
		page, err := conversation.Entries(ctx, durable.EntryQuery{MinEntryId: &minID, Order: &order}, min(recordsAtOnce, most-len(out)), cursor)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Items {
			if len(out) < most {
				out = append(out, wireEntry(record))
			}
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	return out, nil
}

// RunIdentity names the session's current run by its first entry; busy is
// false when no run is in progress.
func (e *Engine) RunIdentity(ctx context.Context, id int64) (first int64, busy bool, err error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return 0, false, err
	}
	watch, err := conversation.Watch(ctx, viewWindow())
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
	return wireUsageState(state, e.Metered), nil
}

// UsageEntries are session id's answers with usage after entry after, read
// newest first down to the first one before since, which is left out with
// every older one, and the session's newest entry (ADR-0052): a session's
// answers are written in the order of their times, so no older one is
// read. Each page of records is converted, and let go, before the next is
// read, as entriesBetween does. It reads only entries after after: the
// first one read is the newest, and a session with none new reads an
// empty page, never its newest records again (Codex review of #107, round
// 1). Each answer's usage is as PiG keeps it: the report prices it once it
// resolved the answer's model (Metered; Codex review of #107, round 2).
func (e *Engine) UsageEntries(ctx context.Context, id, after int64, since time.Time) ([]api.UsageEntry, int64, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	minID, order := durable.EntryId(after+1), durable.ScanDescending
	newest := after
	var out []api.UsageEntry
	var cursor durable.Cursor
	for {
		page, err := conversation.Entries(ctx, durable.EntryQuery{MinEntryId: &minID, Order: &order}, recordsAtOnce, cursor)
		if err != nil {
			return nil, 0, err
		}
		for _, record := range page.Items {
			newest = max(newest, int64(record.Id))
			entry := wireEntry(record)
			if entry.Kind != proto.EntryAssistant || entry.Usage == nil {
				continue
			}
			if entry.At.Before(since) {
				return out, newest, nil
			}
			u := api.UsageEntry{ID: entry.ID, At: entry.At, Usage: *entry.Usage}
			if entry.Model != nil {
				u.Model = *entry.Model
			}
			out = append(out, u)
		}
		if page.Next == nil {
			return out, newest, nil
		}
		cursor = *page.Next
	}
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
	handle, err := conversation.Watch(context.WithoutCancel(ctx), viewWindow())
	if err != nil {
		return nil, err
	}
	w := &watcher{handle: handle, conversation: conversation}
	// The watch outlives the caller's request: its queue's lookups do too.
	w.queue.sender = e.queuedSender(context.WithoutCancel(ctx))
	first := handle.Value()
	for _, record := range first.Entries {
		w.lastID = max(w.lastID, int64(record.Id))
	}
	live := e.liveOf(id, first, &w.queue)
	w.lastLive, _ = json.Marshal(live)
	e.watches[id] = w
	// A queued message whose sender could not be read is left out, and
	// read again soon, whether or not anything commits (Codex review of
	// #56, round 1).
	// The retry is installed under w.mu, which its callback takes before
	// it reads or replaces w.cancel (Codex review of #56, round 2).
	if w.queue.partial {
		w.mu.Lock()
		w.cancel = e.retry(first, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			e.advanceLocked(context.Background(), id, w, first, 0)
		})
		w.mu.Unlock()
	}
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
	live := e.liveOf(id, view, &w.queue)
	if data, err := json.Marshal(live); err == nil && string(data) != string(w.lastLive) {
		w.lastLive = data
		e.opts.OnLive(live)
	}
	// A queued message left out for its sender is read again soon, whether
	// or not anything commits (Codex review of #56, round 1).
	if w.queue.partial {
		w.cancel = e.retry(view, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			e.advanceLocked(context.Background(), id, w, view, call)
		})
	}
}

// newEntries are the entries of view after the watcher's cursor, in order,
// read in full from the store where the view's window does not reach.
func (e *Engine) newEntries(ctx context.Context, id int64, w *watcher, view harness.ConversationView) ([]proto.Entry, error) {
	var entries []proto.Entry
	for _, record := range view.Entries {
		if int64(record.Id) > w.lastID {
			entries = append(entries, wireEntry(record))
		}
	}
	if view.Before != nil && int64(*view.Before) > w.lastID {
		missed, err := entriesBetween(ctx, w.conversation, w.lastID, int64(*view.Before))
		if err != nil {
			return nil, err
		}
		entries = append(entries, missed...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	entries = slices.CompactFunc(entries, func(a, b proto.Entry) bool { return a.ID == b.ID })
	return e.placeEntries(ctx, id, entries)
}
