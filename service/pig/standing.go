package pig

import (
	"context"
	"errors"
	"math"
	"slices"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// reserveLimit is the most free context summaryReserve keeps: PiG then
// writes a summary in up to 26,214 tokens, room for a long session's
// summary, thinking included, without compacting much earlier.
const reserveLimit = 32_768

// summaryReserve is the free context PiG keeps for model, whose largest
// answer is maxTokens and whose context window is window (0 for unknown),
// ported from pi-pocket (ADR-0025). PiG writes a compaction's summary in
// at most 80% of the reserve, thinking included; its default reserve,
// 16,384, gives 13,107 tokens, and a long session's summary, which folds in
// the one before, grows past that: once it is cut off, the session can no
// longer compact, and every run after fails. This keeps room for all the
// model can write, up to reserveLimit and a quarter of its window, and
// never less than PiG's default.
func summaryReserve(maxTokens, window int) int {
	fallback := harness.DefaultCompactionPolicy.ReserveTokens
	output := reserveLimit
	if maxTokens > 0 {
		output = int(math.Ceil(float64(maxTokens) / 0.8))
	}
	quarter := fallback
	if window > 0 {
		quarter = window / 4
	}
	return max(fallback, min(output, reserveLimit, quarter))
}

// compactionSettings are the harness's settings: PiG's own, with each
// model of the catalog given its summaryReserve, for every session.
func compactionSettings(models *ai.Models) *harness.HarnessSettings {
	overrides := map[string]durable.CompactionPolicyOverride{}
	for _, m := range models.GetModels() {
		reserve := summaryReserve(m.Capabilities.MaxOutputTokens, m.Capabilities.ContextWindow)
		overrides[m.ProviderID()+"/"+m.ID] = durable.CompactionPolicyOverride{ReserveTokens: &reserve}
	}
	return &harness.HarnessSettings{Compaction: &harness.CompactionPolicyPatch{ModelOverrides: overrides}}
}

// window is the context window of the model ref runs on, 0 when the
// catalog has no such model or names none.
func (e *Engine) window(ref *durable.ModelRef) int {
	if ref == nil {
		return 0
	}
	if m := e.models.GetModel(ref.Provider, ref.ModelId); m != nil {
		return m.Capabilities.ContextWindow
	}
	return 0
}

// idleIn says, inside a commit, whether conversation id has no run and
// nothing in its inbox: a change made in the commit comes before any run.
func idleIn(tx durable.Tx, id durable.ConversationId) (bool, error) {
	live, err := tx.Doc(harness.LiveDoc, id)
	if err != nil || live.Has("run") {
		return false, err
	}
	inbox, err := tx.Doc(harness.InboxDoc, id)
	if err != nil {
		return false, err
	}
	items := inbox.Array("items")
	return items == nil || items.Len() == 0, nil
}

// CompactIfOver compacts standing session id once its context, as PiG
// estimates it, reaches compactAt below its model's window
// (team.ShouldCompact; ADR-0025). The compaction starts in one commit
// that finds the session idle, no compaction listed in its live state and
// no entry written since the estimate, so a check run again, after a
// restart or beside another, never compacts twice. PiG's own threshold
// stays the backstop. It says whether it started one.
func (e *Engine) CompactIfOver(ctx context.Context, id int64, compactAt int) (bool, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	view, err := conversation.Context(ctx, nil)
	if err != nil {
		return false, err
	}
	if agent.Model == nil || !team.ShouldCompact(harness.EstimateContext(view, nil), compactAt, e.window(agent.Model)) {
		return false, nil
	}
	// As PiG's own threshold: only when there is an older part to
	// summarize, past what it keeps as it is.
	policy := harness.ResolveSettings(e.settings).Compaction.PolicyFor(*agent.Model)
	if _, ok := harness.SelectCut(view, policy.KeepRecentTokens); !ok {
		return false, nil
	}
	var at durable.EntryId
	for _, entry := range view.Entries {
		at = max(at, entry.Id)
	}
	if view.Head != nil {
		at = max(at, view.Head.Id)
	}
	started, err := conversation.Commit(ctx, func(tx durable.Tx) (any, error) {
		conversationID := durable.ConversationId(id)
		latest, err := tx.LatestEntryId(conversationID)
		if err != nil || latest == nil || *latest != at {
			return false, err
		}
		if idle, err := idleIn(tx, conversationID); err != nil || !idle {
			return false, err
		}
		live, err := tx.Doc(harness.LiveDoc, conversationID)
		if err != nil || live.Has("compactions") {
			return false, err
		}
		_, err = harness.CreateCompaction(tx, conversationID, harness.CompactionInput{Reason: durable.CompactionManual}, nil)
		return err == nil, err
	})
	if err != nil {
		return false, err
	}
	return started.(bool), nil
}

// StartOver starts session id's context afresh, in the same session: a
// reset entry, which PiG places at once, so the model sees nothing before
// it (ADR-0025). It is keyed by key, so a retry that finds its reset does
// nothing; a session with a run, or messages waiting for one, is refused
// (api.ErrSessionBusy), with nothing written.
func (e *Engine) StartOver(ctx context.Context, id int64, key string) error {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return err
	}
	_, err = conversation.Commit(ctx, func(tx durable.Tx) (any, error) {
		conversationID := durable.ConversationId(id)
		done, err := tx.SubmissionByRequest(conversationID, key)
		if err != nil || done != nil {
			return nil, err
		}
		if idle, err := idleIn(tx, conversationID); err != nil {
			return nil, err
		} else if !idle {
			return nil, api.ErrSessionBusy
		}
		entry := durable.EntryDraft{Kind: durable.ResetEntry.Kind, HeadSelf: true}
		_, err = conversation.SubmitIn(tx, durable.SubmissionDraft{RequestId: &key, Type: durable.SubmissionTypeWrite, Entry: &entry})
		return nil, err
	})
	return err
}

// Reconfigure gives standing session id the agent spec makes, in one
// commit that finds it with no run: its instructions, with the lessons
// section it has kept (SetLessons writes that), its tools and its model
// (ADR-0025). What is the same already stays. Messages a failed run left
// queued wait for the next run, which takes the new agent: only a run under
// way keeps the one it started with, and then nothing changes
// (api.ReconfigureDeferred).
func (e *Engine) Reconfigure(ctx context.Context, id int64, spec api.SessionSpec) (api.Reconfigured, error) {
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return api.ReconfigureSame, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return api.ReconfigureSame, err
	}
	current := ""
	if agent.Instructions != nil {
		current = *agent.Instructions
	}
	base, _ := team.WithoutLessons(current)
	spec.Lessons = current[len(base):]
	extensions, tools, err := e.toolsOf(spec)
	if err != nil {
		return api.ReconfigureSame, err
	}
	changed := false
	change := harness.AgentChange{}
	if want := sessionInstructions(spec); want != current {
		change.Instructions, changed = harness.SetTo(want), true
	}
	if !slices.Equal(extensionNames(agent.Extensions), extensionNames(extensions)) {
		change.Extensions, changed = harness.SetTo(harness.ExtensionChange{List: extensions}), true
	}
	if tools != nil && !slices.Equal(toolNames(agent.Tools), toolNames(tools)) {
		change.Tools, changed = harness.SetTo(harness.ToolChange{List: tools}), true
	}
	model := durable.ModelRef{Provider: spec.Model.Provider, ModelId: spec.Model.ID}
	if agent.Model == nil || *agent.Model != model {
		// A new model at its default thinking level: the level the session
		// had may be one the new model does not take.
		change.Model, change.ThinkingLevel, changed = harness.SetTo(model), harness.Cleared[ai.ModelThinkingLevel](), true
	}
	if !changed {
		return api.ReconfigureSame, nil
	}
	_, err = conversation.Commit(ctx, func(tx durable.Tx) (any, error) {
		live, err := tx.Doc(harness.LiveDoc, durable.ConversationId(id))
		if err != nil {
			return nil, err
		}
		if live.Has("run") {
			return nil, errBusy
		}
		return nil, harness.Configure(tx, durable.ConversationId(id), change)
	})
	switch {
	case errors.Is(err, errBusy):
		return api.ReconfigureDeferred, nil
	case err != nil:
		return api.ReconfigureSame, err
	}
	return api.ReconfigureApplied, nil
}

// errBusy ends a commit that found its session at work.
var errBusy = errors.New("the session has a run")

func extensionNames(list []*durable.Extension) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, x.Name)
	}
	return out
}

func toolNames(list []*durable.ToolRegistration) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}
