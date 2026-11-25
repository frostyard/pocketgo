package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// The supervisor hears each answer (ADR-0071): the owner's new answer to
// an ask files one note to the asking thread's supervisor, in the
// transaction that records the answer, and the outbox places it once.
// It is the one message that tells the supervisor of an answer: an answer
// that clears an approved plan's gate names the plan's ready tasks in it,
// and no other note is filed for that answer.

// AnsweredPrefix begins the key of every answer's note.
const AnsweredPrefix = "answered:"

// The most characters an answer's note shows of the question and of the
// answer: the whole answer goes to the asking session.
const (
	answeredQuestion = 300
	answeredAnswer   = 500
)

// AnsweredKey is the key of the note that tells a thread's supervisor of
// the owner's answer to an ask: the thread's random owner, the ask's own
// key and a digest of the answer, never a row number, which a restored
// work database gives out again. So the note is filed once, and a placed
// key proves this answer, not another one a restored work database let
// the owner give, as the answer's own key does.
func AnsweredKey(threadOwner, askKey, answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return AnsweredPrefix + threadOwner + ":" + askKey + ":" + hex.EncodeToString(sum[:6])
}

// Answered is what the note of an answer tells the supervisor, as the
// transaction that recorded the answer read it.
type Answered struct {
	Thread int64
	Title  string
	// Agent is the thread's specialist, who asked; Closed says the thread
	// is closed.
	Agent  string
	Closed bool
	Ask    int64
	// Question and Answer are the ask's, whole: the note cuts them short.
	Question, Answer string
	// Open are the thread's other asks that still wait for the owner, by
	// id, oldest first.
	Open []int64
	// Plan is the plan the thread leads, nil for none.
	Plan *AnsweredPlan
}

// AnsweredPlan is the plan an asking thread leads, as the answer left it.
type AnsweredPlan struct {
	ID int64
	// State is the plan's state (PlanDrafting, PlanApproved …).
	State string
	// Cleared says the answer left the approved plan nothing in its gate;
	// Ready are its tasks the supervisor may deliver then.
	Cleared bool
	Ready   []Task
}

// AnsweredNote is the note that tells a thread's supervisor of the owner's
// answer: the thread, the ask, the agent, the question and the answer cut
// short, that the answer goes to the agent's session, and the thread's
// next step.
func AnsweredNote(a Answered) string {
	var b strings.Builder
	name := "T" + strconv.FormatInt(a.Thread, 10)
	if title := flat(a.Title, inboxTitle); title != "" {
		name += " " + strconv.Quote(title)
	}
	fmt.Fprintf(&b, "[T%d · A%d answered] The owner answered %s's ask A%d in %s.\n", a.Thread, a.Ask, a.Agent, a.Ask, name)
	fmt.Fprintf(&b, "Question: %q\nAnswer: %q\n", flat(a.Question, answeredQuestion), flat(a.Answer, answeredAnswer))
	fmt.Fprintf(&b, "pocketd brings the answer to %s's session. %s", a.Agent, a.next())
	return b.String()
}

// next is the thread's next step once the answer is in: the first that
// holds of a closed thread, the thread's other open asks, a plan in
// drafting, an approved plan the answer cleared, and the specialist going
// on.
func (a Answered) next() string {
	thread := "T" + strconv.FormatInt(a.Thread, 10)
	switch p := a.Plan; {
	case a.Closed:
		return fmt.Sprintf("%s is closed, so %s files no report on it: if the answer needs more work, team_send reopens %s where pocketd allows it, or delegate a thread that follows %s.",
			thread, a.Agent, thread, thread)
	case len(a.Open) > 0:
		ids := make([]string, len(a.Open))
		for i, id := range a.Open {
			ids[i] = "A" + strconv.FormatInt(id, 10)
		}
		verb := "waits"
		if len(ids) > 1 {
			verb = "wait"
		}
		return fmt.Sprintf("%s of %s still %s for the owner.", joinAnd(ids), thread, verb)
	case p != nil && p.State == PlanDrafting:
		return fmt.Sprintf("%s writes the answer into plan P%d with team_plan; the plan goes to the owner for approval once nothing in it is open.", a.Agent, p.ID)
	case p != nil && p.Cleared && len(p.Ready) > 0:
		return fmt.Sprintf("Plan P%d has no open question now: its ready tasks may go. %s", p.ID, deliverLine(p.ID, p.Ready))
	case p != nil && p.Cleared:
		return fmt.Sprintf("Plan P%d has no open question now, and none of its tasks is ready to deliver.", p.ID)
	}
	return fmt.Sprintf("%s goes on with %s once the answer is in its session, and its next report comes to you.", a.Agent, thread)
}
