package state

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// noticeHarness is a harness whose store hands its notices and the keys it
// keeps to the test, as the app takes them (ADR-0049). kept stands for the
// settings file, by service URL.
type noticeHarness struct {
	*harness
	notices []Notice
	kept    map[string][]string
	keeps   int
	changes int
}

func newNoticeHarness(t *testing.T, kept map[string][]string) *noticeHarness {
	t.Helper()
	h := &noticeHarness{harness: newHarness(t), kept: kept}
	if h.kept == nil {
		h.kept = map[string][]string{}
	}
	h.store.Close()
	h.store = h.noticeStore(t)
	return h
}

// restarted is the app started again: a new store of a new loop, on the
// same pocketd, with the keys kept.
func (h *noticeHarness) restarted(t *testing.T) *noticeHarness {
	t.Helper()
	again := newNoticeHarness(t, h.kept)
	again.store.Close()
	again.srv = h.srv
	again.store = again.noticeStore(t)
	return again
}

// noticeStore is a store of the harness's loop, clock and pocketd that
// hands its notices and keys to the harness.
func (h *noticeHarness) noticeStore(t *testing.T) *Store {
	t.Helper()
	s := New(Options{
		Post: h.loop.Post, After: h.clock.After, Transport: h.srv.Transport(),
		Now: func() time.Time { return h.now }, TempDir: t.TempDir(),
		Notify:      func(n Notice) { h.notices = append(h.notices, n) },
		KeptNotices: func(server string) []string { return h.kept[server] },
		KeepNotices: func(server string, keys []string) { h.kept[server] = keys; h.keeps++ },
		Changed:     func() { h.changes++ },
	})
	t.Cleanup(s.Close)
	return s
}

// noticeKeys are the notices' keys, in order.
func noticeKeys(notices []Notice) []string {
	var out []string
	for _, n := range notices {
		out = append(out, n.Key)
	}
	return out
}

// until runs the loop until cond holds, then until every request is in.
func (h *noticeHarness) until(t *testing.T, cond func() bool) {
	t.Helper()
	h.loop.RunUntil(t, cond)
	h.settle(t)
}

func threadAsk(id, thread int64, question string) proto.Ask {
	return proto.Ask{ID: id, Thread: thread, SessionID: 10, Question: question, State: proto.AskOpen,
		CreatedAt: pocketdtest.Now.Add(time.Duration(id) * time.Minute)}
}

// A new ask from a thread notifies once, and what waited at the first load
// does not: a reconnect, pocketd's restart (a resync, and every list
// fetched again) and a store started again with the kept keys show
// nothing again. The issue's "Done when" (frostyard/pocketgo#98).
func TestANewAskNotifiesOnceAndNeverAgain(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddThread(proto.Thread{ID: 3, Title: "Fix the login", Agent: "duncan", State: proto.ThreadBlocked})
	h.srv.AddAsk(threadAsk(1, 3, "Which branch?"))
	h.connect(t)
	if len(h.notices) != 0 {
		t.Fatalf("the first load notified %q: what waits then is the badge's", noticeKeys(h.notices))
	}

	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(threadAsk(2, 3, "Merge it now?\nOr wait for CI."))})
	h.until(t, func() bool { return len(h.store.Data.Asks()) == 2 })
	want := Notice{Key: "ask:2", Kind: NoticeAsks, Title: "A2 asks you", Subtitle: "T3 · Fix the login", Body: "Merge it now?",
		Target: NoticeTarget{Card: AskCard(2)}}
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices %+v, want one: %+v", h.notices, want)
	}
	if got := h.kept[h.srv.URL]; !slices.Contains(got, "ask:1") || !slices.Contains(got, "ask:2") {
		t.Errorf("kept keys %q, want both asks'", got)
	}

	// A reconnect replays nothing applied, and fetches nothing that loaded.
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	h.until(t, func() bool { return h.store.Conn.State == Connected })
	// pocketd restarting resyncs: every list is fetched again.
	h.srv.Restart()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.clock.Fire()
	h.until(t, func() bool { return len(h.srv.RequestsTo(http.MethodGet, proto.PathAsks)) == 2 })
	// The same event again, as a second copy of it.
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(threadAsk(2, 3, "Merge it now?"))})
	h.settle(t)
	if len(h.notices) != 1 {
		t.Fatalf("notices %q after a reconnect, a resync and the event again, want the one", noticeKeys(h.notices))
	}

	// The app started again, with the keys kept: nothing again.
	again := h.restarted(t)
	again.connect(t)
	if len(again.notices) != 0 {
		t.Errorf("a restart notified %q", noticeKeys(again.notices))
	}
}

// The keys kept from the last run hold where the first load cannot: a
// question notified before, missing from the first load while pocketd
// restarts its task, and back with the same id (protocol rule 61), shows
// nothing again; without the kept key it would.
func TestAKeptKeyHoldsAcrossARestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		kept []string
		want int
	}{
		{"kept", []string{"question:q1"}, 0},
		{"not kept", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNoticeHarness(t, nil)
			h.kept[h.srv.URL] = tc.kept
			h.srv.AddSession(session(1, "Odrade", 0))
			h.connect(t)
			s := session(1, "Odrade", time.Minute)
			s.State = proto.SessionWaiting
			s.Question = &proto.Question{ID: "q1", SessionID: 1, Question: "Ship it?", State: proto.QuestionWaiting}
			h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
			h.until(t, func() bool { return len(h.store.Data.QuestionsWaiting()) == 1 })
			if len(h.notices) != tc.want {
				t.Errorf("notices %q, want %d", noticeKeys(h.notices), tc.want)
			}
		})
	}
}

// A list notifies nothing until its first load, which it takes as seen:
// an event before it, or while its first fetch failed, brings a thing the
// load then counts as waiting already. Only what comes after notifies.
func TestAListNotifiesOnlyAfterItsFirstLoad(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.FailOnce(http.MethodGet, proto.PathAsks, http.StatusBadGateway)
	h.connect(t)
	if status, _ := h.store.Data.AsksStatus(); status != ListFailed {
		t.Fatalf("asks %v, want failed", status)
	}
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(threadAsk(1, 3, "While the list failed"))})
	h.until(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	if len(h.notices) != 0 {
		t.Fatalf("a list that never loaded notified %q", noticeKeys(h.notices))
	}
	h.store.RetryNow()
	h.until(t, func() bool { status, _ := h.store.Data.AsksStatus(); return status == ListLoaded })
	if len(h.notices) != 0 {
		t.Fatalf("the first load notified %q", noticeKeys(h.notices))
	}
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(threadAsk(2, 3, "After it"))})
	h.until(t, func() bool { return len(h.store.Data.Asks()) == 2 })
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"ask:2"}) {
		t.Errorf("notices %q, want ask:2 alone", got)
	}
}

// A sign-in begins again: what waits at its first load is seen, and the
// keys it reads are its own service's.
func TestASignInReadsItsServicesKeys(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.kept["https://other.example"] = []string{"ask:9"}
	h.connect(t)
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(threadAsk(9, 3, "Ours"))})
	h.until(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"ask:9"}) {
		t.Errorf("notices %q: another service's key held for this one", got)
	}
	if !slices.Equal(h.kept["https://other.example"], []string{"ask:9"}) {
		t.Errorf("the other service's keys became %q", h.kept["https://other.example"])
	}
}

// Each kind's key is the thing it is about: an approval, a question and an
// ask by id, a pull request at its head, a plan at its revision, a lesson
// at the proposal it shows. A push, a new revision and another proposal
// notify again; a change that leaves the key does not.
func TestEachThingNotifiesOnceByItsKey(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Odrade", 0))
	h.srv.AddThread(proto.Thread{ID: 3, Title: "Fix the login", Agent: "duncan", State: proto.ThreadOpen})
	h.connect(t)
	// Each event is applied before the next goes: a fetch's answer that
	// lands on the loop also moves Data.Rev, so waiting for Rev alone could
	// read the notices before the event itself was applied
	// (frostyard/pocketgo#122).
	emit := func(ev proto.Event) {
		t.Helper()
		h.srv.Emit(ev)
		h.until(t, func() bool { return h.store.lastSeq == h.srv.Seq() })
	}
	pull := proto.PullRequest{ID: 5, Thread: 3, Repository: "frostyard/pocketgo", Number: 98, Title: "Notify the owner", State: proto.PullOpen,
		Head: "bk/notify", HeadSHA: "aaa", Base: "main"}
	plan := proto.Plan{ID: 4, Thread: 3, Lead: "duncan", Title: "The plan", State: proto.PlanReview, Rev: "r1",
		Graph: &proto.PlanGraph{Rev: "r1", Version: 1}}
	lesson := proto.Lesson{ID: 7, Agent: "duncan", Text: "Run make verify first.", State: proto.LessonPending}
	s := session(1, "Odrade", time.Minute)
	s.State = proto.SessionWaiting
	s.Question = &proto.Question{ID: "q1", SessionID: 1, Question: "Ship it?", State: proto.QuestionWaiting}

	emit(proto.Event{Type: proto.EventApproval, Approval: new(approval("a1", 1, "c1", 0))})
	emit(proto.Event{Type: proto.EventSession, Session: &s})
	emit(proto.Event{Type: proto.EventPull, Pull: &pull})
	emit(proto.Event{Type: proto.EventPlan, Plan: &plan})
	emit(proto.Event{Type: proto.EventLesson, Lesson: &lesson})
	// What leaves the key as it was notifies nothing again.
	pull.Title = "Notify the owner, retitled"
	emit(proto.Event{Type: proto.EventPull, Pull: &pull})
	plan.Title = "The plan, retitled"
	emit(proto.Event{Type: proto.EventPlan, Plan: &plan})
	s.Title = "Odrade, renamed"
	emit(proto.Event{Type: proto.EventSession, Session: &s})
	// A push, a new revision with its graph and another proposal do.
	pull.HeadSHA = "bbb"
	emit(proto.Event{Type: proto.EventPull, Pull: &pull})
	plan.Rev, plan.Graph = "r2", &proto.PlanGraph{Rev: "r2", Version: 2}
	emit(proto.Event{Type: proto.EventPlan, Plan: &plan})
	lesson.State, lesson.Change = proto.LessonActive, &proto.LessonChange{Text: "Run make ci first.", Writer: "duncan"}
	emit(proto.Event{Type: proto.EventLesson, Lesson: &lesson})

	sum := func(l proto.Lesson) string { return h.store.Data.LessonNotice(l).Key }
	pending := proto.Lesson{ID: 7, Agent: "duncan", Text: "Run make verify first.", State: proto.LessonPending}
	want := []string{"approval:a1", "question:q1", "pull:5:aaa", "plan:4:r1", sum(pending), "pull:5:bbb", "plan:4:r2", sum(lesson)}
	if got := noticeKeys(h.notices); !slices.Equal(got, want) {
		t.Errorf("notices %q, want %q", got, want)
	}
	if sum(pending) == sum(lesson) {
		t.Error("another proposal of a lesson has the key of the first")
	}
}

// What waits for no one notifies nothing: a draft pull request, a plan
// whose gate still waits, an answered ask, a decided approval.
func TestWhatWaitsForNoOneNotifiesNothing(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.connect(t)
	h.srv.Emit(proto.Event{Type: proto.EventPull, Pull: &proto.PullRequest{ID: 5, Thread: 3, State: proto.PullOpen, Draft: true, HeadSHA: "aaa"}})
	h.srv.Emit(proto.Event{Type: proto.EventPlan, Plan: &proto.Plan{ID: 4, Thread: 3, State: proto.PlanReview, Rev: "r1",
		Graph: &proto.PlanGraph{Rev: "r1", Version: 1}, Waiting: []string{"A3 open"}}})
	answered := threadAsk(1, 3, "Done?")
	answered.State = proto.AskAnswered
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &answered})
	decided := approval("a1", 1, "c1", 0)
	decided.State = proto.ApprovalAllowed
	last := h.srv.Emit(proto.Event{Type: proto.EventApproval, Approval: &decided})
	h.until(t, func() bool { return h.store.lastSeq == last.Seq })
	if len(h.notices) != 0 {
		t.Errorf("notices %q of what waits for no one", noticeKeys(h.notices))
	}
}

// Each notice says what waits and where, as its card does, and a click
// opens its card, or the session of a question.
func TestNoticeWords(t *testing.T) {
	var d Data
	d.SetProfiles(d.Begin(), []proto.Profile{{ID: "duncan", Name: "Duncan"}, {ID: "odrade", Name: "Odrade"}})
	odrade := session(1, "Supervisor", 0)
	odrade.Profile = "odrade"
	d.SetSessions(d.Begin(), []proto.Session{odrade})
	d.applyThread(proto.Thread{ID: 3, Title: "Fix the login", Agent: "duncan"})

	args, _ := json.Marshal(map[string]string{"command": "go test ./..."})
	pull := proto.PullRequest{ID: 5, Thread: 3, Repository: "frostyard/pocketgo", Number: 98, Title: "Notify the owner", HeadSHA: "aaa"}
	for _, tc := range []struct {
		name string
		got  Notice
		want Notice
	}{
		{"an ask of a thread not known", d.AskNotice(proto.Ask{ID: 2, Thread: 9, SessionID: 1, Question: "\n  Which branch?  \nmain or dev"}),
			Notice{Key: "ask:2", Kind: NoticeAsks, Title: "A2 asks you", Subtitle: "Supervisor", Body: "Which branch?", Target: NoticeTarget{Card: "ask:2"}}},
		{"a question", d.QuestionNotice(proto.Question{ID: "q1", SessionID: 1, Question: "Ship it?"}),
			Notice{Key: "question:q1", Kind: NoticeAsks, Title: "Odrade waits for your answer", Subtitle: "Supervisor", Body: "Ship it?",
				Target: NoticeTarget{Session: 1}}},
		{"an approval", d.ApprovalNotice(proto.Approval{ID: "a1", SessionID: 1, Tool: "bash", Arguments: args}),
			Notice{Key: "approval:a1", Kind: NoticeApprovals, Title: "bash waits for your approval", Subtitle: "Supervisor",
				Body: "go test ./...", Target: NoticeTarget{Card: "approval:a1"}}},
		{"a pull request", d.PullNotice(pull),
			Notice{Key: "pull:5:aaa", Kind: NoticeReviews, Title: "frostyard/pocketgo#98 waits for your review", Subtitle: "T3 · Fix the login",
				Body: "Notify the owner", Target: NoticeTarget{Card: "pull:5"}}},
		{"a plan", d.PlanNotice(proto.Plan{ID: 4, Thread: 3, Lead: "duncan", Title: "The plan", Rev: "r1"}),
			Notice{Key: "plan:4:r1", Kind: NoticeReviews, Title: "P4 waits for your approval", Subtitle: "T3 · led by Duncan", Body: "The plan",
				Target: NoticeTarget{Card: "plan:4"}}},
		{"a run's answer", d.RunNotice(1, 12, proto.Entry{Text: "Done: the tests pass.\nDetails follow.", StopReason: "stop"}),
			Notice{Key: "run:1:12", Kind: NoticeRuns, Title: "Supervisor finished", Body: "Done: the tests pass.", Target: NoticeTarget{Session: 1}}},
		{"a failed run", d.RunNotice(1, 12, proto.Entry{Error: "rate limited", StopReason: "error"}),
			Notice{Key: "run:1:12", Kind: NoticeRuns, Title: "Supervisor finished", Body: "It stopped with an error: rate limited",
				Target: NoticeTarget{Session: 1}}},
		{"an aborted run", d.RunNotice(1, 12, proto.Entry{StopReason: "aborted"}),
			Notice{Key: "run:1:12", Kind: NoticeRuns, Title: "Supervisor finished", Body: "It was stopped.", Target: NoticeTarget{Session: 1}}},
		{"a run of a session not known", d.RunNotice(8, 3, proto.Entry{StopReason: "stop"}),
			Notice{Key: "run:8:3", Kind: NoticeRuns, Title: "Session 8 finished", Body: "Its answer is ready.", Target: NoticeTarget{Session: 8}}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		lesson proto.Lesson
		title  string
		body   string
	}{
		{proto.Lesson{ID: 7, Agent: "duncan", Text: "Run make verify.", State: proto.LessonPending},
			"A lesson for Duncan waits for your approval", "Run make verify."},
		{proto.Lesson{ID: 7, Agent: "duncan", Text: "Run make verify.", State: proto.LessonActive, Change: &proto.LessonChange{Text: "Run make ci."}},
			"New text for L7 of Duncan waits for your approval", "Run make ci."},
		{proto.Lesson{ID: 7, Agent: "duncan", Text: "Run make verify.", State: proto.LessonActive, Change: &proto.LessonChange{Forget: true}},
			"Forgetting L7 of Duncan waits for your approval", "Run make verify."},
	} {
		n := d.LessonNotice(tc.lesson)
		if n.Title != tc.title || n.Body != tc.body || n.Subtitle != "L7" || n.Kind != NoticeReviews || n.Target != (NoticeTarget{Card: "lesson:7"}) {
			t.Errorf("lesson notice %+v, want %q, %q", n, tc.title, tc.body)
		}
	}
	long := d.AskNotice(proto.Ask{ID: 1, Thread: 3, Question: strings.Repeat("word ", 100)})
	if n := len([]rune(long.Body)); n != noticeBodyMost {
		t.Errorf("a long question's body has %d characters, want %d", n, noticeBodyMost)
	}
}

// A notice shows only while the window is not focused, with notifications
// on and its kind's switch on; every switch is on by default.
func TestShowNotice(t *testing.T) {
	ask := Notice{Kind: NoticeAsks}
	if !ShowNotice(ask, NotifyPrefs{}, false) {
		t.Error("the default switches hide an ask")
	}
	if ShowNotice(ask, NotifyPrefs{}, true) {
		t.Error("a notice shows while the window is focused")
	}
	if ShowNotice(ask, NotifyPrefs{Off: true}, false) {
		t.Error("a notice shows with notifications off")
	}
	for _, k := range NoticeKinds {
		off := NotifyPrefs{}.WithKind(k, false)
		for _, other := range NoticeKinds {
			if got, want := ShowNotice(Notice{Kind: other}, off, false), other != k; got != want {
				t.Errorf("with %q off, a notice of %q shows %v, want %v", k.Title(), other.Title(), got, want)
			}
		}
		if off.WithKind(k, true) != (NotifyPrefs{}) {
			t.Errorf("%q switched off and on again is %+v", k.Title(), off.WithKind(k, true))
		}
		// Off keeps each kind's own switch, which shows again once on.
		if !(NotifyPrefs{Off: true}).KindOn(k) || (NotifyPrefs{Off: true}).On(k) {
			t.Errorf("notifications off changed %q's own switch", k.Title())
		}
		if k.Title() == "" || k.Subtitle() == "" {
			t.Errorf("kind %d has no words", k)
		}
	}
	if ShowNotice(Notice{}, NotifyPrefs{}, false) {
		t.Error("a notice of no kind shows")
	}
}

// iOS shows no notification: it keeps the badge alone.
func TestOnlyTheDesktopsNotify(t *testing.T) {
	for goos, want := range map[string]bool{"ios": false, "darwin": true, "linux": true, "windows": true} {
		if got := Notifies(goos); got != want {
			t.Errorf("Notifies(%q) = %v, want %v", goos, got, want)
		}
	}
	// MyGo v0.10.0 shows the icon's badge on macOS and Linux alone (Codex
	// review of #104, round 1).
	for goos, want := range map[string]bool{"ios": false, "darwin": true, "linux": true, "windows": false} {
		if got := BadgesIcon(goos); got != want {
			t.Errorf("BadgesIcon(%q) = %v, want %v", goos, got, want)
		}
	}
}

// The app's icon counts what the Inbox counts, the same number as the
// Inbox tab's badge up to its most.
func TestInboxCountIsTheBadgesNumber(t *testing.T) {
	kinds := []InboxKind{{Count: 2, Status: ListLoaded}, {Count: 0, Status: ListFailed}, {Count: 5, Status: ListLoaded}}
	if got := InboxCount(kinds); got != 7 {
		t.Errorf("InboxCount = %d, want 7", got)
	}
	if got := InboxBadge(kinds); got != strconv.Itoa(InboxCount(kinds)) {
		t.Errorf("the tab's badge %q is not the count", got)
	}
	if InboxCount(nil) != 0 {
		t.Error("nothing counts as something")
	}
}

// A notification's id names its service and what a click opens, whatever
// characters an approval's id holds; an id not PocketGo's, or one that
// names neither or both, opens nothing.
func TestNoticeIDsRoundTrip(t *testing.T) {
	for _, target := range []NoticeTarget{{Session: 7}, {Card: AskCard(12)}, {Card: ApprovalCard("a&b=c d@e")}, {Card: PullCard(5)}} {
		id := NoticeID("http://pocketd.example.ts.net:8788", target)
		server, got, ok := ParseNoticeID(id)
		if !ok || server != "http://pocketd.example.ts.net:8788" || got != target {
			t.Errorf("ParseNoticeID(%q) = %q, %+v, %v, want %+v", id, server, got, ok, target)
		}
	}
	for _, id := range []string{"", "mygo-123", "pocketgo-notice:", "pocketgo-notice:card=ask%3A1",
		"pocketgo-notice:server=http%3A%2F%2Fa&session=1&card=ask%3A1", "pocketgo-notice:server=http%3A%2F%2Fa&session=-1",
		"pocketgo-notice:server=http%3A%2F%2Fa", "pocketgo-notice:%zz"} {
		if _, _, ok := ParseNoticeID(id); ok {
			t.Errorf("ParseNoticeID(%q) names a target", id)
		}
	}
}

// The keys kept stay under NoticeKeysKept: the oldest of what no longer
// waits go first, and what still waits keeps its key, however old.
func TestTheKeptKeysStayBounded(t *testing.T) {
	var d Data
	d.SetAsks(d.BeginAsks(), []proto.Ask{threadAsk(1, 3, "Still waits")})
	var b noticeBook
	keys := []string{"ask:1"}
	for i := range NoticeKeysKept + 10 {
		keys = append(keys, "run:1:"+strconv.Itoa(i))
	}
	b.reset(keys)
	if _, changed := b.scan(&d, true); !changed {
		t.Fatal("a scan over the limit kept every key")
	}
	got := b.keys()
	if len(got) != NoticeKeysKept || got[0] != "ask:1" || got[1] != "run:1:11" || !b.known["ask:1"] || b.known["run:1:0"] {
		t.Errorf("kept %d keys from %q, want %d with ask:1 first, then run:1:11", len(got), got[:3], NoticeKeysKept)
	}
	if _, changed := b.scan(&d, true); changed {
		t.Error("a scan under the limit changed the keys")
	}
}

// send sends text to session id as the owner and waits until pocketd
// took it.
func (h *noticeHarness) send(t *testing.T, id int64, text string) {
	t.Helper()
	sent := false
	h.store.Send(id, text, func(err error) {
		if err != nil {
			t.Errorf("send: %v", err)
		}
		sent = true
	})
	h.until(t, func() bool { return sent })
}

// final writes session id's final answer, as a run's end does.
func (h *noticeHarness) final(t *testing.T, id int64, text, stop string) {
	t.Helper()
	last := h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 100, SessionID: id, Kind: proto.EntryAssistant, Text: text, StopReason: stop}})
	h.loop.RunUntil(t, func() bool { return h.store.lastSeq == last.Seq })
}

// A run that answers the owner's message from this desktop notifies as its
// final answer comes, once; a round that calls a tool does not end it.
func TestARunNotifiesWhenItAnswersTheOwnersMessage(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Fix the flaky CI", 0))
	h.connect(t)
	h.send(t, 1, "Run the tests")
	h.final(t, 1, "Running them.", "toolUse")
	if len(h.notices) != 0 {
		t.Fatalf("a tool round notified %q", noticeKeys(h.notices))
	}
	h.final(t, 1, "They pass.", "stop")
	want := Notice{Key: "run:1:1", Kind: NoticeRuns, Title: "Fix the flaky CI finished", Body: "They pass.", Target: NoticeTarget{Session: 1}}
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices %+v, want %+v", h.notices, want)
	}
	// Another final answer of the same turn notifies nothing.
	h.final(t, 1, "Also, the lint passes.", "stop")
	if len(h.notices) != 1 {
		t.Errorf("notices %q after a second answer of the turn", noticeKeys(h.notices))
	}
	// The next message's run is another.
	h.send(t, 1, "Push it")
	h.final(t, 1, "Pushed.", "stop")
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"run:1:1", "run:1:2"}) {
		t.Errorf("notices %q, want both runs'", got)
	}
}

// Only a run whose last message is the owner's notifies: a message of a
// supervisor's, or one pocketd placed, after the owner's makes the answer
// another's; one that came before the owner's entry does not.
func TestARunAfterAnothersMessageDoesNotNotify(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Duncan", 0))
	h.srv.AddSession(session(2, "Chani", 0))
	h.connect(t)
	h.send(t, 1, "Look at the login")
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 50, SessionID: 1, Kind: proto.EntryUser, Text: "From Odrade",
		SubmissionID: 77, Sender: &proto.Sender{Kind: proto.SenderSupervisor, Session: 9}}})
	h.final(t, 1, "On it, Odrade.", "stop")
	if len(h.notices) != 0 {
		t.Fatalf("an answer to a supervisor's message notified %q", noticeKeys(h.notices))
	}

	// A message placed before the owner's, still queued as the owner sent.
	h.srv.QueueMessages(true)
	h.send(t, 2, "And the logout")
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 60, SessionID: 2, Kind: proto.EntryUser, Text: "A report", SubmissionID: 76}})
	h.srv.PlaceQueued(2)
	h.final(t, 2, "Both done.", "stop")
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"run:2:2"}) {
		t.Errorf("notices %q, want the owner's run in session 2", got)
	}
}

// pocketd's answer to the owner's message may come after its entry, and
// after the run's final answer: the run notifies once the answer comes.
func TestARunWhoseSendAnswerCameLastNotifies(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Fix the flaky CI", 0))
	h.connect(t)
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathMessages, 1))
	sent := false
	h.store.Send(1, "Run the tests", func(err error) { sent = err == nil })
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, proto.Path(proto.PathMessages, 1)) })
	h.final(t, 1, "They pass.", "stop")
	if len(h.notices) != 0 {
		t.Fatalf("notices %q before pocketd's answer named the message", noticeKeys(h.notices))
	}
	release()
	h.until(t, func() bool { return sent })
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"run:1:1"}) {
		t.Errorf("notices %q, want the run's", got)
	}
}

// Every applied event and answer tells Options.Changed, so the app's
// badge follows the Inbox; and a sign-out keeps nothing.
func TestChangesAreTold(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.connect(t)
	before := h.changes
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: new(threadAsk(1, 3, "Q"))})
	h.until(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	if h.changes == before {
		t.Error("an event told no change")
	}
	h.store.SignOut()
	if len(h.store.notes.known) != 0 || len(h.store.notes.runs) != 0 {
		t.Errorf("a sign-out kept %d keys and %d runs", len(h.store.notes.known), len(h.store.notes.runs))
	}
}

// The owner's message queued while the run of the one before works does
// not take that run's place: the first's final answer notifies, then the
// second's (Codex review of #104, round 1).
func TestAQueuedMessageKeepsTheRunBeforeIt(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Fix the flaky CI", 0))
	h.connect(t)
	h.send(t, 1, "Run the tests")
	h.srv.QueueMessages(true)
	h.send(t, 1, "Then push it")
	h.final(t, 1, "They pass.", "stop")
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"run:1:1"}) {
		t.Fatalf("notices %q, want the first message's run", got)
	}
	h.srv.PlaceQueued(1)
	h.final(t, 1, "Pushed.", "stop")
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"run:1:1", "run:1:2"}) {
		t.Errorf("notices %q, want both messages' runs", got)
	}
}

// A final answer written while the stream was gone past pocketd's replay
// (a resync) still notifies, read from the session's transcript; another's
// message then makes the answer after it another's (Codex review of #104,
// round 1).
func TestARunThatEndedDuringAResyncNotifies(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Fix the flaky CI", 0))
	h.srv.AddSession(session(2, "Chani", 0))
	h.connect(t)
	h.send(t, 1, "Run the tests")
	h.send(t, 2, "Look at the logout")
	h.srv.Restart()
	h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant, Text: "They pass.", StopReason: "stop"}})
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 2, SessionID: 2, Kind: proto.EntryUser, Text: "From Odrade",
		SubmissionID: 77, Sender: &proto.Sender{Kind: proto.SenderSupervisor, Session: 9}}})
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 3, SessionID: 2, Kind: proto.EntryAssistant, Text: "On it, Odrade.", StopReason: "stop"}})
	h.clock.Fire()
	h.until(t, func() bool { return len(h.srv.RequestsTo(http.MethodGet, proto.Path(proto.PathSession, 2))) == 1 })
	if got := noticeKeys(h.notices); !slices.Equal(got, []string{"run:1:1"}) {
		t.Errorf("notices %q, want session 1's run alone", got)
	}
	if len(h.store.notes.runs) != 0 {
		t.Errorf("%d runs still followed after both settled", len(h.store.notes.runs))
	}
}

// Of two resyncs, only the second's read of a transcript counts: the
// first's may miss what the second lost.
func TestOnlyTheNewestTranscriptReadCounts(t *testing.T) {
	var b noticeBook
	b.reset(nil)
	b.sending(1)
	b.sent(1, 5, true)
	first := b.resync()
	second := b.resync()
	transcript := []proto.Entry{{ID: 1, SessionID: 1, Kind: proto.EntryUser, SubmissionID: 5},
		{ID: 2, SessionID: 1, Kind: proto.EntryAssistant, Text: "Done.", StopReason: "stop"}}
	b.reread(first[0], transcript[:1], true)
	if w := b.runs[1]; !w.reading || w.turn != 0 {
		t.Fatalf("the first read counted: %+v", w)
	}
	b.reread(second[0], transcript, true)
	var d Data
	if out, _ := b.scan(&d, false); len(out) != 1 || out[0].Key != "run:1:5" {
		t.Errorf("notices %+v, want run:1:5's", out)
	}
}

// Only the sessions the owner sent a message to from this desktop are
// followed, and only until the message settles; what a turn keeps of an
// answer is its first line, cut short (Codex review of #104, round 1).
func TestOnlyTheOwnersRunsAreFollowedAndBriefly(t *testing.T) {
	h := newNoticeHarness(t, nil)
	h.srv.AddSession(session(1, "Fix the flaky CI", 0))
	h.srv.AddSession(session(2, "A worker", 0))
	h.connect(t)
	h.srv.Emit(proto.Event{Type: proto.EventEntry, Entry: &proto.Entry{ID: 1, SessionID: 2, Kind: proto.EntryUser, Text: "Work", SubmissionID: 5}})
	h.final(t, 2, strings.Repeat("A long answer. ", 500), "stop")
	if len(h.store.notes.runs) != 0 {
		t.Fatalf("%d runs followed with no message of the owner's", len(h.store.notes.runs))
	}
	h.send(t, 1, "Run the tests")
	if len(h.store.notes.runs) != 1 {
		t.Fatalf("%d runs followed, want the owner's one", len(h.store.notes.runs))
	}
	h.final(t, 1, strings.Repeat("They pass. ", 500), "stop")
	if len(h.notices) != 1 || len([]rune(h.notices[0].Body)) > noticeBodyMost {
		t.Errorf("notices %+v, want one with a short body", h.notices)
	}
	if len(h.store.notes.runs) != 0 {
		t.Errorf("%d runs still followed after the run notified", len(h.store.notes.runs))
	}

	// While an answer of pocketd's is on its way, the turn it may name
	// keeps the first line of its answer alone.
	w := &runWatch{sending: 1, mine: map[int64]bool{}, unclaimed: map[int64]*proto.Entry{}}
	w.follow(proto.Entry{Kind: proto.EntryUser, SubmissionID: 9})
	w.follow(slimEntry(proto.Entry{Kind: proto.EntryAssistant, Text: strings.Repeat("word ", 1000), StopReason: "stop"}))
	if w.final == nil || len([]rune(w.final.Text)) > noticeBodyMost {
		t.Errorf("the turn keeps %d characters of its answer", len([]rune(w.final.Text)))
	}
}

// A scan makes the words of a new key's notice alone, and scans the lists
// again only when the Data changed (Codex review of #104, round 1).
func TestAScanMakesOnlyNewNotices(t *testing.T) {
	var b noticeBook
	b.reset([]string{"ask:1", "ask:2"})
	b.baseline["asks"] = true
	made := 0
	out, _ := b.scanLists([]waitingList{{name: "asks", status: ListLoaded, keys: []string{"ask:1", "ask:2", "ask:3"},
		notice: func(i int) Notice { made++; return Notice{Key: "ask:" + strconv.Itoa(i+1)} }}})
	if made != 1 || len(out) != 1 || out[0].Key != "ask:3" {
		t.Errorf("made %d notices, %+v: want ask:3's alone", made, out)
	}

	var d Data
	d.SetAsks(d.BeginAsks(), []proto.Ask{threadAsk(1, 3, "Q")})
	b.scan(&d, true)
	if allocs := testing.AllocsPerRun(10, func() { b.scan(&d, true) }); allocs != 0 {
		t.Errorf("a scan of Data that did not change allocated %v times: it scanned the lists again", allocs)
	}
}
