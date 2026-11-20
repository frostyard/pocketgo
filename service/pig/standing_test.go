package pig

import (
	"context"
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
		waitIdle(t, e, id)
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
	waitIdle(t, e, id)
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
	waitIdle(t, e, id)
	for range 2 {
		if err := e.StartOver(ctx, id, "over-1"); err != nil {
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
	waitIdle(t, e, id)
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
	if did, busy, err := e.Reconfigure(ctx, id, spec); err != nil || !did || busy {
		t.Fatalf("reconfigure: %v %v %v", did, busy, err)
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := sessionInstructions(api.SessionSpec{Cwd: f.work, Profile: &changed, Roster: spec.Roster, Lessons: section})
	if *agent.Instructions != want {
		t.Fatalf("instructions:\n%s", *agent.Instructions)
	}
	if *agent.Model != (durable.ModelRef{Provider: "faux", ModelId: "faux-2"}) || agent.ThinkingLevel == "high" {
		t.Fatalf("model %+v thinking %q", agent.Model, agent.ThinkingLevel)
	}
	if names := toolNames(agent.Tools); !slices.Contains(names, "bash") || !slices.Contains(names, toolDelegate) {
		t.Fatalf("tools %v", names)
	}
	if did, busy, err := e.Reconfigure(ctx, id, spec); err != nil || did || busy {
		t.Fatalf("the same again: %v %v %v", did, busy, err)
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
