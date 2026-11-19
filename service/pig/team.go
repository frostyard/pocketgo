package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Team runs the team tools' work (ADR-0014); api.Server implements it.
// Each method returns the text the calling agent sees, or an error whose
// text it sees instead.
type Team interface {
	TeamDelegate(ctx context.Context, call api.TeamCall, d team.Delegation) (string, error)
	TeamMessage(ctx context.Context, call api.TeamCall, r team.Report) (string, error)
	TeamClose(ctx context.Context, call api.TeamCall, thread int64) (string, error)
}

// SetTeam wires the team tools to t. Call it before Resume, as the other
// receivers are wired: no tool runs before.
func (e *Engine) SetTeam(t Team) { e.team = t }

// The team tools' names.
const (
	toolDelegate = "team_delegate"
	toolMessage  = "team_message"
	toolClose    = "team_close"
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
	`},"required":["question"],"additionalProperties":false}}` +
	`},"required":["message","status"],"additionalProperties":false}`

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
	var out []string
	if spec.Profile != nil && spec.Profile.Role == profiles.RoleSupervisor {
		out = append(out, toolDelegate, toolClose)
	}
	if spec.Thread != 0 {
		out = append(out, toolMessage)
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
// record as a whole one. ctx ends it.
func (e *Engine) Git(ctx context.Context, dir string, args ...string) (string, error) {
	argv := append([]string{"git", "-C", dir}, args...)
	if e.opts.Sandbox != nil {
		wrapped, err := e.opts.Sandbox.Command(argv)
		if err != nil {
			return "", err
		}
		argv = wrapped
	} else if !e.opts.Unsandboxed {
		return "", errors.New("git runs in the sandbox, and there is none")
	}
	//nolint:gosec // G204: git, or bwrap running git, with pocketd's own arguments (worktree.Add).
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	env := maps.Clone(e.commands)
	// git never asks on a terminal: there is none.
	env["GIT_TERMINAL_PROMPT"] = "0"
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	stdout, stderr := &capped{max: gitOutput}, &capped{max: gitDiagnostics}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A hook's own child that keeps the pipes open does not hold pocketd.
	cmd.WaitDelay = gitWait
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	if stdout.dropped > 0 {
		return "", fmt.Errorf("git %s printed more than %d bytes", strings.Join(args, " "), gitOutput)
	}
	return stdout.String(), nil
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
