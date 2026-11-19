package approvals

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
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
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "w.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &recorder{note: make(chan struct{}, 16)}
	return New(s, r.publish), s, r
}

func TestPolicy(t *testing.T) {
	for tool, want := range map[string]bool{"bash": true, "write": true, "edit": true, "read": false, "grep": false} {
		if NeedsApproval(tool) != want {
			t.Errorf("NeedsApproval(%q) = %v", tool, !want)
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

func TestRetireWakesTheWaitingCall(t *testing.T) {
	m, _, r := setup(t)
	ctx := context.Background()
	done := make(chan Decision, 1)
	go func() {
		d, _ := m.Await(ctx, Call{SessionID: 4, TaskID: 9, CallID: "b", Tool: "bash"})
		done <- d
	}()
	<-r.note
	if err := m.RetireSession(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if d := <-done; d.Allow || d.Reason != "the run was aborted" {
		t.Fatalf("decision %+v", d)
	}
}

func TestRetireEndedSkipsLiveTasks(t *testing.T) {
	m, s, _ := setup(t)
	ctx := context.Background()
	for _, task := range []int64{1, 2} {
		if _, _, err := s.RequestApproval(ctx, store.ApprovalRow{SessionID: 1, TaskID: task, CallID: "c", Tool: "bash", Arguments: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	err := m.RetireEnded(ctx, func(_ context.Context, task int64) (bool, error) { return task == 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := m.Pending(ctx)
	if len(pending) != 1 || pending[0].ID != store.ApprovalID(2, "c") {
		t.Fatalf("pending %+v", pending)
	}
}
