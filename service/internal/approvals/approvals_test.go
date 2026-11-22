package approvals

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/store/storetest"
)

type recorder struct {
	mu   sync.Mutex
	seen []proto.Approval
	note chan struct{}
}

func (r *recorder) publish(a proto.Approval) {
	r.mu.Lock()
	r.seen = append(r.seen, a)
	r.mu.Unlock()
	select {
	case r.note <- struct{}{}:
	default:
	}
}

func setup(t *testing.T) (*Manager, *store.Store, *recorder) {
	t.Helper()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "w.sqlite"), nil)
	t.Cleanup(func() { _ = s.Close() })
	r := &recorder{note: make(chan struct{}, 16)}
	return New(s, r.publish), s, r
}

// No tool call waits for the owner (ADR-0012): the commands run in the
// sandbox instead.
func TestPolicy(t *testing.T) {
	for _, tool := range []string{"bash", "write", "edit", "read", "grep", "find", "ls"} {
		if NeedsApproval(tool) {
			t.Errorf("NeedsApproval(%q) = true, want false", tool)
		}
	}
}

func TestAwaitBlocksUntilDecided(t *testing.T) {
	m, _, r := setup(t)
	ctx := context.Background()
	call := Call{SessionID: 1, TaskID: 7, CallID: "c1", Tool: "bash", Arguments: []byte(`{"command":"ls"}`)}
	done := make(chan Decision, 1)
	go func() {
		d, err := m.Await(ctx, call)
		if err != nil {
			t.Error(err)
		}
		done <- d
	}()
	<-r.note // the request was published
	pending, _ := m.Pending(ctx)
	if len(pending) != 1 || pending[0].State != proto.ApprovalPending || pending[0].Tool != "bash" {
		t.Fatalf("pending %+v", pending)
	}
	select {
	case <-done:
		t.Fatal("Await returned before a decision")
	default:
	}
	decided, _, err := m.Decide(ctx, "k1", pending[0].ID, false, "laptop", "not now")
	if err != nil || decided.State != proto.ApprovalDenied || decided.DecidedAt == nil {
		t.Fatalf("Decide %+v %v", decided, err)
	}
	if d := <-done; d.Allow || d.Reason != "not now" {
		t.Fatalf("decision %+v", d)
	}
}

func TestAwaitFindsADecisionMadeEarlier(t *testing.T) {
	m, s, _ := setup(t)
	ctx := context.Background()
	call := Call{SessionID: 1, TaskID: 7, CallID: "c1", Tool: "write"}
	row, _, _ := s.RequestApproval(ctx, store.ApprovalRow{SessionID: 1, TaskID: 7, CallID: "c1", Tool: "write", Arguments: []byte(`{}`)})
	if _, _, err := s.DecideApproval(ctx, "k1", row.ID, true, "phone", ""); err != nil {
		t.Fatal(err)
	}
	d, err := m.Await(ctx, call)
	if err != nil || !d.Allow {
		t.Fatalf("Await after a stored decision %+v %v", d, err)
	}
}

func TestAwaitReturnsWhenTheContextEnds(t *testing.T) {
	m, _, r := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Await(ctx, Call{SessionID: 1, TaskID: 1, CallID: "x", Tool: "bash"})
		done <- err
	}()
	<-r.note // the pending approval is recorded and published
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Await = %v", err)
	}
	// The approval stays pending for the next run of the task.
	pending, _ := m.Pending(context.Background())
	if len(pending) != 1 {
		t.Fatalf("pending after cancel %+v", pending)
	}
}

// A wait that ends leaves no wake behind, as a question's does: on its
// context's end, and when a decision ends 2 waits of one approval (#128).
func TestAnEndedWaitLeavesNoWake(t *testing.T) {
	noWake := func(t *testing.T, m *Manager) {
		t.Helper()
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.waiters) != 0 {
			t.Fatalf("an ended wait left %d wakes", len(m.waiters))
		}
	}
	t.Run("context ended", func(t *testing.T) {
		m, _, _ := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m.publish = func(proto.Approval) { cancel() }
		_, err := m.Await(ctx, Call{SessionID: 1, TaskID: 1, CallID: "ended", Tool: "bash"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Await = %v, want context.Canceled", err)
		}
		noWake(t, m)
	})
	t.Run("two waits", func(t *testing.T) {
		m, _, _ := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		published := make(chan string, 2)
		m.publish = func(a proto.Approval) {
			if a.State == proto.ApprovalPending {
				published <- a.ID
			}
		}
		type result struct {
			decision Decision
			err      error
		}
		done := make(chan result, 2)
		call := Call{SessionID: 1, TaskID: 1, CallID: "shared", Tool: "bash"}
		for range 2 {
			go func() {
				d, err := m.Await(ctx, call)
				done <- result{d, err}
			}()
		}
		id := <-published
		if other := <-published; other != id {
			t.Fatalf("the waits published different approvals: %s, %s", id, other)
		}
		// Publication precedes registration; wait for both registrations
		// under their own lock, rather than assuming either goroutine ran.
		for {
			m.mu.Lock()
			w := m.waiters[id]
			registered := w != nil && w.n == 2
			m.mu.Unlock()
			if registered {
				break
			}
			runtime.Gosched()
		}
		if _, _, err := m.Decide(ctx, "shared-decision", id, true, "laptop", ""); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if got := <-done; got.err != nil || !got.decision.Allow {
				t.Fatalf("Await = %+v, %v; want allowed", got.decision, got.err)
			}
		}
		noWake(t, m)
	})
}

func TestDecideTwiceConflicts(t *testing.T) {
	m, s, _ := setup(t)
	ctx := context.Background()
	row, _, _ := s.RequestApproval(ctx, store.ApprovalRow{SessionID: 1, TaskID: 2, CallID: "y", Tool: "edit", Arguments: []byte(`{}`)})
	if _, _, err := m.Decide(ctx, "k1", row.ID, true, "a", ""); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := m.Decide(ctx, "k1", row.ID, true, "a", ""); err != nil || !replayed {
		t.Fatalf("retry = %v, replayed %v", err, replayed)
	}
	if _, _, err := m.Decide(ctx, "k2", row.ID, true, "a", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Decide = %v", err)
	}
}

func TestRetireEndedWakesTheWaitingCall(t *testing.T) {
	m, _, r := setup(t)
	ctx := context.Background()
	done := make(chan Decision, 1)
	go func() {
		d, _ := m.Await(ctx, Call{SessionID: 4, TaskID: 9, CallID: "b", Tool: "bash"})
		done <- d
	}()
	<-r.note
	if err := m.RetireEnded(ctx, 4, func(context.Context, int64) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if d := <-done; d.Allow || d.Reason != "the task ended" {
		t.Fatalf("decision %+v", d)
	}
}

func TestEventsFollowTheStoreOrder(t *testing.T) {
	m, _, r := setup(t)
	ctx := context.Background()
	go func() { _, _ = m.Await(ctx, Call{SessionID: 1, TaskID: 1, CallID: "o", Tool: "bash"}) }()
	<-r.note
	pending, _ := m.Pending(ctx)
	if _, _, err := m.Decide(ctx, "k", pending[0].ID, true, "laptop", ""); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.seen[len(r.seen)-1]
	if last.State != proto.ApprovalAllowed {
		t.Fatalf("events %+v end in %s", r.seen, last.State)
	}
}

func TestAwaitAnnouncesAPendingApprovalAgain(t *testing.T) {
	m, s, r := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, _, err := s.RequestApproval(ctx, store.ApprovalRow{SessionID: 1, TaskID: 5, CallID: "w", Tool: "write", Arguments: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = m.Await(ctx, Call{SessionID: 1, TaskID: 5, CallID: "w", Tool: "write"}) }()
	<-r.note // the existing row was announced
	cancel()
}

func TestRetireEndedSkipsLiveTasks(t *testing.T) {
	m, s, _ := setup(t)
	ctx := context.Background()
	for _, task := range []int64{1, 2} {
		if _, _, err := s.RequestApproval(ctx, store.ApprovalRow{SessionID: 1, TaskID: task, CallID: "c", Tool: "bash", Arguments: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	err := m.RetireEnded(ctx, 0, func(_ context.Context, task int64) (bool, error) { return task == 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := m.Pending(ctx)
	if len(pending) != 1 || pending[0].ID != store.ApprovalID(2, "c") {
		t.Fatalf("pending %+v", pending)
	}
}
