// Package approvals holds tool calls for the owner's decision. Which calls
// need a decision is a rule in this package (ADR-0011); the decision itself
// is stored, so a call that waited when the service stopped asks for the same
// approval when its task runs again, and finds the decision if one was made.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// needsApproval lists the tools whose calls wait for the owner: none. The VM
// and the command sandbox are the boundary (ADR-0012). A gate that needs the
// owner's word adds its tool here, with its test.
var needsApproval = map[string]bool{}

// NeedsApproval reports whether a call of tool waits for the owner.
func NeedsApproval(tool string) bool { return needsApproval[tool] }

// Call is a tool call that needs a decision.
type Call struct {
	SessionID int64
	TaskID    int64
	CallID    string
	Tool      string
	Arguments json.RawMessage
}

// Decision is the owner's answer.
type Decision struct {
	Allow  bool
	Reason string
}

// Manager is safe for concurrent use.
type Manager struct {
	store   *store.Store
	publish func(proto.Approval)
	mu      sync.Mutex
	waiters map[string]chan struct{}
	// order holds each approval's state change together with its
	// publication, so events leave in the order the store changed: a stale
	// "pending" never follows a decision.
	order sync.Mutex
}

// New returns a manager that publishes every requested and decided approval.
func New(s *store.Store, publish func(proto.Approval)) *Manager {
	return &Manager{store: s, publish: publish, waiters: map[string]chan struct{}{}}
}

// Await records the approval of call (or finds it) and blocks until it is
// decided or ctx ends.
func (m *Manager) Await(ctx context.Context, call Call) (Decision, error) {
	arguments := call.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	m.order.Lock()
	row, _, err := m.store.RequestApproval(ctx, store.ApprovalRow{
		SessionID: call.SessionID, TaskID: call.TaskID, CallID: call.CallID, Tool: call.Tool, Arguments: arguments,
	})
	if err != nil {
		m.order.Unlock()
		return Decision{}, err
	}
	if decision, ok := decided(row); ok {
		m.order.Unlock()
		return decision, nil
	}
	// Published on every wait, so a call that waits again after a restart
	// announces itself again.
	m.publish(Wire(row))
	m.order.Unlock()
	wake := m.waiter(row.ID)
	// Re-read after registering, so a decision made in between is not missed.
	if row, err = m.store.Approval(ctx, row.ID); err != nil {
		return Decision{}, err
	}
	if decision, ok := decided(row); ok {
		return decision, nil
	}
	select {
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case <-wake:
	}
	row, err = m.store.Approval(ctx, row.ID)
	if err != nil {
		return Decision{}, err
	}
	decision, ok := decided(row)
	if !ok {
		return Decision{}, errors.New("approval woke undecided")
	}
	return decision, nil
}

func (m *Manager) waiter(id string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.waiters[id]
	if !ok {
		ch = make(chan struct{})
		m.waiters[id] = ch
	}
	return ch
}

func decided(row store.ApprovalRow) (Decision, bool) {
	switch row.State {
	case "allowed":
		return Decision{Allow: true, Reason: row.Reason}, true
	case "denied":
		return Decision{Allow: false, Reason: row.Reason}, true
	}
	return Decision{}, false
}

// Decide records the owner's decision under a device-scoped request key, in
// one transaction with the key, then wakes the call that waits for it. A
// retry with the same key returns the decided approval without publishing
// again (replayed true).
func (m *Manager) Decide(ctx context.Context, key, id string, allow bool, by, reason string) (approval proto.Approval, replayed bool, err error) {
	m.order.Lock()
	defer m.order.Unlock()
	row, replayed, err := m.store.DecideApproval(ctx, key, id, allow, by, reason)
	if err != nil {
		return proto.Approval{}, false, err
	}
	approval = Wire(row)
	if !replayed {
		m.wake(id)
		m.publish(approval)
	}
	return approval, replayed, nil
}

func (m *Manager) wake(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.waiters[id]; ok {
		close(ch)
		delete(m.waiters, id)
	}
}

// RetireEnded denies each pending approval whose task ended, as reported by
// ended, wakes its call and publishes it. sessionID 0 means every session:
// the service runs that at start, before tasks resume; an abort runs it for
// its session once the run settled, so a run started since keeps its
// approvals.
func (m *Manager) RetireEnded(ctx context.Context, sessionID int64, ended func(ctx context.Context, taskID int64) (bool, error)) error {
	rows, err := m.store.PendingApprovals(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if sessionID != 0 && row.SessionID != sessionID {
			continue
		}
		done, err := ended(ctx, row.TaskID)
		if err != nil {
			return err
		}
		if !done {
			continue
		}
		if err := m.retire(ctx, row.ID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) retire(ctx context.Context, id string) error {
	m.order.Lock()
	defer m.order.Unlock()
	retired, err := m.store.RetireApproval(ctx, id, "the task ended")
	if errors.Is(err, store.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	m.wake(retired.ID)
	m.publish(Wire(retired))
	return nil
}

// Pending lists the pending approvals, the oldest first.
func (m *Manager) Pending(ctx context.Context) ([]proto.Approval, error) {
	rows, err := m.store.PendingApprovals(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]proto.Approval, len(rows))
	for i, row := range rows {
		out[i] = Wire(row)
	}
	return out, nil
}

// Wire is the protocol form of an approval row.
func Wire(row store.ApprovalRow) proto.Approval {
	a := proto.Approval{
		ID: row.ID, SessionID: row.SessionID, CallID: row.CallID, Tool: row.Tool,
		Arguments: json.RawMessage(row.Arguments), State: proto.ApprovalState(row.State),
		RequestedAt: row.RequestedAt, DecidedBy: row.DecidedBy, Reason: row.Reason,
	}
	if row.DecidedAt != nil {
		t := row.DecidedAt.UTC().Truncate(time.Millisecond)
		a.DecidedAt = &t
	}
	return a
}
