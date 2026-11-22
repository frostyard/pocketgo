package state

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// Notifications (ADR-0049): the desktop tells the owner with a system
// notification when something new waits for them, and shows the Inbox's
// count on the app's icon (InboxCount). Which change notifies, its key,
// its words and what a click on it opens are decided here, by the Store as
// it applies each event and each answer (Options.Notify); whether a notice
// shows, given the window's focus and the owner's switches, is ShowNotice.
// desktop/internal/app only sends what these decide.
//
// Each thing notifies once: its key is the thing itself (the ask's, the
// question's or the approval's id, the pull request at its head, the plan
// at its revision, the lesson at the proposal it shows, a run by the
// owner's message it answered). The keys seen are kept per service URL
// between runs (Options.KeptNotices, Options.KeepNotices), and what waits
// at a list's first load after a sign-in begins notifies nothing: the
// badge counts it.

// NoticeKind is a kind of notification, as Settings turns each on or off
// (NotifyPrefs).
type NoticeKind int

const (
	// NoticeAsks: an ask from a thread, or a session's question
	// (ADR-0048).
	NoticeAsks NoticeKind = iota + 1
	// NoticeApprovals: a tool call that waits for the owner's approval.
	NoticeApprovals
	// NoticeReviews: a pull request to review (ToReview), a plan to
	// approve (ToApprove) or a lesson to decide.
	NoticeReviews
	// NoticeRuns: a run that answered the owner's message ended, in a
	// session where that message, sent from this desktop, is still the
	// last one.
	NoticeRuns
)

// NoticeKinds are the kinds, in the order Settings lists their switches.
var NoticeKinds = [...]NoticeKind{NoticeAsks, NoticeApprovals, NoticeReviews, NoticeRuns}

// Title names kind k's switch in Settings.
func (k NoticeKind) Title() string {
	switch k {
	case NoticeAsks:
		return "Asks and questions"
	case NoticeApprovals:
		return "Tool approvals"
	case NoticeReviews:
		return "Pull requests, plans and lessons"
	case NoticeRuns:
		return "Finished runs"
	}
	return ""
}

// Subtitle says what kind k's switch notifies of.
func (k NoticeKind) Subtitle() string {
	switch k {
	case NoticeAsks:
		return "A thread's ask, or a session's question, waits for your answer"
	case NoticeApprovals:
		return "A tool call waits for your approval"
	case NoticeReviews:
		return "A pull request waits for your review, a plan for your approval, or a lesson for your decision"
	case NoticeRuns:
		return "A run that answers your message from this computer ended"
	}
	return ""
}

// Notice is one system notification, as the Store decides it.
type Notice struct {
	// Key is the thing it is about, which notifies once.
	Key  string
	Kind NoticeKind
	// Title, Subtitle and Body are its words. Subtitle names where the
	// thing is (a thread, a session), "" for nothing.
	Title, Subtitle, Body string
	// Target is what a click on it opens.
	Target NoticeTarget
}

// NoticeTarget is what a click on a notification opens: a session, or the
// Inbox at a card.
type NoticeTarget struct {
	// Session is the session to open; 0 opens the Inbox at Card.
	Session int64
	// Card names the Inbox's card (AskCard and the rest).
	Card string
}

// The Inbox's cards a notification opens at, by what they show.
func AskCard(id int64) string       { return "ask:" + strconv.FormatInt(id, 10) }
func ApprovalCard(id string) string { return "approval:" + id }
func PullCard(id int64) string      { return "pull:" + strconv.FormatInt(id, 10) }
func PlanCard(id int64) string      { return "plan:" + strconv.FormatInt(id, 10) }
func LessonCard(id int64) string    { return "lesson:" + strconv.FormatInt(id, 10) }

// NotifyPrefs are the owner's notification switches on this desktop, for
// one service. Every notification is on unless switched off, so the zero
// value has them all on: Off turns them all off, and each kind's field
// turns off its kind alone.
type NotifyPrefs struct {
	Off          bool
	AsksOff      bool
	ApprovalsOff bool
	ReviewsOff   bool
	RunsOff      bool
}

// On reports whether notices of kind k show: notifications are on, and so
// is k's switch.
func (p NotifyPrefs) On(k NoticeKind) bool { return !p.Off && p.KindOn(k) }

// KindOn reports whether kind k's own switch is on, whatever Off says.
func (p NotifyPrefs) KindOn(k NoticeKind) bool {
	switch k {
	case NoticeAsks:
		return !p.AsksOff
	case NoticeApprovals:
		return !p.ApprovalsOff
	case NoticeReviews:
		return !p.ReviewsOff
	case NoticeRuns:
		return !p.RunsOff
	}
	return false
}

// WithKind returns p with kind k's own switch set to on.
func (p NotifyPrefs) WithKind(k NoticeKind, on bool) NotifyPrefs {
	switch k {
	case NoticeAsks:
		p.AsksOff = !on
	case NoticeApprovals:
		p.ApprovalsOff = !on
	case NoticeReviews:
		p.ReviewsOff = !on
	case NoticeRuns:
		p.RunsOff = !on
	}
	return p
}

// ShowNotice reports whether notice n shows as a system notification: only
// while the window is not focused, and only with notifications and n's
// kind on. A notice that does not show is spent all the same: it never
// shows later.
func ShowNotice(n Notice, p NotifyPrefs, focused bool) bool {
	return !focused && p.On(n.Kind)
}

// Notifies reports whether the app shows system notifications on goos.
// iOS shows none: a suspended app needs push to notify, which pocketd does
// not send, and a running one is in front. It keeps the badge.
func Notifies(goos string) bool { return goos != platformIOS }

// InboxCount is how many things wait for the owner in kinds (Data.Inbox):
// the app's icon shows it, and the Inbox tab's badge up to InboxBadgeMost.
func InboxCount(kinds []InboxKind) int {
	n := 0
	for _, k := range kinds {
		n += k.Count
	}
	return n
}

// noticeIDPrefix starts the id of every notification of PocketGo's.
const noticeIDPrefix = "pocketgo-notice:"

// NoticeID is the system's id of a notification about target, shown for
// the service at server: a click hands it back (ParseNoticeID), also after
// the app started again.
func NoticeID(server string, t NoticeTarget) string {
	v := url.Values{"server": {server}}
	if t.Session != 0 {
		v.Set("session", strconv.FormatInt(t.Session, 10))
	} else {
		v.Set("card", t.Card)
	}
	return noticeIDPrefix + v.Encode()
}

// ParseNoticeID is the service and the target of the notification id
// names, and whether id names one of PocketGo's.
func ParseNoticeID(id string) (server string, t NoticeTarget, ok bool) {
	rest, found := strings.CutPrefix(id, noticeIDPrefix)
	if !found {
		return "", NoticeTarget{}, false
	}
	v, err := url.ParseQuery(rest)
	if err != nil || v.Get("server") == "" {
		return "", NoticeTarget{}, false
	}
	switch session, card := v.Get("session"), v.Get("card"); {
	case session != "" && card == "":
		n, err := strconv.ParseInt(session, 10, 64)
		if err != nil || n <= 0 {
			return "", NoticeTarget{}, false
		}
		t.Session = n
	case card != "" && session == "":
		t.Card = card
	default:
		return "", NoticeTarget{}, false
	}
	return v.Get("server"), t, true
}

// noticeText is the first line of s that holds text, trimmed and cut to n
// characters.
func noticeText(s string, n int) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return Truncate(line, n, false)
		}
	}
	return ""
}

// noticeBodyMost is the most characters of a notification's body.
const noticeBodyMost = 200

// threadCaption names thread id as the Inbox's cards do: "T3 · its
// title", "Thread T3" while the thread is not known.
func (d *Data) threadCaption(id int64) string {
	if th, ok := d.Thread(id); ok {
		return "T" + strconv.FormatInt(th.ID, 10) + " · " + th.Title
	}
	return "Thread T" + strconv.FormatInt(id, 10)
}

// SessionAgent names who speaks in session id: its profile's agent, else
// its title, else "The agent".
func (d *Data) SessionAgent(id int64) string {
	if s, ok := d.Session(id); ok {
		if s.Profile != "" {
			return d.AgentName(s.Profile)
		}
		if s.Title != "" {
			return s.Title
		}
	}
	return "The agent"
}

// sessionCaption is session id's title, "" while it is not known.
func (d *Data) sessionCaption(id int64) string {
	if s, ok := d.Session(id); ok {
		return SessionTitle(s)
	}
	return ""
}

// AskNotice is the notice of open ask a, which opens its card.
func (d *Data) AskNotice(a proto.Ask) Notice {
	caption := d.threadCaption(a.Thread)
	if _, ok := d.Thread(a.Thread); !ok {
		if s, ok := d.Session(a.SessionID); ok {
			caption = SessionTitle(s)
		}
	}
	return Notice{Key: "ask:" + strconv.FormatInt(a.ID, 10), Kind: NoticeAsks,
		Title: "A" + strconv.FormatInt(a.ID, 10) + " asks you", Subtitle: caption, Body: noticeText(a.Question, noticeBodyMost),
		Target: NoticeTarget{Card: AskCard(a.ID)}}
}

// QuestionNotice is the notice of waiting question q (ADR-0048), which
// opens its session, where it is answered.
func (d *Data) QuestionNotice(q proto.Question) Notice {
	return Notice{Key: "question:" + q.ID, Kind: NoticeAsks,
		Title: d.SessionAgent(q.SessionID) + " waits for your answer", Subtitle: d.sessionCaption(q.SessionID),
		Body: noticeText(q.Question, noticeBodyMost), Target: NoticeTarget{Session: q.SessionID}}
}

// ApprovalNotice is the notice of pending approval a, which opens its
// card.
func (d *Data) ApprovalNotice(a proto.Approval) Notice {
	return Notice{Key: "approval:" + a.ID, Kind: NoticeApprovals,
		Title: a.Tool + " waits for your approval", Subtitle: d.sessionCaption(a.SessionID),
		Body: noticeText(ToolSummary(a.Tool, a.Arguments), noticeBodyMost), Target: NoticeTarget{Card: ApprovalCard(a.ID)}}
}

// PullNotice is the notice of pull request p waiting for review, at its
// head: a push to it notifies again. It opens its card.
func (d *Data) PullNotice(p proto.PullRequest) Notice {
	return Notice{Key: "pull:" + strconv.FormatInt(p.ID, 10) + ":" + p.HeadSHA, Kind: NoticeReviews,
		Title: PullRef(p) + " waits for your review", Subtitle: d.threadCaption(p.Thread),
		Body: noticeText(p.Title, noticeBodyMost), Target: NoticeTarget{Card: PullCard(p.ID)}}
}

// PlanNotice is the notice of plan p waiting for approval, at its
// revision: a new revision notifies again. It opens its card.
func (d *Data) PlanNotice(p proto.Plan) Notice {
	return Notice{Key: "plan:" + strconv.FormatInt(p.ID, 10) + ":" + p.Rev, Kind: NoticeReviews,
		Title:    "P" + strconv.FormatInt(p.ID, 10) + " waits for your approval",
		Subtitle: "T" + strconv.FormatInt(p.Thread, 10) + " · led by " + d.AgentName(p.Lead),
		Body:     noticeText(p.Title, noticeBodyMost), Target: NoticeTarget{Card: PlanCard(p.ID)}}
}

// LessonNotice is the notice of lesson l waiting for the owner, at what
// waits on it (LessonSeen): another proposal notifies again. It opens its
// card.
func (d *Data) LessonNotice(l proto.Lesson) Notice {
	name := "L" + strconv.FormatInt(l.ID, 10)
	agent := d.AgentName(l.Agent)
	text, forget := SeenText(l)
	title := "New text for " + name + " of " + agent + " waits for your approval"
	switch {
	case l.State == proto.LessonPending:
		title = "A lesson for " + agent + " waits for your approval"
	case forget:
		// What would be forgotten: the text in force.
		title, text = "Forgetting "+name+" of "+agent+" waits for your approval", l.Text
	}
	sum := sha256.Sum256([]byte(LessonSeen(l)))
	return Notice{Key: "lesson:" + strconv.FormatInt(l.ID, 10) + ":" + hex.EncodeToString(sum[:8]), Kind: NoticeReviews,
		Title: title, Subtitle: name, Body: noticeText(text, noticeBodyMost), Target: NoticeTarget{Card: LessonCard(l.ID)}}
}

// RunNotice is the notice that session id's run answered the owner's
// message submission with final, its final answer: the answer's first
// line, else why it stopped. It opens the session.
func (d *Data) RunNotice(id, submission int64, final proto.Entry) Notice {
	title := "Session " + strconv.FormatInt(id, 10)
	if s, ok := d.Session(id); ok {
		title = SessionTitle(s)
	}
	body := noticeText(final.Text, noticeBodyMost)
	switch {
	case final.Error != "":
		body = noticeText("It stopped with an error: "+final.Error, noticeBodyMost)
	case final.StopReason == "error":
		body = "It stopped with an error."
	case final.StopReason == "aborted":
		body = "It was stopped."
	case body == "":
		body = "Its answer is ready."
	}
	return Notice{Key: "run:" + strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(submission, 10), Kind: NoticeRuns,
		Title: title + " finished", Body: body, Target: NoticeTarget{Session: id}}
}

// waitingList is one list of what waits for the owner, as notices: its
// name, where its fetch stands, and the notice of each thing in it.
type waitingList struct {
	name   string
	status ListStatus
	items  []Notice
}

// waitingLists are the lists of what waits for the owner, in the Inbox's
// order (Data.Inbox), as notices.
func (d *Data) waitingLists() []waitingList {
	sessions, _ := d.SessionsStatus()
	plans, _ := d.PlansStatus()
	asks, _ := d.AsksStatus()
	lessons, _ := d.LessonsStatus()
	pulls, _ := d.PullsStatus()
	approvals, _ := d.ApprovalsStatus()
	out := []waitingList{
		{name: "plans", status: plans}, {name: "asks", status: asks}, {name: "lessons", status: lessons},
		{name: "pulls", status: pulls}, {name: "approvals", status: approvals}, {name: "questions", status: sessions},
	}
	for _, p := range d.PlansToApprove() {
		out[0].items = append(out[0].items, d.PlanNotice(p))
	}
	for _, a := range d.asks {
		out[1].items = append(out[1].items, d.AskNotice(a))
	}
	for _, l := range d.Lessons() {
		out[2].items = append(out[2].items, d.LessonNotice(l))
	}
	for _, p := range d.PullsToReview() {
		out[3].items = append(out[3].items, d.PullNotice(p))
	}
	for _, a := range d.approvals {
		out[4].items = append(out[4].items, d.ApprovalNotice(a))
	}
	for _, q := range d.QuestionsWaiting() {
		out[5].items = append(out[5].items, d.QuestionNotice(q))
	}
	return out
}

// NoticeKeysKept is the most keys a service keeps between runs. Past it,
// the oldest keys of what no longer waits go first; a key of what still
// waits is never dropped, so it never notifies twice.
const NoticeKeysKept = 500

// noticeBook is the Store's record of what it notified, for one sign-in:
// the keys it has seen, kept between runs for the service; the lists whose
// first load it took as seen; and the owner's messages from this desktop
// whose answers it waits for. It belongs to the UI thread, as the Store.
type noticeBook struct {
	known map[string]bool
	order []string // known's keys, oldest first
	// baseline holds the lists whose first load was taken as seen.
	baseline map[string]bool
	// owner holds, by session, the owner's newest message sent from this
	// desktop, and turns the newest user entry seen and what came after
	// it, by session.
	owner map[int64]*ownerMessage
	turns map[int64]*turn
}

// ownerMessage is the owner's message to a session, by its submission,
// and whether its entry came.
type ownerMessage struct {
	submission int64
	placed     bool
}

// turn is a session's newest user entry, by its submission, and the final
// answer written after it (a final assistant entry), nil before one.
type turn struct {
	submission int64
	final      *proto.Entry
}

// reset starts the record of a new sign-in, with the keys kept for its
// service.
func (b *noticeBook) reset(keys []string) {
	*b = noticeBook{known: map[string]bool{}, baseline: map[string]bool{}, owner: map[int64]*ownerMessage{}, turns: map[int64]*turn{}}
	for _, k := range keys {
		b.know(k)
	}
}

// know records key as seen, and reports whether it was new.
func (b *noticeBook) know(key string) bool {
	if key == "" || b.known[key] {
		return false
	}
	b.known[key] = true
	b.order = append(b.order, key)
	return true
}

// keys are the keys to keep, oldest first; a copy.
func (b *noticeBook) keys() []string { return slices.Clone(b.order) }

// entry follows an entry of the stream, of any session. A user entry
// starts its session's turn: the owner's message placed, or another's
// after it, which is the last message now. A final answer, an assistant
// entry that calls no tool for the next round (as pocketd's own rule for a
// run's end, "a final answer ends its run"), ends the turn.
func (b *noticeBook) entry(e proto.Entry) {
	switch e.Kind {
	case proto.EntryUser:
		b.turns[e.SessionID] = &turn{submission: e.SubmissionID}
		if m := b.owner[e.SessionID]; m != nil {
			switch {
			case e.SubmissionID == m.submission:
				m.placed = true
			case m.placed:
				// Another message came after the owner's: the answer
				// that ends its run is not to the owner's.
				delete(b.owner, e.SessionID)
			}
		}
	case proto.EntryAssistant:
		if e.StopReason == "toolUse" {
			return
		}
		if t := b.turns[e.SessionID]; t != nil {
			final := proto.Entry{Text: e.Text, Error: e.Error, StopReason: e.StopReason}
			t.final = &final
		}
	}
}

// sent records the owner's message to session id that pocketd took as
// submission. Its entry may have come before pocketd's answer did.
func (b *noticeBook) sent(id, submission int64) {
	if submission <= 0 {
		return
	}
	m := &ownerMessage{submission: submission}
	if t := b.turns[id]; t != nil && t.submission == submission {
		m.placed = true
	}
	b.owner[id] = m
}

// scan decides the notices of what is new in d since the last scan, each
// key once, and reports whether the keys to keep changed. A list notifies
// nothing until its first load, which it takes as seen; then each thing
// that waits with a key not seen notifies. A run notifies once the turn
// of the owner's message has its final answer.
func (b *noticeBook) scan(d *Data) (out []Notice, changed bool) {
	current := map[string]bool{}
	for _, l := range d.waitingLists() {
		for _, n := range l.items {
			current[n.Key] = true
		}
		if !b.baseline[l.name] {
			if l.status != ListLoaded {
				continue
			}
			b.baseline[l.name] = true
			for _, n := range l.items {
				changed = b.know(n.Key) || changed
			}
			continue
		}
		for _, n := range l.items {
			if b.know(n.Key) {
				out = append(out, n)
				changed = true
			}
		}
	}
	ids := make([]int64, 0, len(b.owner))
	for id := range b.owner {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		m, t := b.owner[id], b.turns[id]
		if !m.placed || t == nil || t.submission != m.submission || t.final == nil {
			continue
		}
		delete(b.owner, id)
		if n := d.RunNotice(id, m.submission, *t.final); b.know(n.Key) {
			out = append(out, n)
			changed = true
		}
	}
	return out, b.prune(current) || changed
}

// prune drops the oldest keys past NoticeKeysKept of what no longer waits
// (current holds the keys of what waits), and reports whether it dropped
// any.
func (b *noticeBook) prune(current map[string]bool) bool {
	drop := len(b.order) - NoticeKeysKept
	if drop <= 0 {
		return false
	}
	kept := make([]string, 0, NoticeKeysKept)
	for _, k := range b.order {
		if drop > 0 && !current[k] {
			delete(b.known, k)
			drop--
			continue
		}
		kept = append(kept, k)
	}
	pruned := len(kept) < len(b.order)
	b.order = kept
	return pruned
}
