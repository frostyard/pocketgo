// Package state is the desktop client's data layer: one sign-in to a
// pocketd, the event stream that keeps it current, and what the client
// knows (Data), merged from fetches and events. It talks to pocketd only
// through proto (ADR-0006) and imports no UI, so a phone client can reuse
// it (ADR-0010); every rule it follows is code with a test (ADR-0011).
//
// A Store belongs to the UI thread. Each request runs on a goroutine of its
// own and hands its result back through Options.Post, so the Store and its
// Data are only ever touched on that thread.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Level is how much a line of the store's log matters.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
	// LevelCommand is a command the owner sent.
	LevelCommand
)

// ConnState is how the store stands with its pocketd.
type ConnState int

const (
	// Offline: no sign-in.
	Offline ConnState = iota
	// Connecting: the first try of a sign-in, before the stream opened.
	Connecting
	// Connected: the stream is open.
	Connected
	// Reconnecting: the stream dropped and the store tries again, while
	// it keeps what it knows.
	Reconnecting
)

// Conn is the store's connection, as the window's status shows it.
type Conn struct {
	State ConnState
	// Server is the service's URL ("https://host:port").
	Server string
	// Reason says why the stream is not open, "" when it is.
	Reason string
	// Stopped says the stream gave up: only RetryNow starts it again.
	Stopped bool
}

// Options configure a Store.
type Options struct {
	// Post runs fn on the UI thread; it is safe from any goroutine.
	// Required.
	Post func(fn func())
	// After returns a channel that fires after d, for the stream's
	// backoff; nil is time.After. Tests give a clock of their own.
	After func(d time.Duration) <-chan time.Time
	// Transport carries the requests; nil is http.DefaultTransport.
	Transport http.RoundTripper
	// Backoff is the stream's wait between tries; zero is DefaultBackoff.
	Backoff Backoff
	// Timeout bounds each request but the stream; 0 is 30 s.
	Timeout time.Duration
	// Log receives what the store does, for the window's activity log;
	// nil drops it.
	Log func(level Level, text string)
	// SignedOut runs on the UI thread when the service refuses the device
	// token: the store has signed out, and the app forgets the token of
	// server and shows the sign-in with reason.
	SignedOut func(server, reason string)
}

// ErrSignedOut is what an action fails with while no pocketd is signed in.
var ErrSignedOut = errors.New("not signed in to a pocketd")

// Pending is where an action of the owner stands: sent and not answered
// yet, or failed with Err.
type Pending struct {
	Busy bool
	Err  string
}

// Store is one sign-in to a pocketd and what the client knows of it.
type Store struct {
	// Data is what the client knows; the store keeps it current.
	Data Data
	// Conn is the connection.
	Conn Conn

	opts    Options
	client  *proto.Client
	gen     int // the sign-in: answers of an earlier one are dropped
	linkGen int // the stream: events of an earlier one are dropped
	stop    context.CancelFunc
	lastSeq int64

	inflight int // requests sent and not answered

	hello     *proto.Hello
	models    []proto.Model
	modelsErr string
	loading   map[string]bool
	// fetching counts the fetches in flight of what the client shows, by
	// list ("sessions", "approvals") or open session (detailKey).
	fetching map[string]int

	ids requestIDs
	// abortRuns holds the sessions whose abort keeps its request id (sent
	// and not answered, or failed), and whether their targeted run was
	// seen ended (the session idle) since the abort was sent.
	abortRuns map[int64]bool
	sends     map[int64]*Pending
	aborts    map[int64]*Pending
	decisions map[string]*Pending
	creating  Pending
}

// New returns a store with no sign-in.
func New(opts Options) *Store {
	if opts.Post == nil {
		panic("state.New: Options.Post is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Backoff == (Backoff{}) {
		opts.Backoff = DefaultBackoff
	}
	s := &Store{opts: opts}
	s.resetActions()
	return s
}

func (s *Store) resetActions() {
	s.ids.reset()
	s.abortRuns = map[int64]bool{}
	s.sends, s.aborts, s.decisions = map[int64]*Pending{}, map[int64]*Pending{}, map[string]*Pending{}
	s.creating = Pending{}
	s.loading = map[string]bool{}
	s.fetching = map[string]int{}
}

func (s *Store) log(level Level, format string, args ...any) {
	if s.opts.Log != nil {
		s.opts.Log(level, fmt.Sprintf(format, args...))
	}
}

// InFlight is how many requests the store sent that are not answered yet.
func (s *Store) InFlight() int { return s.inflight }

// SignedIn reports whether the store has a sign-in.
func (s *Store) SignedIn() bool { return s.client != nil }

// Hello is the service's answer to GET /v1/hello, nil until it came.
func (s *Store) Hello() *proto.Hello { return s.hello }

// Models are the models the service runs now (GET /v1/models), and why
// loading them failed.
func (s *Store) Models() ([]proto.Model, string) { return s.models, s.modelsErr }

func (s *Store) newClient(server, token string) (*proto.Client, error) {
	return proto.NewClient(server, token, HTTPClient(s.opts.Transport))
}

// SignIn checks server and token with GET /v1/hello and, when the service
// takes them, makes them the store's sign-in and opens the stream. done
// runs on the UI thread, with an error that says what to fix.
func (s *Store) SignIn(server, token string, done func(error)) {
	client, err := s.newClient(server, token)
	if err != nil {
		done(err)
		return
	}
	gen := s.gen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
		defer cancel()
		hello, err := client.Hello(ctx)
		s.opts.Post(func() {
			if gen != s.gen {
				done(errors.New("a newer sign-in replaced this one"))
				return
			}
			if err != nil {
				done(SignInError(server, err))
				return
			}
			s.start(server, client)
			s.hello = &hello
			s.log(LevelInfo, "Signed in to pocketd %s at %s as device %q", hello.Server, server, hello.Device)
			done(nil)
		})
	}()
}

// SignInError says what a failed sign-in means for the person signing in.
func SignInError(server string, err error) error {
	if errors.Is(err, ErrRedirect) {
		return fmt.Errorf("%w; PocketGo sends the token only to the service URL you entered (%s), and follows a redirect only there and with the same method. If pocketd moved and you trust its new address, sign in to it", err, server)
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case proto.ErrorCodeUnauthorized:
			return errors.New("pocketd refused the device token: paste only the pgd_… value that `pocketd device add NAME` printed (its token line, without the word token), and check that the device is not revoked")
		case proto.ErrorCodeProtocol:
			return fmt.Errorf("this pocketd does not speak protocol version %d: update the client or the service (%s)", proto.Version, pe.Message)
		}
		return fmt.Errorf("pocketd answered %d: %s", pe.Status, pe.Message)
	}
	return fmt.Errorf("could not reach pocketd at %s: %w", server, err)
}

// Connect makes a kept sign-in the store's and opens the stream, without
// checking it first: the stream says whether the service still takes it.
func (s *Store) Connect(server, token string) error {
	client, err := s.newClient(server, token)
	if err != nil {
		return err
	}
	s.start(server, client)
	return nil
}

// start replaces the sign-in with client and opens its stream.
func (s *Store) start(server string, client *proto.Client) {
	s.stopLink()
	s.gen++
	s.client = client
	s.hello, s.models, s.modelsErr = nil, nil, ""
	s.lastSeq = 0
	s.Data.Reset()
	s.resetActions()
	s.Conn = Conn{State: Connecting, Server: server}
	s.log(LevelInfo, "Connecting to %s", server)
	s.startLink()
}

// SignOut ends the sign-in and forgets what the store knew.
func (s *Store) SignOut() {
	s.stopLink()
	s.gen++
	s.client = nil
	s.hello, s.models, s.modelsErr = nil, nil, ""
	s.lastSeq = 0
	s.Data.Reset()
	s.resetActions()
	s.Conn = Conn{}
}

// Close stops the stream, as the window closes.
func (s *Store) Close() { s.stopLink() }

func (s *Store) signedOut(why string) {
	server := s.Conn.Server
	s.log(LevelError, "pocketd at %s refused the device token", server)
	s.SignOut()
	if s.opts.SignedOut != nil {
		s.opts.SignedOut(server, why)
	}
}

func (s *Store) stopLink() {
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	s.linkGen++
}

// RetryNow drops the stream's wait and opens it again at once, from the
// last event the store applied.
func (s *Store) RetryNow() {
	if s.client == nil {
		return
	}
	s.stopLink()
	s.Conn.Stopped = false
	s.startLink()
}

// startLink runs the stream on a goroutine. Its callbacks post to the UI
// thread, where those of a stream stopped since are dropped: events posted
// but not applied are sent again, from the last one applied (lastSeq).
func (s *Store) startLink() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	gen := s.linkGen
	post := func(fn func()) {
		s.opts.Post(func() {
			if gen == s.linkGen {
				fn()
			}
		})
	}
	link := &Link{
		Client:  s.client,
		Backoff: s.opts.Backoff,
		After:   s.opts.After,
		Opened:  func() { post(s.streamOpened) },
		Event:   func(ev proto.Event) { post(func() { s.apply(ev) }) },
		Dropped: func(err error, failures int, wait time.Duration) {
			post(func() { s.streamDropped(err, failures, wait) })
		},
	}
	from := s.lastSeq
	go func() {
		err := link.Run(ctx, from)
		if ctx.Err() != nil {
			return
		}
		post(func() { s.streamStopped(err) })
	}()
}

// streamOpened marks the connection open. A stream without an event to
// come back from starts from now, so the store fetches what it shows; its
// hello gives the next stream one (protocol rule 18). A stream that comes
// back from an event replays what came after it, or sends resync, so the
// store fetches again only what failed to load. Neither waits for the
// hello, which a service from before it never sends.
func (s *Store) streamOpened() {
	if s.Conn.State != Connected {
		s.log(LevelInfo, "Connected to %s", s.Conn.Server)
	}
	s.Conn.State, s.Conn.Reason, s.Conn.Stopped = Connected, "", false
	if s.lastSeq == 0 {
		s.Refresh()
	} else {
		s.retryFailed()
	}
}

func (s *Store) streamDropped(err error, failures int, wait time.Duration) {
	reason := "the stream ended"
	if err != nil && !errors.Is(err, proto.ErrStreamEnded) {
		reason = err.Error()
	}
	if s.Conn.State == Connected {
		s.Conn.State = Reconnecting
	}
	s.Conn.Reason = fmt.Sprintf("%s; trying again in %s", reason, wait.Round(100*time.Millisecond))
	level := LevelWarn
	if failures > 1 {
		level = LevelInfo
	}
	s.log(level, "Lost pocketd at %s: %s", s.Conn.Server, s.Conn.Reason)
}

func (s *Store) streamStopped(err error) {
	if Unauthorized(err) {
		s.signedOut("pocketd no longer takes this device's token. Run `pocketd device add NAME` on the service host and sign in with the new token.")
		return
	}
	if s.Conn.State == Connected {
		s.Conn.State = Reconnecting
	}
	s.Conn.Stopped = true
	s.Conn.Reason = SignInError(s.Conn.Server, err).Error()
	s.log(LevelError, "Stopped the stream: %s", s.Conn.Reason)
}

// apply applies one event of the stream, on the UI thread. Each event's
// seq, the hello's included, is where the next stream comes back from.
func (s *Store) apply(ev proto.Event) {
	if ev.Seq != 0 {
		s.lastSeq = ev.Seq
	}
	if s.Data.Apply(ev) {
		s.log(LevelWarn, "pocketd no longer has the events this client missed; loading everything again")
		s.Refresh()
	}
	s.settleAborts()
}

// retryFailed fetches again what failed to load or has not loaded yet: a
// list or the open session's detail whose fetch failed or is still in
// flight, also over what loaded before, as the refetch a resync started.
// A fetch in flight may fail after the stream came back, and the client is
// past the events that would bring what it missed; the answer of the fetch
// begun later wins (Data). Then the service's hello and its models, also
// when a reload of the models failed over an older list.
func (s *Store) retryFailed() {
	if status, _ := s.Data.SessionsStatus(); status != ListLoaded || s.fetching["sessions"] > 0 {
		s.loadSessions()
	}
	if status, _ := s.Data.ApprovalsStatus(); status != ListLoaded || s.fetching["approvals"] > 0 {
		s.loadApprovals()
	}
	if o := s.Data.Open(); o != nil && (o.Err != "" || !o.Loaded || s.fetching[detailKey(o.ID)] > 0) {
		s.loadDetail(o.ID)
	}
	if s.hello == nil {
		s.loadHello()
	}
	if s.models == nil || s.modelsErr != "" {
		s.LoadModels()
	}
}

// Refresh fetches again what the client shows: the sessions, the pending
// approvals and the open session, and the service's hello and models when
// it has none yet.
func (s *Store) Refresh() {
	s.loadSessions()
	s.loadApprovals()
	if id := s.Data.OpenID(); id != 0 {
		s.loadDetail(id)
	}
	if s.hello == nil {
		s.loadHello()
	}
	if s.models == nil {
		s.LoadModels()
	}
}

// do runs fn against the service on a goroutine and hands its error to
// done on the UI thread. An answer of an earlier sign-in is dropped; a 401
// signs out; a failure is logged under what.
func (s *Store) do(what string, fn func(ctx context.Context, c *proto.Client) error, done func(error)) {
	client, gen := s.client, s.gen
	if client == nil {
		if done != nil {
			done(ErrSignedOut)
		}
		return
	}
	s.inflight++
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
		defer cancel()
		err := fn(ctx, client)
		s.opts.Post(func() {
			s.inflight--
			if gen != s.gen {
				return
			}
			if Unauthorized(err) {
				s.signedOut("pocketd no longer takes this device's token. Run `pocketd device add NAME` on the service host and sign in with the new token.")
				return
			}
			if err != nil {
				s.log(LevelError, "%s: %v", what, err)
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// load runs a fetch under key, one at a time: asking for it again while it
// is in flight does nothing. It serves what no event changes (the hello,
// the models), so the answer in flight is as good as a new one.
func (s *Store) load(key, what string, fn func(ctx context.Context, c *proto.Client) error, done func(error)) {
	if s.loading[key] {
		return
	}
	s.loading[key] = true
	gen := s.gen
	s.do(what, fn, func(err error) {
		if gen == s.gen {
			delete(s.loading, key)
		}
		done(err)
	})
}

// detailKey names an open session's detail in fetching.
func detailKey(id int64) string { return fmt.Sprintf("detail:%d", id) }

// track counts a fetch of key in flight until done runs with its answer.
func (s *Store) track(key string, done func(error)) func(error) {
	s.fetching[key]++
	gen := s.gen
	return func(err error) {
		if gen == s.gen {
			s.fetching[key]--
		}
		done(err)
	}
}

func (s *Store) loadSessions() {
	f := s.Data.Begin()
	var list []proto.Session
	s.do("Loading the sessions", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Sessions(ctx)
		return err
	}, s.track("sessions", func(err error) {
		if err != nil {
			s.Data.SetSessionsError(f, err.Error())
			return
		}
		s.Data.SetSessions(f, list)
		s.settleAborts()
	}))
}

func (s *Store) loadApprovals() {
	f := s.Data.Begin()
	var list []proto.Approval
	s.do("Loading the approvals", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Approvals(ctx)
		return err
	}, s.track("approvals", func(err error) {
		if err != nil {
			s.Data.SetApprovalsError(f, err.Error())
			return
		}
		s.Data.SetApprovals(f, list)
	}))
}

func (s *Store) loadDetail(id int64) {
	f := s.Data.Begin()
	var detail proto.SessionDetail
	s.do(fmt.Sprintf("Loading session %d", id), func(ctx context.Context, c *proto.Client) (err error) {
		detail, err = c.Session(ctx, id)
		return err
	}, s.track(detailKey(id), func(err error) {
		if err != nil {
			s.Data.SetDetailError(f, id, err.Error())
			return
		}
		s.Data.SetDetail(f, detail)
		s.settleAborts()
	}))
}

func (s *Store) loadHello() {
	var hello proto.Hello
	s.load("hello", "Reading the service's hello", func(ctx context.Context, c *proto.Client) (err error) {
		hello, err = c.Hello(ctx)
		return err
	}, func(err error) {
		if err == nil {
			s.hello = &hello
		}
	})
}

// LoadModels fetches the models the service runs now.
func (s *Store) LoadModels() {
	var models []proto.Model
	s.load("models", "Loading the models", func(ctx context.Context, c *proto.Client) (err error) {
		models, err = c.Models(ctx)
		return err
	}, func(err error) {
		if err != nil {
			s.modelsErr = err.Error()
			return
		}
		if models == nil {
			models = []proto.Model{}
		}
		s.models, s.modelsErr = models, ""
	})
}

// LoadProfiles asks pocketd for the agents' profiles. Every call fetches,
// even with an earlier fetch in flight: the owner may have edited a profile
// since. The newest answer wins (Data.SetProfiles), and a failure keeps the
// profiles an earlier answer brought.
func (s *Store) LoadProfiles() {
	f := s.Data.Begin()
	var profiles []proto.Profile
	s.do("Loading the profiles", func(ctx context.Context, c *proto.Client) (err error) {
		profiles, err = c.Profiles(ctx)
		return err
	}, func(err error) {
		if err != nil {
			s.Data.SetProfilesError(f, err.Error())
			return
		}
		s.Data.SetProfiles(f, profiles)
	})
}

// Profiles are the profiles of pocketd's newest answer (nil before one
// came) and, when the newest fetch failed, why.
func (s *Store) Profiles() ([]proto.Profile, string) {
	status, err := s.Data.ProfilesStatus()
	if status != ListFailed {
		err = ""
	}
	return s.Data.Profiles(), err
}

// Open shows session id (0 for none): its transcript and live state load
// from the service, and the stream keeps them current.
func (s *Store) Open(id int64) {
	s.Data.SetOpen(id)
	if id != 0 && s.client != nil {
		s.loadDetail(id)
	}
}

// pending returns the entry of m under key, made when missing.
func pending[K comparable](m map[K]*Pending, key K) *Pending {
	p := m[key]
	if p == nil {
		p = &Pending{}
		m[key] = p
	}
	return p
}

// Sending is where the last message sent to session id stands.
func (s *Store) Sending(id int64) Pending { return valueOf(s.sends[id]) }

// Aborting is where the last abort of session id stands.
func (s *Store) Aborting(id int64) Pending { return valueOf(s.aborts[id]) }

// Deciding is where the owner's decision on approval id stands.
func (s *Store) Deciding(id string) Pending { return valueOf(s.decisions[id]) }

// Creating is where the last new session stands.
func (s *Store) Creating() Pending { return s.creating }

func valueOf(p *Pending) Pending {
	if p == nil {
		return Pending{}
	}
	return *p
}

// Send sends text to session id. done runs on the UI thread with the
// outcome; a message tried again unchanged after a failure keeps its
// request id. A message the session queues shows in its live state
// (Live.Queued) until its entry comes.
func (s *Store) Send(id int64, text string, done func(error)) {
	p := pending(s.sends, id)
	if p.Busy {
		done(errors.New("the last message is still on its way"))
		return
	}
	if err := ValidateMessage(text); err != nil {
		p.Err = err.Error()
		done(err)
		return
	}
	key := fmt.Sprintf("send:%d", id)
	request := proto.SendMessage{RequestID: s.ids.For(key, text), Text: text}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Message to session %d (%d bytes)", id, len(text))
	s.do(fmt.Sprintf("Sending to session %d", id), func(ctx context.Context, c *proto.Client) error {
		_, err := c.Send(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		if err != nil {
			p.Err = "sending failed: " + err.Error() + "; send it again to retry"
		} else {
			s.ids.Done(key)
		}
		done(err)
	})
}

// Abort stops the run of session id. An abort that failed keeps its
// request id for a retry of the same press, which finishes the abort of the
// run it first targeted (protocol rule 5), unless the session was seen
// idle at any moment since the abort was sent: then the run it targeted has
// ended, and the next press is a new abort of the next run, even if that
// run started before the failure came back (settleAborts).
func (s *Store) Abort(id int64) {
	p := pending(s.aborts, id)
	if p.Busy {
		return
	}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Abort session %d", id)
	key := fmt.Sprintf("abort:%d", id)
	request := proto.Abort{RequestID: s.ids.For(key, "abort")}
	if _, kept := s.abortRuns[id]; !kept {
		s.abortRuns[id] = false
	}
	s.settleAborts()
	s.do(fmt.Sprintf("Aborting session %d", id), func(ctx context.Context, c *proto.Client) error {
		return c.Abort(ctx, id, request)
	}, func(err error) {
		p.Busy = false
		if err != nil {
			p.Err = err.Error()
		} else {
			s.ids.Done(key)
			delete(s.abortRuns, id)
		}
		s.settleAborts()
	})
}

// settleAborts notes, for each abort that keeps its request id, whether
// its session is idle now, which ends the run it targeted; and drops the id
// of each failed abort whose run was seen ended: a retry would finish
// nothing, and the service would leave a later run running. It runs after
// every change to the data and to the aborts.
func (s *Store) settleAborts() {
	for id, ended := range s.abortRuns {
		if !ended && s.Data.StateOf(id) == proto.SessionIdle {
			ended = true
			s.abortRuns[id] = true
		}
		if ended && !s.Aborting(id).Busy {
			s.ids.Done(fmt.Sprintf("abort:%d", id))
			delete(s.abortRuns, id)
		}
	}
}

// Decide allows or denies approval id, with reason for a denial. The
// decision's answer moves the approval out of the pending list. An
// approval decided elsewhere (409) or gone (404) is fetched again.
func (s *Store) Decide(id string, allow bool, reason string) {
	p := pending(s.decisions, id)
	if p.Busy {
		return
	}
	if allow {
		reason = ""
	}
	reason = strings.TrimSpace(reason)
	key := "decide:" + id
	request := proto.Decide{RequestID: s.ids.For(key, fmt.Sprintf("%t\x00%s", allow, reason)), Allow: allow, Reason: reason}
	p.Busy, p.Err = true, ""
	verb := map[bool]string{true: "Allow", false: "Deny"}[allow]
	s.log(LevelCommand, "%s approval %s", verb, id)
	var decided proto.Approval
	s.do(verb+" a tool call", func(ctx context.Context, c *proto.Client) (err error) {
		decided, err = c.Decide(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(key)
			delete(s.decisions, id)
			s.Data.Decided(decided)
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound):
			s.ids.Done(key)
			p.Err = "Someone decided this call already."
			if pe.Code == proto.ErrorCodeNotFound {
				p.Err = "pocketd no longer has this approval."
			}
			s.loadApprovals()
		default:
			p.Err = err.Error()
		}
	})
}

// CreateSession starts a session (its RequestID is the store's to set).
// done runs on the UI thread with the new session. A request tried again
// unchanged after a failure keeps its request id, so a lost answer does not
// start a second session.
func (s *Store) CreateSession(request proto.CreateSession, done func(proto.Session, error)) {
	request.Cwd = strings.TrimSpace(request.Cwd)
	request.Title = strings.TrimSpace(request.Title)
	if err := ValidateNewSession(request, s.models); err != nil {
		done(proto.Session{}, err)
		return
	}
	if s.creating.Busy {
		done(proto.Session{}, errors.New("a new session is still on its way"))
		return
	}
	request.RequestID = ""
	action, _ := json.Marshal(request)
	request.RequestID = s.ids.For("create", string(action))
	s.creating = Pending{Busy: true}
	s.log(LevelCommand, "New session in %s on %s", request.Cwd, request.Model)
	var session proto.Session
	s.do("Starting a session", func(ctx context.Context, c *proto.Client) (err error) {
		session, err = c.CreateSession(ctx, request)
		return err
	}, func(err error) {
		s.creating.Busy = false
		if err != nil {
			s.creating.Err = err.Error()
			done(proto.Session{}, err)
			return
		}
		s.ids.Done("create")
		s.Data.Created(session)
		done(session, nil)
	})
}
