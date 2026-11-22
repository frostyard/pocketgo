package state

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A prompt whose text with its arguments would pass what a message takes
// never goes in, counted before it is built: a $ARGUMENTS repeated over a
// long paste leaves the draft as it is, and the picker says why (Codex
// review of #108, round 1).
func TestAPromptPastAMessageStaysOut(t *testing.T) {
	echo := proto.Prompt{Name: "echo", Text: strings.Repeat("$ARGUMENTS ", 1000)}
	prompts := proto.Prompts{Prompts: []proto.Prompt{echo}}
	draft := "/echo " + strings.Repeat("x", 300)
	if got, _, err := InsertPrompt(draft, echo); err == nil || got != draft || !strings.Contains(err.Error(), "over the 262144 a message takes") {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	// Within the bound it goes in.
	fits := "/echo " + strings.Repeat("x", proto.MaxMessageSize/1000-1)
	if got, _, err := InsertPrompt(fits, echo); err != nil || len(got) > proto.MaxMessageSize {
		t.Fatalf("within the bound: %d bytes, %v", len(got), err)
	}
	// A draft whose lines below the call pass it keeps them, as it was.
	below := "/echo\n" + strings.Repeat("y", proto.MaxMessageSize)
	if got, _, err := InsertPrompt(below, echo); err == nil || got != below {
		t.Fatalf("the lines below: %d bytes, %v", len(got), err)
	}
	var p PromptPicker
	p.View(draft, prompts, ListLoaded, "")
	if r := p.Key(PickerEnter, draft, prompts); !r.Took || r.Insert {
		t.Fatalf("enter: %+v", r)
	}
	m := p.View(draft, prompts, ListLoaded, "")
	if note := PromptNote(m); !m.Open || !strings.HasPrefix(note, "That prompt did not go in: /echo with these words would be ") {
		t.Fatalf("the picker after the refusal: %q %v", note, m.Open)
	}
	if _, _, ok := p.Choose("echo", draft, prompts); ok || p.Refused(draft) == "" {
		t.Fatal("a click put a prompt past a message in")
	}
	// The draft changed, the refusal goes.
	if m := p.View(draft+"x", prompts, ListLoaded, ""); m.Refused != "" || p.Refused(draft) != "" {
		t.Fatalf("after a change: %q", m.Refused)
	}
}

// The prompt shown highlighted is the one Enter takes: a list that comes
// with a prompt before it, under the same name typed, keeps it; a new name
// typed highlights its best match (Codex review of #108, round 1).
func TestTheHighlightShownIsTheOneEnterTakes(t *testing.T) {
	var p PromptPicker
	if m := p.View("/re", savedPrompts, ListLoaded, ""); m.Items[m.Highlight].Name != "review-pr" {
		t.Fatalf("shown %+v", m)
	}
	more := proto.Prompts{Prompts: append([]proto.Prompt{{Name: "refactor", Text: "Refactor it."}}, savedPrompts.Prompts...)}
	if m := p.View("/re", more, ListLoaded, ""); m.Items[m.Highlight].Name != "review-pr" {
		t.Fatalf("after the list changed: %q at %d", names(m.Items), m.Highlight)
	}
	if r := p.Key(PickerEnter, "/re", more); !r.Insert || !strings.HasPrefix(r.Text, "Review pull request") {
		t.Fatalf("enter took %+v", r)
	}
	var q PromptPicker
	q.View("/", savedPrompts, ListLoaded, "")
	q.Key(PickerDown, "/", savedPrompts)
	if m := q.View("/s", savedPrompts, ListLoaded, ""); m.Items[m.Highlight].Name != "ship-it" {
		t.Fatalf("a new name typed: %+v", m)
	}
	// A key typed ahead of the frame takes the best match of its text.
	q.View("/", savedPrompts, ListLoaded, "")
	if r := q.Key(PickerEnter, "/sh", savedPrompts); !r.Insert || r.Text != "Ship it." {
		t.Fatalf("enter ahead of the frame: %+v", r)
	}
}

// The Enter that put a prompt in takes its own repeats until it is let go,
// so holding Enter never sends the prompt; a new press after it is the
// field's (Codex review of #108, round 1).
func TestAnEnterThatPutAPromptInHoldsUntilLetGo(t *testing.T) {
	var p PromptPicker
	p.View("/ship", savedPrompts, ListLoaded, "")
	r := p.Key(PickerEnter, "/ship", savedPrompts)
	if !r.Insert {
		t.Fatalf("enter: %+v", r)
	}
	for range 3 {
		if again := p.Key(PickerEnter, r.Text, savedPrompts); !again.Took || again.Insert {
			t.Fatalf("a repeat: %+v", again)
		}
	}
	p.EnterReleased()
	if again := p.Key(PickerEnter, r.Text, savedPrompts); again.Took {
		t.Fatalf("a new press: %+v", again)
	}
	// Tab holds nothing.
	var q PromptPicker
	q.View("/ship", savedPrompts, ListLoaded, "")
	if r := q.Key(PickerTab, "/ship", savedPrompts); !r.Insert {
		t.Fatalf("tab: %+v", r)
	}
	if again := q.Key(PickerEnter, "Ship it.", savedPrompts); again.Took {
		t.Fatalf("enter after tab: %+v", again)
	}
}

// A picker whose composer left the window says it opened as it shows
// again, so the composer asks pocketd again; a close by Escape stays
// (Codex review of #108, round 1).
func TestAPickerShownAgainOpensAgain(t *testing.T) {
	var p PromptPicker
	if m := p.View("/", savedPrompts, ListLoaded, ""); !m.Opened {
		t.Fatalf("first %+v", m)
	}
	if m := p.View("/", savedPrompts, ListLoaded, ""); m.Opened {
		t.Fatalf("again %+v", m)
	}
	p.Hide()
	if m := p.View("/", savedPrompts, ListLoaded, ""); !m.Open || !m.Opened {
		t.Fatalf("after Hide %+v", m)
	}
	p.Key(PickerEscape, "/", savedPrompts)
	p.Hide()
	if m := p.View("/", savedPrompts, ListLoaded, ""); m.Open {
		t.Fatalf("closed, hidden and shown %+v", m)
	}
}
