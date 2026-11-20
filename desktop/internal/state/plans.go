package state

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// The plans (ADR-0020), merged from fetches and events by the rule of the
// threads (keyed): a plan an event changed after a fetch began follows the
// event, and the answer of a fetch begun before one already applied is
// dropped. The owner's approval is never applied from its reply: the
// plan's event, or a fetch begun after the reply, brings the plan as
// approved, so a reply that arrives late never undoes a newer change.

func planID(p proto.Plan) int64 { return p.ID }

// planLess orders plans as GET /v1/plans does: most recently changed first.
func planLess(a, b proto.Plan) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.ID > b.ID
}

// BeginPlans starts a plans fetch, counted as BeginThreads is.
func (d *Data) BeginPlans() Fetch { return d.plans.begin(d) }

// SetPlans takes the answer of GET /v1/plans, begun as f, by the rule of
// SetThreads.
func (d *Data) SetPlans(f Fetch, list []proto.Plan) {
	if d.plans.set(f, list, planID, planLess) {
		d.rev++
	}
}

// SetPlansError records that the plans' fetch f failed, unless a fetch
// begun later already answered; the plans stay.
func (d *Data) SetPlansError(f Fetch, err string) {
	if d.plans.fail(f, err) {
		d.rev++
	}
}

// PlansStatus is where the plans stand, and why their last fetch failed.
// The Inbox says no plan waits only when it is ListLoaded.
func (d *Data) PlansStatus() (ListStatus, string) {
	return d.plans.list.status, d.plans.list.err
}

// Plans are every plan, ended ones too, most recently changed first. The
// slice is the Data's own: read it, do not change it.
func (d *Data) Plans() []proto.Plan { return d.plans.items }

// Plan returns the plan with id.
func (d *Data) Plan(id int64) (proto.Plan, bool) {
	if i := d.plans.index(id, planID); i >= 0 {
		return d.plans.items[i], true
	}
	return proto.Plan{}, false
}

// PlansToApprove are the plans that wait for the owner's approval
// (ToApprove), oldest first.
func (d *Data) PlansToApprove() []proto.Plan {
	var out []proto.Plan
	for i := len(d.plans.items) - 1; i >= 0; i-- {
		if p := d.plans.items[i]; ToApprove(p) {
			out = append(out, p)
		}
	}
	return out
}

// ToApprove says whether p waits for the owner's approval: in review, with
// the graph its lead drafted for its revision. Only such a plan has what
// an approval names (PlanSeen).
func ToApprove(p proto.Plan) bool {
	_, _, ok := PlanSeen(p)
	return ok
}

// PlanSeen is what an approval of p names, as the client shows it: its
// revision and its graph's version (protocol: ApprovePlan). ok is false
// for a plan that does not wait for an approval.
func PlanSeen(p proto.Plan) (rev string, graph int, ok bool) {
	if p.State != proto.PlanReview || p.Rev == "" || p.Graph == nil || p.Graph.Approved || p.Graph.Rev != p.Rev || len(p.Waiting) > 0 {
		return "", 0, false
	}
	return p.Rev, p.Graph.Version, true
}

// ReadyTasks are the ids of p's tasks that the supervisor may deliver now,
// in graph order: ready in its approved graph.
func ReadyTasks(p proto.Plan) []string {
	if p.Graph == nil || !p.Graph.Approved || (p.State != proto.PlanApproved && p.State != proto.PlanBuilding) {
		return nil
	}
	var out []string
	for _, t := range p.Graph.Tasks {
		if t.State == proto.TaskReady {
			out = append(out, t.ID)
		}
	}
	return out
}

// OpenAsksOf are the entries of p's gate that wait for the owner: its open
// asks ("A3 open"), not an answer to write in or a consult that runs.
func OpenAsksOf(p proto.Plan) []string {
	var out []string
	for _, w := range p.Waiting {
		if strings.HasSuffix(w, " open") {
			out = append(out, w)
		}
	}
	return out
}

// PlanThread says whether t is a plan's own thread, whose specialist plans:
// it takes no place on the desk.
func PlanThread(t proto.Thread) bool { return t.Plan != 0 && t.PlanTask == "" }

func (d *Data) applyPlan(p proto.Plan) {
	d.rev++
	d.plans.put(d.touched(), p, planID, planLess)
}

// ShortRev is a plan revision's first 7 characters, as pocketd's texts
// show it.
func ShortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func planKey(id int64) string { return fmt.Sprintf("plan:%d", id) }

// ApprovePlan approves plan p for the owner (POST /v1/plans/{id}/approve),
// at the revision and graph version the client shows (PlanSeen): pocketd
// refuses an approval of a plan that changed since. One tried again after
// a failure, of the same view, keeps its request id; another view of the
// plan is a new request. The plan comes back by its event, or by the fetch
// that follows the reply.
func (s *Store) ApprovePlan(p proto.Plan) {
	id := p.ID
	pend := pending(s.planApprovals, id)
	if pend.Busy {
		return
	}
	rev, graph, ok := PlanSeen(p)
	if !ok {
		pend.Err = "This plan does not wait for your approval."
		return
	}
	key := planKey(id)
	request := proto.ApprovePlan{RequestID: s.ids.For(key, rev+":"+strconv.Itoa(graph)), Rev: rev, Graph: graph}
	pend.Busy, pend.Err = true, ""
	s.log(LevelCommand, "Approve P%d at rev %s, tasks version %d", id, ShortRev(rev), graph)
	s.do(fmt.Sprintf("Approving P%d", id), func(ctx context.Context, c *proto.Client) error {
		_, err := c.ApprovePlan(ctx, id, request)
		return err
	}, func(err error) {
		pend.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(key)
			delete(s.planApprovals, id)
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound):
			s.ids.Done(key)
			// pocketd says why: the plan changed, or no longer waits. It
			// shows until the reload brings the plan as it is now.
			pend.Err = pe.Message
			if pe.Code == proto.ErrorCodeNotFound {
				pend.Err = "pocketd no longer has this plan."
			}
		default:
			pend.Err = err.Error()
		}
		s.loadPlans()
	})
}

// Approving is where the owner's approval of plan id stands.
func (s *Store) Approving(id int64) Pending { return valueOf(s.planApprovals[id]) }

// settlePlans drops the retry state of each approval whose plan no longer
// waits for one (approved here, elsewhere, or by a request whose reply
// was lost, or changed): no card offers its retry. An approval on its way
// keeps its state.
func (s *Store) settlePlans() {
	for id, p := range s.planApprovals {
		if p.Busy {
			continue
		}
		if plan, ok := s.Data.Plan(id); !ok || !ToApprove(plan) {
			delete(s.planApprovals, id)
			s.ids.Done(planKey(id))
		}
	}
}

func (s *Store) loadPlans() {
	f := s.Data.BeginPlans()
	var list []proto.Plan
	s.do("Loading the plans", func(ctx context.Context, c *proto.Client) (err error) {
		list, err = c.Plans(ctx)
		return err
	}, s.track("plans", func(err error) {
		if err != nil {
			s.Data.SetPlansError(f, err.Error())
			return
		}
		s.Data.SetPlans(f, list)
		s.settlePlans()
	}))
}
