package pig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable/harness"
)

// The prompt cache keeper (ADR-0074), ported from pi-pocket's
// cache-keeper.ts: while a run waits on a tool call, it sends the run's
// last model request again with a one-token output cap just before its
// prompt cache entry expires, when that costs less than the rewrite it
// saves. It never warms an idle session. The rules are the pure functions
// cacheLifetime, refreshDelay, decide, isReplayable and payloadHash.

const (
	// maxWarmAge: warming never goes on past this long after the real
	// request that started it, whatever the warms since. It bounds the
	// loss while a tool waits on a person.
	maxWarmAge = 30 * time.Minute
	// toolHazard is the chance of a real request in the next interval
	// after an answer that called a tool: the run goes on when the tool
	// ends.
	toolHazard = 0.9
	// refreshMargin: a refresh is never closer than this to the end of
	// the entry's lifetime.
	refreshMargin = 10 * time.Second
)

// refreshDelay is how long after a request to refresh the cache entry it
// wrote, which lives for ttl: at 90% of the lifetime, and never closer
// than refreshMargin to its end; false for a lifetime of refreshMargin or
// less.
func refreshDelay(ttl time.Duration) (time.Duration, bool) {
	if ttl <= refreshMargin {
		return 0, false
	}
	return max(time.Millisecond, min(ttl/10*9, ttl-refreshMargin)), true
}

// cacheLifetime is how long the prompt cache entry of a request to model
// with options lives: the model's lifetime for the retention the request
// used, resolved as PiG's providers resolve it (an unset retention is
// short, or long when PI_CACHE_RETENTION says so, the request's provider
// environment first); false for retention none, or a model that names no
// lifetime for it.
func cacheLifetime(model *ai.Model, options ai.StreamOptions, getenv func(string) string) (time.Duration, bool) {
	retention := options.CacheRetention
	if retention == "" {
		retention = ai.CacheRetentionShort
		value := options.Env["PI_CACHE_RETENTION"]
		if value == "" && getenv != nil {
			value = getenv("PI_CACHE_RETENTION")
		}
		if value == "long" {
			retention = ai.CacheRetentionLong
		}
	}
	if retention == ai.CacheRetentionNone || model == nil {
		return 0, false
	}
	seconds, ok := model.PromptCache[string(retention)]
	if !ok || seconds <= 0 {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// isReplayable says whether the same request with a one-token output cap
// leaves its cache entry as it is. Anthropic keys the cache on a
// budget-based thinking budget, which follows the cap, so a request that
// thinks on Anthropic Messages replays only on a model that forces
// adaptive thinking (PiG's own IsReplayable).
func isReplayable(model *ai.Model, options ai.StreamOptions) bool {
	if options.Thinking == "" || model.ProviderMeta.API != ai.APIAnthropicMessages {
		return true
	}
	compat := model.ProviderMeta.Compat
	return compat != nil && compat.ForceAdaptiveThinking != nil && *compat.ForceAdaptiveThinking
}

// cacheRates are a model's prices, in dollars per million tokens.
type cacheRates struct {
	Input, Output, CacheRead, CacheWrite float64
}

// ratesOf are model's base prices, as its catalog entry names them.
func ratesOf(model *ai.Model) cacheRates {
	c := model.Capabilities
	return cacheRates{Input: c.InputCostPer1M, Output: c.OutputCostPer1M, CacheRead: c.CacheReadCostPer1M, CacheWrite: c.CacheWriteCostPer1M}
}

// warmCosts are one refresh's economics, in dollars: warm is what the
// refresh costs, miss what the next real request pays more if the entry
// is gone, and savings hazard × miss − warm.
type warmCosts struct {
	Warm, Miss, Savings float64
}

// decide says whether a refresh of a prompt of promptTokens is worth its
// cost at rates: warm when hazard × missCost > warmCost. warmCost is a
// cache read of the whole prompt plus one output token; missCost a cache
// write of it in place of a read, at the input price when the model names
// no write price. A model priced at nothing is never warmed.
func decide(promptTokens int, rates cacheRates, hazard float64) (bool, warmCosts) {
	write := rates.CacheWrite
	if write == 0 {
		write = rates.Input
	}
	tokens := float64(promptTokens)
	c := warmCosts{Warm: (tokens*rates.CacheRead + rates.Output) / 1e6, Miss: tokens * (write - rates.CacheRead) / 1e6}
	c.Savings = hazard*c.Miss - c.Warm
	return c.Savings > 0, c
}

// capParents are the payload objects that hold an output cap one level
// down, and capFields the cap fields at the top or in one of them: the
// only fields a warm may change, as each API names them.
var (
	capFields  = []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "maxTokens"}
	capParents = map[string]string{"options": "maxTokens", "config": "maxOutputTokens", "inferenceConfig": "maxTokens", "generationConfig": "maxOutputTokens"}
)

// errUnhashable is a payload whose encoding panicked.
var errUnhashable = errors.New("the payload cannot be hashed")

// payloadHash is the SHA-256 of a provider payload as JSON with its
// output-cap fields left out, the top object's members in name order: a
// warm's payload has the real request's hash, or it is not sent. A
// payload that is no JSON object is hashed as it encodes. It runs inside
// a provider's request, so an encoding that panics is an error, never a
// panic: a payload that cannot be hashed is never warmed, and its real
// request goes as it would have.
func payloadHash(payload any) (hash string, err error) {
	defer func() {
		if recover() != nil {
			hash, err = "", errUnhashable
		}
	}()
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) == nil && top != nil {
		for _, field := range capFields {
			delete(top, field)
		}
		for parent, field := range capParents {
			raw, ok := top[parent]
			if !ok {
				continue
			}
			var inner map[string]json.RawMessage
			if json.Unmarshal(raw, &inner) != nil || inner == nil {
				continue
			}
			delete(inner, field)
			if top[parent], err = json.Marshal(inner); err != nil {
				return "", err
			}
		}
		// A map's members encode in name order.
		if data, err = json.Marshal(top); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// keeperClock is the keeper's clock and timers; tests drive their own.
type keeperClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) keeperTimer
}

// keeperTimer is a timer AfterFunc started.
type keeperTimer interface{ Stop() bool }

// wallClock is the host's clock.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) AfterFunc(d time.Duration, f func()) keeperTimer { return time.AfterFunc(d, f) }

// newKeeperClock is the clock of an engine's keeper; tests replace it.
var newKeeperClock = func() keeperClock { return wallClock{} }

// errWarmDiffers fails a warm whose payload is not the real request's, so
// the provider never sends it.
var errWarmDiffers = errors.New("the warm's payload differs from the cached request's")

// cacheKeeper keeps the prompt cache entries of runs that wait on a tool
// call warm (ADR-0074). It is safe for concurrent use.
type cacheKeeper struct {
	log    *slog.Logger
	clock  keeperClock
	getenv func(string) string

	// life ends at close: every warm's context, and every wait on a real
	// request's result, is under it.
	life context.Context
	end  context.CancelFunc

	mu     sync.Mutex
	closed bool
	// entries are the warms under way, and requests the real requests
	// whose result the keeper waits for, by provider session id.
	entries  map[string]*warmEntry
	requests map[string]*realRequest
	// work counts the waits on real requests' results and the warms in
	// flight, which close waits for.
	work sync.WaitGroup
}

// warmCall is a real request as its provider's stream function took it,
// which a warm sends again.
type warmCall struct {
	next       ai.ModelsStreamFunction
	model      *ai.Model
	transcript ai.TranscriptContext
	options    ai.StreamOptions
}

// realRequest is a real request the keeper may warm after.
type realRequest struct {
	key  string
	call warmCall
	ttl  time.Duration
	// hash is its final payload's (payloadHash), "" until the provider
	// built it or when it could not be hashed.
	hash atomic.Pointer[string]
}

// warmEntry is a cache entry the keeper keeps warm.
type warmEntry struct {
	call   warmCall
	hash   string
	ttl    time.Duration
	prompt int
	// started is when the real request ended: the age cap counts from
	// here, never from a warm.
	started time.Time
	// due is when the next refresh should go out.
	due      time.Time
	timer    keeperTimer
	deadline keeperTimer
	// cancel ends the warm in flight.
	cancel context.CancelFunc
	warms  int
	spent  float64
}

func newCacheKeeper(log *slog.Logger, clock keeperClock) *cacheKeeper {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	life, end := context.WithCancel(context.Background())
	return &cacheKeeper{log: log, clock: clock, getenv: os.Getenv, life: life, end: end,
		entries: map[string]*warmEntry{}, requests: map[string]*realRequest{}}
}

// install wraps the stream functions of every provider of models in
// place, PiG's own and the owner's alike, so every model request passes
// the keeper. Call it once, after the last SetProvider.
func (k *cacheKeeper) install(models *ai.Models) {
	for _, p := range models.GetProviders() {
		if p == nil {
			continue
		}
		wrapped := *p
		if p.Stream != nil {
			wrapped.Stream = k.wrap(p.Stream)
		}
		if p.StreamSimple != nil {
			wrapped.StreamSimple = k.wrap(p.StreamSimple)
		}
		models.SetProvider(&wrapped)
	}
}

// wrap is next with the keeper around it: a real request that uses the
// cache stops its session's warm, and one the keeper may warm after has
// its final payload hashed and its result followed. The request itself
// goes to next as it came, but for that payload hook, which runs the
// harness's own first and returns what it returned.
func (k *cacheKeeper) wrap(next ai.ModelsStreamFunction) ai.ModelsStreamFunction {
	return func(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, options ai.StreamOptions) (*ai.AssistantMessageEventStream, error) {
		req := k.begin(warmCall{next: next, model: model, transcript: transcript, options: options})
		if req == nil {
			return next(ctx, model, transcript, options)
		}
		observed := options
		inner := options.OnPayload
		observed.OnPayload = func(payload any, m *ai.Model) (any, error) {
			var shaped any
			if inner != nil {
				var err error
				if shaped, err = inner(payload, m); err != nil {
					return shaped, err
				}
			}
			final := payload
			if shaped != nil {
				final = shaped
			}
			// A payload that cannot be hashed serves the real request, but
			// is never warmed.
			hash, err := payloadHash(final)
			if err != nil {
				hash = ""
			}
			req.hash.Store(&hash)
			return shaped, nil
		}
		stream, err := next(ctx, model, transcript, observed)
		if err != nil || stream == nil {
			k.forget(req)
			return stream, err
		}
		go k.await(req, stream)
		return stream, nil
	}
}

// begin takes a real request: one that uses the cache stops its session's
// warm, and one the keeper may warm after is returned to follow, nil for
// one it leaves alone. A request with no session or no cache leaves the
// session's warm as it is.
func (k *cacheKeeper) begin(call warmCall) *realRequest {
	key := call.options.SessionID
	ttl, cached := cacheLifetime(call.model, call.options, k.getenv)
	if key == "" || !cached {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.dropLocked(key, "the session's next request")
	delete(k.requests, key)
	delay, ok := refreshDelay(ttl)
	if k.closed || !ok || delay >= maxWarmAge || !isReplayable(call.model, call.options) {
		return nil
	}
	req := &realRequest{key: key, call: call, ttl: ttl}
	k.requests[key] = req
	k.work.Add(1)
	return req
}

// forget lets go of a real request whose stream never started.
func (k *cacheKeeper) forget(req *realRequest) {
	k.mu.Lock()
	if k.requests[req.key] == req {
		delete(k.requests, req.key)
	}
	k.mu.Unlock()
	k.work.Done()
}

// await waits for a real request's result, and starts its session's warm
// when it ended in a tool call and is still its session's last request.
func (k *cacheKeeper) await(req *realRequest, stream *ai.AssistantMessageEventStream) {
	defer k.work.Done()
	message, err := stream.ResultContext(k.life)
	k.mu.Lock()
	defer k.mu.Unlock()
	// A later request of the session, its run's end or close came first.
	if k.requests[req.key] != req {
		return
	}
	delete(k.requests, req.key)
	if err != nil || message == nil || k.closed || message.StopReason != ai.StopReasonToolUse {
		return
	}
	hash := req.hash.Load()
	prompt := message.Usage.Input + message.Usage.CacheRead + message.Usage.CacheWrite
	delay, ok := refreshDelay(req.ttl)
	if hash == nil || *hash == "" || prompt <= 0 || !ok {
		return
	}
	e := &warmEntry{call: req.call, hash: *hash, ttl: req.ttl, prompt: prompt, started: k.clock.Now()}
	k.entries[req.key] = e
	k.scheduleLocked(req.key, e, delay)
}

// scheduleLocked sets e's next refresh delay from now, and its timer for
// then, or for the age cap when that comes first.
func (k *cacheKeeper) scheduleLocked(key string, e *warmEntry, delay time.Duration) {
	now := k.clock.Now()
	e.due = now.Add(delay)
	wait := min(delay, max(0, e.started.Add(maxWarmAge).Sub(now)))
	e.timer = k.clock.AfterFunc(wait, func() { k.fire(key, e) })
}

// fire is e's refresh point: it warms e, or stops it.
func (k *cacheKeeper) fire(key string, e *warmEntry) {
	k.mu.Lock()
	if k.closed || k.entries[key] != e {
		k.mu.Unlock()
		return
	}
	e.timer = nil
	now := k.clock.Now()
	delay, _ := refreshDelay(e.ttl)
	// A timer that fired late, as after the host slept, would warm an
	// entry that may be gone: its rewrite costs what the next request's
	// would.
	slack := (e.ttl - delay) / 2
	stop := ""
	switch worth, _ := decide(e.prompt, ratesOf(e.call.model), toolHazard); {
	case now.Sub(e.started) >= maxWarmAge:
		stop = "the age cap"
	case now.After(e.due.Add(slack)):
		stop = "a late refresh"
	case !worth:
		stop = "a warm not worth its cost"
	}
	if stop != "" {
		k.dropLocked(key, stop)
		k.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(k.life)
	e.cancel = cancel
	e.deadline = k.clock.AfterFunc(e.started.Add(maxWarmAge).Sub(now), func() { k.drop(key, e, "the age cap") })
	k.work.Add(1)
	k.mu.Unlock()
	defer k.work.Done()

	message, sent := k.warm(ctx, e)

	k.mu.Lock()
	defer k.mu.Unlock()
	cancel()
	e.cancel = nil
	if e.deadline != nil {
		e.deadline.Stop()
		e.deadline = nil
	}
	if sent && message != nil {
		// A warm costs what it costs even when a new request or a stop
		// cut it short.
		e.warms++
		e.spent += message.Usage.Cost.Total
		u := message.Usage
		k.log.Info("the cache keeper warmed a prompt cache", "model", e.call.model.ProviderID()+"/"+e.call.model.ID,
			"prompt_tokens", e.prompt, "input", u.Input, "cache_read", u.CacheRead, "cache_write", u.CacheWrite, "output", u.Output,
			"cost", u.Cost.Total, "stop", string(message.StopReason))
	}
	if k.entries[key] != e {
		return
	}
	switch {
	case !sent:
		k.dropLocked(key, "a warm that would differ or did not start")
	case message == nil || message.StopReason == ai.StopReasonError || message.StopReason == ai.StopReasonAborted:
		k.dropLocked(key, "a warm that failed")
	default:
		k.scheduleLocked(key, e, delay)
	}
}

// warm sends e's real request again with a one-token output cap, and
// returns its answer, nil when it never ended; sent is false when it went
// unsent, its payload not the real request's, or its stream not started.
func (k *cacheKeeper) warm(ctx context.Context, e *warmEntry) (message *ai.AssistantMessage, sent bool) {
	var differs atomic.Bool
	options := warmOptions(ctx, e.call.options, func(payload any, _ *ai.Model) (any, error) {
		if hash, err := payloadHash(payload); err != nil || hash != e.hash {
			differs.Store(true)
			return nil, errWarmDiffers
		}
		return nil, nil
	})
	stream, err := e.call.next(ai.WithProviderMaxRetries(ctx, 0), e.call.model, e.call.transcript, options)
	if err != nil || stream == nil || differs.Load() {
		return nil, false
	}
	// The keeper's life, not the warm's context: a warm a new request or
	// a stop aborted still ends with its usage.
	message, err = stream.ResultContext(k.life)
	switch {
	case differs.Load():
		return nil, false
	case err != nil:
		return nil, true
	}
	return message, true
}

// warmOptions are a real request's options for its warm: the same
// request, with a one-token output cap, no retry, the warm's context, and
// nothing of the generation task that sent it, which may have ended: its
// payload hook (onPayload in its place), telemetry span and response
// hooks.
func warmOptions(ctx context.Context, real ai.StreamOptions, onPayload func(any, *ai.Model) (any, error)) ai.StreamOptions {
	o := real
	o.MaxTokens = 1
	o.MaxRetries = new(int)
	o.Signal = ctx
	o.Telemetry = nil
	o.OnPayload = onPayload
	o.OnResponse = nil
	o.OnProviderStreamEvent = nil
	o.ProviderStreamEventWanted = nil
	o.TransformHeaders = nil
	return o
}

// providerSession is the provider session id a conversation's requests
// go under, as its view's pi.provider document holds it: the keeper's key
// of its warm. "" when the view holds none yet.
func providerSession(view harness.ConversationView) string {
	doc, ok := view.Docs.Get(harness.ProviderDoc.AnyDefinition().Kind)
	if !ok {
		return ""
	}
	value, _ := doc.Get("sessionId")
	id, _ := value.(string)
	return id
}

// runEnded stops the warm of provider session key, whose run ended or
// was aborted: no request follows its last tool call.
func (k *cacheKeeper) runEnded(key string) {
	if k == nil || key == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.requests, key)
	k.dropLocked(key, "its run's end")
}

// drop stops e, if it is still key's warm.
func (k *cacheKeeper) drop(key string, e *warmEntry, why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.entries[key] == e {
		k.dropLocked(key, why)
	}
}

// dropLocked stops key's warm, the one in flight included.
func (k *cacheKeeper) dropLocked(key, why string) {
	e, ok := k.entries[key]
	if !ok {
		return
	}
	delete(k.entries, key)
	if e.timer != nil {
		e.timer.Stop()
	}
	if e.deadline != nil {
		e.deadline.Stop()
	}
	if e.cancel != nil {
		e.cancel()
	}
	if e.warms > 0 {
		k.log.Info("the cache keeper stopped warming a prompt cache", "model", e.call.model.ProviderID()+"/"+e.call.model.ID,
			"warms", e.warms, "cost", e.spent, "why", why)
	}
}

// warming is how many entries the keeper keeps warm.
func (k *cacheKeeper) warming() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}

// close stops every warm, and no warm starts after it. It waits, within
// ctx, for the warms in flight and the waits on real requests to end.
// Calling it again waits again.
func (k *cacheKeeper) close(ctx context.Context) error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	k.closed = true
	k.end()
	for key := range k.entries {
		k.dropLocked(key, "pocketd's stop")
	}
	clear(k.requests)
	k.mu.Unlock()
	done := make(chan struct{})
	go func() {
		k.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the cache keeper's warms did not end: %w", ctx.Err())
	}
}
