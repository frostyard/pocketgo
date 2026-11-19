package state

import (
	"slices"
	"sort"

	"github.com/frostyard/pocketgo/proto"
)

// The lessons that wait for the owner (ADR-0015), merged from fetches and
// events by the rules of the asks: a lesson an event changed after a fetch
// began follows the event, and the answer of a fetch begun before one
// already applied is dropped.

// lessons are the Data's lessons and their merge state: every lesson not
// gone, oldest first, of which the Inbox's cards are those that wait.
type lessons struct {
	live    []proto.Lesson // oldest first
	touched map[int64]uint64
	// inFlight counts the fetches begun (BeginLessons) and not answered:
	// with none, no stale copy can land, and no marker is kept.
	inFlight int
	list     list
}

// LessonsStatus is where the waiting lessons stand, and why their last
// fetch failed. The Inbox says no lesson waits only when it is ListLoaded.
func (d *Data) LessonsStatus() (ListStatus, string) {
	return d.lessons.list.status, d.lessons.list.err
}

// BeginLessons starts a lessons fetch: Begin, counted, so the merge
// markers live only while a fetch is in flight.
func (d *Data) BeginLessons() Fetch {
	d.lessons.inFlight++
	return d.Begin()
}

// SetLessonsError records that the lessons' fetch f failed, unless a fetch
// begun later already answered.
func (d *Data) SetLessonsError(f Fetch, err string) {
	defer d.lessonAnswered()
	if d.lessons.list.answer(f) {
		d.rev++
		d.lessons.list.status, d.lessons.list.err = ListFailed, err
		d.retireLessonMarks()
	}
}

func (d *Data) lessonAnswered() {
	if d.lessons.inFlight > 0 {
		d.lessons.inFlight--
	}
	if d.lessons.inFlight == 0 {
		d.lessons.touched = nil
	}
}

// retireLessonMarks drops the marker of each lesson an event touched before
// the newest fetch answered: no stale copy it guards against can land any
// more.
func (d *Data) retireLessonMarks() {
	for id, touched := range d.lessons.touched {
		if touched < d.lessons.list.applied {
			delete(d.lessons.touched, id)
		}
	}
}

// Lessons are the lessons that wait for the owner, oldest first: a new
// lesson, or a change to one in force (proto.Lesson.Waits).
func (d *Data) Lessons() []proto.Lesson {
	var out []proto.Lesson
	for _, l := range d.lessons.live {
		if l.Waits() {
			out = append(out, l)
		}
	}
	return out
}

// LessonsInForce are the lessons in force, oldest first, a change waiting
// on them or not: those the owner may forget.
func (d *Data) LessonsInForce() []proto.Lesson {
	var out []proto.Lesson
	for _, l := range d.lessons.live {
		if l.State == proto.LessonActive {
			out = append(out, l)
		}
	}
	return out
}

// Lesson returns the waiting lesson with id.
func (d *Data) Lesson(id int64) (proto.Lesson, bool) {
	if l, ok := d.liveLesson(id); ok && l.Waits() {
		return l, true
	}
	return proto.Lesson{}, false
}

// LiveLesson returns the lesson with id that is not gone.
func (d *Data) LiveLesson(id int64) (proto.Lesson, bool) { return d.liveLesson(id) }

func (d *Data) liveLesson(id int64) (proto.Lesson, bool) {
	for _, l := range d.lessons.live {
		if l.ID == id {
			return l, true
		}
	}
	return proto.Lesson{}, false
}

// SetLessons takes the answer of GET /v1/lessons, begun as f: every lesson
// not gone. A lesson an event changed since f began follows the event.
func (d *Data) SetLessons(f Fetch, list []proto.Lesson) {
	defer d.lessonAnswered()
	if !d.lessons.list.answer(f) {
		return
	}
	d.rev++
	d.lessons.list.status, d.lessons.list.err = ListLoaded, ""
	out := make([]proto.Lesson, 0, len(list))
	seen := map[int64]bool{}
	for _, l := range list {
		if seen[l.ID] {
			continue
		}
		seen[l.ID] = true
		if newer(d.lessons.touched[l.ID], f) {
			cur, ok := d.liveLesson(l.ID)
			if !ok {
				continue // gone since
			}
			l = cur
		}
		if l.State == proto.LessonPending || l.State == proto.LessonActive {
			out = append(out, l)
		}
	}
	for _, l := range d.lessons.live {
		if !seen[l.ID] && newer(d.lessons.touched[l.ID], f) {
			out = append(out, l)
		}
	}
	d.lessons.live = out
	d.sortLessons()
	d.retireLessonMarks()
}

// BeginLessonDecision starts a decision's request (POST /v1/lessons/{id}):
// its reply is the lesson as it was when it was decided, so an event about
// the lesson that came since the request began is newer, as with a fetch.
func (d *Data) BeginLessonDecision() Fetch {
	d.lessons.inFlight++
	return d.Begin()
}

// LessonDecided applies the reply of a decision begun as f, as the
// lesson's event would, unless the client saw the lesson since f began:
// by an event, or by a fetch begun after f that applied (a refresh, or a
// resync's, whose copy may be the first sign of a change the agent
// proposed meanwhile). The reply is never newer than those, and the
// decision's own event follows all the same: a new proposal stays.
func (d *Data) LessonDecided(f Fetch, l proto.Lesson) {
	defer d.lessonAnswered()
	if newer(d.lessons.touched[l.ID], f) || d.lessons.list.applied > f.epoch {
		return
	}
	d.applyLesson(l)
}

// LessonDecisionFailed ends a decision begun as f that got no reply.
func (d *Data) LessonDecisionFailed(Fetch) { d.lessonAnswered() }

// LessonTarget is what decision acts on in l now, as one string, and
// whether it acts on anything: approve and discard on what waits
// (LessonSeen), forget on the text in force. A decision's request id
// follows it, so a decision on another proposal, or another lesson in
// force, is a new request.
func LessonTarget(l proto.Lesson, decision proto.LessonDecision) (string, bool) {
	if decision == proto.LessonForget {
		return "in force\x00" + l.Text, l.State == proto.LessonActive
	}
	return string(decision) + "\x00" + LessonSeen(l), l.Waits()
}

// LessonSeen is what the owner sees waiting on l, as one string: a
// card's identity follows it, so a decision on another proposal of the
// same lesson is a new one.
func LessonSeen(l proto.Lesson) string {
	text, forget := SeenText(l)
	if forget {
		return "forget"
	}
	return "text\x00" + text
}

// applyLesson follows a lesson: one not gone joins the list or takes its
// place, one gone leaves it.
func (d *Data) applyLesson(l proto.Lesson) {
	d.rev++
	if d.lessons.inFlight > 0 {
		if d.lessons.touched == nil {
			d.lessons.touched = map[int64]uint64{}
		}
		d.lessons.touched[l.ID] = d.touched()
	}
	i := slices.IndexFunc(d.lessons.live, func(x proto.Lesson) bool { return x.ID == l.ID })
	live := l.State == proto.LessonPending || l.State == proto.LessonActive
	switch {
	case live && i >= 0:
		d.lessons.live[i] = l
	case live:
		d.lessons.live = append(d.lessons.live, l)
		d.sortLessons()
	case i >= 0:
		d.lessons.live = slices.Delete(d.lessons.live, i, i+1)
	}
}

// sortLessons orders the list oldest first, as GET /v1/lessons does.
func (d *Data) sortLessons() {
	sort.SliceStable(d.lessons.live, func(i, j int) bool {
		a, b := d.lessons.live[i], d.lessons.live[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}
