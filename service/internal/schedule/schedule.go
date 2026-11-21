package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The bounds of a schedule's message and of a session's own schedules
// (ADR-0030).
const (
	// MaxText is the most characters a scheduled message has.
	MaxText = 4000
	// MaxPerSession is the most schedules a supervisor's session keeps
	// that still go out: a runaway loop of the model's own reminders costs
	// money. The owner's schedules count apart.
	MaxPerSession = 50
)

// Prefix starts every scheduled message, so the agent, and the owner
// reading along, know it was set up earlier.
const Prefix = "[scheduled] "

// Guide tells a session what a scheduled message is.
const Guide = "A message that starts with [scheduled] was set up earlier, by the owner's schedules or by you: " +
	"nobody may be watching, so do the work, then end with a short summary that stands on its own."

// Message is the text pocketd sends for a schedule's message text.
func Message(text string) string { return Prefix + text }

// CheckText is a schedule's message as it goes: trimmed, 1 to MaxText
// characters.
func CheckText(text string) (string, error) {
	text = strings.TrimSpace(text)
	switch n := utf8.RuneCountInString(text); {
	case !utf8.ValidString(text):
		return "", errors.New("the message is not UTF-8")
	case n == 0:
		return "", errors.New("say what the message is")
	case n > MaxText:
		return "", fmt.Errorf("a scheduled message has at most %d characters, not %d", MaxText, n)
	}
	return text, nil
}

// Actions of team_schedule.
const (
	ActionAdd    = "add"
	ActionList   = "list"
	ActionCancel = "cancel"
)

// Call is a team_schedule call: add a schedule (When, Message), list the
// session's schedules, or cancel one of its own (ID).
type Call struct {
	Action  string `json:"action"`
	When    string `json:"when,omitempty"`
	Message string `json:"message,omitempty"`
	ID      string `json:"id,omitempty"`
}

// CheckCall checks that a call has its action's fields, and only those.
func CheckCall(c Call) error {
	switch c.Action {
	case ActionAdd:
		if c.When == "" || c.Message == "" {
			return errors.New("add takes when and message")
		}
		if c.ID != "" {
			return errors.New("add takes when and message, no id")
		}
	case ActionList:
		if c.When != "" || c.Message != "" || c.ID != "" {
			return errors.New("list takes nothing else")
		}
	case ActionCancel:
		if c.ID == "" {
			return errors.New("cancel takes the schedule's id, as S3")
		}
		if c.When != "" || c.Message != "" {
			return errors.New("cancel takes the schedule's id alone")
		}
	default:
		return fmt.Errorf("the action %q is none of add, list and cancel", c.Action)
	}
	return nil
}

// ID names a session's own schedule: S3.
func ID(n int64) string { return "S" + strconv.FormatInt(n, 10) }

// ParseID reads a session's schedule id, as S3 or 3.
func ParseID(id string) (int64, error) {
	digits := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(id), "S"), "s")
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != digits {
		return 0, fmt.Errorf("%q is no schedule id: one is S3", id)
	}
	return n, nil
}

// Line is a schedule as team_schedule lists it.
type Line struct {
	// ID is S3 for a session's own, or the owner's id in their schedules
	// file.
	ID string
	// Owners says it is one of the owner's schedules, which the owner
	// changes in their file.
	Owners bool
	When   string
	Zone   *time.Location
	// Next is when it goes out next; zero for none.
	Next    time.Time
	Skipped int
	Text    string
	// Problem says why it does not go out, "" when it does.
	Problem string
}

func (l Line) String() string {
	var b strings.Builder
	b.WriteString(l.ID)
	if l.Owners {
		b.WriteString(" (the owner's)")
	}
	b.WriteString(": " + l.When)
	if !l.Next.IsZero() {
		b.WriteString(", next " + When(l.Next, l.Zone))
	}
	if l.Skipped > 0 {
		fmt.Fprintf(&b, ", skipped %d times while its last message waited", l.Skipped)
	}
	if l.Problem != "" {
		b.WriteString(", does not go out: " + l.Problem)
	}
	b.WriteString(" · " + l.Text)
	return b.String()
}

// Describe says when a schedule goes out, from its spec, as written in its
// zone: the repeat, or the one time it goes out.
func Describe(when string, first time.Time, zone *time.Location) string {
	if spec, err := Parse(when); err == nil && spec.Repeat != nil {
		return spec.Repeat.String() + " (" + zone.String() + ")"
	}
	return "once, " + When(first, zone)
}

// AddedText is team_schedule's answer to an add: it depends only on the
// schedule as it was made, so the call run again answers as the first did.
func AddedText(id, when string, first time.Time, zone *time.Location) string {
	return fmt.Sprintf("Scheduled %s: %s. It comes to this session as a message that starts with %q; cancel it with team_schedule cancel %s.",
		id, Describe(when, first, zone), strings.TrimSpace(Prefix), id)
}

// ListText is team_schedule's answer to a list.
func ListText(lines []Line) string {
	if len(lines) == 0 {
		return "Nothing is scheduled for this session."
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.String()
	}
	return strings.Join(out, "\n")
}

// CancelledText is team_schedule's answer to a cancel: it depends only on
// the schedule, so the call run again answers as the first did.
func CancelledText(id string, done bool) string {
	if done {
		return fmt.Sprintf("%s went out already and does not repeat: nothing is left to cancel.", id)
	}
	return fmt.Sprintf("Cancelled %s: it sends nothing more.", id)
}
