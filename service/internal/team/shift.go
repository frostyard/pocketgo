package team

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Shift is a delegation's shift (ADR-0024): its thread runs in rounds
// toward the task, until Stop holds or a limit is reached.
type Shift struct {
	// Stop is the completion condition the specialist judges.
	Stop string `json:"stop"`
	// MaxRounds bounds the rounds; 0 is DefaultShiftRounds.
	MaxRounds int `json:"max_rounds,omitempty"`
	// Hours is the deadline after the thread is recorded; 0 is none.
	Hours float64 `json:"hours,omitempty"`
	// CI says done also needs CI green on each pull request the thread
	// links, and at least one.
	CI bool `json:"ci,omitempty"`
}

// The bounds of a shift (ADR-0024).
const (
	MaxShiftStop       = 3000
	DefaultShiftRounds = 20
	MaxShiftRounds     = 100
	MaxShiftHours      = 24
	// ShiftFailures is how many rounds in a row may see the same failing
	// checks before the shift ends.
	ShiftFailures = 3
	// MaxShiftWhy bounds a round's reason as recorded.
	MaxShiftWhy = 500
)

// A shift's states.
const (
	ShiftRunning = "running"
	ShiftSummary = "summary"
	ShiftPaused  = "paused"
	ShiftEnded   = "ended"
)

// Why a shift stopped.
const (
	ShiftDone        = "done"
	ShiftBlocked     = "blocked"
	ShiftMaxRounds   = "max-rounds"
	ShiftDeadline    = "deadline"
	ShiftFailedCheck = "failed-check"
	ShiftFailed      = "failed"
	ShiftPerson      = "person"
	ShiftStopped     = "stopped"
	ShiftClosed      = "closed"
)

// A round's verdict, the last STATUS line of the specialist's answer.
const (
	VerdictDone     = "done"
	VerdictContinue = "continue"
	VerdictBlocked  = "blocked"
	// VerdictNone is an answer with no STATUS line, which goes on as
	// continue.
	VerdictNone = "none"
)

// CheckShift refuses a shift that breaks a rule, and fills its defaults.
func CheckShift(s Shift) (Shift, error) {
	stop := strings.TrimSpace(s.Stop)
	switch {
	case stop == "":
		return Shift{}, errors.New("the shift's stop condition is empty: say when the work is done")
	case utf8.RuneCountInString(stop) > MaxShiftStop:
		return Shift{}, fmt.Errorf("the shift's stop condition has %d characters, more than %d", utf8.RuneCountInString(stop), MaxShiftStop)
	case s.MaxRounds < 0 || s.MaxRounds > MaxShiftRounds:
		return Shift{}, fmt.Errorf("the shift's max_rounds is %d, not 1 to %d", s.MaxRounds, MaxShiftRounds)
	case math.IsNaN(s.Hours) || s.Hours < 0 || s.Hours > MaxShiftHours:
		return Shift{}, fmt.Errorf("the shift's hours are not above 0 and at most %d", MaxShiftHours)
	}
	s.Stop = stop
	if s.MaxRounds == 0 {
		s.MaxRounds = DefaultShiftRounds
	}
	return s, nil
}

// Deadline is the shift's deadline for a thread recorded at, zero for
// none.
func (s Shift) Deadline(at time.Time) time.Time {
	if s.Hours <= 0 {
		return time.Time{}
	}
	return at.Add(time.Duration(s.Hours * float64(time.Hour)))
}

// ParseShiftStatus reads a round's answer for its last line of the form
// STATUS: DONE|CONTINUE|BLOCKED[: reason], with any emphasis around it.
// No such line is VerdictNone. No model judges it (ADR-0011).
func ParseShiftStatus(text string) (verdict, reason string) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.Trim(strings.TrimSpace(lines[i]), "*_`> ")
		head, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(head), "STATUS") {
			continue
		}
		word, why, _ := strings.Cut(strings.TrimSpace(rest), ":")
		word = strings.Trim(strings.TrimSpace(word), "*_` .")
		why = Clip(strings.Trim(strings.TrimSpace(why), "*_`"), MaxShiftWhy)
		switch strings.ToUpper(word) {
		case "DONE":
			return VerdictDone, why
		case "CONTINUE":
			return VerdictContinue, why
		case "BLOCKED":
			return VerdictBlocked, why
		}
	}
	return VerdictNone, ""
}

// ShiftFingerprint names a set of failing checks, in any order: the same
// set seen again is the same failure.
func ShiftFingerprint(failing []string) string {
	sorted := slices.Clone(failing)
	slices.Sort(sorted)
	return strings.Join(sorted, "\n")
}

// ShiftRules is what a shift's brief adds: how its rounds work, and the
// line each answer ends with.
func ShiftRules(s Shift, deadline time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This thread is a shift: you work in rounds, at most %d", s.MaxRounds)
	if !deadline.IsZero() {
		fmt.Fprintf(&b, ", until %s", deadline.UTC().Format("2006-01-02 15:04 UTC"))
	}
	fmt.Fprintf(&b, ". This message is round 1. The shift is done when: %s\n", s.Stop)
	if s.CI {
		b.WriteString("Done also needs CI green on each pull request you report for this thread, and at least one: pocketd reads them on GitHub.\n")
	}
	b.WriteString("Each round, do the next piece of the work, then end your turn with one last line: STATUS: DONE when the stop condition holds, " +
		"STATUS: CONTINUE when work is left, or STATUS: BLOCKED: <why> when you cannot go on without the owner. pocketd sends the next round when your turn, " +
		"and your worker's, have ended. Send no team_message with status done or blocked before pocketd asks for your report, in the shift's summary round; " +
		"a working report with asks is fine. End your turn rather than wait, sleep or poll.")
	return b.String()
}

// ShiftRoundText is round n's message to the specialist. failing names
// the checks of the thread's pull requests that failed, when done waited
// on them.
func ShiftRoundText(thread int64, n, max int, deadline time.Time, stop string, failing []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[shift] T%d · Round %d of %d", thread, n, max)
	if !deadline.IsZero() {
		fmt.Fprintf(&b, ", until %s", deadline.UTC().Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString(".")
	if len(failing) > 0 {
		fmt.Fprintf(&b, " CI is not green yet on the thread's pull requests: %s fail.", strings.Join(failing, ", "))
	}
	fmt.Fprintf(&b, " Go on with the work. The shift is done when: %s\nEnd your turn with STATUS: DONE, STATUS: CONTINUE or STATUS: BLOCKED: <why>.", stop)
	return b.String()
}

// ShiftWhy says in words why a shift stopped.
func ShiftWhy(reason, detail string, round, max int) string {
	var why string
	switch reason {
	case ShiftDone:
		why = "the stop condition holds"
	case ShiftBlocked:
		why = "the work is blocked"
	case ShiftMaxRounds:
		why = fmt.Sprintf("it reached its %d rounds", max)
	case ShiftDeadline:
		why = "it reached its deadline"
	case ShiftFailedCheck:
		why = fmt.Sprintf("the same checks failed %d rounds in a row", ShiftFailures)
	case ShiftFailed:
		why = "its run failed"
	case ShiftStopped:
		why = "the owner stopped it"
	case ShiftClosed:
		why = "its thread closed"
	case ShiftPerson:
		why = "the owner wrote to its session"
	default:
		why = reason
	}
	if detail = strings.TrimSpace(detail); detail != "" {
		why += ": " + detail
	}
	return Clip(why, MaxShiftWhy)
}

// ShiftSummaryText is the summary round's message: why the shift stopped,
// and the one report it asks for.
func ShiftSummaryText(thread int64, why string, round, max int, reason string) string {
	status := StatusDone
	if reason != ShiftDone {
		status = StatusBlocked
	}
	return fmt.Sprintf("[shift] T%d · The shift has stopped: %s. It took %d of %d rounds. "+
		"Write your report now, with one team_message, status %s: what you did, what is left, the pull requests, "+
		"and each decision you need from the owner as an ask. Then end your turn.", thread, why, round, max, status)
}

// ShiftEndedReport is the report pocketd files for a shift whose summary
// round ended with none, or that the owner stopped.
func ShiftEndedReport(why string, round, max int) string {
	return fmt.Sprintf("The shift ended: %s. It took %d of %d rounds.", why, round, max)
}
