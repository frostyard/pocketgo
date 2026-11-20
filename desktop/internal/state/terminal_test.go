package state

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func shellInfo(id string, exited bool) proto.Terminal {
	return proto.Terminal{ID: id, Title: "team", Cwd: pocketdtest.HomeFolder, StartedAt: pocketdtest.Now, Exited: exited}
}

func shellIDs(list []proto.Terminal) []string {
	var out []string
	for _, t := range list {
		out = append(out, t.ID)
	}
	return out
}

// A terminals event carries the whole list, and wins over a fetch begun
// before it, whether the fetch answers or fails; the answer of a fetch
// begun before one applied is dropped.
func TestTheTerminalListFollowsItsEventsAndFetches(t *testing.T) {
	var d Data
	if status, _ := d.TerminalsStatus(); status != ListLoading || d.TerminalsGone([]string{"a"}) != nil {
		t.Fatal("a list never loaded says what is gone")
	}
	f1 := d.Begin()
	d.Apply(proto.Event{Type: proto.EventTerminals, Terminals: []proto.Terminal{shellInfo("a", false)}})
	d.SetTerminals(f1, nil)
	if got := shellIDs(d.Terminals()); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a fetch begun before the event undid it: %v", got)
	}
	f2 := d.Begin()
	d.SetTerminals(f2, []proto.Terminal{shellInfo("a", false), shellInfo("b", true)})
	if got := shellIDs(d.Terminals()); !slices.Equal(got, []string{"a", "b"}) || d.LiveTerminals() != 1 {
		t.Fatalf("after a fetch: %v, %d live", got, d.LiveTerminals())
	}
	f3 := d.Begin()
	d.SetTerminalsError(f3, "boom")
	if status, err := d.TerminalsStatus(); status != ListFailed || err != "boom" || d.TerminalsGone([]string{"x"}) != nil {
		t.Fatalf("a failed fetch: %v %q", status, err)
	}
	f4 := d.Begin()
	d.Apply(proto.Event{Type: proto.EventTerminals})
	d.SetTerminalsError(f4, "late")
	if status, _ := d.TerminalsStatus(); status != ListLoaded || len(d.Terminals()) != 0 {
		t.Fatalf("a fetch that failed after an event: %v, %v", status, d.Terminals())
	}
	f5, f6 := d.Begin(), d.Begin()
	d.SetTerminals(f6, []proto.Terminal{shellInfo("c", false)})
	d.SetTerminals(f5, []proto.Terminal{shellInfo("d", false)})
	if got := shellIDs(d.Terminals()); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("an older fetch undid a newer one: %v", got)
	}
	if got := d.TerminalsGone([]string{"x", "c", "a"}); !slices.Equal(got, []string{"a", "x"}) {
		t.Fatalf("gone: %v", got)
	}
}

// The start's answer may come before the event that lists its shell: the
// shell stays selected, and its window open, until a list brings it, or a
// fetch begun after the start says it is gone.
func TestAStartedShellStaysShownUntilTheListHasIt(t *testing.T) {
	var d Data
	d.SetTerminals(d.Begin(), []proto.Terminal{shellInfo("a", false)})
	stale := d.Begin()
	d.awaitTerminal("b")
	if d.TerminalSelection("b") != "b" || d.TerminalsGone([]string{"b"}) != nil {
		t.Fatal("a shell just started is not shown")
	}
	d.SetTerminals(stale, []proto.Terminal{shellInfo("a", false)})
	if d.TerminalSelection("b") != "b" {
		t.Fatal("a fetch begun before the start dropped its shell")
	}
	d.SetTerminals(d.Begin(), []proto.Terminal{shellInfo("a", false)})
	if d.TerminalSelection("b") != "a" || !slices.Equal(d.TerminalsGone([]string{"b"}), []string{"b"}) {
		t.Fatal("a shell a later fetch does not list stays")
	}
	d.awaitTerminal("c")
	d.Apply(proto.Event{Type: proto.EventTerminals, Terminals: []proto.Terminal{shellInfo("a", false), shellInfo("c", false)}})
	d.Apply(proto.Event{Type: proto.EventTerminals, Terminals: []proto.Terminal{shellInfo("a", false)}})
	if d.TerminalSelection("c") != "a" {
		t.Fatal("a shell that was listed, then ended, stays")
	}
}

func TestWhichShellShowsAndHowItEnded(t *testing.T) {
	list := []proto.Terminal{shellInfo("a", false), shellInfo("b", false), shellInfo("c", true)}
	for sel, want := range map[string]string{"b": "b", "z": "c", "": "c"} {
		if got := TerminalSelection(list, sel); got != want {
			t.Errorf("selection of %q: %q, want %q", sel, got, want)
		}
	}
	if TerminalSelection(nil, "a") != "" || TerminalAfterEnd(list, "c") != "b" || TerminalAfterEnd(list, "a") != "c" || TerminalAfterEnd(list[:1], "a") != "" {
		t.Fatal("the shell shown after another")
	}
	if TerminalTitle(list[0], "") != "team" || TerminalTitle(list[0], "team@team: ~/projects/x") != "team@team: ~/projects/x" {
		t.Fatal("a tab's title")
	}
	if TerminalEndNote(3) != "The shell exited (code 3)." || TerminalEndNote(-1) != "Disconnected from the shell." {
		t.Fatal("how a shell ended")
	}
}

// frostyard/pocket#3's guard, blocking point 1: a link a shell prints
// opens only on the web or in mail, never as a file or another program's
// address.
func TestOnlyWebAndMailLinksOpenFromAShell(t *testing.T) {
	for link, want := range map[string]bool{
		"https://example.org/x":      true,
		"http://example.org":         true,
		"mailto:someone@example.org": true,
		"file:///etc/passwd":         false,
		"smb://example.org/share":    false,
		"vscode://open?x":            false,
		"https:no-host":              false,
		"/etc/hosts":                 false,
		"%zz":                        false,
	} {
		if got := SafeLink(link); got != want {
			t.Errorf("SafeLink(%q) = %v, want %v", link, got, want)
		}
	}
}

// A start whose answer was lost, tried again unchanged, keeps its request
// id: pocketd answers the shell it started, and starts no second.
func TestAStartTriedAgainStartsOneShell(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	h.srv.LoseAnswerOnce(http.MethodPost, proto.PathTerminals)
	var got []string
	var errs []error
	start := func() {
		h.store.StartTerminal(TerminalPlace{}, 30, 100, func(id string, err error) { got, errs = append(got, id), append(errs, err) })
		h.settle(t)
	}
	start()
	if errs[0] == nil || h.store.StartingTerminal().Err == "" {
		t.Fatal("a lost answer is a success")
	}
	start()
	if errs[1] != nil || got[1] != "s1" || len(h.srv.Terminals()) != 1 {
		t.Fatalf("the retry: %q, %v; pocketd lists %v", got[1], errs[1], shellIDs(h.srv.Terminals()))
	}
	posts := h.srv.RequestsTo(http.MethodPost, proto.PathTerminals)
	if len(posts) != 2 || !strings.Contains(string(posts[0].Body), requestIDOf(t, posts[1].Body)) {
		t.Fatalf("the retry's request id differs: %s / %s", posts[0].Body, posts[1].Body)
	}
	if got := shellIDs(h.store.Data.Terminals()); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("the client lists %v", got)
	}
	// After a success, a start is a new request.
	start()
	if got[2] != "s2" || len(h.srv.Terminals()) != 2 {
		t.Fatalf("a second start: %q", got[2])
	}
	for len(h.srv.Terminals()) < proto.MaxTerminals {
		start()
	}
	if ok, why := h.store.CanStartTerminal(); ok || !strings.Contains(why, "4 terminals") {
		t.Fatalf("a fifth start is offered: %v %q", ok, why)
	}
	h.store.EndTerminal("s1")
	h.settle(t)
	if ok, _ := h.store.CanStartTerminal(); !ok || len(h.store.Data.Terminals()) != 3 {
		t.Fatalf("after an end: %v", shellIDs(h.store.Data.Terminals()))
	}
}

func requestIDOf(t *testing.T, body []byte) string {
	t.Helper()
	_, rest, ok := strings.Cut(string(body), `"requestId":"`)
	id, _, _ := strings.Cut(rest, `"`)
	if !ok || id == "" {
		t.Fatalf("no request id in %s", body)
	}
	return id
}

// dropOnce passes the first request to its path to the fake, which takes
// it, then fails as a dropped connection does: the answer is lost.
type dropOnce struct {
	base    http.RoundTripper
	path    string
	mu      sync.Mutex
	dropped bool
}

var errDropped = errors.New("connection reset by peer")

func (d *dropOnce) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(r)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil && r.URL.Path == d.path && !d.dropped {
		d.dropped = true
		_ = resp.Body.Close()
		return nil, errDropped
	}
	return resp, err
}

// connTo connects to shell id of the harness's pocketd, on a clock of its
// own: a wait between tries ends at once, and a send's deadline never
// passes.
func connTo(t *testing.T, s *Store, id string) *TermConn {
	t.Helper()
	conn := s.OpenTerminal(id)
	fired := make(chan time.Time)
	close(fired)
	conn.after = func(time.Duration) <-chan time.Time { return fired }
	conn.now = func() time.Time { return pocketdtest.Now }
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// open is connTo, with the reads coming on got.
func open(t *testing.T, s *Store, id string) (*TermConn, <-chan string) {
	t.Helper()
	conn := connTo(t, s, id)
	got := make(chan string, 8)
	go func() {
		defer close(got)
		buf := make([]byte, 64)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				got <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return conn, got
}

// frostyard/pocket's TermConn: the output in order, from where pocketd's
// answer says; keys sent again after a lost answer, with their number, so
// the shell takes them once; and the exit code at the end.
func TestTermConnReadsWritesAndEnds(t *testing.T) {
	h := newHarness(t)
	drop := &dropOnce{base: h.srv.Transport(), path: proto.Path(proto.PathTerminalInput, "s1")}
	h.store = h.newStore(t, drop)
	h.srv.AddTerminal(shellInfo("s1", false))
	h.connect(t)
	conn, got := open(t, h.store, "s1")
	h.srv.PrintTerminal("s1", "é🙂 $ ")
	if s := <-got; s != "é🙂 $ " {
		t.Fatalf("first read %q", s)
	}
	if _, err := conn.Write([]byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if posts := h.srv.RequestsTo(http.MethodPost, drop.path); len(posts) != 2 || h.srv.TerminalKeys("s1") != "ls\r" {
		t.Fatalf("%d posts took %q: keys sent once, or taken twice", len(posts), h.srv.TerminalKeys("s1"))
	}
	if sizes := h.srv.TerminalSizes("s1"); !slices.Equal(sizes, [][2]int{{30, 100}}) {
		t.Fatalf("sizes %v", sizes)
	}
	h.srv.PrintTerminal("s1", "file.txt\r\n")
	if s := <-got; s != "file.txt\r\n" {
		t.Fatalf("second read %q", s)
	}
	outputs := h.srv.RequestsTo(http.MethodGet, proto.Path(proto.PathTerminalOutput, "s1"))
	if len(outputs) < 2 {
		t.Fatalf("polled %d times", len(outputs))
	}
	h.srv.ExitTerminal("s1", 3)
	for s := range got {
		t.Errorf("read %q after the shell exited", s)
	}
	if code := conn.ExitCode(); code != 3 {
		t.Fatalf("exit code %d", code)
	}
}

// A shell pocketd no longer has, and keys it refuses, end the connection
// with no exit code, so the view says it is disconnected rather than
// taking keys that go nowhere. A poll that fails is tried again.
func TestTermConnEndsWhenTheShellGoes(t *testing.T) {
	t.Run("gone", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddTerminal(shellInfo("s1", false))
		h.connect(t)
		h.srv.FailOnce(http.MethodGet, proto.Path(proto.PathTerminalOutput, "s1"), http.StatusBadGateway)
		conn, got := open(t, h.store, "s1")
		h.srv.PrintTerminal("s1", "after a failure\r\n")
		if s := <-got; s != "after a failure\r\n" {
			t.Fatalf("read %q", s)
		}
		h.srv.EndTerminal("s1")
		for s := range got {
			t.Errorf("read %q from a shell that went", s)
		}
		if code := conn.ExitCode(); code != -1 {
			t.Fatalf("exit code %d, want -1", code)
		}
	})
	t.Run("refused keys", func(t *testing.T) {
		h := newHarness(t)
		h.srv.AddTerminal(shellInfo("s1", true))
		h.connect(t)
		conn := connTo(t, h.store, "s1")
		if _, err := conn.Write([]byte("x")); err == nil {
			t.Fatal("refused keys were written")
		}
		if conn.ctx.Err() == nil {
			t.Fatal("the connection stays open after its keys were refused")
		}
		if posts := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathTerminalInput, "s1")); len(posts) != 1 || conn.ExitCode() != -1 {
			t.Fatalf("posts %d exit code %d: a refusal is not sent again, and ends with no code", len(posts), conn.ExitCode())
		}
		if _, err := conn.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
			t.Fatalf("a read after the refusal: %v", err)
		}
	})
}

// The first output, the history pocketd kept, goes through Replay once,
// at the size the view gave; a Replay that fails ends the connection with
// nothing of it shown.
func TestTermConnTransformsOnlyTheFirstOutputAndFailsClosed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			h := newHarness(t)
			h.srv.AddTerminal(shellInfo("s1", false))
			h.connect(t)
			conn := connTo(t, h.store, "s1")
			if err := conn.Resize(100, 30); err != nil {
				t.Fatal(err)
			}
			calls := 0
			errReplay := errors.New("replay failed")
			conn.Replay = func(data []byte, cols, rows int) ([]byte, error) {
				calls++
				if string(data) != "history" || cols != 100 || rows != 30 {
					t.Errorf("replay got %q at %dx%d", data, cols, rows)
				}
				if fail {
					return nil, errReplay
				}
				return []byte("snapshot"), nil
			}
			h.srv.PrintTerminal("s1", "history")
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if fail {
				if n != 0 || !errors.Is(err, errReplay) {
					t.Fatalf("failed replay leaked %q / %v", buf[:n], err)
				}
				if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
					t.Fatal("failed replay did not close the connection")
				}
				return
			}
			if err != nil || string(buf[:n]) != "snapshot" {
				t.Fatalf("replay %q / %v", buf[:n], err)
			}
			h.srv.PrintTerminal("s1", "live")
			n, err = conn.Read(buf)
			if err != nil || string(buf[:n]) != "live" || calls != 1 {
				t.Fatalf("live %q / %v; replay calls %d", buf[:n], err, calls)
			}
		})
	}
}

// The terminals load with the rest, and again when a stream comes back
// with the list not loaded.
func TestTheTerminalsLoadWithTheRest(t *testing.T) {
	h := newHarness(t)
	h.srv.AddTerminal(shellInfo("s1", false))
	h.srv.FailOnce(http.MethodGet, proto.PathTerminals, http.StatusInternalServerError)
	h.connect(t)
	if status, _ := h.store.Data.TerminalsStatus(); status != ListFailed {
		t.Fatalf("status %v", status)
	}
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	h.loop.RunUntil(t, func() bool {
		status, _ := h.store.Data.TerminalsStatus()
		return status == ListLoaded
	})
	if got := shellIDs(h.store.Data.Terminals()); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("listed %v", got)
	}
}
