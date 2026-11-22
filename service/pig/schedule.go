package pig

import (
	"context"
	"errors"
	"slices"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/schedule"
)

// toolOwnSchedule is a private agent's schedule tool (ADR-0060): its own
// reminders and repeats, to the calling session, with team_schedule's
// engine and limits. It is no team tool: a private agent's session, which
// gets no team tool (ADR-0042), gets it, in an extension of its own, and
// TeamAllows does not run before it. A supervisor keeps team_schedule.
const toolOwnSchedule = schedule.OwnTool

// scheduleDescription is what team_schedule and schedule say they do:
// the same engine, to the calling session.
const scheduleDescription = "Send yourself a message in this session later, or on repeat: to check back on something slow, " +
	"or for a regular job. pocketd keeps the time; clock times are in the service's zone. " + schedule.Guide

// scheduleExtension holds the schedule tool. It is replay-safe, as
// team_schedule is: an add is kept by the calling session, its task and
// the call's id (api.TeamCall), so a run after a restart finds it.
func (e *Engine) scheduleExtension() *durable.Extension {
	return &durable.Extension{
		Name: "pocketgo-schedule",
		Tools: []*durable.ToolRegistration{{
			ToolSchema: schema(toolOwnSchedule, scheduleDescription, scheduleParameters),
			Replay:     durable.ReplaySafe,
			Execute: func(ctx context.Context, args any, call durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
				c, err := durable.FromJsonValue[schedule.Call](args)
				if err != nil {
					return toolText("", err), nil
				}
				if e.team == nil {
					return toolText("", errors.New("pocketd's team is not wired")), nil
				}
				return toolText(e.team.Schedule(ctx, api.TeamCall{Session: int64(call.ConversationId()), Task: int64(call.TaskId()), CallID: call.CallId()}, c)), nil
			},
		}},
	}
}

// SetScheduleTool gives the schedule tool, and its extension, to session
// id made before it, after its other tools, and says whether it did
// (ADR-0060). Its caller decides which sessions get it
// (team.MaySchedule); a session that has it is left as it is.
func (e *Engine) SetScheduleTool(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	names := extensionNames(agent.Extensions)
	has := slices.Contains(toolNames(agent.Tools), toolOwnSchedule)
	if has && slices.Contains(names, e.scheduleExt.Name) {
		return false, nil
	}
	change := harness.AgentChange{}
	if !slices.Contains(names, e.scheduleExt.Name) {
		change.Extensions = harness.SetTo(harness.ExtensionChange{List: append(slices.Clone(agent.Extensions), e.scheduleExt)})
	}
	// The agent's tools are the ones it is offered, resolved: an empty
	// list is a selection of none, which a new extension adds nothing to,
	// so the tool is named in the list always, as the question tool is
	// (Codex review of #95, round 1).
	if !has {
		change.Tools = harness.SetTo(harness.ToolChange{List: append(slices.Clone(agent.Tools), e.scheduleExt.Tools...)})
	}
	return true, conversation.Configure(ctx, change)
}
