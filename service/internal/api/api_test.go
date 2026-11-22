package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/uploads"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

var model = proto.ModelRef{Provider: "faux", ID: "faux-1"}

// fakeEngine is an in-memory Engine.
type fakeEngine struct {
	// watchErr fails every Watch while set.
	watchErr error
	// entriesErr fails EntriesAfter while set.
	entriesErr error
	// readAfter are the cursors TranscriptAfter read from, in order.
	readAfter []int64
	// transcriptLive are the live states Transcript gives with a test's transcripts.
	transcriptLive map[int64]proto.Live
	// transcriptErr fails the next Transcript.
	transcriptErr error
	// transcripts are the sessions' transcripts a test set, by id.
	transcripts map[int64][]proto.Entry
	// keyModels are the models on an API key, which Models lists like any
	// other (ADR-0022).
	keyModels []proto.Model
	mu        sync.Mutex
	next      int64
	created   int
	sent      map[string]int64
	aborted   []int64
	watching  map[int64]bool
	onLive    func(proto.Live)
	runFirst  int64 // the current run's first entry; 0 when idle
	ended     map[int64]bool
	byKey     map[string]int64
	// specs are the sessions created, in order; more are models served
	// besides faux-1.
	specs []SessionSpec
	more  []proto.Model
	// made are the sessions created, by request key, as Created reads them.
	made     map[string]Created
	usage    proto.Usage
	usageErr error
	// texts and to are each sent message's text and session, by key, and
	// images its images (ADR-0029); sendErr fails every send.
	texts   map[string]string
	images  map[string][]Image
	to      map[string]int64
	sendErr error
	// failTo fails every send to these sessions; onSend sees each send,
	// and order lists the keys sent, in order. onGit sees each git call.
	failTo map[int64]bool
	onSend func(session int64, key string)
	order  []string
	onGit  func(args []string)
	// identities counts RunIdentity's calls.
	identities int
	// queued and withdrawn are the submissions Placed finds queued or
	// withdrawn; every other is placed. placedErr fails Placed.
	queued, withdrawn map[int64]bool
	placedErr         error
	// signedOut lists no model, as a sign-in that was removed; dirErr
	// fails Dir.
	signedOut bool
	// lessons is each session's lessons section, as SetLessons last set
	// it; lessonsErr fails SetLessons.
	lessons      map[int64]string
	lessonsErr   error
	onSetLessons func()
	// verdictTool are the sessions AddVerdictTool gave team_verdict;
	// scheduleTool those SetSupervisorTools gave team_schedule.
	verdictTool  map[int64]bool
	scheduleTool map[int64]bool
	// sendTool are the sessions SetSupervisorTools gave team_send.
	sendTool map[int64]bool
	// githubTool are the sessions SetGitHubTool gave team_github;
	// githubLate those that had been sent a message when it was called.
	githubTool map[int64]bool
	githubLate map[int64]bool
	// starters are the request keys of the submissions that started each
	// session's run, as RunStarter reads them (ADR-0048); starterErr
	// fails it.
	starters   map[int64]string
	starterErr error
	// questionTool are the sessions SetQuestionTool gave the question
	// tool; taskEndedFails fails that many TaskEnded calls.
	questionTool   map[int64]bool
	taskEndedFails int
	// onBusy sees each Busy, before it answers; busy are sessions at work
	// whatever runFirst says. onTranscriptAfter sees each TranscriptAfter,
	// before it reads.
	onBusy            func(session int64)
	busy              map[int64]bool
	onTranscriptAfter func(id int64)
	// idleSays, when set, is what Idle answers, whatever the session is:
	// a run that began right after the check.
	idleSays *bool
	// onGH answers gh's calls, ghCalls records them.
	onGH    func(ctx context.Context, args []string) (string, error)
	ghCalls []string
	// onCreate sees each session's spec before the engine makes it;
	// createErr fails the next CreateSession, which makes nothing.
	onCreate    func(spec SessionSpec)
	createErr   error
	dirErr      error
	onDir       func(ctx context.Context) error
	createdErr  error
	withdrawals []string
	lastCtx     context.Context
	onPlaced    func(key string)
	onWithdraw  func(key string)
	// onAbort sees each abort before it runs; abortErr fails the next one.
	onAbort       func(id int64)
	abortErr      error
	busyErr       error // fails the next Busy
	withdrawErr   error
	withdrawFails string
	// onWatch sees each Watch.
	onWatch func(id int64)
	// standing is what the standing sessions' calls did (ADR-0026).
	standing standingCalls
	// unseen are paths the sandbox shows no file at, whatever the host
	// has; readErr fails ReadUpTo, as a sandbox that did not start.
	unseen  map[string]bool
	readErr error
	// createLost makes the next CreateSession's session, then fails it,
	// as an answer lost to a stop. reads counts ReadUpTo's reads.
	createLost error
	reads      int
	// usageReads are UsageEntries' reads, in order, and usageReadErr
	// fails the next one (ADR-0052); onUsageRead sees each, before it
	// reads.
	usageReads   []usageRead
	usageReadErr error
	onUsageRead  func(id int64)
	// unmetered are the providers Metered says are subscriptions.
	unmetered map[string]bool
}

// usageRead is one read of UsageEntries: its session, the entry it read
// after and its since.
type usageRead struct {
	id, after int64
	since     time.Time
}

func (f *fakeEngine) Placed(_ context.Context, session int64, key string) (Placement, error) {
	if f.onPlaced != nil {
		f.onPlaced(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	submission, sent := f.sent[key]
	reset, resetSent := f.standing.resets[key]
	switch {
	case f.placedErr != nil:
		return 0, f.placedErr
	case resetSent && reset == session:
		// A Start Over's reset is placed as it is written (ADR-0026).
		return PlacementPlaced, nil
	case !sent || f.to[key] != session:
		return PlacementAbsent, nil
	case f.withdrawn[submission]:
		return PlacementWithdrawn, nil
	case f.queued[submission]:
		return PlacementQueued, nil
	}
	return PlacementPlaced, nil
}

// PlacedEntry is the entry of the transcript a test set that holds key's
// submission, once Placed finds it placed.
func (f *fakeEngine) PlacedEntry(ctx context.Context, session int64, key string) (int64, bool, error) {
	if where, err := f.Placed(ctx, session, key); err != nil || where != PlacementPlaced {
		return 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	submission := f.sent[key]
	for _, e := range f.transcripts[session] {
		if e.Kind == proto.EntryUser && e.SubmissionID == submission {
			return e.ID, true, nil
		}
	}
	return 0, false, nil
}

// GH answers gh as onGH says, or as GitHub says no pull request exists.
func (f *fakeEngine) GH(ctx context.Context, args ...string) (string, error) {
	f.mu.Lock()
	hook := f.onGH
	f.ghCalls = append(f.ghCalls, strings.Join(args, " "))
	f.mu.Unlock()
	if hook == nil {
		return "", errors.New("gh: HTTP 404: Not Found")
	}
	return hook(ctx, args)
}

// SetLessons records the lessons section each session's instructions end
// with, as the engine sets it.
func (f *fakeEngine) SetLessons(_ context.Context, session int64, section string) error {
	f.mu.Lock()
	hook := f.onSetLessons
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lessonsErr != nil {
		return f.lessonsErr
	}
	if f.lessons == nil {
		f.lessons = map[int64]string{}
	}
	f.lessons[session] = section
	return nil
}

// Withdraw withdraws a queued submission of key, as PiG does: one placed
// is left alone.
func (f *fakeEngine) Withdraw(_ context.Context, session int64, key string) (bool, error) {
	if f.onWithdraw != nil {
		f.onWithdraw(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withdrawErr != nil || (f.withdrawFails != "" && f.withdrawFails == key) {
		return false, errors.Join(f.withdrawErr, errors.New("cannot withdraw "+key))
	}
	f.withdrawals = append(f.withdrawals, key)
	submission, sent := f.sent[key]
	if !sent || f.to[key] != session || !f.queued[submission] {
		return false, nil
	}
	delete(f.queued, submission)
	if f.withdrawn == nil {
		f.withdrawn = map[int64]bool{}
	}
	f.withdrawn[submission] = true
	return true, nil
}

func (f *fakeEngine) Models(context.Context) ([]proto.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signedOut {
		return nil, nil
	}
	list := append([]proto.Model{{ModelRef: model, Name: "Faux", Reasoning: true, ThinkingLevels: []string{"off", "minimal", "low", "medium", "high"}}},
		f.more...)
	return append(list, f.keyModels...), nil
}

func (f *fakeEngine) CreateSession(_ context.Context, key string, spec SessionSpec) (int64, error) {
	f.mu.Lock()
	hook := f.onCreate
	f.mu.Unlock()
	if hook != nil {
		hook(spec)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createErr; err != nil {
		f.createErr = nil
		return 0, err
	}
	if id, ok := f.byKey[key]; ok {
		return id, nil
	}
	if f.made == nil {
		f.made = map[string]Created{}
	}
	// A standing session made under its profile's key already is the one
	// any request gets, recorded under the request's key too (ADR-0026).
	if id, ok := f.byKey[spec.StandingKey]; ok && spec.StandingKey != "" {
		f.byKey[key], f.made[key] = id, f.made[spec.StandingKey]
		return id, nil
	}
	f.next++
	f.created++
	f.byKey[key] = f.next
	f.specs = append(f.specs, spec)
	made := Created{ID: f.next, Title: spec.Title, Cwd: spec.Cwd, Model: spec.Model, Thinking: spec.Thinking, Route: spec.Route,
		Standing: spec.StandingKey != ""}
	if spec.Profile != nil {
		made.Profile = spec.Profile.ID
	}
	f.made[key] = made
	if spec.StandingKey != "" {
		f.byKey[spec.StandingKey], f.made[spec.StandingKey] = f.next, made
	}
	if err := f.createLost; err != nil {
		f.createLost = nil
		return 0, err
	}
	return f.next, nil
}

func (f *fakeEngine) Created(_ context.Context, key string) (Created, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createdErr != nil {
		return Created{}, false, f.createdErr
	}
	made, ok := f.made[key]
	return made, ok, nil
}

func (f *fakeEngine) Tools() []string { return []string{"read", "write", "edit", "bash"} }

// Watch reports the initial live state, as the real engine does.
func (f *fakeEngine) Watch(_ context.Context, id int64) error {
	if f.onWatch != nil {
		f.onWatch(id)
	}
	f.mu.Lock()
	if f.watchErr != nil {
		err := f.watchErr
		f.mu.Unlock()
		return err
	}
	already := f.watching[id]
	f.watching[id] = true
	onLive := f.onLive
	f.mu.Unlock()
	if !already && onLive != nil {
		onLive(proto.Live{SessionID: id})
	}
	return nil
}

func (f *fakeEngine) Busy(_ context.Context, session int64) (bool, error) {
	f.mu.Lock()
	onBusy := f.onBusy
	f.mu.Unlock()
	if onBusy != nil {
		onBusy(session)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busyErr != nil {
		err := f.busyErr
		f.busyErr = nil
		return false, err
	}
	return f.runFirst != 0 || f.busy[session], nil
}

// Idle is no run, and no submission of session's still queued.
func (f *fakeEngine) Idle(ctx context.Context, session int64) (bool, error) {
	f.mu.Lock()
	says := f.idleSays
	f.mu.Unlock()
	if says != nil {
		return *says, nil
	}
	if busy, err := f.Busy(ctx, session); err != nil || busy {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, sub := range f.sent {
		if f.queued[sub] && f.to[key] == session {
			return false, nil
		}
	}
	return true, nil
}

func (f *fakeEngine) RunIdentity(context.Context, int64) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identities++
	return f.runFirst, f.runFirst != 0, nil
}

// RunStarter answers starters' key for a session at work: a session with
// a key there has a run, which that key started.
func (f *fakeEngine) RunStarter(_ context.Context, id int64) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starterErr != nil {
		return "", false, f.starterErr
	}
	key, busy := f.starters[id]
	return key, busy, nil
}

func (f *fakeEngine) WaitIdle(context.Context, int64) error { return nil }

// SetVerdictTool records the sessions given team_verdict, and those it
// was taken from (verdictTool false), each change once.
func (f *fakeEngine) SetVerdictTool(_ context.Context, id int64, on bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verdictTool == nil {
		f.verdictTool = map[int64]bool{}
	}
	had, known := f.verdictTool[id]
	if known && had == on {
		return false, nil
	}
	f.verdictTool[id] = on
	return on || had, nil
}

// SetSupervisorTools records the sessions given the later supervisor
// tools, team_schedule and team_send among them, each once.
func (f *fakeEngine) SetSupervisorTools(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scheduleTool == nil {
		f.scheduleTool, f.sendTool = map[int64]bool{}, map[int64]bool{}
	}
	if f.scheduleTool[id] {
		return false, nil
	}
	f.scheduleTool[id], f.sendTool[id] = true, true
	return true, nil
}

// SetGitHubTool records the sessions given team_github, each once.
func (f *fakeEngine) SetGitHubTool(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.githubTool == nil {
		f.githubTool, f.githubLate = map[int64]bool{}, map[int64]bool{}
	}
	for _, to := range f.to {
		if to == id {
			f.githubLate[id] = true
		}
	}
	if f.githubTool[id] {
		return false, nil
	}
	f.githubTool[id] = true
	return true, nil
}

// SetQuestionTool records the sessions given the question tool, each once.
func (f *fakeEngine) SetQuestionTool(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.questionTool == nil {
		f.questionTool = map[int64]bool{}
	}
	if f.questionTool[id] {
		return false, nil
	}
	f.questionTool[id] = true
	return true, nil
}

func (f *fakeEngine) TaskEnded(_ context.Context, task int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taskEndedFails > 0 {
		f.taskEndedFails--
		return false, errors.New("the durable store is busy")
	}
	return f.ended[task], nil
}

// SendIdle is Send refused while the session has a run, as PiG's
// WhenBusyReject is.
// A test's onSend sees it once admitted: one that starts a run there
// leaves the message queued, as a message admitted before this check
// existed may be.
func (f *fakeEngine) SendIdle(ctx context.Context, session int64, key, text string) (int64, error) {
	f.mu.Lock()
	busy := f.runFirst != 0 || f.busy[session]
	_, sent := f.sent[key]
	f.mu.Unlock()
	if busy && !sent {
		return 0, ErrSessionBusy
	}
	if f.onSend != nil {
		f.onSend(session, key)
	}
	return f.send(ctx, session, key, text)
}

func (f *fakeEngine) Send(ctx context.Context, session int64, key, text string, attached ...Attached) (int64, error) {
	if f.onSend != nil {
		f.onSend(session, key)
	}
	// The session reads the files' lines after the words, as PiG's
	// entry gives them back.
	var images []Image
	for _, a := range attached {
		if a.Lines != "" {
			text = proto.WithAttachmentLines(text, a.Lines)
		}
		images = append(images, a.Images...)
	}
	sub, err := f.send(ctx, session, key, text)
	if err == nil && len(images) > 0 {
		f.mu.Lock()
		if f.images == nil {
			f.images = map[string][]Image{}
		}
		if _, ok := f.images[key]; !ok {
			f.images[key] = images
		}
		f.mu.Unlock()
	}
	return sub, err
}

func (f *fakeEngine) send(ctx context.Context, session int64, key, text string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCtx = ctx
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	if f.failTo[session] {
		return 0, fmt.Errorf("session %d takes no message", session)
	}
	if id, ok := f.sent[key]; ok {
		return id, nil
	}
	f.sent[key] = int64(len(f.sent) + 100)
	f.order = append(f.order, key)
	if f.texts == nil {
		f.texts, f.to = map[string]string{}, map[string]int64{}
	}
	f.texts[key], f.to[key] = text, session
	return f.sent[key], nil
}

// Read reads outside any sandbox, as only a test does.
func (f *fakeEngine) Read(_ context.Context, path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, nil
	}
	data, err := os.ReadFile(path)
	return string(data), err == nil, err
}

// ReadUpTo reads outside any sandbox, as only a test does, but the paths
// a test says the sandbox does not show; it opens without waiting for a
// writer, as the sandbox's test -f does.
func (f *fakeEngine) ReadUpTo(_ context.Context, path string, max int) ([]byte, bool, error) {
	f.mu.Lock()
	unseen, err := f.unseen[path], f.readErr
	f.reads++
	f.mu.Unlock()
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(path)
	if unseen || err != nil || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(max)+1))
	return data, err == nil, err
}

// Kind looks outside any sandbox, as only a test does.
func (f *fakeEngine) Kind(_ context.Context, path string) (string, error) {
	return hostKind(path)
}

// Same compares outside any sandbox, as only a test does.
func (f *fakeEngine) Same(_ context.Context, a, b string) (bool, error) {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib), nil
}

// Real resolves a folder outside any sandbox, as only a test does.
func (f *fakeEngine) Real(_ context.Context, path string) (string, bool, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, nil
	}
	info, err := os.Stat(real)
	return real, err == nil && info.IsDir(), nil
}

// MakeDir makes a folder outside any sandbox, as only a test does.
func (f *fakeEngine) MakeDir(_ context.Context, path string) (bool, error) {
	err := os.Mkdir(path, 0o700)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

// Dir looks outside any sandbox, as only a test does.
func (f *fakeEngine) Dir(ctx context.Context, path string) (bool, error) {
	f.mu.Lock()
	failed, hook := f.dirErr, f.onDir
	f.mu.Unlock()
	if failed != nil {
		return false, failed
	}
	if hook != nil {
		if err := hook(ctx); err != nil {
			return false, err
		}
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir(), nil
}

// hostKind is Kind outside any sandbox.
func hostKind(path string) (string, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", err
	case info.Mode()&os.ModeSymlink != 0:
		return "link", nil
	case info.Mode().IsRegular():
		return "file", nil
	case info.IsDir():
		return "dir", nil
	}
	return "other", nil
}

// Git runs git outside any sandbox, as only a test does, without the
// developer's git configuration.
func (f *fakeEngine) Git(ctx context.Context, dir string, args ...string) (string, error) {
	if f.onGit != nil {
		f.onGit(args)
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		err = &worktree.GitExit{Status: exit.ExitCode()}
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// GitEach runs git as Git does, and splits its whole output.
func (f *fakeEngine) GitEach(ctx context.Context, dir string, sep byte, each func(string) error, args ...string) error {
	out, err := f.Git(ctx, dir, args...)
	if err != nil {
		return err
	}
	for _, record := range strings.Split(out, string(sep)) {
		if record == "" {
			continue
		}
		if err := each(record); err != nil {
			return err
		}
	}
	return nil
}

// GitPage runs git as Git does, and pages its output.
func (f *fakeEngine) GitPage(ctx context.Context, dir string, offset, limit int, args ...string) (string, bool, error) {
	out, err := f.Git(ctx, dir, args...)
	if err != nil {
		return "", false, err
	}
	if out != "" {
		out += "\n"
	}
	if offset >= len(out) {
		return "", false, nil
	}
	end := min(offset+limit, len(out))
	return out[offset:end], end < len(out), nil
}

func (f *fakeEngine) Abort(_ context.Context, id int64) error {
	if f.onAbort != nil {
		f.onAbort(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.abortErr != nil {
		err := f.abortErr
		f.abortErr = nil
		return err
	}
	f.aborted = append(f.aborted, id)
	f.runFirst = 0
	return nil
}

// TranscriptAfter is Transcript's entries after entry after.
func (f *fakeEngine) TranscriptAfter(ctx context.Context, id, after int64) ([]proto.Entry, proto.Live, error) {
	f.mu.Lock()
	hook := f.onTranscriptAfter
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	entries, live, err := f.Transcript(ctx, id)
	f.mu.Lock()
	f.readAfter = append(f.readAfter, after)
	f.mu.Unlock()
	var out []proto.Entry
	for _, e := range entries {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, live, err
}

// EntriesAfter is at most most of the transcript's entries after entry
// after, as the search index reads them (ADR-0050); entriesErr fails it.
func (f *fakeEngine) EntriesAfter(ctx context.Context, id, after int64, most int) ([]proto.Entry, error) {
	f.mu.Lock()
	failed := f.entriesErr
	f.mu.Unlock()
	if failed != nil {
		return nil, failed
	}
	entries, _, err := f.Transcript(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []proto.Entry
	for _, e := range entries {
		if e.ID > after && len(out) < most {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeEngine) Transcript(_ context.Context, id int64) ([]proto.Entry, proto.Live, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transcriptErr != nil {
		err := f.transcriptErr
		f.transcriptErr = nil
		return nil, proto.Live{}, err
	}
	if entries, ok := f.transcripts[id]; ok {
		live := f.transcriptLive[id]
		live.SessionID = id
		return entries, live, nil
	}
	return []proto.Entry{{ID: 1, SessionID: id, Kind: proto.EntryUser, Text: "hi"}}, proto.Live{SessionID: id}, nil
}

func (f *fakeEngine) Usage(context.Context, int64) (proto.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usage, f.usageErr
}

// Metered says a provider is metered unless a test made it a
// subscription's.
func (f *fakeEngine) Metered(provider string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unmetered[provider]
}

// UsageEntries reads a test's transcript as PiG's does: its answers with
// usage after entry after, newest first, down to the first one before
// since.
func (f *fakeEngine) UsageEntries(_ context.Context, id, after int64, since time.Time) ([]UsageEntry, int64, error) {
	if f.onUsageRead != nil {
		f.onUsageRead(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageReads = append(f.usageReads, usageRead{id: id, after: after, since: since})
	if err := f.usageReadErr; err != nil {
		f.usageReadErr = nil
		return nil, 0, err
	}
	entries := f.transcripts[id]
	var newest int64
	for _, e := range entries {
		newest = max(newest, e.ID)
	}
	var out []UsageEntry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.ID <= after || e.Kind != proto.EntryAssistant || e.Usage == nil {
			continue
		}
		if e.At.Before(since) {
			break
		}
		u := UsageEntry{ID: e.ID, At: e.At, Usage: *e.Usage}
		if e.Model != nil {
			u.Model = *e.Model
		}
		out = append(out, u)
	}
	return out, newest, nil
}

// clock is the work store's clock in a test; it moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t      *testing.T
	clock  *clock
	hub    *events.Hub
	server *Server
	engine *fakeEngine
	store  *store.Store
	http   *http.Client
	client *proto.Client
	token  string
	// device is the harness device's id: a request id's key is scoped to
	// it (requestKey).
	device string
	work   string
	// dbPath is the work database's file: a test opens it apart to see
	// that a read wrote nothing (ADR-0046).
	dbPath string
	// scheduled are the retries the server scheduled (Server.retry), run
	// only when a test runs them: no timer outlives a test.
	retryMu   sync.Mutex
	scheduled []func()
	// shiftPasses are the shift passes the server started, and shiftWakes
	// its shifts' wakes; each waits for the test (runShiftPasses,
	// fireShiftWakes). shiftMu guards both.
	shiftMu     sync.Mutex
	shiftPasses []func()
	shiftWakes  []shiftWake
	// schedulePasses are the schedules passes the server started, and
	// scheduleWakes their wakes; each waits for the test
	// (runSchedulePasses, fireScheduleWakes). shiftMu guards both.
	schedulePasses []func()
	scheduleWakes  []shiftWake
	// gatherWakes are the gathers' wakes (armGathers); each waits for the
	// test (fireGatherWakes). shiftMu guards them.
	gatherWakes []shiftWake
	// launchPasses are the launch passes the server started, and
	// launchWakes their wakes; each waits for the test (runLaunchPasses,
	// fireLaunchWakes). shiftMu guards both.
	launchPasses []func()
	launchWakes  []shiftWake
}

// runLaunchPasses runs the launch passes the server started, and those
// they start, and says how many ran.
func (h *harness) runLaunchPasses() int {
	n := 0
	for {
		h.shiftMu.Lock()
		if len(h.launchPasses) == 0 {
			h.shiftMu.Unlock()
			return n
		}
		f := h.launchPasses[0]
		h.launchPasses = h.launchPasses[1:]
		h.shiftMu.Unlock()
		f()
		n++
	}
}

// fireLaunchWakes runs the launches' wakes due by the clock, but those the
// server cancelled, and the passes they start.
func (h *harness) fireLaunchWakes() int {
	h.shiftMu.Lock()
	var due []func()
	kept := h.launchWakes[:0]
	for _, w := range h.launchWakes {
		if *w.cancelled {
			continue
		}
		if !w.at.After(h.clock.Now()) {
			due = append(due, w.f)
		} else {
			kept = append(kept, w)
		}
	}
	h.launchWakes = kept
	h.shiftMu.Unlock()
	for _, f := range due {
		f()
	}
	h.runLaunchPasses()
	return len(due)
}

// fireGatherWakes runs the gathers' wakes due by the clock, but those the
// server cancelled, and says how many ran.
func (h *harness) fireGatherWakes() int {
	h.shiftMu.Lock()
	var due []func()
	kept := h.gatherWakes[:0]
	for _, w := range h.gatherWakes {
		if *w.cancelled {
			continue
		}
		if !w.at.After(h.clock.Now()) {
			due = append(due, w.f)
		} else {
			kept = append(kept, w)
		}
	}
	h.gatherWakes = kept
	h.shiftMu.Unlock()
	for _, f := range due {
		f()
	}
	return len(due)
}

// runSchedulePasses runs the schedules passes the server started, and
// those they start, and says how many ran.
func (h *harness) runSchedulePasses() int {
	n := 0
	for {
		h.shiftMu.Lock()
		if len(h.schedulePasses) == 0 {
			h.shiftMu.Unlock()
			return n
		}
		f := h.schedulePasses[0]
		h.schedulePasses = h.schedulePasses[1:]
		h.shiftMu.Unlock()
		f()
		n++
	}
}

// fireScheduleWakes runs the schedules' wakes due by the clock, but those
// the server cancelled, and the passes they start.
func (h *harness) fireScheduleWakes() {
	h.shiftMu.Lock()
	var due []func()
	kept := h.scheduleWakes[:0]
	for _, w := range h.scheduleWakes {
		if *w.cancelled {
			continue
		}
		if !w.at.After(h.clock.Now()) {
			due = append(due, w.f)
		} else {
			kept = append(kept, w)
		}
	}
	h.scheduleWakes = kept
	h.shiftMu.Unlock()
	for _, f := range due {
		f()
	}
	h.runSchedulePasses()
}

// nextScheduleWake is when the schedules' wake that is not cancelled is
// due; zero for none.
func (h *harness) nextScheduleWake() time.Time {
	h.shiftMu.Lock()
	defer h.shiftMu.Unlock()
	var next time.Time
	for _, w := range h.scheduleWakes {
		if !*w.cancelled && (next.IsZero() || w.at.Before(next)) {
			next = w.at
		}
	}
	return next
}

// shiftWake is a shift's wake the server set, due at at; cancelled says
// the server cancelled it.
type shiftWake struct {
	at        time.Time
	f         func()
	cancelled *bool
}

// runShiftPasses runs the shift passes the server started, and those they
// start, and says how many ran.
func (h *harness) runShiftPasses() int {
	n := 0
	for {
		h.shiftMu.Lock()
		if len(h.shiftPasses) == 0 {
			h.shiftMu.Unlock()
			return n
		}
		f := h.shiftPasses[0]
		h.shiftPasses = h.shiftPasses[1:]
		h.shiftMu.Unlock()
		f()
		n++
	}
}

// fireShiftWakes runs the shifts' wakes due by the clock, but those the
// server cancelled.
func (h *harness) fireShiftWakes() {
	h.shiftMu.Lock()
	var due []func()
	kept := h.shiftWakes[:0]
	for _, w := range h.shiftWakes {
		if *w.cancelled {
			continue
		}
		if !w.at.After(h.clock.Now()) {
			due = append(due, w.f)
		} else {
			kept = append(kept, w)
		}
	}
	h.shiftWakes = kept
	h.shiftMu.Unlock()
	for _, f := range due {
		f()
	}
}

// runScheduled runs the retries scheduled so far, and those they schedule,
// up to limit runs.
func (h *harness) runScheduled(limit int) {
	for i := 0; i < limit; i++ {
		h.retryMu.Lock()
		if len(h.scheduled) == 0 {
			h.retryMu.Unlock()
			return
		}
		f := h.scheduled[0]
		h.scheduled = h.scheduled[1:]
		h.retryMu.Unlock()
		f()
	}
}

const base = "http://pocketd.test"

// memTransport serves requests with a handler in memory, streaming the
// response body through a pipe, so the tests open no socket and SSE works.
type memTransport struct{ handler http.Handler }

func (m memTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, write := io.Pipe()
	response := &pipeResponse{header: http.Header{}, body: write, ready: make(chan struct{})}
	go func() {
		m.handler.ServeHTTP(response, r)
		response.writeHeader(http.StatusOK)
		_ = write.Close()
	}()
	select {
	case <-response.ready:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: response.status, Header: response.header, Body: body, Request: r, ProtoMajor: 1, ProtoMinor: 1}, nil
}

type pipeResponse struct {
	header http.Header
	status int
	body   *io.PipeWriter
	once   sync.Once
	ready  chan struct{}
}

func (p *pipeResponse) Header() http.Header  { return p.header }
func (p *pipeResponse) WriteHeader(code int) { p.writeHeader(code) }
func (p *pipeResponse) writeHeader(code int) {
	p.once.Do(func() { p.status = code; close(p.ready) })
}
func (p *pipeResponse) Write(b []byte) (int, error) {
	p.writeHeader(http.StatusOK)
	return p.body.Write(b)
}
func (p *pipeResponse) Flush() {}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	now := &clock{now: time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)}
	dbPath := filepath.Join(t.TempDir(), "w.sqlite")
	s, err := store.Open(ctx, dbPath, now.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	device, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(64, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), func(floor, block int64) (int64, error) { return s.ReserveSeq(context.Background(), floor, block) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := New(s, hub, "test", now.Now, nil)
	server.SetApprovals(approvals.New(s, server.PublishApproval))
	engine := &fakeEngine{sent: map[string]int64{}, byKey: map[string]int64{}, watching: map[int64]bool{}, ended: map[int64]bool{}, onLive: server.PublishLive,
		usage: proto.Usage{Output: 3}}
	server.SetEngine(engine)
	// The owner's uploads go in a home of the test's own (ADR-0029).
	server.SetUploads(uploads.New(uploads.Dir(t.TempDir()), filepath.Join(t.TempDir(), "upload-staging"), now.Now))
	h := &harness{t: t, clock: now, hub: hub, server: server, engine: engine, store: s, token: token, device: device.ID, work: t.TempDir(), dbPath: dbPath}
	// A retry waits for the test, never for a timer, and the server stops
	// before its store closes (cleanups run last first).
	server.retry = func(f func()) func() {
		h.retryMu.Lock()
		defer h.retryMu.Unlock()
		h.scheduled = append(h.scheduled, f)
		return func() {}
	}
	// A shift pass, and a shift's wake, wait for the test, never for a
	// goroutine or a timer; a pass left at the end runs before the server
	// stops, which waits for it.
	server.goShift = func(f func()) {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		h.shiftPasses = append(h.shiftPasses, f)
	}
	server.shiftTimer = func(d time.Duration, f func()) func() {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		cancelled := new(bool)
		h.shiftWakes = append(h.shiftWakes, shiftWake{at: now.Now().Add(d), f: f, cancelled: cancelled})
		return func() {
			h.shiftMu.Lock()
			defer h.shiftMu.Unlock()
			*cancelled = true
		}
	}
	// A schedules pass, and its wake, wait for the test too (ADR-0030).
	server.goSchedule = func(f func()) {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		h.schedulePasses = append(h.schedulePasses, f)
	}
	server.scheduleTimer = func(d time.Duration, f func()) func() {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		cancelled := new(bool)
		h.scheduleWakes = append(h.scheduleWakes, shiftWake{at: now.Now().Add(d), f: f, cancelled: cancelled})
		return func() {
			h.shiftMu.Lock()
			defer h.shiftMu.Unlock()
			*cancelled = true
		}
	}
	// A launch pass, and its wake, wait for the test too (ADR-0034).
	server.goLaunch = func(f func()) {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		h.launchPasses = append(h.launchPasses, f)
	}
	server.launchTimer = func(d time.Duration, f func()) func() {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		cancelled := new(bool)
		h.launchWakes = append(h.launchWakes, shiftWake{at: now.Now().Add(d), f: f, cancelled: cancelled})
		return func() {
			h.shiftMu.Lock()
			defer h.shiftMu.Unlock()
			*cancelled = true
		}
	}
	// A gather's wake waits for the test too (ADR-0033).
	server.gatherTimer = func(d time.Duration, f func()) func() {
		h.shiftMu.Lock()
		defer h.shiftMu.Unlock()
		cancelled := new(bool)
		h.gatherWakes = append(h.gatherWakes, shiftWake{at: now.Now().Add(d), f: f, cancelled: cancelled})
		return func() {
			h.shiftMu.Lock()
			defer h.shiftMu.Unlock()
			*cancelled = true
		}
	}
	t.Cleanup(server.Stop)
	t.Cleanup(func() { h.runShiftPasses(); h.runSchedulePasses(); h.runLaunchPasses() })
	h.http = &http.Client{Transport: memTransport{server.Handler()}}
	h.client, _ = proto.NewClient(base, token, h.http)
	return h
}

// raw sends a request with full control of headers and body.
func (h *harness) raw(method, path, body string, headers map[string]string) (*http.Response, proto.Error) {
	h.t.Helper()
	request, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response, err := h.http.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var e proto.Error
	_ = json.NewDecoder(response.Body).Decode(&e)
	return response, e
}

func asError(t *testing.T, err error, status int, code proto.ErrorCode) {
	t.Helper()
	var e *proto.Error
	if !errors.As(err, &e) || e.Status != status || e.Code != code {
		t.Fatalf("error = %v (want %d %s)", err, status, code)
	}
}

func TestProtocolHeaderAndTokenAreRequired(t *testing.T) {
	h := newHarness(t)
	response, e := h.raw("GET", proto.PathHello, "", map[string]string{"Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("no protocol header: %d %+v", response.StatusCode, e)
	}
	// Version 1 had no hello and none of what came with it: refused.
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: "1", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("protocol 1: %d %+v", response.StatusCode, e)
	}
	// Version 7's desktop merged on a head alone, whatever base branch a
	// verdict was filed on: refused (Codex review of #26, round 6).
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: "7", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("protocol 7: %d %+v", response.StatusCode, e)
	}
	// Version 12's desktop has no terminal (ADR-0025): refused.
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: "12", "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("protocol 12: %d %+v", response.StatusCode, e)
	}
	response, e = h.raw("GET", proto.PathHello, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + auth.NewToken()})
	if response.StatusCode != http.StatusUnauthorized || e.Code != proto.ErrorCodeUnauthorized {
		t.Fatalf("unknown token: %d %+v", response.StatusCode, e)
	}
	hello, err := h.client.Hello(context.Background())
	if err != nil || hello.Protocol != proto.Version || hello.Device != "laptop" || hello.Server != "test" {
		t.Fatalf("hello %+v, %v", hello, err)
	}
}

func TestCreateSessionValidates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	good := proto.CreateSession{RequestID: "r1", Cwd: h.work, Model: model}
	cases := map[string]func(*proto.CreateSession){
		"no request id":    func(c *proto.CreateSession) { c.RequestID = "" },
		"long request id":  func(c *proto.CreateSession) { c.RequestID = strings.Repeat("x", proto.MaxRequestIDSize+1) },
		"relative cwd":     func(c *proto.CreateSession) { c.Cwd = "work" },
		"missing cwd":      func(c *proto.CreateSession) { c.Cwd = filepath.Join(h.work, "nope") },
		"unknown model":    func(c *proto.CreateSession) { c.Model = proto.ModelRef{Provider: "x", ID: "y"} },
		"unknown thinking": func(c *proto.CreateSession) { c.Thinking = "maximum" },
		// faux-1 lists off to high: a level it does not list is refused,
		// not left for the provider to change quietly (rule 8).
		"thinking the model lacks": func(c *proto.CreateSession) { c.Thinking = "xhigh" },
	}
	for name, mutate := range cases {
		request := good
		mutate(&request)
		_, err := h.client.CreateSession(ctx, request)
		var e *proto.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	response, e := h.raw("POST", proto.PathSessions, `{"requestId":"r","cwd":"`+h.work+`","model":{"provider":"faux","id":"faux-1"},"extra":1}`,
		map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, "unknown field") {
		t.Fatalf("unknown field: %d %+v", response.StatusCode, e)
	}
	if h.engine.created != 0 {
		t.Fatalf("an invalid request created %d sessions", h.engine.created)
	}
}

func TestCreateSessionIsIdempotentAndWatched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	request := proto.CreateSession{RequestID: "r1", Cwd: h.work + "/", Model: model, Thinking: "high"}
	first, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "Session 1" || first.Cwd != h.work || first.Thinking != "high" || first.State != proto.SessionIdle || first.Usage.Output != 3 {
		t.Fatalf("session %+v", first)
	}
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != first.ID || h.engine.created != 1 {
		t.Fatalf("retry %+v %v (created %d)", again, err, h.engine.created)
	}
	if !h.engine.watching[first.ID] {
		t.Fatal("the new session is not watched")
	}
	sessions, err := h.client.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != first.ID {
		t.Fatalf("sessions %+v %v", sessions, err)
	}
	detail, err := h.client.Session(ctx, first.ID)
	if err != nil || len(detail.Entries) != 1 || detail.Session.ID != first.ID {
		t.Fatalf("detail %+v %v", detail, err)
	}
	_, err = h.client.Session(ctx, 99)
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}

func TestSendAndAbort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	sent, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "hello"})
	if err != nil || sent.SubmissionID == 0 {
		t.Fatalf("send %+v %v", sent, err)
	}
	if again, _ := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "hello"}); again != sent {
		t.Fatalf("retry %+v != %+v", again, sent)
	}
	if _, ok := h.engine.sent["u:"+mustDevice(t, h).ID+":m1"]; !ok {
		t.Fatalf("request key not scoped to the device: %v", h.engine.sent)
	}
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m2", Text: "   "})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	_, err = h.client.Send(ctx, 42, proto.SendMessage{RequestID: "m3", Text: "x"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	h.engine.runFirst = 7
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a1"}); err != nil || len(h.engine.aborted) != 1 {
		t.Fatalf("abort %v %v", err, h.engine.aborted)
	}
	h.engine.runFirst = 8 // a later run
	// A retry of an abort whose answer was lost does not abort again.
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a1"}); err != nil || len(h.engine.aborted) != 1 {
		t.Fatalf("abort retry %v %v", err, h.engine.aborted)
	}
	_, err = h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "a1", Text: "x"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

func TestAbortRetiresOnlyTheEndedRunsApprovals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	for _, task := range []int64{3, 4} {
		if _, _, err := h.store.RequestApproval(ctx, store.ApprovalRow{SessionID: session.ID, TaskID: task, CallID: "b", Tool: "bash", Arguments: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	h.engine.runFirst = 10
	h.engine.ended[3] = true // task 4 belongs to a run started since
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a"}); err != nil {
		t.Fatal(err)
	}
	pending, _ := h.client.Approvals(ctx)
	if len(pending) != 1 || pending[0].ID != store.ApprovalID(4, "b") {
		t.Fatalf("pending after abort %+v", pending)
	}
}

func TestAbortRetryNeverStopsALaterRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	device := mustDevice(t, h)
	key := "u:" + device.ID + ":a"
	kind := "abort:" + strconv.FormatInt(session.ID, 10)
	// The first call recorded run 10, then the service stopped.
	if _, _, err := h.store.ClaimRequest(ctx, key, kind); err != nil {
		t.Fatal(err)
	}
	_ = h.store.CompleteRequest(ctx, key, "run:10")
	h.engine.runFirst = 20 // the owner started another run
	if err := h.client.Abort(ctx, session.ID, proto.Abort{RequestID: "a"}); err != nil {
		t.Fatal(err)
	}
	if len(h.engine.aborted) != 0 {
		t.Fatalf("the retry aborted run 20: %v", h.engine.aborted)
	}
	if result, _, _ := h.store.LookupRequest(ctx, key, kind); result != "done" {
		t.Fatalf("the retry did not complete the abort: %q", result)
	}
}

func TestAFailedCreateLeavesTheRequestIDFree(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "x", Cwd: filepath.Join(h.work, "missing"), Model: model})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "x", Text: "hi"}); err != nil {
		t.Fatalf("a request id of a failed create is still taken: %v", err)
	}
}

func TestADatabaseFailureIsNotUnauthorized(t *testing.T) {
	h := newHarness(t)
	_ = h.store.Close()
	_, err := h.client.Hello(context.Background())
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
}

func TestUnknownRoutesAnswerWithProtocolErrors(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
	response, e := h.raw("GET", "/v1/nowhere", "", headers)
	if response.StatusCode != http.StatusNotFound || e.Code != proto.ErrorCodeNotFound {
		t.Fatalf("unknown path: %d %+v", response.StatusCode, e)
	}
	response, e = h.raw("DELETE", proto.PathSessions, "", headers)
	if response.StatusCode != http.StatusMethodNotAllowed || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("wrong method: %d %+v", response.StatusCode, e)
	}
}

func TestCreateRetryReplaysBeforeRevalidatingAndRestoresTheWatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	work := filepath.Join(h.work, "gone")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	request := proto.CreateSession{RequestID: "r", Cwd: work, Model: model}
	first, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(work); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	delete(h.engine.watching, first.ID)
	h.engine.mu.Unlock()
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != first.ID {
		t.Fatalf("retry after the directory went away: %+v %v", again, err)
	}
	if !h.engine.watching[first.ID] {
		t.Fatal("the retry did not restore the watch")
	}
}

func TestEventStreamsEndOnRevocationAndStop(t *testing.T) {
	h := newHarness(t)
	h.server.streamCheck = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- h.client.Events(ctx, 0, func(proto.Event) error { return nil }) }()
	for h.hub.Subscribers() == 0 {
		runtime.Gosched()
	}
	if err := h.store.RevokeDevice(context.Background(), "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; !errors.Is(err, proto.ErrStreamEnded) {
		t.Fatalf("stream of a revoked device ended with %v", err)
	}

	other := newHarness(t)
	go func() { ended <- other.client.Events(ctx, 0, func(proto.Event) error { return nil }) }()
	for other.hub.Subscribers() == 0 {
		runtime.Gosched()
	}
	other.server.Stop()
	if err := <-ended; !errors.Is(err, proto.ErrStreamEnded) {
		t.Fatalf("stream after Stop ended with %v", err)
	}
}

func mustDevice(t *testing.T, h *harness) store.Device {
	t.Helper()
	d, err := h.store.DeviceByTokenHash(context.Background(), auth.Hash(h.token))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApprovalsDecideOnceAndRetriesReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	row, _, err := h.store.RequestApproval(ctx, store.ApprovalRow{SessionID: session.ID, TaskID: 5, CallID: "w1", Tool: "write", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := h.client.Approvals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != row.ID {
		t.Fatalf("pending %+v %v", pending, err)
	}
	decided, err := h.client.Decide(ctx, row.ID, proto.Decide{RequestID: "d1", Allow: true})
	if err != nil || decided.State != proto.ApprovalAllowed || decided.DecidedBy != "laptop" {
		t.Fatalf("decide %+v %v", decided, err)
	}
	retry, err := h.client.Decide(ctx, row.ID, proto.Decide{RequestID: "d1", Allow: true})
	if err != nil || retry.ID != decided.ID || retry.State != decided.State {
		t.Fatalf("retry %+v %v", retry, err)
	}
	_, err = h.client.Decide(ctx, row.ID, proto.Decide{RequestID: "d2", Allow: false})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.Decide(ctx, "anope", proto.Decide{RequestID: "d3", Allow: true})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	// A request id used for one request is refused for another.
	_, err = h.client.Decide(ctx, "anope", proto.Decide{RequestID: "d1", Allow: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "d1", Cwd: h.work, Model: model})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	response, e := h.raw("POST", proto.Path(proto.PathApproval, row.ID), `{"requestId":"d9"}`, map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, "allow") {
		t.Fatalf("a decision without allow: %d %+v", response.StatusCode, e)
	}
}

// collect reads events until it has n or the timeout passes.
func collect(t *testing.T, hub *events.Hub, client *proto.Client, last int64, n int, during func()) []proto.Event {
	t.Helper()
	before := hub.Subscribers()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []proto.Event
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- client.Events(ctx, last, func(e proto.Event) error {
			got = append(got, e)
			if len(got) == n {
				return context.Canceled
			}
			return nil
		})
	}()
	if during != nil {
		go func() {
			for hub.Subscribers() == before {
				runtime.Gosched()
			}
			close(started)
		}()
		<-started
		during()
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("events ended with %v after %d events", err, len(got))
	}
	return got
}

func TestEventsCarrySessionsLiveAndApprovals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var session proto.Session
	got := collect(t, h.hub, h.client, 0, 7, func() {
		session, _ = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
		h.server.PublishLive(proto.Live{SessionID: session.ID, Busy: true})
		h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	})
	types := []proto.EventType{}
	for _, e := range got {
		types = append(types, e.Type)
	}
	// The stream's hello, one session event at creation, then the watch's
	// initial live state; the entry brings its session.
	want := []proto.EventType{proto.EventHello, proto.EventSession, proto.EventLive, proto.EventLive, proto.EventSession, proto.EventEntry, proto.EventSession}
	for i, w := range want {
		if i >= len(types) || types[i] != w {
			t.Fatalf("event types %v, want %v", types, want)
		}
	}
	if got[4].Session.State != proto.SessionRunning {
		t.Fatalf("a busy live makes the session running: %+v", got[4].Session)
	}
	// Reconnecting after the first session event replays the rest, after
	// a hello at that event.
	replay := collect(t, h.hub, h.client, got[1].Seq, 6, nil)
	if replay[0].Type != proto.EventHello || replay[0].Seq != got[1].Seq || replay[1].Seq != got[2].Seq || replay[5].Seq != got[6].Seq {
		t.Fatalf("replay %v, want hello at %d, then %d..%d", replay, got[1].Seq, got[2].Seq, got[6].Seq)
	}
}

// TestEveryStreamStartsWithHello checks rule 18: the first event of every
// stream is hello, at the client's Last-Event-ID, or at the newest event
// without one. A client that opened without Last-Event-ID and reconnects
// from that seq misses nothing, even when no other event came before the
// stream dropped.
func TestEveryStreamStartsWithHello(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	newest := h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq
	first := collect(t, h.hub, h.client, 0, 1, nil)
	if first[0].Type != proto.EventHello || first[0].Seq != newest {
		t.Fatalf("a stream from now starts with %+v, want hello at %d", first[0], newest)
	}
	// The stream dropped after its hello alone; an entry landed meanwhile.
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: session.ID, Kind: proto.EntryUser, Text: "hi"})
	again := collect(t, h.hub, h.client, first[0].Seq, 3, nil)
	if again[0].Type != proto.EventHello || again[0].Seq != first[0].Seq || again[1].Type != proto.EventEntry || again[2].Type != proto.EventSession {
		t.Fatalf("reconnect from the hello: %+v", again)
	}
	// A future Last-Event-ID: resync, with the newest event. The hello's
	// seq resyncs again, also once the service gave out the future id, so
	// a client that drops before the resync gets it again.
	future := again[2].Seq + 1
	gone := collect(t, h.hub, h.client, future, 2, nil)
	if gone[0].Type != proto.EventHello || gone[0].Seq >= future || gone[1].Type != proto.EventResync || gone[1].Seq != again[2].Seq {
		t.Fatalf("a future Last-Event-ID: %+v", gone)
	}
	if h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq != future {
		t.Fatal("the next event did not take the future id")
	}
	back := collect(t, h.hub, h.client, gone[0].Seq, 2, nil)
	if back[1].Type != proto.EventResync {
		t.Fatalf("back from the resync's hello: %+v", back)
	}
}

// TestAnEntrySendsItsSession checks that an entry's event is followed by
// its session's (the session event's rule): updatedAt moved to when the
// entry landed, and the usage the answer added. A client keeps the session
// list in order from events alone.
func TestAnEntrySendsItsSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	h.clock.Advance(time.Minute)
	h.engine.mu.Lock()
	h.engine.usage = proto.Usage{Output: 40, Total: 50}
	h.engine.mu.Unlock()
	got := collect(t, h.hub, h.client, 0, 3, func() {
		h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	})
	if got[1].Type != proto.EventEntry || got[2].Type != proto.EventSession {
		t.Fatalf("events %+v", got)
	}
	sent := got[2].Session
	if !sent.UpdatedAt.Equal(session.UpdatedAt.Add(time.Minute)) || sent.Usage.Output != 40 || sent.Usage.Total != 50 {
		t.Fatalf("the entry's session %+v, created %+v", sent, session)
	}
}

func TestAPendingApprovalMakesTheSessionWaiting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	_, _, events, cancel := h.hub.Subscribe(0)
	defer cancel()
	manager := h.server.approvals
	done := make(chan struct{})
	go func() {
		_, _ = manager.Await(ctx, approvals.Call{SessionID: session.ID, TaskID: 1, CallID: "b1", Tool: "bash"})
		close(done)
	}()
	var requested proto.Approval
	for e := range events {
		if e.Type == proto.EventApproval {
			requested = *e.Approval
			break
		}
	}
	if s, _ := h.client.Session(ctx, session.ID); s.Session.State != proto.SessionWaiting {
		t.Fatalf("state while waiting %s", s.Session.State)
	}
	if _, _, err := manager.Decide(ctx, "k", requested.ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	<-done
	if s, _ := h.client.Session(ctx, session.ID); s.Session.State != proto.SessionIdle {
		t.Fatalf("state after the decision %s", s.Session.State)
	}
}

func TestBadLastEventID(t *testing.T) {
	h := newHarness(t)
	response, e := h.raw("GET", proto.PathEvents, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token, "Last-Event-ID": "x"})
	if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("%d %+v", response.StatusCode, e)
	}
}

func TestBodyLimits(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
	valid := `{"requestId":"r","cwd":"` + h.work + `","model":{"provider":"faux","id":"faux-1"}}`
	for name, body := range map[string]string{
		"trailing data":  valid + `{}`,
		"trailing close": valid + `}`,
		"big trailer":    valid + " " + strings.Repeat("x", maxBody),
		"over 1 MiB":     `{"requestId":"r","cwd":"` + strings.Repeat("a", maxBody) + `"}`,
		"long title":     `{"requestId":"r","cwd":"` + h.work + `","model":{"provider":"faux","id":"faux-1"},"title":"` + strings.Repeat("t", 201) + `"}`,
		"not an object":  `[]`,
	} {
		response, e := h.raw("POST", proto.PathSessions, body, headers)
		if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s: %d %+v", name, response.StatusCode, e)
		}
	}
	if h.engine.created != 0 {
		t.Fatalf("a rejected body created %d sessions", h.engine.created)
	}
	response, _ := h.raw("POST", proto.PathSessions, valid, headers)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("valid body: %d", response.StatusCode)
	}
	response, _ = h.raw("POST", proto.PathSessions, valid, headers)
	if response.StatusCode != http.StatusOK || h.engine.created != 1 {
		t.Fatalf("retry: %d, created %d", response.StatusCode, h.engine.created)
	}
}

// TestASessionEventThatFailsIsTriedAgain fails the read of a session after
// its entry: the entry goes out, and the session follows once a read
// succeeds, with no other change to send it (rule 16b). A summary with no
// usage never goes out in its place.
func TestASessionEventThatFailsIsTriedAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	h.engine.mu.Lock()
	h.engine.usageErr = errors.New("the durable store is busy")
	h.engine.mu.Unlock()
	from := h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "ok"})
	h.server.PublishEntry(proto.Entry{ID: 8, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "more"})
	if len(retries) != 1 {
		t.Fatalf("%d retries waiting, want one for the session", len(retries))
	}
	h.engine.mu.Lock()
	h.engine.usageErr, h.engine.usage = nil, proto.Usage{Output: 40}
	h.engine.mu.Unlock()
	(<-retries)()
	got := collect(t, h.hub, h.client, from, 4, nil)
	types := []proto.EventType{got[0].Type, got[1].Type, got[2].Type, got[3].Type}
	if !slices.Equal(types, []proto.EventType{proto.EventHello, proto.EventEntry, proto.EventEntry, proto.EventSession}) || got[3].Session.Usage.Output != 40 {
		t.Fatalf("events %v, session %+v", types, got[3].Session)
	}
	// A stopped server schedules no retry.
	h.engine.mu.Lock()
	h.engine.usageErr = errors.New("still busy")
	h.engine.mu.Unlock()
	h.server.Stop()
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: session.ID, Kind: proto.EntryAssistant, Text: "late"})
	if len(retries) != 0 {
		t.Fatal("a stopped server scheduled a retry")
	}
}

// TestAnEntrysTimeIsWrittenBeforeItsSessionGoesOut fails the write of an
// entry's time: the session does not go out with the time before the
// entry, and once the write succeeds it goes out with the new time.
func TestAnEntrysTimeIsWrittenBeforeItsSessionGoesOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	var fail sync.Map
	fail.Store("touch", true)
	h.server.touch = func(ctx context.Context, id int64, at time.Time) error {
		if v, _ := fail.Load("touch"); v == true {
			return errors.New("database is locked")
		}
		return h.store.TouchSession(ctx, id, at)
	}
	h.clock.Advance(time.Minute)
	from := h.hub.Publish(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: session.ID}}).Seq
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryUser, Text: "hi"})
	if len(retries) != 1 {
		t.Fatalf("%d retries waiting, want one", len(retries))
	}
	fail.Store("touch", false)
	(<-retries)()
	got := collect(t, h.hub, h.client, from, 3, nil)
	if got[1].Type != proto.EventEntry || got[2].Type != proto.EventSession || !got[2].Session.UpdatedAt.Equal(session.UpdatedAt.Add(time.Minute)) {
		t.Fatalf("events %+v", got)
	}
}

// TestACreatedSessionIsFollowedWhenItsAnswerFails fails the read of a new
// session's answer: the session is watched and its event goes out once it
// reads, and a retry of the request answers with it.
func TestACreatedSessionIsFollowedWhenItsAnswerFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	h.engine.mu.Lock()
	h.engine.usageErr = errors.New("the durable store is busy")
	h.engine.mu.Unlock()
	request := proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model}
	_, err := h.client.CreateSession(ctx, request)
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if h.engine.created != 1 || !h.engine.watching[1] {
		t.Fatalf("created %d, watching %v", h.engine.created, h.engine.watching)
	}
	h.engine.mu.Lock()
	h.engine.usageErr = nil
	h.engine.mu.Unlock()
	if len(retries) != 1 {
		t.Fatalf("%d retries waiting, want the creation event's", len(retries))
	}
	got := collect(t, h.hub, h.client, 0, 2, func() { (<-retries)() })
	if got[1].Type != proto.EventSession || got[1].Session.ID != 1 {
		t.Fatalf("events %+v", got)
	}
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != 1 || h.engine.created != 1 {
		t.Fatalf("the retry: %+v %v, created %d", again, err, h.engine.created)
	}
}

// TestEntriesAndSessionsGoOutUnderTheSessionLock checks that an entry, and
// every session event, is published holding sessionMu: no session event
// read before an entry's time is noted can go out after the entry (rule
// 16b).
func TestEntriesAndSessionsGoOutUnderTheSessionLock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	var unlocked []proto.EventType
	h.server.emit = func(e proto.Event) {
		if (e.Type == proto.EventEntry || e.Type == proto.EventSession) && h.server.sessionMu.TryLock() {
			h.server.sessionMu.Unlock()
			unlocked = append(unlocked, e.Type)
		}
		h.hub.Publish(e)
	}
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryUser, Text: "hi"})
	h.server.PublishLive(proto.Live{SessionID: session.ID, Busy: true})
	if len(unlocked) != 0 {
		t.Fatalf("published without the session lock: %v", unlocked)
	}
}

// TestATouchRetriedLaterKeepsItsEntrysTime fails the write of session 1's
// entry time, then session 2 gets a later entry: when the write of session
// 1 succeeds at last, it writes the time of its own entry, so session 2
// stays first.
func TestATouchRetriedLaterKeepsItsEntrysTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	one, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c1", Cwd: h.work, Model: model})
	two, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c2", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} }
	failing := map[int64]bool{one.ID: true}
	h.server.touch = func(ctx context.Context, id int64, at time.Time) error {
		if failing[id] {
			return errors.New("database is locked")
		}
		return h.store.TouchSession(ctx, id, at)
	}
	h.clock.Advance(time.Minute)
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: one.ID, Kind: proto.EntryUser, Text: "first"})
	h.clock.Advance(time.Minute)
	h.server.PublishEntry(proto.Entry{ID: 8, SessionID: two.ID, Kind: proto.EntryUser, Text: "later"})
	h.clock.Advance(time.Minute)
	failing[one.ID] = false
	(<-retries)()
	list, err := h.client.Sessions(ctx)
	if err != nil || len(list) != 2 || list[0].ID != two.ID {
		t.Fatalf("sessions %+v %v: the one with the later entry is not first", list, err)
	}
	if want := one.UpdatedAt.Add(time.Minute); !list[1].UpdatedAt.Equal(want) {
		t.Fatalf("session %d updated at %s, want its entry's %s", one.ID, list[1].UpdatedAt, want)
	}
}

// TestAStaleRetryNeitherPublishesNorEndsTheNextOne lets a retry run after
// it was cancelled, as a timer that fired while the lock was held does:
// it stands down, and leaves the retry that replaced it. After Stop, no
// retry runs.
func TestAStaleRetryNeitherPublishesNorEndsTheNextOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: model})
	retries := make(chan func(), 4)
	h.server.retry = func(f func()) func() { retries <- f; return func() {} } // cancel cannot stop it
	fail := func(on bool) {
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		h.engine.usageErr = nil
		if on {
			h.engine.usageErr = errors.New("busy")
		}
	}
	sessions := 0
	h.server.emit = func(e proto.Event) {
		if e.Type == proto.EventSession {
			sessions++
		}
		h.hub.Publish(e)
	}
	fail(true)
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: session.ID, Kind: proto.EntryUser, Text: "a"})
	stale := <-retries
	fail(false)
	h.server.PublishEntry(proto.Entry{ID: 8, SessionID: session.ID, Kind: proto.EntryUser, Text: "b"}) // goes out, cancels stale
	fail(true)
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: session.ID, Kind: proto.EntryUser, Text: "c"})
	next := <-retries
	before := sessions
	fail(false)
	stale()
	if sessions != before {
		t.Fatal("a cancelled retry published")
	}
	h.server.sessionMu.Lock()
	stillThere := h.server.retries[session.ID] != nil
	h.server.sessionMu.Unlock()
	if !stillThere {
		t.Fatal("a cancelled retry ended the one that replaced it")
	}
	h.server.Stop()
	next()
	if sessions != before {
		t.Fatal("a retry ran after Stop")
	}
}

// writeProfiles writes profile files into a folder of the harness's and
// gives the server that folder.
func (h *harness) writeProfiles(files map[string]string) (dir, home string) {
	h.t.Helper()
	dir, home = h.t.TempDir(), h.t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	h.server.SetProfiles(profiles.Folder{Dir: dir, Home: home})
	return dir, home
}

// GET /v1/profiles lists every profile file by id: a valid one with the
// model a session from it runs, through the router, or why none can; an
// invalid one with its problem; never a backup (ADR-0013).
func TestProfilesListsEveryProfileWithItsModel(t *testing.T) {
	h := newHarness(t)
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
	h.writeProfiles(map[string]string{
		"odrade.md":         "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\n---\n# Odrade\n",
		"vera.md":           "---\nmodel: openai/gpt-6-astra\nrepositories:\n  frostyard/b: /b\n  frostyard/a: /a\ntools: [read]\n---\n# Vera\n",
		"clerk.md":          "---\nrole: clerk\nmodel: github-copilot/gpt-6-luna\n---\n",
		"handy.md":          "---\nmodel: faux/faux-1\ntools: [read, teleport]\n---\n",
		"broken.md":         "---\nmodel: nope\n---\n",
		"odrade.md.bak-123": "---\nmodel: nope\n---\n",
	})
	list, err := h.client.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]proto.Profile{}
	for _, p := range list {
		ids = append(ids, p.ID)
		byID[p.ID] = p
	}
	if want := []string{"broken", "clerk", "handy", "odrade", "vera"}; !slices.Equal(ids, want) {
		t.Fatalf("ids %q, want %q", ids, want)
	}
	if p := byID["odrade"]; p.Name != "Odrade" || p.Role != proto.RoleSupervisor || p.Runs != model || p.Problem != "" || p.Home == "" {
		t.Errorf("odrade %+v", p)
	}
	// Each repository with its checkout's folder, by name (ADR-0027).
	if p := byID["vera"]; p.Runs != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}) ||
		!slices.Equal(p.Repositories, []proto.Repository{{Name: "frostyard/a", Folder: "/a"}, {Name: "frostyard/b", Folder: "/b"}}) ||
		!slices.Equal(p.Tools, []string{"read"}) {
		t.Errorf("vera %+v", p)
	}
	if p := byID["clerk"]; p.Runs != (proto.ModelRef{}) || !strings.Contains(p.Problem, "github-copilot/gpt-6-luna") {
		t.Errorf("clerk %+v", p)
	}
	if p := byID["handy"]; !strings.Contains(p.Problem, "teleport") {
		t.Errorf("handy %+v", p)
	}
	if p := byID["broken"]; p.Name != "" || !strings.Contains(p.Problem, "provider/id") {
		t.Errorf("broken %+v", p)
	}
}

// A session from a profile starts in the profile's home with the router's
// model, records its profile and route, and gives the engine the profile;
// the owner's model wins over the profile's.
func TestCreateSessionFromAProfile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
	_, home := h.writeProfiles(map[string]string{
		"vera.md":  "---\nmodel: openai/gpt-6-astra\nhome: ~\ntools: [read, bash]\n---\n# Vera\n\nShe tidies.\n",
		"chani.md": "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Chani\n",
	})
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	alias := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}
	if session.Cwd != home || session.Model != alias || session.Profile != "vera" || session.Route != "profile-alias" {
		t.Fatalf("session %+v", session)
	}
	spec := h.engine.specs[0]
	if spec.Profile == nil || spec.Profile.ID != "vera" || !slices.Equal(spec.Profile.Tools, []string{"read", "bash"}) || spec.Model != alias {
		t.Fatalf("spec %+v", spec)
	}
	owner, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r2", Profile: "vera", Cwd: h.work, Model: model})
	if err != nil || owner.Model != model || owner.Route != "owner" || owner.Cwd != h.work {
		t.Fatalf("the owner's model and folder: %+v, %v", owner, err)
	}
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r3", Cwd: h.work, Model: model})
	if err != nil || plain.Profile != "" || plain.Route != "owner" {
		t.Fatalf("a plain session: %+v, %v", plain, err)
	}
	if private, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r4", Profile: "chani"}); err != nil || private.Cwd != home {
		t.Fatalf("a private agent in its home: %+v, %v", private, err)
	}
	created := h.engine.created
	for name, request := range map[string]proto.CreateSession{
		"a private agent elsewhere": {RequestID: "b1", Profile: "chani", Cwd: h.work},
		"an unknown profile":        {RequestID: "b2", Profile: "nobody"},
		"a path as a profile":       {RequestID: "b3", Profile: "../vera"},
		"no model and no profile":   {RequestID: "b4", Cwd: h.work},
	} {
		var e *proto.Error
		if _, err := h.client.CreateSession(ctx, request); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if h.engine.created != created {
		t.Fatalf("a refused request created %d sessions", h.engine.created-created)
	}
}

// A conversation made for a request whose record failed is recorded, at
// the retry, as it was made: not re-routed from a profile that changed or
// is gone since.
func TestARetryRecordsTheSessionAsItWasMade(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}}}
	dir, home := h.writeProfiles(map[string]string{"vera.md": "---\nmodel: openai/gpt-6-astra\nhome: ~\n---\n"})
	device, err := auth.Authenticate(ctx, h.store, "Bearer "+h.token)
	if err != nil {
		t.Fatal(err)
	}
	// The first try made the conversation, and failed before recording it.
	vera, err := profiles.Find(dir, home, "vera")
	if err != nil {
		t.Fatal(err)
	}
	alias := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}
	if _, err := h.engine.CreateSession(ctx, "u:"+device.ID+":r1", SessionSpec{Title: "First title", Cwd: home, Model: alias, Profile: &vera, Route: "profile-alias"}); err != nil {
		t.Fatal(err)
	}
	// Since then the profile was removed.
	if err := os.Remove(filepath.Join(dir, "vera.md")); err != nil {
		t.Fatal(err)
	}
	// The retry's own title is neither checked nor taken: the first was.
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "vera", Title: strings.Repeat("x", 300)})
	if err != nil {
		t.Fatal(err)
	}
	if session.Title != "First title" || session.Cwd != home || session.Model != alias || session.Profile != "vera" || session.Route != "profile-alias" || h.engine.created != 1 {
		t.Fatalf("the retry recorded %+v (%d created)", session, h.engine.created)
	}
}

// An owner's model does not start a profile whose own model cannot run:
// GET /v1/profiles lists it with a problem.
func TestAnOwnerModelDoesNotStartAProfileThatCannot(t *testing.T) {
	h := newHarness(t)
	h.writeProfiles(map[string]string{"clerk.md": "---\nrole: clerk\nmodel: github-copilot/gpt-6-luna\nhome: ~\n---\n"})
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "clerk", Model: model})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
}

// With profiles off (no source), a plain session starts and a profile is
// refused, never read from the working directory.
func TestProfilesOff(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.server.SetProfiles(nil)
	if list, err := h.client.Profiles(ctx); err != nil || len(list) != 0 {
		t.Fatalf("profiles %+v, %v", list, err)
	}
	if _, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Cwd: h.work, Model: model}); err != nil {
		t.Fatalf("a plain session: %v", err)
	}
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r2", Profile: "vera"})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
}

// A profile with a workspace and no home lists the workspace as where a
// session from it starts, and a session starts there.
func TestAWorkspaceProfileStartsInItsWorkspace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	workspace := t.TempDir()
	h.writeProfiles(map[string]string{"clerk.md": "---\nrole: clerk\nmodel: faux/faux-1\nworkspace: " + workspace + "\n---\n"})
	list, err := h.client.Profiles(ctx)
	if err != nil || len(list) != 1 || list[0].Cwd != workspace || list[0].Home != "" {
		t.Fatalf("profiles %+v, %v", list, err)
	}
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "clerk"})
	if err != nil || session.Cwd != workspace {
		t.Fatalf("session %+v, %v", session, err)
	}
}

// A private profile whose home is not a folder is listed with a problem.
func TestAPrivateProfileWithoutItsHomeCannotStart(t *testing.T) {
	h := newHarness(t)
	_, home := h.writeProfiles(map[string]string{"chani.md": "---\nmodel: faux/faux-1\nhome: ~/gone\nprivate: true\n---\n"})
	list, err := h.client.Profiles(context.Background())
	if err != nil || len(list) != 1 || !strings.Contains(list[0].Problem, filepath.Join(home, "gone")) {
		t.Fatalf("profiles %+v, %v", list, err)
	}
}

// A legacy create record knows no title: a retry's own title, checked, is
// recorded.
func TestALegacyRetryKeepsItsTitle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	device, err := auth.Authenticate(ctx, h.store, "Bearer "+h.token)
	if err != nil {
		t.Fatal(err)
	}
	key := "u:" + device.ID + ":r1"
	h.engine.next = 7
	h.engine.byKey[key] = 7
	h.engine.made = map[string]Created{key: {ID: 7, Legacy: true, Cwd: h.work, Model: model, Route: "owner"}}
	session, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Cwd: h.work, Model: model, Title: "My own title"})
	if err != nil || session.ID != 7 || session.Title != "My own title" || session.Route != "owner" {
		t.Fatalf("legacy retry %+v, %v", session, err)
	}
	_, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r2", Cwd: h.work, Model: model, Title: strings.Repeat("x", 300)})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
}

// liveShiftWakes are the wakes the server set and did not cancel.
func (h *harness) liveShiftWakes() []shiftWake {
	h.shiftMu.Lock()
	defer h.shiftMu.Unlock()
	var out []shiftWake
	for _, w := range h.shiftWakes {
		if !*w.cancelled {
			out = append(out, w)
		}
	}
	return out
}

// tryLock reserves a free key under the one hold of the map that found it
// free: no request can take the key between, so it never waits (Codex
// review of #49, round 4).
func TestTryLockReservesTheKeyAsItFindsItFree(t *testing.T) {
	var k keyLocks
	reserved := false
	k.afterTryLock = func() { reserved = k.held["K"] != nil && k.held["K"].users == 1 }
	unlock, ok := k.tryLock("K")
	if !ok || !reserved {
		t.Fatalf("tryLock = %v, reserved as it found the key free: %v", ok, reserved)
	}
	if _, again := k.tryLock("K"); again {
		t.Fatal("a key held was taken again")
	}
	unlock()
	if len(k.held) != 0 {
		t.Fatalf("the key is still held: %v", k.held)
	}
}
