//go:build linux

package terminal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// harness runs real shells (/bin/sh as a login shell, or a silent cat) on
// a clock that moves only when told to. A check (Tick) runs only when a
// test runs it; an ended shell's SIGKILL and close run at once.
type harness struct {
	m    *Manager
	home string

	mu       sync.Mutex
	now      time.Time
	changes  [][]proto.Terminal
	deadline chan time.Time
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), home: t.TempDir()}
	if opts.Shell == "" {
		opts.Shell = "/bin/sh"
	}
	opts.Home = h.home
	if opts.Env == nil {
		opts.Env = Environment([]string{"PATH=" + os.Getenv("PATH")}, h.home)
	}
	opts.Now = h.clock
	opts.After = func(d time.Duration) <-chan time.Time {
		if d == PollWait {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.deadline
		}
		return nil
	}
	opts.Timer = func(d time.Duration, f func()) func() {
		if d != Tick {
			go f()
		}
		return func() {}
	}
	opts.OnChange = func(list []proto.Terminal) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.changes = append(h.changes, list)
	}
	h.m = New(opts)
	t.Cleanup(h.m.Close)
	return h
}

// silent is a "shell" that prints nothing unless it is typed to: a poll of
// it waits.
func silent(t *testing.T) Options {
	path, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	return Options{Shell: path}
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *harness) lastChange() []proto.Terminal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changes[len(h.changes)-1]
}

// bound is how long a test waits for a shell before it fails.
func bound(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// read polls shell id from *from until its output holds want, and returns
// what came; *from moves on.
func (h *harness) read(t *testing.T, id string, from *int64, want string) string {
	t.Helper()
	ctx := bound(t)
	var got []byte
	for !bytes.Contains(got, []byte(want)) {
		out, err := h.m.Poll(ctx, id, "", *from, *from)
		if err != nil {
			t.Fatalf("poll: %v (after %q)", err, got)
		}
		if ctx.Err() != nil {
			t.Fatalf("no %q in %q", want, got)
		}
		got = append(got, out.Data...)
		*from = out.To
		if out.Exited && !bytes.Contains(got, []byte(want)) {
			t.Fatalf("the shell exited without %q: %q", want, got)
		}
	}
	return string(got)
}

// exit polls shell id until it exited, and returns its code.
func (h *harness) exit(t *testing.T, id string) *int {
	t.Helper()
	ctx := bound(t)
	var from int64
	for {
		out, err := h.m.Poll(ctx, id, "", from, from)
		if err != nil || ctx.Err() != nil {
			t.Fatalf("poll: %v, %v", err, ctx.Err())
		}
		from = out.To
		if out.Exited {
			return out.ExitCode
		}
	}
}

func (h *harness) start(t *testing.T, cwd string, rows, cols int) proto.Terminal {
	t.Helper()
	info, err := h.m.Start(cwd, rows, cols)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func (h *harness) term(id string) *term {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.terms[id]
}

// waitPolled waits until a poll of shell id waits.
func (h *harness) waitPolled(t *testing.T, id string) {
	ctx := bound(t)
	for {
		h.m.mu.Lock()
		polls := h.m.terms[id].polls
		h.m.mu.Unlock()
		if polls > 0 {
			return
		}
		if ctx.Err() != nil {
			t.Fatal("no poll waits")
		}
		runtime.Gosched()
	}
}

func TestAShellStartsInItsFolderTakesKeysAndFollowsItsSize(t *testing.T) {
	h := newHarness(t, Options{})
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info := h.start(t, dir, 30, 100)
	if info.Title != "project" || info.Cwd != dir || info.Exited {
		t.Fatalf("listed as %+v", info)
	}
	if list := h.lastChange(); len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("the start announced %+v", list)
	}
	var from int64
	if err := h.m.Input(info.ID, "tab-1", 1, []byte("printf 'at=%s\\n' \"$PWD\"; stty size\n")); err != nil {
		t.Fatal(err)
	}
	got := h.read(t, info.ID, &from, "30 100")
	if !strings.Contains(got, "at="+dir) {
		t.Fatalf("the shell is not in its folder: %q", got)
	}
	if err := h.m.Resize(info.ID, 33, 101); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Input(info.ID, "tab-1", 2, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	h.read(t, info.ID, &from, "33 101")
	for _, err := range []error{h.m.Resize(info.ID, 1, 80), h.m.Input(info.ID, "tab 1", 3, nil), h.m.Input(info.ID, "tab-1", 0, nil),
		h.m.Input(info.ID, "tab-1", 3, make([]byte, InputMax+1))} {
		var b *BadRequest
		if !errors.As(err, &b) {
			t.Errorf("a bad call is %v", err)
		}
	}
	for _, call := range []func() error{
		func() error { _, err := h.m.Poll(bound(t), info.ID, "", 1, 2); return err },
		func() error { _, err := h.m.Poll(bound(t), info.ID, "a.b", 1, 1); return err },
	} {
		var b *BadRequest
		if err := call(); !errors.As(err, &b) {
			t.Errorf("a bad poll is %v", err)
		}
	}
}

// pi-pocket-team's rule: a client numbers its inputs, and one whose number
// is not above the last it sent is written no second time.
func TestKeysAreWrittenOnce(t *testing.T) {
	h := newHarness(t, Options{})
	info := h.start(t, h.home, 24, 80)
	keys := []byte("printf 'one-%s\\n' 1\n")
	for _, seq := range []int64{1, 1} {
		if err := h.m.Input(info.ID, "tab-1", seq, keys); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.m.Input(info.ID, "tab-2", 1, []byte("printf 'two-%s\\n' 2\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Input(info.ID, "tab-1", 2, []byte("printf 'end-%s\\n' 3\n")); err != nil {
		t.Fatal(err)
	}
	var from int64
	got := h.read(t, info.ID, &from, "end-3")
	if strings.Count(got, "one-1") != 1 || !strings.Contains(got, "two-2") {
		t.Fatalf("the keys went in as %q", got)
	}
}

// TabsMax clients may type into a shell; another waits until one has been
// quiet for ReceiptSpan.
func TestTheClientsThatTypeAreBounded(t *testing.T) {
	h := newHarness(t, silent(t))
	info := h.start(t, h.home, 24, 80)
	for i := range TabsMax {
		if err := h.m.Input(info.ID, "tab-"+string(rune('a'+i)), 1, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.m.Input(info.ID, "tab-late", 1, nil); !errors.Is(err, ErrTooManyTabs) {
		t.Fatalf("a client past the bound: %v", err)
	}
	h.advance(ReceiptSpan)
	if err := h.m.Input(info.ID, "tab-late", 1, nil); err != nil {
		t.Fatalf("a client after the others went quiet: %v", err)
	}
}

func TestFourShellsRunAtOnce(t *testing.T) {
	h := newHarness(t, Options{})
	var first proto.Terminal
	for i := range Cap {
		info := h.start(t, h.home, 24, 80)
		if i == 0 {
			first = info
		}
	}
	if _, err := h.m.Start(h.home, 24, 80); !errors.Is(err, ErrFull) {
		t.Fatalf("a fifth start: %v", err)
	}
	if err := h.m.Input(first.ID, "tab-1", 1, []byte("exit 0\n")); err != nil {
		t.Fatal(err)
	}
	h.exit(t, first.ID)
	// A shell that exited stays listed, and takes no place.
	if list := h.m.List(); len(list) != Cap || !list[0].Exited {
		t.Fatalf("listed %+v", list)
	}
	h.start(t, h.home, 24, 80)
}

func TestAShellThatExitsKeepsItsCode(t *testing.T) {
	h := newHarness(t, Options{})
	info := h.start(t, h.home, 24, 80)
	if err := h.m.Input(info.ID, "tab-1", 1, []byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}
	if code := h.exit(t, info.ID); code == nil || *code != 7 {
		t.Fatalf("exit code %v", code)
	}
	if list := h.lastChange(); len(list) != 1 || !list[0].Exited || *list[0].ExitCode != 7 {
		t.Fatalf("the exit announced %+v", list)
	}
	if err := h.m.Input(info.ID, "tab-1", 2, []byte("x")); !errors.Is(err, ErrEnded) {
		t.Fatalf("keys to an exited shell: %v", err)
	}
	if err := h.m.Resize(info.ID, 30, 90); !errors.Is(err, ErrEnded) {
		t.Fatalf("a size for an exited shell: %v", err)
	}
	h.advance(Grace - time.Second)
	h.m.Check()
	if len(h.m.List()) != 1 {
		t.Fatal("an exited shell left the list before its grace")
	}
	h.advance(time.Second)
	h.m.Check()
	if len(h.m.List()) != 0 || len(h.lastChange()) != 0 {
		t.Fatalf("an exited shell stays past its grace: %+v", h.m.List())
	}
}

func TestEndEndsTheShell(t *testing.T) {
	h := newHarness(t, silent(t))
	info := h.start(t, h.home, 24, 80)
	tm := h.term(info.ID)
	waiting := make(chan error, 1)
	go func() {
		_, err := h.m.Poll(bound(t), info.ID, "", 0, 0)
		waiting <- err
	}()
	h.waitPolled(t, info.ID)
	if err := h.m.End(info.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-waiting; !errors.Is(err, ErrNotFound) {
		t.Fatalf("a poll that waited on it: %v", err)
	}
	if len(h.m.List()) != 0 || len(h.lastChange()) != 0 {
		t.Fatal("an ended shell stays listed")
	}
	select {
	case <-tm.done:
	case <-bound(t).Done():
		t.Fatal("the shell runs on")
	}
	for _, err := range []error{h.m.End(info.ID), h.m.Input(info.ID, "tab-1", 1, nil), h.m.Resize(info.ID, 30, 90)} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("a call to an ended shell: %v", err)
		}
	}
	if _, err := h.m.Poll(bound(t), info.ID, "", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("a poll of an ended shell: %v", err)
	}
}

// A shell no poll asked for DetachAfter ends, but not while a poll waits;
// one that took no keys for IdleAfter ends too, however often it is
// polled.
func TestADetachedOrIdleShellEnds(t *testing.T) {
	h := newHarness(t, silent(t))
	h.start(t, h.home, 24, 80)
	h.advance(DetachAfter - time.Second)
	h.m.Check()
	if len(h.m.List()) != 1 {
		t.Fatal("ended before its detach span")
	}
	h.advance(time.Second)
	h.m.Check()
	if len(h.m.List()) != 0 {
		t.Fatal("a shell nobody polls runs on")
	}

	watched := h.start(t, h.home, 24, 80)
	ctx, cancel := context.WithCancel(bound(t))
	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		_, _ = h.m.Poll(ctx, watched.ID, "", 0, 0)
	}()
	h.waitPolled(t, watched.ID)
	h.advance(2 * DetachAfter)
	h.m.Check()
	if len(h.m.List()) != 1 {
		t.Fatal("a shell ended while a poll waited on it")
	}
	cancel()
	<-waiting
	done, stop := context.WithCancel(context.Background())
	stop()
	elapsed := 2 * DetachAfter
	for step := 50 * time.Second; elapsed+step < IdleAfter; elapsed += step {
		h.advance(step)
		if _, err := h.m.Poll(done, watched.ID, "", 0, 0); err != nil {
			t.Fatalf("polled, the shell ended %s after its last keys: %v", elapsed+step, err)
		}
		h.m.Check()
	}
	h.advance(IdleAfter - elapsed)
	h.m.Check()
	if len(h.m.List()) != 0 {
		t.Fatal("an idle shell runs on")
	}
}

func TestAShellSeesOnlyItsEnvironment(t *testing.T) {
	h := newHarness(t, Options{Env: Environment([]string{"PATH=" + os.Getenv("PATH"), "SECRET=hush", "POCKETD_DATA=/x"}, "/")})
	info := h.start(t, h.home, 24, 80)
	if err := h.m.Input(info.ID, "tab-1", 1, []byte("printf 'secret=[%s%s] term=%s\\n' \"$SECRET\" \"$POCKETD_DATA\" \"$TERM\"\n")); err != nil {
		t.Fatal(err)
	}
	var from int64
	h.read(t, info.ID, &from, "secret=[] term=xterm-256color")
}

// pi-pocket-team's back pressure: a client that drew little holds the
// shell, which then loses no byte of its output for the client as it
// keeps up.
func TestASlowClientHoldsTheShellAndLosesNothing(t *testing.T) {
	h := newHarness(t, Options{})
	info := h.start(t, h.home, 24, 80)
	const size = 3 * Keep
	// The slow client has drawn nothing.
	if _, err := h.m.Poll(bound(t), info.ID, "slow", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Input(info.ID, "tab-1", 1, []byte("head -c 786432 /dev/zero | tr '\\0' x; printf 'done-%s\\n' 1\n")); err != nil {
		t.Fatal(err)
	}
	tm := h.term(info.ID)
	ctx := bound(t)
	for {
		h.m.mu.Lock()
		paused, total, changed := tm.paused, tm.total, tm.changed
		h.m.mu.Unlock()
		if paused {
			if total > Keep/2+64*1024 {
				t.Fatalf("paused at %d bytes, past the line", total)
			}
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("never paused (at %d bytes)", total)
		}
	}
	// The slow client draws all it has, then keeps up: every byte comes.
	var from int64
	var got []byte
	for !bytes.Contains(got, []byte("done-1")) {
		out, err := h.m.Poll(ctx, info.ID, "slow", from, from)
		if err != nil || ctx.Err() != nil {
			t.Fatalf("poll: %v, %v", err, ctx.Err())
		}
		if out.From != from {
			t.Fatalf("lost the output from %d to %d", from, out.From)
		}
		got = append(got, out.Data...)
		from = out.To
	}
	if n := bytes.Count(got, []byte("x")); n < size {
		t.Fatalf("%d of the %d bytes came", n, size)
	}
}

// A shell that a slow client holds back, and that a signal ends, still
// exits: what it left is read, whatever the client drew.
func TestAShellEndedWhileHeldBackExits(t *testing.T) {
	h := newHarness(t, Options{})
	info := h.start(t, h.home, 24, 80)
	if _, err := h.m.Poll(bound(t), info.ID, "slow", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Input(info.ID, "tab-1", 1, []byte("head -c 786432 /dev/zero | tr '\\0' x\n")); err != nil {
		t.Fatal(err)
	}
	tm := h.term(info.ID)
	ctx := bound(t)
	for {
		h.m.mu.Lock()
		paused, changed := tm.paused, tm.changed
		h.m.mu.Unlock()
		if paused {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("never paused")
		}
	}
	tm.sh.signal(kill)
	if code := h.exit(t, info.ID); code == nil || *code != 128+int(kill) {
		t.Fatalf("exit code %v", code)
	}
}

func TestAPollAnswersNothingAtItsDeadline(t *testing.T) {
	h := newHarness(t, silent(t))
	info := h.start(t, h.home, 24, 80)
	deadline := make(chan time.Time)
	close(deadline)
	h.mu.Lock()
	h.deadline = deadline
	h.mu.Unlock()
	out, err := h.m.Poll(bound(t), info.ID, "tab-1", 5, 5)
	if err != nil || out.From != 0 || out.To != 0 || len(out.Data) != 0 || out.Exited {
		t.Fatalf("at the deadline: %+v, %v", out, err)
	}
}

func TestCloseEndsEveryShellAndStartsNoMore(t *testing.T) {
	h := newHarness(t, Options{})
	a, b := h.start(t, h.home, 24, 80), h.start(t, h.home, 24, 80)
	ta, tb := h.term(a.ID), h.term(b.ID)
	h.m.Close()
	for _, tm := range []*term{ta, tb} {
		select {
		case <-tm.done:
		default:
			t.Fatal("Close came back before a shell ended")
		}
	}
	if len(h.m.List()) != 0 {
		t.Fatal("shells stay listed")
	}
	if _, err := h.m.Start(h.home, 24, 80); !errors.Is(err, ErrClosed) {
		t.Fatalf("a start after Close: %v", err)
	}
}
