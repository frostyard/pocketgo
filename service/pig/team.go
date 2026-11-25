package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// Team runs the team tools' work (ADR-0014); api.Server implements it.
// Each method returns the text the calling agent sees, or an error whose
// text it sees instead.
type Team interface {
	// TeamAllows runs before every team tool: an error refuses the call,
	// and its text is what the agent sees (a private agent's session,
	// ADR-0042).
	TeamAllows(ctx context.Context, session int64) error
	TeamDelegate(ctx context.Context, call api.TeamCall, d team.Delegation) (string, error)
	TeamMessage(ctx context.Context, call api.TeamCall, r team.Report) (string, error)
	TeamClose(ctx context.Context, call api.TeamCall, thread int64) (string, error)
	TeamMemory(ctx context.Context, call api.TeamCall, m team.Memory) (string, error)
	TeamVerdict(ctx context.Context, call api.TeamCall, v team.Verdict) (string, error)
	TeamPlan(ctx context.Context, call api.TeamCall, c team.PlanCall) (string, error)
	TeamFinding(ctx context.Context, call api.TeamCall, f team.Finding) (string, error)
	TeamDiff(ctx context.Context, call api.TeamCall, d team.DiffRequest) (string, error)
	TeamWorker(ctx context.Context, call api.TeamCall, w team.WorkerMessage) (string, error)
	TeamSchedule(ctx context.Context, call api.TeamCall, c schedule.Call) (string, error)
	TeamSend(ctx context.Context, call api.TeamCall, m team.Send) (string, error)
	TeamGather(ctx context.Context, call api.TeamCall, g team.Gather) (string, error)
	TeamBoard(ctx context.Context, call api.TeamCall, c team.BoardCall) (string, error)
	TeamPropose(ctx context.Context, call api.TeamCall, c team.ProposeCall) (string, error)
	TeamGitHub(ctx context.Context, call api.TeamCall, r team.GitHubRead) (string, error)
	TeamLaunch(ctx context.Context, call api.TeamCall, c team.LaunchCall) (string, error)
	// Question puts a question to the owner and waits for the answer
	// (ADR-0048). It is no team tool: TeamAllows does not run before it,
	// and it checks its caller itself. ctx ending ends the wait with
	// ctx's error.
	Question(ctx context.Context, call api.TeamCall, q team.Question) (string, error)
	TeamInbox(ctx context.Context, call api.TeamCall) (string, error)
	TeamThreads(ctx context.Context, call api.TeamCall, c team.ThreadsCall) (string, error)
	// TeamWatch keeps the calling session's watches of pull requests
	// (ADR-0073).
	TeamWatch(ctx context.Context, call api.TeamCall, c team.WatchCall) (string, error)
	// TeamWork starts the calling agent's own work session in a fresh
	// worktree of one of its repositories (ADR-0074).
	TeamWork(ctx context.Context, call api.TeamCall, w team.Work) (string, error)
	// Schedule keeps a private agent's own schedules, to the calling
	// session, with team_schedule's engine (ADR-0060). It is no team
	// tool: TeamAllows does not run before it, and it checks its caller
	// itself.
	Schedule(ctx context.Context, call api.TeamCall, c schedule.Call) (string, error)
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
	toolPlan     = "team_plan"
	toolFinding  = "team_finding"
	toolDiff     = "team_diff"
	toolWorker   = "team_worker"
	toolSchedule = schedule.TeamTool
	toolSend     = "team_send"
	toolGather   = "team_gather"
	toolBoard    = "team_board"
	toolPropose  = "team_propose"
	toolGitHub   = "team_github"
	toolLaunch   = "team_launch"
	toolInbox    = "team_inbox"
	toolThreads  = "team_threads"
	toolWatch    = "team_watch"
	toolWork     = "team_work"
)

// proposalProperties are a proposal's fields, as team_propose and
// team_board name them.
const proposalProperties = `"repository":{"type":"string","description":"owner/name of one of the agent's repositories, or none for work in its home; \"\" clears it in an edit."},` +
	`"title":{"type":"string","description":"A short title, at most 120 characters."},` +
	`"task":{"type":"string","description":"The task in full, at most 6000 characters: all the shift's brief will say."},` +
	`"stop_when":{"type":"string","description":"When the work is done, in words that can be checked, at most 3000 characters. Name green CI in it only with ci true: without ci nothing waits for CI."},` +
	`"max_rounds":{"type":"integer","minimum":1,"maximum":100,"description":"At most this many rounds, 1 to 100; 20 when left out of an add."},` +
	`"ci":{"type":"boolean","description":"true when done also needs CI green on the thread's pull requests: pocketd then reads CI itself after the shift's agent says done. false when left out of an add."},` +
	`"note":{"type":"string","description":"A note for the owner, at most 500 characters."}`

const gatherParameters = `{"type":"object","properties":{` +
	`"skill":{"type":"string","description":"The name of the skill that says what to look for, as the owner names it (code-grooming, say): pocketd puts its text in each agent's brief."},` +
	`"agents":{"type":"array","items":{"type":"string"},"description":"The agents to ask, by id; leave it out to ask every agent you can delegate to."},` +
	`"note":{"type":"string","description":"A note to the agents, at most 2000 characters."}` +
	`},"required":["skill"],"additionalProperties":false}`

const proposeParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["add","edit","drop","list","done"],"description":"add a proposal, edit or drop one of yours, list yours, or say you are done."},` +
	`"row":{"type":"integer","description":"edit, drop: the proposal's number, as list shows it."},` +
	`"rev":{"type":"integer","description":"edit, drop: its rev, as list shows it."},` +
	`"page":{"type":"integer","minimum":1,"description":"list: the page, from 1; a long list comes in pages."},` +
	proposalProperties +
	`},"required":["action"],"additionalProperties":false}`

const githubParameters = `{"type":"object","properties":{` +
	`"path":{"type":"string","description":"The REST endpoint's path, with no host and no query: repos/<owner>/<name>/issues, repos/<owner>/<name>/pulls/12/files, search/issues. Letters, digits and . _ ~ @ + - in each segment."},` +
	`"query":{"type":"object","additionalProperties":{"type":"string"},"description":"Query parameters, by name: {\"state\": \"open\", \"per_page\": \"30\", \"page\": \"2\"}, or {\"q\": \"repo:<owner>/<name> is:open label:bug\"} for search/issues. pocketd encodes them."}` +
	`},"required":["path"],"additionalProperties":false}`

const launchParameters = `{"type":"object","properties":{` +
	`"rows":{"type":"array","items":{"type":"integer"},"description":"The proposals of your board to launch, by number, in the order they start: 1 to 20."},` +
	`"hours":{"type":"number","description":"How long their threads run, from now: above 0 and at most 24."},` +
	`"until":{"type":"string","description":"Or the clock time they run until, HH:MM on the 24-hour clock, the next such time. Neither is until the next ` + team.DefaultLaunchUntil + `."}` +
	`},"required":["rows"],"additionalProperties":false}`

const workParameters = `{"type":"object","properties":{` +
	`"repository":{"type":"string","description":"owner/name of one of your repositories, as your profile names it; leave it out when you have one."},` +
	`"task":{"type":"string","description":"The task in full, at most 6000 characters: the work session sees only this, its folder and its instructions."},` +
	`"title":{"type":"string","description":"A short title, at most 120 characters. Default: the task's first line."}` +
	`},"required":["task"],"additionalProperties":false}`

// inboxParameters are team_inbox's: none.
const inboxParameters = `{"type":"object","properties":{},"additionalProperties":false}`

// threadsParameters are team_threads' (ADR-0072).
var threadsParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["list","read"],"description":"list every thread not closed with who moves it next, and the closed threads whose records changed in the last ` +
	strconv.Itoa(team.InboxDays) + ` days; or read one thread whole."},` +
	`"thread":{"type":"integer","minimum":1,"description":"read: the thread's number, 5 for T5."}` +
	`},"required":["action"],"additionalProperties":false}`

const boardParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["list","read","edit","order","drop"],"description":"list the board's proposals, read one whole, edit or drop one at its rev, or order them."},` +
	`"page":{"type":"integer","minimum":1,"description":"list: the page, from 1; a long board comes in pages."},` +
	`"row":{"type":"integer","description":"read, edit, drop: the proposal's number."},` +
	`"rev":{"type":"integer","description":"edit, drop: its rev, as list shows it."},` +
	`"rows":{"type":"array","items":{"type":"integer"},"description":"order: proposals' numbers, in the order you want them first on the board."},` +
	proposalProperties +
	`},"required":["action"],"additionalProperties":false}`

const scheduleParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["add","list","cancel"],"description":"add a schedule, list this session's, or cancel one of yours."},` +
	`"when":{"type":"string","description":"add: when it goes out, as in 30m, tomorrow 9:00, fri 17:30, every 2h, every day 19:00, every weekday 8:00 or every mon,thu 9:00."},` +
	`"message":{"type":"string","description":"add: what this session gets then, at most 4000 characters; write it so it stands on its own."},` +
	`"id":{"type":"string","description":"cancel: the schedule's id, as S3, from add or list."},` +
	`"page":{"type":"integer","minimum":1,"description":"list: the page, from 1; a long list comes in pages, and each says whether one follows."}` +
	`},"required":["action"],"additionalProperties":false}`

const workerParameters = `{"type":"object","properties":{` +
	`"message":{"type":"string","description":"The task for your worker, in full: it sees only this, its folder (yours) and its tools. The first message starts your worker; later ones go to the same worker, with what to fix. At most 20000 characters."}` +
	`},"required":["message"],"additionalProperties":false}`

const diffParameters = `{"type":"object","properties":{` +
	`"paths":{"type":"array","maxItems":20,"items":{"type":"string"},"description":"Only these files or folders, as paths in your folder. Leave it out for the whole change set."},` +
	`"stat":{"type":"boolean","description":"Only the stat: which files changed, and how much."},` +
	`"log":{"type":"boolean","description":"The commits, each with its patch, instead of one diff."},` +
	`"since_last_review":{"type":"boolean","description":"For a re-review: from the head the last review read, instead of the base."},` +
	`"offset":{"type":"integer","minimum":0,"description":"The byte to start at: an answer that holds part of the change set says where the next part starts."}` +
	`},"additionalProperties":false}`

const delegateParameters = `{"type":"object","properties":{` +
	`"agent":{"type":"string","description":"The id of the specialist's profile, as your instructions list it."},` +
	`"task":{"type":"string","description":"What the specialist is to do, in full: it sees only this and its own profile. At most 6000 characters."},` +
	`"title":{"type":"string","description":"A short title for the thread, at most 120 characters. Default: the task's first line."},` +
	`"repository":{"type":"string","description":"owner/name of a repository the specialist works on: it works in a new git worktree of it, on a branch of its own. Leave it out for work in the specialist's home."},` +
	`"plan":{"type":"object","description":"Plan work: {\"start\": true} starts a plan, led by the agent, in its home, with the task as the idea to plan; {\"id\": 1, \"task\": \"t2\"} delivers a ready task of an approved plan, to the task's agent and repository.","properties":{` +
	`"start":{"type":"boolean","description":"true to start a plan."},` +
	`"id":{"type":"integer","description":"The plan's number: 1 for P1."},` +
	`"task":{"type":"string","description":"The task's id in the plan, as t2."}` +
	`},"additionalProperties":false},` +
	`"worker":{"type":"string","description":"Only when the owner named a worker model for this work: its provider (as deepseek) or provider/id, an entry of the owner's worker list. Every worker of the thread runs it, over the specialist's profile's worker. Leave it out otherwise: the profile's worker, else the first model pocketd runs of the owner's list, runs."},` +
	`"follows":{"type":"integer","description":"When this thread carries on the work of a thread you delegated, closed or about to close, as the same pull request: that thread's number, 12 for T12. The older thread then names this one as its successor, so a late message to it, and its specialist, are pointed here. Leave it out otherwise."},` +
	`"shift":{"type":"object","description":"Run the thread as a shift: pocketd sends the specialist round after round toward the task until the stop condition holds, a round reports blocked, or the round limit or deadline is reached, then asks for one report. For long work the owner asks to run as a shift; leave it out otherwise.","properties":{` +
	`"stop":{"type":"string","description":"When the work is done, in words the specialist can check, at most 3000 characters. Name green CI in it only with ci true: without ci nothing waits for CI."},` +
	`"max_rounds":{"type":"integer","description":"At most this many rounds, 1 to 100; 20 when left out."},` +
	`"hours":{"type":"number","description":"Stop after this many hours, above 0 and at most 24; no deadline when left out."},` +
	`"ci":{"type":"boolean","description":"true when done also needs CI green on the pull requests the specialist reports for the thread."}` +
	`},"required":["stop"],"additionalProperties":false}` +
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

const planParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["read","write","tasks","ready","consult"],"description":"read the plan, write its text, draft its tasks, ask for the owner's approval (ready), or consult other specialists."},` +
	`"page":{"type":"integer","description":"read: the page, from 1; a long plan reads in pages, and each page says whether one follows."},` +
	`"snapshot":{"type":"string","description":"read, past page 1: the snapshot page 1 named."},` +
	`"text":{"type":"string","description":"write: the plan's whole new text, at most 20000 characters."},` +
	`"rev":{"type":"string","description":"write: the rev team_plan read shows, which you wrote from; none for the first write."},` +
	`"asks":{"type":"array","items":{"type":"integer"},"description":"write: the answered asks this write puts into the plan, by number: 3 for A3."},` +
	`"tasks":{"type":"array","maxItems":30,"description":"tasks: the plan's tasks, 1 to 30, each a pull request's worth of work for one agent.","items":{"type":"object","properties":{` +
	`"id":{"type":"string","description":"t1 to t999, unique in the plan."},` +
	`"title":{"type":"string","description":"At most 120 characters."},` +
	`"agent":{"type":"string","description":"The id of the specialist that does it."},` +
	`"repository":{"type":"string","description":"owner/name of one of the agent's repositories; leave it out for work in its home."},` +
	`"acceptance":{"type":"array","items":{"type":"string"},"description":"1 to 8 checks its work must pass, each at most 300 characters."},` +
	`"deps":{"type":"array","items":{"type":"string"},"description":"The ids of the tasks that must be done first."}` +
	`},"required":["id","title","agent","acceptance"],"additionalProperties":false}},` +
	`"agents":{"type":"array","items":{"type":"string"},"maxItems":3,"description":"consult: 1 to 3 specialists' ids, not yours."},` +
	`"question":{"type":"string","description":"consult: what they are asked, at most 2000 characters."},` +
	`"ask":{"type":"integer","description":"consult: the plan's ask it is on, by number: 3 for A3."}` +
	`},"required":["action"],"additionalProperties":false}`

const findingParameters = `{"type":"object","properties":{` +
	`"stance":{"type":"string","enum":["supports","objects","neutral"],"description":"Whether you support what the question asks about, object to it, or neither."},` +
	`"summary":{"type":"string","description":"Your answer and why, at most 2000 characters."}` +
	`},"required":["stance","summary"],"additionalProperties":false}`

const closeParameters = `{"type":"object","properties":{` +
	`"thread":{"type":"integer","description":"The thread's number: 12 for T12."}` +
	`},"required":["thread"],"additionalProperties":false}`

var watchParameters = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["add","list","cancel"],"description":"add a watch, list this session's, or cancel one of yours."},` +
	`"pull":{"type":"string","description":"add: the pull request, as owner/name#number."},` +
	`"until":{"type":"string","enum":["checks","merged","closed"],"description":"add: what to wait for. checks: CI at the pull request's head is done, green, failing or none; merged: it merged; closed: it merged or was closed without merging."},` +
	`"hours":{"type":"number","description":"add: how long to wait at most, above 0 and at most ` + strconv.Itoa(team.MaxWatchHours) + `; ` + strconv.Itoa(team.DefaultWatchHours) + ` when left out. A few hours suit checks."},` +
	`"id":{"type":"string","description":"cancel: the watch's id, as W3, from add or list."}` +
	`},"required":["action"],"additionalProperties":false}`

const sendParameters = `{"type":"object","properties":{` +
	`"thread":{"type":"integer","description":"The thread's number: 12 for T12. A thread you delegated: open, done, blocked, or closed, which the message reopens."},` +
	`"text":{"type":"string","description":"The message for the thread's specialist, in full, at most 6000 characters: it reaches its session as a message from you."}` +
	`},"required":["thread","text"],"additionalProperties":false}`

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
				// The plan rule is in the tool's description, which a session
				// reads from this registration, so a supervisor started before
				// plans has it too (Codex review of #25, round 8).
				ToolSchema: schema(toolDelegate, "Delegate a task to a specialist in a new thread. pocketd checks the rules, makes the specialist's worktree, "+
					"starts its session and sends it a brief. The specialist's reports come back to you as messages. "+team.PlanRoster(), delegateParameters),
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
				ToolSchema: schema(toolClose, "Close a thread you delegated, which frees its place on the desk. Closing a plan's own thread drops the plan, "+
					"and frees no place: a plan's thread takes none.", closeParameters),
				Replay: durable.ReplaySafe,
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
				ToolSchema: schema(toolPlan, "Lead your plan: read it, write its whole text from the rev you read, draft its tasks, consult other specialists, "+
					"and call ready when nothing is open. pocketd checks every rule; the owner approves the plan.", planParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[team.PlanCall](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamPlan(ctx, call, c) })
				},
			},
			{
				ToolSchema: schema(toolFinding, "File your one finding as a consult: your stance and a summary. It goes to the plan's lead.", findingParameters),
				Replay:     durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					f, err := durable.FromJsonValue[team.Finding](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamFinding(ctx, call, f) })
				},
			},
			{
				ToolSchema: schema(toolDiff, "Read the change set of the pull request you review: git diff from its base to its head, run by pocketd in your folder. "+
					"Ask for the stat first on a large change, then name paths.", diffParameters),
				Replay: durable.ReplaySafe,
				// A whole page and its footer reach the reviewer: PiG's default
				// line limit would cut a page of short lines, and its next
				// offset with it (Codex review of #26, round 3).
				OutputLimits: &durable.ToolOutputLimits{MaxBytes: ptr(team.DiffPage + 1024), MaxLines: ptr(team.DiffPage + 16)},
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					d, err := durable.FromJsonValue[team.DiffRequest](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamDiff(ctx, call, d) })
				},
			},
			{
				ToolSchema: schema(toolWorker, "Give work to your thread's worker, on a cheaper model, in your folder: the first call starts it with your brief, later calls "+
					"send it fixes. Its reply comes back to you as a message; check its work before you report.", workerParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					w, err := durable.FromJsonValue[team.WorkerMessage](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamWorker(ctx, call, w) })
				},
			},
			{
				ToolSchema: schema(toolSchedule, scheduleDescription, scheduleParameters),
				Replay:     durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[schedule.Call](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamSchedule(ctx, call, c) })
				},
			},
			{
				// After team_schedule, as roleTools and laterSupervisorTools order
				// them: a session's tools follow this list's order (teamTools).
				ToolSchema: schema(toolSend, "Send a message to the specialist of a thread you delegated, open, done, blocked or closed: new information, a finding to fix, "+
					"the owner's decision. pocketd delivers it to the specialist's session once, after its current turn when it is at work, and a done, blocked or closed thread is open "+
					"again: a blocked thread's asks that wait for the owner are withdrawn. A closed thread whose plan settled it, or whose worktree is gone, takes none: the refusal "+
					"names the thread that carries its work on. The answer says whether it was delivered or is queued, and which asks it withdrew; "+
					"the specialist's next report goes to the session that delegated the thread.", sendParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					m, err := durable.FromJsonValue[team.Send](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamSend(ctx, call, m) })
				},
			},
			{
				ToolSchema: schema(toolGather, "Gather proposals from the team: pocketd asks each agent, in a read-only session of its own, for work it could do as a shift, "+
					"with the text of the skill you name. You hear once their proposals are in; the owner picks from the board, or tells you which to launch.", gatherParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					g, err := durable.FromJsonValue[team.Gather](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamGather(ctx, call, g) })
				},
			},
			{
				ToolSchema: schema(toolBoard, "Keep the board of proposals the team's gathers filed: list them, read one whole, edit or drop one at its rev, "+
					"and order them, the best first. The owner picks from it.", boardParameters),
				Replay:       durable.ReplaySafe,
				OutputLimits: &durable.ToolOutputLimits{MaxBytes: ptr(team.MaxBoardBytes), MaxLines: ptr(team.MaxBoardLines)},
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[team.BoardCall](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamBoard(ctx, call, c) })
				},
			},
			{
				ToolSchema: schema(toolLaunch, "Launch proposals of your board: pocketd starts them in order as shift threads delegated from your standing session, "+
					"until the end you name, a few at once while the desk keeps places free, and their reports come to it. Launch only what the owner picked or asked for.", launchParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[team.LaunchCall](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamLaunch(ctx, call, c) })
				},
			},
			{
				// After team_launch, as roleTools and laterSupervisorTools order
				// them (ADR-0046). The rule to check it first is in its
				// description too, which a session reads from this
				// registration, so a supervisor started before the line in its
				// instructions has it.
				ToolSchema: schema(toolInbox, fmt.Sprintf("See the owner's Inbox: what waits for the owner now (open asks, pull requests to review with their "+
					"ready-for-human label, verdict and CI, plans to approve, lessons, tool calls), and what was settled in the last %d days (asks answered or "+
					"withdrawn, pull requests merged or closed). It reads pocketd's records alone, never GitHub, and changes nothing; each pull request says "+
					"when pocketd last read it. Check it before you remind the owner of an ask or a pull request.", team.InboxDays), inboxParameters),
				Replay: durable.ReplaySafe,
				// A whole answer reaches the supervisor: team.InboxText keeps it
				// within these bounds, cut as it says.
				OutputLimits: &durable.ToolOutputLimits{MaxBytes: ptr(team.MaxInboxBytes), MaxLines: ptr(team.MaxInboxLines)},
				Execute: func(ctx context.Context, _ any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamInbox(ctx, call) })
				},
			},
			{
				// After team_inbox, as roleTools and laterSupervisorTools order
				// them (ADR-0072). The rule to read a thread before asking where
				// it stands is in its description too, for a supervisor started
				// before the line in its instructions.
				ToolSchema: schema(toolThreads, fmt.Sprintf("See the threads: list every thread not closed, grouped by who moves it next as the owner's work board "+
					"groups it (the owner, its specialist or its supervisor), each with its state, shift, open asks, pull requests with CI and verdict, and "+
					"who delegated it, then the cross-reviews pocketd runs and the closed threads whose records changed in the last %d days; or read one thread whole, with its task, "+
					"latest report, asks and answers, pull requests, shift rounds, plan and worker. It reads pocketd's records alone, never GitHub, and "+
					"changes nothing. Read a thread before you ask the owner or a specialist where it stands.", team.InboxDays), threadsParameters),
				Replay: durable.ReplaySafe,
				// A whole answer reaches the supervisor: team.ThreadsText and
				// team.ThreadText keep it within these bounds, cut as they say.
				OutputLimits: &durable.ToolOutputLimits{MaxBytes: ptr(team.MaxThreadsBytes), MaxLines: ptr(team.MaxThreadsLines)},
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[team.ThreadsCall](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamThreads(ctx, call, c) })
				},
			},
			{
				ToolSchema: schema(toolPropose, "File your proposals for the supervisor's gather: add one, edit or drop one of yours at its rev, list yours, "+
					"and say done when you have no more.", proposeParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[team.ProposeCall](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamPropose(ctx, call, c) })
				},
			},
			{
				// After team_propose: a session's team tools follow this list's
				// order (teamTools). A proposal session's, a consult's, a
				// plan's lead's and a cross-review's (roleTools, ADR-0036,
				// ADR-0057).
				ToolSchema: schema(toolGitHub, "Read GitHub with a GET call of its REST API, with the service user's sign-in: issues, pull requests, "+
					"comments, checks, files. It changes nothing on GitHub. A long answer is cut, and says so: ask for fewer with per_page, then a later page.", githubParameters),
				Replay: durable.ReplaySafe,
				// A whole answer and its note reach the agent, whatever its shape:
				// compacted JSON is one line, which PiG drops whole past its byte
				// limit, and text of many short lines passes PiG's line limit.
				OutputLimits: &durable.ToolOutputLimits{MaxBytes: ptr(team.MaxGitHubAnswer + team.MaxGitHubNote), MaxLines: ptr(team.MaxGitHubAnswer + 16)},
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					r, err := durable.FromJsonValue[team.GitHubRead](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamGitHub(ctx, call, r) })
				},
			},
			{
				// Before team_memory: a session's team tools follow this
				// list's order (teamTools). An agent's own session's alone
				// (roleTools, team.MayWork); the rule to change a repository
				// from a work session is in its description too, which an
				// older session reads from this registration (ADR-0074).
				ToolSchema: schema(toolWork, "Start a work session of yours in a fresh git worktree of one of your repositories, on a new branch, with the task "+
					"as its first message: that repository's own AGENTS.md and skills load there. In a session at your home, change a repository through a work session, "+
					"never in your home; a session the owner opened in a repository works there. "+
					"Each answer of the work session comes back to this session as a message. Refused in a work session: report to the session that started you.", workParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					w, err := durable.FromJsonValue[team.Work](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamWork(ctx, call, w) })
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
			{
				// After team_memory: a session given it later gets it last
				// among its team tools (SetWatchTool), as a new one has it
				// (ADR-0073). How to wait is in its description, which a
				// session started before the guide in its brief reads too.
				ToolSchema: schema(toolWatch, "Have pocketd tell you when a pull request reaches a condition: its checks are done, it merged, or it closed. "+
					"The call returns at once. Only an answer that starts with \"Watching W\" promises a message: end your run then. pocketd checks the pull "+
					"request on GitHub itself, in each of its rounds, and sends this session one message when the condition holds, when it no longer can, "+
					"or when the time is up, which starts your next run. Any other answer (the pull request is there already, or a refusal) comes with "+
					"no message: act on it now. Never sleep or poll for CI or a merge: watch it. list shows your watches, and cancel ends one.", watchParameters),
				Replay: durable.ReplaySafe,
				Execute: func(ctx context.Context, args any, api durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
					c, err := durable.FromJsonValue[team.WatchCall](args)
					if err != nil {
						return toolText("", err), nil
					}
					return e.runTeam(ctx, api, func(t Team, call apiCall) (string, error) { return t.TeamWatch(ctx, call, c) })
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
	if err := e.team.TeamAllows(ctx, int64(call.ConversationId())); err != nil {
		return toolText("", err), nil
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
// supervisor delegates, closes, keeps its schedules (ADR-0030) and sends
// to its threads (ADR-0032); a thread's specialist reports; a plan's
// lead plans too; a consult files its finding, and nothing else
// (ADR-0020); a gather's agent files its proposals, and nothing else
// (ADR-0033). A consult, a gather's agent, a plan's lead and a
// cross-review read GitHub with GET alone, too (team_github, ADR-0036,
// ADR-0057), and no other session does. A supervisor alone reads the
// owner's Inbox (team_inbox, ADR-0046) and the threads (team_threads,
// ADR-0072). Every session that keeps lessons but a shift's specialist
// watches pull requests (team_watch, ADR-0073). An agent's own session
// starts its work sessions (team_work, team.MayWork, ADR-0074). A private
// agent has no team tool at all (ADR-0042): it keeps its own schedules
// with the schedule tool, which is none (toolsOf, ADR-0060).
func roleTools(spec api.SessionSpec) []string {
	// A private agent has no team: no team tool, team_memory and
	// team_schedule included, whatever its role (ADR-0042, ADR-0060).
	if spec.Profile != nil && spec.Profile.Private {
		return nil
	}
	// A consult reads files and GitHub, and files its finding: nothing
	// else, whatever its profile or its other roles (ADR-0020, ADR-0036).
	if spec.Consult != 0 {
		return []string{toolFinding, toolGitHub}
	}
	// A gather's agent reads files and GitHub, and files its proposals:
	// nothing else (ADR-0033, ADR-0036).
	if spec.Gather != 0 {
		return []string{toolPropose, toolGitHub}
	}
	// A cross-review reads its change set and GitHub, with GET alone, and
	// files its verdict: nothing else (ADR-0019, ADR-0057). What a pin
	// bump brings in is in another repository, which team_diff does not
	// show.
	if spec.Review {
		return []string{toolDiff, toolVerdict, toolGitHub}
	}
	// A worker has no team tool: it answers its specialist alone (ADR-0021).
	if spec.Worker {
		return nil
	}
	var out []string
	if spec.Profile != nil && spec.Profile.Role == profiles.RoleSupervisor {
		// A supervisor keeps its own reminders and repeats too (ADR-0030),
		// sends to the threads it delegated (ADR-0032), gathers proposals
		// and keeps their board (ADR-0033), launches them (ADR-0034), reads
		// the owner's Inbox (ADR-0046) and reads the threads (ADR-0072).
		out = append(out, toolDelegate, toolClose)
		out = append(out, laterSupervisorTools...)
	}
	switch {
	case spec.Thread != 0 && spec.Plan != 0:
		// A plan's lead reports and leads its plan, reads GitHub, and
		// builds nothing: no worker (ADR-0020, ADR-0036). A plan task's
		// thread is a thread like any.
		out = append(out, toolMessage, toolPlan, toolGitHub)
	case spec.Thread != 0:
		// A thread's specialist reports, and has a worker (ADR-0021).
		out = append(out, toolMessage, toolWorker)
	}
	// An agent's own session starts work sessions in fresh worktrees of
	// its repositories; a work session itself does not (ADR-0074).
	if team.MayWork(spec.Profile, roleOf(spec)) {
		out = append(out, toolWork)
	}
	if spec.Profile != nil {
		out = append(out, toolMemory)
	}
	// Every session that keeps lessons watches pull requests too, but a
	// shift's specialist, whose rounds read CI in code (team.MayWatch,
	// ADR-0073).
	if team.MayWatch(spec.Profile, roleOf(spec)) {
		out = append(out, toolWatch)
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
	stdout := &capped{max: gitOutput}
	if err := e.runGit(ctx, dir, stdout, args); err != nil {
		return "", err
	}
	if stdout.dropped > 0 {
		return "", fmt.Errorf("git %s printed more than %d bytes", strings.Join(args, " "), gitOutput)
	}
	return strings.TrimSuffix(string(stdout.buf), "\n"), nil
}

// GitPage runs git as Git does, and keeps the limit bytes of its output
// from offset on: a cross-review pages through a patch larger than Git's
// bound (ADR-0019; Codex review of #26, round 2). git is stopped once the
// page is full, so no size of the whole is needed, and more says output
// went on past the page (round 3); ctx bounds the time.
func (e *Engine) GitPage(ctx context.Context, dir string, offset, limit int, args ...string) (string, bool, error) {
	if offset < 0 || limit <= 0 || limit > gitOutput {
		return "", false, fmt.Errorf("a page is 1 to %d bytes from an offset of 0 or more", gitOutput)
	}
	page := &window{from: int64(offset), keep: limit}
	if err := e.runGit(ctx, dir, page, args); err != nil && !page.more {
		return "", false, err
	}
	// A page cut inside a character ends before it: the next offset starts
	// at it, so no character is split between two answers (Codex review of
	// #26, round 4).
	buf, more := page.buf, page.more
	if more {
		if cut := wholeRunes(buf); cut < len(buf) {
			buf = buf[:cut]
		}
	}
	return string(buf), more, nil
}

// wholeRunes is how many bytes of b end at a whole UTF-8 character: b
// less a character it ends inside.
func wholeRunes(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			return len(b)
		}
	}
	return len(b)
}

// GitEach runs git as Git does, and gives each record of its output,
// split at sep, to each, keeping no more than one record: a listing of
// every tracked file of a large repository is read with bounded memory
// (worktree.Each; Codex review of #26, round 5). A record past gitOutput
// bytes is an error; an error of each stops git and comes back.
func (e *Engine) GitEach(ctx context.Context, dir string, sep byte, each func(string) error, args ...string) error {
	split := &splitter{sep: sep, each: each}
	err := e.runGit(ctx, dir, split, args)
	if split.err != nil {
		return split.err
	}
	if err != nil {
		return err
	}
	if len(split.buf) > 0 {
		return each(string(split.buf))
	}
	return nil
}

// splitter calls each with every record written to it, split at sep.
type splitter struct {
	sep  byte
	each func(string) error
	buf  []byte
	err  error
}

func (s *splitter) Write(p []byte) (int, error) {
	for i, b := range p {
		if b != s.sep {
			if len(s.buf) >= gitOutput {
				s.err = fmt.Errorf("a record of git's output is longer than %d bytes", gitOutput)
				return i, s.err
			}
			s.buf = append(s.buf, b)
			continue
		}
		if s.err = s.each(string(s.buf)); s.err != nil {
			return i, s.err
		}
		s.buf = s.buf[:0]
	}
	return len(p), nil
}

// errPageFull stops git's output once a page is full.
var errPageFull = errors.New("the page is full")

// window keeps keep bytes of what is written from byte from on, and stops
// the writer, with more set, at the first byte past them.
type window struct {
	from  int64
	keep  int
	buf   []byte
	total int64
	more  bool
}

func (w *window) Write(p []byte) (int, error) {
	start := w.total
	w.total += int64(len(p))
	if w.total <= w.from {
		return len(p), nil
	}
	lo := max(w.from-start, 0)
	hi := min(lo+int64(w.keep-len(w.buf)), int64(len(p)))
	w.buf = append(w.buf, p[lo:hi]...)
	if hi < int64(len(p)) {
		w.more = true
		return int(hi), errPageFull
	}
	return len(p), nil
}

// runGit runs git with args in dir, in the sandbox, its output to stdout;
// an error says it did not run or did not succeed.
func (e *Engine) runGit(ctx context.Context, dir string, stdout io.Writer, args []string) error {
	// git's exit status comes back on a file of its own, so a sandbox that
	// failed around git (a mount, a missing git) is never read as git's
	// answer (worktree.GitExit).
	// The program is one only root can change: its answers are a review's
	// evidence, and its checks guard what pocketd hands an agent, so no git
	// an agent could put on the PATH runs (Codex review of #26, round 9).
	program := e.opts.Git
	if program == "" {
		var err error
		if program, err = trustedGit(); err != nil {
			return fmt.Errorf("git %s did not run: %w", strings.Join(args, " "), err)
		}
	}
	status, err := os.CreateTemp("", "pocketd-git-")
	if err != nil {
		return err
	}
	_ = os.Remove(status.Name())
	defer func() { _ = status.Close() }()
	cmd, err := e.sandboxed(ctx, append([]string{shell(), "-c", gitScript, "sh", program, "-C", dir}, args...))
	if err != nil {
		return err
	}
	stderr := &capped{max: gitDiagnostics}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{status}
	runErr := cmd.Run()
	ran, code := gitStatus(status)
	name := strings.Join(args, " ")
	switch {
	case ran && code != 0:
		return fmt.Errorf("git %s: %w: %s", name, &worktree.GitExit{Status: code}, stderr.String())
	case !ran:
		if runErr == nil {
			runErr = errors.New("it left no exit status")
		}
		return fmt.Errorf("git %s did not run: %w: %s", name, runErr, stderr.String())
	case runErr != nil:
		return fmt.Errorf("git %s: %w: %s", name, runErr, stderr.String())
	}
	return nil
}

// gitScript runs the git program its first argument names with the rest,
// with file descriptor 3 closed for git and what it starts (a hook), then
// writes git's exit status there: only the shell around git writes it,
// after git ran.
const gitScript = `git=$1; shift; "$git" "$@" 3>&-; echo "$?" >&3`

// gitPaths are where a system installs git. The PATH is never searched: a
// folder on it the service user owns could hold a fake git, whose answers
// pocketd would take for the checkout's (Codex review of #26, round 9).
var gitPaths = []string{"/usr/bin/git", "/bin/git", "/usr/local/bin/git"}

// trustedGit is the first installed git that only root can change, along
// every name that leads to it (trustedProgram).
func trustedGit() (string, error) { return trustedIn("git", gitPaths) }

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
func trustedGH() (string, error) { return trustedIn("gh", ghPaths) }

// trustedIn is the first of paths, where a system installs name, that
// only root can change, along every name that leads to it
// (trustedProgram).
func trustedIn(name string, paths []string) (string, error) {
	var problems []string
	for _, candidate := range paths {
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
		return "", fmt.Errorf("no %s only root can change is installed: %s", name, strings.Join(problems, "; "))
	}
	return "", fmt.Errorf("%s is not installed in %s", name, strings.Join(paths, ", "))
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

// gh runs program with args, and env past a tool command's. gh is stopped
// at the first byte of output past ghOutput: an answer that large is an
// error already, so gh does not download the rest (Codex review of #61,
// round 1).
func (e *Engine) gh(ctx context.Context, program string, env []string, args ...string) (string, error) {
	run, stop := context.WithCancel(ctx)
	defer stop()
	cmd, err := e.sandboxed(run, append([]string{program}, args...))
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Dir = "/"
	stdout, stderr := &bounded{max: ghOutput, stop: stop}, &capped{max: gitDiagnostics}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	name := strings.Join(args, " ")
	err = cmd.Run()
	switch {
	case stdout.over:
		return "", fmt.Errorf("gh %s printed more than %d bytes", name, ghOutput)
	case err != nil:
		return "", fmt.Errorf("gh %s: %w: %s", name, err, stderr.String())
	}
	return string(stdout.buf), nil
}

// errOutputFull stops a command whose output passed its bound.
var errOutputFull = errors.New("the output passed its bound")

// bounded keeps at most max bytes written to it. The first byte past them
// stops the command (stop) and the writer, with over set, so a command
// that prints without end ends at once.
type bounded struct {
	max  int
	buf  []byte
	over bool
	stop func()
}

func (b *bounded) Write(p []byte) (int, error) {
	if b.over {
		return 0, errOutputFull
	}
	if len(b.buf)+len(p) <= b.max {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	n := b.max - len(b.buf)
	b.buf = append(b.buf, p[:n]...)
	b.over = true
	b.stop()
	return n, errOutputFull
}

// sandboxed is the command that runs argv in the sandbox (ADR-0012), with
// the environment a tool's command gets (allowedEnv), never pocketd's own:
// a provider key in pocketd's environment never reaches it (protocol rule
// 14c). Only a test, Unsandboxed, runs it outside.
func (e *Engine) sandboxed(ctx context.Context, argv []string) (*exec.Cmd, error) {
	if e.opts.Sandbox != nil {
		wrapped, err := e.opts.Sandbox.CommandContext(ctx, argv)
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

// PlacedEntry is the transcript entry of session's submission of request
// key, once a run placed it (ADR-0024).
func (e *Engine) PlacedEntry(ctx context.Context, session int64, key string) (int64, bool, error) {
	record, err := e.storage.SubmissionByRequest(ctx, durable.ConversationId(session), key)
	if err != nil || record == nil || record.Entry == nil {
		return 0, false, err
	}
	return int64(*record.Entry), true, nil
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

// Idle says session has no run and nothing waiting in its pi.inbox: a
// submission now starts a run with it at once, never waits in a queue
// (ADR-0024).
func (e *Engine) Idle(ctx context.Context, session int64) (bool, error) {
	conversation := durable.ConversationId(session)
	live, err := durable.Snapshot[harness.LiveState](ctx, e.harness, harness.LiveDoc, conversation)
	if err != nil || live != nil && live.Run != nil {
		return false, err
	}
	inbox, err := durable.Snapshot[harness.InboxState](ctx, e.harness, harness.InboxDoc, conversation)
	if err != nil {
		return false, err
	}
	return inbox == nil || len(inbox.Items) == 0, nil
}

// probe is the shell script of the sandbox's checks: it prints exactly
// one answer, so an exit or an output of anything else (bwrap failing to
// make the sandbox, say) is an error, never an answer.
const probe = `case "$1" in
kind) if test -L "$2"; then echo link; elif test -f "$2"; then echo file; elif test -d "$2"; then echo dir; elif test -e "$2"; then echo other; else echo absent; fi ;;
same) if test "$2" -ef "$3"; then echo same; else echo different; fi ;;
dir) if test -d "$2"; then echo dir; else echo none; fi ;;
read) if test -f "$2"; then echo file; head -c 4096 -- "$2"; else echo none; fi ;;
real) if cd -P -- "$2" 2>/dev/null; then echo real; pwd -P; else echo none; fi ;;
mkdir) if "$3" -- "$2" 2>/dev/null; then echo made; elif test -e "$2" || test -L "$2"; then echo there; else echo failed; fi ;;
esac`

// mkdirPaths are where a system installs mkdir. The PATH is never
// searched: a folder on it the service user owns could hold a fake mkdir,
// whose success pocketd would take for a folder its request made (Codex
// review of #46, round 5).
var mkdirPaths = []string{"/usr/bin/mkdir", "/bin/mkdir"}

// trustedMkdir is the first installed mkdir that only root can change,
// along every name that leads to it (trustedProgram).
func trustedMkdir() (string, error) { return trustedIn("mkdir", mkdirPaths) }

// MakeDir makes the folder path, and only it, as a command in the sandbox,
// with a mkdir only root can change: made is true when this call made it,
// false when an entry was there already (mkdir fails then, so of two that
// race for one path one makes it). A folder it cannot make for another
// reason is an error. A session's new worktree takes its folder so
// (ADR-0027).
func (e *Engine) MakeDir(ctx context.Context, path string) (bool, error) {
	program, err := trustedMkdir()
	if err != nil {
		return false, err
	}
	out, err := e.probe(ctx, "mkdir", path, program)
	switch {
	case err != nil:
		return false, err
	case out == "made\n":
		return true, nil
	case out == "there\n":
		return false, nil
	}
	return false, fmt.Errorf("make the folder %s: it could not be made", path)
}

// Real is the folder path names as a command in the sandbox resolves it,
// every link on its way followed (the shell's cd -P and pwd -P, no program
// found on the PATH); ok is false when path names no folder there. A
// session's new worktree is made at its folder's real place, so a link
// that moves later moves nothing it made (ADR-0027; Codex review of #46,
// round 5).
func (e *Engine) Real(ctx context.Context, path string) (string, bool, error) {
	out, err := e.probe(ctx, "real", path)
	switch {
	case err != nil:
		return "", false, err
	case out == "none\n":
		return "", false, nil
	}
	real, ok := strings.CutPrefix(out, "real\n")
	if real, ended := strings.CutSuffix(real, "\n"); ok && ended && strings.HasPrefix(real, "/") {
		return real, true, nil
	}
	return "", false, fmt.Errorf("resolve %s: no answer", path)
}

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

// ReadUpTo is the regular file at path as a command in the sandbox reads
// it, its links followed as the sandbox sees them: at most max+1 bytes, so
// the caller sees a file larger than max; ok is false when path is no
// regular file there, a named pipe included. pocketd reads a gather's
// skill so, never with its own rights: a link an agent put in the skills
// folder leads nowhere the agents' own tools could not read (Codex review
// of #57, round 1). The read is pocketd's own ReadHelper, run in the
// sandbox, which checks the type on the descriptor it reads (round 4).
func (e *Engine) ReadUpTo(ctx context.Context, path string, max int) ([]byte, bool, error) {
	if max < 0 {
		return nil, false, fmt.Errorf("read %s: a bound of %d bytes", path, max)
	}
	self := e.opts.Self
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return nil, false, err
		}
	}
	cmd, err := e.sandboxed(ctx, []string{self, ReadHelper, path, strconv.Itoa(max + 1)})
	if err != nil {
		return nil, false, err
	}
	// Room for the file and its bound, or for the longest answer that is
	// no file, whatever the bound (Codex review of #63, round 4).
	room := len("file\n") + max + 1
	if room < len("linked\n") {
		room = len("linked\n")
	}
	stdout := &capped{max: room}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	out := string(stdout.buf)
	switch {
	case err != nil:
		return nil, false, err
	case strings.HasPrefix(out, "file\n"):
		return []byte(strings.TrimPrefix(out, "file\n")), true, nil
	case out == "none\n":
		return nil, false, nil
	case out == "linked\n":
		return nil, false, fmt.Errorf("read %s: %w", path, api.ErrManyNames)
	}
	return nil, false, fmt.Errorf("read %s: no answer", path)
}

func (e *Engine) probe(ctx context.Context, check string, paths ...string) (string, error) {
	cmd, err := e.sandboxed(ctx, append([]string{shell(), "-c", probe, "sh", check}, paths...))
	if err != nil {
		return "", err
	}
	stdout := &capped{max: 4096 + 16}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		// A probe the cancelled context killed failed for the cancel: its
		// error says so, so a start a stop cut short is finished after the
		// restart. A probe that ended on its own fails as it did (Codex
		// review of #58, round 7).
		var exit *exec.ExitError
		if ctx.Err() != nil && errors.As(err, &exit) && exit.ExitCode() == -1 {
			return "", fmt.Errorf("%s %s: %w (%w)", check, strings.Join(paths, " "), ctx.Err(), err)
		}
		return "", fmt.Errorf("%s %s: %w", check, strings.Join(paths, " "), err)
	}
	return string(stdout.buf), nil
}

// SetVerdictTool gives session id team_verdict, on, or takes it away: only
// a review thread pocketd started files a verdict (ADR-0019), and every
// thread's session made before that had the tool. A tool given goes right
// after team_message when the session has it, else last; every other tool
// stays as it is. It says whether it changed the session.
func (e *Engine) SetVerdictTool(ctx context.Context, id int64, on bool) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	at, has := len(agent.Tools), -1
	for i, tool := range agent.Tools {
		switch tool.Name {
		case toolVerdict:
			has = i
		case toolMessage:
			at = i + 1
		}
	}
	var list []*durable.ToolRegistration
	switch {
	case on && has < 0:
		list = append(slices.Clone(agent.Tools[:at]), e.teamTools([]string{toolVerdict})...)
		list = append(list, agent.Tools[at:]...)
	case !on && has >= 0:
		list = append(slices.Clone(agent.Tools[:has]), agent.Tools[has+1:]...)
	default:
		return false, nil
	}
	return true, conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: list})})
}

// laterSupervisorTools are the supervisor's team tools after team_close,
// in roleTools' order, which a session started before them lacks:
// team_schedule (ADR-0030), team_send (ADR-0032), team_gather and
// team_board (ADR-0033), team_launch (ADR-0034), team_inbox (ADR-0046),
// team_threads (ADR-0072).
var laterSupervisorTools = []string{toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolInbox, toolThreads}

// SetSupervisorTools gives a supervisor's session started before them the
// supervisor's later team tools (laterSupervisorTools), in the order
// roleTools places them, after its other tools, and says whether it did
// (ADR-0030, ADR-0032, ADR-0033, ADR-0046, ADR-0072; Codex review of #51,
// round 1).
// A session that has them all, or that has no team_delegate, is left as it
// is.
func (e *Engine) SetSupervisorTools(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	own, team, tail := e.splitTools(agent.Tools)
	if !slices.Contains(team, toolDelegate) {
		return false, nil
	}
	missing := false
	for _, name := range laterSupervisorTools {
		if !slices.Contains(team, name) {
			team, missing = append(team, name), true
		}
	}
	if !missing {
		return false, nil
	}
	list := append(append(own, e.teamTools(team)...), tail...)
	return true, conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: list})})
}

// SetGitHubTool gives team_github to a plan's lead, a consult or a
// cross-review whose session was made before it (ADR-0036, ADR-0057), in
// the order roleTools places it among its team tools, after its other
// tools, and says whether it did. A plan's lead and an open review get it
// at start, and each gets it before its brief or question when an earlier
// run made its session (Codex review of #61, round 2). A session with none
// of team_plan, team_finding and team_diff (a review's alone), or with
// team_github already, is left as it is.
func (e *Engine) SetGitHubTool(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	own, team, tail := e.splitTools(agent.Tools)
	reads := slices.Contains(team, toolPlan) || slices.Contains(team, toolFinding) || slices.Contains(team, toolDiff)
	if !reads || slices.Contains(team, toolGitHub) {
		return false, nil
	}
	list := append(append(own, e.teamTools(append(team, toolGitHub))...), tail...)
	return true, conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: list})})
}

// SetWatchTool gives team_watch, and the team's extension if the session
// lacks it, to a session made before it (ADR-0073), in the place roleTools
// gives it among its team tools, after its other tools, and says whether
// it did. Its caller decides which sessions get it (team.MayWatch): a
// session made before team_memory, with other team tools or none, gets
// it too (Codex review of #173, round 4). Its tools become an exact list
// of the ones it is offered and team_watch, so a new extension offers no
// other team tool. A session that has it is left as it is.
func (e *Engine) SetWatchTool(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	hasExt := slices.Contains(extensionNames(agent.Extensions), e.teamExt.Name)
	hasTool := slices.Contains(toolNames(agent.Tools), toolWatch)
	if hasExt && hasTool {
		return false, nil
	}
	change := harness.AgentChange{}
	if !hasExt {
		change.Extensions = harness.SetTo(harness.ExtensionChange{List: append(slices.Clone(agent.Extensions), e.teamExt)})
	}
	if !hasTool {
		own, team, tail := e.splitTools(agent.Tools)
		change.Tools = harness.SetTo(harness.ToolChange{List: slices.Concat(own, e.teamTools(append(team, toolWatch)), tail)})
	}
	return true, conversation.Configure(ctx, change)
}

// SetWorkTool gives team_work to session id made before it (ADR-0074),
// among its team tools in the order roleTools places it, after its other
// tools, with the team extension when the session has none, and says
// whether it did. Its caller decides which sessions get it
// (team.MayWork); a session that has it is left as it is.
func (e *Engine) SetWorkTool(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	names := extensionNames(agent.Extensions)
	own, team, tail := e.splitTools(agent.Tools)
	has := slices.Contains(team, toolWork)
	if has && slices.Contains(names, e.teamExt.Name) {
		return false, nil
	}
	change := harness.AgentChange{}
	if !slices.Contains(names, e.teamExt.Name) {
		change.Extensions = harness.SetTo(harness.ExtensionChange{List: append(slices.Clone(agent.Extensions), e.teamExt)})
	}
	// The agent's tools are the ones it is offered, resolved: the tool is
	// named in the list always, as the question tool is.
	if !has {
		list := append(append(own, e.teamTools(append(team, toolWork))...), tail...)
		change.Tools = harness.SetTo(harness.ToolChange{List: list})
	}
	return true, conversation.Configure(ctx, change)
}

// splitTools splits a session's tools into its own (the coding tools),
// the names of its team tools and its tail, the question and schedule
// tools and the MCP tools, which come after the team tools in that order
// (toolsOf, ADR-0048, ADR-0060, ADR-0063): a team tool given later goes
// among the team tools, before the tail.
func (e *Engine) splitTools(tools []*durable.ToolRegistration) (own []*durable.ToolRegistration, team []string, tail []*durable.ToolRegistration) {
	for _, tool := range tools {
		switch {
		case e.isTeamTool(tool.Name):
			team = append(team, tool.Name)
		case tool.Name == toolQuestion || tool.Name == toolOwnSchedule || strings.HasPrefix(tool.Name, mcpToolPrefix):
			tail = append(tail, tool)
		default:
			own = append(own, tool)
		}
	}
	return own, team, tail
}

// isTeamTool says name is one of the team extension's tools.
func (e *Engine) isTeamTool(name string) bool {
	for _, tool := range e.teamExt.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
