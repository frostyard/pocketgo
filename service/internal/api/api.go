// Package api serves protocol version 6 (docs/specs/protocol.md) over HTTP:
// REST for commands and Server-Sent Events for live updates.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/terminal"
)

// SessionSpec is a session to create: where it works, its model, and the
// profile that gives its agent an identity and tools (ADR-0013).
type SessionSpec struct {
	// Title is the validated title, "" for the default.
	Title    string
	Cwd      string
	Model    proto.ModelRef
	Thinking string
	// Profile is nil for a plain session; Route is the router's rule.
	Profile *profiles.Profile
	Route   string
	// Thread is the thread whose specialist works in the session, 0 for
	// none: its agent gets team_message (ADR-0014).
	Thread int64
	// Plan is the plan whose lead plans in the session, its plan
	// thread's specialist: its agent gets team_plan too (ADR-0020).
	Plan int64
	// Consult is the consult whose expert answers in the session: its
	// agent gets read and team_finding, and nothing else (ADR-0020).
	Consult int64
	// Roster, for a supervisor's session, names whom it can delegate to;
	// it goes in the session's instructions.
	Roster string
	// Lessons, for a session started from a profile, is its agent's
	// lessons section (team.LessonsSection), at the end of its
	// instructions (ADR-0015).
	Lessons string
	// Review is a cross-review's session (ADR-0019): read-only tools and
	// team_verdict, no other team tool.
	Review bool
	// Worker is a thread's worker (ADR-0021): the worker's instructions,
	// WorkerTools (nil for every coding tool) and no team tool. Thread
	// names its thread; Profile is nil.
	Worker      bool
	WorkerTools []string
	// StandingKey, for a profile's standing session (ADR-0026), is the key
	// every request that opens it finds it by: the engine records the
	// session under it as well as under the request's own key, and a
	// session made under it already is the one any request gets.
	StandingKey string
}

// Created is a session the engine created for a request key, with what it
// was created from: a retry records this, not a new choice (rule 5).
type Created struct {
	ID int64
	// Legacy is a record from before pocketd kept what a session was
	// created from: its Title is unknown, so a retry's own title is
	// checked and taken.
	Legacy   bool
	Title    string
	Cwd      string
	Model    proto.ModelRef
	Thinking string
	Profile  string
	Route    string
	// Standing is a profile's standing session (ADR-0026), recorded as one
	// whatever the profile says by the time a retry records it.
	Standing bool
}

// ProfileSource reads the agents' profiles; profiles.Folder implements it.
type ProfileSource interface {
	Load() ([]profiles.Profile, []profiles.Problem, error)
	Find(id string) (profiles.Profile, error)
	// Workers is the owner's worker list (ADR-0022).
	Workers() (profiles.Workers, error)
	// Schedules are the owner's schedules (ADR-0030).
	Schedules() (profiles.Schedules, error)
}

// Engine runs sessions; service/pig implements it.
type Engine interface {
	Models(ctx context.Context) ([]proto.Model, error)
	// TranscriptAfter is Transcript's entries after entry after, and the
	// live state of their revision.
	TranscriptAfter(ctx context.Context, id, after int64) ([]proto.Entry, proto.Live, error)
	// Tools are the names of the tools an agent can be given.
	Tools() []string
	// CreateSession is idempotent by key: the same key returns the same id.
	CreateSession(ctx context.Context, key string, spec SessionSpec) (int64, error)
	// Created is the session created for key, if one was; its Model is
	// zero for a session created before pocketd kept what it was created
	// from.
	Created(ctx context.Context, key string) (Created, bool, error)
	Watch(ctx context.Context, id int64) error
	Send(ctx context.Context, id int64, key, text string) (int64, error)
	// SendIdle is Send for a message that may only start a run: PiG
	// refuses it, in the transaction that would admit it, while the session
	// has a run (ErrSessionBusy), and writes nothing (ADR-0024).
	SendIdle(ctx context.Context, id int64, key, text string) (int64, error)
	Abort(ctx context.Context, id int64) error
	Transcript(ctx context.Context, id int64) ([]proto.Entry, proto.Live, error)
	Usage(ctx context.Context, id int64) (proto.Usage, error)
	// RunIdentity names the session's current run by its first entry.
	RunIdentity(ctx context.Context, id int64) (first int64, busy bool, err error)
	WaitIdle(ctx context.Context, id int64) error
	TaskEnded(ctx context.Context, taskID int64) (bool, error)
	// SetVerdictTool gives team_verdict to a review thread's session, or
	// takes it from any other (ADR-0019); it was, before, given to a thread's specialist session
	// started before it existed, and says whether it did (ADR-0017).
	SetVerdictTool(ctx context.Context, id int64, on bool) (bool, error)
	// Git runs git with args in dir, in the sandbox (worktree.Git), and
	// Exists says whether a path names an entry there (worktree.Exists).
	Git(ctx context.Context, dir string, args ...string) (string, error)
	// GitPage runs git as Git does and keeps limit bytes of its output from
	// offset on, with its whole length: a review pages through a patch
	// larger than Git's bound (ADR-0019).
	GitPage(ctx context.Context, dir string, offset, limit int, args ...string) (page string, more bool, err error)
	// GitEach runs git as Git does and gives each record of its output,
	// split at sep, to each, with bounded memory (worktree.Each).
	GitEach(ctx context.Context, dir string, sep byte, each func(string) error, args ...string) error
	Kind(ctx context.Context, path string) (string, error)
	Read(ctx context.Context, path string) (string, bool, error)
	Same(ctx context.Context, a, b string) (bool, error)
	// Dir says whether path is a folder, a link to one included, as a
	// command in the sandbox sees it.
	Dir(ctx context.Context, path string) (bool, error)
	// MakeDir makes the folder path, and only it, in the sandbox: made is
	// false when an entry is there already (worktree.MakeDir).
	MakeDir(ctx context.Context, path string) (made bool, err error)
	// Real is the folder path names, every link on its way followed, as
	// a command in the sandbox resolves it; ok is false for no folder.
	Real(ctx context.Context, path string) (real string, ok bool, err error)
	// Placed says where session's submission of request key is: absent,
	// queued, placed in the transcript, or withdrawn without reaching it
	// (an abort's). The key, not a submission number, proves it is the
	// message owed.
	Placed(ctx context.Context, session int64, key string) (Placement, error)
	// PlacedEntry is the transcript entry of session's submission of
	// request key, once a run placed it: a shift's round begins there
	// (ADR-0024).
	PlacedEntry(ctx context.Context, session int64, key string) (entry int64, ok bool, err error)
	// Busy says whether session has a run, from its live state alone.
	Busy(ctx context.Context, session int64) (bool, error)
	// Idle says session has no run and nothing waiting in its inbox: a
	// message submitted now starts a run with it at once (ADR-0024).
	Idle(ctx context.Context, session int64) (bool, error)
	// Withdraw withdraws session's submission of request key if it is
	// still queued, in one PiG transaction, and says whether it did; one
	// placed meanwhile is left as it is.
	Withdraw(ctx context.Context, session int64, key string) (bool, error)
	// SessionModel is the model session runs now, and its thinking level,
	// as PiG has them (ADR-0026); PiG reads a level it does not hold as
	// off.
	SessionModel(ctx context.Context, session int64) (ModelChoice, error)
	// SetModel gives session choice, as the owner changes it from the
	// composer (ADR-0028), in one commit that keeps it under request key
	// with route, the router's rule the session records for it
	// (ModelChange): at once while it is idle, else from its run's next
	// round. A key whose change is in changes nothing again. pick keeps
	// it, in that commit, as the owner's pick of a standing session
	// (StandingPick), which Start Over drops in its reset's commit.
	SetModel(ctx context.Context, session int64, key string, choice ModelChoice, route string, pick bool) error
	// ModelChange is the change of session's model request key made, as
	// PiG keeps it; false for none.
	ModelChange(ctx context.Context, session int64, key string) (KeptChange, bool, error)
	// StandingPick is the owner's pick of standing session's model, as PiG
	// keeps it; false for none.
	StandingPick(ctx context.Context, session int64) (ModelChoice, bool, error)
	// SetLessons makes section the end of session's instructions, from
	// profiles.LessonsMarker on, and changes nothing else in them
	// (ADR-0015). Instructions that end so already stay as they are.
	SetLessons(ctx context.Context, session int64, section string) error
	// GH runs gh with args in the sandbox, with the service user's sign-in
	// (ADR-0016), and returns what it printed.
	GH(ctx context.Context, args ...string) (string, error)
	// CompactIfOver compacts a standing session once its context reaches
	// compactAt below its model's window (team.ShouldCompact), in a
	// transaction that finds it idle, with no compaction under way and
	// nothing written since its estimate; it says whether it started one
	// (ADR-0026).
	CompactIfOver(ctx context.Context, session int64, compactAt int) (bool, error)
	// StartOver starts session's context afresh with a reset entry keyed
	// by request key, and gives it the agent spec makes as Reconfigure
	// does (none for a nil spec), in one transaction while it has no run
	// and nothing queued, else ErrSessionBusy with nothing written; that
	// transaction drops the owner's pick (StandingPick). A key whose reset
	// is in does nothing, and a pick made since stays.
	StartOver(ctx context.Context, session int64, key string, spec *SessionSpec) (Reconfigured, error)
	// Reconfigure gives session the instructions, tools and model spec
	// makes, keeping the lessons section it has, in one transaction that
	// finds it with no run; one with a run changes nothing, and what
	// differed waits (ReconfigureDeferred).
	Reconfigure(ctx context.Context, session int64, spec SessionSpec) (Reconfigured, error)
}

// ModelChoice is a session's model at a thinking level, "" for the
// model's default (ADR-0028).
type ModelChoice struct {
	Model    proto.ModelRef
	Thinking string
}

// KeptChange is a change of a session's model as the engine keeps it
// under its request's key (ADR-0028).
type KeptChange struct {
	Choice ModelChoice
	// Route is the router's rule the session records for it.
	Route string
	// Latest says no change of the session's model by the owner came
	// after it (Codex review of #48, round 4).
	Latest bool
}

// Reconfigured is what Engine.Reconfigure did.
type Reconfigured int

const (
	// ReconfigureSame: the session had what spec makes already.
	ReconfigureSame Reconfigured = iota
	// ReconfigureApplied: the session has it now.
	ReconfigureApplied
	// ReconfigureDeferred: something differed, and the session had a run,
	// so nothing changed.
	ReconfigureDeferred
)

// ErrSessionBusy is a SendIdle PiG refused: the session has a run.
var ErrSessionBusy = errors.New("the session has a run")

// Placement is where a submission is.
type Placement int

const (
	// PlacementAbsent: no submission of the key.
	PlacementAbsent Placement = iota
	// PlacementQueued: admitted, and waiting for a run to place it.
	PlacementQueued
	// PlacementPlaced: its entry is in the transcript.
	PlacementPlaced
	// PlacementWithdrawn: it ended without an entry, as a queued message
	// an abort withdraws.
	PlacementWithdrawn
)

// abortSettle bounds how long an abort waits for its run to settle before
// it retires the run's approvals.
const abortSettle = 15 * time.Second

// retryDelay is how long a session event that failed to read waits before
// it is read again.
const retryDelay = time.Second

// writeTimeout bounds one event write, so a client that stops reading
// cannot hold a stream past revocation or shutdown.
const writeTimeout = 10 * time.Second

// maxBody bounds every request body.
const maxBody = 1 << 20

// defaultStreamCheck is how often an event stream sends a keepalive comment
// and checks that its device is still signed in.
const defaultStreamCheck = 10 * time.Second

// Server is the HTTP API. Create it with New, finish the wiring with
// SetApprovals and SetEngine before serving, and serve Handler.
type Server struct {
	store     *store.Store
	hub       *events.Hub
	approvals *approvals.Manager
	version   string
	now       func() time.Time
	log       *slog.Logger
	engine    Engine
	// profiles are the agents' profiles; nil when pocketd has none.
	profiles ProfileSource
	// deskCap is how many threads not closed the desk holds; teamMu
	// orders delegations, so two never both take its last place.
	deskCap int
	teamMu  sync.Mutex
	// workMu orders thread and ask changes with their events: each row
	// changes and goes out under it, so events leave in commit order.
	workMu sync.Mutex
	// beforeAttach runs in a thread's start after its session exists and
	// before the thread takes it; tests close the thread there. Nil does
	// nothing.
	beforeAttach func(thread int64)
	// delivering is set while a pass over the undelivered reports and
	// answers waits or runs (deliverLater), and again when one was asked
	// for meanwhile; sessionMu guards both.
	delivering, again bool
	// reviewing says a review pass runs on its own goroutine
	// (reviewSoon), and reviewAgain that one was asked for meanwhile;
	// sessionMu guards both. reviewMu runs one review pass at a time
	// (ADR-0019).
	reviewing, reviewAgain bool
	// reviewsOn says the service's rounds started (StartPulls, after
	// Resume), so a review pass may run; reviewWaiting is what the next
	// pass starts from. sessionMu guards both.
	reviewsOn     bool
	reviewWaiting reviewWork
	// afterRunEnded, in tests, sees each run's end runEnded followed, and
	// afterRunStarted each run's start runStarted followed.
	afterRunEnded, afterRunStarted func(id int64)
	// shiftMu runs one shift pass at a time (ADR-0024); afterShiftPass, in
	// tests, sees each pass end. shifting says a pass runs, shiftAgain
	// that one was asked for meanwhile; sessionMu guards both.
	shiftMu              sync.Mutex
	afterShiftPass       func()
	shifting, shiftAgain bool
	// shiftRetrying says a pass's retry waits (shiftLater); sessionMu
	// guards it.
	shiftRetrying bool
	// threadRetry are the threads whose event a failed read of their shift
	// held back (publishThread); threadRetryMu guards it.
	threadRetryMu sync.Mutex
	threadRetry   map[int64]bool
	// goShift runs a shift pass's goroutine, and shiftTimer a shift's wake
	// at its deadline; tests run both when they choose. shiftWakes are the
	// threads with a wake set; sessionMu guards it.
	goShift    func(f func())
	shiftTimer func(d time.Duration, f func()) (cancel func())
	shiftWakes map[int64]pendingWake
	// shiftLocks hold one shift each (lockShift): a stop, a resume, the
	// owner's message to its specialist, and a round's decision
	// (commitRound) go one at a time. A lock is taken before outboxMu and
	// workMu, never after, and never couples one shift, or a session that
	// is no shift's, with another (Codex review of #36).
	shiftLocks keyLocks
	// ciWaits are the rounds that wait on CI, by thread (driveShift);
	// shiftMu guards it.
	ciWaits map[int64]ciWait
	// workersDue are the workers whose replies, or whose stop, could not
	// be done; the delivery pass tries again (ADR-0021). workerMu guards
	// it.
	workerMu      sync.Mutex
	workersDue    map[int64]bool
	workerScanDue bool
	// workerStartMu starts one worker at a time (Server.workerOf).
	workerStartMu sync.Mutex
	// standingDue says a delivery pass looks after every standing session
	// (standingPass): the first after start, as a run's end may have gone
	// unseen while pocketd stopped. standingMu guards it.
	standingMu  sync.Mutex
	standingDue bool
	// afterStandingRead, in tests, runs once a standing session's refresh
	// read its profile, its row and the models; onRefreshLock right before
	// a standing session's refresh takes refreshMu.
	afterStandingRead func()
	onRefreshLock     func()
	// modelLocks holds each session, by id, from the read that decides a
	// change of its model to the change's record (lockModel); onModelLock,
	// in tests, runs right before a change takes it.
	modelLocks  keyLocks
	onModelLock func(session int64)
	// afterConfirmRead, in tests, runs between a confirmation's read of a
	// pull request and its answer.
	afterConfirmRead func()
	reviewMu         sync.Mutex
	// abortsDue are the ended reviews' sessions whose run a stop did not
	// end, or could not tell; each review pass tries again. reviewMu
	// guards it.
	abortsDue map[int64]bool
	// reviewAsync runs a review pass a report asks for on its own
	// goroutine; reviewGit is the git of a review's folder. Tests set
	// both.
	reviewAsync bool
	reviewGit   reviewGit
	// repoURL is where git fetches a repository GitHub names owner/name
	// (repositoryURL); a test fetches from a folder.
	repoURL func(string) string
	// passes counts the delivery passes that run; Stop waits for them, and
	// passCtx, which Stop cancels, ends their work.
	passes     sync.WaitGroup
	passCtx    context.Context
	passCancel context.CancelFunc
	// keys serializes the requests of one request key (keyLocks).
	keys keyLocks
	// outboxMu serializes bringing messages to sessions (place): each
	// answer, report or brief is read, placed and noted under it, so two
	// callers never advance one message's attempt twice. It is taken
	// before workMu, never after.
	outboxMu sync.Mutex
	// openThread opens a thread whose brief went (store.OpenThread), and
	// closeFailed closes one that could not start (store.FailThread);
	// tests make them fail.
	openThread  func(ctx context.Context, id int64) (store.ThreadRow, error)
	closeFailed func(ctx context.Context, id int64, why string) (store.ThreadRow, error)
	// releaseRequest frees a close's reservation that did nothing
	// (store.ReleaseRequest); tests make it fail. unreleased are the
	// reservations whose release failed, by key, with their kind: a
	// delivery pass frees them. releaseMu guards it, and is taken last:
	// an owner's message takes no other lock for it (Codex review of #36).
	releaseRequest func(ctx context.Context, key, kind string) error
	releaseMu      sync.Mutex
	unreleased     map[string]string
	// claimsDue says the claims of model changes left by a stop, made
	// before startedAt, are still to be looked at: the first delivery pass
	// that reads them adds them to unreleased, which frees each PiG has no
	// change of (Codex review of #48, round 4). releaseMu guards it.
	claimsDue bool
	startedAt time.Time
	// lessonsMu guards stale and allStale: the agents whose sessions'
	// lessons may be out of date, and whether every agent's are (at
	// start). A delivery pass brings them up to date (refreshLessons).
	lessonsMu sync.Mutex
	stale     map[string]bool
	allStale  bool
	// refreshMu serializes refreshLessons.
	refreshMu sync.Mutex
	// pullLocks holds each pull request, by store ref, from its read on
	// GitHub to its record: two reads of one never cross (ADR-0016). A
	// wait for one ends with its context.
	pullLocks ctxLocks
	// pullEvery is how often open pull requests are read again
	// (StartPulls), 0 for never; pullTimer schedules the next round
	// (tests replace it).
	pullEvery time.Duration
	pullTimer func(d time.Duration, f func()) (cancel func())
	pullStop  func()
	// consultTimer wakes a delivery pass at a consult's deadline
	// (armConsults; tests replace it), and consultCancel stops the one
	// set; sessionMu guards consultCancel.
	consultTimer  func(d time.Duration, f func()) (cancel func())
	consultCancel func()
	// zone is the service's time zone, which a schedule that names none
	// reads its clock times in (SetZone; ADR-0030). scheduleMu runs one
	// schedules pass at a time, and orders the schedules events, with
	// schedulesSent, the last list sent. schedulesOn says StartSchedules
	// ran; scheduling says a pass runs, scheduleAgain that one was asked
	// for meanwhile; goSchedule runs a pass's goroutine and scheduleTimer
	// its wake, which scheduleCancel stops; sessionMu guards these.
	zone                      *time.Location
	scheduleMu                sync.Mutex
	schedulesSent             []byte
	schedulesOn               bool
	scheduling, scheduleAgain bool
	goSchedule                func(f func())
	scheduleTimer             func(d time.Duration, f func()) (cancel func())
	scheduleCancel            func()
	// withTimeout bounds the git a thread's start runs (context.WithTimeout);
	// tests see the bound it asks for.
	withTimeout func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
	// afterReport runs in sendReports between two reports, outboxMu free;
	// tests check it is. Nil does nothing.
	afterReport func()
	// afterRecheck runs once the specialist's profile was read the last
	// time, right before its session starts; tests change the profile
	// there. Nil does nothing.
	afterRecheck func()
	// afterFolder runs once a new specialist's folder is ready, before its
	// profile is read again; tests change the profile there.
	afterFolder func()
	// planRead reads a plan to publish (store.PlanFacts; tests make it
	// fail), and planRetry are the plans whose event a failed read held
	// back, which a delivery pass publishes; workMu guards planRetry.
	planRead  func(ctx context.Context, id int64) (store.PlanFacts, error)
	planRetry map[int64]bool
	// holdIfFull records a plan the desk's cap held back
	// (store.HoldIfFull); tests make it fail.
	holdIfFull func(ctx context.Context, plan int64, cap int, check func(store.PlanFacts) error) (bool, error)
	// openConsults reads the consults that start or run
	// (store.OpenConsults); tests make it fail.
	openConsults func(ctx context.Context) ([]store.ConsultRow, error)
	// afterPlanRead runs in a plan task's delivery after the plan's first
	// read, before the transaction that checks the gate; afterConsultSpec
	// once a consult's new session is specified, before the team's locks.
	// Tests change the plan or the profile there.
	afterPlanRead    func()
	afterConsultSpec func()
	// afterDeskCount runs in a plan task's delivery once the desk was
	// counted; tests close a thread there.
	afterDeskCount func()
	// beforeCloseLocks runs in a thread's close right before it takes the
	// team's locks.
	beforeCloseLocks func()

	createMu sync.Mutex
	// afterSessionWorktree runs once a new session's worktree is made,
	// under createMu, before its profile is read again; tests change the
	// profile or the worktree there. beforeCreateLock runs right before a
	// new session's start takes createMu.
	afterSessionWorktree, beforeCreateLock func()
	// markWorktree records a step of a session's worktree
	// (store.MarkWorktree); tests make it fail.
	markWorktree func(ctx context.Context, key string, step int) error
	// sessionMu orders session events: each is read and published under
	// it, so the last one carries every change made before it was read. It
	// also guards retries, untouched and stopped.
	sessionMu sync.Mutex
	// retries are the session events that failed to read and wait to be
	// tried again, by session; retry schedules one (tests replace it).
	retries map[int64]*pendingRetry
	retry   func(f func()) (cancel func())
	// untouched are the sessions with an entry whose time is not written
	// yet, with that time; touch writes it (store.TouchSession; tests
	// replace it).
	untouched map[int64]time.Time
	touch     func(ctx context.Context, id int64, at time.Time) error
	// stopped is set by Stop: no retry runs after it.
	stopped bool
	// emit publishes an event to the hub (tests watch it).
	emit func(proto.Event)

	liveMu   sync.Mutex
	live     map[int64]proto.Live
	states   map[int64]proto.SessionState
	stopOnce sync.Once
	stop     chan struct{}
	// streamCheck is defaultStreamCheck; tests shorten it.
	streamCheck time.Duration
	// terms are the owner's shells (ADR-0025); Stop ends them.
	terms *terminal.Manager
	// recordTerminalStart records a start's request before its shell
	// starts (store.RecordRequest; tests make it fail), and
	// afterTerminalStart runs once the shell started; tests drop the
	// request there.
	recordTerminalStart func(ctx context.Context, key, kind, result string) error
	afterTerminalStart  func()
	// freeTerminalStart frees the request of a start whose shell did not
	// start (store.DeleteRequest); tests make it fail.
	freeTerminalStart func(ctx context.Context, key string) error
}

// New returns a server.
func New(s *store.Store, hub *events.Hub, version string, now func() time.Time, log *slog.Logger) *Server {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	passCtx, passCancel := context.WithCancel(context.Background())
	server := &Server{store: s, hub: hub, version: version, now: now, log: log,
		live: map[int64]proto.Live{}, states: map[int64]proto.SessionState{}, stop: make(chan struct{}), streamCheck: defaultStreamCheck,
		retries: map[int64]*pendingRetry{}, retry: func(f func()) func() { t := time.AfterFunc(retryDelay, f); return func() { t.Stop() } },
		untouched: map[int64]time.Time{}, touch: s.TouchSession, emit: func(e proto.Event) { hub.Publish(e) }, deskCap: team.DefaultDeskCap,
		openThread: s.OpenThread, closeFailed: s.FailThread, releaseRequest: s.ReleaseRequest, unreleased: map[string]string{}, stale: map[string]bool{}, pullTimer: func(d time.Duration, f func()) func() { t := time.AfterFunc(d, f); return func() { t.Stop() } },
		consultTimer:  func(d time.Duration, f func()) func() { t := time.AfterFunc(d, f); return func() { t.Stop() } },
		shiftTimer:    func(d time.Duration, f func()) func() { t := time.AfterFunc(d, f); return func() { t.Stop() } },
		scheduleTimer: func(d time.Duration, f func()) func() { t := time.AfterFunc(d, f); return func() { t.Stop() } },
		goSchedule:    func(f func()) { go f() }, zone: time.UTC,
		goShift: func(f func()) { go f() }, shiftWakes: map[int64]pendingWake{}, threadRetry: map[int64]bool{}, ciWaits: map[int64]ciWait{},
		planRead: s.PlanFacts, planRetry: map[int64]bool{}, openConsults: s.OpenConsults, holdIfFull: s.HoldIfFull, passCtx: passCtx, passCancel: passCancel, withTimeout: context.WithTimeout, reviewAsync: true, repoURL: repositoryURL,
		standingDue: true, markWorktree: s.MarkWorktree, claimsDue: true, startedAt: now()}
	server.reviewGit = sandboxGit{server}
	server.terms = terminal.New(terminal.Options{Now: now, OnChange: server.publishTerminals})
	return server
}

// pendingRetry is a session event waiting to be read again. Its identity
// is its ownership: a retry runs only while it is still the session's.
type pendingRetry struct{ cancel func() }

// SetApprovals and SetEngine finish the wiring; both happen before serving
// and before the engine resumes tasks.
func (s *Server) SetApprovals(m *approvals.Manager) { s.approvals = m }
func (s *Server) SetEngine(e Engine)                { s.engine = e }

// Stop ends every open event stream, the retries of session events, the
// delivery passes and the owner's shells, and waits for a pass that runs
// and, 3 s at most, for the shells; call it before shutting the HTTP
// server down, so the shutdown does not wait on streams or polls, and
// before the engine and the stores close.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.sessionMu.Lock()
		s.stopped = true
		for id, r := range s.retries {
			r.cancel()
			delete(s.retries, id)
		}
		for id, w := range s.shiftWakes {
			w.cancel()
			delete(s.shiftWakes, id)
		}
		s.passCancel()
		if s.pullStop != nil {
			s.pullStop()
		}
		if s.consultCancel != nil {
			s.consultCancel()
			s.consultCancel = nil
		}
		if s.scheduleCancel != nil {
			s.scheduleCancel()
			s.scheduleCancel = nil
		}
		s.sessionMu.Unlock()
		// A delivery pass, or a round of pull requests, that runs ends
		// before the engine and the stores close.
		s.passes.Wait()
		// The owner's shells end, and their polls with them, so the
		// shutdown waits on none (ADR-0025).
		s.terms.Close()
	})
}

// PublishEntry, PublishLive and PublishApproval are the engine's and the
// approvals manager's callbacks. They never block on a client.
//
// PublishEntry sends the entry, then the session it changed: a new entry
// moves the session's updatedAt, and an answer adds to its usage. The entry
// goes out and its time is noted under sessionMu, so no session event read
// before that time is written goes out after the entry.
func (s *Server) PublishEntry(entry proto.Entry) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.emit(proto.Event{Type: proto.EventEntry, Entry: &entry})
	if at := s.now(); at.After(s.untouched[entry.SessionID]) {
		s.untouched[entry.SessionID] = at
	}
	s.publishSessionLocked(entry.SessionID, true)
	// A message that starts a run ends the run before it, though the
	// worker never went idle between them: a worker's reply goes out then,
	// not when its next run ends (ADR-0021; Codex review of #29, round 2).
	if entry.Kind == proto.EntryUser && !s.stopped {
		// Stop waits for it, as for any pass (Codex review of #29, round
		// 3); sessionMu is held.
		s.passes.Add(1)
		go func() {
			defer s.passes.Done()
			s.runStarted(entry.SessionID)
		}()
	}
	// A final answer ends its run: a standing session is looked after
	// then too, since a short run's busy state may never reach
	// PublishLive (ADR-0026; Codex review of #43, round 5).
	if entry.Kind == proto.EntryAssistant && entry.StopReason != "toolUse" && !s.stopped {
		s.passes.Add(1)
		go func() {
			defer s.passes.Done()
			s.standingRunEnded(entry.SessionID)
		}()
	}
}

// runStarted follows a message that started a session's run: a worker's
// replies of the runs before go to its specialist.
func (s *Server) runStarted(id int64) {
	s.followWorker(id)
	if s.afterRunStarted != nil {
		s.afterRunStarted(id)
	}
}

func (s *Server) PublishLive(live proto.Live) {
	s.liveMu.Lock()
	was := s.live[live.SessionID].Busy
	s.live[live.SessionID] = live
	s.liveMu.Unlock()
	s.emit(proto.Event{Type: proto.EventLive, Live: &live})
	s.publishStateChange(live.SessionID)
	if was && !live.Busy {
		s.sessionMu.Lock()
		if !s.stopped {
			s.passes.Add(1)
			go func() {
				defer s.passes.Done()
				s.runEnded(live.SessionID)
			}()
		}
		s.sessionMu.Unlock()
	}
}

func (s *Server) PublishApproval(approval proto.Approval) {
	s.emit(proto.Event{Type: proto.EventApproval, Approval: &approval})
	s.publishStateChange(approval.SessionID)
}

// state is a session's state (rule 11): waiting while an approval of it is
// pending, running while its run is busy, idle otherwise.
func (s *Server) state(waiting bool, id int64) proto.SessionState {
	if waiting {
		return proto.SessionWaiting
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live[id].Busy {
		return proto.SessionRunning
	}
	return proto.SessionIdle
}

func (s *Server) publishStateChange(id int64) { s.publishSession(id, false) }

// publishSession sends session id as it is now: always, as after an entry,
// or else only when its state is not the one the last session event sent.
// Session events are read and sent one at a time (sessionMu), so they leave
// in the order they were read and the last one is current. One that fails
// to read is read again after retryDelay, until it goes out or the server
// stops: the entry or change it follows has gone out already, and nothing
// else may send the session again. A session with an entry whose time is not
// written yet is written first, and the summary waits for it: a summary with
// the time before the entry would keep the session where it was.
func (s *Server) publishSession(id int64, always bool) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.publishSessionLocked(id, always)
}

// publishSessionLocked is publishSession with sessionMu held. The time of
// the entry that left a session untouched is written as it was noted, so a
// write retried later does not move the session past later entries.
func (s *Server) publishSessionLocked(id int64, always bool) {
	ctx := context.Background()
	if at, ok := s.untouched[id]; ok {
		if err := s.touch(ctx, id, at); err != nil {
			s.log.Warn("touch session", "session", id, "err", err)
			s.retryLocked(id)
			return
		}
		delete(s.untouched, id)
	}
	if !always {
		waiting, err := s.store.HasPendingApproval(ctx, id)
		if err != nil {
			s.log.Warn("pending approvals", "session", id, "err", err)
			s.retryLocked(id)
			return
		}
		state := s.state(waiting, id)
		s.liveMu.Lock()
		same := s.states[id] == state
		s.liveMu.Unlock()
		if same {
			return
		}
	}
	session, err := s.session(ctx, id)
	if err != nil {
		s.log.Warn("read session", "session", id, "err", err)
		s.retryLocked(id)
		return
	}
	if r := s.retries[id]; r != nil {
		r.cancel()
		delete(s.retries, id)
	}
	s.liveMu.Lock()
	s.states[id] = session.State
	s.liveMu.Unlock()
	s.emit(proto.Event{Type: proto.EventSession, Session: &session})
}

// retryLocked schedules session id's event to be read again, unless one
// waits already or the server stopped. sessionMu is held. A retry whose
// timer fired while sessionMu was held may run after it was cancelled: it
// runs only if it is still the session's retry and the server runs, and
// keeps sessionMu from that check through the publication.
func (s *Server) retryLocked(id int64) {
	if s.stopped || s.retries[id] != nil {
		return
	}
	r := &pendingRetry{}
	s.retries[id] = r
	r.cancel = s.retry(func() {
		s.sessionMu.Lock()
		defer s.sessionMu.Unlock()
		if s.stopped || s.retries[id] != r {
			return
		}
		delete(s.retries, id)
		s.publishSessionLocked(id, true)
	})
}

func (s *Server) session(ctx context.Context, id int64) (proto.Session, error) {
	row, err := s.store.Session(ctx, id)
	if err != nil {
		return proto.Session{}, err
	}
	waiting, err := s.store.HasPendingApproval(ctx, id)
	if err != nil {
		return proto.Session{}, err
	}
	return s.wireSession(ctx, row, waiting)
}

// wireSession is a session's summary. A usage that cannot be read fails it:
// a summary with no usage would undo the one a client has.
func (s *Server) wireSession(ctx context.Context, row store.SessionRow, waiting bool) (proto.Session, error) {
	usage, err := s.engine.Usage(ctx, row.ID)
	if err != nil {
		return proto.Session{}, fmt.Errorf("usage of session %d: %w", row.ID, err)
	}
	return proto.Session{
		ID: row.ID, Title: row.Title, Cwd: row.Cwd, Model: proto.ModelRef{Provider: row.Provider, ID: row.Model},
		Thinking: row.Thinking, Profile: row.Profile, Route: row.Route, Standing: row.Standing, StartedBy: row.StartedBy,
		State: s.state(waiting, row.ID), Usage: usage, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// WatchAll starts watching every stored session; the service calls it once
// at start, before the engine resumes tasks.
func (s *Server) WatchAll(ctx context.Context) error {
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.engine.Watch(ctx, row.ID); err != nil {
			return fmt.Errorf("watch session %d: %w", row.ID, err)
		}
	}
	return nil
}

type deviceKey struct{}

func deviceOf(r *http.Request) store.Device { return r.Context().Value(deviceKey{}).(store.Device) }

// Handler serves every route of protocol version 6.
func (s *Server) Handler() http.Handler {
	// Each route is registered once, by method, and its path again without
	// one: a wrong method on a known path is 405, never 404, for every
	// route, the ones added later included (Codex review of #25, round 1).
	routes := []struct {
		method, path string
		handle       http.HandlerFunc
	}{
		{"GET", proto.PathHello, s.hello},
		{"GET", proto.PathModels, s.models},
		{"GET", proto.PathProfiles, s.profileList},
		{"GET", proto.PathSessions, s.sessions},
		{"POST", proto.PathSessions, s.createSession},
		{"GET", proto.PathSession, s.getSession},
		{"POST", proto.PathMessages, s.send},
		{"POST", proto.PathAbort, s.abort},
		{"POST", proto.PathStartOver, s.startOver},
		{"POST", proto.PathModel, s.changeModel},
		{"GET", proto.PathApprovals, s.pending},
		{"POST", proto.PathApproval, s.decide},
		{"GET", proto.PathThreads, s.threads},
		{"POST", proto.PathCloseThread, s.ownerCloseThread},
		{"POST", proto.PathStopShift, s.stopShift},
		{"POST", proto.PathResumeShift, s.resumeShift},
		{"GET", proto.PathAsks, s.asks},
		{"POST", proto.PathAsk, s.answer},
		{"GET", proto.PathLessons, s.lessons},
		{"POST", proto.PathLesson, s.decideLesson},
		{"GET", proto.PathPulls, s.listPulls},
		{"GET", proto.PathPull, s.readOnePull},
		{"GET", proto.PathPlans, s.plans},
		{"POST", proto.PathApprovePlan, s.approvePlan},
		{"GET", proto.PathSchedules, s.listSchedules},
		{"GET", proto.PathEvents, s.events},
		{"GET", proto.PathTerminals, s.terminalList},
		{"POST", proto.PathTerminals, s.startTerminal},
		{"DELETE", proto.PathTerminal, s.endTerminal},
		{"GET", proto.PathTerminalOutput, s.terminalOutput},
		{"POST", proto.PathTerminalInput, s.terminalInput},
		{"POST", proto.PathTerminalResize, s.resizeTerminal},
	}
	mux := http.NewServeMux()
	// Paths without a method, to tell a wrong method from an unknown path.
	paths := http.NewServeMux()
	known := map[string]bool{}
	for _, r := range routes {
		mux.HandleFunc(r.method+" "+r.path, r.handle)
		if !known[r.path] {
			known[r.path] = true
			paths.HandleFunc(r.path, func(http.ResponseWriter, *http.Request) {})
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := paths.Handler(r); pattern != "" {
			writeError(w, http.StatusMethodNotAllowed, proto.ErrorCodeBadRequest, r.Method+" is not allowed on "+r.URL.Path)
			return
		}
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no route "+r.Method+" "+r.URL.Path)
	})
	return s.guard(mux)
}

// guard checks the protocol version, then the device token (rule 1).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(proto.HeaderProtocol) != strconv.Itoa(proto.Version) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeProtocol,
				fmt.Sprintf("this service speaks protocol %d; send it in the %s header", proto.Version, proto.HeaderProtocol))
			return
		}
		device, err := auth.Authenticate(r.Context(), s.store, r.Header.Get("Authorization"))
		if errors.Is(err, auth.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, proto.ErrorCodeUnauthorized, "a valid device token is required")
			return
		} else if err != nil {
			s.internal(w, "authenticate", err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceKey{}, device)))
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code proto.ErrorCode, message string) {
	writeJSON(w, status, proto.Error{Code: code, Message: message})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, what+" failed")
}

// decode reads a strict JSON body (rule 3): one object, no unknown fields,
// nothing after it, at most maxBody bytes.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: "+err.Error())
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: data after the JSON object")
		return false
	}
	return true
}

// requestKey scopes a request id to the device that sent it (rule 4).
func requestKey(r *http.Request, requestID string) string {
	return "u:" + deviceOf(r).ID + ":" + requestID
}

func validRequestID(w http.ResponseWriter, id string) bool {
	if id == "" || len(id) > proto.MaxRequestIDSize {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest,
			fmt.Sprintf("requestId must be 1 to %d bytes", proto.MaxRequestIDSize))
		return false
	}
	return true
}

// keyLocks serializes the requests that carry one request key, on every
// endpoint (protocol rule 5): a request looks its key up, acts and records
// it with no other request of the key in between, so of two requests that
// share a key one takes effect and the other is refused or replayed.
type keyLocks struct {
	mu   sync.Mutex
	held map[string]*keyLock
}

type keyLock struct {
	mu    sync.Mutex
	users int
}

// lock holds key until the returned function runs.
func (k *keyLocks) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.held == nil {
		k.held = map[string]*keyLock{}
	}
	l := k.held[key]
	if l == nil {
		l = &keyLock{}
		k.held[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.held, key)
		}
		k.mu.Unlock()
	}
}

// reused answers a request id reused for another request.
func reused(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, proto.ErrorCodeConflict, store.ErrRequestReused.Error())
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) { return pathIDOf(w, r, "session") }

// pathIDOf is the path's {id}, the id of a what.
func pathIDOf(w http.ResponseWriter, r *http.Request, what string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the "+what+" id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.Hello{Protocol: proto.Version, Server: s.version, Device: deviceOf(r).Name, Now: s.now().UTC()})
}

// SetProfiles gives the server the agents' profiles (ADR-0013).
func (s *Server) SetProfiles(source ProfileSource) { s.profiles = source }

// profileList is every profile file, by id: a valid one with the model a
// session from it runs, or why none can; an invalid one with its problem.
func (s *Server) profileList(w http.ResponseWriter, r *http.Request) {
	out := []proto.Profile{}
	if s.profiles == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	valid, problems, err := s.profiles.Load()
	if err != nil {
		s.internal(w, "read profiles", err)
		return
	}
	models, err := s.engine.Models(r.Context())
	if err != nil {
		s.internal(w, "list models", err)
		return
	}
	for _, p := range valid {
		wire := proto.Profile{ID: p.ID, Name: p.Name, Role: proto.Role(p.Role), Model: p.Model, Home: p.Home, Cwd: p.Start(),
			Private: p.Private, Tools: p.Tools, Standing: p.Standing, CompactAt: p.CompactAt}
		// Each with its checkout: New Session works there (ADR-0027).
		for name, folder := range p.Repositories {
			wire.Repositories = append(wire.Repositories, proto.Repository{Name: name, Folder: folder})
		}
		slices.SortFunc(wire.Repositories, func(a, b proto.Repository) int { return strings.Compare(a.Name, b.Name) })
		if err := s.checkTools(p); err != nil {
			wire.Problem = err.Error()
		} else if err := profiles.CheckStandingFolder(p); err != nil {
			// A new standing session starts in its profile's folder, and
			// takes no other (rule 7c); one it has opens all the same.
			wire.Problem = err.Error()
		} else if choice, err := router.Choose(router.Request{Profile: &p}, models); err != nil {
			wire.Problem = err.Error()
		} else if err := profiles.CheckCompactAt(p, router.Window(models, choice.Model)); err != nil {
			// A standing session's threshold is below its model's window
			// (ADR-0026).
			wire.Problem = err.Error()
		} else {
			wire.Runs = choice.Model
		}
		out = append(out, wire)
	}
	for _, p := range problems {
		out = append(out, proto.Profile{ID: p.ID, Problem: p.Err})
	}
	slices.SortFunc(out, func(a, b proto.Profile) int { return strings.Compare(a.ID, b.ID) })
	writeJSON(w, http.StatusOK, out)
}

// checkTools refuses a profile no session can start from, model aside
// (profiles.Startable): a tool pocketd does not have, or a private
// profile whose home is not a folder.
func (s *Server) checkTools(p profiles.Profile) error {
	return profiles.Startable(p, s.engine.Tools())
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	// A model on an API key is offered to nothing the owner starts: the
	// engine lists none here (ADR-0021).
	models, err := s.engine.Models(r.Context())
	if err != nil {
		s.internal(w, "list models", err)
		return
	}
	if models == nil {
		models = []proto.Model{}
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Sessions(r.Context())
	if err != nil {
		s.internal(w, "list sessions", err)
		return
	}
	waiting, err := s.store.PendingSessions(r.Context())
	if err != nil {
		s.internal(w, "list approvals", err)
		return
	}
	out := make([]proto.Session, 0, len(rows))
	for _, row := range rows {
		session, err := s.wireSession(r.Context(), row, waiting[row.ID])
		if err != nil {
			s.internal(w, "read session", err)
			return
		}
		out = append(out, session)
	}
	writeJSON(w, http.StatusOK, out)
}

// validCreate checks a new CreateSession against rules 6 to 9 and returns
// the cleaned working directory. The thinking level must be one the model
// lists (rule 8): a provider would quietly run another.
// created is a valid CreateSession: what to create, and the route and
// profile to record with it.
type created struct {
	spec    SessionSpec
	profile string
	route   router.Rule
	// standing is a CreateSession that opens its profile's standing
	// session (rule 7c).
	standing bool
	// worktree is the new worktree the session works in (rule 6a), nil
	// for none: spec.Cwd is its path.
	worktree *sessionWorktree
}

func (s *Server) validCreate(w http.ResponseWriter, r *http.Request, request proto.CreateSession) (created, bool) {
	bad := func(message string) (created, bool) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, message)
		return created{}, false
	}
	var profile *profiles.Profile
	if request.Profile != "" {
		if s.profiles == nil {
			return bad("pocketd has no profiles")
		}
		p, err := s.profiles.Find(request.Profile)
		if err != nil {
			return bad(err.Error())
		}
		// A standing profile opens its session (rule 7c), which needs no
		// check of a new one's.
		if p.Standing {
			return validStanding(w, request, &p)
		}
		if err := s.checkTools(p); err != nil {
			return bad(err.Error())
		}
		profile = &p
	}
	if request.Worktree && request.Repository == "" {
		return bad("worktree needs a repository: a session's worktree is one of a repository's checkout")
	}
	cwd := request.Cwd
	switch {
	case request.Repository != "":
		// The checkout a profile names, never a folder of the client's
		// alone (rule 6a).
		var every []profiles.Profile
		if profile == nil {
			if s.profiles == nil {
				return bad("pocketd has no profiles, so no repository to work on")
			}
			valid, _, err := s.profiles.Load()
			if err != nil {
				s.internal(w, "read profiles", err)
				return created{}, false
			}
			every = valid
		}
		checkout, err := team.SessionCheckout(profile, every, request.Repository, request.Cwd)
		if err != nil && request.Worktree {
			// A retry of a request that made its worktree from a checkout
			// the profile no longer names: the request has its worktree,
			// and makes no other (Codex review of #46, round 2).
			claim, held, lookErr := s.store.WorktreeOf(r.Context(), requestKey(r, request.RequestID))
			if lookErr != nil {
				s.internal(w, "read the worktree's claim", lookErr)
				return created{}, false
			}
			if held {
				writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("%v: %s, on %s, from a checkout its profile no longer names there (%v): start a new session",
					store.ErrRequestHasWorktree, claim.Path, claim.Branch, err))
				return created{}, false
			}
		}
		if err != nil {
			return bad(err.Error())
		}
		cwd = checkout
	case cwd == "" && profile != nil:
		// A profile starts in its home, or else its workspace.
		if cwd = profile.Start(); cwd == "" {
			return bad("profile " + profile.ID + " has no home or workspace: name the cwd")
		}
	}
	if !filepath.IsAbs(cwd) {
		return bad("cwd must be an absolute path")
	}
	cwd = filepath.Clean(cwd)
	if !dirExists(cwd) {
		return bad("cwd must be an existing directory on the service host")
	}
	var made *sessionWorktree
	if request.Worktree {
		// Named from the request alone, as the client showed it.
		path, branch := team.SessionWorktree(profile, cwd, strings.TrimSpace(request.Title), request.RequestID)
		made = &sessionWorktree{checkout: cwd, path: path, branch: branch, target: requestTarget(request)}
		cwd = path
	}
	if profile != nil && profile.Private && cwd != profile.Home {
		return bad("profile " + profile.ID + " is private: it works only in its home, " + profile.Home)
	}
	if len(request.Title) > 200 {
		return bad("title must be at most 200 bytes")
	}
	models, err := s.engine.Models(r.Context())
	if err != nil {
		s.internal(w, "list models", err)
		return created{}, false
	}
	choice, err := router.Choose(router.Request{Owner: request.Model, Profile: profile}, models)
	if err != nil {
		return bad(err.Error())
	}
	for _, m := range models {
		if m.ModelRef == choice.Model && request.Thinking != "" && !slices.Contains(m.ThinkingLevels, request.Thinking) {
			return bad(fmt.Sprintf("thinking must be empty or one of the levels of %s: %s", m.String(), strings.Join(m.ThinkingLevels, ", ")))
		}
	}
	spec := SessionSpec{Title: strings.TrimSpace(request.Title), Cwd: cwd, Model: choice.Model, Thinking: request.Thinking,
		Profile: profile, Route: string(choice.Rule)}
	if profile != nil && profile.Role == profiles.RoleSupervisor {
		if spec.Roster, err = s.roster(models); err != nil {
			s.internal(w, "read profiles", err)
			return created{}, false
		}
	}
	if profile != nil {
		rows, err := s.store.ActiveLessons(r.Context(), profile.ID)
		if err != nil {
			s.internal(w, "read lessons", err)
			return created{}, false
		}
		spec.Lessons = sectionOf(rows)
	}
	return created{spec: spec, profile: request.Profile, route: choice.Rule, worktree: made}, true
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var request proto.CreateSession
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	key := requestKey(r, request.RequestID)
	// The key's lock keeps the tries of one request apart; createMu orders
	// the starts, but not a new worktree's git, which may take minutes
	// (makeSessionWorktree).
	defer s.keys.lock(key)()
	// A request id that made a worktree is that CreateSession's: the same
	// id without the worktree is another request, before any replay, which
	// would answer the worktree's session, and so is the same id with a
	// worktree after one that made none (Codex review of #46, rounds 4 to
	// 6): a CreateSession that makes a worktree binds it before its session
	// is made.
	claim, held, err := s.store.WorktreeOf(r.Context(), key)
	if err != nil {
		s.internal(w, "read the worktree's claim", err)
		return
	}
	// And the same id naming another target than the worktree's request:
	// compared as it was named, never by reading the profiles again, so a
	// true retry replays whatever changed there (round 7).
	if held && (!request.Worktree || claim.Target != requestTarget(request)) {
		reused(w)
		return
	}
	// A retry replays the first outcome before the current prerequisites are
	// checked again (rule 5): the directory or model may be gone by now.
	result, existed, err := s.store.LookupRequest(r.Context(), key, "createSession")
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case existed && result != "" && request.Worktree && !held:
		reused(w)
		return
	case existed && result != "":
		id, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			s.internal(w, "stored request", err)
			return
		}
		// The first call may have ended before its watch started.
		if err := s.engine.Watch(r.Context(), id); err != nil {
			s.internal(w, "watch session", err)
			return
		}
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	// A conversation made for this request whose record failed is recorded
	// as it was made: a new routing could pick another folder or model, or
	// refuse a profile removed since.
	made, exists, err := s.engine.Created(r.Context(), key)
	if err != nil {
		s.internal(w, "read created session", err)
		return
	}
	var valid created
	if exists && request.Worktree && !held {
		reused(w)
		return
	}
	if exists && made.Model != (proto.ModelRef{}) {
		title := made.Title
		if made.Legacy {
			if len(request.Title) > 200 {
				writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "title must be at most 200 bytes")
				return
			}
			title = strings.TrimSpace(request.Title)
		}
		valid = created{spec: SessionSpec{Title: title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route},
			profile: made.Profile, route: router.Rule(made.Route), standing: made.Standing}
		if made.Standing {
			valid.spec.StandingKey = standingKey(made.Profile)
		}
	} else {
		// A new worktree is made first, outside createMu: git may take
		// minutes, and no other start waits on it.
		var made *sessionWorktree
		if request.Worktree {
			first, ok := s.validCreate(w, r, request)
			if !ok {
				return
			}
			if first.worktree != nil {
				if !s.makeSessionWorktree(w, r, key, *first.worktree) {
					return
				}
				made = first.worktree
			}
		}
		if s.beforeCreateLock != nil {
			s.beforeCreateLock()
		}
		s.createMu.Lock()
		defer s.createMu.Unlock()
		place := ""
		if made != nil {
			if s.afterSessionWorktree != nil {
				s.afterSessionWorktree()
			}
			// The worktree as it is now first: its checks may take a while
			// (Codex review of #46, round 4).
			var ok bool
			if place, ok = s.usableSessionWorktree(w, r, key, *made); !ok {
				return
			}
		}
		// Every start is checked under createMu, from the profiles and
		// models as they are then, read last, right before it starts (Codex
		// review of #46, round 2), a worktree's after its git too, which
		// may have taken minutes, as a thread's specialist is
		// (recheckSpecialist; round 1).
		var ok bool
		if valid, ok = s.validCreate(w, r, request); !ok {
			return
		}
		if valid.standing {
			s.openStanding(w, r, key, valid)
			return
		}
		if made != nil && (valid.worktree == nil || *valid.worktree != *made) {
			// As a retry of it would be (ClaimWorktree).
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("%v: %s, on %s, from a checkout of %s its profile no longer names there: start a new session",
				store.ErrRequestHasWorktree, made.path, made.branch, request.Repository))
			return
		}
		if made != nil {
			// The agent works at the worktree's real place, never through a
			// link that may move (Codex review of #46, round 6).
			valid.spec.Cwd = place
		}
		s.startSession(w, r, key, valid)
		return
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	s.startSession(w, r, key, valid)
}

// startSession starts valid's session for request key, under
// createMu, and answers 201 with it.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, key string, valid created) {
	spec := valid.spec
	// Finish what starts here even if the client goes away. The engine's
	// create is idempotent by key and the work database records the session
	// with its request in one transaction, so a retry after any failure
	// finds the same conversation instead of creating another.
	ctx := context.WithoutCancel(r.Context())
	id, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		s.internal(w, "create session", err)
		return
	}
	if err := s.recordSession(ctx, key, id, spec, valid.profile, deviceOf(r).ID, valid.standing); err != nil {
		s.internal(w, "record session", err)
		return
	}
	s.writeSession(w, r, id, http.StatusCreated)
}

// recordSession records a session the engine created for key, publishes
// it and watches it. One creation event goes out before the watch reports
// the first live state; it is retried if it fails to read. The event and
// the watch come before the answer is read: a session the store has is
// followed even when its answer fails, and a retry of the request answers
// it. A standing one is recorded as its profile's standing session in the
// same transaction (ADR-0026).
func (s *Server) recordSession(ctx context.Context, key string, id int64, spec SessionSpec, profile, createdBy string, standing bool) error {
	title := spec.Title
	if title == "" {
		title = "Session " + strconv.FormatInt(id, 10)
	}
	row := store.SessionRow{
		ID: id, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider, Model: spec.Model.ID,
		Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: createdBy,
	}
	record := s.store.CreateSession
	if standing {
		record = s.store.CreateStandingSession
	}
	if _, err := record(ctx, key, row); err != nil {
		return err
	}
	s.publishSession(id, true)
	if profile != "" {
		s.lessonsSince(ctx, profile, spec.Lessons)
	}
	if err := s.engine.Watch(ctx, id); err != nil {
		return fmt.Errorf("watch session: %w", err)
	}
	return nil
}

func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, id int64, status int) {
	session, err := s.session(r.Context(), id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	writeJSON(w, status, session)
}

// knownSession writes 404 for a session the store does not have.
func (s *Server) knownSession(w http.ResponseWriter, r *http.Request, id int64) bool {
	if _, err := s.store.Session(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", id))
		return false
	} else if err != nil {
		s.internal(w, "read session", err)
		return false
	}
	return true
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !s.knownSession(w, r, id) {
		return
	}
	session, err := s.session(r.Context(), id)
	if err != nil {
		s.internal(w, "read session", err)
		return
	}
	entries, live, err := s.engine.Transcript(r.Context(), id)
	if err != nil {
		s.internal(w, "read transcript", err)
		return
	}
	if entries == nil {
		entries = []proto.Entry{}
	}
	writeJSON(w, http.StatusOK, proto.SessionDetail{Session: session, Entries: entries, Live: live})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.SendMessage
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if strings.TrimSpace(request.Text) == "" || len(request.Text) > proto.MaxMessageSize {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("text must be 1 to %d bytes and not only spaces", proto.MaxMessageSize))
		return
	}
	if !s.knownSession(w, r, id) {
		return
	}
	// A thread's specialist takes no message from the owner until its
	// brief is in its transcript: while the thread starts, and while an
	// open thread's brief waits in the queue, where an abort may withdraw
	// it and leave the owner's message first.
	// A thread's worker is its specialist's alone (ADR-0021); a look that
	// fails admits nothing (Codex review of #29, round 1).
	if thread, err := s.store.ThreadOfWorker(r.Context(), id); err == nil {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("session %d is thread T%d's worker: its specialist directs it", id, thread.ID))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "read the session's thread", err)
		return
	}
	if thread, err := s.store.ThreadOfSession(r.Context(), id); err == nil && thread.ReviewPull != 0 {
		// A cross-review's session is pocketd's alone: every run in it is
		// the review's, so stopping one never stops other work (ADR-0019;
		// Codex review of #26, round 5).
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d is a cross-review pocketd runs: its session takes no message", thread.ID))
		return
	} else if err == nil &&
		(thread.State == store.ThreadStarting || (thread.State == store.ThreadOpen && !thread.Briefed)) {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d's specialist does not have its brief yet: send once it has", thread.ID))
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "read thread", err)
		return
	}
	// Nor does a consult's session before its question is in its
	// transcript: the expert answers the question first (ADR-0020). One
	// whose question was dropped, as its consult ended, gets none.
	if waits, c, err := s.questionWaits(r.Context(), id); err != nil {
		s.internal(w, "read consult", err)
		return
	} else if waits {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("consult K%d's expert does not have its question yet: send once it has", c.ID))
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind := "send:" + strconv.FormatInt(id, 10)
	// The submission itself is idempotent by key in the Durable store. The
	// record, claimed before the submission and completed once it is
	// admitted, refuses the key for any other request; a failed send
	// leaves none.
	result, existed, err := s.store.LookupRequest(r.Context(), key, kind)
	if errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "read request", err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// A standing session takes what its profile says now before the
	// owner's message starts a run in it, or the message is refused
	// (ADR-0026); a retry of a message that went answers as it did, its
	// record lost by a stop of pocketd included.
	went, err := s.messageWent(ctx, id, key, existed, result)
	if err != nil {
		// Whether it went is unknown: never checked as a new message,
		// which it may not be (Codex review of #43, round 3).
		s.internal(w, "look up the message", err)
		return
	}
	if !went && !s.standingBeforeMessage(ctx, w, id) {
		// A claim an earlier try left, whose message PiG never took, is
		// freed (Codex review of #43, round 4).
		if existed {
			s.releaseSend(ctx, id, key, kind)
		}
		return
	}
	// The shift whose specialist's session it is, if any: its lock is held
	// from the claim until the message went, so a round's decision, a stop
	// or a resume goes before it or after. Another session takes no
	// shift's lock.
	shift, err := s.specialistShift(ctx, id)
	if err != nil {
		s.internal(w, "read the session's thread", err)
		return
	}
	if shift != 0 {
		defer s.lockShift(shift)()
	}
	// This request's own claim, which a failed release left for a delivery
	// pass to free, is this request's again: this try frees it if it fails
	// too, and puts it back if it cannot even claim it (Codex review of
	// #36).
	s.releaseMu.Lock()
	_, pending := s.unreleased[key]
	delete(s.unreleased, key)
	s.releaseMu.Unlock()
	// The request is claimed before the message goes, and its claim
	// pauses a running shift whose specialist's session it is (ADR-0024),
	// so a message PiG has never leaves its shift running, whatever stops
	// pocketd (Codex review of #36). One that does not go frees its key.
	existed, paused, err := s.claimSend(ctx, shift, key, kind)
	if err != nil && pending {
		// A claim still to free stays to free (Codex review of #36).
		s.releaseMu.Lock()
		s.unreleased[key] = kind
		s.releaseMu.Unlock()
		s.deliverLater()
	}
	if errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "claim request", err)
		return
	}
	// A claim that did nothing is freed. One that paused a shift says so
	// in the work database (store.RequestPaused), and one that went has
	// its result: neither is ever freed, whatever restarted between, so a
	// retry pauses no later round (Codex review of #36).
	_, _ = existed, paused
	release := func() { s.releaseSend(ctx, id, key, kind) }
	// The session is watched before the message goes, so its entry
	// reaches the event stream (watching a watched one does nothing).
	if err := s.engine.Watch(ctx, id); err != nil {
		release()
		s.internal(w, "watch session", err)
		return
	}
	submission, err := s.engine.Send(ctx, id, key, request.Text)
	if err != nil {
		release()
		s.internal(w, "send message", err)
		return
	}
	if err := s.store.CompleteRequest(ctx, key, strconv.FormatInt(submission, 10)); err != nil {
		s.internal(w, "record request", err)
		return
	}
	writeJSON(w, http.StatusOK, proto.SentMessage{SubmissionID: submission})
}

// releaseSend frees the claim of session id's message of request key,
// kind, once PiG says it has no submission of it: a message PiG has,
// admitted by this try or an earlier one whose record a stop of pocketd
// lost, went, and its claim stays (Codex review of #36). A lookup or a
// release that fails leaves it to a delivery pass (releasePending).
func (s *Server) releaseSend(ctx context.Context, id int64, key, kind string) {
	if where, err := s.engine.Placed(ctx, id, key); err != nil {
		s.log.Warn("look up a message that failed", "err", err)
		s.releaseLater(key, kind)
		return
	} else if where != PlacementAbsent {
		return
	}
	if err := s.releaseRequest(ctx, key, kind); err != nil {
		s.log.Warn("free a request id", "err", err)
		s.releaseLater(key, kind)
	}
}

// abortDone is the result of a settled abort.
const abortDone = "done"

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.Abort
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) || !s.knownSession(w, r, id) {
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind := "abort:" + strconv.FormatInt(id, 10)
	result, existed, err := s.store.LookupRequest(r.Context(), key, kind)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case result == abortDone:
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	ctx := context.WithoutCancel(r.Context())
	// The record names the run this abort targets, by its first entry,
	// before anything acts: a retry, or a retry after a restart, finishes
	// this abort and never stops a run started since.
	var target int64
	if existed {
		target, _ = strconv.ParseInt(strings.TrimPrefix(result, "run:"), 10, 64)
	} else {
		first, busy, err := s.engine.RunIdentity(ctx, id)
		if err != nil {
			s.internal(w, "read the run", err)
			return
		}
		if busy {
			target = first
		}
		if _, _, err := s.store.ClaimRequest(ctx, key, kind); err != nil {
			s.internal(w, "record request", err)
			return
		}
		if err := s.store.CompleteRequest(ctx, key, "run:"+strconv.FormatInt(target, 10)); err != nil {
			s.internal(w, "record request", err)
			return
		}
	}
	if current, busy, err := s.engine.RunIdentity(ctx, id); err != nil {
		s.internal(w, "read the run", err)
		return
	} else if busy && current == target {
		if err := s.engine.Abort(ctx, id); err != nil {
			if !existed {
				_ = s.store.DeleteRequest(ctx, key) // a failed abort leaves no record
			}
			s.internal(w, "abort", err)
			return
		}
	}
	settle, cancel := context.WithTimeout(ctx, abortSettle)
	defer cancel()
	if err := s.engine.WaitIdle(settle, id); err != nil {
		s.internal(w, "abort (the run did not settle; retry with the same requestId)", err)
		return
	}
	// The aborted run's calls will never run: their approvals are retired.
	// Only ended tasks count, so a run started meanwhile keeps its own.
	if err := s.approvals.RetireEnded(ctx, id, s.engine.TaskEnded); err != nil {
		s.internal(w, "retire approvals", err)
		return
	}
	if err := s.store.CompleteRequest(ctx, key, abortDone); err != nil {
		s.internal(w, "record request", err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) pending(w http.ResponseWriter, r *http.Request) {
	pending, err := s.approvals.Pending(r.Context())
	if err != nil {
		s.internal(w, "list approvals", err)
		return
	}
	if pending == nil {
		pending = []proto.Approval{}
	}
	writeJSON(w, http.StatusOK, pending)
}

// decideBody is Decide as it arrives: Allow is a pointer so that a missing
// or null decision is refused instead of read as a denial.
type decideBody struct {
	RequestID string `json:"requestId"`
	Allow     *bool  `json:"allow"`
	Reason    string `json:"reason"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var request decideBody
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if request.Allow == nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "allow must be true or false")
		return
	}
	if len(request.Reason) > 2000 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "reason must be at most 2000 bytes")
		return
	}
	id := r.PathValue("id")
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	approval, _, err := s.approvals.Decide(context.WithoutCancel(r.Context()), key, id, *request.Allow, deviceOf(r).Name, request.Reason)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no approval "+id)
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "approval "+id+" is already decided")
	case err != nil:
		s.internal(w, "decide approval", err)
	default:
		writeJSON(w, http.StatusOK, approval)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, "event stream", errors.New("the response writer cannot flush"))
		return
	}
	var last int64
	if header := r.Header.Get("Last-Event-ID"); header != "" {
		parsed, err := strconv.ParseInt(header, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "Last-Event-ID must be a non-negative integer")
			return
		}
		last = parsed
	}
	token := r.Header.Get("Authorization")
	controller := http.NewResponseController(w)
	// write sends one event or comment within writeTimeout, so a client
	// that stops reading cannot hold the stream.
	write := func(send func() error) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(writeTimeout))
		if send() != nil {
			return false
		}
		return controller.Flush() == nil
	}
	start, backlog, stream, cancel := s.hub.Subscribe(last)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Every stream starts with hello (rule 18): its seq is where the stream
	// starts, so a client that opened without Last-Event-ID learns it.
	backlog = append([]proto.Event{{Seq: start, Type: proto.EventHello}}, backlog...)
	for _, event := range backlog {
		select {
		case <-s.stop:
			return
		case <-r.Context().Done():
			return
		default:
		}
		if !write(func() error { return proto.WriteEvent(w, event) }) {
			return
		}
	}
	flusher.Flush()
	ticker := time.NewTicker(s.streamCheck)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			// A device revoked meanwhile, from any process, loses its stream.
			if _, err := auth.Authenticate(r.Context(), s.store, token); err != nil {
				return
			}
			if !write(func() error { _, err := io.WriteString(w, ": keepalive\n\n"); return err }) {
				return
			}
		case event, open := <-stream:
			if !open {
				return // fell behind: the client reconnects with Last-Event-ID
			}
			if !write(func() error { return proto.WriteEvent(w, event) }) {
				return
			}
		}
	}
}
