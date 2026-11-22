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
	// MaxPerSession is the most schedules a session keeps that still go
	// out, a supervisor's or a private agent's: a runaway loop of the
	// model's own reminders costs money. The owner's schedules count apart.
	MaxPerSession = 50
	// MaxPageBytes and MaxPageLines bound a page of a schedule tool's list:
	// a tool result stays far below what PiG keeps of one (50 KiB or 2,000
	// lines), so no schedule is lost to a cut (Codex review of #51, round
	// 3).
	MaxPageBytes = 16 << 10
	MaxPageLines = 400
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

// The tools that keep a session's own schedules, with one engine and one
// set of limits: a supervisor's team_schedule (ADR-0030) and a private
// agent's schedule (ADR-0060). Their answers name the tool that was
// called.
const (
	TeamTool = "team_schedule"
	OwnTool  = "schedule"
)

// Actions of a schedule tool.
const (
	ActionAdd    = "add"
	ActionList   = "list"
	ActionCancel = "cancel"
)

// Call is a schedule tool's call: add a schedule (When, Message), list the
// session's schedules, or cancel one of its own (ID).
type Call struct {
	Action  string `json:"action"`
	When    string `json:"when,omitempty"`
	Message string `json:"message,omitempty"`
	ID      string `json:"id,omitempty"`
	// Page is the page of a list, from 1; 0 is the first.
	Page int `json:"page,omitempty"`
}

// CheckCall checks that a call has its action's fields, and only those.
func CheckCall(c Call) error {
	if c.Page != 0 && c.Action != ActionList {
		return fmt.Errorf("%s takes no page: only list does", c.Action)
	}
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
			return errors.New("list takes a page alone")
		}
		if c.Page < 0 {
			return errors.New("a page is 1 or more")
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
	// One S, in either case, at most: Ss3 is no id of S3 (Codex review of
	// #51, round 2).
	digits := strings.TrimSpace(id)
	if strings.HasPrefix(digits, "S") || strings.HasPrefix(digits, "s") {
		digits = digits[1:]
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != digits {
		return 0, fmt.Errorf("%q is no schedule id: one is S3", id)
	}
	return n, nil
}

// Line is a schedule as a schedule tool lists it.
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
	// LastSkip is why its last time skipped, as recorded then, when it
	// did: what held it then, a message or a gather, whatever its action
	// is now (Codex review of #57, round 4).
	LastSkip string
	// Text is its message, or which gather it starts (GatherText;
	// ADR-0033).
	Text string
	// Problem says why it does not go out, "" when it does.
	Problem string
}

// GatherText is a schedule's gather as a schedule tool's list shows it: its
// skill, the agents it asks, and its note (Codex review of #57, round 2).
func GatherText(skill string, agents []string, note string) string {
	text := "a gather with the skill " + skill + ", asking "
	if len(agents) == 0 {
		text += "every agent the supervisor can delegate to"
	} else {
		text += strings.Join(agents, ", ")
	}
	if note != "" {
		text += fmt.Sprintf("; its note: %q", note)
	}
	return text
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
		fmt.Fprintf(&b, ", skipped %d times", l.Skipped)
	}
	if l.LastSkip != "" {
		fmt.Fprintf(&b, " (the last time because %s)", l.LastSkip)
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

// AddedText is tool's answer to an add (TeamTool or OwnTool): it depends
// only on the tool called and the schedule as it was made, so the call run
// again answers as the first did.
func AddedText(tool, id, when string, first time.Time, zone *time.Location) string {
	return fmt.Sprintf("Scheduled %s: %s. It comes to this session as a message that starts with %q; cancel it with %s cancel %s.",
		id, Describe(when, first, zone), strings.TrimSpace(Prefix), tool, id)
}

// ListText is tool's answer to a list (TeamTool or OwnTool): page page
// (from 1, 0 for the first) of its lines, in pages of at most MaxPageBytes
// bytes and MaxPageLines lines, cut at line ends, each saying whether one
// follows, and how tool reads it. A page past the last is an error.
func ListText(tool string, lines []Line, page int) (string, error) {
	if len(lines) == 0 {
		return "Nothing is scheduled for this session.", nil
	}
	var pages []string
	var b strings.Builder
	n := 0
	for _, l := range lines {
		// A line longer than a page, as a gather that names many agents,
		// goes in parts across pages, none cut (Codex review of #57,
		// round 3). A message's own newlines count as lines too (#127).
		for _, physical := range strings.Split(l.String(), "\n") {
			for _, line := range cutLine(physical, MaxPageBytes) {
				if n > 0 && (b.Len()+1+len(line) > MaxPageBytes || n+1 > MaxPageLines) {
					pages = append(pages, b.String())
					b.Reset()
					n = 0
				}
				if n > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(line)
				n++
			}
		}
	}
	pages = append(pages, b.String())
	if page == 0 {
		page = 1
	}
	if page < 1 || page > len(pages) {
		return "", fmt.Errorf("the list has %d pages: there is no page %d", len(pages), page)
	}
	if len(pages) == 1 {
		return pages[0], nil
	}
	text := fmt.Sprintf("Page %d of %d of this session's schedules.\n%s", page, len(pages), pages[page-1])
	if page < len(pages) {
		text += fmt.Sprintf("\n(More: %s list with page %d.)", tool, page+1)
	}
	return text, nil
}

// cutLine cuts line into parts of at most max bytes, at character
// boundaries.
func cutLine(line string, max int) []string {
	var parts []string
	for len(line) > max {
		at := max
		for at > 0 && !utf8.RuneStart(line[at]) {
			at--
		}
		parts = append(parts, line[:at])
		line = line[at:]
	}
	return append(parts, line)
}

// CancelledText is a schedule tool's answer to a cancel: it depends only on
// the schedule, so the call run again answers as the first did.
func CancelledText(id string, done bool) string {
	if done {
		return fmt.Sprintf("%s went out already and does not repeat: nothing is left to cancel.", id)
	}
	return fmt.Sprintf("Cancelled %s: it sends nothing more.", id)
}
