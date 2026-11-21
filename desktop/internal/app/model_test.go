package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The composer's chip is frostyard/pocket's model selector, whole
// (ADR-0028): it names the session's model and its thinking level, and
// its menu changes them, each provider's models in a submenu, the current
// one checked, and the model's levels under Thinking. A new model keeps a
// level it takes.
func TestTheComposersChipChangesTheModelSnapshot(t *testing.T) {
	x := newTestApp(t)
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/projects/nsl", Model: opus, Thinking: "high",
		State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.connect(t)
	x.a.openSession(1)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.hasAll(t, "Claude Opus 5.5 · high")
	x.snapshot(t, "composer-model")
	x.click(t, "Model")
	if got, want := x.tt.Menu(), []string{"anthropic", "openai-codex", "-", "Thinking"}; !slices.Equal(got, want) {
		t.Fatalf("the chip's menu is %q, want %q", got, want)
	}
	if err := x.tt.ChooseMenuItem("openai-codex", "GPT-5.5"); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return !x.a.st.ChangingModel(1).Busy })
	x.settle()
	x.hasAll(t, "GPT-5.5 · high")
	p := proto.Path(proto.PathModel, 1)
	sent := x.srv.RequestsTo(http.MethodPost, p)
	var first proto.ChangeModel
	if len(sent) != 1 || json.Unmarshal(sent[0].Body, &first) != nil || first.Model == nil ||
		*first.Model != (proto.ModelRef{Provider: "openai-codex", ID: "gpt-5.5"}) || first.Thinking != "" {
		t.Fatalf("sent %+v", sent)
	}
	x.click(t, "Model")
	if err := x.tt.ChooseMenuItem("Thinking", "low"); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return !x.a.st.ChangingModel(1).Busy })
	x.settle()
	x.hasAll(t, "GPT-5.5 · low")
	sent = x.srv.RequestsTo(http.MethodPost, p)
	var second proto.ChangeModel
	if len(sent) != 2 || json.Unmarshal(sent[1].Body, &second) != nil || second.Model != nil || second.Thinking != "low" {
		t.Fatalf("sent %+v", sent)
	}
}

// A change pocketd refused says why above the composer.
func TestAModelChangeThatFailedSaysWhy(t *testing.T) {
	x := newTestApp(t)
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/projects/nsl", Model: opus,
		State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.connect(t)
	x.a.openSession(1)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.srv.FailOnce(http.MethodPost, proto.Path(proto.PathModel, 1), http.StatusBadGateway)
	x.click(t, "Model")
	if err := x.tt.ChooseMenuItem("openai-codex", "GPT-5.5"); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return x.a.st.ChangingModel(1).Err != "" })
	x.settle()
	if !slices.ContainsFunc(x.tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "Changing the model failed: ") }) {
		t.Fatalf("no problem shows: %q", x.tt.Texts())
	}
}

// A model chosen in a menu the models changed under, as when a stream
// came back to a pocketd with another list, goes to the model chosen: its
// item is keyed by the model, not by its place (Codex review of #48,
// round 3).
func TestAModelChosenAsTheListChangesIsTheOneChosen(t *testing.T) {
	x := newTestApp(t)
	opus := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5"}
	haiku := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-haiku-5-5"}, Name: "Claude Haiku 5.5"}
	sonnet := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-sonnet-5-5"}, Name: "Claude Sonnet 5.5"}
	x.srv.SetModels([]proto.Model{opus, haiku})
	x.srv.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/projects/nsl", Model: opus.ModelRef,
		State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.connect(t)
	x.a.openSession(1)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.click(t, "Model")
	// The list changes while the menu is open: Sonnet comes first.
	x.srv.SetModels([]proto.Model{sonnet, opus, haiku})
	x.a.st.LoadModels()
	x.until(t, func() bool { models, _ := x.a.st.Models(); return len(models) == 3 })
	if err := x.tt.ChooseMenuItem("anthropic", "Claude Haiku 5.5"); err != nil {
		t.Fatal(err)
	}
	x.until(t, func() bool { return len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathModel, 1))) == 1 })
	var sent proto.ChangeModel
	if r := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathModel, 1)); json.Unmarshal(r[0].Body, &sent) != nil || sent.Model == nil || *sent.Model != haiku.ModelRef {
		t.Fatalf("sent %+v, want Haiku", sent)
	}
}
