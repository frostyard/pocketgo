package team

import (
	"errors"
	"fmt"
	"strings"
)

// A closed thread goes back to open when a message pocketd follows by its
// key, the owner's or a supervisor's team_send, reaches its specialist's
// session (ADR-0061): the specialist was given work, so its report is
// expected. A thread a supervisor carried on in another names that one as
// its successor, and every refusal on a closed thread says where to go
// instead.

// MaxSuccessors bounds the chain of successors pocketd follows to name
// where a closed thread's work goes on now: each successor is a newer
// thread, so the chain has no loop, and a long one ends the search.
const MaxSuccessors = 16

// Sender is who sends a message to a closed thread's specialist.
type Sender int

const (
	// FromOwner: the owner, from a client (protocol rule 10).
	FromOwner Sender = iota
	// FromSupervisor: a supervisor, with team_send (ADR-0032).
	FromSupervisor
)

// ReopenTarget is what the rules read of a closed thread a message would
// reopen, or a report would be filed on.
type ReopenTarget struct {
	ID int64
	// Plan is the plan the thread works for, 0 for none; PlanTask the
	// task it delivers, "" for the plan's own thread (ADR-0020).
	Plan     int64
	PlanTask string
	// Briefed says the thread's brief reached its specialist's transcript,
	// by pocketd's note or PiG's record of its key.
	Briefed bool
	// Successor is the thread that carries this one's work on now: the
	// first of its successors that is not closed (Successor), 0 for none;
	// SuccessorAgent is its specialist's name.
	Successor      int64
	SuccessorAgent string
}

// CheckReopen refuses a message to closed thread t that could not reopen
// it: a plan's own thread, whose close ended its plan, a plan task's,
// whose task its plan settled at the close, and one whose brief never
// reached its specialist, which never started its work. Each refusal says
// what to do instead. A thread whose folder is no longer its own is
// refused by FolderGone, once code looked.
func CheckReopen(t ReopenTarget, from Sender) error {
	switch {
	case t.Plan != 0 && t.PlanTask == "":
		return fmt.Errorf("thread T%d is plan P%d's own thread and stays closed: its close ended the plan. %s", t.ID, t.Plan, t.instead(from))
	case t.Plan != 0:
		return fmt.Errorf("thread T%d delivered task %s of plan P%d and stays closed: the plan settled the task when it closed. %s",
			t.ID, t.PlanTask, t.Plan, t.instead(from))
	case !t.Briefed:
		return fmt.Errorf("thread T%d closed before its specialist had its brief, and stays closed. %s", t.ID, t.instead(from))
	}
	return nil
}

// FolderGone is the refusal of a message to closed thread t whose folder
// is no longer its own, as why says: pocketd removes a closed thread's
// worktree (ADR-0054), and its specialist could not work in the session's
// folder.
func FolderGone(t ReopenTarget, from Sender, why string) error {
	return fmt.Errorf("thread T%d is closed and its folder is no longer its own (%s), so its specialist could not work there. %s",
		t.ID, Clip(why, 300), t.instead(from))
}

// instead says where a message to closed thread t goes instead: to the
// thread that carries its work on, else to a new thread.
func (t ReopenTarget) instead(from Sender) string {
	switch {
	case t.Successor != 0 && from == FromOwner:
		return fmt.Sprintf("Send it to T%d%s, which carries its work on.", t.Successor, t.successorAgent())
	case t.Successor != 0:
		return fmt.Sprintf("Send it to T%d%s with team_send: that thread carries its work on.", t.Successor, t.successorAgent())
	case from == FromOwner:
		return "Ask its supervisor to delegate the work again in a new thread."
	}
	return fmt.Sprintf("Delegate the work again in a new thread with follows %d, so T%d names it as its successor.", t.ID, t.ID)
}

// successorAgent is ", <agent>'s thread," for a successor whose agent is
// known.
func (t ReopenTarget) successorAgent() string {
	if t.SuccessorAgent == "" {
		return ""
	}
	return " (" + t.SuccessorAgent + "'s thread)"
}

// ReportRefused is team_message's refusal on closed thread t: it names
// what the specialist can do instead, since the work it did reaches nobody
// else: the thread that carries the work on, or the owner, whose message
// would reopen t.
func ReportRefused(t ReopenTarget) string {
	head := fmt.Sprintf("thread T%d is closed and takes no more reports.", t.ID)
	switch {
	case t.Successor != 0:
		return head + fmt.Sprintf(" Its work goes on in thread T%d%s: end your turn with what you did in your reply, and ask the owner to pass it to T%d.",
			t.Successor, t.successorAgent(), t.Successor)
	case t.Plan != 0:
		return head + fmt.Sprintf(" Plan P%d settled it when it closed: end your turn with what you did in your reply, and ask the owner what comes next.", t.Plan)
	}
	return head + fmt.Sprintf(" End your turn with what you did and what is left in your reply, and ask the owner: a message from the owner or the supervisor reopens T%d, and you report then.", t.ID)
}

// MergedOut says whether a thread's linked pull requests close it
// (ADR-0016): none is open, given open, and one at least merged. A thread
// that reopened (ADR-0061) closes so only once more of them merged than
// had when it last reopened (mergedAtReopen), so the merges that came
// before its reopen never close it again at the next round.
func MergedOut(open, merged, mergedAtReopen int) bool {
	return open == 0 && merged > mergedAtReopen
}

// FollowTarget is what CheckFollows reads of the thread a delegation
// carries on (team_delegate's follows).
type FollowTarget struct {
	ID int64
	// Exists says the work database has the thread.
	Exists bool
	// Review says pocketd started it as a cross-review (ADR-0019).
	Review bool
	// Own says a session of the caller's own profile delegated it, as
	// team_close asks.
	Own bool
}

// CheckFollows refuses a delegation's follows: a number that names no
// thread, a cross-review, which no supervisor delegated, and a thread
// another supervisor delegated. The followed thread may be in any state:
// it names the new thread as its successor once that one is recorded.
func CheckFollows(f FollowTarget) error {
	switch {
	case f.ID <= 0:
		return errors.New("follows names the thread this one carries on by its number: 12 for T12")
	case !f.Exists:
		return fmt.Errorf("no thread T%d to carry on", f.ID)
	case f.Review:
		return fmt.Errorf("thread T%d is a cross-review pocketd runs: no thread carries it on", f.ID)
	case !f.Own:
		return fmt.Errorf("thread T%d was delegated by another supervisor", f.ID)
	}
	return nil
}

// SendTextToClosed is the message a supervisor's team_send brings the
// specialist of a thread that was closed when it was kept (ADR-0061): who
// sent it, the text, that its arrival reopens the thread, and that a
// report answers it. It says nothing of a blocked thread's asks, so its
// delivery withdraws none.
func SendTextToClosed(thread int64, supervisor, text string) string {
	return fmt.Sprintf("[T%d · %s, the supervisor] %s\n\n"+
		"T%d was closed: this message reopens it, and T%d is open again once it reaches you. When you have acted on it, report with team_message.",
		thread, supervisor, strings.TrimSpace(text), thread, thread)
}
