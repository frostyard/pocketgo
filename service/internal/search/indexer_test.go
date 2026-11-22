package search

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// fakes is a Source and an Index in memory: the index keeps docs and
// cursors as the work database does, a doc at or before its session's
// cursor kept already. Every call signals changed.
type fakes struct {
	mu        sync.Mutex
	entries   map[int64][]proto.Entry
	docs      map[int64]Doc
	cursors   map[int64]int64
	reads     []string
	readErr   map[int64]error
	writeErr  error
	cursorErr error
	listErr   error
	sessions  []int64
	changed   chan struct{}
	timers    chan chan time.Time
	waits     []time.Duration
}

func newFakes() *fakes {
	return &fakes{entries: map[int64][]proto.Entry{}, docs: map[int64]Doc{}, cursors: map[int64]int64{}, readErr: map[int64]error{},
		changed: make(chan struct{}, 1), timers: make(chan chan time.Time, 8)}
}

func (f *fakes) signal() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

func (f *fakes) EntriesAfter(_ context.Context, session, after int64, most int) ([]proto.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.signal()
	f.reads = append(f.reads, fmt.Sprintf("%d>%d", session, after))
	if err := f.readErr[session]; err != nil {
		return nil, err
	}
	var out []proto.Entry
	for _, e := range f.entries[session] {
		if e.ID > after && len(out) < most {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakes) SearchSessions(context.Context) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.signal()
	if f.listErr != nil {
		err := f.listErr
		f.listErr = nil
		return nil, err
	}
	return slices.Clone(f.sessions), nil
}

func (f *fakes) SearchCursors(context.Context) (map[int64]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.signal()
	if f.cursorErr != nil {
		err := f.cursorErr
		f.cursorErr = nil
		return nil, err
	}
	out := map[int64]int64{}
	for k, v := range f.cursors {
		out[k] = v
	}
	return out, nil
}

func (f *fakes) IndexEntries(_ context.Context, session, through int64, docs []Doc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.signal()
	if f.writeErr != nil {
		return f.writeErr
	}
	for _, d := range docs {
		if d.Entry <= f.cursors[session] {
			continue
		}
		if _, dup := f.docs[d.Entry]; dup {
			return fmt.Errorf("entry %d kept twice", d.Entry)
		}
		f.docs[d.Entry] = d
	}
	f.cursors[session] = max(f.cursors[session], through)
	return nil
}

func (f *fakes) after(d time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	f.timers <- c
	f.signal()
	return c
}

func (f *fakes) add(session int64, entries ...proto.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.sessions, session) {
		f.sessions = append(f.sessions, session)
	}
	for _, e := range entries {
		e.SessionID = session
		f.entries[session] = append(f.entries[session], e)
	}
}

// indexer is an Indexer over f, which wakes f's waits after each of its
// steps, once the step changed what it changes (Codex review of #111,
// round 1).
func (f *fakes) indexer() *Indexer {
	return New(Options{Source: f, Index: f, After: f.after, Stepped: f.signal})
}

// until waits for cond, which the fakes' calls and the indexer's steps may
// change.
func (f *fakes) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		f.mu.Lock()
		ok := cond()
		f.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-f.changed:
		case <-deadline:
			t.Fatalf("waited for %s", what)
		}
	}
}

func user(id int64, text string) proto.Entry {
	return proto.Entry{ID: id, Kind: proto.EntryUser, Text: text, At: t0.Add(time.Duration(id) * time.Second)}
}

func result(id int64) proto.Entry {
	return proto.Entry{ID: id, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c", Text: "output"}}
}

// The start reads every session from its cursor, a page at a time and in
// turn, keeps only what is searched, and moves each cursor past every
// entry read; it is behind until each is read through.
func TestTheStartReadsEverySessionFromItsCursor(t *testing.T) {
	f := newFakes()
	for id := int64(1); id <= Page+5; id++ {
		f.add(1, user(id, fmt.Sprintf("message %d", id)))
	}
	f.add(2, user(1000, "kept before"), result(1001), user(1002, "new"), result(1003))
	f.cursors[2] = 1000
	f.docs[1000] = Doc{Entry: 1000, Text: "kept before"}
	x := f.indexer()
	if !x.Behind() {
		t.Error("an indexer not started is not behind")
	}
	x.Start()
	defer x.Stop()
	f.until(t, "both sessions read through", func() bool { return f.cursors[1] == Page+5 && f.cursors[2] == 1003 })
	f.until(t, "the start's pass", func() bool { return !x.Behind() })
	if len(f.docs) != Page+5+2 {
		t.Errorf("%d docs kept", len(f.docs))
	}
	for _, id := range []int64{1001, 1003} {
		if _, ok := f.docs[id]; ok {
			t.Errorf("a tool's output, entry %d, is kept", id)
		}
	}
	f.mu.Lock()
	reads := slices.Clone(f.reads)
	f.mu.Unlock()
	// Session 1's second page waits for session 2's turn.
	if want := []string{"1>0", "2>1000", fmt.Sprintf("1>%d", Page)}; !slices.Equal(reads[:3], want) {
		t.Errorf("reads %v, want them to start %v", reads, want)
	}
}

// An entry's event is a hint: the session is read from its cursor, so
// entries the event did not name are kept too, once each.
func TestANotedSessionIsReadFromItsCursor(t *testing.T) {
	f := newFakes()
	x := f.indexer()
	x.Note(7) // before the start, as an entry an early event brings
	x.Start()
	defer x.Stop()
	f.until(t, "the start", func() bool { return len(f.reads) == 1 })
	f.add(7, user(1, "one"), user(2, "two"))
	x.Note(7)
	f.until(t, "both entries", func() bool { return f.cursors[7] == 2 && len(f.docs) == 2 })
	f.add(7, user(3, "three"))
	x.Note(7)
	x.Note(7)
	f.until(t, "the third", func() bool { return f.cursors[7] == 3 && len(f.docs) == 3 })
	if x.Behind() {
		t.Error("behind with nothing to read")
	}
}

// A session whose read or write fails is read again after RetryAfter, and
// the others go on meanwhile; a write that failed keeps nothing.
func TestAFailureIsReadAgainLaterAndHoldsNoOtherBack(t *testing.T) {
	f := newFakes()
	f.add(1, user(1, "one"))
	f.add(2, user(2, "two"))
	f.readErr[1] = errors.New("PiG is busy")
	x := f.indexer()
	x.Start()
	defer x.Stop()
	f.until(t, "session 2", func() bool { return f.cursors[2] == 2 })
	if !x.Behind() {
		t.Error("not behind while session 1 failed")
	}
	timer := <-f.timers
	f.mu.Lock()
	delete(f.readErr, 1)
	f.writeErr = errors.New("disk full")
	f.mu.Unlock()
	timer <- t0
	f.until(t, "the write that fails", func() bool { return len(f.timers) == 1 })
	f.mu.Lock()
	if f.cursors[1] != 0 || len(f.docs) != 1 {
		t.Errorf("a failed write moved the cursor to %d, %d docs", f.cursors[1], len(f.docs))
	}
	f.writeErr = nil
	f.mu.Unlock()
	(<-f.timers) <- t0
	f.until(t, "session 1", func() bool { return f.cursors[1] == 1 && len(f.docs) == 2 })
	f.until(t, "the start's pass", func() bool { return !x.Behind() })
	// The second failure in a row waited twice as long as the first.
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []time.Duration{RetryAfter, 2 * RetryAfter}; !slices.Equal(f.waits, want) {
		t.Errorf("waits %v, want %v", f.waits, want)
	}
}

// Each failure in a row doubles the wait, up to MaxRetryAfter.
func TestRetryDelayDoublesUpToItsMost(t *testing.T) {
	for failures, want := range map[int]time.Duration{0: RetryAfter, 1: RetryAfter, 2: 10 * time.Second, 3: 20 * time.Second,
		6: 160 * time.Second, 7: MaxRetryAfter, 50: MaxRetryAfter} {
		if got := RetryDelay(failures); got != want {
			t.Errorf("RetryDelay(%d) = %v, want %v", failures, got, want)
		}
	}
}

// The cursors that fail to read are read again; Stop ends the run, and a
// Start after it does nothing.
func TestTheCursorsAreReadAgainAndStopEnds(t *testing.T) {
	f := newFakes()
	f.cursorErr = errors.New("locked")
	f.add(1, user(1, "one"))
	x := f.indexer()
	x.Start()
	(<-f.timers) <- t0
	f.until(t, "session 1", func() bool { return f.cursors[1] == 1 })
	x.Stop()
	x.Stop()
	x.Start()
	y := New(Options{Source: f, Index: f})
	y.Stop() // never started
}

// The start lists the sessions on its own goroutine, and a list that
// fails is read again: until it is read, and every session in it read
// through, the index is behind (Codex review of #111, round 1).
func TestTheStartListsTheSessionsAgainUntilItCan(t *testing.T) {
	f := newFakes()
	f.add(4, user(1, "an old reply"))
	f.listErr = errors.New("the work database is busy")
	x := f.indexer()
	x.Start()
	defer x.Stop()
	timer := <-f.timers
	if !x.Behind() {
		t.Error("the index is not behind while the sessions are not listed")
	}
	f.mu.Lock()
	if len(f.reads) != 0 {
		t.Errorf("reads %v before the list", f.reads)
	}
	f.mu.Unlock()
	timer <- t0
	f.until(t, "the old session", func() bool { return f.cursors[4] == 1 && !x.Behind() })
}

// A session on its retry's timer is not read again for a note: the retry
// reads everything after its cursor (Codex review of #111, round 1).
func TestANoteWaitsForTheSessionsRetry(t *testing.T) {
	f := newFakes()
	f.add(1, user(1, "one"))
	f.readErr[1] = errors.New("PiG is busy")
	x := f.indexer()
	x.Start()
	defer x.Stop()
	timer := <-f.timers
	f.until(t, "the failed read", func() bool { return len(f.reads) == 1 })
	for range 5 {
		x.Note(1)
	}
	// Another session's step shows the notes had their turn.
	f.add(2, user(2, "two"))
	x.Note(2)
	f.until(t, "session 2", func() bool { return f.cursors[2] == 2 })
	f.mu.Lock()
	if want := []string{"1>0", "2>0"}; !slices.Equal(f.reads, want) {
		t.Errorf("reads %v, want %v: a note read a session on its timer", f.reads, want)
	}
	delete(f.readErr, 1)
	f.mu.Unlock()
	timer <- t0
	f.until(t, "session 1", func() bool { return f.cursors[1] == 1 })
}
