package team

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Watches (ADR-0072): a session asks pocketd, with team_watch, to tell it
// when a pull request reaches a condition. The call returns at once, the
// session ends its run, and pocketd checks the pull request in its pull
// rounds, in code. When the condition holds, when the pull request can no
// longer reach it, or when the watch's time is up, pocketd ends the watch
// and files one note to the session, which starts its next run. No tool
// call waits.

// team_watch's actions.
const (
	WatchAdd    = "add"
	WatchList   = "list"
	WatchCancel = "cancel"
)

// What a watch waits for.
const (
	// UntilChecks: CI at the pull request's head, read in each round, is
	// done: green, failing or none, anything but pending.
	UntilChecks = "checks"
	// UntilMerged: the pull request merged.
	UntilMerged = "merged"
	// UntilClosed: the pull request merged, or was closed without merging.
	UntilClosed = "closed"
)

// How a watch ended.
const (
	// WatchMet: the condition held, at the add (no note) or in a round.
	WatchMet = "met"
	// WatchUnreachable: the pull request can no longer reach it: closed
	// without merging while the watch waited for merged, or merged or
	// closed while it waited for checks.
	WatchUnreachable = "unreachable"
	// WatchExpired: the watch's time was up.
	WatchExpired = "expired"
	// WatchCancelled: the session cancelled it (no note).
	WatchCancelled = "cancelled"
	// WatchThreadClosed: the watching session's thread closed (no note).
	WatchThreadClosed = "thread-closed"
	// WatchUnchecked: pocketd reads no pull requests in rounds
	// (-pr-every 0), so nothing would check it.
	WatchUnchecked = "unchecked"
)

// The watches' bounds. They guard pocketd's reads of GitHub, which every
// live watch costs in each round: a pull request's read, and for checks
// up to 10 pages each of check runs and statuses. The reads' budget
// (ReadBudget) bounds what the passes spend in any hour, however many
// pages CI has; the counts bound the rows.
const (
	// DefaultWatchHours is how long a watch waits when the call names no
	// hours; MaxWatchHours the most it may name.
	DefaultWatchHours = 24
	MaxWatchHours     = 168
	// MaxWatchesPerSession and MaxWatches bound the live watches of one
	// session and of pocketd.
	MaxWatchesPerSession = 16
	MaxWatches           = 64
	// MaxWatchesListed is how many ended watches list shows, the newest
	// first, after the live ones.
	MaxWatchesListed = 10
	// WatchReadsBurst is the most GitHub requests the watches' passes
	// spend at once, and WatchReadsPerHour how many more they may spend
	// an hour: no hour sees more than WatchReadBudget of them, under a
	// third of GitHub's 5,000 an hour for the service user's sign-in,
	// which the pull rounds and the agents share.
	WatchReadsBurst   = 400
	WatchReadsPerHour = 1200
	WatchReadBudget   = WatchReadsBurst + WatchReadsPerHour
)

// ReadBudget is the watches' GitHub requests left to spend, a bucket of
// at most WatchReadsBurst refilled at WatchReadsPerHour by the clock: any
// hour spends at most what it held at its start and what it gained in it,
// WatchReadBudget. Its zero value is full.
type ReadBudget struct {
	started bool
	left    float64
	at      time.Time
}

// Take spends one request at now, and says whether one was left. A clock
// that went back refills nothing.
func (b *ReadBudget) Take(now time.Time) bool {
	if !b.Has(now, 1) {
		return false
	}
	b.left--
	return true
}

// Has says whether n requests are left at now, spending none: a watch's
// read starts only when its whole read fits, so the budget builds up for
// a read of many pages rather than spending each refill on its first ones
// (Codex review of #173, round 3). n is at most WatchReadsBurst.
func (b *ReadBudget) Has(now time.Time, n int) bool {
	if !b.started {
		b.started, b.left, b.at = true, WatchReadsBurst, now
	}
	if now.After(b.at) {
		b.left = math.Min(WatchReadsBurst, b.left+now.Sub(b.at).Hours()*WatchReadsPerHour)
		b.at = now
	}
	return b.left >= float64(n)
}

// WatchReadsSpent is the refusal of a watch's read once the budget is
// spent: the watch waits for a later pass.
const WatchReadsSpent = "pocketd spent the GitHub reads its watches may make for now; it reads this one again in a later round"

// WatchCall is a team_watch call's arguments.
type WatchCall struct {
	Action string `json:"action"`
	// Pull is the pull request, owner/name#number (add).
	Pull string `json:"pull,omitempty"`
	// Until is what the watch waits for (add): checks, merged or closed.
	Until string `json:"until,omitempty"`
	// Hours is how long at most (add); nil takes DefaultWatchHours.
	Hours *float64 `json:"hours,omitempty"`
	// ID is the watch to cancel, as W3.
	ID string `json:"id,omitempty"`
}

// WatchPlan is an add's checked arguments.
type WatchPlan struct {
	Repository string
	Number     int64
	Until      string
	Hours      float64
}

// Deadline is when a watch added at at gives up.
func (p WatchPlan) Deadline(at time.Time) time.Time {
	return at.Add(time.Duration(p.Hours * float64(time.Hour)))
}

// CheckWatchCall refuses a call whose action is unknown, or that names a
// field its action does not take.
func CheckWatchCall(c WatchCall) error {
	switch c.Action {
	case WatchAdd:
		if c.ID != "" {
			return errors.New("add takes pull, until and hours, not id")
		}
	case WatchList:
		if c.Pull != "" || c.Until != "" || c.Hours != nil || c.ID != "" {
			return errors.New("list takes nothing but its action")
		}
	case WatchCancel:
		if c.Pull != "" || c.Until != "" || c.Hours != nil {
			return errors.New("cancel takes id alone")
		}
	default:
		return fmt.Errorf("action %q is not add, list or cancel", c.Action)
	}
	return nil
}

// CheckWatch checks an add: a pull request as owner/name#number, a known
// condition, and hours above 0 and at most MaxWatchHours, or none for
// DefaultWatchHours. Hours given as 0 are refused: a limit given is a
// limit.
func CheckWatch(c WatchCall) (WatchPlan, error) {
	repository, number, err := ParseWatchPull(c.Pull)
	if err != nil {
		return WatchPlan{}, err
	}
	plan := WatchPlan{Repository: repository, Number: number, Until: c.Until, Hours: DefaultWatchHours}
	switch c.Until {
	case UntilChecks, UntilMerged, UntilClosed:
	case "":
		return WatchPlan{}, errors.New("until is required: checks, merged or closed")
	default:
		return WatchPlan{}, fmt.Errorf("until %q is not checks, merged or closed", c.Until)
	}
	if c.Hours != nil {
		if h := *c.Hours; math.IsNaN(h) || h <= 0 || h > MaxWatchHours {
			return WatchPlan{}, fmt.Errorf("hours are not above 0 and at most %d", MaxWatchHours)
		}
		plan.Hours = *c.Hours
	}
	return plan, nil
}

// ParseWatchPull reads a pull request as owner/name#number. GitHub's own
// rules for the names are checked where the pull request is read.
func ParseWatchPull(pull string) (string, int64, error) {
	repository, digits, ok := strings.Cut(strings.TrimSpace(pull), "#")
	owner, name, slash := strings.Cut(repository, "/")
	number, err := strconv.ParseInt(digits, 10, 64)
	if !ok || !slash || owner == "" || name == "" || strings.ContainsAny(repository, " \t#") || strings.Contains(name, "/") ||
		err != nil || number <= 0 || strconv.FormatInt(number, 10) != digits {
		return "", 0, fmt.Errorf("%q is no pull request: give it as owner/name#number", pull)
	}
	return repository, number, nil
}

// WatchID is a watch's id as the tool shows it: W3.
func WatchID(n int64) string { return "W" + strconv.FormatInt(n, 10) }

// ParseWatchID reads a watch's id, as W3 or 3.
func ParseWatchID(id string) (int64, error) {
	digits := strings.TrimSpace(id)
	if strings.HasPrefix(digits, "W") || strings.HasPrefix(digits, "w") {
		digits = digits[1:]
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != digits {
		return 0, fmt.Errorf("%q is no watch id: one is W3", id)
	}
	return n, nil
}

// MayWatch says whether a session of profile p (nil for none), working
// as role, gets team_watch: every session of a profile that gets
// team_memory (a supervisor's, a thread's specialist, a plan's lead, and
// a session the owner talks in), but a shift's specialist, whose rounds
// read CI in code already (ADR-0024) and whose runs a note would start
// outside them. A consult, a gather's agent, a review and a worker work
// at one fixed job, and a private agent has no team (ADR-0042).
func MayWatch(p *profiles.Profile, role SessionRole) bool {
	return p != nil && !p.Private && !role.Review && !role.Worker && !role.Consult && !role.Gather && !role.Shift
}

// CheckWatchCaller refuses a team_watch call from a session that does not
// get the tool (MayWatch), p as its profile is now: one that lists it
// from before is refused too.
func CheckWatchCaller(p *profiles.Profile, role SessionRole) error {
	switch {
	case MayWatch(p, role):
		return nil
	case role.Shift && !role.Review:
		return errors.New("team_watch is not a shift's: pocketd reads CI itself between the shift's rounds, so end your round with your STATUS line")
	case p == nil && !role.Worker:
		return errors.New("team_watch works only in a session started from a profile")
	}
	return errors.New("team_watch is not this session's: a review, a consult, a gather's agent and a worker do their one job and nothing else")
}

// WatchSeen is what pocketd read of a watched pull request.
type WatchSeen struct {
	// Open and Merged are its state; neither is closed without merging.
	Open, Merged bool
	Title        string
	// Head is the commit at its head. CIRead says CI there was read, CI
	// its state as github.CIChecks names it, CIDone that it is not
	// pending, and Failing the checks that failed; CIError is why CI could
	// not be read, "" when it was or was not tried.
	Head    string
	CIRead  bool
	CI      string
	CIDone  bool
	Failing []string
	CIError string
}

// WatchNeedsCI says whether a watch until until reads CI at a pull
// request that open says is open or not: only checks reads it, and only
// on an open pull request.
func WatchNeedsCI(until string, open bool) bool { return until == UntilChecks && open }

// WatchDecide is how a watch until until ends on what pocketd saw:
// WatchMet, WatchUnreachable, or "" while it waits.
func WatchDecide(until string, seen WatchSeen) string {
	switch until {
	case UntilChecks:
		switch {
		case !seen.Open:
			return WatchUnreachable
		case seen.CIRead && seen.CIDone:
			return WatchMet
		}
	case UntilMerged:
		switch {
		case seen.Merged:
			return WatchMet
		case !seen.Open:
			return WatchUnreachable
		}
	case UntilClosed:
		if !seen.Open {
			return WatchMet
		}
	}
	return ""
}

// untilText is what a watch waits for, in words.
func untilText(until string) string {
	switch until {
	case UntilChecks:
		return "its checks are done"
	case UntilMerged:
		return "it merges"
	}
	return "it merges or closes"
}

// hoursText is hours as a model reads them.
func hoursText(hours float64) string {
	unit := " hours"
	if hours == 1 {
		unit = " hour"
	}
	return strconv.FormatFloat(hours, 'f', -1, 64) + unit
}

// WatchSeenText is what pocketd saw, on one line: open, with CI at its
// head when read; merged; or closed without merging.
func WatchSeenText(seen WatchSeen) string {
	switch {
	case seen.Merged:
		return "merged"
	case !seen.Open:
		return "closed without merging"
	case !seen.CIRead && seen.CIError != "":
		return "open, CI unread: " + strings.TrimSuffix(flat(seen.CIError, 200), ".")
	case !seen.CIRead:
		return "open"
	}
	return "open, " + ciText(seen)
}

// ciText is CI at a pull request's head, with the failing checks' names.
func ciText(seen WatchSeen) string {
	head := seen.Head
	if len(head) > 12 {
		head = head[:12]
	}
	text := "CI at " + head + " " + seen.CI
	if seen.CI == "none" {
		text = "CI at " + head + " ran nothing"
	}
	if len(seen.Failing) > 0 {
		text += ": " + failingText(seen.Failing)
	}
	return text
}

// MaxWatchFailingBytes bounds the failing checks' names a watch's text
// lists, all together: a CI matrix may fail hundreds of checks, and the
// text keeps room for its outcome and the line that ends it, whatever
// carries it, a tool's answer or the note that starts a run (Codex review
// of #173, round 4).
const MaxWatchFailingBytes = 1500

// failingText is the failing checks' names, each cut to 100 characters,
// as many as fit in MaxWatchFailingBytes, and how many were left out.
func failingText(failing []string) string {
	var b strings.Builder
	for i, name := range failing {
		name = flat(name, 100)
		if i > 0 && b.Len()+len(", ")+len(name) > MaxWatchFailingBytes {
			fmt.Fprintf(&b, " and %d more", len(failing)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
	}
	return b.String()
}

// WatchAddedText is team_watch's answer to an add that keeps a watch.
// every is how often pocketd reads pull requests (-pr-every).
func WatchAddedText(id int64, ref, until string, hours float64, deadline time.Time, every time.Duration) string {
	rounds := "in each of its pull request rounds"
	if every > 0 {
		rounds = "every " + span(every)
	}
	return fmt.Sprintf("Watching %s: %s until %s, for at most %s, until %s. pocketd checks it %s and sends this session one message "+
		"when that holds, when it no longer can, or when the time is up. End your run now; that message starts your next one.",
		WatchID(id), ref, untilText(until), hoursText(hours), deadline.UTC().Format("2006-01-02 15:04 UTC"), rounds)
}

// WatchAtOnceText is team_watch's answer to an add whose pull request
// ended the watch as it was read: outcome WatchMet or WatchUnreachable.
// No note follows.
func WatchAtOnceText(ref, until, outcome string, seen WatchSeen) string {
	if outcome == WatchMet {
		return fmt.Sprintf("%s is there already: %s. No watch was kept, and no message follows.", ref, atOnceSeen(until, seen))
	}
	return fmt.Sprintf("%s %s. No watch was kept, and no message follows.", ref, unreachableText(until, seen))
}

// atOnceSeen is what met a condition, in words.
func atOnceSeen(until string, seen WatchSeen) string {
	if until == UntilChecks {
		return ciText(seen)
	}
	return WatchSeenText(seen)
}

// unreachableText says why a pull request can no longer reach until, as
// the rest of a sentence about it.
func unreachableText(until string, seen WatchSeen) string {
	switch {
	case until == UntilMerged:
		return "was closed without merging, so it will not merge unless someone reopens it"
	case seen.Merged:
		return "is merged, so there are no checks to wait for"
	}
	return "was closed without merging, so there are no checks to wait for"
}

// waitText is what a watch waited for, as "pocketd watched it …" ends.
func waitText(until string) string {
	switch until {
	case UntilChecks:
		return "for its checks to be done"
	case UntilMerged:
		return "for it to merge"
	}
	return "for it to merge or close"
}

// WatchNoteText is the one message a watch's end sends its session:
// outcome is how it ended, seen what pocketd read last (nil when it read
// nothing), readErr why its last read failed, and waited how long it
// watched.
func WatchNoteText(id int64, ref, until, outcome string, seen *WatchSeen, readErr string, waited time.Duration) string {
	var head, body string
	switch outcome {
	case WatchMet:
		switch until {
		case UntilChecks:
			head, body = "checks done", ciText(*seen)+"."
		case UntilMerged:
			head, body = "merged", "It merged."
		default:
			head = "closed"
			body = "It was closed without merging."
			if seen.Merged {
				body = "It merged."
			}
		}
	case WatchUnreachable:
		head, body = WatchSeenText(*seen), "It "+unreachableText(until, *seen)+"."
	case WatchExpired:
		head = "time is up"
		switch {
		case seen != nil:
			body = fmt.Sprintf("pocketd watched it for %s %s, and it is still %s.", span(waited.Round(time.Minute)), waitText(until), WatchSeenText(*seen))
		default:
			body = fmt.Sprintf("pocketd watched it for %s %s, and could not read it at the end: %s.", span(waited.Round(time.Minute)), waitText(until),
				strings.TrimSuffix(flat(readErr, 300), "."))
		}
	case WatchUnchecked:
		head = "not watched"
		body = "pocketd reads no pull requests in rounds now (-pr-every 0), so nothing would check this watch."
	}
	title := ""
	if seen != nil && seen.Title != "" {
		title = " (" + flat(seen.Title, 120) + ")"
	}
	return fmt.Sprintf("[%s · %s · %s] %s%s: %s The watch ended; add another with team_watch to go on waiting.", WatchID(id), ref, head, ref, title, body)
}

// WatchLine is a watch as list shows it.
type WatchLine struct {
	ID         int64
	Ref, Until string
	// Live says it still waits until Deadline; else Outcome says how it
	// ended, at Ended.
	Live     bool
	Deadline time.Time
	Outcome  string
	Ended    time.Time
}

// WatchListText is team_watch's list: the session's live watches, then
// the last ones that ended, the newest first.
func WatchListText(lines []WatchLine) string {
	if len(lines) == 0 {
		return "This session has no watches."
	}
	var b strings.Builder
	b.WriteString("This session's watches:")
	for _, l := range lines {
		if l.Live {
			fmt.Fprintf(&b, "\n- %s %s until %s: watching, until %s", WatchID(l.ID), l.Ref, untilText(l.Until), l.Deadline.UTC().Format("2006-01-02 15:04 UTC"))
			continue
		}
		fmt.Fprintf(&b, "\n- %s %s until %s: ended %s, %s", WatchID(l.ID), l.Ref, untilText(l.Until), l.Ended.UTC().Format("2006-01-02 15:04 UTC"), endedText(l.Outcome))
	}
	return b.String()
}

// endedText is how a watch ended, in words.
func endedText(outcome string) string {
	switch outcome {
	case WatchMet:
		return "the condition held"
	case WatchUnreachable:
		return "the condition could no longer hold"
	case WatchExpired:
		return "the time was up"
	case WatchCancelled:
		return "cancelled"
	case WatchThreadClosed:
		return "its thread closed"
	case WatchUnchecked:
		return "nothing checked it"
	}
	return outcome
}

// WatchCancelledText is team_watch's answer to a cancel of watch id;
// live says it still waited, else outcome is how it had ended.
func WatchCancelledText(id int64, live bool, outcome string) string {
	if live {
		return "Cancelled " + WatchID(id) + ": no message follows."
	}
	return WatchID(id) + " had ended already: " + endedText(outcome) + "."
}

// WatchesOff is team_watch's refusal of an add while pocketd reads no
// pull requests in rounds.
const WatchesOff = "pocketd reads no pull requests in rounds (-pr-every 0), so nothing would check a watch: none can be kept"

// WatchGuide tells a thread's specialist how to wait for CI, in its brief
// (ADR-0072).
const WatchGuide = "To wait for CI, open the pull request and call team_watch with action add, the pull request and until checks. " +
	WatchReceipt + " Never sleep or poll for it."

// WatchRoster tells a supervisor it may watch a merge, in its roster
// (ADR-0072).
const WatchRoster = "When work should follow a pull request's merge, watch it with team_watch until merged. Only an answer that starts with \"Watching W\" " +
	"promises a message: end your run then. pocketd tells you once it merged, that it closed without merging, or that the watch's time ran out; " +
	"delegate the follow-up only when pocketd says it merged, never on a close or an expiry. Any other answer comes with no message: act on it now. "

// WatchReceipt tells an agent which answer of team_watch's add promises a
// message: only a live watch's, which starts "Watching W". An answer that
// the pull request is there already, or a refusal, promises none.
const WatchReceipt = "Only an answer that starts with \"Watching W\" promises a message: end your run then, and pocketd's one message, when CI is done, " +
	"green or failing, with the failing checks' names, starts your next one. Any other answer (CI done already, or a refusal) comes with no message: act on it now."
