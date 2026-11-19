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
	if !strings.Contains(h.store.DecidingLesson(2).Err, "changed since you saw it") {
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
