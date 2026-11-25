package state

import (
	"sync"
	"testing"
	"time"
)

// testLoop stands in for the UI thread in these tests: workers post to it
// from any goroutine, as they do through the client's Post, and the test
// runs what they posted on its own goroutine, as the UI thread would. The
// zero value is ready to use.
type testLoop struct {
	mu   sync.Mutex
	fns  []func()
	wake chan struct{}
	ran  int
}

// loopTimeout is how long RunUntil waits for its condition before it
// fails the test.
const loopTimeout = 5 * time.Second

func newTestLoop() *testLoop { return &testLoop{} }

// waker is the channel a post signals, made on first use.
func (l *testLoop) waker() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wake == nil {
		l.wake = make(chan struct{}, 1)
	}
	return l.wake
}

// Post queues fn; any goroutine may call it.
func (l *testLoop) Post(fn func()) {
	w := l.waker()
	l.mu.Lock()
	l.fns = append(l.fns, fn)
	l.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

// Flush runs what was posted, and what that posts, until nothing is left.
func (l *testLoop) Flush() {
	for {
		l.mu.Lock()
		fns := l.fns
		l.fns = nil
		l.mu.Unlock()
		if len(fns) == 0 {
			return
		}
		for _, fn := range fns {
			fn()
			l.mu.Lock()
			l.ran++
			l.mu.Unlock()
		}
	}
}

// RunUntil runs what is posted until cond holds, checking it after each
// flush, and fails the test after loopTimeout. cond runs on the test's
// goroutine, so it may read what the posted functions write. It also
// checks cond every millisecond, for a condition a worker meets without
// posting.
func (l *testLoop) RunUntil(t testing.TB, cond func() bool) {
	t.Helper()
	w := l.waker()
	deadline := time.After(loopTimeout)
	for {
		l.Flush()
		if cond() {
			return
		}
		select {
		case <-w:
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for the UI thread's work")
		}
	}
}

// Ran is how many posted functions ran so far.
func (l *testLoop) Ran() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ran
}
