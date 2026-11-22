package pig

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// toolQuestion is the question tool's name (ADR-0047). It is no team tool:
// a private agent's session, which gets no team tool (ADR-0042), gets it,
// in an extension of its own.
const toolQuestion = "question"

var questionParameters = `{"type":"object","properties":{` +
	`"question":{"type":"string","description":"The question, at most ` + strconv.Itoa(proto.MaxQuestion) + ` characters, standing on its own: the owner reads it in this session."},` +
	`"options":{"type":"array","maxItems":` + strconv.Itoa(proto.MaxQuestionOptions) + `,"items":{"type":"string"},"description":"Answers you suggest: none, or 2 to ` + strconv.Itoa(proto.MaxQuestionOptions) +
	`, each at most ` + strconv.Itoa(proto.MaxQuestionOption) + ` characters. The owner may always answer in words instead."},` +
	`"multiple":{"type":"boolean","description":"Let the owner pick more than one option."},` +
	`"placeholder":{"type":"string","description":"A hint for an answer in words, at most ` + strconv.Itoa(proto.MaxQuestionPlaceholder) + ` characters."}` +
	`},"required":["question"],"additionalProperties":false}`

// questionExtension holds the question tool. Its call waits for the
// owner's answer in Team.Question; it is replay-safe, since the question
// is kept by the calling task and the call's id, so a run after a
// restart shows the same question and finds an answer made before.
func (e *Engine) questionExtension() *durable.Extension {
	return &durable.Extension{
		Name: "pocketgo-question",
		Tools: []*durable.ToolRegistration{{
			ToolSchema: schema(toolQuestion, "Put a question to the owner inline, in this session, and wait for the answer: it is this call's result, "+
				`{"answer", "option"} or {"skipped": true, "reason"}. Use it only in a run the owner's own message started, while the owner watches: `+
				"in a run a schedule, a report or a note started it is refused, and you put the question in prose in your summary. "+
				"One question at a time; it is skipped when no answer comes in time.", questionParameters),
			Replay: durable.ReplaySafe,
			Execute: func(ctx context.Context, args any, call durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
				q, err := durable.FromJsonValue[team.Question](args)
				if err != nil {
					return toolText("", err), nil
				}
				if e.team == nil {
					return toolText("", errors.New("pocketd's team is not wired")), nil
				}
				text, err := e.team.Question(ctx, api.TeamCall{Session: int64(call.ConversationId()), Task: int64(call.TaskId()), CallID: call.CallId()}, q)
				// A wait an abort or the service's stop ended is no result:
				// the task stops where it is, and a run of it after a
				// restart waits again.
				if err != nil && ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					return durable.ToolExecutionResult{}, ctx.Err()
				}
				return toolText(text, err), nil
			},
		}},
	}
}

// questionRole is what a session made from spec works as, for
// team.MayQuestion.
func questionRole(spec api.SessionSpec) team.SessionRole {
	return team.SessionRole{Thread: spec.Thread != 0, Review: spec.Review, Worker: spec.Worker, Consult: spec.Consult != 0, Gather: spec.Gather != 0}
}

// RunStarter is the request key of the submission that placed the first
// entry of session id's current run, which started it: "" when no
// submission placed it, as a run a boundary started; busy is false when
// no run is in progress (ADR-0047).
func (e *Engine) RunStarter(ctx context.Context, id int64) (string, bool, error) {
	first, busy, err := e.RunIdentity(ctx, id)
	if err != nil || !busy || first == 0 {
		return "", busy, err
	}
	placed, err := e.placedKeys(ctx, id, first)
	if err != nil {
		return "", true, err
	}
	return placed, true, nil
}

// placedKeys is the request key of the input submission of session id that
// placed entry, "" for none: the session's submissions are read newest
// first, and the reading stops at the one that placed it.
func (e *Engine) placedKeys(ctx context.Context, id, entry int64) (string, error) {
	conversation, order := durable.ConversationId(id), durable.ScanDescending
	query := durable.SubmissionQuery{ConversationId: &conversation, Order: &order}
	var cursor durable.Cursor
	for {
		page, err := e.storage.ScanSubmissions(ctx, query, 200, cursor)
		if err != nil {
			return "", err
		}
		for _, record := range page.Items {
			if record.Type == durable.SubmissionTypeInput && record.Entry != nil && int64(*record.Entry) == entry {
				if record.RequestId == nil {
					return "", nil
				}
				return *record.RequestId, nil
			}
		}
		if page.Next == nil {
			return "", nil
		}
		cursor = *page.Next
	}
}

// SetQuestionTool gives the question tool, and its extension, to session
// id made before it, after its other tools, and says whether it did
// (ADR-0047). Its caller decides which sessions get it
// (team.MayQuestion); a session that has it is left as it is.
func (e *Engine) SetQuestionTool(ctx context.Context, id int64) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	names := extensionNames(agent.Extensions)
	if slices.Contains(toolNames(agent.Tools), toolQuestion) && slices.Contains(names, e.questionExt.Name) {
		return false, nil
	}
	change := harness.AgentChange{}
	if !slices.Contains(names, e.questionExt.Name) {
		change.Extensions = harness.SetTo(harness.ExtensionChange{List: append(slices.Clone(agent.Extensions), e.questionExt)})
	}
	// A list of no tools is every tool of the extensions, the new one's
	// included; a list names each tool it gives.
	if len(agent.Tools) > 0 && !slices.Contains(toolNames(agent.Tools), toolQuestion) {
		change.Tools = harness.SetTo(harness.ToolChange{List: append(slices.Clone(agent.Tools), e.questionExt.Tools...)})
	}
	return true, conversation.Configure(ctx, change)
}
