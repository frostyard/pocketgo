package team

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The supervisor's view of the owner's Inbox (ADR-0046): team_inbox lists,
// from pocketd's own records alone, what waits for the owner now, as the
// desktop's Inbox counts it, and what was settled in the last InboxWindow:
// the asks answered or withdrawn, and the pull requests merged or closed.
// The supervisor checks it before it reminds the owner of an ask or a pull
// request, so it does not remind them of work they did already.

// The bounds of team_inbox's answer (ADR-0046).
const (
	// InboxDays is how many days back the answer lists what was settled,
	// and InboxWindow that span.
	InboxDays   = 7
	InboxWindow = InboxDays * 24 * time.Hour
	// MaxInboxBytes and MaxInboxLines bound the whole answer, its notes
	// included: when it would be longer, every list is cut to the same
	// number of first items, the most that fit (InboxText). The tool's
	// output limits let an answer this size through whole.
	MaxInboxBytes = 24 << 10
	MaxInboxLines = 400
	// The most characters an item shows of a question, of an answer, of a
	// title, and of the name of the device that answered: an answer is
	// shown short, and every one of them on one line, so no item can push
	// the others out of the answer (Codex review of #91, round 1).
	inboxQuestion = 200
	inboxAnswer   = 160
	inboxTitle    = 100
	inboxDevice   = 40
)

// InboxGuide is the line of the supervisor's instructions that names
// team_inbox (ADR-0046).
var InboxGuide = fmt.Sprintf("Before you remind the owner of an ask or a pull request, check team_inbox: it lists what still waits for the owner and what was settled in the last %d days. ", InboxDays)

// CheckInboxCaller refuses team_inbox from a caller that is no
// supervisor: caller is the calling session's profile, nil for a session
// without one.
func CheckInboxCaller(caller *profiles.Profile) error {
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return errors.New("only a supervisor reads the owner's Inbox with team_inbox: this session's profile is not one")
	}
	return nil
}

// InboxSince is the start of the window team_inbox lists settled work in,
// for a read at now.
func InboxSince(now time.Time) time.Time { return now.Add(-InboxWindow) }

// PullWaits says p waits for the owner's review, as the desktop's Inbox
// counts it (state.ToReview): open, and no draft.
func PullWaits(p proto.PullRequest) bool { return p.State == proto.PullOpen && !p.Draft }

// PlanWaits says p waits for the owner's approval, as the desktop's Inbox
// counts it (state.ToApprove): in review, with nothing in its gate, and
// the graph its lead drafted for its revision, not approved yet.
func PlanWaits(p proto.Plan) bool {
	return p.State == proto.PlanReview && p.Rev != "" && p.Graph != nil && !p.Graph.Approved && p.Graph.Rev == p.Rev && len(p.Waiting) == 0
}

// InboxThread is what team_inbox shows of a thread an item names.
type InboxThread struct {
	Title string
	// Agent is its specialist's profile id: who asks in it.
	Agent string
}

// InboxAsk is an ask as team_inbox reads it: the protocol's, and when a
// supervisor's message withdrew it (ADR-0037), which the protocol does not
// carry.
type InboxAsk struct {
	proto.Ask
	WithdrawnAt *time.Time
}

// settledAt is when a was answered or withdrawn, and whether it was.
func (a InboxAsk) settledAt() (time.Time, bool) {
	switch {
	case a.State == proto.AskAnswered && a.AnsweredAt != nil:
		return *a.AnsweredAt, true
	case a.State == proto.AskWithdrawn && a.WithdrawnAt != nil:
		return *a.WithdrawnAt, true
	}
	return time.Time{}, false
}

// Inbox is what team_inbox reads, all of it from pocketd's records: no
// call to GitHub is made for it.
type Inbox struct {
	// Now is when it was read; Zone is the service's time zone, which its
	// times are shown in.
	Now  time.Time
	Zone *time.Location
	// PullsStarted says pocketd's rounds of pull request reads began
	// since it started (StartPulls); PullEvery is how often a round runs
	// (-pr-every), 0 for no rounds.
	PullsStarted bool
	PullEvery    time.Duration
	// Threads are the threads the items name, by id.
	Threads map[int64]InboxThread
	// What GET /v1/asks, /v1/pulls, /v1/plans, /v1/lessons and
	// /v1/approvals answer, in their order: the Inbox shows those that
	// wait for the owner.
	Asks      []proto.Ask
	Pulls     []proto.PullRequest
	Plans     []proto.Plan
	Lessons   []proto.Lesson
	Approvals []proto.Approval
	// Questions are the questions that wait for the owner in sessions,
	// oldest first, as GET /v1/sessions gives them and the desktop's Inbox
	// counts them (ADR-0048).
	Questions []InboxQuestion
	// Answered are asks answered or withdrawn, and Closed pull requests
	// merged or closed, at least those settled within the window.
	Answered []InboxAsk
	Closed   []proto.PullRequest
}

// inboxList is one list of the answer: its heading and its items, one
// line each, the most relevant first.
type inboxList struct {
	heading string
	items   []string
}

// InboxText is team_inbox's answer: a few lines on where its facts come
// from and how fresh they are, then what waits for the owner now, then
// what was settled in the window, newest first. An answer longer than
// MaxInboxBytes or MaxInboxLines keeps the same number of first items of
// every list, the most that fit, says so at its top, and each cut list's
// heading says how many items it has.
func InboxText(in Inbox) string { return in.text(MaxInboxBytes, MaxInboxLines) }

// inboxBounds are the most bytes and lines an answer has.
type inboxBounds struct{ bytes, lines int }

// fits says text is within b.
func (b inboxBounds) fits(text string) bool {
	return len(text) <= b.bytes && strings.Count(text, "\n")+1 <= b.lines
}

// text is InboxText within maxBytes and maxLines.
func (in Inbox) text(maxBytes, maxLines int) string {
	bounds := inboxBounds{maxBytes, maxLines}
	since := InboxSince(in.Now)
	waiting, settled := in.lists(since)
	most := 0
	for _, l := range slices.Concat(waiting, settled) {
		most = max(most, len(l.items))
	}
	if text := in.render(waiting, settled, since, most, nil); bounds.fits(text) {
		return text
	}
	// Every list keeps its first keep items: the largest keep that fits.
	// More items never make the answer shorter (an item's line is longer
	// than what its heading's count drops), so a search finds it.
	lo, hi := 0, most
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if bounds.fits(in.render(waiting, settled, since, mid, &bounds)) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	text := in.render(waiting, settled, since, lo, &bounds)
	if bounds.fits(text) {
		return text
	}
	// Even the headings do not fit, which team_inbox's own bounds never
	// let happen: the whole lines that fit, and a note.
	note := "[team_inbox: cut at a whole line to fit its bounds.]"
	var b strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if b.Len()+len(line)+1+len(note) > maxBytes || i+2 > maxLines {
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String() + note
}

// lists are the answer's lists: what waits for the owner, in the Inbox's
// order, and what was settled at since or later.
func (in Inbox) lists(since time.Time) (waiting, settled []inboxList) {
	waiting = []inboxList{
		{"Asks", in.askLines()},
		{"Pull requests to review", in.pullLines()},
		{"Plans to approve", in.planLines()},
		{"Lessons to decide", in.lessonLines()},
		{"Tool calls to approve", in.approvalLines()},
		{"Questions waiting in sessions", in.questionLines()},
	}
	settled = []inboxList{
		{"Asks answered or withdrawn", in.answeredLines(since)},
		{"Pull requests merged or closed whose record changed in the window", in.closedLines(since)},
	}
	return waiting, settled
}

// render is the answer with at most keep items of each list; cut is the
// bounds it was cut to fit, nil when it was not cut.
func (in Inbox) render(waiting, settled []inboxList, since time.Time, keep int, cut *inboxBounds) string {
	var b strings.Builder
	fmt.Fprintf(&b, "team_inbox at %s, from pocketd's records alone: nothing was read on GitHub for this answer. %s\n", in.when(in.Now), in.freshness())
	if cut != nil {
		fmt.Fprintf(&b, "[team_inbox: cut to fit %d bytes and %d lines: each list shows its first %d items, and a cut list's heading says how many it has.]\n", cut.bytes, cut.lines, keep)
	}
	b.WriteString("\nWaiting for the owner now:\n")
	for _, l := range waiting {
		writeList(&b, l, keep)
	}
	fmt.Fprintf(&b, "\nSettled in the last %d days, since %s:\n", InboxDays, in.when(since))
	for _, l := range settled {
		writeList(&b, l, keep)
	}
	b.WriteString("\nAn ask's times are pocketd's own. pocketd keeps no time of a pull request's merge or close: a merged or closed one's time is when pocketd last changed its record, which is the read that found it merged or closed or a later change of its title, label or other fields, so one closed long ago may be listed for a recent change.")
	return b.String()
}

// writeList writes l with at most keep of its items.
func writeList(b *strings.Builder, l inboxList, keep int) {
	switch n := len(l.items); {
	case n == 0:
		fmt.Fprintf(b, "%s: none.\n", l.heading)
	case n > keep:
		fmt.Fprintf(b, "%s (%d; the first %d shown):\n", l.heading, n, keep)
	default:
		fmt.Fprintf(b, "%s (%d):\n", l.heading, n)
	}
	for _, item := range l.items[:min(keep, len(l.items))] {
		b.WriteString("- " + item + "\n")
	}
}

// freshness says how fresh pocketd's pull requests are.
func (in Inbox) freshness() string {
	switch {
	case !in.PullsStarted:
		return "pocketd's rounds of pull request reads have not begun since it started; each pull request says when pocketd last read it on GitHub."
	case in.PullEvery <= 0:
		return "pocketd reads pull requests in no rounds (-pr-every 0), only when a report links one, a cross-review pass reads one, or the owner's desktop opens one; each says when pocketd last read it on GitHub."
	}
	return fmt.Sprintf("pocketd reads each open pull request on GitHub again every %s; each says when pocketd last read it, and a merge or close since then is not here yet.", span(in.PullEvery))
}

func (in Inbox) askLines() []string {
	var out []string
	for _, a := range in.Asks {
		if a.State != proto.AskOpen {
			continue
		}
		line := fmt.Sprintf("A%d · %s · %s asked %s: %q", a.ID, in.thread(a.Thread), in.agent(a.Thread), in.ago(a.CreatedAt), flat(a.Question, inboxQuestion))
		if len(a.Options) > 0 {
			options := make([]string, len(a.Options))
			for i, o := range a.Options {
				options[i] = flat(o, inboxTitle)
			}
			line += " Options: " + strings.Join(options, " | ") + "."
		}
		out = append(out, line)
	}
	return out
}

func (in Inbox) pullLines() []string {
	var out []string
	for _, p := range in.Pulls {
		if !PullWaits(p) {
			continue
		}
		ready := "no"
		if p.Ready {
			ready = "yes"
		}
		verdict := "none"
		if r := p.Review; r != nil {
			verdict = fmt.Sprintf("%s, by %s", r.Verdict, r.Agent)
			if r.Family != "" {
				verdict += " (" + r.Family + ")"
			}
		}
		ci := string(p.Checks)
		if ci == "" {
			ci = "not read yet"
		}
		out = append(out, fmt.Sprintf("%s#%d · %s · %q · open · ready-for-human: %s · verdict at its head: %s · CI: %s · read %s",
			p.Repository, p.Number, in.thread(p.Thread), flat(p.Title, inboxTitle), ready, verdict, ci, in.ago(p.CheckedAt)))
	}
	return out
}

func (in Inbox) planLines() []string {
	var out []string
	for _, p := range in.Plans {
		if !PlanWaits(p) {
			continue
		}
		out = append(out, fmt.Sprintf("P%d · %s · %q · lead %s · rev %s, tasks version %d · in review, changed %s",
			p.ID, in.thread(p.Thread), flat(p.Title, inboxTitle), p.Lead, short(p.Rev), p.Graph.Version, in.ago(p.UpdatedAt)))
	}
	return out
}

func (in Inbox) lessonLines() []string {
	var out []string
	for _, l := range in.Lessons {
		switch {
		case !l.Waits():
		case l.State == proto.LessonPending:
			out = append(out, fmt.Sprintf("L%d · %s · new, by %s: %q", l.ID, l.Agent, l.Writer, flat(l.Text, inboxQuestion)))
		case l.Change.Forget:
			out = append(out, fmt.Sprintf("L%d · %s · forget, asked by %s: %q", l.ID, l.Agent, l.Change.Writer, flat(l.Text, inboxQuestion)))
		default:
			out = append(out, fmt.Sprintf("L%d · %s · change, by %s: %q to %q", l.ID, l.Agent, l.Change.Writer, flat(l.Text, inboxQuestion), flat(l.Change.Text, inboxQuestion)))
		}
	}
	return out
}

func (in Inbox) approvalLines() []string {
	var out []string
	for _, a := range in.Approvals {
		if a.State != proto.ApprovalPending {
			continue
		}
		out = append(out, fmt.Sprintf("%s · session %d · %s · asked %s", a.ID, a.SessionID, flat(a.Tool, inboxTitle), in.ago(a.RequestedAt)))
	}
	return out
}

// InboxQuestion is a question that waits for the owner, with the profile
// of its session, "" for none.
type InboxQuestion struct {
	proto.Question
	Profile string
}

// questionLines name each session whose question waits for the owner, as
// the desktop's Inbox lists it (state.Data.QuestionsWaiting), and never
// the question's words: a private agent's session is the owner's alone
// (ADR-0042, ADR-0048).
func (in Inbox) questionLines() []string {
	var out []string
	for _, q := range in.Questions {
		if q.State != proto.QuestionWaiting {
			continue
		}
		agent := q.Profile
		if agent == "" {
			agent = "no profile"
		}
		out = append(out, fmt.Sprintf("session %d · %s · waits for the owner's answer since %s; skipped at %s if none comes",
			q.SessionID, agent, in.ago(q.CreatedAt), in.when(q.Deadline)))
	}
	return out
}

// answeredLines are the asks answered or withdrawn at since or later, the
// most recently settled first.
func (in Inbox) answeredLines(since time.Time) []string {
	var asks []InboxAsk
	for _, a := range in.Answered {
		if at, ok := a.settledAt(); ok && !at.Before(since) {
			asks = append(asks, a)
		}
	}
	slices.SortStableFunc(asks, func(a, b InboxAsk) int {
		at, _ := a.settledAt()
		bt, _ := b.settledAt()
		return cmp.Or(bt.Compare(at), cmp.Compare(b.ID, a.ID))
	})
	out := make([]string, len(asks))
	for i, a := range asks {
		at, _ := a.settledAt()
		head := fmt.Sprintf("A%d · %s · %s: %q", a.ID, in.thread(a.Thread), in.agent(a.Thread), flat(a.Question, inboxQuestion))
		if a.State == proto.AskAnswered {
			by := ""
			if device := flat(a.AnsweredBy, inboxDevice); device != "" {
				by = " on " + device
			}
			out[i] = fmt.Sprintf("%s · answered %s%s: %q", head, in.ago(at), by, flat(a.Answer, inboxAnswer))
		} else {
			out[i] = fmt.Sprintf("%s · withdrawn %s by a supervisor's message to the thread, unanswered", head, in.ago(at))
		}
	}
	return out
}

// closedLines are the pull requests merged or closed whose record last
// changed at since or later, the most recent first: pocketd keeps no time
// of a merge or a close, so the time shown is said to be the record's last
// change, never the close (Codex review of #91, round 1).
func (in Inbox) closedLines(since time.Time) []string {
	var pulls []proto.PullRequest
	for _, p := range in.Closed {
		if (p.State == proto.PullMerged || p.State == proto.PullClosed) && !p.UpdatedAt.Before(since) {
			pulls = append(pulls, p)
		}
	}
	slices.SortStableFunc(pulls, func(a, b proto.PullRequest) int {
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), cmp.Compare(b.ID, a.ID))
	})
	out := make([]string, len(pulls))
	for i, p := range pulls {
		out[i] = fmt.Sprintf("%s#%d · %s · %q · %s · record last changed %s", p.Repository, p.Number, in.thread(p.Thread), flat(p.Title, inboxTitle), p.State, in.ago(p.UpdatedAt))
	}
	return out
}

// thread names thread id with its title, as T5 "Fix CI".
func (in Inbox) thread(id int64) string {
	name := "T" + strconv.FormatInt(id, 10)
	if t, ok := in.Threads[id]; ok && t.Title != "" {
		name += " " + strconv.Quote(flat(t.Title, inboxTitle))
	}
	return name
}

// agent is the specialist of thread id, "its specialist" when unknown.
func (in Inbox) agent(id int64) string {
	if t, ok := in.Threads[id]; ok && t.Agent != "" {
		return t.Agent
	}
	return "its specialist"
}

// ago is t as people read it from now: "3h ago (Sat 21 Nov 12:00 UTC)".
func (in Inbox) ago(t time.Time) string {
	if t.IsZero() {
		return "at no time pocketd recorded"
	}
	d := in.Now.Sub(t)
	var rel string
	switch {
	case d < 0:
		rel = "later than now"
	case d < time.Minute:
		rel = "just now"
	case d < time.Hour:
		rel = fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		rel = fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		rel = fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
	return rel + " (" + in.when(t) + ")"
}

// when is t in the service's zone, as schedule.When writes it.
func (in Inbox) when(t time.Time) string {
	zone := in.Zone
	if zone == nil {
		zone = time.UTC
	}
	return t.In(zone).Format("Mon 2 Jan 15:04 MST")
}

// span is d as a model reads it: 5m, 2h, 90s.
func span(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// flat is text on one line, its runs of white space one space, cut to at
// most n characters.
func flat(text string, n int) string {
	return cut(strings.Join(strings.Fields(text), " "), n)
}
