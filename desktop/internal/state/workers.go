package state

import (
	"sort"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
)

// WorkerCardsShown is how many worker cards show before "+N more".
const WorkerCardsShown = 3

// WorkerCard is one card of a worker above its starter's composer.
type WorkerCard struct {
	ID    int64
	Title string             // SessionTitle of the worker's session
	State proto.SessionState // Data.StateOf of the worker's session
	Model proto.ModelRef     // the worker's model, as pocketd sends it
}

// WorkerCards are the cards a session's composer shows.
type WorkerCards struct {
	Cards []WorkerCard // the cards shown, in order
	More  int          // how many more are hidden behind "+N more"; 0 when none are
	Fewer bool         // true when expanded and there are more than WorkerCardsShown: offer "Show fewer"
}

// WorkersOf lists a session's workers, active first, then newest ID first.
// Idle workers stay: a thread has one worker (ADR-0021), which its specialist
// directs again after it goes idle, so an idle worker is not finished for good.
// The cap with "+N more" bounds the space.
func (d *Data) WorkersOf(parent int64) []WorkerCard {
	p, held := d.Session(parent)
	if parent == 0 || !held || IsWorkerRoute(p.Route) {
		return nil
	}
	var cards []WorkerCard
	for _, s := range d.Sessions() {
		if IsWorkerRoute(s.Route) && s.StartedBy == parent {
			cards = append(cards, WorkerCard{ID: s.ID, Title: SessionTitle(s), State: d.StateOf(s.ID), Model: s.Model})
		}
	}
	active := func(s proto.SessionState) bool {
		return s == proto.SessionWaiting || s == proto.SessionRunning
	}
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := active(cards[i].State), active(cards[j].State)
		if a != b {
			return a
		}
		return cards[i].ID > cards[j].ID
	})
	return cards
}

// WorkerCards gives the shown cards and the controls for the rest.
func (d *Data) WorkerCards(parent int64, expanded bool) WorkerCards {
	all := d.WorkersOf(parent)
	if len(all) <= WorkerCardsShown {
		return WorkerCards{Cards: all}
	}
	if !expanded {
		return WorkerCards{Cards: all[:WorkerCardsShown], More: len(all) - WorkerCardsShown}
	}
	return WorkerCards{Cards: all, Fewer: true}
}

// ParentOf gives a held worker's starter, when held and no worker itself.
func (d *Data) ParentOf(id int64) (proto.Session, bool) {
	s, held := d.Session(id)
	if !held || !IsWorkerRoute(s.Route) || s.StartedBy == 0 {
		return proto.Session{}, false
	}
	p, held := d.Session(s.StartedBy)
	if !held || IsWorkerRoute(p.Route) {
		return proto.Session{}, false
	}
	return p, true
}

// MoreWorkersLabel names the control that shows the hidden workers.
func MoreWorkersLabel(n int) string { return "+" + strconv.Itoa(n) + " more" }

// WorkerCardLabel names a worker card for a screen reader.
func WorkerCardLabel(c WorkerCard) string {
	label := "Open worker " + c.Title + ", " + string(c.State)
	if name := ModelName(c.Model); name != "" {
		label += ", " + name
	}
	return label
}

// ModelName is the short name the UI shows for a model, as the compact top
// bar does; its tooltip is m.String().
func ModelName(m proto.ModelRef) string { return m.ID }

// WorkerHeader is the line over a worker session's composer: the way back
// to its specialist, and the worker's own title and model, since a
// worker's session has no composer whose model chip would name it.
type WorkerHeader struct {
	Parent      int64          // the specialist's session, 0 when the Data holds none to go back to (ParentOf)
	ParentTitle string         // SessionTitle of the parent, "" when Parent is 0
	Title       string         // SessionTitle of the worker
	Model       proto.ModelRef // the worker's model
}

// WorkerHeaderOf gives a held worker's title and model, with its parent when held.
func (d *Data) WorkerHeaderOf(id int64) (WorkerHeader, bool) {
	s, held := d.Session(id)
	if id == 0 || !held || !IsWorkerRoute(s.Route) {
		return WorkerHeader{}, false
	}
	h := WorkerHeader{Title: SessionTitle(s), Model: s.Model}
	if parent, ok := d.ParentOf(id); ok {
		h.Parent, h.ParentTitle = parent.ID, SessionTitle(parent)
	}
	return h, true
}
