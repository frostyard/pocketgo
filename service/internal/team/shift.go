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

// Shift is a delegation's shift as team_delegate takes it (ADR-0024): its
// thread runs in rounds toward the task, until Stop holds or a limit is
// reached. A limit left out is nil, apart from one given as 0, which is
// refused (Codex review of #36).
type Shift struct {
	// Stop is the completion condition the specialist judges.
	Stop string `json:"stop"`
	// MaxRounds bounds the rounds; nil is DefaultShiftRounds.
	MaxRounds *int `json:"max_rounds,omitempty"`
	// Hours is the deadline after the thread is recorded; nil is none.
	Hours *float64 `json:"hours,omitempty"`
	// CI says done also needs CI green on each pull request the thread
	// links, and at least one.
	CI bool `json:"ci,omitempty"`
}

// ShiftPlan is a shift as checked, with its defaults.
type ShiftPlan struct {
	Stop      string
	MaxRounds int
	// Hours is 0 for no deadline.
	Hours float64
	CI    bool
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

// CheckShift refuses a shift that breaks a rule, and fills its defaults:
// a limit given is a limit, so 0 is refused.
func CheckShift(s Shift) (ShiftPlan, error) {
	stop := strings.TrimSpace(s.Stop)
	plan := ShiftPlan{Stop: stop, MaxRounds: DefaultShiftRounds, CI: s.CI}
	switch {
	case stop == "":
		return ShiftPlan{}, errors.New("the shift's stop condition is empty: say when the work is done")
	case utf8.RuneCountInString(stop) > MaxShiftStop:
		return ShiftPlan{}, fmt.Errorf("the shift's stop condition has %d characters, more than %d", utf8.RuneCountInString(stop), MaxShiftStop)
	}
	if s.MaxRounds != nil {
		if *s.MaxRounds < 1 || *s.MaxRounds > MaxShiftRounds {
			return ShiftPlan{}, fmt.Errorf("the shift's max_rounds is %d, not 1 to %d", *s.MaxRounds, MaxShiftRounds)
		}
		plan.MaxRounds = *s.MaxRounds
	}
	if s.Hours != nil {
		if h := *s.Hours; math.IsNaN(h) || h <= 0 || h > MaxShiftHours {
			return ShiftPlan{}, fmt.Errorf("the shift's hours are not above 0 and at most %d", MaxShiftHours)
		}
		plan.Hours = *s.Hours
	}
	return plan, nil
}

// Deadline is the shift's deadline for a thread recorded at, zero for
// none.
func (s ShiftPlan) Deadline(at time.Time) time.Time {
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
func ShiftRules(s ShiftPlan, deadline time.Time) string {
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

// ShiftReportStatus is the status of the one report a shift stopped for
// reason takes: done for done, blocked for any other stop.
func ShiftReportStatus(reason string) string {
	if reason == ShiftDone {
		return StatusDone
	}
	return StatusBlocked
}

// CheckShiftReport says whether a thread whose shift is in state, stopped
// for reason at round of max, takes a new report of status (ADR-0024):
// while the shift runs rounds, no final report (done or blocked), since
// pocketd asks for it in the summary round; in that round, a final report
// only with the status its stop asks for (ShiftReportStatus). A working
// report, with asks, goes any time, and so does any report once the shift
// is paused or ended.
func CheckShiftReport(thread int64, state, reason string, round, max int, status string) error {
	if status != StatusDone && status != StatusBlocked {
		return nil
	}
	switch state {
	case ShiftRunning:
		return fmt.Errorf("thread T%d runs a shift, round %d of %d: end your turn with STATUS: DONE, STATUS: CONTINUE or STATUS: BLOCKED: <why>, "+
			"and pocketd asks for your report in the shift's summary round. A working report with asks is fine", thread, round, max)
	case ShiftSummary:
		if want := ShiftReportStatus(reason); status != want {
			return fmt.Errorf("thread T%d's shift stopped as %s: report with status %s", thread, reason, want)
		}
	}
	return nil
}

// ShiftAsk is one of a thread's asks as a shift's summary names it.
type ShiftAsk struct {
	ID       int64
	Question string
	// Answer is the owner's answer, empty for an open ask.
	Answer string
}

// The most of a thread's asks a summary round's message names
// (ShiftSummaryText), per list: MaxSummaryAsks by number and text, each
// question cut to MaxSummaryQuestion characters and each answer to
// MaxSummaryAnswer, then MaxSummaryNumbers by number alone, then a count.
const (
	MaxSummaryAsks     = 8
	MaxSummaryQuestion = 200
	MaxSummaryAnswer   = 300
	MaxSummaryNumbers  = 100
)

// ShiftSummaryText is the summary round's message: why the shift stopped,
// and the report it needs, with the thread's open and recently answered asks.
func ShiftSummaryText(thread int64, why string, round, max int, reason string, open, answered []ShiftAsk) string {
	status := ShiftReportStatus(reason)
	text := fmt.Sprintf("[shift] T%d · The shift has stopped: %s. It took %d of %d rounds. "+
		"Write your report now, with one team_message, status %s: what you did, what is left, the pull requests, "+
		"and each new decision you need from the owner as an ask. Then end your turn.", thread, why, round, max, status)
	if len(open) > 0 {
		text += "\n\nThese asks of this thread wait for the owner already. Do not file them again: an ask that repeats one is filed as that one, and its answer comes to you as a message.\n" + summaryAskLines(open, false)
	}
	if len(answered) > 0 {
		text += "\n\nThe owner answered these asks since the shift began. Act on the answers, and do not ask them again:\n" + summaryAskLines(answered, true)
	}
	return text
}

func summaryAskLines(asks []ShiftAsk, answered bool) string {
	var lines []string
	for _, a := range asks[:min(len(asks), MaxSummaryAsks)] {
		line := fmt.Sprintf("- A%d: %q", a.ID, cut(a.Question, MaxSummaryQuestion))
		if answered {
			line += fmt.Sprintf(" → %q", cut(a.Answer, MaxSummaryAnswer))
		}
		lines = append(lines, line)
	}
	end := min(len(asks), MaxSummaryAsks+MaxSummaryNumbers)
	if len(asks) > MaxSummaryAsks {
		var numbers []string
		for _, a := range asks[MaxSummaryAsks:end] {
			numbers = append(numbers, fmt.Sprintf("A%d", a.ID))
		}
		lines = append(lines, "- also "+strings.Join(numbers, ", "))
	}
	if len(asks) > end {
		lines = append(lines, fmt.Sprintf("- and %d more", len(asks)-end))
	}
	return strings.Join(lines, "\n")
}

// ShiftEndedReport is the report pocketd files for a shift whose summary
// round ended with none, or that the owner stopped.
func ShiftEndedReport(why string, round, max int) string {
	return fmt.Sprintf("The shift ended: %s. It took %d of %d rounds.", why, round, max)
}

// ShiftClosedNote is the supervisor's note when a thread whose shift was
// not ended closes: a closed thread takes no report.
func ShiftClosedNote(thread int64, round, max int) string {
	return fmt.Sprintf("[T%d · shift] The shift ended: its thread closed. It took %d of %d rounds.", thread, round, max)
}

// ShiftExpiredNote is the supervisor's note when a shift's deadline passed
// before its brief reached the specialist: its thread closed, and no work
// began.
func ShiftExpiredNote(thread int64, why string) string {
	return fmt.Sprintf("[T%d · shift] The shift ended before it began: %s. Its thread closed.", thread, why)
}
