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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
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
	// TempDir is where attached files are copied before they upload; ""
	// is the system's.
	TempDir string
	// SignInChanged runs on the UI thread each time the store's sign-in
	// changes, to a new one or to none: the store drops every answer of
	// the one before, so what waits on one ends here (Merge All Ready,
	// ADR-0031; Codex review of #53, round 1).
	SignInChanged func()
	// Now is the desktop's clock, in its zone, which a launch's end is
	// computed from (LaunchEnd); nil is time.Now. Tests give a clock of
	// their own.
	Now func() time.Time
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

	opts Options
	// withTimeout bounds a request (context.WithTimeout); tests see the
	// bound each asks for.
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	client      *proto.Client
	gen         int // the sign-in: answers of an earlier one are dropped
	linkGen     int // the stream: events of an earlier one are dropped
	stop        context.CancelFunc
	lastSeq     int64

	inflight int // requests sent and not answered

	hello   *proto.Hello
	loading map[string]bool
	// fetching counts the fetches in flight of what the client shows, by
	// list ("sessions", "approvals", "asks", "lessons", "threads", "pulls",
	// "plans", "proposals", "schedules", "profiles", "terminals") or open
	// session (detailKey).
	fetching map[string]int
	// schedulesDirty says a schedules event came while their fetch ran: one
	// more fetch follows it (refetchSchedules).
	schedulesDirty bool
	// schedulesEvents counts the schedules events: a fetch begun before
	// the last one answers with a list older than it.
	schedulesEvents int

	ids requestIDs
	// abortRuns holds the sessions whose abort keeps its request id (sent
	// and not answered, or failed), and whether their targeted run was
	// seen ended (the session idle) since the abort was sent.
	abortRuns map[int64]bool
	sends     map[int64]*Pending
	aborts    map[int64]*Pending
	decisions map[string]*Pending
	answers   map[int64]*Pending
	// answerRefusals are the owner's answers pocketd refused because their
	// ask no longer waits, by ask (AnswerRefusals).
	answerRefusals map[int64]AnswerRefusal
	// answerTexts are the answers sent, as the owner wrote them, by ask,
	// until their ask settles (settleAnswers); answerOutcomes, for those
	// alone, how an event said their ask left the open list: answered or
	// withdrawn (ADR-0037).
	answerTexts    map[int64]string
	answerOutcomes map[int64]proto.AskState
	// answerDrafts are the owner's answers being written, by ask
	// (AnswerDraft), and withdrawnDrafts the drafts whose ask a
	// supervisor's message withdrew, kept for their card (askLeft).
	answerDrafts    map[int64]*string
	withdrawnDrafts map[int64]WithdrawnDraft
	// lessonDecisions are the owner's decisions on lessons, by lesson;
	// lessonSeen what each acts on (LessonTarget), and lessonStopped
	// whether that went while the decision was on its way.
	lessonDecisions map[int64]*Pending
	lessonSeen      map[int64]string
	lessonStopped   map[int64]bool
	// closes are the owner's closes of threads, by thread; planApprovals
	// the owner's approvals of plans, by plan.
	closes map[int64]*Pending
	// shifts are the owner's stops and resumes of threads' shifts, by
	// thread (ADR-0024); shiftAsked is the shift each was asked for on, as
	// shiftSeen has it.
	shifts        map[int64]*Pending
	shiftAsked    map[int64]string
	planApprovals map[int64]*Pending
	// startOvers are the owner's Start Overs of standing sessions, by
	// session (ADR-0026).
	startOvers map[int64]*Pending
	// modelChanges are the owner's changes of a session's model, by
	// session (ADR-0028), and modelFrom those that keep their request id
	// (settleModels).
	modelChanges map[int64]*Pending
	modelFrom    map[int64]*keptModel
	creating     Pending
	// startingTerminal is the owner's start of a shell (terminal.go);
	// startKept says a start that failed without an answer keeps its
	// request id; freshTerminals are the shells this client started and
	// has not shown yet.
	startingTerminal Pending
	startKept        bool
	freshTerminals   map[string]bool
	// startBefore are the shells listed as the start on its way left.
	startBefore map[string]bool
	// picks are the proposals the owner picked for the next launch;
	// launching is the owner's launch, launchKept the one whose request
	// id is kept (nightshift.go), and launched the last one made. drops
	// are the owner's drops of proposals, by proposal.
	picks      map[int64]bool
	launching  Pending
	launchKept *keptLaunch
	launched   *proto.Launch
	drops      map[int64]*Pending
	// attachments are the files attached to each session's next message,
	// in order (ADR-0029); nextAttach numbers the next chip, and tempDir is
	// where their snapshots go (Options.TempDir).
	attachments map[int64][]*attachment
	nextAttach  int64
	tempDir     string
	snaps       snapshots
	// attachLine are the files waiting their turn, attachHeld how many
	// hold a slot (MaxAttachWork), and attachGen ends the turns of an
	// earlier sign-in.
	attachLine []attachUnit
	attachHeld int
	attachGen  int
	// attachWaiting are the bytes pasted files waiting their turn hold
	// (PastedWaitingLimit).
	attachWaiting int64
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
	s := &Store{opts: opts, withTimeout: context.WithTimeout, tempDir: opts.TempDir}
	s.resetActions()
	return s
}

func (s *Store) resetActions() {
	s.ids.reset()
	s.abortRuns = map[int64]bool{}
	s.sends, s.aborts, s.decisions = map[int64]*Pending{}, map[int64]*Pending{}, map[string]*Pending{}
	s.answers = map[int64]*Pending{}
	s.answerRefusals = map[int64]AnswerRefusal{}
	s.answerTexts = map[int64]string{}
	s.answerOutcomes = map[int64]proto.AskState{}
	s.answerDrafts = map[int64]*string{}
	s.withdrawnDrafts = map[int64]WithdrawnDraft{}
	s.lessonDecisions = map[int64]*Pending{}
	s.lessonSeen = map[int64]string{}
	s.lessonStopped = map[int64]bool{}
	s.closes = map[int64]*Pending{}
	s.shifts = map[int64]*Pending{}
	s.shiftAsked = map[int64]string{}
	s.planApprovals = map[int64]*Pending{}
	s.startOvers = map[int64]*Pending{}
	s.modelChanges = map[int64]*Pending{}
	s.modelFrom = map[int64]*keptModel{}
	s.creating = Pending{}
	s.startingTerminal = Pending{}
	s.startKept = false
	s.freshTerminals = nil
	s.startBefore = nil
	s.picks = map[int64]bool{}
	s.launching = Pending{}
	s.launchKept = nil
	s.launched = nil
	s.drops = map[int64]*Pending{}
	for _, files := range s.attachments {
		for _, a := range files {
			if a.cancel != nil {
				a.cancel()
			}
		}
	}
	s.snaps.removeAll(false)
	// A turn under way keeps its slot until it ends, as its files are
	// open until then, its upload's answer dropped; a copy kept for Try
	// Again is let go now (Codex review of #49, rounds 11 and 12).
	s.attachLine, s.attachWaiting = nil, 0
	s.attachGen++
	for _, files := range s.attachments {
		for _, a := range files {
			switch {
			case a.active && a.dropAnswer != nil:
				a.dropAnswer()
			case a.held && !a.active:
				s.releaseAttach(a)
			}
		}
	}
	s.attachments = map[int64][]*attachment{}
	s.loading = map[string]bool{}
	s.fetching = map[string]int{}
	s.schedulesDirty = false
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
func (s *Store) Models() ([]proto.Model, string) { return s.Data.Models() }

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
	s.hello = nil
	s.lastSeq = 0
	s.Data.Reset()
	s.resetActions()
	s.Conn = Conn{State: Connecting, Server: server}
	s.log(LevelInfo, "Connecting to %s", server)
	s.startLink()
	s.signInChanged()
}

// SignOut ends the sign-in and forgets what the store knew.
func (s *Store) SignOut() {
	s.stopLink()
	s.gen++
	s.client = nil
	s.hello = nil
	s.lastSeq = 0
	s.Data.Reset()
	s.resetActions()
	s.Conn = Conn{}
	s.signInChanged()
}

// signInChanged tells Options.SignInChanged.
func (s *Store) signInChanged() {
	if s.opts.SignInChanged != nil {
		s.opts.SignInChanged()
	}
}

// Close stops the stream, as the window closes.
// Close ends the event stream, and removes the attached files' copies,
// those still being made included (Codex review of #49, round 5).
func (s *Store) Close() {
	s.stopLink()
	for _, files := range s.attachments {
		for _, a := range files {
			if a.cancel != nil {
				a.cancel()
			}
		}
	}
	s.snaps.removeAll(true)
	s.attachLine = nil
}

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
	if ev.Type == proto.EventAsk && ev.Ask != nil && ev.Ask.State != "" && ev.Ask.State != proto.AskOpen {
		s.askLeft(*ev.Ask)
	}
	switch s.Data.Apply(ev) {
	case RefetchAll:
		s.log(LevelWarn, "pocketd no longer has the events this client missed; loading everything again")
		s.Refresh()
	case RefetchSchedules:
		s.refetchSchedules()
	}
	s.settleAborts()
	s.settleModels()
	s.settleAnswers()
	s.settleLessons()
	s.settleCloses()
	s.settleShifts()
	s.settlePlans()
	s.settleProposals()
}

// settleCloses drops the retry state of each close whose thread is closed
// now (closed here, elsewhere, or by a request whose reply was lost): no
// card offers its retry. A close on its way keeps its state, and one whose
// thread is still open keeps its request id for the retry.
func (s *Store) settleCloses() {
	for id, p := range s.closes {
		if p.Busy {
			continue
		}
		if t, ok := s.Data.Thread(id); ok && t.State == proto.ThreadClosed {
			delete(s.closes, id)
			s.ids.Done(closeKey(id))
		}
	}
}

func closeKey(id int64) string { return fmt.Sprintf("close:%d", id) }

// CloseThread closes thread id for the owner (POST /v1/threads/{id}/close):
// its specialist gets no more of its work, and it leaves the desk. A close
// tried again after a failure keeps its request id, so a lost reply is not
// applied twice. A thread pocketd no longer has (404) is fetched again.
func (s *Store) CloseThread(id int64) {
	p := pending(s.closes, id)
	if p.Busy {
		return
	}
	key := closeKey(id)
	request := proto.CloseThread{RequestID: s.ids.For(key, "close")}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Close T%d", id)
	var closed proto.Thread
	s.do(fmt.Sprintf("Closing T%d", id), func(ctx context.Context, c *proto.Client) (err error) {
		closed, err = c.CloseThread(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(key)
			delete(s.closes, id)
			s.Data.ThreadClosed(closed)
		case errors.As(err, &pe) && pe.Code == proto.ErrorCodeNotFound:
			s.ids.Done(key)
			p.Err = "pocketd no longer has this thread."
			s.loadThreads()
		default:
			p.Err = err.Error()
		}
		// The thread's event may have come before this reply: a closed
		// thread keeps no retry state.
		s.settleCloses()
	})
}

// Closing is where the owner's close of thread id stands.
func (s *Store) Closing(id int64) Pending { return valueOf(s.closes[id]) }

// settleAnswers drops the retry state of each answer whose ask is no
// longer open (answered here, elsewhere, or by a request whose reply was
// lost): no card offers its retry. An answer on its way keeps its state,
// and one to an ask that still waits keeps its request id for the retry.
// An answer that failed, whose ask left the open list, waits for the
// event that says how: a withdrawal by a supervisor's message means it was
// not taken, whatever its reply was, and it is kept as a refusal with the
// text the owner wrote; an answer means it settled. A list that dropped the
// ask before its event came says neither, so the answer waits (ADR-0037;
// Codex review of #62, rounds 1 and 2).
func (s *Store) settleAnswers() {
	for id, p := range s.answers {
		if p.Busy {
			continue
		}
		if _, open := s.Data.Ask(id); open {
			continue
		}
		typed, sent := s.answerTexts[id]
		outcome, known := s.answerOutcomes[id]
		if sent && p.Err != "" && !known {
			continue
		}
		if sent && p.Err != "" && outcome == proto.AskWithdrawn {
			if _, kept := s.answerRefusals[id]; !kept {
				s.answerRefusals[id] = AnswerRefusal{Ask: id, Answer: typed,
					Why: fmt.Sprintf("A%d was withdrawn: the supervisor's message reached its thread's specialist before your answer was taken, so pocketd did not send it", id)}
			}
		}
		s.forgetAnswer(id)
	}
}

// forgetAnswer drops what the store keeps of the owner's answer to ask id.
func (s *Store) forgetAnswer(id int64) {
	delete(s.answers, id)
	delete(s.answerTexts, id)
	delete(s.answerOutcomes, id)
	s.ids.Done(fmt.Sprintf("answer:%d", id))
}

// retryFailed fetches again what failed to load or has not loaded yet: a
// list or the open session's detail whose fetch failed or is still in
// flight, also over what loaded before, as the refetch a resync started.
// A fetch in flight may fail after the stream came back, and the client is
// past the events that would bring what it missed; the answer of the fetch
// begun later wins (Data). Then the service's hello, and its models in
// any case: a pocketd that came back may run other models, which the
// composer's chip offers (Codex review of #48, round 2).
func (s *Store) retryFailed() {
	if status, _ := s.Data.SessionsStatus(); status != ListLoaded || s.fetching["sessions"] > 0 {
		s.loadSessions()
	}
	if status, _ := s.Data.ApprovalsStatus(); status != ListLoaded || s.fetching["approvals"] > 0 {
		s.loadApprovals()
	}
	if status, _ := s.Data.AsksStatus(); status != ListLoaded || s.fetching["asks"] > 0 {
		s.loadAsks()
	}
	if status, _ := s.Data.LessonsStatus(); status != ListLoaded || s.fetching["lessons"] > 0 {
		s.loadLessons()
	}
	if status, _ := s.Data.ThreadsStatus(); status != ListLoaded || s.fetching["threads"] > 0 {
		s.loadThreads()
	}
	if status, _ := s.Data.PullsStatus(); status != ListLoaded || s.fetching["pulls"] > 0 {
		s.loadPulls()
	}
	if status, _ := s.Data.PlansStatus(); status != ListLoaded || s.fetching["plans"] > 0 {
		s.loadPlans()
	}
	if status, _ := s.Data.ProposalsStatus(); status != ListLoaded || s.fetching["proposals"] > 0 {
		s.loadProposals()
	}
	if status, _ := s.Data.SchedulesStatus(); status != ListLoaded || s.fetching["schedules"] > 0 {
		s.loadSchedules()
	}
	if status, _ := s.Data.ProfilesStatus(); status != ListLoaded || s.fetching["profiles"] > 0 {
		s.LoadProfiles()
	}
	if status, _ := s.Data.TerminalsStatus(); status != ListLoaded || s.fetching["terminals"] > 0 {
		s.loadTerminals()
	}
	if o := s.Data.Open(); o != nil && (o.Err != "" || !o.Loaded || s.fetching[detailKey(o.ID)] > 0) {
		s.loadDetail(o.ID)
	}
	if s.hello == nil {
		s.loadHello()
	}
	s.LoadModels()
}

// Refresh fetches again what the client shows: the sessions, the pending
// approvals, the open asks, the lessons, the threads, their pull requests,
// the plans, the board's proposals and the schedules, the profiles, the
// owner's terminals, the open session and the models the composer's chip
// offers, and the service's hello when it has none yet.
func (s *Store) Refresh() {
	s.loadSessions()
	s.loadApprovals()
	s.loadAsks()
	s.loadLessons()
	s.loadThreads()
	s.loadPulls()
	s.loadPlans()
	s.loadProposals()
	s.loadSchedules()
	// The board and the Inbox name agents by their profiles.
	s.LoadProfiles()
	s.loadTerminals()
	if id := s.Data.OpenID(); id != 0 {
		s.loadDetail(id)
	}
	if s.hello == nil {
		s.loadHello()
	}
	s.LoadModels()
}

// do runs fn against the service on a goroutine and hands its error to
// done on the UI thread. An answer of an earlier sign-in is dropped; a 401
// signs out; a failure is logged under what.
func (s *Store) do(what string, fn func(ctx context.Context, c *proto.Client) error, done func(error)) {
	s.doWithin(what, s.opts.Timeout, fn, done)
}

// doWithin is do with a deadline of its own, for a request pocketd may
// take longer than Options.Timeout to answer.
func (s *Store) doWithin(what string, timeout time.Duration, fn func(ctx context.Context, c *proto.Client) error, done func(error)) {
	client, gen := s.client, s.gen
	if client == nil {
		if done != nil {
			done(ErrSignedOut)
		}
		return
	}
	s.inflight++
	go func() {
		ctx, cancel := s.withTimeout(context.Background(), timeout)
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
		s.settleModels()
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

func (s *Store) loadAsks() {
	f := s.Data.BeginAsks()
	var list []proto.Ask
	s.do("Loading the questions", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Asks(ctx)
		return err
	}, s.track("asks", func(err error) {
		if err != nil {
			s.Data.SetAsksError(f, err.Error())
			return
		}
		s.Data.SetAsks(f, list)
		s.settleAnswers()
	}))
}

func (s *Store) loadLessons() {
	f := s.Data.BeginLessons()
	var list []proto.Lesson
	s.do("Loading the lessons", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Lessons(ctx)
		return err
	}, s.track("lessons", func(err error) {
		if err != nil {
			s.Data.SetLessonsError(f, err.Error())
			return
		}
		if s.Data.SetLessons(f, list) {
			s.loadLessons()
		}
		s.settleLessons()
	}))
}

func (s *Store) loadThreads() {
	f := s.Data.BeginThreads()
	var list []proto.Thread
	s.do("Loading the threads", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Threads(ctx)
		return err
	}, s.track("threads", func(err error) {
		if err != nil {
			s.Data.SetThreadsError(f, err.Error())
			return
		}
		s.Data.SetThreads(f, list)
		s.settleCloses()
		s.settleShifts()
	}))
}

func (s *Store) loadPulls() {
	f := s.Data.BeginPulls()
	var list []proto.PullRequest
	s.do("Loading the pull requests", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Pulls(ctx)
		return err
	}, s.track("pulls", func(err error) {
		if err != nil {
			s.Data.SetPullsError(f, err.Error())
			return
		}
		s.Data.SetPulls(f, list)
	}))
}

// ReadPull reads linked pull request id from pocketd now, not from what
// the client held: an approve and merge checks pocketd's verdict as it is
// at the owner's click (ADR-0018, Codex review of #24, round 4).
//
// pocketd first reads the pull request on GitHub (Client.Pull): its
// verdict backs the merge only at the head, base branch and merge base
// GitHub has now, and a base branch reset since pocketd's last round
// leaves none (Codex review of #26, round 7). That answer is direct, the
// row the merge checks: one pocketd did not find is not found, and one it
// could not read fails the confirmation (round 8).
//
// A pulls fetch follows, merged by the Data's rule, so an event newer than
// it wins (#24, round 5): the merge needs both direct and the merged row
// to back the head, since a replayed event may be older than the response
// yet win the merge by arriving later (#24, round 7). An answer a newer
// fetch superseded proves nothing, though the list loaded: the newer
// answer may refuse what this one backs (#24, round 8). done runs on the
// UI thread; found is false when pocketd no longer lists it.
//
// The read of GitHub is a request of its own: one that fails, or finds
// nothing, ends the confirmation, and the list fetch begun at the click,
// so an event newer than the click still wins, is abandoned unsent: the
// list's status and the other fetches stay the list fetches' (Codex
// review of #26, round 11).
func (s *Store) ReadPull(id int64, done func(fresh FreshPull, found bool, err error)) {
	f := s.Data.BeginPulls()
	var direct proto.PullRequest
	read := false
	s.do("Reading the pull request on GitHub", func(ctx context.Context, c *proto.Client) error {
		var pe *proto.Error
		one, err := c.Pull(ctx, id)
		switch {
		case err == nil:
			direct, read = one, one.ID == id
			return nil
		case errors.As(err, &pe) && pe.Code == proto.ErrorCodeNotFound:
			return nil
		}
		return err
	}, func(err error) {
		if err != nil || !read {
			s.Data.AbandonPulls(f)
			done(FreshPull{}, false, err)
			return
		}
		s.readPullList(f, id, direct, done)
	})
}

// readPullList is ReadPull's list fetch f, after pocketd's read of GitHub
// answered direct.
func (s *Store) readPullList(f Fetch, id int64, direct proto.PullRequest, done func(fresh FreshPull, found bool, err error)) {
	var list []proto.PullRequest
	s.do("Reading the pull request", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Pulls(ctx)
		return err
	}, s.track("pulls", func(err error) {
		if err != nil {
			s.Data.SetPullsError(f, err.Error())
			done(FreshPull{}, false, err)
			return
		}
		if !s.Data.SetPulls(f, list) {
			done(FreshPull{}, false, errors.New("a newer read of pocketd's pull requests answered first: check it again"))
			return
		}
		// A newer fetch that failed wins over this answer: the list is
		// then the cached one, which proves nothing now (Codex review of
		// #24, round 6).
		if st, why := s.Data.PullsStatus(); st != ListLoaded {
			done(FreshPull{}, false, fmt.Errorf("pocketd's pull requests did not load: %s", why))
			return
		}
		// The list's own row counts too, not only the row an event may
		// have won the merge with (Codex review of #26, round 9).
		listed := slices.IndexFunc(list, func(p proto.PullRequest) bool { return p.ID == id })
		if listed < 0 {
			done(FreshPull{}, false, nil)
			return
		}
		for _, p := range s.Data.Pulls() {
			if p.ID == id {
				done(FreshPull{merged: p, direct: direct, listed: list[listed], read: true}, true, nil)
				return
			}
		}
		done(FreshPull{}, false, nil)
	}))
}

// settleLessons drops the retry state of each decision whose proposal no
// longer waits as it decided it (decided here, elsewhere, by a request
// whose reply was lost, or changed by the agent). A decision on its way
// notes that its proposal stopped waiting, so its request id is retired
// once it settles, as settleAborts does for runs: a later decision on the
// lesson is a new request, never a retry of this one. One whose proposal
// still waits keeps its request id for the retry.
func (s *Store) settleLessons() {
	for id, p := range s.lessonDecisions {
		still := false
		if l, live := s.Data.LiveLesson(id); live {
			for _, decision := range []proto.LessonDecision{proto.LessonApprove, proto.LessonDiscard, proto.LessonForget} {
				if target, ok := LessonTarget(l, decision); ok && target == s.lessonSeen[id] {
					still = true
				}
			}
		}
		if p.Busy {
			if !still {
				s.lessonStopped[id] = true
			}
			continue
		}
		if !still || s.lessonStopped[id] {
			s.forgetLessonDecision(id)
		}
	}
}

// forgetLessonDecision drops a decision's state and retires its id.
func (s *Store) forgetLessonDecision(id int64) {
	delete(s.lessonDecisions, id)
	delete(s.lessonSeen, id)
	delete(s.lessonStopped, id)
	s.ids.Done(lessonKey(id))
}

func lessonKey(id int64) string { return fmt.Sprintf("lesson:%d", id) }

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
		s.settleModels()
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

// LoadModels fetches the models the service runs now. Every call fetches,
// even with an earlier fetch in flight, and the answer of the fetch begun
// last wins (Data.SetModels): a stream that came back to a restarted
// pocketd while an older fetch was on its way gets the models it runs now
// (Codex review of #48, rounds 3 and 4).
func (s *Store) LoadModels() {
	f := s.Data.Begin()
	var models []proto.Model
	s.do("Loading the models", func(ctx context.Context, c *proto.Client) (err error) {
		models, err = c.Models(ctx)
		return err
	}, func(err error) {
		if err != nil {
			s.Data.SetModelsError(f, err.Error())
			return
		}
		s.Data.SetModels(f, models)
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
	}, s.track("profiles", func(err error) {
		if err != nil {
			s.Data.SetProfilesError(f, err.Error())
			return
		}
		s.Data.SetProfiles(f, profiles)
	}))
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

// DecidingLesson is where the owner's decision on lesson id stands.
func (s *Store) DecidingLesson(id int64) Pending { return valueOf(s.lessonDecisions[id]) }

// Answering is where the owner's answer to ask id stands.
func (s *Store) Answering(id int64) Pending { return valueOf(s.answers[id]) }

// Creating is where the last new session stands.
func (s *Store) Creating() Pending { return s.creating }

func valueOf(p *Pending) Pending {
	if p == nil {
		return Pending{}
	}
	return *p
}

// Send sends text to session id, with the files attached to it
// (Attachments), which may stand for the text (ADR-0029). done runs on the
// UI thread with the outcome; a message tried again unchanged after a
// failure, its files included, keeps its request id. Its files leave the
// composer once pocketd took it. A message the session queues shows in
// its live state (Live.Queued) until its entry comes.
func (s *Store) Send(id int64, text string, done func(error)) {
	p := pending(s.sends, id)
	if p.Busy {
		done(errors.New("the last message is still on its way"))
		return
	}
	if s.ChangingModel(id).Busy {
		// Sent now, it could start the run before the change, on the model
		// the session had (MaySend).
		done(errors.New("the model change is still on its way"))
		return
	}
	if why := s.attachmentsBlock(id); why != "" {
		// Send waits for every upload, as pocket's does.
		done(errors.New(why))
		return
	}
	sent := slices.Clone(s.attachments[id])
	var uploads []int64
	for _, a := range sent {
		uploads = append(uploads, a.Upload.ID)
	}
	if err := ValidateMessage(text, len(uploads)); err != nil {
		p.Err = err.Error()
		done(err)
		return
	}
	key := fmt.Sprintf("send:%d", id)
	request := proto.SendMessage{RequestID: s.ids.For(key, sendAction(text, uploads)), Text: text, Attachments: uploads}
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
			s.attachments[id] = slices.DeleteFunc(s.attachments[id], func(a *attachment) bool { return slices.Contains(sent, a) })
		}
		done(err)
	})
}

// sendAction is what a message's request id is kept for: its text and
// its files, so a message tried again with other files is a new request.
// The text's length comes first, so no text and files read as another's
// (Codex review of #49, round 1).
func sendAction(text string, uploads []int64) string {
	// One buffer, so a long message with many files is not copied once a
	// file (Codex review of #49, round 6).
	action := make([]byte, 0, len(text)+21+21*len(uploads))
	action = strconv.AppendInt(action, int64(len(text)), 10)
	action = append(action, ':')
	action = append(action, text...)
	for _, id := range uploads {
		action = append(action, ',')
		action = strconv.AppendInt(action, id, 10)
	}
	return string(action)
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

// Answer answers ask id with text. The answer's reply moves the ask out of
// the open list; an answer tried again unchanged after a failure keeps its
// request id, so a lost reply is not answered twice. An answer to an ask
// answered elsewhere or withdrawn by a supervisor's message (409), or gone
// (404), is refused: the asks are fetched again, and the refusal, with the
// service's reason, stays in AnswerRefusals after the ask's card is gone
// (ADR-0037).
func (s *Store) Answer(id int64, text string) {
	p := pending(s.answers, id)
	if p.Busy {
		return
	}
	if err := ValidateAnswer(text); err != nil {
		p.Err = err.Error()
		return
	}
	// The answer as the owner wrote it, for a refusal's card; the request
	// and its retry key take it trimmed (Codex review of #62, round 1).
	typed := text
	s.answerTexts[id] = typed
	text = strings.TrimSpace(text)
	key := fmt.Sprintf("answer:%d", id)
	request := proto.AnswerAsk{RequestID: s.ids.For(key, text), Answer: text}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Answer A%d", id)
	var answered proto.Ask
	s.do(fmt.Sprintf("Answering A%d", id), func(ctx context.Context, c *proto.Client) (err error) {
		answered, err = c.AnswerAsk(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			// Taken: its draft goes with its card.
			s.forgetAnswer(id)
			delete(s.answerDrafts, id)
			s.Data.Answered(answered)
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound):
			// The service says why in words the owner can act on: answered
			// already, or withdrawn by a supervisor's message. The card goes
			// with the reload, or went with the ask's event; the refusal
			// stays until dismissed, with the answer, so the draft goes.
			s.forgetAnswer(id)
			delete(s.answerDrafts, id)
			why := pe.Message
			if pe.Code == proto.ErrorCodeNotFound || why == "" {
				why = "pocketd no longer has this question."
			}
			s.answerRefusals[id] = AnswerRefusal{Ask: id, Answer: typed, Why: why}
			s.loadAsks()
		default:
			p.Err = err.Error()
		}
		// The ask's event may have come before this reply: an ask no
		// longer open keeps no retry state.
		s.settleAnswers()
	})
}

// AnswerRefusal is an answer of the owner's that pocketd refused because
// its ask no longer waits: Why is the service's reason, as a supervisor's
// message that withdrew the ask (ADR-0037), and Answer what the owner
// wrote, so it is not lost with the ask's card.
type AnswerRefusal struct {
	Ask    int64
	Answer string
	Why    string
}

// AnswerRefusals are the refused answers the owner has not dismissed, by
// ask.
func (s *Store) AnswerRefusals() []AnswerRefusal {
	out := make([]AnswerRefusal, 0, len(s.answerRefusals))
	for _, r := range s.answerRefusals {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b AnswerRefusal) int { return cmp.Compare(a.Ask, b.Ask) })
	return out
}

// DismissAnswerRefusal drops the refusal of the answer to ask id.
func (s *Store) DismissAnswerRefusal(id int64) { delete(s.answerRefusals, id) }

// AnswerDraft is the owner's answer being written to ask id, not sent:
// the Inbox's field edits it in place. It lives until its ask leaves the
// open list by an event, or the answer is taken (askLeft, Answer).
func (s *Store) AnswerDraft(id int64) *string {
	d := s.answerDrafts[id]
	if d == nil {
		d = new(string)
		s.answerDrafts[id] = d
	}
	return d
}

// WithdrawnDraft is a question a supervisor's message withdrew while the
// owner had an answer to it written and not sent (ADR-0037): Ask is the
// question as its withdrawal's event gave it, Draft the answer as written.
// Its card stays in the Inbox with the draft until the owner dismisses it
// (the owner's choice, 2026-11-21).
type WithdrawnDraft struct {
	Ask   proto.Ask
	Draft string
}

// WithdrawnDrafts are the withdrawn questions with a draft the owner has
// not dismissed, by ask.
func (s *Store) WithdrawnDrafts() []WithdrawnDraft {
	out := make([]WithdrawnDraft, 0, len(s.withdrawnDrafts))
	for _, w := range s.withdrawnDrafts {
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b WithdrawnDraft) int { return cmp.Compare(a.Ask.ID, b.Ask.ID) })
	return out
}

// DismissWithdrawnDraft drops the card of the withdrawn question id, its
// draft with it.
func (s *Store) DismissWithdrawnDraft(id int64) { delete(s.withdrawnDrafts, id) }

// WithdrawalNote says why the card of withdrawn question a keeps the
// owner's draft: its thread's supervisor's message withdrew it. The
// supervisor is named as the thread gives it, by its session's agent, else
// that session's title; the message is pocketd's team_send, which only a
// session of that agent sends (ADR-0032).
func (d *Data) WithdrawalNote(a proto.Ask) string {
	who := "The supervisor's"
	if th, ok := d.Thread(a.Thread); ok {
		if s, ok := d.Session(th.Supervisor); ok {
			switch {
			case s.Profile != "":
				who = d.AgentName(s.Profile) + "'s"
			case strings.TrimSpace(s.Title) != "":
				who = s.Title + "'s"
			}
		}
	}
	return fmt.Sprintf("%s message to T%d withdrew A%d before you sent your answer: pocketd takes no answer to it now. Your draft is kept here until you dismiss it.",
		who, a.Thread, a.ID)
}

// askLeft follows ask a, which an event says left the open list, answered
// or withdrawn, before the event is applied:
//   - An answer of the owner's that is out learns how its ask left
//     (settleAnswers): a withdrawal means pocketd did not take it.
//   - The draft of an answer not sent goes. One a supervisor's message
//     withdrew with text in it stays, as a card with its question, the
//     draft and why, until the owner dismisses it (ADR-0037); with no text
//     it goes with the card. A later event of the same ask keeps the card
//     as it is, and brings back none dismissed.
func (s *Store) askLeft(a proto.Ask) {
	_, out := s.answerTexts[a.ID]
	if out {
		s.answerOutcomes[a.ID] = a.State
	}
	draft := s.answerDrafts[a.ID]
	delete(s.answerDrafts, a.ID)
	if a.State != proto.AskWithdrawn || out || draft == nil || strings.TrimSpace(*draft) == "" {
		return
	}
	if _, kept := s.withdrawnDrafts[a.ID]; !kept {
		s.withdrawnDrafts[a.ID] = WithdrawnDraft{Ask: a, Draft: *draft}
	}
}

// DecideLesson approves or discards what waits on lesson, or forgets a
// lesson in force, as the owner saw it: an approval names the waiting
// text, or that it is to be forgotten, and holds only for that
// (ADR-0015). The reply moves the lesson out of the waiting list; a decision tried again unchanged after a failure keeps its
// request id, so a lost reply is not decided twice. A lesson that changed
// since (409) or is gone (404) is fetched again.
func (s *Store) DecideLesson(lesson proto.Lesson, decision proto.LessonDecision) {
	id := lesson.ID
	p := pending(s.lessonDecisions, id)
	if p.Busy {
		return
	}
	request := proto.DecideLesson{Decision: decision}
	if decision == proto.LessonApprove {
		request.Text, request.Forget = SeenText(lesson)
	}
	seen, ok := LessonTarget(lesson, decision)
	if !ok {
		p.Err = "Nothing waits on this lesson for that decision."
		return
	}
	if s.lessonSeen[id] != seen {
		// Another proposal than the one a failed decision saw: a new
		// decision, never a retry of that one.
		s.ids.Done(lessonKey(id))
		delete(s.lessonStopped, id)
	}
	s.lessonSeen[id] = seen
	key := lessonKey(id)
	request.RequestID = s.ids.For(key, seen)
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "%s lesson L%d", decision, id)
	f := s.Data.BeginLessonDecision()
	var decided proto.Lesson
	s.do(fmt.Sprintf("Deciding L%d", id), func(ctx context.Context, c *proto.Client) (err error) {
		decided, err = c.DecideLesson(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		var pe *proto.Error
		if err != nil {
			s.Data.LessonDecisionFailed(f)
		}
		switch {
		case err == nil:
			s.forgetLessonDecision(id)
			if s.Data.LessonDecided(f, decided) {
				s.loadLessons()
			}
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound):
			s.ids.Done(key)
			// The service says why, in words the owner can act on: the
			// lesson changed, nothing waits, or its agent's lessons are
			// full. It shows until the reload brings what waits now; then
			// the decision's state goes (settleLessons).
			p.Err = pe.Message
			if pe.Code == proto.ErrorCodeNotFound {
				p.Err = "pocketd no longer has this lesson."
			}
			s.loadLessons()
		default:
			p.Err = err.Error()
		}
		s.settleLessons()
	})
}

// SeenText is what the owner sees waiting on lesson, and approves: its
// text, or the new text of a change, or that it is to be forgotten.
func SeenText(l proto.Lesson) (text string, forget bool) {
	switch {
	case l.State == proto.LessonPending || l.Change == nil:
		return l.Text, false
	case l.Change.Forget:
		return "", true
	default:
		return l.Change.Text, false
	}
}

// WorktreeTimeout bounds a new session's request that makes a worktree:
// pocketd gives git 5 minutes for it, past the client's usual deadline,
// and a request that gave up earlier would show a session that starts as
// one that failed (protocol rule 6a; Codex review of #46, round 1).
const WorktreeTimeout = 6 * time.Minute

// createTimeout is how long CreateSession waits for request's answer:
// WorktreeTimeout for one that makes a worktree, base for any other.
func createTimeout(request proto.CreateSession, base time.Duration) time.Duration {
	if request.Worktree {
		return max(base, WorktreeTimeout)
	}
	return base
}

// createAction is request as CreateSession sends it, a folder typed and the
// title trimmed, and no request id, and the action its request id is kept
// for. A repository's folder is its checkout's, as its profile names it,
// and goes as it is (Codex review of #46, round 1).
func createAction(request proto.CreateSession) (proto.CreateSession, string) {
	if request.Repository == "" {
		request.Cwd = strings.TrimSpace(request.Cwd)
	}
	request.Title = strings.TrimSpace(request.Title)
	request.RequestID = ""
	action, _ := json.Marshal(request)
	return request, string(action)
}

// NextCreateID is the request id CreateSession would send request with
// now (requestIDs.Peek): New Session shows the worktree it names before
// it sends (protocol rule 6a), and a retry of a request that failed shows
// the worktree that try may have made.
func (s *Store) NextCreateID(request proto.CreateSession) string {
	_, action := createAction(request)
	return s.ids.Peek("create", action)
}

// CreateSession starts a session (its RequestID is the store's to set).
// done runs on the UI thread with the new session. A request tried again
// unchanged after a failure keeps its request id, so a lost answer does not
// start a second session.
func (s *Store) CreateSession(request proto.CreateSession, done func(proto.Session, error)) {
	request, action := createAction(request)
	if err := ValidateNewSession(request, s.Data.models); err != nil {
		done(proto.Session{}, err)
		return
	}
	if s.creating.Busy {
		done(proto.Session{}, errors.New("a new session is still on its way"))
		return
	}
	request.RequestID = s.ids.For("create", action)
	s.creating = Pending{Busy: true}
	s.log(LevelCommand, "New session in %s on %s", request.Cwd, request.Model)
	var session proto.Session
	f := s.Data.Begin()
	s.doWithin("Starting a session", createTimeout(request, s.opts.Timeout), func(ctx context.Context, c *proto.Client) (err error) {
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
		s.Data.Created(f, session)
		done(session, nil)
	})
}
