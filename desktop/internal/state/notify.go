package state

import (
	"cmp"
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
// The client (desktop/internal/nsgoclient) only sends what these decide.
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

// BadgesIcon reports whether the app's icon shows the badge on goos: the
// Dock's on macOS, the launcher entry's count on Linux. MyGo v0.10.0
// keeps it in memory alone on Windows, and drops it on iOS, so Settings
// promises no count there (Codex review of #104, round 1).
func BadgesIcon(goos string) bool { return goos == "darwin" || goos == "linux" }

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
	return Notice{Key: askNoticeKey(a), Kind: NoticeAsks,
		Title: "A" + strconv.FormatInt(a.ID, 10) + " asks you", Subtitle: caption, Body: noticeText(a.Question, noticeBodyMost),
		Target: NoticeTarget{Card: AskCard(a.ID)}}
}

// QuestionNotice is the notice of waiting question q (ADR-0048), which
// opens its session, where it is answered.
func (d *Data) QuestionNotice(q proto.Question) Notice {
	return Notice{Key: questionNoticeKey(q), Kind: NoticeAsks,
		Title: d.SessionAgent(q.SessionID) + " waits for your answer", Subtitle: d.sessionCaption(q.SessionID),
		Body: noticeText(q.Question, noticeBodyMost), Target: NoticeTarget{Session: q.SessionID}}
}

// ApprovalNotice is the notice of pending approval a, which opens its
// card.
func (d *Data) ApprovalNotice(a proto.Approval) Notice {
	return Notice{Key: approvalNoticeKey(a), Kind: NoticeApprovals,
		Title: a.Tool + " waits for your approval", Subtitle: d.sessionCaption(a.SessionID),
		Body: noticeText(ToolSummary(a.Tool, a.Arguments), noticeBodyMost), Target: NoticeTarget{Card: ApprovalCard(a.ID)}}
}

// PullNotice is the notice of pull request p waiting for review, at its
// head: a push to it notifies again. It opens its card.
func (d *Data) PullNotice(p proto.PullRequest) Notice {
	return Notice{Key: pullNoticeKey(p), Kind: NoticeReviews,
		Title: PullRef(p) + " waits for your review", Subtitle: d.threadCaption(p.Thread),
		Body: noticeText(p.Title, noticeBodyMost), Target: NoticeTarget{Card: PullCard(p.ID)}}
}

// PlanNotice is the notice of plan p waiting for approval, at its
// revision: a new revision notifies again. It opens its card.
func (d *Data) PlanNotice(p proto.Plan) Notice {
	return Notice{Key: planNoticeKey(p), Kind: NoticeReviews,
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
	return Notice{Key: lessonNoticeKey(l), Kind: NoticeReviews,
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
	return Notice{Key: runNoticeKey(id, submission), Kind: NoticeRuns,
		Title: title + " finished", Body: body, Target: NoticeTarget{Session: id}}
}

// The keys of the notices, which a scan computes for everything that
// waits: cheap, apart from the words, which only a new key's notice needs.
func askNoticeKey(a proto.Ask) string           { return "ask:" + strconv.FormatInt(a.ID, 10) }
func questionNoticeKey(q proto.Question) string { return "question:" + q.ID }
func approvalNoticeKey(a proto.Approval) string { return "approval:" + a.ID }
func pullNoticeKey(p proto.PullRequest) string {
	return "pull:" + strconv.FormatInt(p.ID, 10) + ":" + p.HeadSHA
}
func planNoticeKey(p proto.Plan) string { return "plan:" + strconv.FormatInt(p.ID, 10) + ":" + p.Rev }
func lessonNoticeKey(l proto.Lesson) string {
	sum := sha256.Sum256([]byte(LessonSeen(l)))
	return "lesson:" + strconv.FormatInt(l.ID, 10) + ":" + hex.EncodeToString(sum[:8])
}
func runNoticeKey(id, submission int64) string {
	return "run:" + strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(submission, 10)
}

// waitingList is one list of what waits for the owner: its name, where its
// fetch stands, the key of each thing in it, and the notice of its i-th
// thing, made only for a key not seen.
type waitingList struct {
	name   string
	status ListStatus
	keys   []string
	notice func(i int) Notice
}

// waitingLists are the lists of what waits for the owner, in the Inbox's
// order (Data.Inbox).
func (d *Data) waitingLists() []waitingList {
	sessions, _ := d.SessionsStatus()
	plans, _ := d.PlansStatus()
	asks, _ := d.AsksStatus()
	lessons, _ := d.LessonsStatus()
	pulls, _ := d.PullsStatus()
	approvals, _ := d.ApprovalsStatus()
	toApprove, waitingLessons, toReview, questions := d.PlansToApprove(), d.Lessons(), d.PullsToReview(), d.QuestionsWaiting()
	askList, approvalList := d.asks, d.approvals
	return []waitingList{
		{name: "plans", status: plans, keys: noticeKeysOf(toApprove, planNoticeKey), notice: func(i int) Notice { return d.PlanNotice(toApprove[i]) }},
		{name: "asks", status: asks, keys: noticeKeysOf(askList, askNoticeKey), notice: func(i int) Notice { return d.AskNotice(askList[i]) }},
		{name: "lessons", status: lessons, keys: noticeKeysOf(waitingLessons, lessonNoticeKey),
			notice: func(i int) Notice { return d.LessonNotice(waitingLessons[i]) }},
		{name: "pulls", status: pulls, keys: noticeKeysOf(toReview, pullNoticeKey), notice: func(i int) Notice { return d.PullNotice(toReview[i]) }},
		{name: "approvals", status: approvals, keys: noticeKeysOf(approvalList, approvalNoticeKey),
			notice: func(i int) Notice { return d.ApprovalNotice(approvalList[i]) }},
		{name: "questions", status: sessions, keys: noticeKeysOf(questions, questionNoticeKey),
			notice: func(i int) Notice { return d.QuestionNotice(questions[i]) }},
	}
}

// noticeKeysOf are the keys of items, by key.
func noticeKeysOf[T any](items []T, key func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = key(it)
	}
	return out
}

// NoticeKeysKept is the most keys a service keeps between runs. Past it,
// the oldest keys of what no longer waits go first; a key of what still
// waits is never dropped, so it never notifies twice.
const NoticeKeysKept = 500

// noticeBook is the Store's record of what it notified, for one sign-in:
// the keys it has seen, kept between runs for the service; the lists whose
// first load it took as seen; and the runs it follows, of the sessions the
// owner sent a message to from this desktop. It belongs to the UI thread,
// as the Store.
type noticeBook struct {
	known map[string]bool
	order []string // known's keys, oldest first
	// baseline holds the lists whose first load was taken as seen;
	// scanned is the Data's Rev the lists were last scanned at, and
	// current the keys of what waited then.
	baseline map[string]bool
	scanned  uint64
	current  map[string]bool
	// runs are the runs followed, by session.
	runs map[int64]*runWatch
}

// runWatch follows the turns of a session the owner sent a message to
// from this desktop, until every such message is settled (ADR-0049). A
// turn is a user entry, by its submission, and what came after it: it ends
// with its final answer (an assistant entry that stops for no tool, as
// pocketd's own rule for a run's end has it, "a final answer ends its
// run"), or with the next user entry. A turn of the owner's message that
// ends with its final answer notifies.
type runWatch struct {
	// sending counts the owner's messages to the session that pocketd has
	// not answered yet, and mine are the submissions it answered with,
	// not settled: a message the session queues waits there while another
	// turn runs.
	sending int
	mine    map[int64]bool
	// turn is the submission of the newest user entry seen, 0 for none,
	// and final the final answer after it, nil before one: only its first
	// lines are kept.
	turn  int64
	final *proto.Entry
	// unclaimed are the turns that ended while a message was on its way,
	// not known as the owner's: their final answers, or nil for one the
	// next message ended. The answer that names one settles it.
	unclaimed map[int64]*proto.Entry
	// done are the turns of the owner's messages that ended with their
	// final answers, for the next scan to notify.
	done []finishedTurn
	// reading says the session's transcript is read again after a resync,
	// reads numbers those reads, and held are the entries that came
	// meanwhile, newer than it.
	reading bool
	reads   int
	held    []proto.Entry
}

// finishedTurn is a turn of the owner's message, by its submission, and
// its final answer.
type finishedTurn struct {
	submission int64
	final      proto.Entry
}

// reset starts the record of a new sign-in, with the keys kept for its
// service.
func (b *noticeBook) reset(keys []string) {
	*b = noticeBook{known: map[string]bool{}, baseline: map[string]bool{}, runs: map[int64]*runWatch{}}
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

// sending records that the owner's message to session id is on its way:
// the session's entries are followed from now on.
func (b *noticeBook) sending(id int64) {
	w := b.runs[id]
	if w == nil {
		w = &runWatch{mine: map[int64]bool{}, unclaimed: map[int64]*proto.Entry{}}
		b.runs[id] = w
	}
	w.sending++
}

// sent records pocketd's answer to the owner's message to session id:
// ok with the submission it took the message as, or not taken. The
// message's turn may have begun, and even ended, before the answer came.
func (b *noticeBook) sent(id, submission int64, ok bool) {
	w := b.runs[id]
	if w == nil {
		return
	}
	w.sending--
	if ok && submission > 0 {
		switch final, ended := w.unclaimed[submission]; {
		case ended && final != nil:
			w.done = append(w.done, finishedTurn{submission: submission, final: *final})
		case ended:
			// Its turn ended with the next message, with no final answer.
		default:
			w.mine[submission] = true
		}
		delete(w.unclaimed, submission)
	}
	if w.sending == 0 {
		clear(w.unclaimed)
	}
	w.settle()
}

// entry follows an entry of the stream, of any session: only those of the
// sessions whose runs are followed count.
func (b *noticeBook) entry(e proto.Entry) {
	w := b.runs[e.SessionID]
	switch {
	case w == nil:
	case w.reading:
		w.held = append(w.held, slimEntry(e))
	default:
		w.follow(slimEntry(e))
	}
}

// slimEntry is what a turn keeps of entry e: its kind, its submission, how
// it stopped, and the first line of its text and its error.
func slimEntry(e proto.Entry) proto.Entry {
	return proto.Entry{ID: e.ID, SessionID: e.SessionID, Kind: e.Kind, SubmissionID: e.SubmissionID, StopReason: e.StopReason,
		Text: noticeText(e.Text, noticeBodyMost), Error: noticeText(e.Error, noticeBodyMost)}
}

// follow takes the session's next entry e. A user entry ends the turn
// before it, with no final answer if none came: the last message is
// another now. A final answer ends its turn.
func (w *runWatch) follow(e proto.Entry) {
	switch e.Kind {
	case proto.EntryUser:
		if w.turn != 0 && w.turn != e.SubmissionID {
			delete(w.mine, w.turn)
			if w.sending > 0 {
				// The answer naming it may still come: it settles nothing.
				w.unclaimed[w.turn] = w.final
			}
		}
		w.turn, w.final = e.SubmissionID, nil
	case proto.EntryAssistant:
		if e.StopReason == "toolUse" || w.turn == 0 {
			return
		}
		w.final = &e
		w.settle()
	}
}

// settle ends the turn whose final answer came: the owner's notifies; one
// no message on its way may be the owner's is another's.
func (w *runWatch) settle() {
	if w.turn == 0 || w.final == nil {
		return
	}
	switch {
	case w.mine[w.turn]:
		w.done = append(w.done, finishedTurn{submission: w.turn, final: *w.final})
		delete(w.mine, w.turn)
	case w.sending > 0:
		return // the answer naming it may yet come
	}
	w.turn, w.final = 0, nil
}

// idle says nothing of the session needs following any more.
func (w *runWatch) idle() bool {
	return w.sending == 0 && len(w.mine) == 0 && len(w.done) == 0 && !w.reading
}

// resync starts reading again the transcripts of the sessions whose runs
// are followed, after pocketd's resync: the entries between are lost to
// the stream. It returns those sessions, in order; the entries that come
// meanwhile wait for the transcript (reread).
func (b *noticeBook) resync() []transcriptRead {
	var out []transcriptRead
	for id, w := range b.runs {
		w.reading, w.held = true, nil
		w.turn, w.final = 0, nil
		w.reads++
		out = append(out, transcriptRead{session: id, read: w.reads})
	}
	slices.SortFunc(out, func(a, b transcriptRead) int { return cmp.Compare(a.session, b.session) })
	return out
}

// transcriptRead is a read of a session's transcript after a resync, by
// its number: only the newest read of a session counts.
type transcriptRead struct {
	session int64
	read    int
}

// reread takes the transcript read r brought (ok false if the read
// failed): its entries, then those the stream brought meanwhile that are
// newer. Without it, only those the stream brought count. A read a later
// resync began again over counts for nothing.
func (b *noticeBook) reread(r transcriptRead, entries []proto.Entry, ok bool) {
	w := b.runs[r.session]
	if w == nil || !w.reading || w.reads != r.read {
		return
	}
	w.reading = false
	var last int64
	if ok {
		for _, e := range entries {
			w.follow(slimEntry(e))
			last = max(last, e.ID)
		}
	}
	for _, e := range w.held {
		if e.ID > last {
			w.follow(e)
		}
	}
	w.held = nil
}

// scan decides the notices of what is new in d since the last scan, each
// key once, and reports whether the keys to keep changed. lists says the
// lists may have changed: they are scanned again only when the Data did.
// A list notifies nothing until its first load, which it takes as seen;
// then each thing that waits with a key not seen notifies. Then each turn
// of the owner's message that ended with its final answer notifies.
func (b *noticeBook) scan(d *Data, lists bool) (out []Notice, changed bool) {
	if lists && (b.current == nil || d.Rev() != b.scanned) {
		b.scanned = d.Rev()
		out, changed = b.scanLists(d.waitingLists())
	}
	ids := make([]int64, 0, len(b.runs))
	for id := range b.runs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		w := b.runs[id]
		for _, t := range w.done {
			if n := d.RunNotice(id, t.submission, t.final); b.know(n.Key) {
				out = append(out, n)
				changed = true
			}
		}
		w.done = nil
		if w.idle() {
			delete(b.runs, id)
		}
	}
	return out, b.prune() || changed
}

// scanLists is scan's part for lists: the notices of the keys not seen,
// made for those alone.
func (b *noticeBook) scanLists(lists []waitingList) (out []Notice, changed bool) {
	b.current = map[string]bool{}
	for _, l := range lists {
		for _, k := range l.keys {
			b.current[k] = true
		}
		if !b.baseline[l.name] {
			if l.status != ListLoaded {
				continue
			}
			b.baseline[l.name] = true
			for _, k := range l.keys {
				changed = b.know(k) || changed
			}
			continue
		}
		for i, k := range l.keys {
			if b.know(k) {
				out = append(out, l.notice(i))
				changed = true
			}
		}
	}
	return out, changed
}

// prune drops the oldest keys past NoticeKeysKept of what no longer waits
// (current holds the keys of what waits), and reports whether it dropped
// any.
func (b *noticeBook) prune() bool {
	drop := len(b.order) - NoticeKeysKept
	if drop <= 0 {
		return false
	}
	kept := make([]string, 0, NoticeKeysKept)
	for _, k := range b.order {
		if drop > 0 && !b.current[k] {
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
