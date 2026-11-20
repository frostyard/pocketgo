package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// Team runs the team tools' work (ADR-0014); api.Server implements it.
// Each method returns the text the calling agent sees, or an error whose
// text it sees instead.
type Team interface {
	TeamDelegate(ctx context.Context, call api.TeamCall, d team.Delegation) (string, error)
	TeamMessage(ctx context.Context, call api.TeamCall, r team.Report) (string, error)
	TeamClose(ctx context.Context, call api.TeamCall, thread int64) (string, error)
	TeamMemory(ctx context.Context, call api.TeamCall, m team.Memory) (string, error)
	TeamVerdict(ctx context.Context, call api.TeamCall, v team.Verdict) (string, error)
}

// SetTeam wires the team tools to t. Call it before Resume, as the other
// receivers are wired: no tool runs before.
func (e *Engine) SetTeam(t Team) { e.team = t }

// The team tools' names.
const (
	toolDelegate = "team_delegate"
	toolMessage  = "team_message"
	toolClose    = "team_close"
	toolMemory   = "team_memory"
	toolVerdict  = "team_verdict"
)

const delegateParameters = `{"type":"object","properties":{` +
	`"agent":{"type":"string","description":"The id of the specialist's profile, as your instructions list it."},` +
	`"task":{"type":"string","description":"What the specialist is to do, in full: it sees only this and its own profile. At most 6000 characters."},` +
	`"title":{"type":"string","description":"A short title for the thread, at most 120 characters. Default: the task's first line."},` +
	`"repository":{"type":"string","description":"owner/name of a repository the specialist works on: it works in a new git worktree of it, on a branch of its own. Leave it out for work in the specialist's home."}` +
	`},"required":["agent","task"],"additionalProperties":false}`

const messageParameters = `{"type":"object","properties":{` +
	`"message":{"type":"string","description":"Your report to the supervisor, at most 6000 characters."},` +
	`"status":{"type":"string","enum":["working","done","blocked"],"description":"working for progress, done when the work is finished, blocked when you cannot go on."},` +
	`"asks":{"type":"array","maxItems":4,"description":"Decisions you need from the owner, at most 4. Each waits in the owner's Inbox, and its answer comes back to this session as a message.",` +
	`"items":{"type":"object","properties":{` +
	`"question":{"type":"string","description":"The question, at most 500 characters."},` +
	`"options":{"type":"array","items":{"type":"string"},"description":"2 to 6 suggested answers of at most 40 characters each, or none."}` +
	`},"required":["question"],"additionalProperties":false}},` +
	`"pull_requests":{"type":"array","maxItems":10,"description":"The pull requests you opened for this thread, at most 10. pocketd checks each on GitHub and links it to the thread.",` +
	`"items":{"type":"object","properties":{` +
	`"number":{"type":"integer","description":"The pull request's number."},` +
	`"repository":{"type":"string","description":"owner/name: only when the thread works in no repository."}` +
	`},"required":["number"],"additionalProperties":false}}` +
	`},"required":["message","status"],"additionalProperties":false}`

const memoryParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["add","update","remove","list"],"description":"add a lesson, update its text, remove it, or list your lessons."},` +
	`"text":{"type":"string","description":"add, update: the lesson, one or two sentences, at most 300 characters."},` +
	`"id":{"type":"string","description":"update, remove: the lesson's id, as L3."},` +
	`"agent":{"type":"string","description":"Supervisor only, add or list: another agent's id, as your instructions list it."}` +
	`},"required":["action"],"additionalProperties":false}`

const verdictParameters = `{"type":"object","properties":{` +
	`"pull_request":{"type":"object","description":"The linked pull request you reviewed.","properties":{` +
	`"number":{"type":"integer","description":"The pull request's number."},` +
	`"repository":{"type":"string","description":"owner/name: needed when your thread works in no repository, or in another one."}` +
	`},"required":["number"],"additionalProperties":false},` +
	`"head":{"type":"string","description":"The full 40-character id of the head commit you reviewed. A push after your review voids it."},` +
	`"verdict":{"type":"string","enum":["ready","not-ready","unclear"],"description":"ready: nothing blocks it; not-ready: something blocks it; unclear: you could not tell."},` +
	`"summary":{"type":"string","description":"What you reviewed and why you decided so, at most 2000 characters."},` +
	`"findings":{"type":"array","maxItems":20,"items":{"type":"string"},"description":"Each finding, at most 500 characters, the blocking ones first. A not-ready verdict names at least one."}` +
	`},"required":["pull_request","head","verdict","summary"],"additionalProperties":false}`

const closeParameters = `{"type":"object","properties":{` +
	`"thread":{"type":"integer","description":"The thread's number: 12 for T12."}` +
	`},"required":["thread"],"additionalProperties":false}`

// schema is a tool's schema from its JSON parameters.
func schema(name, description, parameters string) ai.ToolSchema {
	encoded, err := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}{name, description, json.RawMessage(parameters)})
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", name, err))
	}
	var out ai.ToolSchema
	if err := json.Unmarshal(encoded, &out); err != nil {
		panic(fmt.Sprintf("tool %s: %v", name, err))
	}
	return out
}

// teamExtension holds the team tools. Every one is replay-safe: what it
// does is keyed by the calling session and the call's id, so a run after a
// restart finds what the first one did (api.TeamCall).
func (e *Engine) teamExtension() *durable.Extension {
	return &durable.Extension{
		Name: "pocketgo-team",
		Tools: []*durable.ToolRegistration{
			{
				ToolSchema: schema(toolDelegate, "Delegate a task to a specialist in a new thread. pocketd checks the rules, makes the specialist's worktree, "+
					"starts its session and sends it a brief. The specialist's reports come back to you as messages.", delegateParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					d, err := durable.FromJsonValue[team.Delegation](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamDelegate(ctx, call, d) })
				},
			},
			{
				ToolSchema: schema(toolMessage, "Report on your thread to the supervisor, with the decisions you need from the owner as asks.", messageParameters),
				Replay:     durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					r, err := durable.FromJsonValue[team.Report](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamMessage(ctx, call, r) })
				},
			},
			{
				ToolSchema: schema(toolClose, "Close a thread you delegated, which frees its place on the desk.", closeParameters),
				Replay:     durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					in, err := durable.FromJsonValue[struct {
						Thread int64 `json:"thread"`
					}](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamClose(ctx, call, in.Thread) })
				},
			},
			{
				ToolSchema: schema(toolVerdict, "File your cross-review verdict on a linked pull request, at the head you reviewed. pocketd checks that you are of another "+
					"model family than its author, then keeps its ready-for-human label in step: it has the label while the newest verdict at its head is ready and CI there is green.", verdictParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					v, err := durable.FromJsonValue[team.Verdict](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamVerdict(ctx, call, v) })
				},
			},
			{
				ToolSchema: schema(toolMemory, "Keep your lessons, across all your sessions: add, update, remove or list them. "+
					"Every change waits for the owner's approval before your sessions get it.", memoryParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					m, err := durable.FromJsonValue[team.Memory](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamMemory(ctx, call, m) })
				},
			},
		},
	}
}

type apiCall = api.TeamCall

// runTeam runs one team call for the calling conversation. A refusal or a
// failure is an error result the agent reads, not a failed task.
func (e *Engine) runTeam(ctx context.Context, call durable.ToolExecutionApi, run func(Team, apiCall) (string, error)) (durable.ToolExecutionResult, error) {
	if e.team == nil {
		return toolText("", errors.New("pocketd's team is not wired")), nil
	}
	return toolText(run(e.team, apiCall{Session: int64(call.ConversationId()), Task: int64(call.TaskId()), CallID: call.CallId()})), nil
}

// toolText is a tool's result: text, or err's text as an error result.
func toolText(text string, err error) durable.ToolExecutionResult {
	if err != nil {
		isError := true
		return durable.ToolExecutionResult{Output: []ai.ToolResultMessageContent{ai.TextContent{Text: err.Error()}}, IsError: &isError}
	}
	return durable.ToolExecutionResult{Output: []ai.ToolResultMessageContent{ai.TextContent{Text: text}}}
}

// roleTools are the team tools a session gets by its role (ADR-0014): a
// supervisor delegates and closes; a thread's specialist reports.
func roleTools(spec api.SessionSpec) []string {
	// A cross-review files its verdict and nothing else (ADR-0019).
	if spec.Review {
		return []string{toolVerdict}
	}
	var out []string
	if spec.Profile != nil && spec.Profile.Role == profiles.RoleSupervisor {
		out = append(out, toolDelegate, toolClose)
	}
	if spec.Thread != 0 {
		out = append(out, toolMessage)
	}
	if spec.Profile != nil {
		out = append(out, toolMemory)
	}
	return out
}

// teamTools are the team tools with these names.
func (e *Engine) teamTools(names []string) []*durable.ToolRegistration {
	var out []*durable.ToolRegistration
	for _, tool := range e.teamExt.Tools {
		if slices.Contains(names, tool.Name) {
			out = append(out, tool)
		}
	}
	return out
}

// gitOutput and gitDiagnostics bound what pocketd keeps of a git command's
// output and errors: a hook or a filter of the checkout may print without
// end. gitWait bounds how long a command's pipes may outlive it.
const (
	gitOutput      = 64 << 10
	gitDiagnostics = 4 << 10
	gitWait        = 5 * time.Second
)

// capped keeps the first max bytes written to it and counts the rest.
type capped struct {
	max     int
	buf     []byte
	dropped int64
}

func (c *capped) Write(p []byte) (int, error) {
	n := min(max(c.max-len(c.buf), 0), len(p))
	c.buf = append(c.buf, p[:n]...)
	c.dropped += int64(len(p) - n)
	return len(p), nil
}

// String is the diagnostics kept, trimmed, with how many bytes went.
func (c *capped) String() string {
	text := strings.TrimSpace(string(c.buf))
	if c.dropped > 0 {
		text += fmt.Sprintf(" … (%d more bytes)", c.dropped)
	}
	return text
}

// Git runs git with args in dir, in the sandbox (ADR-0012), with the same
// environment a tool's command gets: pocketd makes a thread's worktree with
// it, and a hook or a filter of the checkout runs with an agent's rights.
// It keeps at most gitDiagnostics bytes of errors; output past gitOutput
// bytes is an error, never a cut answer, so a caller never reads half a
// record as a whole one. Its answer is the output as printed, but for the
// newline that ends the last record: a path may end in a space. ctx ends
// it.
func (e *Engine) Git(ctx context.Context, dir string, args ...string) (string, error) {
	// git's exit status comes back on a file of its own, so a sandbox that
	// failed around git (a mount, a missing git) is never read as git's
	// answer (worktree.GitExit).
	status, err := os.CreateTemp("", "pocketd-git-")
	if err != nil {
		return "", err
	}
	_ = os.Remove(status.Name())
	defer func() { _ = status.Close() }()
	cmd, err := e.sandboxed(ctx, append([]string{shell(), "-c", gitScript, "sh", "-C", dir}, args...))
	if err != nil {
		return "", err
	}
	stdout, stderr := &capped{max: gitOutput}, &capped{max: gitDiagnostics}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{status}
	runErr := cmd.Run()
	ran, code := gitStatus(status)
	name := strings.Join(args, " ")
	switch {
	case ran && code != 0:
		return "", fmt.Errorf("git %s: %w: %s", name, &worktree.GitExit{Status: code}, stderr.String())
	case !ran:
		if runErr == nil {
			runErr = errors.New("it left no exit status")
		}
		return "", fmt.Errorf("git %s did not run: %w: %s", name, runErr, stderr.String())
	case stdout.dropped > 0:
		return "", fmt.Errorf("git %s printed more than %d bytes", name, gitOutput)
	case runErr != nil:
		return "", fmt.Errorf("git %s: %w: %s", name, runErr, stderr.String())
	}
	return strings.TrimSuffix(string(stdout.buf), "\n"), nil
}

// gitScript runs git with its arguments, with file descriptor 3 closed
// for git and what it starts (a hook), then writes git's exit status
// there: only the shell around git writes it, after git ran.
const gitScript = `git "$@" 3>&-; echo "$?" >&3`

// gitStatus is the exit status gitScript wrote to f, and whether it is
// git's own answer: the shell's 126 and 127 say it could not start git
// (not executable, not found), and a status past 128 that a signal ended
// git (the out-of-memory killer, say). git itself answers 0, 1 or 128.
func gitStatus(f *os.File) (bool, int) {
	var buf [16]byte
	n, _ := f.ReadAt(buf[:], 0)
	code, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || code < 0 || code > 128 || code == 126 || code == 127 {
		return false, 0
	}
	return true, code
}

// ghPaths are where a system installs gh. The PATH is never searched: a
// folder on it the service user owns could hold a fake gh, whose answer
// pocketd would take as GitHub's (ADR-0016).
var ghPaths = []string{"/usr/bin/gh", "/bin/gh", "/usr/local/bin/gh"}

// trustedGH is the first installed gh that only root can change, along
// every name that leads to it (trustedProgram).
func trustedGH() (string, error) {
	var problems []string
	for _, candidate := range ghPaths {
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		program, err := trustedProgram(candidate)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		return program, nil
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("no gh only root can change is installed: %s", strings.Join(problems, "; "))
	}
	return "", fmt.Errorf("gh is not installed in %s", strings.Join(ghPaths, ", "))
}

// ghOutput bounds what pocketd keeps of gh's answer: a pull request's JSON
// is tens of kilobytes.
const ghOutput = 1 << 20

// GH runs gh with args in the sandbox (ADR-0012, ADR-0016), with a tool
// command's environment and gh's prompts, pager, colors and update notes
// off: pocketd reads pull requests with the service user's gh sign-in, as
// agents use it. Output past ghOutput is an error, never a cut answer; an
// error keeps at most gitDiagnostics bytes of what gh printed. ctx ends it.
//
// Its answer stands for GitHub's, so nothing an agent can change decides
// where gh asks: the program is one only root can change (trustedGH), and
// it runs with its configuration folder in pocketd's data folder, which
// the sandbox shows empty and agents cannot write, so no setting of the
// service user's (a host, a socket, an alias) applies; the host is
// github.com, and the sign-in is the token the service user's gh holds.
func (e *Engine) GH(ctx context.Context, args ...string) (string, error) {
	program := e.opts.GH
	if program == "" {
		var err error
		if program, err = trustedGH(); err != nil {
			return "", err
		}
	}
	token, err := e.gh(ctx, program, nil, "auth", "token", "--hostname", "github.com")
	if err != nil {
		return "", fmt.Errorf("the service user's gh sign-in: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\n") {
		return "", errors.New("the service user's gh holds no sign-in for github.com")
	}
	return e.gh(ctx, program, []string{"GH_CONFIG_DIR=" + e.opts.DataDir, "GH_HOST=github.com", "GH_TOKEN=" + token}, args...)
}

// gh runs program with args, and env past a tool command's.
func (e *Engine) gh(ctx context.Context, program string, env []string, args ...string) (string, error) {
	cmd, err := e.sandboxed(ctx, append([]string{program}, args...))
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Dir = "/"
	stdout, stderr := &capped{max: ghOutput}, &capped{max: gitDiagnostics}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	name := strings.Join(args, " ")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", name, err, stderr.String())
	}
	if stdout.dropped > 0 {
		return "", fmt.Errorf("gh %s printed more than %d bytes", name, ghOutput)
	}
	return string(stdout.buf), nil
}

// sandboxed is the command that runs argv in the sandbox (ADR-0012), with
// the environment a tool's command gets (allowedEnv), never pocketd's own:
// a provider key in pocketd's environment never reaches it (protocol rule
// 14c). Only a test, Unsandboxed, runs it outside.
func (e *Engine) sandboxed(ctx context.Context, argv []string) (*exec.Cmd, error) {
	if e.opts.Sandbox != nil {
		wrapped, err := e.opts.Sandbox.Command(argv)
		if err != nil {
			return nil, err
		}
		argv = wrapped
	} else if !e.opts.Unsandboxed {
		return nil, errors.New("the command runs in the sandbox, and there is none")
	}
	//nolint:gosec // G204: git or the shell's test, or bwrap running them, with pocketd's own arguments.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	env := maps.Clone(e.commands)
	// git never asks on a terminal: there is none.
	env["GIT_TERMINAL_PROMPT"] = "0"
	cmd.Env = []string{}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	// A hook's own child that keeps the pipes open does not hold pocketd.
	cmd.WaitDelay = gitWait
	return cmd, nil
}

// Placed says where session's submission of request key is, from the
// durable store's record of that key: absent without one (never sent, or
// a store restored from before it), placed once its entry is in the
// transcript, withdrawn when it ended without one (an abort withdraws the
// queued messages), and queued otherwise.
func (e *Engine) Placed(ctx context.Context, session int64, key string) (api.Placement, error) {
	record, err := e.storage.SubmissionByRequest(ctx, durable.ConversationId(session), key)
	switch {
	case err != nil:
		return 0, err
	case record == nil:
		return api.PlacementAbsent, nil
	case record.Entry != nil:
		return api.PlacementPlaced, nil
	case record.Status == durable.SubmissionUnanswered || record.Status == durable.SubmissionDone:
		return api.PlacementWithdrawn, nil
	}
	return api.PlacementQueued, nil
}

// Withdraw withdraws session's submission of request key while it is
// queued: PiG settles it unanswered and takes it out of the inbox in one
// transaction, so one a run placed meanwhile is left alone (false).
func (e *Engine) Withdraw(ctx context.Context, session int64, key string) (bool, error) {
	conversation := durable.ConversationId(session)
	record, err := e.storage.SubmissionByRequest(ctx, conversation, key)
	if err != nil || record == nil {
		return false, err
	}
	result, err := e.harness.AbortSubmission(ctx, record.Id, &conversation)
	return result == durable.SubmissionAborted, err
}

// Busy says whether session has a run, from its pi.live document alone:
// no transcript is read.
func (e *Engine) Busy(ctx context.Context, session int64) (bool, error) {
	live, err := durable.Snapshot[harness.LiveState](ctx, e.harness, harness.LiveDoc, durable.ConversationId(session))
	if err != nil {
		return false, err
	}
	return live != nil && live.Run != nil, nil
}

// probe is the shell script of the sandbox's checks: it prints exactly
// one answer, so an exit or an output of anything else (bwrap failing to
// make the sandbox, say) is an error, never an answer.
const probe = `case "$1" in
kind) if test -L "$2"; then echo link; elif test -f "$2"; then echo file; elif test -d "$2"; then echo dir; elif test -e "$2"; then echo other; else echo absent; fi ;;
same) if test "$2" -ef "$3"; then echo same; else echo different; fi ;;
dir) if test -d "$2"; then echo dir; else echo none; fi ;;
read) if test -f "$2"; then echo file; head -c 4096 -- "$2"; else echo none; fi ;;
esac`

// Kind is what path names, as a command in the sandbox sees it, without
// following a link at path itself: absent, link, file, dir or other. It
// reads nothing; pocketd looks at no checkout path with its own rights.
func (e *Engine) Kind(ctx context.Context, path string) (string, error) {
	out, err := e.probe(ctx, "kind", path)
	if err != nil {
		return "", err
	}
	switch kind := strings.TrimSuffix(out, "\n"); kind {
	case "absent", "link", "file", "dir", "other":
		return kind, nil
	}
	return "", fmt.Errorf("look at %s: no answer", path)
}

// Exists says whether path names an entry, a dangling link included, as a
// command in the sandbox sees it (Kind).
func (e *Engine) Exists(ctx context.Context, path string) (bool, error) {
	kind, err := e.Kind(ctx, path)
	return kind != "absent", err
}

// Same says whether a and b name one entry, as a command in the sandbox
// resolves them, bound by ctx.
func (e *Engine) Same(ctx context.Context, a, b string) (bool, error) {
	out, err := e.probe(ctx, "same", a, b)
	switch {
	case err != nil:
		return false, err
	case out == "same\n":
		return true, nil
	case out == "different\n":
		return false, nil
	}
	return false, fmt.Errorf("compare %s and %s: no answer", a, b)
}

// Dir says whether path is a folder, a link to one included, as a command
// in the sandbox sees it.
func (e *Engine) Dir(ctx context.Context, path string) (bool, error) {
	out, err := e.probe(ctx, "dir", path)
	switch {
	case err != nil:
		return false, err
	case out == "dir\n":
		return true, nil
	case out == "none\n":
		return false, nil
	}
	return false, fmt.Errorf("look at %s: no answer", path)
}

// Read is the first 4 KiB of the regular file at path as a command in the
// sandbox reads it; ok is false when path is no regular file there.
func (e *Engine) Read(ctx context.Context, path string) (text string, ok bool, err error) {
	out, err := e.probe(ctx, "read", path)
	switch {
	case err != nil:
		return "", false, err
	case strings.HasPrefix(out, "file\n"):
		return strings.TrimPrefix(out, "file\n"), true, nil
	case out == "none\n":
		return "", false, nil
	}
	return "", false, fmt.Errorf("read %s: no answer", path)
}

func (e *Engine) probe(ctx context.Context, check string, paths ...string) (string, error) {
	cmd, err := e.sandboxed(ctx, append([]string{shell(), "-c", probe, "sh", check}, paths...))
	if err != nil {
		return "", err
	}
	stdout := &capped{max: 4096 + 16}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w", check, strings.Join(paths, " "), err)
	}
	return string(stdout.buf), nil
}

// AddVerdictTool gives team_verdict to session id when it is a thread's
// specialist session started before the tool existed (ADR-0017): its
// tools hold team_message and not team_verdict. The tool goes right
// after team_message, as a new session has it, and every other tool
// stays as it is. It says whether it changed the session.
func (e *Engine) AddVerdictTool(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	message := -1
	for i, tool := range agent.Tools {
		switch tool.Name {
		case toolVerdict:
			return false, nil
		case toolMessage:
			message = i
		}
	}
	if message < 0 {
		return false, nil
	}
	verdict := e.teamTools([]string{toolVerdict})
	list := append(slices.Clone(agent.Tools[:message+1]), verdict...)
	list = append(list, agent.Tools[message+1:]...)
	return true, conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: list})})
}
