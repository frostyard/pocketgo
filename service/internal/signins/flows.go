package signins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The bounds of a running flow (rule 69).
const (
	// maxFailures is how many polls in a row may fail to reach GitHub, or
	// get a 5xx or a 429, before the flow fails.
	maxFailures = 3
	// keepTimeout bounds the keeping of an approved sign-in: the account's
	// read, the exchange for Copilot's token and the write.
	keepTimeout = 2 * time.Minute
)

// ErrNoDeviceFlow is a provider pocketd does not sign in to with a device
// flow it runs: it signs in on the service host (pocketd login).
var ErrNoDeviceFlow = errors.New("pocketd signs in to this provider on the service host alone, with pocketd login")

// ErrStopped is a start once Stop ran.
var ErrStopped = errors.New("pocketd is stopping")

// ErrGone is a start whose request key answered a flow pocketd no longer
// keeps: a later start of the provider replaced it.
var ErrGone = errors.New("the device flow this request started is gone")

// Keep keeps provider's sign-in from token, the GitHub token an approved
// device flow got, through the service's one credential writer, with
// account, the login GitHub named for it ("" for none). It never logs or
// returns token.
type Keep func(ctx context.Context, provider, token, account string) error

// Options configure Flows.
type Options struct {
	// GitHub is GitHub's device flow; nil is one on DefaultHTTP.
	GitHub *GitHub
	// Keep keeps an approved flow's sign-in. Required.
	Keep Keep
	// Changed runs after a flow of provider starts or ends, outside the
	// flows' lock: the caller publishes the provider's sign-in. nil does
	// nothing.
	Changed func(provider string)
	// Now is the clock a flow's start and its code's expiry are read on,
	// and After the wait between polls; nil are time.Now and time.After.
	// Tests give a clock of their own.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// NewID makes a flow's id; nil is 16 random bytes in hex.
	NewID func() string
}

// Flows runs the device flows pocketd signs in with (ADR-0062): one a
// provider at a time, polled at GitHub's interval, slow_down honored,
// until the code is approved, denied or expired, or a client cancels it.
// It keeps each provider's newest flow, running or ended, in memory: a
// flow does not outlive pocketd. It is safe for concurrent use.
type Flows struct {
	o    Options
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
	// startMu runs one start at a time, so two never both ask GitHub for a
	// code; writeMu orders every write of a sign-in Flows makes or takes:
	// an approved flow's keep and a sign-out (SignOut). mu guards the
	// rest, and is taken last.
	startMu sync.Mutex
	writeMu sync.Mutex
	mu      sync.Mutex
	latest  map[string]*flow
	byID    map[string]*flow
	// byKey are the flows the starts answered, by their request's key,
	// taken before a new flow polls: a start that answered once answers
	// the same flow, whatever became of the request's record.
	byKey   map[string]string
	stopped bool
}

type flow struct {
	wire    proto.SignInFlow
	expires time.Time
	cancel  context.CancelFunc
	// keeping says the approved flow's sign-in is being kept: it ends
	// approved or failed, and a cancel no longer takes it.
	keeping bool
}

// New returns flows that run until Stop.
func New(o Options) *Flows {
	if o.Keep == nil {
		panic("signins.New: Options.Keep is required")
	}
	if o.GitHub == nil {
		o.GitHub = &GitHub{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.NewID == nil {
		o.NewID = newID
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Flows{o: o, ctx: ctx, stop: stop, latest: map[string]*flow{}, byID: map[string]*flow{}, byKey: map[string]string{}}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Start starts provider's device flow for the request key: it asks GitHub
// for a code, and polls for the owner's approval until the code expires.
// A provider with a running flow answers that one, and started is false.
// A key that answered a flow before answers it again, as it is now, and
// starts nothing; ErrGone when a later start replaced it. The key is
// taken in the same lock that keeps a new flow, before it polls (Codex
// review of #123, round 1). A start GitHub did not answer with a code is
// a flow that failed, with why: it is kept and answered as any other.
func (f *Flows) Start(provider, key string) (out proto.SignInFlow, started bool, err error) {
	if p, ok := Find(provider); !ok || !p.Device {
		return proto.SignInFlow{}, false, ErrNoDeviceFlow
	}
	f.startMu.Lock()
	defer f.startMu.Unlock()
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return proto.SignInFlow{}, false, ErrStopped
	}
	if id, ok := f.byKey[key]; ok {
		defer f.mu.Unlock()
		if fl, kept := f.byID[id]; kept {
			return fl.wire, false, nil
		}
		return proto.SignInFlow{}, false, ErrGone
	}
	if fl := f.latest[provider]; fl != nil && !fl.wire.State.Ended() {
		f.byKey[key] = fl.wire.ID
		out = fl.wire
		f.mu.Unlock()
		return out, false, nil
	}
	f.mu.Unlock()

	at := f.o.Now().UTC()
	code, err := f.o.GitHub.Start(f.ctx)
	fl := &flow{wire: proto.SignInFlow{ID: f.o.NewID(), Provider: provider, StartedAt: at}}
	var run context.Context
	if err != nil {
		fl.wire.State, fl.wire.Error, fl.wire.EndedAt = proto.FlowFailed, "GitHub's device flow did not start: "+err.Error(), f.o.Now().UTC()
	} else {
		fl.expires = at.Add(code.ExpiresIn)
		fl.wire.State, fl.wire.UserCode, fl.wire.VerificationURI = proto.FlowWaiting, code.UserCode, code.VerificationURI
		fl.wire.ExpiresIn, fl.wire.Interval, fl.wire.ExpiresAt = seconds(code.ExpiresIn), seconds(code.Interval), fl.expires
		run, fl.cancel = context.WithCancel(f.ctx)
	}
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		if fl.cancel != nil {
			fl.cancel()
		}
		return proto.SignInFlow{}, false, ErrStopped
	}
	if old := f.latest[provider]; old != nil {
		delete(f.byID, old.wire.ID)
	}
	f.latest[provider], f.byID[fl.wire.ID], f.byKey[key] = fl, fl, fl.wire.ID
	out = fl.wire
	if run != nil {
		f.wg.Add(1)
		go f.run(run, fl, code)
	}
	f.mu.Unlock()
	f.changed(provider)
	return out, true, nil
}

func seconds(d time.Duration) int { return int(d / time.Second) }

// run polls GitHub for flow fl until it ends: at the code's interval,
// widened by each slow_down, first after one interval (RFC 8628, 3.4),
// never past the code's expiry.
func (f *Flows) run(ctx context.Context, fl *flow, code Code) {
	defer f.wg.Done()
	interval, failures := code.Interval, 0
	for {
		if wait := min(interval, fl.expires.Sub(f.o.Now())); wait > 0 {
			select {
			case <-ctx.Done():
				f.end(fl, proto.FlowCanceled, "", "")
				return
			case <-f.o.After(wait):
			}
		}
		if ctx.Err() != nil {
			f.end(fl, proto.FlowCanceled, "", "")
			return
		}
		if !f.o.Now().Before(fl.expires) {
			f.end(fl, proto.FlowExpired, "The code expired before it was approved at "+DevicePage+".", "")
			return
		}
		poll, err := f.o.GitHub.Poll(ctx, code.DeviceCode)
		switch {
		case ctx.Err() != nil:
			f.end(fl, proto.FlowCanceled, "", "")
			return
		case errors.Is(err, errTransient):
			if failures++; failures >= maxFailures {
				f.end(fl, proto.FlowFailed, err.Error(), "")
				return
			}
		case err != nil:
			f.end(fl, proto.FlowFailed, err.Error(), "")
			return
		case poll.Token != "":
			f.keep(fl, poll.Token)
			return
		case poll.Pending:
			failures = 0
		case poll.SlowDown:
			failures = 0
			interval = SlowedDown(interval, poll.Interval)
			f.mu.Lock()
			fl.wire.Interval = seconds(interval)
			f.mu.Unlock()
		case poll.Denied:
			f.end(fl, proto.FlowDenied, "The code was denied at "+DevicePage+".", "")
			return
		case poll.Expired:
			f.end(fl, proto.FlowExpired, "The code expired before it was approved at "+DevicePage+".", "")
			return
		default:
			f.end(fl, proto.FlowFailed, poll.Refused, "")
			return
		}
	}
}

// keep keeps an approved flow's sign-in under writeMu, unless a cancel or
// a sign-out ended the flow first: the account GitHub names, then Keep.
// An error never carries token.
func (f *Flows) keep(fl *flow, token string) {
	ctx, cancel := context.WithTimeout(f.ctx, keepTimeout)
	defer cancel()
	account, _ := f.o.GitHub.User(ctx, token)
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.mu.Lock()
	if fl.wire.State.Ended() {
		f.mu.Unlock()
		return
	}
	fl.keeping = true
	f.mu.Unlock()
	if err := f.o.Keep(ctx, fl.wire.Provider, token, account); err != nil {
		f.end(fl, proto.FlowFailed, "pocketd could not keep the sign-in: "+strings.ReplaceAll(err.Error(), token, "[token]"), "")
		return
	}
	f.end(fl, proto.FlowApproved, "", account)
}

// end ends fl in state, with why or the account approved, unless it ended
// already; the provider's sign-in is published, but after Stop.
func (f *Flows) end(fl *flow, state proto.SignInFlowState, why, account string) {
	f.mu.Lock()
	if fl.wire.State.Ended() {
		f.mu.Unlock()
		return
	}
	fl.wire.State, fl.wire.Error, fl.wire.Account, fl.wire.EndedAt, fl.keeping = state, why, account, f.o.Now().UTC(), false
	if f.stopped && state == proto.FlowCanceled {
		fl.wire.Error = "pocketd stopped."
	}
	if fl.cancel != nil {
		fl.cancel()
	}
	stopped := f.stopped
	f.mu.Unlock()
	if !stopped {
		f.changed(fl.wire.Provider)
	}
}

func (f *Flows) changed(provider string) {
	if f.o.Changed != nil {
		f.o.Changed(provider)
	}
}

// Flow is flow id as it is now: a provider's newest flow, running or
// ended; false for one pocketd does not keep.
func (f *Flows) Flow(id string) (proto.SignInFlow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.byID[id]
	if !ok {
		return proto.SignInFlow{}, false
	}
	return fl.wire, true
}

// Latest is provider's newest flow, running or ended; false for none.
func (f *Flows) Latest(provider string) (proto.SignInFlow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.latest[provider]
	if !ok {
		return proto.SignInFlow{}, false
	}
	return fl.wire, true
}

// Cancel cancels flow id while it waits for the owner's approval, and says
// how it is then: an ended flow, or one whose sign-in is being kept, stays
// as it is. False for a flow pocketd does not keep.
func (f *Flows) Cancel(id string) (proto.SignInFlow, bool) {
	f.mu.Lock()
	fl, ok := f.byID[id]
	if !ok {
		f.mu.Unlock()
		return proto.SignInFlow{}, false
	}
	canceled := f.cancelLocked(fl)
	out := fl.wire
	f.mu.Unlock()
	if canceled {
		f.changed(out.Provider)
	}
	return out, true
}

// cancelLocked cancels fl if it waits and is not being kept; f.mu is held.
func (f *Flows) cancelLocked(fl *flow) bool {
	if fl.wire.State.Ended() || fl.keeping {
		return false
	}
	fl.wire.State, fl.wire.EndedAt = proto.FlowCanceled, f.o.Now().UTC()
	fl.cancel()
	return true
}

// SignOut signs pocketd out of provider: under writeMu, so no approved
// flow's keep comes between, it cancels the provider's waiting flow and
// runs remove, which takes the sign-in out of the credential store. A keep
// under way ends first, and remove then takes what it kept.
func (f *Flows) SignOut(ctx context.Context, provider string, remove func(context.Context) error) error {
	if p, ok := Find(provider); !ok || !p.Device {
		return ErrNoDeviceFlow
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.mu.Lock()
	canceled := false
	if fl := f.latest[provider]; fl != nil {
		canceled = f.cancelLocked(fl)
	}
	f.mu.Unlock()
	if canceled {
		f.changed(provider)
	}
	return remove(ctx)
}

// Stop cancels every flow and waits for their goroutines: no start, poll
// or keep runs after it.
func (f *Flows) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.stop()
	f.wg.Wait()
}
