// Package terminal runs the owner's shells on the service host (ADR-0025):
// login shells of the service user, each on a pseudo-terminal, outside the
// agents' sandbox, for whoever holds a device token. It is pi-pocket-team's
// terminal module (terminal.ts) in Go:
//
//   - a shell starts in a folder the owner picks, MaxTerminals at most at
//     once, with only the variables of pocketd's environment that
//     Environment keeps;
//   - pocketd keeps the last Keep bytes of each shell's output, so a client
//     that comes back gets its screen;
//   - the output comes by a long poll of at most PollWait that names how
//     much of it the client has, and a shell runs only as fast as its
//     slowest client draws: its output pauses while a client that still
//     polls is more than Keep/2 behind;
//   - keys come numbered by the client that sends them, and keys sent again
//     are written once;
//   - a shell ends on End, when it exits, DetachAfter after its last poll,
//     IdleAfter after its last input, or when pocketd stops; one that
//     exited stays listed for Grace.
//
// Nothing here writes a key or the output to a log.
package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The limits of pi-pocket-team's terminal module.
const (
	// Cap is the most shells that run at once.
	Cap = proto.MaxTerminals
	// Keep is how many bytes of a shell's output are kept, and the most
	// one answer carries.
	Keep = proto.TerminalKept
	// InputMax is the most bytes of keys one input carries.
	InputMax = proto.MaxTerminalInput
	// PollWait is how long a poll waits for output before it answers with
	// none.
	PollWait = 25 * time.Second
	// Tick is how often the timers are checked (Check).
	Tick = 5 * time.Second
	// Grace is how long a shell that exited stays listed, so a poll can
	// show how it ended.
	Grace = 60 * time.Second
	// TabSpan is how long a client counts for back pressure after its last
	// poll: a poll waits PollWait, and 5 s is the margin.
	TabSpan = 30 * time.Second
	// TabsMax is how many clients one shell keeps, for back pressure and
	// for input: the oldest goes, or a new client's keys are refused.
	TabsMax = 16
	// ReceiptSpan is how long a client's last input number is kept: a
	// client that gives up after 30 s sends its keys again with a new one.
	ReceiptSpan = 10 * time.Minute
	// DetachAfter ends a shell no poll asked for this long.
	DetachAfter = 60 * time.Second
	// IdleAfter ends a shell that took no input for this long.
	IdleAfter = 60 * time.Minute
	// The size of a shell: rows and columns, and the size it starts at
	// when none is given.
	MinRows, MaxRows         = 2, 500
	MinCols, MaxCols         = 2, 1000
	DefaultRows, DefaultCols = 24, 80
	// InputQueue is the most bytes of keys that wait for a shell to read
	// them: an input past it waits, as a full pipe does.
	InputQueue = InputMax
	// hangUpGrace is how long an ended shell's group has after SIGHUP
	// before SIGKILL, closeAfter when its pseudo-terminal closes if it
	// still runs, and closeWait how long Close waits for the shells.
	hangUpGrace = 2 * time.Second
	closeAfter  = 5 * time.Second
	closeWait   = 3 * time.Second
	// DrainAfter is how long the output of a shell that exited is read
	// before its pseudo-terminal closes, whatever still holds it open (a
	// job left in the background).
	DrainAfter = time.Second
)

// What a call fails with; the API answers ErrNotFound with 404, a
// *BadRequest with 400 and the others with 409.
var (
	ErrNotFound    = errors.New("no such terminal")
	ErrFull        = fmt.Errorf("pocketd runs %d terminals at once: end one first", Cap)
	ErrEnded       = errors.New("this terminal has ended")
	ErrTooManyTabs = fmt.Errorf("%d clients type into this terminal already", TabsMax)
	ErrClosed      = errors.New("pocketd is stopping: try again in a moment")
)

// BadRequest is a call that breaks a rule of its arguments.
type BadRequest struct{ Why string }

func (e *BadRequest) Error() string { return e.Why }

func bad(format string, args ...any) error { return &BadRequest{Why: fmt.Sprintf(format, args...)} }

// StartSize is the size a shell starts at: 0 for the default, else rows of
// MinRows to MaxRows and columns of MinCols to MaxCols.
func StartSize(rows, cols int) (int, int, error) {
	if rows == 0 {
		rows = DefaultRows
	}
	if cols == 0 {
		cols = DefaultCols
	}
	return rows, cols, CheckSize(rows, cols)
}

// CheckSize checks a size a shell takes.
func CheckSize(rows, cols int) error {
	if rows < MinRows || rows > MaxRows || cols < MinCols || cols > MaxCols {
		return bad("give rows of %d to %d and cols of %d to %d, not %d by %d", MinRows, MaxRows, MinCols, MaxCols, rows, cols)
	}
	return nil
}

// tabPattern is a client's name: 1 to 64 letters, digits and dashes.
var tabPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// CheckTab checks a client's name.
func CheckTab(tab string) error {
	if !tabPattern.MatchString(tab) {
		return bad("give tab as 1 to 64 letters, digits and dashes")
	}
	return nil
}

// allowed are the variables of pocketd's environment a shell gets, with
// those that start with LC_: no key, token or pocketd setting goes through.
var allowed = []string{"HOME", "USER", "LOGNAME", "SHELL", "PATH", "LANG", "TZ", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}

// Environment is a shell's environment from pocketd's (environ): the
// variables allowed and LC_*, HOME (home when environ has none), and the
// terminal's TERM and COLORTERM.
func Environment(environ []string, home string) []string {
	var out []string
	hasHome := false
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || (!slices.Contains(allowed, name) && !strings.HasPrefix(name, "LC_")) {
			continue
		}
		hasHome = hasHome || name == "HOME"
		out = append(out, kv)
	}
	if !hasHome && home != "" {
		out = append(out, "HOME="+home)
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}

// LoginShell is the shell the passwd file gives user uid, else /bin/bash.
func LoginShell(passwd []byte, uid int) string {
	for line := range strings.Lines(string(passwd)) {
		fields := strings.Split(strings.TrimRight(line, "\r\n"), ":")
		if len(fields) == 7 && fields[2] == strconv.Itoa(uid) && filepath.IsAbs(fields[6]) {
			return fields[6]
		}
	}
	return "/bin/bash"
}

// Options configure a Manager.
type Options struct {
	// Shell is the login shell a terminal runs; "" is the service user's
	// (LoginShell of /etc/passwd).
	Shell string
	// Home is the service user's home folder; "" is os.UserHomeDir's.
	Home string
	// Env is a shell's environment; nil is Environment of pocketd's.
	Env []string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// After fires a poll's deadline and Close's; nil is time.After.
	After func(d time.Duration) <-chan time.Time
	// Timer runs f after d, for the checks (Tick) and an ended shell's
	// SIGKILL; nil is time.AfterFunc. Tests run them when they choose.
	Timer func(d time.Duration, f func()) (cancel func())
	// OnChange gets the whole list after every change of it, in order,
	// under the Manager's lock: it must not call the Manager.
	OnChange func([]proto.Terminal)
}

// Manager runs the owner's shells.
type Manager struct {
	opts Options

	mu    sync.Mutex
	terms map[string]*term
	// running are the shells not reaped yet, ended ones included: Close
	// waits for them.
	running map[*term]bool
	closed  bool
	// ticking says a check is due (Timer); seq orders the shells started
	// at one instant.
	ticking    bool
	cancelTick func()
	seq        int64
	// afterExit runs once a shell exited, before it is reaped; tests look
	// at the process there.
	afterExit func(pid int)
}

// New returns a Manager with no shell.
func New(opts Options) *Manager {
	if opts.Home == "" {
		opts.Home, _ = os.UserHomeDir()
	}
	if opts.Shell == "" {
		passwd, _ := os.ReadFile("/etc/passwd")
		opts.Shell = LoginShell(passwd, os.Getuid())
	}
	if opts.Env == nil {
		opts.Env = Environment(os.Environ(), opts.Home)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = time.After
	}
	if opts.Timer == nil {
		opts.Timer = func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		}
	}
	return &Manager{opts: opts, terms: map[string]*term{}, running: map[*term]bool{}}
}

// Home is the service user's home folder, where a shell starts by default.
func (m *Manager) Home() string { return m.opts.Home }

// term is one shell. Everything but sh and the fixed fields is guarded by
// the Manager's mu.
type term struct {
	info proto.Terminal
	seq  int64
	sh   *shell
	// cond wakes the reader (paused), the writer (in) and both at the end
	// (gone, exited); changed is closed and replaced at each change a poll
	// waits for.
	cond    *sync.Cond
	changed chan struct{}
	// out is the last Keep bytes of the output, total every byte since the
	// start: out begins at total-out.size.
	out   history
	total int64
	// asked is the highest offset a poll asked for; tabs what each client
	// last drew and when it polled, receipts the last input each client
	// sent and when.
	asked    int64
	tabs     map[string]stamp
	receipts map[string]stamp
	lastPoll time.Time
	// lastInput is when the shell last took keys; polls are the polls
	// that wait now.
	lastInput time.Time
	polls     int
	// paused stops the reader, as a full pipe would: a client is more than
	// Keep/2 behind.
	paused bool
	// in is the keys not written yet; taken is closed and replaced as the
	// writer takes them, for an input that waits for room.
	in    []byte
	taken chan struct{}
	// reaped says the shell's process is gone (its id may name another
	// now), exited once its output was read too, with its code; gone says
	// it ended, and left the list.
	reaped   bool
	exited   bool
	code     *int
	exitedAt time.Time
	gone     bool
	// readDone closes when the reader stops, done when the shell is
	// reaped and read.
	readDone chan struct{}
	done     chan struct{}
}

// stamp is an offset or a number, and when a client gave it.
type stamp struct {
	n    int64
	seen time.Time
}

// NewID is a new terminal's id: 32 hexadecimal characters.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Start starts a login shell with id (NewID) in folder cwd at rows by cols
// (checked by StartSize), and returns it as listed: in the folder it
// started in, the home folder, else /, when cwd is gone. Cap counts the
// shells that run, those that ended and are not reaped yet included. A
// shell that fails to start leaves nothing behind.
func (m *Manager) Start(id, cwd string, rows, cols int) (proto.Terminal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return proto.Terminal{}, ErrClosed
	}
	if m.runningLocked() >= Cap {
		return proto.Terminal{}, ErrFull
	}
	if _, taken := m.terms[id]; taken || id == "" {
		return proto.Terminal{}, fmt.Errorf("terminal id %q is taken", id)
	}
	sh, cwd, err := startShell(m.opts.Shell, cwd, m.opts.Home, m.opts.Env, rows, cols)
	if err != nil {
		return proto.Terminal{}, err
	}
	now := m.opts.Now()
	title := filepath.Base(cwd)
	if title == "." || title == "" {
		title = cwd
	}
	m.seq++
	t := &term{
		info:     proto.Terminal{ID: id, Title: title, Cwd: cwd, StartedAt: now.UTC()},
		seq:      m.seq,
		sh:       sh,
		changed:  make(chan struct{}),
		tabs:     map[string]stamp{},
		receipts: map[string]stamp{},
		lastPoll: now, lastInput: now,
		taken:    make(chan struct{}),
		readDone: make(chan struct{}),
		done:     make(chan struct{}),
	}
	t.cond = sync.NewCond(&m.mu)
	m.terms[t.info.ID] = t
	m.running[t] = true
	go m.read(t)
	go m.write(t)
	go m.wait(t)
	m.announceLocked()
	m.armLocked()
	return t.info, nil
}

// runningLocked counts the shells not reaped: those listed that have not
// exited, and those ended that are still on their way out.
func (m *Manager) runningLocked() int {
	n := 0
	for t := range m.running {
		if !t.reaped {
			n++
		}
	}
	return n
}

// read takes the shell's output until its pseudo-terminal ends, waiting
// while a client is too far behind.
func (m *Manager) read(t *term) {
	defer close(t.readDone)
	buf := make([]byte, 64*1024)
	for {
		m.mu.Lock()
		for t.paused && !t.gone {
			t.cond.Wait()
		}
		m.mu.Unlock()
		n, err := t.sh.read(buf)
		if n > 0 {
			m.mu.Lock()
			if !t.gone {
				m.appendLocked(t, buf[:n])
			}
			m.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// write gives the shell its keys, in order, until it ends.
func (m *Manager) write(t *term) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		for len(t.in) == 0 && !t.gone && !t.exited {
			t.cond.Wait()
		}
		if t.gone || t.exited {
			return
		}
		data := t.in
		t.in = nil
		close(t.taken)
		t.taken = make(chan struct{})
		m.mu.Unlock()
		// Keys that do not go in are lost with the shell, which is ending.
		_, _ = t.sh.write(data)
		m.mu.Lock()
	}
}

// wait reaps the shell once it exited, under the lock that signals it, so
// no signal goes to its id after the kernel may give it to another. Then
// it marks it exited once its output is read, what it printed before it
// exited first: until the pseudo-terminal ends, DrainAfter at most, since
// a job left in the background may hold it open.
func (m *Manager) wait(t *term) {
	t.sh.awaitExit()
	if m.afterExit != nil {
		m.afterExit(t.sh.pid())
	}
	m.mu.Lock()
	code := t.sh.reap()
	// What the shell left is read whatever a client drew: no program waits
	// on it any more.
	t.reaped, t.paused = true, false
	t.cond.Broadcast()
	m.mu.Unlock()
	select {
	case <-t.readDone:
	case <-m.opts.After(DrainAfter):
	}
	t.sh.close()
	<-t.readDone
	m.mu.Lock()
	defer m.mu.Unlock()
	t.exited, t.code, t.exitedAt = true, code, m.opts.Now()
	delete(m.running, t)
	t.cond.Broadcast()
	close(t.done)
	if !t.gone {
		m.wakeLocked(t)
		m.announceLocked()
	}
}

// appendLocked adds output: the oldest goes past Keep bytes, from a
// character's first byte.
func (m *Manager) appendLocked(t *term, p []byte) {
	t.total += int64(len(p))
	t.out.write(p)
	m.wakeLocked(t)
	m.pressureLocked(t)
}

func (m *Manager) wakeLocked(t *term) {
	close(t.changed)
	t.changed = make(chan struct{})
}

// watched is the offset back pressure measures the output against: what
// the slowest client that still polls has drawn, else the highest offset
// asked for.
func (t *term) watched() int64 {
	if len(t.tabs) == 0 {
		return t.asked
	}
	oldest := int64(-1)
	for _, s := range t.tabs {
		if oldest < 0 || s.n < oldest {
			oldest = s.n
		}
	}
	return oldest
}

// pressureLocked pauses the reader while a client is more than Keep/2
// behind, so a read that comes after still leaves its offset in what is
// kept, and goes on once it is not.
func (m *Manager) pressureLocked(t *term) {
	if t.reaped || t.gone {
		return
	}
	behind := t.total - t.watched()
	switch {
	case !t.paused && behind > Keep/2:
		t.paused = true
	case t.paused && behind <= Keep/2:
		t.paused = false
		t.cond.Broadcast()
	}
}

// bounded drops the clients not seen for span, and all but the TabsMax
// seen last.
func bounded(m map[string]stamp, now time.Time, span time.Duration) {
	for id, s := range m {
		if now.Sub(s.seen) >= span {
			delete(m, id)
		}
	}
	for len(m) > TabsMax {
		var oldest string
		for id, s := range m {
			if oldest == "" || s.seen.Before(m[oldest].seen) {
				oldest = id
			}
		}
		delete(m, oldest)
	}
}

// find is a listed shell that has not ended.
func (m *Manager) findLocked(id string) (*term, error) {
	t := m.terms[id]
	if t == nil || t.gone {
		return nil, ErrNotFound
	}
	return t, nil
}

// offsetLocked is where offset from is in what is kept: never before its
// start, never past its end.
func (t *term) offset(from int64) int64 {
	return min(max(from, t.total-int64(t.out.size)), t.total)
}

// progress is what the shell has for a poll from offset from: its output
// from there, or that it exited; ok is false when it has nothing new.
func (t *term) progress(from int64) (proto.TerminalOutput, bool) {
	offset := t.offset(from)
	if t.total <= offset && !t.exited {
		return proto.TerminalOutput{}, false
	}
	data := t.out.from(int(offset - (t.total - int64(t.out.size))))
	out := proto.TerminalOutput{Data: data, From: offset, To: offset + int64(len(data))}
	if t.exited {
		out.Exited, out.ExitCode = true, t.code
	}
	return out, true
}

// Poll waits for shell id's output past offset from, at most PollWait or
// until ctx ends, and answers it, or none from where from is now; without
// wait it answers at once with what is kept, as a client reads a shell's
// history apart from what comes live. tab names the client, "" for none:
// back pressure follows what each client drew (ack, at most from). A shell
// that ended is ErrNotFound, even while the poll waits.
func (m *Manager) Poll(ctx context.Context, id, tab string, from, ack int64, wait bool) (proto.TerminalOutput, error) {
	switch {
	case from < 0 || ack < 0:
		return proto.TerminalOutput{}, bad("give from and ack as whole numbers")
	case ack > from:
		return proto.TerminalOutput{}, bad("a poll draws no more than it received: ack %d is past from %d", ack, from)
	case tab != "":
		if err := CheckTab(tab); err != nil {
			return proto.TerminalOutput{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.findLocked(id)
	if err != nil {
		return proto.TerminalOutput{}, err
	}
	now := m.opts.Now()
	// A poll keeps its shell from the detach timer while it waits.
	t.polls++
	t.lastPoll = now
	defer func() { t.polls-- }()
	// Back pressure counts only output that was made: a poll from past
	// the end claims no more than the end (Codex review of #41, round 4).
	t.asked = max(t.asked, min(from, t.total))
	if tab != "" {
		t.tabs[tab] = stamp{n: min(ack, t.total), seen: now}
		bounded(t.tabs, now, TabSpan)
	}
	m.pressureLocked(t)
	deadline := m.opts.After(PollWait)
	timedOut := false
	for {
		if t.gone {
			return proto.TerminalOutput{}, ErrNotFound
		}
		if out, ok := t.progress(from); ok {
			return out, nil
		}
		if !wait || timedOut || ctx.Err() != nil {
			offset := t.offset(from)
			return proto.TerminalOutput{Data: []byte{}, From: offset, To: offset}, nil
		}
		changed := t.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		case <-deadline:
			timedOut = true
		}
		m.mu.Lock()
	}
}

// Input gives shell id the keys data from client tab, numbered seq: an
// input whose number is not above the last one tab sent is taken already,
// and nothing is written. While InputQueue bytes wait for the shell to
// read them, an input waits for room, as on a full pipe, until ctx ends:
// one that does not go in is not taken, and is sent again with its number.
func (m *Manager) Input(ctx context.Context, id, tab string, seq int64, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.findLocked(id)
	if err != nil {
		return err
	}
	if err := CheckTab(tab); err != nil {
		return err
	}
	if seq < 1 {
		return bad("give seq as a positive whole number")
	}
	if len(data) > InputMax {
		return bad("send at most %d bytes at a time", InputMax)
	}
	for {
		if t.gone {
			return ErrNotFound
		}
		// A shell reaped takes no keys, though its output still drains:
		// a job left in the background would read them (Codex review of
		// #41, round 3).
		if t.reaped {
			return ErrEnded
		}
		now := m.opts.Now()
		for name, r := range t.receipts {
			if now.Sub(r.seen) >= ReceiptSpan {
				delete(t.receipts, name)
			}
		}
		last, known := t.receipts[tab]
		if !known && len(t.receipts) >= TabsMax {
			return ErrTooManyTabs
		}
		if seq <= last.n {
			t.receipts[tab] = stamp{n: last.n, seen: now}
			return nil
		}
		if len(t.in) == 0 || len(t.in)+len(data) <= InputQueue {
			t.receipts[tab] = stamp{n: seq, seen: now}
			if len(data) > 0 {
				t.lastInput = now
				t.in = append(t.in, data...)
				t.cond.Broadcast()
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		taken, changed := t.taken, t.changed
		m.mu.Unlock()
		select {
		case <-taken:
		case <-changed:
		case <-ctx.Done():
		}
		m.mu.Lock()
	}
}

// Resize tells shell id its size.
func (m *Manager) Resize(id string, rows, cols int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.findLocked(id)
	if err != nil {
		return err
	}
	if err := CheckSize(rows, cols); err != nil {
		return err
	}
	if t.reaped {
		return ErrEnded
	}
	return t.sh.resize(rows, cols)
}

// Running says whether shell id is listed and its process runs.
func (m *Manager) Running(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.findLocked(id)
	return err == nil && !t.reaped
}

// End ends shell id now: it leaves the list, and a poll that waits on it
// is ErrNotFound.
func (m *Manager) End(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.findLocked(id)
	if err != nil {
		return err
	}
	m.stopLocked(t)
	return nil
}

// stopLocked ends a shell: it leaves the list at once, its process group
// gets SIGHUP, SIGKILL hangUpGrace later, and its pseudo-terminal closes
// closeAfter later, if it still runs.
func (m *Manager) stopLocked(t *term) {
	if t.gone {
		return
	}
	t.gone = true
	delete(m.terms, t.info.ID)
	t.paused, t.in = false, nil
	t.cond.Broadcast()
	m.wakeLocked(t)
	if !t.reaped {
		t.sh.signal(hangUp)
		m.opts.Timer(hangUpGrace, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if !t.reaped {
				t.sh.signal(kill)
			}
		})
		m.opts.Timer(closeAfter, func() {
			m.mu.Lock()
			exited := t.exited
			m.mu.Unlock()
			if !exited {
				t.sh.close()
			}
		})
	}
	m.announceLocked()
}

// List is every shell listed, oldest first.
func (m *Manager) List() []proto.Terminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked()
}

func (m *Manager) listLocked() []proto.Terminal {
	terms := make([]*term, 0, len(m.terms))
	for _, t := range m.terms {
		terms = append(terms, t)
	}
	slices.SortFunc(terms, func(a, b *term) int {
		if c := a.info.StartedAt.Compare(b.info.StartedAt); c != 0 {
			return c
		}
		return int(a.seq - b.seq)
	})
	out := make([]proto.Terminal, 0, len(terms))
	for _, t := range terms {
		info := t.info
		if t.exited {
			info.Exited, info.ExitCode = true, t.code
		}
		out = append(out, info)
	}
	return out
}

func (m *Manager) announceLocked() {
	if m.opts.OnChange != nil {
		m.opts.OnChange(m.listLocked())
	}
}

// armLocked has the checks run Tick from now while a shell is listed.
func (m *Manager) armLocked() {
	if m.ticking || m.closed || len(m.terms) == 0 {
		return
	}
	m.ticking = true
	m.cancelTick = m.opts.Timer(Tick, func() {
		m.mu.Lock()
		m.ticking = false
		m.mu.Unlock()
		m.Check()
	})
}

// Check runs the timers: a shell that exited Grace ago leaves the list, one
// no poll asked for DetachAfter ends, as does one that took no keys for
// IdleAfter, and a client that stopped polling no longer holds its shell
// back.
func (m *Manager) Check() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opts.Now()
	for _, info := range m.listLocked() {
		t := m.terms[info.ID]
		bounded(t.tabs, now, TabSpan)
		switch {
		case t.exited:
			if now.Sub(t.exitedAt) >= Grace {
				delete(m.terms, t.info.ID)
				m.announceLocked()
			}
		case t.polls == 0 && now.Sub(t.lastPoll) >= DetachAfter:
			m.stopLocked(t)
		case now.Sub(t.lastInput) >= IdleAfter:
			m.stopLocked(t)
		default:
			m.pressureLocked(t)
		}
	}
	m.armLocked()
}

// Close ends every shell, as pocketd stops, and waits closeWait at most for
// them to go. No shell starts after it.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancelTick != nil {
		m.cancelTick()
		m.cancelTick, m.ticking = nil, false
	}
	for _, info := range m.listLocked() {
		m.stopLocked(m.terms[info.ID])
	}
	var waits []chan struct{}
	for t := range m.running {
		waits = append(waits, t.done)
	}
	m.mu.Unlock()
	deadline := m.opts.After(closeWait)
	for _, done := range waits {
		select {
		case <-done:
		case <-deadline:
			return
		}
	}
}
