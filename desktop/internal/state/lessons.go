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

// lessons are the Data's waiting lessons and their merge state.
type lessons struct {
	waiting []proto.Lesson // oldest first
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
func (d *Data) Lessons() []proto.Lesson { return d.lessons.waiting }

// Lesson returns the waiting lesson with id.
func (d *Data) Lesson(id int64) (proto.Lesson, bool) {
	for _, l := range d.lessons.waiting {
		if l.ID == id {
			return l, true
		}
	}
	return proto.Lesson{}, false
}

// SetLessons takes the answer of GET /v1/lessons, begun as f: of every
// lesson not gone, it keeps those that wait. A lesson an event changed
// since f began follows the event.
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
			cur, ok := d.Lesson(l.ID)
			if !ok {
				continue // decided since
			}
			l = cur
		}
		if l.Waits() {
			out = append(out, l)
		}
	}
	for _, l := range d.lessons.waiting {
		if !seen[l.ID] && newer(d.lessons.touched[l.ID], f) {
			out = append(out, l)
		}
	}
	d.lessons.waiting = out
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
// lesson's event would, unless an event about the lesson came since f
// began: a new change the agent proposed meanwhile stays in the list.
func (d *Data) LessonDecided(f Fetch, l proto.Lesson) {
	defer d.lessonAnswered()
	if newer(d.lessons.touched[l.ID], f) {
		return
	}
	d.applyLesson(l)
}

// LessonDecisionFailed ends a decision begun as f that got no reply.
func (d *Data) LessonDecisionFailed(Fetch) { d.lessonAnswered() }

// LessonSeen is what the owner sees waiting on l, as one string: a
// decision's request id, and a card's identity, follow it, so a decision
// on another proposal of the same lesson is a new one.
func LessonSeen(l proto.Lesson) string {
	text, forget := SeenText(l)
	if forget {
		return "forget"
	}
	return "text\x00" + text
}

// applyLesson follows a lesson: one that waits joins the list or takes its
// place, one that no longer waits leaves it.
func (d *Data) applyLesson(l proto.Lesson) {
	d.rev++
	if d.lessons.inFlight > 0 {
		if d.lessons.touched == nil {
			d.lessons.touched = map[int64]uint64{}
		}
		d.lessons.touched[l.ID] = d.touched()
	}
	i := slices.IndexFunc(d.lessons.waiting, func(x proto.Lesson) bool { return x.ID == l.ID })
	switch {
	case l.Waits() && i >= 0:
		d.lessons.waiting[i] = l
	case l.Waits():
		d.lessons.waiting = append(d.lessons.waiting, l)
		d.sortLessons()
	case i >= 0:
		d.lessons.waiting = slices.Delete(d.lessons.waiting, i, i+1)
	}
}

// sortLessons orders the list oldest first, as GET /v1/lessons does.
func (d *Data) sortLessons() {
	sort.SliceStable(d.lessons.waiting, func(i, j int) bool {
		a, b := d.lessons.waiting[i], d.lessons.waiting[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}
