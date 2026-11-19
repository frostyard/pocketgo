package state

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/frostui/frostuitest"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// fakeClock is Options.After for tests: a wait fires only when the test
// says, so no test sleeps.
type fakeClock struct {
	mu    sync.Mutex
	waits []fakeWait
}

type fakeWait struct {
	d  time.Duration
	ch chan time.Time
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, fakeWait{d: d, ch: ch})
	return ch
}

// Waiting is the waits asked for and not fired, in order.
func (c *fakeClock) Waiting() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, w := range c.waits {
		out = append(out, w.d)
	}
	return out
}

// Fire ends every wait.
func (c *fakeClock) Fire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waits {
		w.ch <- pocketdtest.Now
	}
	c.waits = nil
}

// harness is a store signed in to a fake pocketd, on a test loop.
type harness struct {
	srv       *pocketdtest.Server
	loop      *frostuitest.Loop
	clock     *fakeClock
	store     *Store
	logs      []string
	signedOut string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{srv: pocketdtest.New(), loop: frostuitest.NewLoop(), clock: &fakeClock{}}
	h.srv.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5", Reasoning: true})
	h.store = h.newStore(t, h.srv.Transport())
	return h
}

// newStore is a store of the harness's loop and clock over transport.
func (h *harness) newStore(t *testing.T, transport http.RoundTripper) *Store {
	t.Helper()
	s := New(Options{
		Post:      h.loop.Post,
		After:     h.clock.After,
		Transport: transport,
		Log:       func(_ Level, text string) { h.logs = append(h.logs, text) },
		SignedOut: func(_, reason string) { h.signedOut = reason },
	})
	t.Cleanup(s.Close)
	return s
}

// connect signs the store in with a kept token and waits until the stream
// is open and the first fetches are in.
func (h *harness) connect(t *testing.T) {
	t.Helper()
	if err := h.store.Connect(h.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	h.loop.RunUntil(t, func() bool {
		return h.store.Conn.State == Connected && h.store.Hello() != nil && len(h.store.loading) == 0
	})
	h.settle(t)
}

// settle waits until every request the store sent is answered.
func (h *harness) settle(t *testing.T) {
	t.Helper()
	h.loop.RunUntil(t, func() bool { return h.store.InFlight() == 0 })
}

func session(id int64, title string, updated time.Duration) proto.Session {
	return proto.Session{ID: id, Title: title, Cwd: "/home/team/projects/demo", State: proto.SessionIdle,
		Model:     proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"},
		CreatedAt: pocketdtest.Now.Add(-time.Hour), UpdatedAt: pocketdtest.Now.Add(updated)}
}

func approval(id string, sessionID int64, call string, requested time.Duration) proto.Approval {
	return proto.Approval{ID: id, SessionID: sessionID, CallID: call, Tool: "bash",
		Arguments: []byte(`{"command":"go test ./..."}`), State: proto.ApprovalPending, RequestedAt: pocketdtest.Now.Add(requested)}
}
