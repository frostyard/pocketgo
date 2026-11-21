package state

import (
	"net/http"
	"slices"
	"testing"

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
	if got := chip.Levels; !slices.Equal(got, []LevelItem{{"off", false}, {"low", false}, {"high", true}}) {
		t.Errorf("levels %+v", got)
	}
	if chip := ChipOf(proto.Session{Model: sol.ModelRef}, models, ""); chip.Label != "gpt-6.1-sol" ||
		!slices.Equal(chip.Levels, []LevelItem{{"off", true}, {"medium", false}}) {
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
