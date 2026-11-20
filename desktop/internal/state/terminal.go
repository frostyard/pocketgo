package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's terminals (ADR-0025): shells pocketd runs on the service
// host, which frostyard/pocket showed against pi-pocket-team's terminal
// module. Their list merges whole: each terminals event carries all of it,
// and wins over a fetch begun before it. A shell's output and keys go
// through a TermConn, which pocket's terminal emulator reads and writes.

// SetTerminals takes the answer of GET /v1/terminals, begun as f, unless
// the answer of a fetch begun later was applied, or an event brought the
// list since f began.
func (d *Data) SetTerminals(f Fetch, list []proto.Terminal) {
	if !d.terminalsList.answer(f) {
		return
	}
	d.rev++
	d.terminalsList.status, d.terminalsList.err = ListLoaded, ""
	// A fetch begun after a start answered lists its shell, or knows it
	// ended.
	for id, at := range d.terminalsAwaited {
		if f.epoch > at {
			delete(d.terminalsAwaited, id)
		}
	}
	if newer(d.terminalsTouched, f) {
		return
	}
	d.terminals = slices.Clone(list)
}

// SetTerminalsError records that the terminals' fetch f failed, unless a
// fetch begun later already answered, or an event brought the whole list
// since f began.
func (d *Data) SetTerminalsError(f Fetch, err string) {
	if d.terminalsList.answer(f) && !newer(d.terminalsTouched, f) {
		d.rev++
		d.terminalsList.status, d.terminalsList.err = ListFailed, err
	}
}

// applyTerminals takes a terminals event: the whole list, known now.
func (d *Data) applyTerminals(list []proto.Terminal) {
	d.terminals = slices.Clone(list)
	d.terminalsTouched = d.touched()
	d.terminalsList.status, d.terminalsList.err = ListLoaded, ""
	for _, t := range list {
		delete(d.terminalsAwaited, t.ID)
	}
}

// awaitTerminal notes a shell pocketd started for the owner, before the
// fetch that lists it begins: until a list brings it, or a fetch begun
// after says it is gone, it stays selected and open.
func (d *Data) awaitTerminal(id string) {
	if d.terminalsAwaited == nil {
		d.terminalsAwaited = map[string]uint64{}
	}
	d.terminalsAwaited[id] = d.epoch
}

// TerminalSelection is the shell to show: sel while it is listed, or
// started and not listed yet, else the newest listed, "" when none is.
func (d *Data) TerminalSelection(sel string) string {
	if _, awaited := d.terminalsAwaited[sel]; awaited && sel != "" {
		return sel
	}
	return TerminalSelection(d.terminals, sel)
}

// TerminalsStatus is where the list of shells stands, and why its last
// fetch failed. The Terminal place says no shell runs only when it is
// ListLoaded.
func (d *Data) TerminalsStatus() (ListStatus, string) {
	return d.terminalsList.status, d.terminalsList.err
}

// Terminals are the shells pocketd lists, oldest first. The slice is the
// Data's own: read it, do not change it.
func (d *Data) Terminals() []proto.Terminal { return d.terminals }

// Terminal returns the listed shell id.
func (d *Data) Terminal(id string) (proto.Terminal, bool) {
	for _, t := range d.terminals {
		if t.ID == id {
			return t, true
		}
	}
	return proto.Terminal{}, false
}

// LiveTerminals counts the shells that have not exited: what the sidebar's
// Terminal entry shows, and what pocketd's cap counts.
func (d *Data) LiveTerminals() int {
	n := 0
	for _, t := range d.terminals {
		if !t.Exited {
			n++
		}
	}
	return n
}

// TerminalsGone are the shells of open the list no longer has: ended
// elsewhere, or exited and dropped. They leave the window, with their
// scrollback. Only a loaded list says so: one that failed or is loading
// may lack a shell that runs.
func (d *Data) TerminalsGone(open []string) []string {
	if d.terminalsList.status != ListLoaded {
		return nil
	}
	var out []string
	for _, id := range open {
		_, awaited := d.terminalsAwaited[id]
		if _, ok := d.Terminal(id); !ok && !awaited {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// TerminalSelection is the shell to show: sel while it is listed, else
// the newest listed, "" when none is.
func TerminalSelection(list []proto.Terminal, sel string) string {
	if slices.ContainsFunc(list, func(t proto.Terminal) bool { return t.ID == sel }) {
		return sel
	}
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1].ID
}

// TerminalAfterEnd is the shell to show once the owner ended id: the
// newest other one listed, "" when none is.
func TerminalAfterEnd(list []proto.Terminal, id string) string {
	next := ""
	for _, t := range list {
		if t.ID != id {
			next = t.ID
		}
	}
	return next
}

// TerminalTitle is a shell's tab: the title its shell set (as
// "team@team: ~/projects/x"), else the name of its folder.
func TerminalTitle(t proto.Terminal, set string) string {
	if set != "" {
		return set
	}
	return t.Title
}

// TerminalEndNote says how a shell's connection ended: the shell exited
// with a code, or the connection to it was lost (code below 0).
func TerminalEndNote(code int) string {
	if code < 0 {
		return "Disconnected from the shell."
	}
	return fmt.Sprintf("The shell exited (code %d).", code)
}

// SafeLink says whether a link a shell printed may open: http or https
// with a host, or mailto, and nothing else (no file:, smb: or an app's own
// scheme). What a shell prints is the server's, not the owner's word.
func SafeLink(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host != ""
	case "mailto":
		return true
	}
	return false
}

// TerminalPlace says where a new shell starts: the service user's home
// (all empty), a session's folder, or a path.
type TerminalPlace struct {
	Session int64
	Path    string
}

func (p TerminalPlace) request() proto.StartTerminal {
	switch {
	case p.Session != 0:
		return proto.StartTerminal{Folder: proto.TerminalSession, Session: p.Session}
	case p.Path != "":
		return proto.StartTerminal{Folder: proto.TerminalPath, Path: p.Path}
	}
	return proto.StartTerminal{Folder: proto.TerminalHome}
}

// startTerminalKey keys a start's request id: a start tried again
// unchanged after a failure keeps it, so a lost answer starts no second
// shell.
const startTerminalKey = "terminal:start"

// CanStartTerminal says whether the owner may start a shell now, and why
// not: one is starting, or pocketd runs its cap.
func (s *Store) CanStartTerminal() (bool, string) {
	switch {
	case s.startingTerminal.Busy:
		return false, "A shell is starting"
	case s.Data.LiveTerminals() >= proto.MaxTerminals:
		return false, fmt.Sprintf("pocketd runs %d terminals at most: end one first", proto.MaxTerminals)
	}
	return true, ""
}

// StartingTerminal is where the owner's start of a shell stands.
func (s *Store) StartingTerminal() Pending { return s.startingTerminal }

// StartTerminal starts a shell at place, rows by cols (POST
// /v1/terminals). done runs on the UI thread with its id once pocketd
// started it; the list comes with its event, and a fetch.
func (s *Store) StartTerminal(place TerminalPlace, rows, cols int, done func(id string, err error)) {
	if ok, _ := s.CanStartTerminal(); !ok {
		return
	}
	request := place.request()
	request.Rows, request.Cols = rows, cols
	request.RequestID = s.ids.For(startTerminalKey, fmt.Sprintf("%s|%d|%s|%dx%d", request.Folder, request.Session, request.Path, rows, cols))
	s.startingTerminal = Pending{Busy: true}
	s.log(LevelCommand, "New terminal")
	var started proto.TerminalStarted
	s.do("Starting a terminal", func(ctx context.Context, c *proto.Client) (err error) {
		started, err = c.StartTerminal(ctx, request)
		return err
	}, func(err error) {
		s.startingTerminal = Pending{}
		if err != nil {
			s.startingTerminal.Err = err.Error()
		} else {
			s.ids.Done(startTerminalKey)
			s.Data.awaitTerminal(started.ID)
		}
		s.loadTerminals()
		if done != nil {
			done(started.ID, err)
		}
	})
}

// EndTerminal ends shell id (DELETE /v1/terminals/{id}): pocketd ends its
// shell, which leaves the list. One pocketd no longer has is gone too.
func (s *Store) EndTerminal(id string) {
	s.log(LevelCommand, "End the terminal")
	s.do("Ending the terminal", func(ctx context.Context, c *proto.Client) error {
		return c.EndTerminal(ctx, id)
	}, func(error) { s.loadTerminals() })
}

func (s *Store) loadTerminals() {
	f := s.Data.Begin()
	var list []proto.Terminal
	s.do("Loading the terminals", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Terminals(ctx)
		return err
	}, s.track("terminals", func(err error) {
		if err != nil {
			s.Data.SetTerminalsError(f, err.Error())
			return
		}
		s.Data.SetTerminals(f, list)
	}))
}

// LoadTerminals fetches the list of shells again, as Try Again does.
func (s *Store) LoadTerminals() { s.loadTerminals() }

// OpenTerminal connects to shell id, nil while signed out. The TermConn
// keeps the sign-in it was opened with: close it as the sign-in ends.
func (s *Store) OpenTerminal(id string) *TermConn {
	if s.client == nil {
		return nil
	}
	return newTermConn(s.client, id, s.opts.After)
}

// TermConn is a shell on pocketd as a connection a terminal emulator reads
// and writes: Read gives its output, Write sends keys, Resize its size
// (frostyard/pocket's, for pi-pocket-team's module). Closing it stops
// reading; pocketd ends the shell a minute later unless EndTerminal ends
// it at once. Its methods are safe from the emulator's goroutines: it
// touches no Store.
type TermConn struct {
	client *proto.Client
	id     string
	// tab names this connection to pocketd: its polls hold the shell back
	// as it draws, and its inputs are numbered by it.
	tab string

	ctx    context.Context
	cancel context.CancelFunc

	// Replay transforms the first output that comes before it reaches the
	// emulator. Set it before Read starts. A failure ends the connection.
	Replay func(data []byte, cols, rows int) ([]byte, error)

	mu         sync.Mutex
	pending    []byte
	replayed   bool
	cols, rows int
	// from is how much of the output has come, in pocketd's bytes.
	from     int64
	ended    bool
	exitCode int

	// seq numbers the inputs, so one sent again is written once; wmu sends
	// them one at a time, in order.
	wmu sync.Mutex
	seq int64
	// after and now are the clock of the waits between tries and of a
	// send's deadline; retry is the wait.
	after func(time.Duration) <-chan time.Time
	now   func() time.Time
	retry time.Duration
}

// sendFor is how long keys are sent again while the network fails.
const sendFor = 30 * time.Second

func newTermConn(client *proto.Client, id string, after func(time.Duration) <-chan time.Time) *TermConn {
	if after == nil {
		after = time.After
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	ctx, cancel := context.WithCancel(context.Background())
	return &TermConn{client: client, id: id, tab: hex.EncodeToString(b[:]), ctx: ctx, cancel: cancel,
		after: after, now: time.Now, retry: time.Second, exitCode: -1, cols: 80, rows: 24}
}

// ID is the shell's id.
func (t *TermConn) ID() string { return t.id }

// ExitCode is the shell's exit code once Read returned io.EOF for its
// exit; -1 when the connection ended without one (a disconnect, a
// sign-out, a shell that went).
func (t *TermConn) ExitCode() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitCode
}

// Read gives the shell's output as it comes, waiting for it; io.EOF once
// the shell exited or went, or the connection closed.
func (t *TermConn) Read(p []byte) (int, error) {
	for {
		t.mu.Lock()
		if len(t.pending) > 0 {
			n := copy(p, t.pending)
			t.pending = t.pending[n:]
			t.mu.Unlock()
			return n, nil
		}
		ended, from := t.ended, t.from
		t.mu.Unlock()
		if ended || t.ctx.Err() != nil {
			return 0, io.EOF
		}
		out, gone, err := t.poll(from)
		if err != nil {
			if t.ctx.Err() != nil {
				return 0, io.EOF
			}
			if Fatal(err) {
				return 0, err
			}
			// The network or pocketd failed: wait and poll again. pocketd
			// keeps the output, so nothing is lost.
			select {
			case <-t.ctx.Done():
				return 0, io.EOF
			case <-t.after(t.retry):
			}
			continue
		}
		data := out.Data
		if !t.replayed {
			t.replayed = true
			if t.Replay != nil && !gone {
				t.mu.Lock()
				cols, rows := t.cols, t.rows
				t.mu.Unlock()
				if data, err = t.Replay(data, cols, rows); err != nil {
					t.cancel()
					return 0, err
				}
			}
		}
		t.mu.Lock()
		switch {
		case gone:
			t.ended, t.exitCode = true, -1
		default:
			t.pending = append(t.pending, data...)
			t.from = max(t.from, out.To)
			if out.Exited {
				t.ended, t.exitCode = true, -1
				if out.ExitCode != nil {
					t.exitCode = *out.ExitCode
				}
			}
		}
		t.mu.Unlock()
	}
}

// poll waits for output past from; gone says pocketd no longer has the
// shell.
func (t *TermConn) poll(from int64) (out proto.TerminalOutput, gone bool, err error) {
	ctx, cancel := context.WithTimeout(t.ctx, 40*time.Second)
	defer cancel()
	// What the connection received is what it drew: the emulator takes
	// what Read gives at once.
	out, err = t.client.TerminalOutput(ctx, t.id, from, from, t.tab)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.ErrorCodeNotFound {
		return proto.TerminalOutput{}, true, nil
	}
	return out, false, err
}

// Write sends keys to the shell, in inputs pocketd takes, each sent again
// until it lands.
func (t *TermConn) Write(p []byte) (int, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	for data := p; len(data) > 0; {
		n := min(len(data), proto.MaxTerminalInput)
		chunk := data[:n]
		data = data[n:]
		t.seq++
		if err := t.send(proto.TerminalInput{Tab: t.tab, Seq: t.seq, Data: chunk}); err != nil {
			// The keys did not land, and the emulator takes no more after
			// a failed write: end the connection, so the view says so
			// rather than taking keys that go nowhere.
			t.cancel()
			return 0, err
		}
	}
	return len(p), nil
}

// send posts an input, trying again for sendFor while the network fails:
// its number makes a repeat harmless. pocketd's answer, or a sign-out,
// ends it.
func (t *TermConn) send(input proto.TerminalInput) error {
	deadline := t.now().Add(sendFor)
	for {
		ctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
		err := t.client.TerminalInput(ctx, t.id, input)
		cancel()
		var pe *proto.Error
		switch {
		case err == nil:
			return nil
		case errors.As(err, &pe), Fatal(err), t.ctx.Err() != nil, t.now().After(deadline):
			return err
		}
		select {
		case <-t.ctx.Done():
			return io.ErrClosedPipe
		case <-t.after(t.retry):
		}
	}
}

// Resize tells the shell its size, as the emulator lays it out.
func (t *TermConn) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer cancel()
	return t.client.ResizeTerminal(ctx, t.id, proto.ResizeTerminal{Rows: rows, Cols: cols})
}

// Close stops reading the shell.
func (t *TermConn) Close() error {
	t.cancel()
	return nil
}
