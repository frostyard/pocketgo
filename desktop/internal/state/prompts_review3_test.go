package state

import (
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A list that brings the first prompt for a name the last frame showed
// none for never puts it in under Enter or Tab: the next frame shows it
// highlighted first (Codex review of #108, round 3).
func TestAHighlightNotYetShownTakesNothing(t *testing.T) {
	var p PromptPicker
	if m := p.View("/rev", proto.Prompts{}, ListLoaded, ""); !m.Open || m.Highlight != -1 {
		t.Fatalf("shown %+v", m)
	}
	for _, key := range []PickerKey{PickerEnter, PickerTab} {
		if r := p.Key(key, "/rev", savedPrompts); !r.Took || r.Insert {
			t.Fatalf("%d on a prompt not shown yet: %+v", key, r)
		}
	}
	if m := p.View("/rev", savedPrompts, ListLoaded, ""); m.Items[m.Highlight].Name != "review-pr" {
		t.Fatalf("the next frame %+v", m)
	}
	if r := p.Key(PickerEnter, "/rev", savedPrompts); !r.Insert {
		t.Fatalf("enter on the one shown: %+v", r)
	}
	// Up and Down show where they go, and Enter takes it.
	var q PromptPicker
	q.View("/", proto.Prompts{}, ListLoaded, "")
	q.Key(PickerDown, "/", savedPrompts)
	if r := q.Key(PickerEnter, "/", savedPrompts); !r.Insert || r.Text != "Review pull request  and fix what you find." {
		t.Fatalf("enter after down: %+v", r)
	}
}

// Keys pressed while a prompt waits to go in edit the prompt's text at
// its caret, never the call the field still shows (Codex review of #108,
// round 3).
func TestKeysEditThePromptOnItsWay(t *testing.T) {
	e := PromptEdit{Text: "Ship it.\nNow.", Caret: 8}
	e.Key(EditBackspace)
	if e.Text != "Ship it\nNow." || e.Caret != 7 {
		t.Fatalf("backspace %+v", e)
	}
	e.Key(EditDelete)
	if e.Text != "Ship itNow." || e.Caret != 7 {
		t.Fatalf("delete %+v", e)
	}
	e.Key(EditLeft)
	e.Typed("!")
	if e.Text != "Ship i!tNow." || e.Caret != 7 {
		t.Fatalf("left and a character %+v", e)
	}
	e.Key(EditRight)
	e.Key(EditRight)
	e.Typed(" ")
	if e.Text != "Ship i!tN ow." || e.Caret != 10 {
		t.Fatalf("right and a space %+v", e)
	}
	ends := PromptEdit{Text: "é", Caret: 0}
	ends.Key(EditBackspace)
	ends.Key(EditLeft)
	if ends.Text != "é" || ends.Caret != 0 {
		t.Fatalf("at the start %+v", ends)
	}
	ends.Caret = 1
	ends.Key(EditDelete)
	ends.Key(EditRight)
	if ends.Text != "é" || ends.Caret != 1 {
		t.Fatalf("at the end %+v", ends)
	}
	ends.Key(EditBackspace)
	if ends.Text != "" || ends.Caret != 0 {
		t.Fatalf("a character of two bytes %+v", ends)
	}
}
