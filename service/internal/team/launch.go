package team

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A launch (ADR-0034) starts proposals of a supervisor's board as shift
// threads delegated from its standing session, in order, until one end,
// within limits code keeps: how many run at once, the desk's reserve, and
// the rounds they may take together.

// The bounds of a launch.
const (
	// MaxLaunchItems bounds the proposals one launch takes.
	MaxLaunchItems = 20
	// LaunchAtOnce bounds the launch's threads open at once: the next
	// starts as one closes.
	LaunchAtOnce = 3
	// LaunchReserve is the desk's places a launch leaves free, for the
	// owner's and the supervisor's own work.
	LaunchReserve = 2
	// LaunchRounds is a launch's round budget: its proposals' max_rounds
	// added up.
	LaunchRounds = 200
	// DefaultLaunchHours is how long a launch runs when its call names no
	// end.
	DefaultLaunchHours = 4
)

// A launch's states and its items'.
const (
	LaunchRunning = "running"
	LaunchDone    = "done"

	LaunchItemWaiting = "waiting"
	LaunchItemStarted = "started"
	LaunchItemSkipped = "skipped"

	LaunchByOwner      = "owner"
	LaunchBySupervisor = "supervisor"
)

// LaunchEndPassed is why an item skipped at its launch's end.
const LaunchEndPassed = "the launch's end passed before it could start"

// LaunchCall is a team_launch call: rows of the caller's board, in the
// order they start, and the end, in hours from now or a clock time
// (HH:MM, the service's zone), DefaultLaunchHours when it names neither.
type LaunchCall struct {
	Rows  []int64  `json:"rows"`
	Hours *float64 `json:"hours,omitempty"`
	Until string   `json:"until,omitempty"`
}

// CheckLaunchRows refuses proposals no launch takes: none, more than
// MaxLaunchItems, a number that is no row's, or one named twice.
func CheckLaunchRows(rows []int64) error {
	if len(rows) == 0 || len(rows) > MaxLaunchItems {
		return fmt.Errorf("a launch takes 1 to %d proposals, not %d", MaxLaunchItems, len(rows))
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		switch {
		case row <= 0:
			return fmt.Errorf("%d is no proposal's number", row)
		case seen[row]:
			return fmt.Errorf("#%d is named twice", row)
		}
		seen[row] = true
	}
	return nil
}

// LaunchEnd is the end a call names, from now in zone: hours after now, or
// the next time the clock shows until, or DefaultLaunchHours after now.
// Both is refused; the end is then checked (CheckLaunchEnd).
func LaunchEnd(hours *float64, until string, now time.Time, zone *time.Location) (time.Time, error) {
	until = strings.TrimSpace(until)
	switch {
	case hours != nil && until != "":
		return time.Time{}, errors.New("give hours or until, not both")
	case hours != nil:
		if h := *hours; math.IsNaN(h) || math.IsInf(h, 0) || h <= 0 || h > MaxShiftHours {
			return time.Time{}, fmt.Errorf("hours are not above 0 and at most %d", MaxShiftHours)
		}
		return now.Add(time.Duration(*hours * float64(time.Hour))), nil
	case until != "":
		// One daylight saving policy, the desktop's too (Codex review of
		// #58, round 1).
		return proto.NextClockTime(until, now, zone)
	}
	return now.Add(DefaultLaunchHours * time.Hour), nil
}

// CheckLaunchEnd refuses an end that has come, or that is more than
// MaxShiftHours after now: a launch's threads are shifts.
func CheckLaunchEnd(end, now time.Time) error {
	switch {
	case !end.After(now):
		return errors.New("the launch's end has come already: name a later one")
	case end.Sub(now) > MaxShiftHours*time.Hour:
		return fmt.Errorf("the launch's end is more than %d hours away", MaxShiftHours)
	}
	return nil
}

// CheckLaunchRounds refuses proposals whose max_rounds add up to more than
// the launch's round budget.
func CheckLaunchRounds(rounds []int) error {
	total := 0
	for _, r := range rounds {
		total += r
	}
	if total > LaunchRounds {
		return fmt.Errorf("the proposals may take %d rounds together, more than a launch's %d: launch fewer, or lower their max_rounds", total, LaunchRounds)
	}
	return nil
}

// LaunchRoom is how many of a launch's items may start now: fewer than
// LaunchAtOnce of its threads open (running), and the desk's open threads
// (open, the launch's among them) below its cap less LaunchReserve.
func LaunchRoom(open, cap, running int) int {
	return max(0, min(LaunchAtOnce-running, cap-LaunchReserve-open))
}

// LaunchLine is one proposal of a launch, as its texts name it.
type LaunchLine struct {
	ID           int64
	Agent, Title string
}

// LaunchedText is team_launch's answer: it depends only on the launch as
// it was made, so the call run again answers as the first did.
func LaunchedText(id int64, end string, lines []LaunchLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Launch L%d: %d proposals start in this order as shift threads delegated from your standing session, until %s, at most %d at once, while the desk keeps %d places free.",
		id, len(lines), end, LaunchAtOnce, LaunchReserve)
	for _, l := range lines {
		fmt.Fprintf(&b, "\n#%d %s: %s", l.ID, l.Agent, Clip(l.Title, MaxTitle))
	}
	b.WriteString("\nTheir reports come here.")
	return b.String()
}

// LaunchNote is the supervisor's one message of an owner's launch.
func LaunchNote(id int64, end string, lines []LaunchLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[launch L%d] The owner launched %d proposals from your board, until %s. pocketd starts them in this order as shift threads delegated from this session, at most %d at once, while the desk keeps %d places free:",
		id, len(lines), end, LaunchAtOnce, LaunchReserve)
	for _, l := range lines {
		fmt.Fprintf(&b, "\n#%d %s: %s", l.ID, l.Agent, Clip(l.Title, MaxTitle))
	}
	b.WriteString("\nTheir reports come here.")
	return b.String()
}
