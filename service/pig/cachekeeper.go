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
// cacheLifetime, refreshDelay, lateRefresh, decide, isReplayable and
// payloadHash.

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

// decide says whether a refresh of a prompt is worth its cost at rates,
// the prompt as the real request's usage reported it: cached tokens the
// provider read from its cache or wrote to it (the entry a warm keeps),
// and uncached ones it read as plain input. Warm when hazard × missCost >
// warmCost: warmCost is a cache read of the cached tokens, the uncached
// ones at the input price, and one output token; missCost a cache write
// of the cached tokens in place of a read, at the input price when the
// model names no write price. A prompt with no cached token has no entry
// to keep, and a model priced at nothing is never warmed.
func decide(cached, uncached int, rates cacheRates, hazard float64) (bool, warmCosts) {
	write := rates.CacheWrite
	if write == 0 {
		write = rates.Input
	}
	hit, plain := float64(cached), float64(uncached)
	c := warmCosts{Warm: (hit*rates.CacheRead + plain*rates.Input + rates.Output) / 1e6, Miss: hit * (write - rates.CacheRead) / 1e6}
	c.Savings = hazard*c.Miss - c.Warm
	return cached > 0 && c.Savings > 0, c
}

// lateRefresh says whether a refresh due at due, of an entry that lives
// for ttl, comes too late at now: more than half the margin between the
// refresh point and the entry's end past it, as after the host slept,
// when the entry may be gone and a warm would pay its rewrite.
func lateRefresh(now, due time.Time, ttl time.Duration) bool {
	delay, _ := refreshDelay(ttl)
	return now.After(due.Add((ttl - delay) / 2))
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

// keeperClock is the keeper's clock and timers, both of which count the
// time the host spent suspended, as a provider's cache entry's lifetime
// does; tests drive their own.
type keeperClock interface {
	Now() time.Time
	// At runs f on a goroutine of its own at wall-clock time at, or soon
	// after; at once when at has passed.
	At(at time.Time, f func()) keeperTimer
}

// keeperTimer is a timer At started.
type keeperTimer interface{ Stop() bool }

// hostTimers are the host's clock and timers: Now counts the time the
// host spent suspended, AfterFunc's durations need not.
type hostTimers interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) keeperTimer
}

// goTimers are the host's: wall-clock times with no monotonic reading,
// so comparing two counts the time the host spent suspended, and Go's
// timers, which on Linux run on the monotonic clock and so count none.
type goTimers struct{}

func (goTimers) Now() time.Time { return time.Now().Round(0) }

func (goTimers) AfterFunc(d time.Duration, f func()) keeperTimer { return time.AfterFunc(d, f) }

// wallStep is the longest a wallClock timer waits on the host's timers
// before it reads the wall clock again: after a suspend it fires at most
// this long past its time, well inside a refresh's margin (lateRefresh).
const wallStep = 5 * time.Second

// wallClock is the keeper's clock on host timers. Its times are the
// host's, and At waits in steps of at most wallStep, comparing wall-clock
// times, so a refresh due before the host wakes fires on waking, not a
// suspend's length late. A timer also fires at once when the wall clock
// went back since its last step: its time no longer says when it is due,
// and what it runs finds the clock earlier than it last saw it (fire).
type wallClock struct{ host hostTimers }

func (c wallClock) Now() time.Time { return c.host.Now() }

func (c wallClock) At(at time.Time, f func()) keeperTimer {
	t := &steppedTimer{host: c.host, at: at, f: f}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armLocked()
	return t
}

// steppedTimer is a wallClock timer.
type steppedTimer struct {
	host hostTimers
	at   time.Time
	f    func()

	mu   sync.Mutex
	step keeperTimer
	// seen is the wall clock at the last step.
	seen time.Time
	// done is set when it fired or was stopped.
	done bool
}

func (t *steppedTimer) armLocked() {
	t.seen = t.host.Now()
	wait := max(0, min(t.at.Sub(t.seen), wallStep))
	t.step = t.host.AfterFunc(wait, t.wake)
}

func (t *steppedTimer) wake() {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	if now := t.host.Now(); now.Before(t.at) && !now.Before(t.seen) {
		t.armLocked()
		t.mu.Unlock()
		return
	}
	t.done = true
	t.mu.Unlock()
	t.f()
}

func (t *steppedTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	t.step.Stop()
	return true
}

// newKeeperClock is the clock of an engine's keeper; tests replace it.
var newKeeperClock = func() keeperClock { return wallClock{host: goTimers{}} }

// errWarmDiffers fails a warm whose payload is not the real request's, so
// the provider never sends it.
var errWarmDiffers = errors.New("the warm's payload differs from the cached request's")

// errWarmStale fails a warm whose refresh went stale (refreshStale) before
// the provider sent it, so it never pays a rewrite.
var errWarmStale = errors.New("the warm's refresh went stale before it was sent")

// cacheKeeper keeps the prompt cache entries of runs that wait on a tool
// call warm (ADR-0074). It is safe for concurrent use.
type cacheKeeper struct {
	log    *slog.Logger
	clock  keeperClock
	getenv func(string) string

	// life ends at close: every warm's request context, and every wait on
	// a real request's result, is under it. A warm's own result is waited
	// for whatever life says, so its usage is counted.
	life context.Context
	end  context.CancelFunc

	mu     sync.Mutex
	closed bool
	// entries are the warms under way, and requests the real requests
	// whose result the keeper waits for, by provider session id.
	entries  map[string]*warmEntry
	requests map[string]*realRequest
	// tokens hold, for each session with a request followed or a warm
	// under way, the token of the real request that retired its warm last,
	// drawn from seq, which never repeats one: a run's end stops only what
	// came before a later request (runEnded). A session with neither has
	// no token.
	seq    uint64
	tokens map[string]uint64
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
	// dispatched is when it went to the provider: its cache entry is
	// there from when the answer begins, not when it ends, so its
	// lifetime counts from here.
	dispatched time.Time
	// hash is its final payload's (payloadHash), "" until the provider
	// built it or when it could not be hashed.
	hash atomic.Pointer[string]
}

// warmEntry is a cache entry the keeper keeps warm.
type warmEntry struct {
	call warmCall
	hash string
	ttl  time.Duration
	// cached and uncached are the real request's prompt tokens the
	// provider cached, and those it read as plain input (decide).
	cached, uncached int
	// started is when the real request's answer came: the age cap counts
	// from here, never from a warm.
	started time.Time
	// written is when the request that last wrote or read the entry,
	// real or warm, went to the provider, and due when the next refresh
	// should go out: written plus refreshDelay.
	written, due time.Time
	// checked is the clock when the refresh was set: a clock earlier
	// than it went back, and the entry's age can no longer be told.
	checked  time.Time
	timer    keeperTimer
	deadline keeperTimer
	// cancel ends the warm in flight, and stopped says why the entry
	// stopped while one was in flight: its summary waits for that warm's
	// usage.
	cancel  context.CancelFunc
	stopped string
	warms   int
	spent   float64
}

func newCacheKeeper(log *slog.Logger, clock keeperClock) *cacheKeeper {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	life, end := context.WithCancel(context.Background())
	return &cacheKeeper{log: log, clock: clock, getenv: os.Getenv, life: life, end: end,
		entries: map[string]*warmEntry{}, requests: map[string]*realRequest{}, tokens: map[string]uint64{}}
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

// begin takes a real request: it retires its session's warm, and one the
// keeper may warm after is returned to follow, nil for one it leaves
// alone. Only a request with no session, or one that asks for no cache
// (retention none, as a compaction's), leaves the session's warm as it
// is: a request on a model with no cache lifetime, as after the owner
// changed a run's model, retires it though it starts none.
func (k *cacheKeeper) begin(call warmCall) *realRequest {
	key := call.options.SessionID
	if key == "" || call.options.CacheRetention == ai.CacheRetentionNone {
		return nil
	}
	ttl, cached := cacheLifetime(call.model, call.options, k.getenv)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seq++
	delete(k.requests, key)
	k.dropLocked(key, "the session's next request")
	// The request or warm it replaced has nothing left to retire, even
	// when this one starts nothing to follow.
	k.untrackLocked(key)
	delay, ok := refreshDelay(ttl)
	if !cached || k.closed || !ok || delay >= maxWarmAge || !isReplayable(call.model, call.options) {
		return nil
	}
	req := &realRequest{key: key, call: call, ttl: ttl, dispatched: k.clock.Now()}
	k.requests[key] = req
	k.tokens[key] = k.seq
	k.work.Add(1)
	return req
}

// untrackLocked lets go of key's token once it has neither a request
// followed nor a warm under way: there is nothing left to retire.
func (k *cacheKeeper) untrackLocked(key string) {
	if _, ok := k.requests[key]; ok {
		return
	}
	if _, ok := k.entries[key]; ok {
		return
	}
	delete(k.tokens, key)
}

// forget lets go of a real request whose stream never started.
func (k *cacheKeeper) forget(req *realRequest) {
	k.mu.Lock()
	if k.requests[req.key] == req {
		delete(k.requests, req.key)
		k.untrackLocked(req.key)
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
	// Unless it starts a warm, the session has nothing left to retire.
	defer k.untrackLocked(req.key)
	if err != nil || message == nil || k.closed || message.StopReason != ai.StopReasonToolUse {
		return
	}
	hash := req.hash.Load()
	u := message.Usage
	// Only what the provider cached is an entry to keep: a prompt below
	// its cacheable minimum has none.
	cached := u.CacheRead + u.CacheWrite
	delay, ok := refreshDelay(req.ttl)
	now := k.clock.Now()
	// The entry's lifetime counts from the request: an answer that took
	// long may come when its refresh is late already.
	// A clock earlier than the request's went back: its age cannot be told.
	if hash == nil || *hash == "" || cached <= 0 || !ok || now.Before(req.dispatched) || lateRefresh(now, req.dispatched.Add(delay), req.ttl) {
		return
	}
	e := &warmEntry{call: req.call, hash: *hash, ttl: req.ttl, cached: cached, uncached: u.Input, started: now, written: req.dispatched}
	k.entries[req.key] = e
	k.scheduleLocked(req.key, e)
}

// scheduleLocked sets e's next refresh at refreshDelay after the request
// that last wrote or read its entry went out, and its timer for then, or
// for the age cap when that comes first.
func (k *cacheKeeper) scheduleLocked(key string, e *warmEntry) {
	delay, _ := refreshDelay(e.ttl)
	e.due = e.written.Add(delay)
	e.checked = k.clock.Now()
	e.timer = k.clock.At(minTime(e.due, e.started.Add(maxWarmAge)), func() { k.fire(key, e) })
}

// refreshStale says why a refresh of an entry cannot go out at now, ""
// when it can: the clock went back, so the entry's age can no longer be
// told and it may be long gone; the age cap, counted from started; or a
// refresh late past due (lateRefresh). The clock went back when it is
// earlier than when the refresh was set (checked), or than the time the
// refresh's timer was for, its due time or the age cap: a timer fires
// before its time only when it saw the clock go back (wallClock), even
// to a time after checked.
func refreshStale(now, checked, started, due time.Time, ttl time.Duration) string {
	switch {
	case now.Before(checked) || now.Before(minTime(due, started.Add(maxWarmAge))):
		return "the clock went back"
	case now.Sub(started) >= maxWarmAge:
		return "the age cap"
	case lateRefresh(now, due, ttl):
		return "a late refresh"
	}
	return ""
}

// wentBack says whether the clock went back: now is earlier than any of
// the times the keeper read from it before.
func wentBack(now time.Time, earlier ...time.Time) bool {
	for _, t := range earlier {
		if now.Before(t) {
			return true
		}
	}
	return false
}

// minTime is the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// fire is e's refresh point: it warms e, or stops it.
func (k *cacheKeeper) fire(key string, e *warmEntry) {
	k.mu.Lock()
	if k.closed || k.entries[key] != e {
		k.mu.Unlock()
		return
	}
	e.timer = nil
	// The clock counts the time the host slept (wallClock): a timer that
	// fired late would warm an entry that may be gone, whose rewrite costs
	// what the next request's would.
	now := k.clock.Now()
	stop := refreshStale(now, e.checked, e.started, e.due, e.ttl)
	if worth, _ := decide(e.cached, e.uncached, ratesOf(e.call.model), toolHazard); stop == "" && !worth {
		stop = "a warm not worth its cost"
	}
	if stop != "" {
		k.dropLocked(key, stop)
		k.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(k.life)
	e.cancel = cancel
	e.deadline = k.clock.At(e.started.Add(maxWarmAge), func() { k.drop(key, e, "the age cap") })
	// The provider builds the payload on its own time: the host may sleep
	// before it does, so the warm's payload hook asks again.
	checked, started, due, ttl := e.checked, e.started, e.due, e.ttl
	stale := func() bool { return refreshStale(k.clock.Now(), checked, started, due, ttl) != "" }
	k.work.Add(1)
	k.mu.Unlock()
	defer k.work.Done()

	dispatched := k.clock.Now()
	message, sent := k.warm(ctx, e, stale)

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
			"cached_tokens", e.cached, "input", u.Input, "cache_read", u.CacheRead, "cache_write", u.CacheWrite, "output", u.Output,
			"cost", u.Cost.Total, "stop", string(message.StopReason))
	}
	if k.entries[key] != e {
		// It stopped while this warm was in flight: its summary counts
		// this warm too.
		k.logStop(e, e.stopped)
		return
	}
	switch {
	case !sent:
		k.dropLocked(key, "a warm not sent: its payload differed, its refresh went stale, or its stream did not start")
	case message == nil || message.StopReason == ai.StopReasonError || message.StopReason == ai.StopReasonAborted:
		k.dropLocked(key, "a warm that failed")
	case message.Usage.CacheRead+message.Usage.CacheWrite <= 0:
		// The provider cached nothing of the warm: there is no entry left
		// to keep.
		k.dropLocked(key, "a warm the provider cached nothing of")
	case wentBack(k.clock.Now(), dispatched, e.checked):
		// The clock went back while the warm was in flight, maybe before
		// its timer's next step saw it: the next refresh cannot be counted
		// from when the warm went out.
		k.dropLocked(key, "the clock went back")
	default:
		// The next refresh is decided on what this warm found.
		e.cached, e.uncached = message.Usage.CacheRead+message.Usage.CacheWrite, message.Usage.Input
		e.written = dispatched
		k.scheduleLocked(key, e)
	}
}

// warm sends e's real request again with a one-token output cap, and
// returns its answer, nil when it never ended; sent is false when it went
// unsent: its payload not the real request's, its refresh stale by the
// time the provider built it (stale), or its stream not started.
func (k *cacheKeeper) warm(ctx context.Context, e *warmEntry, stale func() bool) (message *ai.AssistantMessage, sent bool) {
	var differs atomic.Bool
	options := warmOptions(ctx, e.call.options, func(payload any, _ *ai.Model) (any, error) {
		if hash, err := payloadHash(payload); err != nil || hash != e.hash {
			differs.Store(true)
			return nil, errWarmDiffers
		}
		// The last moment before the provider sends it: a warm that would
		// find the entry gone pays its rewrite.
		if stale() {
			differs.Store(true)
			return nil, errWarmStale
		}
		return nil, nil
	})
	stream, err := e.call.next(ai.WithProviderMaxRetries(ctx, 0), e.call.model, e.call.transcript, options)
	if err != nil || stream == nil || differs.Load() {
		return nil, false
	}
	// Its terminal result, whatever cut it short: a new request, the run's
	// end and close cancel the request's context, which ends the stream,
	// and its usage still counts. close's own deadline bounds the wait.
	message, err = stream.ResultContext(context.Background())
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

// runEndRetry is how long after a failed read of a session's state the
// keeper reads it again, to stop the warm of a run that ended.
const runEndRetry = time.Second

// noteRunEnded tells the keeper that session id's run ended, as view, the
// watch's, shows it. The watch may show it late, after the session's next
// run began under the same provider session: the keeper's token for the
// session is read first, then the session's state now (checkRunEnded).
func (e *Engine) noteRunEnded(ctx context.Context, id int64, view harness.ConversationView) {
	if e.keeper == nil {
		return
	}
	key := providerSession(view)
	e.checkRunEnded(ctx, id, key, e.keeper.token(key))
}

// checkRunEnded stops provider session key's warm, of session id, when
// the session has no run now and no request began since token was read.
// A read of its state that fails proves nothing: it is read again after
// runEndRetry with the same token, so a later run's request still spares
// its own warm, for as long as there is something to retire.
func (e *Engine) checkRunEnded(ctx context.Context, id int64, key string, token uint64) {
	if !e.keeper.retiring(key, token) {
		return
	}
	read := e.Busy
	if e.runBusy != nil {
		read = e.runBusy
	}
	busy, err := read(ctx, id)
	switch {
	case err != nil:
		e.keeper.after(runEndRetry, func() { e.checkRunEnded(e.keeper.life, id, key, token) })
	case !busy:
		e.keeper.runEnded(key, token)
	}
}

// token is provider session key's token now: that of the real request
// that last retired its warm, 0 when it has no request followed and no
// warm under way. runEnded takes it as read before the session was found
// idle.
func (k *cacheKeeper) token(key string) uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tokens[key]
}

// retiring says whether a run's end seen with token read would still
// stop something: the keeper is open, and key has a request followed or a
// warm under way that no later request retired.
func (k *cacheKeeper) retiring(key string, token uint64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return !k.closed && token != 0 && k.tokens[key] == token
}

// after runs f after d on the keeper's clock, unless the keeper closed by
// then.
func (k *cacheKeeper) after(d time.Duration, f func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	k.clock.At(k.clock.Now().Add(d), func() {
		k.mu.Lock()
		closed := k.closed
		k.mu.Unlock()
		if !closed {
			f()
		}
	})
}

// runEnded stops the warm of provider session key, whose run ended or
// was aborted, no request following its last tool call; token is the
// session's token as read before the end was seen. A provider session
// lasts across runs, so when a later request began since, the warm and
// the request are a later run's, and stay: that request retired whatever
// the ended run left. Tokens never repeat, so a stale one matches nothing
// later.
func (k *cacheKeeper) runEnded(key string, token uint64) {
	if k == nil || key == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tokens[key] != token {
		return
	}
	delete(k.requests, key)
	k.dropLocked(key, "its run's end")
	k.untrackLocked(key)
}

// drop stops e, if it is still key's warm.
func (k *cacheKeeper) drop(key string, e *warmEntry, why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.entries[key] == e {
		k.dropLocked(key, why)
	}
}

// dropLocked stops key's warm, the one in flight included, whose end
// then logs the summary with its own usage (fire).
func (k *cacheKeeper) dropLocked(key, why string) {
	e, ok := k.entries[key]
	if !ok {
		return
	}
	delete(k.entries, key)
	k.untrackLocked(key)
	if e.timer != nil {
		e.timer.Stop()
	}
	if e.deadline != nil {
		e.deadline.Stop()
	}
	if e.cancel != nil {
		e.stopped = why
		e.cancel()
		return
	}
	k.logStop(e, why)
}

// logStop logs what an entry's warms came to, when it had any.
func (k *cacheKeeper) logStop(e *warmEntry, why string) {
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

// close stops every warm, and no warm starts after it. It cancels the
// warms in flight and waits, within ctx, for each one's terminal result,
// so its usage is counted, and for the waits on real requests to end.
// Calling it again waits again.
func (k *cacheKeeper) close(ctx context.Context) error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	k.closed = true
	k.end()
	clear(k.requests)
	for key := range k.entries {
		k.dropLocked(key, "pocketd's stop")
	}
	clear(k.tokens)
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
