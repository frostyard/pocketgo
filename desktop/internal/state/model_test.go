package state

import (
	"net/http"
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

var (
	opus  = proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5", Reasoning: true, ThinkingLevels: []string{"off", "low", "high"}}
	haiku = proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-haiku-5-5"}, Name: "Claude Haiku 5.5"}
	sol   = proto.Model{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}, Reasoning: true, ThinkingLevels: []string{"off", "medium"}}
	flash = proto.Model{ModelRef: proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4-flash"}, Name: "DeepSeek Flash", ThinkingLevels: []string{"off"}}
)

// The chip is pocket's: the model's name and a level other than off; a
// submenu per provider, in the order the models list them, the current
// model checked; a Thinking submenu only for a model with more than one
// level, an empty level reading as off.
func TestTheModelChipIsPockets(t *testing.T) {
	models := []proto.Model{opus, sol, haiku, flash}
	chip := ChipOf(proto.Session{Model: opus.ModelRef, Thinking: "high"}, models, "")
	if chip.Label != "Claude Opus 5.5 · high" {
		t.Errorf("label %q", chip.Label)
	}
	var groups []string
	for _, g := range chip.Providers {
		var names []string
		for _, m := range g.Models {
			name := m.Label
			if m.Current {
				name += "*"
			}
			names = append(names, name)
		}
		groups = append(groups, g.Provider+": "+join(names))
	}
	want := []string{"anthropic: Claude Opus 5.5*, Claude Haiku 5.5", "openai-codex: gpt-6.1-sol", "deepseek: DeepSeek Flash"}
	if !slices.Equal(groups, want) {
		t.Errorf("providers %q, want %q", groups, want)
	}
	if got := chip.Levels; !slices.Equal(got, []LevelItem{{"thinking:off", "off", false}, {"thinking:low", "low", false}, {"thinking:high", "high", true}}) {
		t.Errorf("levels %+v", got)
	}
	if chip := ChipOf(proto.Session{Model: sol.ModelRef}, models, ""); chip.Label != "gpt-6.1-sol" ||
		!slices.Equal(chip.Levels, []LevelItem{{"thinking:off", "off", true}, {"thinking:medium", "medium", false}}) {
		t.Errorf("a model with no name at its default level: %+v", chip)
	}
	if chip := ChipOf(proto.Session{Model: flash.ModelRef, Thinking: "off"}, models, ""); chip.Label != "DeepSeek Flash" || chip.Levels != nil {
		t.Errorf("a model with one level: %+v", chip)
	}
	gone := proto.ModelRef{Provider: "anthropic", ID: "claude-gone"}
	if chip := ChipOf(proto.Session{Model: gone, Thinking: "high"}, models, ""); chip.Label != "claude-gone · high" || chip.Levels != nil {
		t.Errorf("a model pocketd no longer runs: %+v", chip)
	}
}

func join(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// A change goes to pocketd, and the session shows it. One that failed
// and is tried again unchanged keeps its request id; once one went, the
// same change again is a new request.
func TestAModelChangeTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModels([]proto.Model{opus, sol})
	s := session(1, "Plan", 0)
	s.Model, s.Thinking = opus.ModelRef, "high"
	h.srv.AddSession(s)
	h.connect(t)
	change := func(model *proto.ModelRef, thinking string) string {
		t.Helper()
		h.store.ChangeModel(1, model, thinking)
		h.loop.RunUntil(t, func() bool { return !h.store.ChangingModel(1).Busy })
		return h.store.ChangingModel(1).Err
	}
	p := proto.Path(proto.PathModel, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	if err := change(&sol.ModelRef, ""); err == "" {
		t.Fatal("the failure was not kept")
	}
	if err := change(&sol.ModelRef, ""); err != "" {
		t.Fatal(err)
	}
	// The answer shows, or the session's event does when one that came
	// after the change began wins over the answer.
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Model == sol.ModelRef })
	if got, _ := h.store.Data.Session(1); got.Thinking != "" || got.Route != "owner" {
		t.Fatalf("the session shows %+v after the change", got)
	}
	if err := change(nil, "medium"); err != "" {
		t.Fatal(err)
	}
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Thinking == "medium" })
	if err := change(&sol.ModelRef, ""); err != "" {
		t.Fatal(err)
	}
	ids := sentIDs(t, h, p)
	if len(ids) != 4 || ids[0] != ids[1] || ids[2] == ids[1] || ids[3] == ids[1] || ids[3] == ids[2] {
		t.Fatalf("request ids %q", ids)
	}
}

// A message waits for the owner's change of its session's model: while
// the change is on its way, none goes, which could start the run on the
// model the session had; once it is in, the message goes (Codex review of
// #48, round 1).
func TestAMessageWaitsForAModelChangeOnItsWay(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModels([]proto.Model{opus, sol})
	s := session(1, "Plan", 0)
	s.Model = opus.ModelRef
	h.srv.AddSession(s)
	h.connect(t)
	release := h.srv.HoldAnswerOnce(http.MethodPost, proto.Path(proto.PathModel, 1))
	h.store.ChangeModel(1, &sol.ModelRef, "")
	if !h.store.ChangingModel(1).Busy || h.store.MaySend(1) {
		t.Fatalf("a change on its way: %+v, may send %v", h.store.ChangingModel(1), h.store.MaySend(1))
	}
	var refused error
	h.store.Send(1, "and then the tests", func(err error) { refused = err })
	if refused == nil {
		t.Fatal("a message went while the model change was on its way")
	}
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.ChangingModel(1).Busy })
	if !h.store.MaySend(1) {
		t.Fatal("no message may go once the change is in")
	}
	sent := false
	h.store.Send(1, "and then the tests", func(err error) { sent = err == nil })
	h.loop.RunUntil(t, func() bool { return sent })
	if got := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1)); len(got) != 1 {
		t.Fatalf("messages sent: %d, want the one after the change", len(got))
	}
}

// Two models of one provider with the same name are told apart by their
// ids in the chip's submenu, and the chip names the current one so too
// (Codex review of #48, round 2).
func TestTheChipTellsModelsOfOneNameApart(t *testing.T) {
	twin := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5-preview"}, Name: "Claude Opus 5.5"}
	chip := ChipOf(proto.Session{Model: twin.ModelRef}, []proto.Model{opus, twin, sol}, "")
	var labels []string
	for _, m := range chip.Providers[0].Models {
		labels = append(labels, m.Label)
	}
	if want := []string{"Claude Opus 5.5", "claude-opus-5-5-preview"}; !slices.Equal(labels, want) || chip.Label != "claude-opus-5-5-preview" {
		t.Fatalf("labels %q and chip %q, want %q and the current one's", labels, chip.Label, want)
	}
}

// The chip offers to load the models again when their list failed to
// load, saying why, or has not loaded yet; not once it loaded (Codex
// review of #48, round 2).
func TestTheChipOffersToLoadTheModelsAgain(t *testing.T) {
	session := proto.Session{Model: opus.ModelRef}
	if chip := ChipOf(session, []proto.Model{opus}, "bad gateway"); !chip.Reload || chip.Problem != "bad gateway" {
		t.Errorf("after a failure: %+v", chip)
	}
	if chip := ChipOf(session, nil, ""); !chip.Reload {
		t.Errorf("before a list: %+v", chip)
	}
	if chip := ChipOf(session, []proto.Model{opus}, ""); chip.Reload || chip.Problem != "" {
		t.Errorf("with a list: %+v", chip)
	}
}

// Refresh, and a stream that comes back, load the models again, also over
// a list that loaded: a pocketd that restarted may run other models, which
// the chip offers (Codex review of #48, round 2).
func TestTheModelsLoadAgainOnRefreshAndReconnect(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModels([]proto.Model{opus})
	h.srv.AddSession(session(1, "Plan", 0))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { models, _ := h.store.Models(); return len(models) == 1 })
	h.srv.SetModels([]proto.Model{opus, sol})
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { models, _ := h.store.Models(); return len(models) == 2 })
	h.srv.SetModels([]proto.Model{sol})
	h.store.retryFailed()
	h.loop.RunUntil(t, func() bool {
		models, _ := h.store.Models()
		return len(models) == 1 && models[0].ModelRef == sol.ModelRef
	})
}

// A change whose answer was lost, and that took effect, keeps no request
// id once the session shows another model: the same change again, after
// another device moved the session on, is a new request, which pocketd
// applies (Codex review of #48, round 2). A change tried again while the
// session shows what it showed keeps its id (TestAModelChangeTriedAgainKeepsItsRequestID).
func TestAPickSupersededSinceIsANewRequest(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModels([]proto.Model{opus, sol})
	s := session(1, "Plan", 0)
	s.Model = opus.ModelRef
	h.srv.AddSession(s)
	h.connect(t)
	p := proto.Path(proto.PathModel, 1)
	h.srv.LoseAnswerOnce(http.MethodPost, p)
	h.store.ChangeModel(1, &sol.ModelRef, "")
	h.loop.RunUntil(t, func() bool { return !h.store.ChangingModel(1).Busy })
	if h.store.ChangingModel(1).Err == "" {
		t.Fatal("the lost answer was not kept")
	}
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Model == sol.ModelRef })
	// Another device moves the session back.
	back, _ := h.store.Data.Session(1)
	back.Model = opus.ModelRef
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &back})
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Model == opus.ModelRef })
	h.store.ChangeModel(1, &sol.ModelRef, "")
	h.loop.RunUntil(t, func() bool { return !h.store.ChangingModel(1).Busy })
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Model == sol.ModelRef })
	if ids := sentIDs(t, h, p); len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("request ids %q, want two", ids)
	}
}

// A fetch of the models begun while another is on its way fetches, and
// the answer of the one begun last wins (Data.SetModels): a stream that
// came back to a restarted pocketd gets the models it runs now, never an
// older answer (Codex review of #48, rounds 3 and 4). A later fetch that
// fails keeps the models an earlier one brought.
func TestTheLastModelsFetchBegunWins(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModels([]proto.Model{opus})
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathModels)
	if err := h.store.Connect(h.srv.URL, pocketdtest.Token); err != nil {
		t.Fatal(err)
	}
	h.loop.RunUntil(t, func() bool { return h.store.Conn.State == Connected && h.store.Hello() != nil })
	// The first fetch's answer, the old list, is made and held back.
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathModels) })
	h.srv.SetModels([]proto.Model{opus, sol})
	h.store.retryFailed()
	h.loop.RunUntil(t, func() bool { models, _ := h.store.Models(); return len(models) == 2 })
	release()
	h.settle(t)
	if models, _ := h.store.Models(); len(models) != 2 {
		t.Fatalf("the older answer won: %+v", models)
	}
}

// A pick of a standing session's own model at its own level changes its
// route alone; a change whose answer was lost keeps no request id once
// the session is seen with another route, so the same pick after a Start
// Over dropped it is a new request, which pins the model again (Codex
// review of #48, round 3).
func TestAPickSeenOnlyByItsRouteRetiresItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModels([]proto.Model{opus, sol})
	s := session(1, "Odrade", 0)
	s.Model, s.Standing, s.Route = opus.ModelRef, true, "profile"
	h.srv.AddSession(s)
	h.connect(t)
	p := proto.Path(proto.PathModel, 1)
	h.srv.LoseAnswerOnce(http.MethodPost, p)
	h.store.ChangeModel(1, &opus.ModelRef, "")
	h.loop.RunUntil(t, func() bool { return !h.store.ChangingModel(1).Busy })
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Route == "owner" })
	// Start Over drops the pick: the route alone goes back.
	back, _ := h.store.Data.Session(1)
	back.Route = "profile"
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &back})
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Route == "profile" })
	h.store.ChangeModel(1, &opus.ModelRef, "")
	h.loop.RunUntil(t, func() bool { return !h.store.ChangingModel(1).Busy })
	h.loop.RunUntil(t, func() bool { got, _ := h.store.Data.Session(1); return got.Route == "owner" })
	if ids := sentIDs(t, h, p); len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("request ids %q, want two", ids)
	}
}

// Each model's menu item is keyed by its provider and id, and each level's
// by the level, never by its place (Codex review of #48, round 3); a name
// that is another model's id there gives way to the model's own id, so no
// two items share a label.
func TestTheChipsItemsAreKeyedAndLabeledApart(t *testing.T) {
	a := proto.Model{ModelRef: proto.ModelRef{Provider: "own", ID: "a"}, Name: "b"}
	b := proto.Model{ModelRef: proto.ModelRef{Provider: "own", ID: "b"}}
	chip := ChipOf(proto.Session{Model: a.ModelRef}, []proto.Model{a, b, opus}, "")
	var got []string
	for _, g := range chip.Providers {
		for _, m := range g.Models {
			got = append(got, m.ID+"="+m.Label)
		}
	}
	want := []string{"model:own/a=a", "model:own/b=b", "model:anthropic/claude-opus-5-5=Claude Opus 5.5"}
	if !slices.Equal(got, want) {
		t.Fatalf("items %q, want %q", got, want)
	}
	if chip := ChipOf(proto.Session{Model: opus.ModelRef}, []proto.Model{opus}, ""); chip.Levels[1].ID != "thinking:low" {
		t.Fatalf("levels %+v", chip.Levels)
	}
}

// The answer of a models fetch applies unless one begun later answered
// already, as every list's does: a first answer that comes while a later
// fetch is on its way applies, and that fetch's failure keeps its models
// and says why (Codex review of #48, round 4).
func TestAModelsAnswerAppliesUnlessALaterOneDid(t *testing.T) {
	var d Data
	first, later := d.Begin(), d.Begin()
	d.SetModels(first, []proto.Model{opus})
	if models, problem := d.Models(); len(models) != 1 || problem != "" {
		t.Fatalf("the first answer: %+v %q", models, problem)
	}
	d.SetModelsError(later, "bad gateway")
	if models, problem := d.Models(); len(models) != 1 || problem != "bad gateway" {
		t.Fatalf("after the later failure: %+v %q, want the first answer's models and why", models, problem)
	}
	d.SetModels(first, []proto.Model{opus, sol})
	if models, _ := d.Models(); len(models) != 1 {
		t.Fatalf("an answer older than one applied applied: %+v", models)
	}
}
