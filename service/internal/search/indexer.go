package search

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Page is the most entries the indexer reads of a session at once: a
// session with more is read a page at a time, in turn with the others, so
// a long one held back no other.
const Page = 100

// RetryAfter is how long a session whose read or write failed waits before
// it is read again, the others going on meanwhile; each failure in a row
// doubles it, up to MaxRetryAfter, so a session that keeps failing is
// tried, and logged, a few times an hour.
const (
	RetryAfter    = 5 * time.Second
	MaxRetryAfter = 5 * time.Minute
)

// Source reads a session's entries, as PiG keeps them: Engine.EntriesAfter.
type Source interface {
	// EntriesAfter is at most most of session's entries after entry after,
	// by ascending id.
	EntriesAfter(ctx context.Context, session, after int64, most int) ([]proto.Entry, error)
}

// Index keeps what the indexer read: the work database (store.Store).
type Index interface {
	// SearchCursors are, by session, the newest entry read into the index.
	SearchCursors(ctx context.Context) (map[int64]int64, error)
	// IndexEntries keeps docs of session, and moves its cursor to through,
	// in one transaction: a doc at or before the cursor it finds is kept
	// already, and the cursor never moves back.
	IndexEntries(ctx context.Context, session, through int64, docs []Doc) error
}

// Options are what an Indexer reads and writes.
type Options struct {
	Source Source
	Index  Index
	Log    *slog.Logger
	// After is the timer of RetryAfter; nil is time.After. Tests give one
	// they fire.
	After func(time.Duration) <-chan time.Time
	// Stepped runs after each read of a session, kept or failed; tests
	// wait on it. nil does nothing.
	Stepped func()
}

// Indexer keeps the index up with every session's transcript (ADR-0050).
// An entry's event is a hint alone (Note): the indexer reads the session's
// entries after its cursor from PiG and keeps them with the new cursor in
// one transaction, so no entry is skipped or kept twice, whatever events
// a stop or a failure lost. Start reads every session from its cursor
// once, as the first start with search reads every transcript, and
// serving does not wait for it. It is safe for concurrent use.
type Indexer struct {
	opts Options

	mu       sync.Mutex
	queue    []int64        // the sessions to read, in turn
	queued   map[int64]bool // the sessions in queue
	behind   map[int64]bool // the start's sessions not read through yet
	failures map[int64]int  // each session's failures in a row
	waiting  map[int64]bool // the sessions whose retry's timer runs
	started  bool
	stopped  bool

	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	timers  sync.WaitGroup
	cursors map[int64]int64 // the run's own
}

// New returns an Indexer; Note may be called at once, and Start starts it.
func New(opts Options) *Indexer {
	if opts.After == nil {
		opts.After = time.After
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Indexer{opts: opts, queued: map[int64]bool{}, behind: map[int64]bool{}, failures: map[int64]int{}, waiting: map[int64]bool{},
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

// Note marks session to be read again: an entry of it was written. It
// never blocks.
func (x *Indexer) Note(session int64) {
	x.mu.Lock()
	x.enqueueLocked(session)
	x.mu.Unlock()
	x.signal()
}

// Start reads every session of sessions from its cursor, and then each one
// noted, until Stop. Starting twice, or after Stop, does nothing.
func (x *Indexer) Start(sessions []int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.started || x.stopped {
		return
	}
	x.started = true
	for _, id := range sessions {
		x.behind[id] = true
		x.enqueueLocked(id)
	}
	go x.run()
}

// Behind reports whether a session of the start is not read through yet,
// as before Start: a search may miss what it says.
func (x *Indexer) Behind() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return !x.started || len(x.behind) > 0
}

// Stop ends the run and waits for it: call it before the work database
// and the engine close.
func (x *Indexer) Stop() {
	x.mu.Lock()
	if x.stopped {
		x.mu.Unlock()
		return
	}
	x.stopped = true
	started := x.started
	close(x.stop)
	x.mu.Unlock()
	if started {
		<-x.done
	}
	x.timers.Wait()
}

func (x *Indexer) enqueueLocked(session int64) {
	if !x.queued[session] {
		x.queued[session] = true
		x.queue = append(x.queue, session)
	}
}

func (x *Indexer) signal() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// next takes the next session to read, false for none.
func (x *Indexer) next() (int64, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.queue) == 0 {
		return 0, false
	}
	id := x.queue[0]
	x.queue = x.queue[1:]
	delete(x.queued, id)
	return id, true
}

func (x *Indexer) run() {
	defer close(x.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-x.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for x.cursors == nil {
		cursors, err := x.opts.Index.SearchCursors(ctx)
		if err == nil {
			x.cursors = cursors
			break
		}
		x.opts.Log.Warn("read the search index's cursors", "err", err)
		select {
		case <-x.opts.After(RetryAfter):
		case <-x.stop:
			return
		}
	}
	for {
		select {
		case <-x.stop:
			return
		default:
		}
		id, ok := x.next()
		if !ok {
			select {
			case <-x.wake:
				continue
			case <-x.stop:
				return
			}
		}
		err := x.step(ctx, id)
		if err != nil && ctx.Err() == nil {
			x.opts.Log.Warn("index a session's entries", "session", id, "err", err)
			x.retryLater(id)
		}
		if x.opts.Stepped != nil {
			x.opts.Stepped()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// step reads a page of session id's entries after its cursor and keeps
// them with the cursor moved past them.
func (x *Indexer) step(ctx context.Context, id int64) error {
	entries, err := x.opts.Source.EntriesAfter(ctx, id, x.cursors[id], Page)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		x.succeeded(id)
		x.caughtUp(id)
		return nil
	}
	through := x.cursors[id]
	var docs []Doc
	for _, e := range entries {
		through = max(through, e.ID)
		if d, ok := DocOf(e); ok && e.ID > x.cursors[id] {
			docs = append(docs, d)
		}
	}
	if err := x.opts.Index.IndexEntries(ctx, id, through, docs); err != nil {
		return err
	}
	x.succeeded(id)
	x.cursors[id] = through
	if len(entries) >= Page {
		x.mu.Lock()
		x.enqueueLocked(id)
		x.mu.Unlock()
		return nil
	}
	x.caughtUp(id)
	return nil
}

func (x *Indexer) caughtUp(id int64) {
	x.mu.Lock()
	delete(x.behind, id)
	x.mu.Unlock()
}

// RetryDelay is how long a session waits after failures failures in a
// row: RetryAfter, doubled for each failure after the first, at most
// MaxRetryAfter.
func RetryDelay(failures int) time.Duration {
	d := RetryAfter
	for i := 1; i < failures && d < MaxRetryAfter; i++ {
		d *= 2
	}
	return min(d, MaxRetryAfter)
}

// retryLater reads session id again after RetryDelay of its failures in a
// row, on a timer of its own, while the others go on.
func (x *Indexer) retryLater(id int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.failures[id]++
	if x.waiting[id] || x.stopped {
		return
	}
	x.waiting[id] = true
	wait := x.opts.After(RetryDelay(x.failures[id]))
	x.timers.Add(1)
	go func() {
		defer x.timers.Done()
		select {
		case <-wait:
		case <-x.stop:
			return
		}
		x.mu.Lock()
		delete(x.waiting, id)
		x.enqueueLocked(id)
		x.mu.Unlock()
		x.signal()
	}()
}

// succeeded ends session id's failures in a row.
func (x *Indexer) succeeded(id int64) {
	x.mu.Lock()
	delete(x.failures, id)
	x.mu.Unlock()
}
