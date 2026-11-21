package pig

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Every model gets room for a long session's summary: all it can write, up
// to 32,768 tokens and a quarter of its window, never less than PiG's
// default of 16,384 (ported from pi-pocket).
func TestSummaryReserve(t *testing.T) {
	for _, tc := range []struct{ maxTokens, window, want int }{
		{32_000, 200_000, 32_768},   // the limit
		{20_000, 1_000_000, 25_000}, // all it can write
		{8_192, 128_000, 16_384},    // never below PiG's default
		{32_000, 64_000, 16_384},    // a quarter of a small window, but the default
		{32_000, 100_000, 25_000},   // a quarter of the window
		{0, 0, 16_384},              // nothing known
		{0, 1_000_000, 32_768},      // no output limit known
	} {
		if got := summaryReserve(tc.maxTokens, tc.window); got != tc.want {
			t.Errorf("summaryReserve(%d, %d) = %d, want %d", tc.maxTokens, tc.window, got, tc.want)
		}
	}
}

// windowModels are faux models whose faux-1 has a window and an output
// limit, answering steps.
func windowModels(steps ...ai.FauxResponseStep) *ai.Models {
	faux := ai.NewFauxProvider(ai.FauxConfig{Models: []ai.FauxModelDefinition{
		{ID: "faux-1", Name: "Faux", ContextWindow: 200_000, MaxTokens: 32_000},
		{ID: "faux-2", Name: "Faux 2", ContextWindow: 400_000, MaxTokens: 32_000},
	}})
	models := ai.CreateModels()
	models.SetProvider(faux.Provider())
	faux.SetResponses(steps)
	return models
}

// A standing session compacts once its context reaches its threshold:
// PiG summarizes what came before the recent part, with the room
// summaryReserve keeps for every session, and the transcript shows the
// summary alone. A check run again finds nothing to do.
func TestAStandingSessionCompactsAtItsThreshold(t *testing.T) {
	var mu sync.Mutex
	var summaryBudget int
	summary := ai.FauxFactoryStep(func(_ ai.TranscriptContext, options ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		mu.Lock()
		summaryBudget = options.MaxTokens
		mu.Unlock()
		return ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("## Goal\nKeep going.")}}.AssistantMessage(), nil
	})
	f := newFixture(t)
	e := f.open(windowModels(answer("one"), answer("two"), summary))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "s1", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	// Two long messages: the first is old enough to summarize, past the
	// 20,000 tokens PiG keeps as they are.
	long := strings.Repeat("words of the owner ", 6000)
	for i, key := range []string{"m1", "m2"} {
		if _, err := e.Send(ctx, id, key, long+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
		settle(t, e, id)
	}
	if started, err := e.CompactIfOver(ctx, id, 150_000); err != nil || started {
		t.Fatalf("below the threshold: %v, %v", started, err)
	}
	if started, err := e.CompactIfOver(ctx, id, 30_000); err != nil || !started {
		t.Fatalf("at the threshold: %v, %v", started, err)
	}
	if started, err := e.CompactIfOver(ctx, id, 30_000); err != nil || started {
		t.Fatalf("again: %v, %v", started, err)
	}
	settle(t, e, id)
	entries, live, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries, func(e proto.Entry) bool { return e.Kind == proto.EntryCompaction })
	if i < 0 || entries[i].Text != "## Goal\nKeep going." {
		t.Fatalf("entries %s, %+v", kinds(entries), entries)
	}
	if live.Compacting || live.CompactionFailure != "" {
		t.Fatalf("live %+v", live)
	}
	mu.Lock()
	defer mu.Unlock()
	// 80% of the 32,768 tokens summaryReserve keeps for this model, not
	// of PiG's 16,384.
	if summaryBudget != 26_214 {
		t.Fatalf("the summary's budget %d", summaryBudget)
	}
	if started, err := e.CompactIfOver(ctx, id, 30_000); err != nil || started {
		t.Fatalf("after the compaction: %v, %v", started, err)
	}
}

// Start Over writes a reset in an idle session, once a key: the model sees
// nothing before it, and the transcript shows it.
func TestStartOverResetsTheContextOnce(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	counting := ai.FauxFactoryStep(func(transcript ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		mu.Lock()
		seen = append(seen, len(transcript.Messages()))
		mu.Unlock()
		return ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("ok")}}.AssistantMessage(), nil
	})
	f := newFixture(t)
	e := f.open(windowModels(counting, counting))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "s1", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "m1", "Remember this."); err != nil {
		t.Fatal(err)
	}
	settle(t, e, id)
	for range 2 {
		if _, err := e.StartOver(ctx, id, "over-1", nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n := countKind(entries, proto.EntryReset); n != 1 || entries[len(entries)-1].Kind != proto.EntryReset {
		t.Fatalf("entries %s", kinds(entries))
	}
	if _, err := e.Send(ctx, id, "m2", "What did I say?"); err != nil {
		t.Fatal(err)
	}
	settle(t, e, id)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[1] != seen[0] {
		t.Fatalf("the model saw %v messages", seen)
	}
}

func countKind(entries []proto.Entry, kind proto.EntryKind) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Reconfigure gives a standing session its profile's identity, roster,
// tools and model as they are now, and keeps the lessons section it has;
// the same spec again changes nothing.
func TestReconfigureFollowsTheProfileAndKeepsTheLessons(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Identity: "# Odrade\nShe leads.", Tools: []string{"read"}, Standing: true}
	section := team.LessonsSection([]team.Lesson{{ID: 1, Text: "Ask before merging."}})
	id, err := e.CreateSession(ctx, "standing:odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Thinking: "high", Profile: odrade, Roster: "Delegate to vera.", Lessons: section})
	if err != nil {
		t.Fatal(err)
	}
	changed := *odrade
	changed.Identity, changed.Tools = "# Odrade\nShe leads, and listens.", []string{"read", "bash"}
	moved := proto.ModelRef{Provider: "faux", ID: "faux-2"}
	spec := api.SessionSpec{Cwd: f.work, Model: moved, Profile: &changed, Roster: "Delegate to vera or duncan."}
	if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != api.ReconfigureApplied {
		t.Fatalf("reconfigure: %v %v", did, err)
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := sessionInstructions(api.SessionSpec{Cwd: f.work, Profile: &changed, Roster: spec.Roster, Lessons: section}, team.Loaded{})
	if *agent.Instructions != want {
		t.Fatalf("instructions:\n%s", *agent.Instructions)
	}
	if *agent.Model != (durable.ModelRef{Provider: "faux", ModelId: "faux-2"}) || agent.ThinkingLevel == "high" {
		t.Fatalf("model %+v thinking %q", agent.Model, agent.ThinkingLevel)
	}
	if names := toolNames(agent.Tools); !slices.Contains(names, "bash") || !slices.Contains(names, toolDelegate) {
		t.Fatalf("tools %v", names)
	}
	if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != api.ReconfigureSame {
		t.Fatalf("the same again: %v %v", did, err)
	}
}

// The live state says when a compaction runs, and why the newest failed;
// a compaction entry carries its summary without PiG's wrapping.
func TestCompactionsInTheLiveStateAndTheTranscript(t *testing.T) {
	live := wireLive(7, []byte(`{"compactions":[{"taskId":3,"trigger":"manual","blocking":false,"attempt":1}],"compactionFailure":{"taskId":2,"trigger":"threshold","error":"the summary was cut off"}}`))
	if !live.Compacting || live.CompactionFailure != "the summary was cut off" || live.Busy {
		t.Fatalf("live %+v", live)
	}
	if idle := wireLive(7, []byte(`{}`)); idle.Compacting || idle.CompactionFailure != "" {
		t.Fatalf("idle %+v", idle)
	}
	if got := summaryOf(summaryPrefix + "## Goal" + summarySuffix); got != "## Goal" {
		t.Fatalf("summary %q", got)
	}
	if got := summaryOf("a summary of another form"); got != "a summary of another form" {
		t.Fatalf("summary %q", got)
	}
}

// A standing session is recorded under the key of the request that made
// it and under its profile's key, as a standing session: a retry of that
// request finds it whatever the profile says by then, and another request
// gets the same session, recorded under its own key too (Codex review of
// #43, round 1).
func TestAStandingSessionIsRecordedUnderBothKeys(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Standing: true}
	spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade, Route: "profile", StandingKey: "standing:odrade"}
	id, err := e.CreateSession(ctx, "u:d:r1", spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"u:d:r1", "standing:odrade"} {
		made, ok, err := e.Created(ctx, key)
		if err != nil || !ok || made.ID != id || !made.Standing || made.Profile != "odrade" {
			t.Fatalf("%s: %+v %v %v", key, made, ok, err)
		}
	}
	again, err := e.CreateSession(ctx, "u:d:r2", spec)
	if err != nil || again != id {
		t.Fatalf("another request: %d, %v", again, err)
	}
	if made, ok, err := e.Created(ctx, "u:d:r2"); err != nil || !ok || made.ID != id || !made.Standing {
		t.Fatalf("the other request's record: %+v %v %v", made, ok, err)
	}
}

// A failed run's queued messages wait for the next run with no run under
// way: the profile applies to that run, which the owner's next message
// starts, never the old one (Codex review of #43, round 2). Start Over
// still waits for them.
func TestAQueueLeftByAFailedRunTakesTheNewProfile(t *testing.T) {
	f := newFixture(t)
	failed := ai.FauxStaticStep(ai.FauxResponse{StopReason: "error", ErrorMessage: "the provider is down"})
	e := f.open(fauxModels(toolTurn("bash", map[string]any{"command": "true"}, "b1"), failed))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Identity: "# Odrade\nShe leads.", Standing: true}
	id, err := e.CreateSession(ctx, "standing:odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:first", "first"); err != nil {
		t.Fatal(err)
	}
	request := <-f.requests
	if _, err := e.Send(ctx, id, "u:dev:second", "second"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+request.ID, request.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	settle(t, e, id)
	if _, live, err := e.Transcript(ctx, id); err != nil || live.Busy || len(live.Queued) != 1 {
		t.Fatalf("after the failed run: %+v, %v", live, err)
	}
	changed := *odrade
	changed.Identity = "# Odrade\nShe leads, and listens."
	spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &changed}
	instructions := func() string {
		conversation, _ := e.conversation(ctx, id)
		agent, _ := conversation.Agent(ctx)
		return *agent.Instructions
	}
	// A Start Over refused changes nothing, its profile's change included
	// (Codex review of #43, round 4).
	if _, err := e.StartOver(ctx, id, "over-1", &spec); !errors.Is(err, api.ErrSessionBusy) {
		t.Fatalf("start over with a queue: %v", err)
	}
	if strings.Contains(instructions(), "and listens") {
		t.Fatal("a refused Start Over changed the agent")
	}
	if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != api.ReconfigureApplied {
		t.Fatalf("reconfigure with a queue: %v, %v", did, err)
	}
	if !strings.Contains(instructions(), "and listens") {
		t.Fatal("the reconfigure did not apply")
	}
}

// Start Over of an idle session gives it its profile and its reset in one
// step.
func TestStartOverTakesTheProfileWithTheReset(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Identity: "# Odrade\nShe leads.", Standing: true}
	id, err := e.CreateSession(ctx, "standing:odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade})
	if err != nil {
		t.Fatal(err)
	}
	changed := *odrade
	changed.Identity = "# Odrade\nShe leads, and listens."
	did, err := e.StartOver(ctx, id, "over-1", &api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &changed})
	if err != nil || did != api.ReconfigureApplied {
		t.Fatalf("start over: %v, %v", did, err)
	}
	conversation, _ := e.conversation(ctx, id)
	agent, _ := conversation.Agent(ctx)
	entries, _, err := e.Transcript(ctx, id)
	if err != nil || !strings.Contains(*agent.Instructions, "and listens") || countKind(entries, proto.EntryReset) != 1 {
		t.Fatalf("instructions %q, entries %s, %v", *agent.Instructions, kinds(entries), err)
	}
}

// settle waits until session id has no live task, on PiG's own idle
// condition, with no wall-clock deadline (Codex review of #43, round 5).
func settle(t *testing.T, e *Engine, id int64) {
	t.Helper()
	conversation, err := e.conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := conversation.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The live state says something waits for a run while pi.inbox holds an
// item, a write the queue does not list included: a Start Over waits for
// it (Codex review of #43, round 5).
func TestAPendingWriteShowsInTheLiveState(t *testing.T) {
	if !inboxPending(inbox(t, `{"id":4,"mode":"write"}`)) {
		t.Error("a write is not pending")
	}
	if !inboxPending(inbox(t, `{"id":5,"mode":"followUp","content":"next"}`)) {
		t.Error("a message is not pending")
	}
	if inboxPending(inbox(t)) || inboxPending(nil) {
		t.Error("an empty inbox is pending")
	}
}

// SetModel gives a session the owner's model and level at once (ADR-0028);
// an empty level is the model's default, which PiG reads as off. A
// standing session's refresh then keeps a level its spec names, and takes
// the default again for a spec that names none.
func TestSetModelAndTheSpecsLevel(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Identity: "# Odrade\nShe leads.", Tools: []string{"read"}, Standing: true}
	id, err := e.CreateSession(ctx, "standing:odrade", api.SessionSpec{Cwd: f.work, Model: fauxRef, Thinking: "high", Profile: odrade})
	if err != nil {
		t.Fatal(err)
	}
	runs := func() (durable.ModelRef, ai.ModelThinkingLevel) {
		t.Helper()
		conversation, err := e.conversation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		agent, err := conversation.Agent(ctx)
		if err != nil || agent.Model == nil {
			t.Fatalf("agent: %+v, %v", agent, err)
		}
		return *agent.Model, agent.ThinkingLevel
	}
	two := proto.ModelRef{Provider: "faux", ID: "faux-2"}
	if err := e.SetModel(ctx, id, "k1", api.ModelChoice{Model: two, Thinking: "low"}, "owner", false); err != nil {
		t.Fatal(err)
	}
	if m, level := runs(); m != (durable.ModelRef{Provider: "faux", ModelId: "faux-2"}) || level != "low" {
		t.Fatalf("after SetModel: %+v at %q", m, level)
	}
	if err := e.SetModel(ctx, id, "k2", api.ModelChoice{Model: two}, "owner", false); err != nil {
		t.Fatal(err)
	}
	if _, level := runs(); level != ai.ThinkingOff {
		t.Fatalf("the default level reads %q, want off", level)
	}
	spec := api.SessionSpec{Cwd: f.work, Model: two, Thinking: "high", Profile: odrade}
	for i, want := range []api.Reconfigured{api.ReconfigureApplied, api.ReconfigureSame} {
		if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != want {
			t.Fatalf("reconfigure %d with the pick's level: %v %v", i, did, err)
		}
	}
	if _, level := runs(); level != "high" {
		t.Fatalf("the spec's level: %q", level)
	}
	spec.Thinking = ""
	for i, want := range []api.Reconfigured{api.ReconfigureApplied, api.ReconfigureSame} {
		if did, err := e.Reconfigure(ctx, id, spec); err != nil || did != want {
			t.Fatalf("reconfigure %d with no level: %v %v", i, did, err)
		}
	}
	if _, level := runs(); level != ai.ThinkingOff {
		t.Fatalf("no level in the spec reads %q, want off", level)
	}
}

// SetModel keeps each change under its request key, in the commit that
// applies it: the key run again changes nothing, so a retry never undoes a
// later change (Codex review of #48, round 1). ModelChange reads it, for
// its session alone.
func TestSetModelChangesOnceAKey(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "c1", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	two := api.ModelChoice{Model: proto.ModelRef{Provider: "faux", ID: "faux-2"}, Thinking: "low"}
	high := api.ModelChoice{Model: fauxRef, Thinking: "high"}
	if _, in, err := e.ModelChange(ctx, id, "m1"); err != nil || in {
		t.Fatalf("a change before SetModel: %v %v", in, err)
	}
	for _, step := range []struct {
		key    string
		choice api.ModelChoice
	}{{"m1", two}, {"m2", high}, {"m1", two}} {
		if err := e.SetModel(ctx, id, step.key, step.choice, "owner", false); err != nil {
			t.Fatal(err)
		}
	}
	if runs, err := e.SessionModel(ctx, id); err != nil || runs != high {
		t.Fatalf("after m1 run again the session runs %+v (%v), want m2's %+v", runs, err, high)
	}
	if got, in, err := e.ModelChange(ctx, id, "m1"); err != nil || !in || got.Choice != two || got.Route != "owner" || got.Latest {
		t.Fatalf("m1's change: %+v %v %v, want it kept and not the latest", got, in, err)
	}
	if got, in, err := e.ModelChange(ctx, id, "m2"); err != nil || !in || !got.Latest {
		t.Fatalf("m2's change: %+v %v %v, want the latest", got, in, err)
	}
	if _, in, err := e.ModelChange(ctx, id+1, "m1"); err != nil || in {
		t.Fatalf("m1's change for another session: %v %v", in, err)
	}
	if _, found, err := e.StandingPick(ctx, id); err != nil || found {
		t.Fatalf("a change with no pick kept one: %v %v", found, err)
	}
}

// The owner's pick is kept with its change, and Start Over drops it in
// its reset's commit: a Start Over run again, whose reset is in, keeps a
// pick made since (Codex review of #48, round 1).
func TestStartOverDropsOnlyThePickItsResetReplaced(t *testing.T) {
	f := newFixture(t)
	e := f.open(windowModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	odrade := &profiles.Profile{ID: "odrade", Name: "Odrade", Role: profiles.RoleSupervisor, Identity: "# Odrade\nShe leads.", Tools: []string{"read"}, Standing: true}
	spec := api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: odrade}
	id, err := e.CreateSession(ctx, "standing:odrade", spec)
	if err != nil {
		t.Fatal(err)
	}
	first := api.ModelChoice{Model: proto.ModelRef{Provider: "faux", ID: "faux-2"}, Thinking: "low"}
	if err := e.SetModel(ctx, id, "p1", first, "owner", true); err != nil {
		t.Fatal(err)
	}
	if got, found, err := e.StandingPick(ctx, id); err != nil || !found || got != first {
		t.Fatalf("the pick: %+v %v %v", got, found, err)
	}
	if _, err := e.StartOver(ctx, id, "over-1", &spec); err != nil {
		t.Fatal(err)
	}
	if _, found, err := e.StandingPick(ctx, id); err != nil || found {
		t.Fatalf("the pick outlived its Start Over: %v %v", found, err)
	}
	later := api.ModelChoice{Model: proto.ModelRef{Provider: "faux", ID: "faux-2"}, Thinking: "high"}
	if err := e.SetModel(ctx, id, "p2", later, "owner", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartOver(ctx, id, "over-1", &spec); err != nil {
		t.Fatal(err)
	}
	if got, found, err := e.StandingPick(ctx, id); err != nil || !found || got != later {
		t.Fatalf("a Start Over run again dropped the pick made since: %+v %v %v", got, found, err)
	}
	if runs, err := e.SessionModel(ctx, id); err != nil || runs != later {
		t.Fatalf("the session runs %+v (%v), want the later pick", runs, err)
	}
}
