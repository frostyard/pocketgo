package state

import (
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A call pasted with a no-break space, or any white space TrimSpace
// trims, after its name is a call: the picker opens on it, and Enter never
// sends it as typed (Codex review of #108, round 2).
func TestACallWithAnyWhiteSpaceAfterItsNameIsACall(t *testing.T) {
	for _, draft := range []string{"/review-pr 42", "/review-pr  42", "/review-pr　 42 "} {
		if got, ok := ParseSlashHead(draft); !ok || got != (SlashHead{Name: "review-pr", Args: "42"}) {
			t.Errorf("%q: %+v %v", draft, got, ok)
		}
	}
	for _, draft := range []string{"/review-pr­42", "/review-pr·42"} {
		if got, ok := ParseSlashHead(draft); ok {
			t.Errorf("%q is a call: %+v", draft, got)
		}
	}
	var p PromptPicker
	p.View("/review-pr 42", savedPrompts, ListLoaded, "")
	if r := p.Key(PickerEnter, "/review-pr 42", savedPrompts); !r.Insert || r.Text != "Review pull request 42 and fix what you find." {
		t.Fatalf("enter: %+v", r)
	}
}

// The prompt the frame showed highlighted that a newer list no longer
// offers is never swapped for another under Enter or Tab: they take
// nothing until the next frame shows what takes its place (Codex review
// of #108, round 2).
func TestAHighlightThatWentTakesNothing(t *testing.T) {
	var p PromptPicker
	if m := p.View("/", savedPrompts, ListLoaded, ""); m.Items[m.Highlight].Name != "fix-pr" {
		t.Fatalf("shown %+v", m)
	}
	without := proto.Prompts{Prompts: []proto.Prompt{savedPrompts.Prompts[0], savedPrompts.Prompts[2], savedPrompts.Prompts[3]}}
	for _, key := range []PickerKey{PickerEnter, PickerTab} {
		if r := p.Key(key, "/", without); !r.Took || r.Insert {
			t.Fatalf("%d on a highlight that went: %+v", key, r)
		}
	}
	if m := p.View("/", without, ListLoaded, ""); m.Items[m.Highlight].Name != "review-pr" {
		t.Fatalf("the next frame %+v", m)
	}
	if r := p.Key(PickerEnter, "/", without); !r.Insert || r.Text != "Review pull request  and fix what you find." {
		t.Fatalf("enter on the one shown: %+v", r)
	}
	// A prompt that went invalid went too.
	var q PromptPicker
	q.View("/ship", savedPrompts, ListLoaded, "")
	broken := proto.Prompts{Prompts: []proto.Prompt{{Name: "ship-it", Problem: "its text holds the control character U+0000"}}}
	if r := q.Key(PickerEnter, "/ship", broken); !r.Took || r.Insert {
		t.Fatalf("enter on a highlight gone invalid: %+v", r)
	}
	if r := q.Key(PickerTab, "/ship", broken); !r.Took || r.Insert {
		t.Fatalf("tab on a highlight gone invalid: %+v", r)
	}
}

// A press of Enter of its own ends a hold as it comes, whether or not a
// frame saw the key let go; an auto-repeat keeps it; a row chosen while
// Enter is down holds it, as the field's Enter does (Codex review of #108,
// round 2).
func TestAFreshPressOfEnterEndsTheHold(t *testing.T) {
	var p PromptPicker
	p.View("/ship", savedPrompts, ListLoaded, "")
	r := p.Key(PickerEnter, "/ship", savedPrompts)
	p.EnterPressed(true)
	if again := p.Key(PickerEnter, r.Text, savedPrompts); !again.Took {
		t.Fatalf("a repeat was the field's: %+v", again)
	}
	p.EnterPressed(false)
	if again := p.Key(PickerEnter, r.Text, savedPrompts); again.Took {
		t.Fatalf("a press of its own was held: %+v", again)
	}
	var q PromptPicker
	q.HoldEnter()
	q.EnterPressed(true)
	if again := q.Key(PickerEnter, "Ship it.", savedPrompts); !again.Took {
		t.Fatalf("a row's Enter repeated into the field: %+v", again)
	}
	q.EnterReleased()
	if again := q.Key(PickerEnter, "Ship it.", savedPrompts); again.Took {
		t.Fatalf("after the release: %+v", again)
	}
}

// Text typed into the field before the prompt went in follows the prompt,
// at its caret, so the field's text, replaced by the prompt's, keeps it
// (Codex review of #108, round 2).
func TestTextTypedBeforeThePromptWentInFollowsIt(t *testing.T) {
	e := PromptEdit{Text: "Ship it.\nThe CI is red.", Caret: 8}
	e.Typed(" Now")
	e.Typed("…")
	if e.Text != "Ship it. Now…\nThe CI is red." || e.Caret != 13 {
		t.Fatalf("%+v", e)
	}
	past := PromptEdit{Text: "Ship it.", Caret: 99}
	past.Typed("!")
	if past.Text != "Ship it.!" || past.Caret != 9 {
		t.Fatalf("a caret past the end: %+v", past)
	}
}
