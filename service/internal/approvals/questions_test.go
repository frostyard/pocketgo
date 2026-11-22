package approvals

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// questionsFixture is a Questions on a fixed clock whose timers the test
// fires, recording each session it published.
type questionsFixture struct {
	q         *Questions
	s         *store.Store
	mu        sync.Mutex
	now       time.Time
	published []int64
	timers    []func()
	durations []time.Duration
	// armed gets a note as each timer is armed.
	armed chan struct{}
}

func setupQuestions(t *testing.T) *questionsFixture {
	t.Helper()
	f := &questionsFixture{now: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC), armed: make(chan struct{}, 16)}
	now := func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	// The store and the questions share the fixture's clock, as pocketd's
	// share the service's.
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "w.sqlite"), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.s = s
	f.q = NewQuestions(s, func(session int64) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.published = append(f.published, session)
	}, now, func(d time.Duration, fire func()) func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.timers, f.durations = append(f.timers, fire), append(f.durations, d)
		f.armed <- struct{}{}
		return func() {}
	})
	return f
}

func (f *questionsFixture) put(t *testing.T, task int64, wait time.Duration) store.QuestionRow {
	t.Helper()
	row, created, err := f.q.Put(context.Background(), store.QuestionRow{SessionID: 2, TaskID: task, CallID: "q", Question: "Which?",
		Options: []string{"a", "b"}, Deadline: f.now.Add(wait)})
	if err != nil || !created {
		t.Fatalf("Put = %+v, %v, %v", row, created, err)
	}
	return row
}

// await runs Await in the background.
func (f *questionsFixture) await(ctx context.Context, id string) <-chan store.QuestionRow {
	out := make(chan store.QuestionRow, 1)
	go func() {
		row, err := f.q.Await(ctx, id)
		if err != nil {
			row.State = "error: " + err.Error()
		}
		out <- row
	}()
	return out
}

func got(t *testing.T, ch <-chan store.QuestionRow) store.QuestionRow {
	t.Helper()
	select {
	case row := <-ch:
		return row
	case <-time.After(10 * time.Second):
		t.Fatal("Await did not return")
		return store.QuestionRow{}
	}
}

// waitTimer waits until Await armed its n-th timer: it does so only once
// it registered for a wake.
func (f *questionsFixture) waitTimer(t *testing.T, n int) (func(), time.Duration) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		f.mu.Lock()
		if len(f.timers) >= n {
			fire, d := f.timers[n-1], f.durations[n-1]
			f.mu.Unlock()
			return fire, d
		}
		f.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("no timer was armed")
		case <-f.armed:
		}
	}
}

// Await waits until the owner answers, and the answer wakes it; a put and
// an answer each publish the session once, a retried answer not again.
func TestAwaitWaitsForTheAnswer(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	q := f.put(t, 7, 30*time.Minute)
	done := f.await(ctx, q.ID)
	if _, d := f.waitTimer(t, 1); d != 30*time.Minute {
		t.Fatalf("the timer waits %v", d)
	}
	answered, replayed, err := f.q.Answer(ctx, "u:d:r1", 2, q.ID, "b", []int{1}, false, "laptop")
	if err != nil || replayed || answered.State != store.QuestionAnswered {
		t.Fatalf("Answer = %+v, %v, %v", answered, replayed, err)
	}
	if row := got(t, done); row.State != store.QuestionAnswered || row.Answer != "b" {
		t.Fatalf("Await = %+v", row)
	}
	if _, replayed, err := f.q.Answer(ctx, "u:d:r1", 2, q.ID, "b", []int{1}, false, "laptop"); err != nil || !replayed {
		t.Fatalf("a retried answer: %v %v", replayed, err)
	}
	if _, _, err := f.q.Answer(ctx, "u:d:r2", 2, q.ID, "a", nil, false, "phone"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second answer: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.published) != 2 {
		t.Fatalf("published %v", f.published)
	}
}

// An answer made while no call waited is found at once, as after a
// restart; a skip by the owner carries the owner's reason.
func TestAwaitFindsAnAnswerMadeEarlier(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	q := f.put(t, 7, time.Hour)
	if _, _, err := f.q.Answer(ctx, "u:d:r1", 2, q.ID, "", nil, true, "laptop"); err != nil {
		t.Fatal(err)
	}
	row := got(t, f.await(ctx, q.ID))
	if row.State != store.QuestionSkipped || WireQuestion(row).Reason != proto.SkippedByOwner || row.SettledBy != "laptop" {
		t.Fatalf("Await = %+v", row)
	}
}

// A question with no answer by its deadline is skipped, reason timeout,
// by pocketd; one whose deadline passed while nothing waited is skipped
// as its call waits again.
func TestAwaitSkipsAQuestionPastItsDeadline(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	q := f.put(t, 7, 30*time.Minute)
	done := f.await(ctx, q.ID)
	fire, _ := f.waitTimer(t, 1)
	f.mu.Lock()
	f.now = f.now.Add(30 * time.Minute)
	f.mu.Unlock()
	fire()
	row := got(t, done)
	if row.State != store.QuestionSkipped || row.Reason != proto.SkippedTimeout || row.SettledBy != "pocketd" {
		t.Fatalf("Await = %+v", row)
	}

	late := f.put(t, 8, time.Minute)
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	if row := got(t, f.await(ctx, late.ID)); row.State != store.QuestionSkipped || row.Reason != proto.SkippedTimeout {
		t.Fatalf("a deadline passed before the wait: %+v", row)
	}
}

// ctx ending, as an abort or a stop does, ends the wait and settles
// nothing; RetireEnded then skips the question, reason aborted, only once
// its task ended, and only in the session named.
func TestAnAbortedRunsQuestionIsSkippedOnceItsTaskEnded(t *testing.T) {
	f := setupQuestions(t)
	ctx, cancel := context.WithCancel(context.Background())
	q := f.put(t, 7, time.Hour)
	done := f.await(ctx, q.ID)
	f.waitTimer(t, 1)
	cancel()
	if row := got(t, done); row.State != "error: "+context.Canceled.Error() {
		t.Fatalf("Await after its ctx ended: %+v", row)
	}
	bg := context.Background()
	if row, err := f.s.Question(bg, q.ID); err != nil || row.State != store.QuestionWaiting {
		t.Fatalf("a stopped wait settled its question: %+v, %v", row, err)
	}
	ended := map[int64]bool{}
	taskEnded := func(_ context.Context, task int64) (bool, error) { return ended[task], nil }
	if err := f.q.RetireEnded(bg, 0, taskEnded); err != nil {
		t.Fatal(err)
	}
	if row, _ := f.s.Question(bg, q.ID); row.State != store.QuestionWaiting {
		t.Fatalf("a live task's question was skipped: %+v", row)
	}
	ended[7] = true
	if err := f.q.RetireEnded(bg, 3, taskEnded); err != nil {
		t.Fatal(err)
	}
	if row, _ := f.s.Question(bg, q.ID); row.State != store.QuestionWaiting {
		t.Fatalf("another session's retire skipped it: %+v", row)
	}
	if err := f.q.RetireEnded(bg, 2, taskEnded); err != nil {
		t.Fatal(err)
	}
	if row, _ := f.s.Question(bg, q.ID); row.State != store.QuestionSkipped || row.Reason != proto.SkippedAborted || row.SettledBy != "pocketd" {
		t.Fatalf("the aborted run's question %+v", row)
	}
}

// A skip at the deadline that fails, as a store busy past its timeout, is
// tried again later, the wait doubling, until it holds: the run never
// waits past its deadline for good (Codex review of #95, round 1).
func TestAFailedTimeoutIsTriedAgain(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	q := f.put(t, 7, 30*time.Minute)
	fails := 2
	f.q.skipQuestion = func(ctx context.Context, id, reason string) (store.QuestionRow, error) {
		f.mu.Lock()
		failing := fails > 0
		fails--
		f.mu.Unlock()
		if failing {
			return store.QuestionRow{}, errors.New("database is locked")
		}
		return f.s.SkipQuestion(ctx, id, reason)
	}
	done := f.await(ctx, q.ID)
	fire, _ := f.waitTimer(t, 1)
	f.mu.Lock()
	f.now = f.now.Add(30 * time.Minute)
	f.mu.Unlock()
	fire()
	retry, d := f.waitTimer(t, 2)
	if d != expiryRetry {
		t.Fatalf("the first retry waits %v", d)
	}
	retry()
	retry, d = f.waitTimer(t, 3)
	if d != 2*expiryRetry {
		t.Fatalf("the second retry waits %v", d)
	}
	retry()
	if row := got(t, done); row.State != store.QuestionSkipped || row.Reason != proto.SkippedTimeout {
		t.Fatalf("Await = %+v", row)
	}
}

// A read of the question that fails, as right after an answer woke the
// call, is tried again later: the call gets the answer made, never an
// error in its place (Codex review of #95, round 2). A question that is
// gone ends the wait.
func TestAFailedReadIsTriedAgain(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	// The second read, the one after the answer's wake, fails.
	reads := 0
	f.q.readQuestion = func(ctx context.Context, id string) (store.QuestionRow, error) {
		f.mu.Lock()
		reads++
		failing := reads == 2
		f.mu.Unlock()
		if failing {
			return store.QuestionRow{}, errors.New("database is locked")
		}
		return f.s.Question(ctx, id)
	}
	q := f.put(t, 7, time.Hour)
	done := f.await(ctx, q.ID)
	f.waitTimer(t, 1)
	if _, _, err := f.q.Answer(ctx, "u:d:r1", 2, q.ID, "b", []int{1}, false, "laptop"); err != nil {
		t.Fatal(err)
	}
	retry, d := f.waitTimer(t, 2)
	if d != expiryRetry {
		t.Fatalf("the retry waits %v", d)
	}
	retry()
	if row := got(t, done); row.State != store.QuestionAnswered || row.Answer != "b" {
		t.Fatalf("Await = %+v", row)
	}
	if row := got(t, f.await(ctx, "qnone")); row.State != "error: "+store.ErrNotFound.Error() {
		t.Fatalf("a question that is gone: %+v", row)
	}
}

// An answer after the deadline is refused, as late, and skips the
// question, whether or not its timer fired: the call's result is the
// timeout, never the late answer (Codex review of #95, round 1).
func TestALateAnswerSkipsTheQuestion(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	q := f.put(t, 7, 30*time.Minute)
	done := f.await(ctx, q.ID)
	f.waitTimer(t, 1)
	f.mu.Lock()
	f.now = f.now.Add(30 * time.Minute)
	f.mu.Unlock()
	_, _, err := f.q.Answer(ctx, "u:d:r1", 2, q.ID, "a", []int{0}, false, "laptop")
	if !errors.Is(err, store.ErrQuestionLate) || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a late answer: %v", err)
	}
	if row := got(t, done); row.State != store.QuestionSkipped || row.Reason != proto.SkippedTimeout || row.Answer != "" {
		t.Fatalf("Await = %+v", row)
	}
	// The refused answer left its key free.
	if _, existed, err := f.s.LookupRequest(ctx, "u:d:r1", "question:"+q.ID); err != nil || existed {
		t.Fatalf("a late answer kept its key: %v %v", existed, err)
	}
}

// A wait that ends, by an answer, a settled question found at once, or its
// ctx, leaves no wake behind (Codex review of #95, round 1).
func TestAWaitThatEndsKeepsNoWake(t *testing.T) {
	f := setupQuestions(t)
	ctx := context.Background()
	q := f.put(t, 7, time.Hour)
	done := f.await(ctx, q.ID)
	f.waitTimer(t, 1)
	if _, _, err := f.q.Answer(ctx, "u:d:r1", 2, q.ID, "a", nil, false, "laptop"); err != nil {
		t.Fatal(err)
	}
	got(t, done)
	got(t, f.await(ctx, q.ID)) // a call run again, settled already
	stopped, cancel := context.WithCancel(ctx)
	other := f.put(t, 8, time.Hour)
	done = f.await(stopped, other.ID)
	f.waitTimer(t, 2)
	cancel()
	got(t, done)
	f.q.mu.Lock()
	defer f.q.mu.Unlock()
	if len(f.q.waiters) != 0 {
		t.Fatalf("wakes left: %v", f.q.waiters)
	}
}
