package pig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
)

// The prompt cache keeper (ADR-0074), with its clock and timers driven by
// hand and PiG's faux provider behind a shim that builds a payload as a
// real provider does: no network, no wall clock, no sleep.

// sonnetRates and sonnetCost are Claude Sonnet 4.6's prices per million
// tokens, with a 5-minute cache.
var (
	sonnetRates = cacheRates{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
	sonnetCost  = &ai.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
)

func TestRefreshDelayIsNinetyPercentAndTenSecondsEarlyAtLeast(t *testing.T) {
	for ttl, want := range map[time.Duration]time.Duration{
		300 * time.Second:  270 * time.Second,
		3600 * time.Second: 3240 * time.Second,
		60 * time.Second:   50 * time.Second,
		20 * time.Second:   10 * time.Second,
	} {
		if got, ok := refreshDelay(ttl); !ok || got != want {
			t.Errorf("refreshDelay(%v) = %v %v, want %v", ttl, got, ok, want)
		}
	}
	for _, ttl := range []time.Duration{10 * time.Second, 5 * time.Second, 0} {
		if got, ok := refreshDelay(ttl); ok {
			t.Errorf("refreshDelay(%v) = %v: a lifetime this short has no refresh", ttl, got)
		}
	}
}

func TestCacheLifetimeFollowsTheRetentionTheRequestUsed(t *testing.T) {
	model := &ai.Model{PromptCache: ai.ModelPromptCache{"short": 300, "long": 3600}}
	noEnv := func(string) string { return "" }
	longEnv := func(name string) string {
		if name == "PI_CACHE_RETENTION" {
			return "long"
		}
		return ""
	}
	for _, tc := range []struct {
		name    string
		model   *ai.Model
		options ai.StreamOptions
		getenv  func(string) string
		want    time.Duration
	}{
		{"unset is short", model, ai.StreamOptions{}, noEnv, 300 * time.Second},
		{"short", model, ai.StreamOptions{CacheRetention: ai.CacheRetentionShort}, longEnv, 300 * time.Second},
		{"long", model, ai.StreamOptions{CacheRetention: ai.CacheRetentionLong}, noEnv, 3600 * time.Second},
		{"unset is long by the service's environment", model, ai.StreamOptions{}, longEnv, 3600 * time.Second},
		{"unset is long by the request's environment", model, ai.StreamOptions{Env: ai.ProviderEnv{"PI_CACHE_RETENTION": "long"}}, noEnv, 3600 * time.Second},
		{"no environment reader", model, ai.StreamOptions{}, nil, 300 * time.Second},
		{"none", model, ai.StreamOptions{CacheRetention: ai.CacheRetentionNone}, noEnv, 0},
		{"a model with no lifetime", &ai.Model{}, ai.StreamOptions{}, noEnv, 0},
		{"a model with no long lifetime", &ai.Model{PromptCache: ai.ModelPromptCache{"short": 300}}, ai.StreamOptions{CacheRetention: ai.CacheRetentionLong}, noEnv, 0},
	} {
		got, ok := cacheLifetime(tc.model, tc.options, tc.getenv)
		if got != tc.want || ok != (tc.want > 0) {
			t.Errorf("%s: %v %v, want %v", tc.name, got, ok, tc.want)
		}
	}
}

func TestDecideWarmsOnlyWhenTheHazardTimesTheMissIsMoreThanTheWarm(t *testing.T) {
	// A 5-minute write costs 12.5 reads: a 0.9 hazard pays, a 0.05 one
	// does not.
	if warm, c := decide(50_000, 0, sonnetRates, toolHazard); !warm || c.Savings <= 0 {
		t.Fatalf("a 50,000-token cached prompt after a tool call: %v %+v", warm, c)
	}
	if warm, _ := decide(50_000, 0, sonnetRates, 0.05); warm {
		t.Fatal("a 0.05 hazard warms")
	}
	// warmCost is a read of the cached tokens and one output token,
	// missCost a write of them in place of a read.
	if _, c := decide(1_000_000, 0, sonnetRates, 1); c.Warm != 0.3+15.0/1e6 || c.Miss != 3.45 {
		t.Fatalf("the costs of a million cached tokens: %+v", c)
	}
	// One output token costs more than a tiny prompt's rewrite.
	if warm, _ := decide(1, 0, sonnetRates, toolHazard); warm {
		t.Fatal("a one-token prompt warms")
	}
	if warm, _ := decide(50_000, 0, cacheRates{}, toolHazard); warm {
		t.Fatal("a model priced at nothing warms")
	}
	// No write price: the miss is a write at the input price.
	if _, c := decide(1_000_000, 0, cacheRates{Input: 3, Output: 15, CacheRead: 0.3}, 1); c.Miss != 2.7 {
		t.Fatalf("the miss with no write price: %+v", c)
	}
}

// Codex review of #172, round 1: only what the provider cached is an
// entry a warm keeps, and what it read as plain input costs the input
// price again in every warm.
func TestDecideCountsOnlyTheTokensTheProviderCached(t *testing.T) {
	// A prompt below the cacheable minimum: no entry, however long.
	if warm, c := decide(0, 50_000, sonnetRates, toolHazard); warm || c.Miss != 0 {
		t.Fatalf("an uncached prompt: %v %+v", warm, c)
	}
	// The uncached tokens are input in the warm's cost, and save nothing.
	if _, c := decide(1_000_000, 1_000_000, sonnetRates, 1); math.Abs(c.Warm-3.300015) > 1e-9 || c.Miss != 3.45 {
		t.Fatalf("the costs of a million cached and a million plain tokens: %+v", c)
	}
	// Enough plain input outweighs what the cached part saves.
	if warm, _ := decide(10_000, 100_000, sonnetRates, toolHazard); warm {
		t.Fatal("a warm that rereads ten times its cached part as input warms")
	}
}

func TestLateRefreshAllowsHalfTheMarginPastTheRefreshPoint(t *testing.T) {
	due := time.Date(2026, 11, 24, 12, 4, 30, 0, time.UTC)
	ttl := 300 * time.Second // the refresh point is 30 s before the end
	for at, want := range map[time.Duration]bool{0: false, 15 * time.Second: false, 15*time.Second + 1: true, 6 * time.Minute: true} {
		if got := lateRefresh(due.Add(at), due, ttl); got != want {
			t.Errorf("%v past the refresh point: late %v, want %v", at, got, want)
		}
	}
}

// Codex review of #172, round 1: Linux's monotonic clock stops while the
// host is suspended, and a cache entry's lifetime does not, so the
// keeper's times carry no monotonic reading.
func TestTheKeepersClockCountsTimeTheHostSlept(t *testing.T) {
	now := newKeeperClock().Now()
	if strings.Contains(now.String(), "m=") || now != now.Round(0) {
		t.Fatalf("the keeper's clock reads %s, with a monotonic reading", now)
	}
}

// sleepyHost is host timers whose wall clock the test moves, and whose
// timers count only the time it runs, as Go's timers on Linux count no
// suspend.
type sleepyHost struct {
	mu     sync.Mutex
	wall   time.Time
	awake  time.Duration
	timers []*sleepyTimer
}

type sleepyTimer struct {
	host *sleepyHost
	at   time.Duration
	f    func()
}

func (h *sleepyHost) Now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wall
}

func (h *sleepyHost) AfterFunc(d time.Duration, f func()) keeperTimer {
	h.mu.Lock()
	defer h.mu.Unlock()
	timer := &sleepyTimer{host: h, at: h.awake + d, f: f}
	h.timers = append(h.timers, timer)
	return timer
}

func (t *sleepyTimer) Stop() bool {
	h := t.host
	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.Index(h.timers, t)
	if i < 0 {
		return false
	}
	h.timers = slices.Delete(h.timers, i, i+1)
	return true
}

// sleep moves the wall clock alone, as a suspend does, or back, as a
// clock set back does.
func (h *sleepyHost) sleep(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wall = h.wall.Add(d)
}

// run lets the host run for d, firing each timer due on the way, in
// order, at its time; a timer may start another.
func (h *sleepyHost) run(d time.Duration) {
	h.mu.Lock()
	end := h.awake + d
	for {
		soonest := -1
		for i, timer := range h.timers {
			if timer.at <= end && (soonest < 0 || timer.at < h.timers[soonest].at) {
				soonest = i
			}
		}
		if soonest < 0 {
			break
		}
		timer := h.timers[soonest]
		h.timers = slices.Delete(h.timers, soonest, soonest+1)
		h.wall = h.wall.Add(timer.at - h.awake)
		h.awake = timer.at
		h.mu.Unlock()
		timer.f()
		h.mu.Lock()
	}
	h.wall = h.wall.Add(end - h.awake)
	h.awake = end
	h.mu.Unlock()
}

// Codex review of #172, round 2: Go's timers count no suspend, so the
// keeper's timers wait in short steps against the wall clock: one due
// while the host slept fires soon after it wakes, not a suspend's length
// late.
func TestTheKeepersTimersCountTimeTheHostSlept(t *testing.T) {
	host := &sleepyHost{wall: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	clock := wallClock{host: host}
	start := host.Now()
	var fired time.Time
	clock.At(start.Add(270*time.Second), func() { fired = host.Now() })
	host.run(10 * time.Second)
	host.sleep(time.Minute)
	host.run(10 * time.Minute)
	if fired.IsZero() {
		t.Fatal("the timer never fired")
	}
	if late := fired.Sub(start) - 270*time.Second; late < 0 || late > wallStep {
		t.Fatalf("the timer fired %v after its time, past the host's minute asleep", late)
	}
	// A timer stopped never fires.
	stopped := false
	timer := clock.At(host.Now().Add(time.Minute), func() { stopped = true })
	if !timer.Stop() || timer.Stop() {
		t.Fatal("Stop says a stopped timer was running")
	}
	host.run(time.Hour)
	if stopped || len(host.timers) != 0 {
		t.Fatalf("a stopped timer fired %v, or left %d steps", stopped, len(host.timers))
	}
	// One whose time has passed fires at once.
	now := false
	clock.At(host.Now().Add(-time.Second), func() { now = true })
	host.run(0)
	if !now {
		t.Fatal("a timer whose time has passed does not fire")
	}
}

// Codex review of #172, round 3: a wall clock that went back would hold a
// timer to its old time, an hour later; it fires at the next step instead,
// and what it runs finds the clock earlier than it last saw.
func TestTheKeepersTimersFireWhenTheClockGoesBack(t *testing.T) {
	host := &sleepyHost{wall: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	clock := wallClock{host: host}
	fired := false
	clock.At(host.Now().Add(270*time.Second), func() { fired = true })
	host.run(10 * time.Second)
	host.sleep(-time.Hour)
	host.run(wallStep)
	if !fired {
		t.Fatal("a timer waits out a clock that went back an hour")
	}
}

func TestIsReplayableOnlyWhereAOneTokenCapKeepsTheCacheKey(t *testing.T) {
	adaptive := true
	anthropic := ai.ProviderMetadata{API: ai.APIAnthropicMessages}
	for _, tc := range []struct {
		name    string
		model   *ai.Model
		options ai.StreamOptions
		want    bool
	}{
		{"no thinking", &ai.Model{ProviderMeta: anthropic}, ai.StreamOptions{}, true},
		{"thinking on another API", &ai.Model{ProviderMeta: ai.ProviderMetadata{API: ai.APIOpenAICompletions}}, ai.StreamOptions{Thinking: ai.ThinkingLevelHigh}, true},
		{"budget thinking on Anthropic Messages", &ai.Model{ProviderMeta: anthropic}, ai.StreamOptions{Thinking: ai.ThinkingLevelHigh}, false},
		{"adaptive thinking on Anthropic Messages", &ai.Model{ProviderMeta: ai.ProviderMetadata{API: ai.APIAnthropicMessages, Compat: &ai.ModelCompat{ForceAdaptiveThinking: &adaptive}}},
			ai.StreamOptions{Thinking: ai.ThinkingLevelHigh}, true},
	} {
		if got := isReplayable(tc.model, tc.options); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPayloadHashLeavesOutOnlyTheOutputCaps(t *testing.T) {
	hash := func(payload any) string {
		t.Helper()
		h, err := payloadHash(payload)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := map[string]any{"model": "m", "system": "rules", "messages": []any{"work"}}
	with := func(fields map[string]any) map[string]any {
		out := maps.Clone(base)
		maps.Copy(out, fields)
		return out
	}
	want := hash(base)
	for _, capped := range []map[string]any{
		{"max_tokens": 1}, {"max_completion_tokens": 1}, {"max_output_tokens": 1}, {"maxTokens": 1},
	} {
		if got := hash(with(capped)); got != want {
			t.Errorf("%v changes the hash", capped)
		}
	}
	for parent, field := range capParents {
		a := hash(with(map[string]any{parent: map[string]any{field: 32000, "other": "x"}}))
		b := hash(with(map[string]any{parent: map[string]any{field: 1, "other": "x"}}))
		c := hash(with(map[string]any{parent: map[string]any{field: 1, "other": "y"}}))
		if a != b || b == c {
			t.Errorf("%s.%s: only the cap may differ (%v %v)", parent, field, a == b, b == c)
		}
	}
	// A struct's payload hashes as the same object does.
	type request struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		Messages  []any  `json:"messages"`
		MaxTokens int    `json:"max_tokens"`
	}
	if got := hash(request{Model: "m", System: "rules", Messages: []any{"work"}, MaxTokens: 8192}); got != want {
		t.Error("a struct payload hashes otherwise")
	}
	if hash(with(map[string]any{"system": "other rules"})) == want || hash(with(map[string]any{"temperature": 0})) == want {
		t.Error("a payload that differs beyond its cap hashes the same")
	}
	if hash([]any{"a"}) == hash([]any{"b"}) {
		t.Error("payloads that are no object hash the same")
	}
	if _, err := payloadHash(map[string]any{"f": func() {}}); err == nil {
		t.Error("a payload that cannot be encoded hashes")
	}
	// The hash runs inside a provider's request: an encoding that panics
	// is an error, and the real request goes on.
	if _, err := payloadHash(map[string]any{"p": panickyPayload{}}); err == nil {
		t.Error("a payload whose encoding panics hashes")
	}
}

// panickyPayload is a payload part whose encoding panics.
type panickyPayload struct{}

func (panickyPayload) MarshalJSON() ([]byte, error) { panic("an encoder that panics") }

// keeperFakeClock is a keeper clock whose timers a test fires by hand.
type keeperFakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*keeperFakeTimer
}

type keeperFakeTimer struct {
	clock *keeperFakeClock
	at    time.Time
	f     func()
}

func newKeeperFakeClock() *keeperFakeClock {
	return &keeperFakeClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
}

// useKeeperClock makes c the clock of every engine's keeper the test
// opens.
func useKeeperClock(t *testing.T) *keeperFakeClock {
	t.Helper()
	c, saved := newKeeperFakeClock(), newKeeperClock
	newKeeperClock = func() keeperClock { return c }
	t.Cleanup(func() { newKeeperClock = saved })
	return c
}

func (c *keeperFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *keeperFakeClock) At(at time.Time, f func()) keeperTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &keeperFakeTimer{clock: c, at: at, f: f}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *keeperFakeTimer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	i := slices.Index(c.timers, t)
	if i < 0 {
		return false
	}
	c.timers = slices.Delete(c.timers, i, i+1)
	return true
}

// pending are the times from now of the timers neither fired nor
// stopped, soonest first.
func (c *keeperFakeClock) pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, timer := range c.timers {
		out = append(out, timer.at.Sub(c.now))
	}
	slices.Sort(out)
	return out
}

// set moves the clock to now, firing nothing.
func (c *keeperFakeClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// fire moves the clock to the soonest timer, unless it is past it, and
// runs it as its goroutine would, to its end; false when none is pending.
func (c *keeperFakeClock) fire() bool {
	c.mu.Lock()
	if len(c.timers) == 0 {
		c.mu.Unlock()
		return false
	}
	soonest := 0
	for i, timer := range c.timers {
		if timer.at.Before(c.timers[soonest].at) {
			soonest = i
		}
	}
	timer := c.timers[soonest]
	c.timers = slices.Delete(c.timers, soonest, soonest+1)
	if timer.at.After(c.now) {
		c.now = timer.at
	}
	c.mu.Unlock()
	timer.f()
	return true
}

// fireNow runs the soonest timer with the clock left where it is, as a
// wallClock timer does when the clock went back.
func (c *keeperFakeClock) fireNow() {
	c.mu.Lock()
	soonest := 0
	for i, timer := range c.timers {
		if timer.at.Before(c.timers[soonest].at) {
			soonest = i
		}
	}
	timer := c.timers[soonest]
	c.timers = slices.Delete(c.timers, soonest, soonest+1)
	c.mu.Unlock()
	timer.f()
}

// shimCall is a request as the shim's provider saw it.
type shimCall struct {
	maxTokens  int
	maxRetries *int
	// ctxRetries is the provider retry budget its context carries.
	ctxRetries int
	// body is its payload as JSON, but for max_tokens.
	body string
	// sent is false when the payload hook refused it.
	sent bool
}

// payloadShim stands in front of a faux provider as a real provider's
// adapter does: it builds a payload of the request, hands it to
// OnPayload, sends nothing when the hook fails, and else streams faux's
// answer. system is a stateful part of every payload, and uncached makes
// faux cache nothing, as a provider does a prompt below its cacheable
// minimum. beforePayload runs as the provider builds the payload, before
// it hands it to OnPayload.
type payloadShim struct {
	mu            sync.Mutex
	system        string
	uncached      bool
	beforePayload func()
	calls         []shimCall
}

func (s *payloadShim) wrap(inner *ai.ModelsProvider) *ai.ModelsProvider {
	p := *inner
	next := inner.StreamSimple
	p.StreamSimple = func(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, options ai.StreamOptions) (*ai.AssistantMessageEventStream, error) {
		s.mu.Lock()
		system, uncached, before := s.system, s.uncached, s.beforePayload
		s.mu.Unlock()
		if before != nil {
			before()
		}
		payload := map[string]any{"model": model.ID, "system": system, "messages": transcript.Messages()}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		payload["max_tokens"] = options.MaxTokens
		call := shimCall{maxTokens: options.MaxTokens, maxRetries: options.MaxRetries, ctxRetries: ai.ProviderMaxRetries(ctx), body: string(body)}
		if options.OnPayload != nil {
			if _, err := options.OnPayload(payload, model); err != nil {
				s.record(call)
				return nil, err
			}
		}
		call.sent = true
		s.record(call)
		if uncached {
			// faux reports no cache read or write for a request that asks
			// for no cache.
			options.CacheRetention = ai.CacheRetentionNone
		}
		return next(ctx, model, transcript, options)
	}
	p.Stream = p.StreamSimple
	return &p
}

func (s *payloadShim) record(call shimCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *payloadShim) recorded() []shimCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *payloadShim) setSystem(system string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.system = system
}

func (s *payloadShim) setUncached(uncached bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uncached = uncached
}

func (s *payloadShim) setBeforePayload(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforePayload = f
}

// keptFaux is a catalog of faux models priced as cost behind a payload
// shim, answering steps: faux-1 with Claude's cache lifetimes, and
// faux-2 with none, as a model the catalog names no lifetime for.
func keptFaux(cost *ai.ModelCost, steps ...ai.FauxResponseStep) (*ai.Models, *ai.FauxProviderHandle, *payloadShim) {
	faux := ai.NewFauxProvider(ai.FauxConfig{Models: []ai.FauxModelDefinition{{ID: "faux-1", Cost: cost}, {ID: "faux-2", Cost: cost}}})
	faux.GetModel("faux-1").PromptCache = ai.ModelPromptCache{"short": 300, "long": 3600}
	faux.SetResponses(steps)
	shim := &payloadShim{system: "the rules"}
	models := ai.CreateModels()
	models.SetProvider(shim.wrap(faux.Provider()))
	return models, faux, shim
}

// lockedBuffer is a log's output, read while the keeper writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// keeperRig is a keeper installed on a kept faux catalog, with a clock
// driven by hand.
type keeperRig struct {
	t      *testing.T
	clock  *keeperFakeClock
	keeper *cacheKeeper
	models *ai.Models
	faux   *ai.FauxProviderHandle
	model  *ai.Model
	shim   *payloadShim
	log    *lockedBuffer
	// hooked counts the calls of the real requests' own payload hook.
	hooked atomic.Int32
}

func newKeeperRig(t *testing.T, cost *ai.ModelCost) *keeperRig {
	t.Helper()
	models, faux, shim := keptFaux(cost)
	r := &keeperRig{t: t, clock: newKeeperFakeClock(), models: models, faux: faux, model: faux.GetModel("faux-1"), shim: shim, log: &lockedBuffer{}}
	r.keeper = newCacheKeeper(slog.New(slog.NewTextHandler(r.log, nil)), r.clock)
	r.keeper.getenv = func(string) string { return "" }
	r.keeper.install(models)
	t.Cleanup(func() { _ = r.keeper.close(context.Background()) })
	return r
}

func (r *keeperRig) request() ai.Context {
	return ai.Context{Messages: []ai.Message{
		ai.SystemMessage{Content: ai.SystemText("You are a coding agent."), Timestamp: 1},
		ai.UserMessage{Content: ai.UserText("work"), Timestamp: 1},
	}}
}

// realOptions are a real request's options, as a generation task gives
// them: its signal, which ends with it, and its own payload hook.
func (r *keeperRig) realOptions(task context.Context, session string, retention ai.CacheRetention) ai.StreamOptions {
	return ai.StreamOptions{SessionID: session, CacheRetention: retention, Signal: task,
		OnPayload: func(any, *ai.Model) (any, error) { r.hooked.Add(1); return nil, nil }}
}

// send makes a real request of session under retention through the
// catalog, answered by step, whose task ends with it; it returns once
// the keeper took the answer.
func (r *keeperRig) send(session string, retention ai.CacheRetention, step ai.FauxResponseStep) *ai.AssistantMessage {
	r.t.Helper()
	return r.sendOn(r.model, session, retention, step)
}

// sendOn is send on model.
func (r *keeperRig) sendOn(model *ai.Model, session string, retention ai.CacheRetention, step ai.FauxResponseStep) *ai.AssistantMessage {
	r.t.Helper()
	r.faux.AppendResponses([]ai.FauxResponseStep{step})
	task, end := context.WithCancel(context.Background())
	message := r.models.StreamSimple(task, model, r.request(), r.realOptions(task, session, retention)).Result()
	end()
	r.keeper.work.Wait()
	if message == nil {
		r.t.Fatal("no answer")
	}
	return message
}

// warmAnswer is what faux answers a warm: a one-token cap is no stop.
func warmAnswer() ai.FauxResponseStep { return answer("w") }

var failedAnswer = ai.FauxStaticStep(ai.FauxResponse{StopReason: "error", ErrorMessage: "the provider is down"})

func readTool(id string) ai.FauxResponseStep {
	return toolTurn("read", map[string]any{"path": "notes.md"}, id)
}

func TestAWarmGoesOutOnceAtItsRefreshPointWithTheSamePayloadAndAOneTokenCap(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	if got := r.send("s", "", readTool("c1")); got.StopReason != ai.StopReasonToolUse {
		t.Fatalf("the real answer: %+v", got)
	}
	if got := r.clock.pending(); !slices.Equal(got, []time.Duration{270 * time.Second}) {
		t.Fatalf("pending timers %v, want one at the refresh point", got)
	}
	if calls := r.shim.recorded(); len(calls) != 1 {
		t.Fatalf("%d requests before the refresh point", len(calls))
	}
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fire()
	calls := r.shim.recorded()
	if len(calls) != 2 || !calls[1].sent {
		t.Fatalf("requests after the refresh point: %+v", calls)
	}
	real, warm := calls[0], calls[1]
	if warm.maxTokens != 1 || warm.maxRetries == nil || *warm.maxRetries != 0 || warm.ctxRetries != 0 {
		t.Fatalf("the warm's cap and retries: %+v", warm)
	}
	if warm.body != real.body || real.maxTokens == 1 {
		t.Fatalf("the warm's payload differs:\n%s\n%s", real.body, warm.body)
	}
	// The real request's own payload hook, its generation's, ran once:
	// the warm runs none of it.
	if got := r.hooked.Load(); got != 1 {
		t.Fatalf("the real request's payload hook ran %d times", got)
	}
	if r.faux.CallCount() != 2 || r.keeper.warming() != 1 {
		t.Fatalf("faux calls %d, warming %d", r.faux.CallCount(), r.keeper.warming())
	}
	if got := r.clock.pending(); !slices.Equal(got, []time.Duration{270 * time.Second}) {
		t.Fatalf("pending timers %v, want the next refresh only", got)
	}
	log := r.log.String()
	if !strings.Contains(log, "the cache keeper warmed a prompt cache") || !strings.Contains(log, "model=faux/faux-1") ||
		!strings.Contains(log, "cached_tokens=") || !strings.Contains(log, "cost=") || strings.Contains(log, "work") {
		t.Fatalf("the warm's log line: %s", log)
	}
}

func TestAFinishedAnswerStartsNoWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", answer("done"))
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after a finished answer", r.keeper.warming(), r.clock.pending())
	}
}

func TestARequestWithNoSessionOrNoCacheStartsNoWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("", "", readTool("c1"))
	r.send("s", ai.CacheRetentionNone, readTool("c2"))
	// The long tier's first refresh comes after the age cap.
	r.send("l", ai.CacheRetentionLong, readTool("c3"))
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v", r.keeper.warming(), r.clock.pending())
	}
	r.model.PromptCache = nil
	r.send("s", "", readTool("c4"))
	if r.keeper.warming() != 0 {
		t.Fatal("a model with no cache lifetime is warmed")
	}
}

func TestAThinkingRequestOnAnthropicMessagesIsWarmedOnlyWhenAdaptive(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.model.ProviderMeta.API = ai.APIAnthropicMessages
	think := func(session string) {
		r.faux.AppendResponses([]ai.FauxResponseStep{readTool("c1")})
		options := r.realOptions(context.Background(), session, "")
		options.Thinking = ai.ThinkingLevelHigh
		r.models.StreamSimple(context.Background(), r.model, r.request(), options).Result()
		r.keeper.work.Wait()
	}
	think("budget")
	if r.keeper.warming() != 0 {
		t.Fatal("budget thinking is warmed")
	}
	adaptive := true
	r.model.ProviderMeta.Compat = &ai.ModelCompat{ForceAdaptiveThinking: &adaptive}
	think("adaptive")
	if r.keeper.warming() != 1 {
		t.Fatal("adaptive thinking is not warmed")
	}
}

func TestTheSessionsNextRequestStopsItsWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	r.clock.set(r.clock.Now().Add(time.Minute))
	r.send("s", "", readTool("c2"))
	if got := r.clock.pending(); !slices.Equal(got, []time.Duration{270 * time.Second}) {
		t.Fatalf("pending timers %v, want the newest request's alone", got)
	}
	r.send("s", "", answer("done"))
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("a finished answer leaves warming %d, timers %v", r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 1: the owner may change a run's model
// between its requests; a request on a model with no cache lifetime
// starts no warm, and retires the one the run's last request left.
func TestARequestOnAModelWithNoCacheLifetimeRetiresTheWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	other := r.faux.GetModel("faux-2")
	if other.PromptCache != nil {
		t.Fatal("faux-2 names a cache lifetime")
	}
	r.sendOn(other, "s", "", readTool("c2"))
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after the model changed", r.keeper.warming(), r.clock.pending())
	}
	if calls := r.shim.recorded(); len(calls) != 2 {
		t.Fatalf("%d requests: the old model's warm went out", len(calls))
	}
}

// Codex review of #172, round 1: a prompt the provider did not cache, as
// one below its cacheable minimum, has no entry to keep warm.
func TestAPromptTheProviderDidNotCacheIsNeverWarmed(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.shim.setUncached(true)
	got := r.send("s", "", readTool("c1"))
	if got.Usage.CacheRead != 0 || got.Usage.CacheWrite != 0 || got.Usage.Input == 0 {
		t.Fatalf("the uncached answer's usage: %+v", got.Usage)
	}
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v for an uncached prompt", r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 1: a cache entry is there from when the
// request's answer begins, so its refresh is due from the request, not
// from the end of a slow answer, and an answer that ends past the
// refresh point starts no warm.
func TestARefreshIsDueFromTheRequestNotTheEndOfItsAnswer(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	slow := func(session string, took time.Duration) {
		started, release := make(chan struct{}), make(chan struct{})
		r.faux.AppendResponses([]ai.FauxResponseStep{blocking(started, release, readCall)})
		done := make(chan struct{})
		go func() {
			r.models.StreamSimple(context.Background(), r.model, r.request(), r.realOptions(context.Background(), session, "")).Result()
			close(done)
		}()
		<-started
		r.clock.set(r.clock.Now().Add(took))
		close(release)
		<-done
		r.keeper.work.Wait()
	}
	slow("a", time.Minute)
	if got := r.clock.pending(); !slices.Equal(got, []time.Duration{210 * time.Second}) {
		t.Fatalf("pending timers %v after a one-minute answer, want the refresh 270 s after the request", got)
	}
	slow("b", 290*time.Second)
	if r.keeper.warming() != 1 {
		t.Fatalf("warming %d: an answer that ended past its refresh point started a warm", r.keeper.warming())
	}
}

// Codex review of #172, round 1: close cancels a warm in flight, and
// waits for its terminal result, whose usage the log counts.
func TestCloseWaitsForAWarmInFlightAndCountsIt(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	started, release := make(chan struct{}), make(chan struct{})
	r.faux.AppendResponses([]ai.FauxResponseStep{blocking(started, release, ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("w")}})})
	fired := make(chan struct{})
	go func() {
		r.clock.fire()
		close(fired)
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- r.keeper.close(context.Background()) }()
	// close has cancelled the warm before the provider ends it.
	<-r.keeper.life.Done()
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	<-fired
	log := r.log.String()
	if !strings.Contains(log, "the cache keeper warmed a prompt cache") || !strings.Contains(log, "warms=1") {
		t.Fatalf("the warm cut short by close is not counted: %s", log)
	}
}

func TestARequestThatUsesNoCacheLeavesTheWarmAlone(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	// A compaction's request, as PiG sends it.
	r.send("s", ai.CacheRetentionNone, answer("## Summary"))
	if r.keeper.warming() != 1 || len(r.clock.pending()) != 1 {
		t.Fatalf("warming %d, timers %v", r.keeper.warming(), r.clock.pending())
	}
}

// ended tells the keeper that key's run ended, seen now.
func (r *keeperRig) ended(key string) { r.keeper.runEnded(key, r.keeper.token(key)) }

// blocking is a faux step that says when faux reached it, and answers
// with response once released.
func blocking(started chan<- struct{}, release <-chan struct{}, response ai.FauxResponse) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
		close(started)
		<-release
		return response.AssistantMessage(), nil
	})
}

var readCall = ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxToolCall("read", map[string]any{"path": "notes.md"}, &ai.FauxToolCallOptions{ID: "c1"})}, StopReason: "toolUse"}

func TestTheRunsEndStopsItsWarmAndNoOther(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("a", "", readTool("c1"))
	r.send("b", "", readTool("c2"))
	r.ended("a")
	if r.keeper.warming() != 1 || len(r.clock.pending()) != 1 {
		t.Fatalf("warming %d, timers %v after a's end", r.keeper.warming(), r.clock.pending())
	}
	r.ended("b")
	r.ended("")
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after b's end", r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 1: a provider session lasts across runs, so
// a run's end seen late must not stop the next run's warm.
func TestALateRunEndLeavesALaterRequestsWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	// The watch read the end of that run before the next run's request.
	seen := r.keeper.token("s")
	r.send("s", "", readTool("c2"))
	r.keeper.runEnded("s", seen)
	if r.keeper.warming() != 1 || !slices.Equal(r.clock.pending(), []time.Duration{270 * time.Second}) {
		t.Fatalf("warming %d, timers %v after a late run end", r.keeper.warming(), r.clock.pending())
	}
	// A request that asks for no cache retires nothing, and counts for
	// nothing.
	seen = r.keeper.token("s")
	r.send("s", ai.CacheRetentionNone, answer("## Summary"))
	r.keeper.runEnded("s", seen)
	if r.keeper.warming() != 0 {
		t.Fatal("a run's end after a compaction's request leaves its warm")
	}
}

func TestARunThatEndedWhileItsRequestRanStartsNoWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	started, release := make(chan struct{}), make(chan struct{})
	r.faux.AppendResponses([]ai.FauxResponseStep{blocking(started, release, readCall)})
	done := make(chan *ai.AssistantMessage)
	go func() {
		done <- r.models.StreamSimple(context.Background(), r.model, r.request(), r.realOptions(context.Background(), "s", "")).Result()
	}()
	<-started
	r.ended("s")
	close(release)
	if got := <-done; got == nil || got.StopReason != ai.StopReasonToolUse {
		t.Fatalf("the answer: %+v", got)
	}
	r.keeper.work.Wait()
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v", r.keeper.warming(), r.clock.pending())
	}
}

func TestAWarmThatFailsStopsWarming(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	r.faux.AppendResponses([]ai.FauxResponseStep{failedAnswer})
	r.clock.fire()
	if r.faux.CallCount() != 2 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 2: a warm the provider cached nothing of
// shows there is no entry left to keep, and the next refresh is decided
// on what the last warm found.
func TestAWarmThatCachedNothingStopsWarming(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer(), warmAnswer()})
	r.clock.fire()
	r.keeper.mu.Lock()
	e := r.keeper.entries["s"]
	r.keeper.mu.Unlock()
	if e == nil || e.cached == 0 || e.uncached != 0 {
		t.Fatalf("the entry after a warm that read the cache: %+v", e)
	}
	r.shim.setUncached(true)
	r.clock.fire()
	if r.faux.CallCount() != 3 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v after a warm that cached nothing", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
	if !strings.Contains(r.log.String(), "cached nothing") {
		t.Fatalf("the stop names no reason: %s", r.log.String())
	}
}

// Codex review of #172, round 2: the keeper keeps a session's token only
// while it has a request followed or a warm under way, and a token is
// never drawn twice, so a stale one matches no later request.
func TestTheKeeperKeepsATokenOnlyWhileThereIsSomethingToRetire(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	tokens := func() int {
		r.keeper.mu.Lock()
		defer r.keeper.mu.Unlock()
		return len(r.keeper.tokens)
	}
	r.send("a", "", answer("done"))
	r.sendOn(r.faux.GetModel("faux-2"), "b", "", readTool("c1"))
	r.send("c", ai.CacheRetentionNone, readTool("c2"))
	if got := tokens(); got != 0 {
		t.Fatalf("%d tokens kept for sessions with nothing to retire", got)
	}
	r.send("d", "", readTool("c3"))
	first := r.keeper.token("d")
	if first == 0 || tokens() != 1 {
		t.Fatalf("a warm under way has token %d, %d kept", first, tokens())
	}
	r.ended("d")
	if got := tokens(); got != 0 || r.keeper.token("d") != 0 {
		t.Fatalf("%d tokens kept after the run's end", got)
	}
	// The same session's next warm has a token of its own: the end of the
	// run before, seen late, stops nothing.
	r.send("d", "", readTool("c4"))
	if again := r.keeper.token("d"); again == first || again == 0 {
		t.Fatalf("a later request's token %d, the earlier %d", again, first)
	}
	r.keeper.runEnded("d", first)
	if r.keeper.warming() != 1 {
		t.Fatal("a stale token stopped a later warm")
	}
}

func TestAWarmWhosePayloadWouldDifferIsNeverSent(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	// What builds the payload changed since the real request.
	r.shim.setSystem("changed rules")
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fire()
	calls := r.shim.recorded()
	if len(calls) != 2 || calls[1].sent {
		t.Fatalf("requests %+v, want the warm refused", calls)
	}
	if r.faux.CallCount() != 1 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
	if strings.Contains(r.log.String(), "warmed a prompt cache") {
		t.Fatal("an unsent warm is logged as a warm")
	}
}

func TestTheAgeCapEndsWarmingThirtyMinutesAfterTheRealRequest(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	start := r.clock.Now()
	for range 10 {
		r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	}
	fires := 0
	for r.clock.fire() {
		if fires++; fires > 20 {
			t.Fatal("warming does not stop")
		}
	}
	// Warms at 270 s apart, from the real request: 6 inside 30 minutes,
	// then the cap's own timer.
	if warms := r.faux.CallCount() - 1; warms != 6 || fires != 7 {
		t.Fatalf("%d warms in %d fires", warms, fires)
	}
	if got := r.clock.Now().Sub(start); got != maxWarmAge {
		t.Fatalf("warming stopped %v after the real request", got)
	}
	if r.keeper.warming() != 0 || !strings.Contains(r.log.String(), "warms=6") {
		t.Fatalf("warming %d, log %s", r.keeper.warming(), r.log.String())
	}
}

func TestARefreshThatFiresLateSendsNothing(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	// The host slept through the refresh point and the entry's end.
	r.clock.set(r.clock.Now().Add(6 * time.Minute))
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fire()
	if r.faux.CallCount() != 1 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
}

func TestRefreshStaleSaysWhyARefreshCannotGo(t *testing.T) {
	checked := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	started, ttl := checked, 300*time.Second
	due := checked.Add(270 * time.Second)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{due, ""},
		{checked.Add(-time.Second), "the clock went back"},
		// A timer that fired before its time saw the clock go back.
		{due.Add(-time.Second), "the clock went back"},
		{started.Add(maxWarmAge), "the age cap"},
		{due.Add(16 * time.Second), "a late refresh"},
	} {
		if got := refreshStale(tc.at, checked, started, due, ttl); got != tc.want {
			t.Errorf("at %v: %q, want %q", tc.at.Sub(checked), got, tc.want)
		}
	}
}

// Codex review of #172, round 3: the provider builds the warm's payload
// on its own time, and the host may sleep before it does, so the warm's
// payload hook checks its refresh again before the warm goes.
func TestAWarmThatWentStaleBeforeItsPayloadIsNeverSent(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	// The host slept between the refresh point and the provider's payload.
	r.shim.setBeforePayload(func() { r.clock.set(r.clock.Now().Add(6 * time.Minute)) })
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fire()
	calls := r.shim.recorded()
	if len(calls) != 2 || calls[1].sent {
		t.Fatalf("requests %+v, want the warm refused", calls)
	}
	if r.faux.CallCount() != 1 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 3: a wall clock that went back would hold
// an entry's refresh to its old time, long after the entry is gone. The
// keeper's timer fires at the step back, and the keeper retires the
// entry, whose age it can no longer tell.
func TestAClockThatWentBackRetiresTheWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	r.clock.set(r.clock.Now().Add(-time.Hour))
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fireNow()
	if r.faux.CallCount() != 1 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v after the clock went back", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 4: a clock that goes back to a time still
// after the refresh was set (12:03 back to 12:02, set at 12:00, due at
// 12:04:30) makes the keeper's timer fire before its time; the keeper
// retires the entry then too.
func TestAClockThatWentBackAfterTheRefreshWasSetRetiresTheWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	set := r.clock.Now()
	r.clock.set(set.Add(3 * time.Minute))
	r.clock.set(set.Add(2 * time.Minute))
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fireNow()
	if r.faux.CallCount() != 1 || r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("faux calls %d, warming %d, timers %v after the clock went back past the refresh's time", r.faux.CallCount(), r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 5: a clock that went back while a warm was
// in flight, its answer coming before the timer's next step could see it,
// would count the next refresh from when the warm went out; the keeper
// retires the entry instead.
func TestAClockThatWentBackDuringAWarmRetiresTheWarm(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("s", "", readTool("c1"))
	started, release := make(chan struct{}), make(chan struct{})
	r.faux.AppendResponses([]ai.FauxResponseStep{blocking(started, release, ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("w")}})})
	fired := make(chan struct{})
	go func() {
		r.clock.fire()
		close(fired)
	}()
	// The warm went out, its payload checked; the provider answers it.
	<-started
	r.clock.set(r.clock.Now().Add(-10 * time.Second))
	close(release)
	<-fired
	if calls := r.shim.recorded(); len(calls) != 2 || !calls[1].sent {
		t.Fatalf("requests %+v, want the warm sent", calls)
	}
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after the clock went back during a warm", r.keeper.warming(), r.clock.pending())
	}
}

// Codex review of #172, round 3 (P3): a request the keeper starts nothing
// for, superseding one it followed, leaves the session no token.
func TestAnIneligibleRequestDropsTheTokenOfTheOneItSupersedes(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	started, release := make(chan struct{}), make(chan struct{})
	r.faux.AppendResponses([]ai.FauxResponseStep{blocking(started, release, readCall)})
	done := make(chan struct{})
	go func() {
		r.models.StreamSimple(context.Background(), r.model, r.request(), r.realOptions(context.Background(), "s", "")).Result()
		close(done)
	}()
	<-started
	if r.keeper.token("s") == 0 {
		t.Fatal("a followed request has no token")
	}
	r.faux.AppendResponses([]ai.FauxResponseStep{answer("done")})
	r.models.StreamSimple(context.Background(), r.faux.GetModel("faux-2"), r.request(), r.realOptions(context.Background(), "s", "")).Result()
	if got := r.keeper.token("s"); got != 0 {
		t.Fatalf("token %d after an ineligible request superseded the followed one", got)
	}
	close(release)
	<-done
	r.keeper.work.Wait()
	if got := r.keeper.token("s"); got != 0 || r.keeper.warming() != 0 {
		t.Fatalf("token %d, warming %d after the superseded answer came", got, r.keeper.warming())
	}
}

func TestAModelPricedAtNothingIsNeverWarmed(t *testing.T) {
	r := newKeeperRig(t, nil)
	r.send("s", "", readTool("c1"))
	r.faux.AppendResponses([]ai.FauxResponseStep{warmAnswer()})
	r.clock.fire()
	if r.faux.CallCount() != 1 || r.keeper.warming() != 0 {
		t.Fatalf("faux calls %d, warming %d", r.faux.CallCount(), r.keeper.warming())
	}
}

func TestCloseStopsEveryWarmAndStartsNoneAfter(t *testing.T) {
	r := newKeeperRig(t, sonnetCost)
	r.send("a", "", readTool("c1"))
	r.send("b", "", readTool("c2"))
	if r.keeper.warming() != 2 {
		t.Fatalf("warming %d", r.keeper.warming())
	}
	if err := r.keeper.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after close", r.keeper.warming(), r.clock.pending())
	}
	// A real request still runs, and starts no warm.
	if got := r.send("c", "", readTool("c3")); got.StopReason != ai.StopReasonToolUse {
		t.Fatalf("a request after close: %+v", got)
	}
	if r.keeper.warming() != 0 || len(r.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after a request after close", r.keeper.warming(), r.clock.pending())
	}
}

// keptRun is a session of an engine with its keeper on, over a kept faux
// catalog, its keeper's clock driven by hand, whose run waits for the
// owner's approval of a write: a run in a tool call.
type keptRun struct {
	f        *fixture
	e        *Engine
	clock    *keeperFakeClock
	faux     *ai.FauxProviderHandle
	shim     *payloadShim
	id       int64
	approval proto.Approval
}

// keptEngine starts a kept run, whose later requests steps answer.
func keptEngine(t *testing.T, steps ...ai.FauxResponseStep) keptRun {
	t.Helper()
	// The short tier, whatever the environment the tests run in says.
	t.Setenv("PI_CACHE_RETENTION", "")
	r := keptRun{clock: useKeeperClock(t), f: newFixture(t)}
	r.f.cacheKeeper = true
	var models *ai.Models
	models, r.faux, r.shim = keptFaux(sonnetCost, append([]ai.FauxResponseStep{toolTurn("write", map[string]any{"path": "hello.txt", "content": "hi\n"}, "w1")}, steps...)...)
	r.e = r.f.open(models)
	t.Cleanup(func() { _ = r.e.Close(context.Background()) })
	ctx := context.Background()
	var err error
	if r.id, err = r.e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: r.f.work, Model: fauxRef}); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Watch(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Send(ctx, r.id, "u:dev:r1", "write hello"); err != nil {
		t.Fatal(err)
	}
	r.approval = r.f.nextApproval()
	// The keeper took the answer that called the tool.
	r.e.keeper.work.Wait()
	if r.e.keeper.warming() != 1 || !slices.Equal(r.clock.pending(), []time.Duration{270 * time.Second}) {
		t.Fatalf("a run in a tool call: warming %d, timers %v", r.e.keeper.warming(), r.clock.pending())
	}
	return r
}

func TestARunInAToolCallIsKeptWarmOutsideItsTranscript(t *testing.T) {
	run := keptEngine(t, warmAnswer(), answer("Wrote hello.txt."))
	f, e, clock, faux, shim, id := run.f, run.e, run.clock, run.faux, run.shim, run.id
	ctx := context.Background()
	before, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := e.Usage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	clock.fire()
	calls := shim.recorded()
	if len(calls) != 2 || !calls[1].sent || calls[1].maxTokens != 1 || calls[1].body != calls[0].body {
		t.Fatalf("the harness's request and its warm: %+v", calls)
	}
	after, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if kinds(after) != kinds(before) || len(after) != len(before) {
		t.Fatalf("the warm reached the transcript: %s, then %s", kinds(before), kinds(after))
	}
	if got, err := e.Usage(ctx, id); err != nil || got != usage {
		t.Fatalf("the warm reached the usage ledger: %+v, then %+v (%v)", usage, got, err)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+run.approval.ID, run.approval.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	// The run's next request stopped the warm, and its answer started none.
	if e.keeper.warming() != 0 || len(clock.pending()) != 0 || faux.CallCount() != 3 {
		t.Fatalf("warming %d, timers %v, faux calls %d", e.keeper.warming(), clock.pending(), faux.CallCount())
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil || kinds(entries) != "user,assistant,toolResult,assistant" {
		t.Fatalf("transcript %s, %v", kinds(entries), err)
	}
}

func TestAnAbortedRunsWarmStops(t *testing.T) {
	run := keptEngine(t)
	// The watch reported the run busy: an idle state after that is its
	// end, not the one Watch reported first.
	busyAt := -1
	if !run.f.waitLive(func(lives []proto.Live) bool {
		busyAt = slices.IndexFunc(lives, func(l proto.Live) bool { return l.Busy })
		return busyAt >= 0
	}) {
		t.Fatal("the watch never reported the run busy")
	}
	if err := run.e.Abort(context.Background(), run.id); err != nil {
		t.Fatal(err)
	}
	// The watch tells the keeper before it hands on the idle state.
	if !run.f.waitLive(func(lives []proto.Live) bool {
		return slices.ContainsFunc(lives[busyAt+1:], func(l proto.Live) bool { return !l.Busy })
	}) {
		t.Fatal("the run did not end")
	}
	if run.e.keeper.warming() != 0 || len(run.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after the abort", run.e.keeper.warming(), run.clock.pending())
	}
}

// Codex review of #172, round 1: the watch may show a run's end after the
// session's next run began, under the same provider session. The engine
// stops the warm only when the session has no run now.
func TestAStaleIdleViewLeavesTheNextRunsWarm(t *testing.T) {
	run := keptEngine(t)
	ctx := context.Background()
	conversation, err := run.e.conversation(ctx, run.id)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := conversation.Watch(ctx, viewWindow())
	if err != nil {
		t.Fatal(err)
	}
	view := watch.Value()
	_, _ = watch.Stop()
	if providerSession(view) == "" {
		t.Fatal("the view names no provider session")
	}
	// The end of an earlier run, seen while this one waits in its tool.
	run.e.noteRunEnded(ctx, run.id, view)
	if run.e.keeper.warming() != 1 || len(run.clock.pending()) != 1 {
		t.Fatalf("warming %d, timers %v after a stale idle view", run.e.keeper.warming(), run.clock.pending())
	}
	if err := run.e.Abort(ctx, run.id); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, run.e, run.id)
	run.e.noteRunEnded(ctx, run.id, view)
	if run.e.keeper.warming() != 0 {
		t.Fatal("the end of a run seen with the session idle leaves its warm")
	}
}

// Codex review of #172, round 2: a failed read of the session's state at
// its run's end proves nothing, and the watch will not report that end
// again: the keeper reads it again soon, with the token as it was, and
// stops the warm of the run that ended.
func TestAFailedReadAtARunsEndIsReadAgain(t *testing.T) {
	run := keptEngine(t)
	var reads atomic.Int32
	run.e.runBusy = func(ctx context.Context, id int64) (bool, error) {
		if reads.Add(1) == 1 {
			return false, errors.New("the store could not be read")
		}
		return run.e.Busy(ctx, id)
	}
	busyAt := -1
	if !run.f.waitLive(func(lives []proto.Live) bool {
		busyAt = slices.IndexFunc(lives, func(l proto.Live) bool { return l.Busy })
		return busyAt >= 0
	}) {
		t.Fatal("the watch never reported the run busy")
	}
	if err := run.e.Abort(context.Background(), run.id); err != nil {
		t.Fatal(err)
	}
	if !run.f.waitLive(func(lives []proto.Live) bool {
		return slices.ContainsFunc(lives[busyAt+1:], func(l proto.Live) bool { return !l.Busy })
	}) {
		t.Fatal("the run did not end")
	}
	if reads.Load() != 1 || run.e.keeper.warming() != 1 || !slices.Equal(run.clock.pending(), []time.Duration{runEndRetry, 270 * time.Second}) {
		t.Fatalf("after a failed read: %d reads, warming %d, timers %v", reads.Load(), run.e.keeper.warming(), run.clock.pending())
	}
	run.clock.fire()
	if reads.Load() != 2 || run.e.keeper.warming() != 0 || len(run.clock.pending()) != 0 {
		t.Fatalf("after the read again: %d reads, warming %d, timers %v", reads.Load(), run.e.keeper.warming(), run.clock.pending())
	}
}

func TestTheEnginesCloseStopsItsKeeper(t *testing.T) {
	run := keptEngine(t)
	if err := run.e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if run.e.keeper.warming() != 0 || len(run.clock.pending()) != 0 {
		t.Fatalf("warming %d, timers %v after Close", run.e.keeper.warming(), run.clock.pending())
	}
}

func TestTheKeeperIsOffUnlessTheEngineIsAskedForIt(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	if e.keeper != nil {
		t.Fatal("an engine opened without CacheKeeper has a keeper")
	}
	// A nil keeper takes every call as a no-op.
	e.keeper.runEnded("s", 0)
	e.noteRunEnded(context.Background(), 1, harness.ConversationView{})
	if err := e.keeper.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
