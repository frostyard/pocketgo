package team

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A supervisor's message to a thread it delegated (ADR-0032): team_send
// keeps it in the outbox, which places it in the thread's specialist
// session once, as it places the owner's answers.

// Send is a team_send call.
type Send struct {
	Thread int64  `json:"thread"`
	Text   string `json:"text"`
}

// MaxSend is the most characters a supervisor's message has: as many as a
// report's (MaxMessage).
const MaxSend = MaxMessage

// CheckSend refuses a team_send by its caller and its text, whatever the
// thread: a caller that is no supervisor (caller is the calling session's
// profile, nil for a session without one), no thread named, and a text
// that is empty or longer than MaxSend.
func CheckSend(caller *profiles.Profile, s Send) error {
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return errors.New("only a supervisor sends to a thread: this session's profile is not one")
	}
	text := strings.TrimSpace(s.Text)
	switch {
	case s.Thread <= 0:
		return errors.New("name the thread by its number: 12 for T12")
	case text == "":
		return errors.New("the message is empty")
	case utf8.RuneCountInString(text) > MaxSend:
		return fmt.Errorf("the message has %d characters, more than %d", utf8.RuneCountInString(text), MaxSend)
	}
	return nil
}

// SendTarget is what CheckSendTarget reads of the thread a message names.
type SendTarget struct {
	ID int64
	// State is the thread's state: starting, open, done, blocked or
	// closed.
	State string
	// Review says pocketd started it as a cross-review (ADR-0019), which
	// no supervisor delegated.
	Review bool
	// Own says a session of the caller's own profile delegated it, as
	// team_close asks.
	Own bool
}

// CheckSendTarget refuses a thread a supervisor's message cannot go to: a
// cross-review, one another supervisor delegated, one still starting, whose
// specialist has no session to take it yet, and a closed one. An open, a
// done and a blocked thread take one.
func CheckSendTarget(t SendTarget) error {
	switch {
	case t.Review:
		return fmt.Errorf("thread T%d is a cross-review pocketd runs: no supervisor sends to it", t.ID)
	case !t.Own:
		return fmt.Errorf("thread T%d was delegated by another supervisor", t.ID)
	case t.State == "closed":
		return fmt.Errorf("thread T%d is closed: delegate the work again in a new thread", t.ID)
	case t.State == "starting":
		return fmt.Errorf("thread T%d is still starting: send once team_delegate has answered", t.ID)
	}
	return nil
}

// sendPrefix begins the outbox key of each supervisor's message, and so
// the request key of every submission that carries one: SendKey writes
// it, SentBy reads it.
const sendPrefix = "team-send:"

// SendKey is the outbox key of the message of team_send call (its key:
// session, tool task and call id), from supervisor session supervisor.
// The key names the supervisor's session first, so the request key of
// each submission that places it says who sent it (SentBy). No other
// message pocketd keeps or the owner sends has a key of this prefix.
func SendKey(supervisor int64, call string) string {
	return sendPrefix + strconv.FormatInt(supervisor, 10) + ":" + call
}

// IsSendKey says whether key is a supervisor's message's outbox key.
func IsSendKey(key string) bool { return strings.HasPrefix(key, sendPrefix) }

// SentBy names the supervisor's session whose message a submission carries,
// by the submission's request key (SendKey, with the suffixes the outbox
// adds); ok is false for every other key, the owner's messages included.
func SentBy(requestKey string) (supervisor int64, ok bool) {
	rest, found := strings.CutPrefix(requestKey, sendPrefix)
	if !found {
		return 0, false
	}
	id, _, found := strings.Cut(rest, ":")
	if !found {
		return 0, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		return 0, false
	}
	return n, true
}

// SendText is the message a supervisor's team_send brings its thread's
// specialist: who sent it, the text, what it does to a blocked thread, and
// that a report answers it, since the thread's next report is expected.
// The text is kept before pocketd knows the thread's state when it is
// placed, so the blocked line says the rule, never which asks it
// withdraws (ADR-0037).
func SendText(thread int64, supervisor, text string) string {
	return fmt.Sprintf("[T%d · %s, the supervisor] %s\n\n"+
		"If T%d is blocked when this reaches you, this message unblocks it: pocketd withdraws its asks that wait for the owner, who will not answer them, "+
		"and T%d is open again. When you have acted on it, report with team_message.", thread, supervisor, strings.TrimSpace(text), thread, thread)
}

// AnswerWithdrawn is the refusal of the owner's answer to ask, of thread,
// that a supervisor's message withdrew first (ADR-0037).
func AnswerWithdrawn(ask, thread int64) string {
	return fmt.Sprintf("A%d was withdrawn: the supervisor's message reached T%d's specialist and unblocked the thread before your answer came, "+
		"so pocketd did not send your answer", ask, thread)
}

// Where a supervisor's message is, as team_send's answer says it.
const (
	// SendQueued: kept, and not in the specialist's transcript yet.
	SendQueued = iota
	// SendDelivered: in the specialist's transcript.
	SendDelivered
	// SendUnconfirmed: kept, and pocketd could not tell where it is.
	SendUnconfirmed
	// SendUnknown: pocketd could not tell whether this call's message was
	// kept, as for a call run again whose lookup failed.
	SendUnknown
)

// SentReceipt is what team_send's answer says.
type SentReceipt struct {
	Thread int64
	Agent  string
	State  int
	// ThreadState is the thread's state as it is now, for a delivered
	// message: open, done, blocked or closed.
	ThreadState string
	// Caller is the session that sent it, Delegator the one that
	// delegated the thread, where its reports go.
	Caller, Delegator int64
	// Unblocks says the message was kept with the text that tells its
	// specialist it unblocks a blocked thread, so its delivery may; one
	// kept before that takes only a done thread back to open (ADR-0037;
	// Codex review of #62, round 2).
	Unblocks bool
	// Unblocked says the delivered message's noting took its blocked
	// thread back to open (ADR-0037), and Withdrawn are the asks it
	// withdrew then, oldest first, at least the first MaxWithdrawnNamed of
	// them; WithdrawnTotal is how many in all, when more than Withdrawn
	// holds. WithdrawnUnread says pocketd could not read them.
	Unblocked       bool
	Withdrawn       []WithdrawnAsk
	WithdrawnTotal  int
	WithdrawnUnread bool
}

// WithdrawnAsk is an ask a supervisor's message withdrew, as team_send's
// answer names it.
type WithdrawnAsk struct {
	ID       int64
	Question string
}

// SentText is team_send's answer: where the message is, the thread's
// state now once it is delivered, never which message moved it, and where
// the specialist's reports go. A message pocketd keeps is never told as
// failed: a supervisor would send it again (Codex review of #56, round 1).
func SentText(r SentReceipt) string {
	var b strings.Builder
	switch r.State {
	case SendDelivered:
		fmt.Fprintf(&b, "Delivered to thread T%d: the message is in %s's session.", r.Thread, r.Agent)
		if r.Unblocked {
			b.WriteString(unblockedText(r))
		}
		switch r.ThreadState {
		case "open":
			fmt.Fprintf(&b, " T%d is open: its next report is expected.", r.Thread)
		case "done", "blocked", "closed":
			fmt.Fprintf(&b, " T%d is %s now.", r.Thread, r.ThreadState)
		}
	case SendQueued:
		fmt.Fprintf(&b, "Queued for thread T%d: pocketd delivers it to %s's session once, as soon as it can take it; ", r.Thread, r.Agent)
		if r.Unblocks {
			b.WriteString("a done or blocked thread is open again then, and a blocked one's asks that wait for the owner are withdrawn.")
		} else {
			b.WriteString("a done thread is open again then.")
		}
	case SendUnknown:
		// Neither kept nor failed: a call run again may have kept it, and
		// a second call would send it twice (Codex review of #56, round 2).
		return fmt.Sprintf("pocketd could not tell whether this message for thread T%d was kept: if it was, it reaches %s's session once. "+
			"Do not send it again unless the specialist's next report shows it never arrived.", r.Thread, r.Agent)
	default:
		if r.Thread == 0 {
			return "Kept: pocketd delivers the message once, but could not tell yet where it is. Do not send it again."
		}
		fmt.Fprintf(&b, "Kept for thread T%d: pocketd delivers it to %s's session once, but could not tell yet where it is. Do not send it again.", r.Thread, r.Agent)
	}
	switch {
	case r.Delegator == 0 || r.Thread == 0:
	case r.Delegator == r.Caller:
		b.WriteString(" Its reports come back to you as messages.")
	default:
		fmt.Fprintf(&b, " Its reports go to session %d, which delegated T%d, not to this one.", r.Delegator, r.Thread)
	}
	return b.String()
}

// unblockedText is what team_send's answer says of a delivered message
// that unblocked its thread (ADR-0037): which asks it withdrew from the
// owner's Inbox, by number and question, or that none waited.
func unblockedText(r SentReceipt) string {
	var b strings.Builder
	fmt.Fprintf(&b, " It unblocked T%d, which was blocked", r.Thread)
	switch {
	case r.WithdrawnUnread:
		b.WriteString(": pocketd withdrew its asks that waited for the owner, but could not read which.")
	case len(r.Withdrawn) == 0:
		b.WriteString("; no ask of it waited for the owner.")
	default:
		// Bounded, whatever the thread asked over its reports: the first
		// asks by number and question, cut short, then more by number
		// alone, then a count (Codex review of #62, round 1).
		total := max(len(r.Withdrawn), r.WithdrawnTotal)
		names := make([]string, 0, min(len(r.Withdrawn), MaxWithdrawnNamed)+1)
		for i, a := range r.Withdrawn {
			switch {
			case i < MaxWithdrawnQuestions:
				names = append(names, fmt.Sprintf("A%d (%q)", a.ID, cut(a.Question, MaxWithdrawnQuestion)))
			case i < MaxWithdrawnNamed:
				names = append(names, fmt.Sprintf("A%d", a.ID))
			}
		}
		if more := total - min(len(r.Withdrawn), MaxWithdrawnNamed); more > 0 {
			names = append(names, fmt.Sprintf("%d more", more))
		}
		fmt.Fprintf(&b, ": pocketd withdrew %s from the owner's Inbox, and the owner will not answer ", joinAnd(names))
		if total == 1 {
			b.WriteString("it.")
		} else {
			b.WriteString("them.")
		}
	}
	return b.String()
}

// The most of the withdrawn asks team_send's answer names (unblockedText):
// MaxWithdrawnQuestions by number and question, each question cut to
// MaxWithdrawnQuestion characters, then MaxWithdrawnNumbers by number
// alone, so the answer stays well within a tool result's budget however
// many asks the thread had.
const (
	MaxWithdrawnQuestions = 8
	MaxWithdrawnQuestion  = 160
	MaxWithdrawnNumbers   = 100
	// MaxWithdrawnNamed is how many it names in all.
	MaxWithdrawnNamed = MaxWithdrawnQuestions + MaxWithdrawnNumbers
)

// cut is text cut to at most n characters, with an ellipsis when cut.
func cut(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n-1]) + "…"
}

// joinAnd joins names as a list: "a", "a and b", "a, b and c".
func joinAnd(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
