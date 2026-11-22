package approvals

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// Questions holds the question tool's calls for the owner's answer
// (ADR-0048), as Manager holds a call for the owner's decision: the
// question is stored under its task's and call's id, so a call that
// waited when the service stopped shows the same question when its task
// runs again, and finds an answer made before; an aborted run's question
// is skipped once its task ended (RetireEnded), as its approvals are
// denied; and a question with no answer by its deadline is skipped too.
// Questions is safe for concurrent use.
type Questions struct {
	store *store.Store
	// publish tells the clients that session's question changed: its
	// session event, read as the session is then.
	publish func(session int64)
	now     func() time.Time
	// timer runs f once after d, unless cancel is called first.
	timer func(d time.Duration, f func()) (cancel func())
	// skipQuestion is store.SkipQuestion, and readQuestion store.Question;
	// tests make them fail.
	skipQuestion func(ctx context.Context, id, reason string) (store.QuestionRow, error)
	readQuestion func(ctx context.Context, id string) (store.QuestionRow, error)
	mu           sync.Mutex
	waiters      map[string]*waiter
}

// waiter is the wake of a question's waits: closed when the question
// changes, and dropped by the last wait that leaves it unclosed.
type waiter struct {
	ch chan struct{}
	n  int
}

// expiryRetry is how long a wait waits to skip a question at its deadline
// again after a skip that failed, at first; each failure doubles it, to
// expiryRetryMax.
const (
	expiryRetry    = time.Second
	expiryRetryMax = 30 * time.Second
)

// NewQuestions returns the questions of s, which publish each change of a
// session's question, on the clock now with its timer.
func NewQuestions(s *store.Store, publish func(session int64), now func() time.Time, timer func(time.Duration, func()) func()) *Questions {
	return &Questions{store: s, publish: publish, now: now, timer: timer, skipQuestion: s.SkipQuestion, readQuestion: s.Question,
		waiters: map[string]*waiter{}}
}

// Put records row as its call's waiting question, or finds the one its
// call recorded already (created false); a new one is published. A
// question while another of its session waits is store.ErrQuestionWaits.
func (q *Questions) Put(ctx context.Context, row store.QuestionRow) (store.QuestionRow, bool, error) {
	stored, created, err := q.store.PutQuestion(ctx, row)
	if err == nil && created {
		q.publish(stored.SessionID)
	}
	return stored, created, err
}

// Await blocks until question id is settled, and returns it: answered or
// skipped by the owner, skipped by an abort, or skipped by pocketd once
// its deadline passed, a deadline that passed while the service stopped
// included. ctx ending, as an abort or the service's stop does, ends the
// wait with ctx's error and settles nothing.
//
// The wait skips the question at its deadline itself, woken by its timer:
// a skip that fails is tried again, later, until it holds or ctx ends,
// so a run never waits past its deadline for a store that failed once
// (Codex review of #95, round 1). A read of the question that fails is
// tried again the same way: the question is published, and a call that
// left it would leave the owner an answer no one takes, or the agent an
// error in place of an answer made (round 2).
func (q *Questions) Await(ctx context.Context, id string) (store.QuestionRow, error) {
	retry := expiryRetry
	backoff := func() time.Duration {
		wait := retry
		retry = min(2*retry, expiryRetryMax)
		return wait
	}
	for {
		wake := q.waiter(id)
		// Read after registering, so a change made in between is not
		// missed.
		row, err := q.readQuestion(ctx, id)
		var wait time.Duration
		switch {
		case errors.Is(err, store.ErrNotFound):
			q.release(id, wake)
			return store.QuestionRow{}, err
		case err != nil && ctx.Err() != nil:
			q.release(id, wake)
			return store.QuestionRow{}, ctx.Err()
		case err != nil:
			wait = backoff()
		case row.State != store.QuestionWaiting:
			q.release(id, wake)
			return row, nil
		default:
			wait = row.Deadline.Sub(q.now())
		}
		if err == nil && wait <= 0 {
			err := q.expire(ctx, id)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				q.release(id, wake)
				return store.QuestionRow{}, ctx.Err()
			}
			wait = backoff()
		}
		cancel := q.timer(wait, func() { q.wake(id) })
		select {
		case <-ctx.Done():
			cancel()
			q.release(id, wake)
			return store.QuestionRow{}, ctx.Err()
		case <-wake:
			cancel()
		}
	}
}

// expire skips question id, which its deadline passed, unless it was
// settled meanwhile.
func (q *Questions) expire(ctx context.Context, id string) error {
	return q.skip(ctx, id, proto.SkippedTimeout)
}

// skip skips question id, by pocketd, for reason, while it waits, then
// wakes its call and publishes it.
func (q *Questions) skip(ctx context.Context, id, reason string) error {
	row, err := q.skipQuestion(ctx, id, reason)
	if errors.Is(err, store.ErrConflict) {
		q.wake(id)
		return nil
	} else if err != nil {
		return err
	}
	q.wake(id)
	q.publish(row.SessionID)
	return nil
}

// Answer records the owner's answer to question id of session, or the
// owner's skip, under a device-scoped request key, in one transaction
// with the key, then wakes the call that waits for it and publishes it.
// A retry with the same key returns the question without publishing again
// (replayed true). The answer and the options picked are as
// team.CheckAnswer gives them. An answer after the deadline is
// store.ErrQuestionLate, and skips the question as its timer would have
// (Codex review of #95, round 1).
func (q *Questions) Answer(ctx context.Context, key string, session int64, id, answer string, option []int, skipped bool, by string) (store.QuestionRow, bool, error) {
	row, replayed, err := q.store.AnswerQuestion(ctx, key, session, id, answer, option, skipped, by)
	if errors.Is(err, store.ErrQuestionLate) {
		// A skip that fails here is the wait's to try again.
		_ = q.expire(ctx, id)
	}
	if err != nil {
		return store.QuestionRow{}, false, err
	}
	if !replayed {
		q.wake(id)
		q.publish(row.SessionID)
	}
	return row, replayed, nil
}

// RetireEnded skips, with the reason proto.SkippedAborted, each waiting
// question whose task ended, as ended reports it, wakes its call and
// publishes it. sessionID 0 means every session: the service runs that at
// start, before tasks resume; an abort runs it for its session once the
// run settled, so a run started since keeps its question.
func (q *Questions) RetireEnded(ctx context.Context, sessionID int64, ended func(ctx context.Context, taskID int64) (bool, error)) error {
	waiting, err := q.store.WaitingQuestions(ctx)
	if err != nil {
		return err
	}
	for session, row := range waiting {
		if sessionID != 0 && session != sessionID {
			continue
		}
		done, err := ended(ctx, row.TaskID)
		if err != nil {
			return err
		}
		if !done {
			continue
		}
		if err := q.skip(ctx, row.ID, proto.SkippedAborted); err != nil {
			return err
		}
	}
	return nil
}

// waiter is question id's wake, which the caller waits on until it is
// closed, or releases (release).
func (q *Questions) waiter(id string) chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	w, ok := q.waiters[id]
	if !ok {
		w = &waiter{ch: make(chan struct{})}
		q.waiters[id] = w
	}
	w.n++
	return w.ch
}

// release leaves the wake ch of question id, which no wake closed: the
// last wait to leave it drops it, so a settled question keeps no wake
// (Codex review of #95, round 1).
func (q *Questions) release(id string, ch chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w, ok := q.waiters[id]; ok && w.ch == ch {
		if w.n--; w.n <= 0 {
			delete(q.waiters, id)
		}
	}
}

// wake wakes every wait of question id.
func (q *Questions) wake(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w, ok := q.waiters[id]; ok {
		close(w.ch)
		delete(q.waiters, id)
	}
}

// WireQuestion is the protocol form of a question row.
func WireQuestion(row store.QuestionRow) proto.Question {
	out := proto.Question{
		ID: row.ID, SessionID: row.SessionID, CallID: row.CallID, Question: row.Question, Options: row.Options, Multiple: row.Multiple,
		Placeholder: row.Placeholder, State: proto.QuestionState(row.State), CreatedAt: row.CreatedAt, Deadline: row.Deadline,
		Answer: row.Answer, Option: row.Chosen, Reason: row.Reason, SettledBy: row.SettledBy,
	}
	if row.SettledAt != nil {
		t := row.SettledAt.UTC().Truncate(time.Millisecond)
		out.SettledAt = &t
	}
	return out
}
