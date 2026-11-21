package state

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func lesson(id int64, agent, text string, created time.Duration) proto.Lesson {
	return proto.Lesson{ID: id, Agent: agent, Text: text, State: proto.LessonPending, Writer: agent, CreatedAt: pocketdtest.Now.Add(created)}
}

func lessonIDs(list []proto.Lesson) []int64 {
	var out []int64
	for _, l := range list {
		out = append(out, l.ID)
	}
	return out
}

// Only lessons that wait are listed: a new one, or one in force with a
// change that waits. A lesson decided while a fetch was on its way stays
// gone when the fetch's stale copy arrives (the Data merge rule).
func TestLessonsFollowTheirEventsOverAStaleList(t *testing.T) {
	var d Data
	inForce := lesson(4, "vera", "d", 3*time.Minute)
	inForce.State = proto.LessonActive
	changed := inForce
	changed.ID, changed.Change = 5, &proto.LessonChange{Forget: true, Writer: "vera"}
	d.SetLessons(d.BeginLessons(), []proto.Lesson{lesson(1, "vera", "a", 0), inForce, changed})
	if got := lessonIDs(d.Lessons()); !slices.Equal(got, []int64{1, 5}) {
		t.Fatalf("lessons %v, want the waiting ones [1 5]", got)
	}
	f := d.BeginLessons()
	approved := lesson(1, "vera", "a", 0)
	approved.State = proto.LessonActive
	d.Apply(proto.Event{Type: proto.EventLesson, Lesson: &approved})
	put := lesson(2, "vera", "b", time.Minute)
	d.Apply(proto.Event{Type: proto.EventLesson, Lesson: &put})
	d.SetLessons(f, []proto.Lesson{lesson(1, "vera", "a", 0), changed})
	if got := lessonIDs(d.Lessons()); !slices.Equal(got, []int64{2, 5}) {
		t.Errorf("lessons %v, want [2 5]", got)
	}
	// An older fetch answering last changes nothing.
	older := d.BeginLessons()
	newest := d.BeginLessons()
	d.SetLessons(newest, nil)
	d.SetLessons(older, []proto.Lesson{lesson(3, "vera", "c", 0)})
	if len(d.Lessons()) != 0 {
		t.Errorf("an older fetch put back %v", lessonIDs(d.Lessons()))
	}
	d.SetLessonsError(d.BeginLessons(), "timeout")
	if st, err := d.LessonsStatus(); st != ListFailed || err != "timeout" {
		t.Errorf("a failed fetch: %v %q", st, err)
	}
	if d.lessons.touched != nil {
		t.Error("a merge marker outlived every fetch")
	}
}

// What the owner approves is what the card shows: the new lesson's text,
// a change's new text, or that the lesson is to be forgotten.
func TestSeenTextIsWhatWaits(t *testing.T) {
	inForce := lesson(1, "vera", "old", 0)
	inForce.State = proto.LessonActive
	for _, c := range []struct {
		l      proto.Lesson
		text   string
		forget bool
	}{
		{lesson(1, "vera", "new", 0), "new", false},
		{func() proto.Lesson { l := inForce; l.Change = &proto.LessonChange{Text: "newer"}; return l }(), "newer", false},
		{func() proto.Lesson { l := inForce; l.Change = &proto.LessonChange{Forget: true}; return l }(), "", true},
	} {
		if text, forget := SeenText(c.l); text != c.text || forget != c.forget {
			t.Errorf("SeenText(%+v) = %q, %v", c.l, text, forget)
		}
	}
}

// The owner's approval names what the card showed; a lesson that changed
// since is a conflict, which loads the lessons again and says so.
func TestApprovingALessonSendsWhatWasShown(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLesson(lesson(1, "vera", "Run the tests.", 0))
	h.srv.AddLesson(lesson(2, "vera", "Ask first.", time.Minute))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 2 })
	l, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(l, proto.LessonApprove)
	if !h.store.DecidingLesson(1).Busy {
		t.Error("the decision is not busy while it is on its way")
	}
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	var body proto.DecideLesson
	reqs := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathLesson, 1))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Decision != proto.LessonApprove ||
		body.Text != "Run the tests." || body.Forget || body.RequestID == "" {
		t.Fatalf("the decision sent %+v", reqs)
	}
	// Changed since the card showed it: conflict, and the lessons load
	// again with the new text.
	shown, _ := h.store.Data.Lesson(2)
	h.srv.AddLesson(lesson(2, "vera", "Ask the owner first.", time.Minute))
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathLessons)
	h.store.DecideLesson(shown, proto.LessonApprove)
	h.loop.RunUntil(t, func() bool { return h.store.DecidingLesson(2).Err != "" && !h.store.DecidingLesson(2).Busy })
	if !strings.Contains(h.store.DecidingLesson(2).Err, "changed since you looked") {
		t.Errorf("a conflict says %q", h.store.DecidingLesson(2).Err)
	}
	release()
	h.settle(t)
	if now, ok := h.store.Data.Lesson(2); !ok || now.Text != "Ask the owner first." {
		t.Errorf("after the reload: %+v", now)
	}
	// Discard drops it.
	now, _ := h.store.Data.Lesson(2)
	h.store.DecideLesson(now, proto.LessonDiscard)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 0 && !h.store.DecidingLesson(2).Busy })
	if _, kept := h.store.lessonDecisions[2]; kept {
		t.Error("the decision's state outlived its lesson")
	}
}

// A decision tried again unchanged after a failure keeps its request id,
// so a lost reply is not decided twice.
func TestALessonDecisionTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLesson(lesson(1, "vera", "Run the tests.", 0))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	p := proto.Path(proto.PathLesson, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	l, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(l, proto.LessonApprove)
	h.loop.RunUntil(t, func() bool { return h.store.DecidingLesson(1).Err != "" })
	h.store.DecideLesson(l, proto.LessonApprove)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 0 })
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] != got[1] {
		t.Errorf("request ids %q, want the retry to keep the first id", got)
	}
}

// A decision's reply that comes after a newer event about its lesson does
// not undo the event: a change the agent proposed after the decision stays
// in the Inbox (Codex review of #18, round 1).
func TestALateDecisionReplyKeepsANewerProposal(t *testing.T) {
	h := newHarness(t)
	inForce := lesson(1, "vera", "a", 0)
	inForce.State, inForce.Change = proto.LessonActive, &proto.LessonChange{Text: "b", Writer: "vera"}
	h.srv.AddLesson(inForce)
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathLesson, 1))
	shown, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(shown, proto.LessonApprove)
	// The decision's own event clears the card; then the agent proposes
	// again, before the reply comes.
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 0 })
	again := inForce
	again.Text, again.Change = "b", &proto.LessonChange{Text: "c", Writer: "vera"}
	h.srv.Emit(proto.Event{Type: proto.EventLesson, Lesson: &again})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.DecidingLesson(1).Busy })
	h.settle(t)
	if l, waits := h.store.Data.Lesson(1); !waits || l.Change == nil || l.Change.Text != "c" {
		t.Fatalf("the newer proposal: %+v, %v", l, waits)
	}
}

// A decision whose proposal stopped waiting while it was on its way, and
// whose reply was lost, leaves no request id behind: a decision on a new
// proposal of the same lesson, even with the same text, is a new request
// (Codex review of #18, round 1).
func TestALostDecisionDoesNotAnswerForTheNextProposal(t *testing.T) {
	h := newHarness(t)
	inForce := lesson(1, "vera", "a", 0)
	inForce.State, inForce.Change = proto.LessonActive, &proto.LessonChange{Text: "b", Writer: "vera"}
	h.srv.AddLesson(inForce)
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	p := proto.Path(proto.PathLesson, 1)
	release := h.srv.HoldAndLoseAnswerOnce(http.MethodPost, p)
	shown, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(shown, proto.LessonDiscard)
	// The discard took effect: its event clears the card. The agent
	// proposes the same text again, then the lost reply fails.
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 0 })
	h.srv.Emit(proto.Event{Type: proto.EventLesson, Lesson: &inForce})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.DecidingLesson(1).Busy })
	h.settle(t)
	shown, _ = h.store.Data.Lesson(1)
	h.store.DecideLesson(shown, proto.LessonDiscard)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 0 && !h.store.DecidingLesson(1).Busy })
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("request ids %q, want a new one for the new proposal", got)
	}
}

// A decision's reply that comes after a refresh begun since the decision
// does not undo what the refresh brought: a new proposal a fetch showed
// first, as after a resync, stays (Codex review of #18, round 2).
func TestALateDecisionReplyKeepsWhatARefreshBrought(t *testing.T) {
	h := newHarness(t)
	inForce := lesson(1, "vera", "a", 0)
	inForce.State, inForce.Change = proto.LessonActive, &proto.LessonChange{Text: "b", Writer: "vera"}
	h.srv.AddLesson(inForce)
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathLesson, 1))
	shown, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(shown, proto.LessonApprove)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 0 })
	// The agent proposes again; the client learns it from a refresh only.
	again := inForce
	again.Text, again.Change = "b", &proto.LessonChange{Text: "c", Writer: "vera"}
	h.srv.AddLesson(again)
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.DecidingLesson(1).Busy })
	h.settle(t)
	if l, waits := h.store.Data.Lesson(1); !waits || l.Change == nil || l.Change.Text != "c" {
		t.Fatalf("the proposal the refresh brought: %+v, %v", l, waits)
	}
}

// The lessons in force are kept apart from those that wait: the owner may
// forget each one, and the waiting count holds only what waits.
func TestLessonsInForceAreKeptForTheOwnersForget(t *testing.T) {
	var d Data
	inForce := lesson(1, "vera", "a", 0)
	inForce.State = proto.LessonActive
	changed := lesson(2, "vera", "b", time.Minute)
	changed.State, changed.Change = proto.LessonActive, &proto.LessonChange{Forget: true}
	d.SetLessons(d.BeginLessons(), []proto.Lesson{inForce, changed, lesson(3, "vera", "c", 2*time.Minute)})
	if got := lessonIDs(d.LessonsInForce()); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("in force %v", got)
	}
	if got := lessonIDs(d.Lessons()); !slices.Equal(got, []int64{2, 3}) {
		t.Fatalf("waiting %v", got)
	}
	forgotten := inForce
	forgotten.State = proto.LessonForgotten
	d.Apply(proto.Event{Type: proto.EventLesson, Lesson: &forgotten})
	if got := lessonIDs(d.LessonsInForce()); !slices.Equal(got, []int64{2}) {
		t.Fatalf("in force after a forget %v", got)
	}
}

// A decision's reply is marked as of when the decision began: a fetch
// begun after it, answering later, brings what it read, a new proposal
// the stream missed included (Codex review of #18, round 3).
func TestADecisionReplyIsMarkedAsOfItsRequest(t *testing.T) {
	var d Data
	inForce := lesson(1, "vera", "a", 0)
	inForce.State, inForce.Change = proto.LessonActive, &proto.LessonChange{Text: "b"}
	d.SetLessons(d.BeginLessons(), []proto.Lesson{inForce})
	decision := d.BeginLessonDecision()
	resync := d.BeginLessons()
	approved := inForce
	approved.Text, approved.Change = "b", nil
	d.LessonDecided(decision, approved)
	again := approved
	again.Change = &proto.LessonChange{Text: "c"}
	// The resync began while the decision was on its way: its copy may
	// predate the decision, so it asks for a fetch again (round 5), and
	// that fetch, begun after the reply, brings the new proposal.
	if !d.SetLessons(resync, []proto.Lesson{again}) {
		t.Fatal("an overlapping fetch that disagrees asked for no fetch")
	}
	if d.SetLessons(d.BeginLessons(), []proto.Lesson{again}) {
		t.Fatal("a fetch begun after the reply asked for another")
	}
	if l, waits := d.Lesson(1); !waits || l.Change.Text != "c" {
		t.Fatalf("the proposal the resync read: %+v, %v", l, waits)
	}
}

// A refresh that failed brought no lesson: it never makes a decision's
// reply look stale (Codex review of #18, round 3).
func TestAFailedRefreshDoesNotDropADecisionReply(t *testing.T) {
	var d Data
	d.SetLessons(d.BeginLessons(), []proto.Lesson{lesson(1, "vera", "a", 0)})
	decision := d.BeginLessonDecision()
	d.SetLessonsError(d.BeginLessons(), "timeout")
	approved := lesson(1, "vera", "a", 0)
	approved.State = proto.LessonActive
	d.LessonDecided(decision, approved)
	if _, waits := d.Lesson(1); waits {
		t.Fatal("an approved lesson still waits after a failed refresh")
	}
}

// The marker of an event a decision in flight needs outlives a fetch that
// answered meanwhile: the decision's reply never undoes that event (Codex
// review of #18, round 3).
func TestAPendingDecisionKeepsTheMarkersItNeeds(t *testing.T) {
	var d Data
	inForce := lesson(1, "vera", "a", 0)
	inForce.State, inForce.Change = proto.LessonActive, &proto.LessonChange{Text: "b"}
	d.SetLessons(d.BeginLessons(), []proto.Lesson{inForce})
	decision := d.BeginLessonDecision()
	again := inForce
	again.Text, again.Change = "b", &proto.LessonChange{Text: "c"}
	d.Apply(proto.Event{Type: proto.EventLesson, Lesson: &again})
	d.SetLessonsError(d.BeginLessons(), "timeout")
	approved := inForce
	approved.Text, approved.Change = "b", nil
	d.LessonDecided(decision, approved)
	if l, waits := d.Lesson(1); !waits || l.Change.Text != "c" {
		t.Fatalf("the proposal of the event: %+v, %v", l, waits)
	}
}

// A refused approval says the service's reason: a full quota asks the
// owner to forget a lesson, not to look at this one again (Codex review of
// #18, round 4).
func TestARefusedApprovalSaysWhy(t *testing.T) {
	h := newHarness(t)
	inForce := lesson(1, "vera", "a", 0)
	inForce.State = proto.LessonActive
	h.srv.AddLesson(inForce)
	h.srv.AddLesson(lesson(2, "vera", "b", time.Minute))
	h.srv.SetLessonRoom(1)
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	shown, _ := h.store.Data.Lesson(2)
	h.store.DecideLesson(shown, proto.LessonApprove)
	h.loop.RunUntil(t, func() bool { return h.store.DecidingLesson(2).Err != "" && !h.store.DecidingLesson(2).Busy })
	if got := h.store.DecidingLesson(2).Err; !strings.Contains(got, "full") || strings.Contains(got, "changed") {
		t.Fatalf("a full quota says %q", got)
	}
}

// A fetch begun after a decision that read the lesson before the decision
// committed, and answered first, keeps its copy; the reply that disagrees
// asks for a fetch again, which settles it (Codex review of #18, round 4).
func TestADecisionReplyThatDisagreesWithAnEarlyReadFetchesAgain(t *testing.T) {
	var d Data
	d.SetLessons(d.BeginLessons(), []proto.Lesson{lesson(1, "vera", "a", 0)})
	decision := d.BeginLessonDecision()
	early := d.BeginLessons()
	d.SetLessons(early, []proto.Lesson{lesson(1, "vera", "a", 0)}) // read before the commit
	approved := lesson(1, "vera", "a", 0)
	approved.State = proto.LessonActive
	if !d.LessonDecided(decision, approved) {
		t.Fatal("a reply that disagrees with an earlier read asked for no fetch")
	}
	// One that agrees asks for nothing.
	decision = d.BeginLessonDecision()
	d.SetLessons(d.BeginLessons(), []proto.Lesson{approved})
	if d.LessonDecided(decision, approved) {
		t.Fatal("a reply the fetch agrees with asked for a fetch")
	}
	// An event since is newer: no fetch.
	decision = d.BeginLessonDecision()
	d.SetLessons(d.BeginLessons(), []proto.Lesson{approved})
	again := approved
	again.Change = &proto.LessonChange{Text: "c"}
	d.Apply(proto.Event{Type: proto.EventLesson, Lesson: &again})
	if d.LessonDecided(decision, approved) {
		t.Fatal("a reply older than an event asked for a fetch")
	}
}

// The store fetches the lessons again when a decision's reply disagrees
// with a fetch that answered first, so the Inbox never keeps a card the
// owner decided (Codex review of #18, round 4).
func TestTheStoreFetchesAgainWhenAReplyDisagrees(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLesson(lesson(1, "vera", "a", 0))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	// The stream is down: the decision's event does not come.
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return h.store.Conn.State != Connected })
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathLesson, 1))
	shown, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(shown, proto.LessonApprove)
	// A fetch begun after the decision, that read the lesson before the
	// decision committed, lands first.
	h.store.Data.SetLessons(h.store.Data.BeginLessons(), []proto.Lesson{lesson(1, "vera", "a", 0)})
	gets := len(h.srv.RequestsTo(http.MethodGet, proto.PathLessons))
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.DecidingLesson(1).Busy })
	h.settle(t)
	if len(h.srv.RequestsTo(http.MethodGet, proto.PathLessons)) == gets {
		t.Fatal("no fetch followed a reply that disagreed")
	}
	if _, waits := h.store.Data.Lesson(1); waits {
		t.Fatal("the decided lesson still waits")
	}
}

// A fetch begun while a decision was on its way, answering after its reply,
// may have read the lesson before the decision committed: it does not
// undo the reply, and the store fetches again (Codex review of #18, round
// 5).
func TestAStaleFetchAfterADecisionReplyFetchesAgain(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLesson(lesson(1, "vera", "a", 0))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Lessons()) == 1 })
	h.srv.DropStreams()
	h.loop.RunUntil(t, func() bool { return h.store.Conn.State != Connected })
	releasePost := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathLesson, 1))
	shown, _ := h.store.Data.Lesson(1)
	h.store.DecideLesson(shown, proto.LessonApprove)
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodPost, proto.Path(proto.PathLesson, 1)) })
	// A refresh begun now reads the lesson as before the decision.
	approved := lesson(1, "vera", "a", 0)
	approved.State = proto.LessonActive
	h.srv.AddLesson(lesson(1, "vera", "a", 0))
	releaseGet := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathLessons)
	h.store.loadLessons()
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathLessons) })
	h.srv.AddLesson(approved)
	releasePost()
	h.loop.RunUntil(t, func() bool { return !h.store.DecidingLesson(1).Busy })
	releaseGet()
	h.settle(t)
	if _, waits := h.store.Data.Lesson(1); waits {
		t.Fatal("a stale fetch put the approved lesson back in the Inbox")
	}
}
