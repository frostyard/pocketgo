package state

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's prompts as pocketd lists them, by name: three valid ones and
// a file with a problem.
var savedPrompts = proto.Prompts{Prompts: []proto.Prompt{
	{Name: "broken", Problem: `its front matter has the field "model"`},
	{Name: "fix-pr", Description: "Fix a pull request's review", ArgumentHint: "<number>", Text: "Fix what the review of $ARGUMENTS found."},
	{Name: "review-pr", Description: "Review a pull request", ArgumentHint: "<number>", Text: "Review pull request $ARGUMENTS and fix what you find."},
	{Name: "ship-it", Description: "Open the pull request", Text: "Ship it."},
}}

func names(list []proto.Prompt) []string {
	out := []string{}
	for _, p := range list {
		out = append(out, p.Name)
	}
	return out
}

// A call of a prompt is / at the very start of the draft, the name typed so
// far, then white space before its arguments: a path or a / after other
// text is none (ADR-0051).
func TestParseSlashHead(t *testing.T) {
	for draft, want := range map[string]SlashHead{
		"/":                         {},
		"/rev":                      {Name: "rev"},
		"/review-pr 42":             {Name: "review-pr", Args: "42"},
		"/review-pr   42 and 43  ":  {Name: "review-pr", Args: "42 and 43"},
		"/Review-PR\t42":            {Name: "Review-PR", Args: "42"},
		"/review-pr 42\nmore lines": {Name: "review-pr", Args: "42"},
		"/\nbelow":                  {},
		"/ 42":                      {Args: "42"},
	} {
		if got, ok := ParseSlashHead(draft); !ok || got != want {
			t.Errorf("%q: %+v %v, want %+v", draft, got, ok, want)
		}
	}
	for _, draft := range []string{"", "review", " /review", "a /review", "/etc/hosts", "//", "/review!", "/review_pr", "/rév", "\n/review"} {
		if got, ok := ParseSlashHead(draft); ok {
			t.Errorf("%q is a call: %+v", draft, got)
		}
	}
}

// The picker offers the prompts whose name starts with what is typed
// first, then those that hold it elsewhere, each by name, in any case; a
// file with a problem is apart, never offered (ADR-0051).
func TestPromptMatches(t *testing.T) {
	for typed, want := range map[string][]string{
		"":     {"fix-pr", "review-pr", "ship-it"},
		"r":    {"review-pr", "fix-pr"},
		"pr":   {"fix-pr", "review-pr"},
		"REV":  {"review-pr"},
		"ship": {"ship-it"},
		"zz":   {},
	} {
		usable, _ := PromptMatches(savedPrompts.Prompts, typed)
		if got := names(usable); !slices.Equal(got, want) {
			t.Errorf("%q: %q, want %q", typed, got, want)
		}
	}
	if _, broken := PromptMatches(savedPrompts.Prompts, "bro"); !slices.Equal(names(broken), []string{"broken"}) {
		t.Errorf("broken %q", names(broken))
	}
	if usable, broken := PromptList("hello", savedPrompts.Prompts); len(usable) != 3 || len(broken) != 1 {
		t.Errorf("a draft with no call offers every prompt: %q %q", names(usable), names(broken))
	}
	if usable, _ := PromptList("/ship now", savedPrompts.Prompts); !slices.Equal(names(usable), []string{"ship-it"}) {
		t.Errorf("a call offers its matches: %q", names(usable))
	}
}

// $ARGUMENTS takes what followed the name, every time it shows; a text
// without it keeps the arguments below it, so no word typed is lost
// (ADR-0051).
func TestExpandPrompt(t *testing.T) {
	for _, tc := range []struct{ text, args, want string }{
		{"Review $ARGUMENTS.", "42", "Review 42."},
		{"Review $ARGUMENTS, then merge $ARGUMENTS.", "42", "Review 42, then merge 42."},
		{"Review $ARGUMENTS.", "", "Review ."},
		{"Ship it.", "", "Ship it."},
		{"Ship it.", "after lunch", "Ship it.\n\nafter lunch"},
	} {
		if got := ExpandPrompt(tc.text, tc.args); got != tc.want {
			t.Errorf("%q with %q: %q, want %q", tc.text, tc.args, got, tc.want)
		}
	}
}

// The prompt takes the place of the call's line, with its arguments, and
// the lines below stay; a draft with no call stays below the prompt. The
// caret goes after the prompt's text, in characters (ADR-0051).
func TestInsertPrompt(t *testing.T) {
	review := savedPrompts.Prompts[2]
	for _, tc := range []struct {
		draft, want string
		caret       int
	}{
		{"/review-pr 42", "Review pull request 42 and fix what you find.", 45},
		{"/rev 42\nThe CI is red.", "Review pull request 42 and fix what you find.\nThe CI is red.", 45},
		{"/", "Review pull request  and fix what you find.", 43},
		{"", "Review pull request  and fix what you find.", 43},
		{"The CI is red.", "Review pull request  and fix what you find.\n\nThe CI is red.", 43},
		{"/review-pr «ünïcode»", "Review pull request «ünïcode» and fix what you find.", 52},
	} {
		if got, caret, err := InsertPrompt(tc.draft, review); err != nil || got != tc.want || caret != tc.caret {
			t.Errorf("%q: %q at %d, want %q at %d", tc.draft, got, caret, tc.want, tc.caret)
		}
	}
}

// The picker opens with / at the start of the message and says so once,
// so the composer asks pocketd again; it filters as the name is typed,
// and a draft with no call closes it (ADR-0051).
func TestThePickerOpensWithASlashAtTheStart(t *testing.T) {
	var p PromptPicker
	if m := p.View("Fix the CI", savedPrompts, ListLoaded, ""); m.Open || m.Opened {
		t.Fatalf("no call: %+v", m)
	}
	m := p.View("/", savedPrompts, ListLoaded, "")
	if !m.Open || !m.Opened || !slices.Equal(names(m.Items), []string{"fix-pr", "review-pr", "ship-it"}) || m.Highlight != 0 ||
		!slices.Equal(names(m.Broken), []string{"broken"}) {
		t.Fatalf("/: %+v", m)
	}
	if m := p.View("/re", savedPrompts, ListLoaded, ""); !m.Open || m.Opened || !slices.Equal(names(m.Items), []string{"review-pr"}) {
		t.Fatalf("/re: %+v", m)
	}
	if m := p.View("/re/", savedPrompts, ListLoaded, ""); m.Open {
		t.Fatalf("a path: %+v", m)
	}
	if m := p.View("/r", savedPrompts, ListLoaded, ""); !m.Open || !m.Opened {
		t.Fatalf("opened again: %+v", m)
	}
	// It says where the list stands while it has none to offer.
	var q PromptPicker
	if m := q.View("/", proto.Prompts{}, ListFailed, "bad gateway"); !m.Open || m.Status != ListFailed || m.Err != "bad gateway" || m.Highlight != -1 {
		t.Fatalf("a failed list: %+v", m)
	}
	if m := q.View("/", proto.Prompts{Problem: "the prompts folder is a symbolic link"}, ListLoaded, ""); m.Problem != "the prompts folder is a symbolic link" {
		t.Fatalf("the folder's problem: %+v", m)
	}
}

// Up and Down move the highlight, around at the ends, kept by name as the
// list changes; Enter and Tab put the highlighted prompt in with the call's
// arguments, and nothing is sent (ADR-0051).
func TestThePickersKeysChooseAndInsert(t *testing.T) {
	var p PromptPicker
	p.View("/", savedPrompts, ListLoaded, "")
	if r := p.Key(PickerDown, "/", savedPrompts); !r.Took || r.Insert {
		t.Fatalf("down: %+v", r)
	}
	if m := p.View("/", savedPrompts, ListLoaded, ""); m.Highlight != 1 {
		t.Fatalf("after down: %+v", m)
	}
	p.Key(PickerDown, "/", savedPrompts)
	p.Key(PickerDown, "/", savedPrompts)
	if m := p.View("/", savedPrompts, ListLoaded, ""); m.Highlight != 0 {
		t.Fatalf("down past the end goes around: %+v", m)
	}
	p.Key(PickerUp, "/", savedPrompts)
	if m := p.View("/", savedPrompts, ListLoaded, ""); m.Highlight != 2 || m.Items[2].Name != "ship-it" {
		t.Fatalf("up past the start goes around: %+v", m)
	}
	// A prompt added before it keeps the highlight on ship-it.
	more := proto.Prompts{Prompts: append([]proto.Prompt{{Name: "audit", Text: "Audit."}}, savedPrompts.Prompts...)}
	if m := p.View("/", more, ListLoaded, ""); m.Items[m.Highlight].Name != "ship-it" {
		t.Fatalf("the highlight moved with the list: %+v", m)
	}
	// Enter puts it in, with what followed its name.
	p.Key(PickerUp, "/", savedPrompts)
	r := p.Key(PickerEnter, "/ tonight\nThen tell me.", savedPrompts)
	if !r.Took || !r.Insert || r.Text != "Review pull request tonight and fix what you find.\nThen tell me." || r.Caret != 50 {
		t.Fatalf("enter: %+v", r)
	}
	// It is closed once the prompt is in, and opens with the next call.
	if m := p.View(r.Text, savedPrompts, ListLoaded, ""); m.Open {
		t.Fatalf("after the insert: %+v", m)
	}
	if m := p.View("/sh", savedPrompts, ListLoaded, ""); !m.Open || !m.Opened {
		t.Fatalf("the next call: %+v", m)
	}
	if r := p.Key(PickerTab, "/sh 1", savedPrompts); !r.Took || !r.Insert || r.Text != "Ship it.\n\n1" {
		t.Fatalf("tab: %+v", r)
	}
}

// With nothing to put in, Enter does nothing, so a call typed before its
// prompt shows is never sent, and Tab moves the focus; a picker that does
// not show takes no key (ADR-0051).
func TestThePickerTakesOnlyTheKeysItUses(t *testing.T) {
	var p PromptPicker
	p.View("/zz", savedPrompts, ListLoaded, "")
	for key, took := range map[PickerKey]bool{PickerEnter: true, PickerTab: false, PickerUp: false, PickerDown: false} {
		if r := p.Key(key, "/zz", savedPrompts); r.Took != took || r.Insert {
			t.Errorf("%d with no match: %+v, want took %v", key, r, took)
		}
	}
	if r := p.Key(PickerEnter, "/review-pr 4", proto.Prompts{}); !r.Took || r.Insert {
		t.Errorf("enter before the list came: %+v", r)
	}
	for _, key := range []PickerKey{PickerUp, PickerDown, PickerEnter, PickerTab, PickerEscape} {
		if r := p.Key(key, "Fix the CI", savedPrompts); r.Took {
			t.Errorf("%d without a call: %+v", key, r)
		}
	}
}

// Escape closes the picker and leaves the text as typed; it stays closed
// while the text starts with the call's line as it was then, so typing on
// keeps it closed, and opens again on a text that does not: the name
// erased, or the whole text typed over with a new / (ADR-0051).
func TestEscapeClosesThePicker(t *testing.T) {
	var p PromptPicker
	p.View("/rev", savedPrompts, ListLoaded, "")
	if r := p.Key(PickerEscape, "/rev", savedPrompts); !r.Took || r.Insert {
		t.Fatalf("escape: %+v", r)
	}
	for _, draft := range []string{"/rev", "/review-pr 42", "/rev 42\nbelow"} {
		if m := p.View(draft, savedPrompts, ListLoaded, ""); m.Open {
			t.Fatalf("%q after escape: %+v", draft, m)
		}
		if r := p.Key(PickerEnter, draft, savedPrompts); r.Took {
			t.Fatalf("%q: enter went to the closed picker: %+v", draft, r)
		}
	}
	// The name erased opens it again, as does the text typed over at once.
	for _, draft := range []string{"/re", "/"} {
		var q PromptPicker
		q.View("/rev 42", savedPrompts, ListLoaded, "")
		q.Key(PickerEscape, "/rev 42", savedPrompts)
		if m := q.View(draft, savedPrompts, ListLoaded, ""); !m.Open || !m.Opened {
			t.Fatalf("%q after escape on /rev 42: %+v", draft, m)
		}
	}
	p.View("", savedPrompts, ListLoaded, "")
	if m := p.View("/rev", savedPrompts, ListLoaded, ""); !m.Open || !m.Opened {
		t.Fatalf("the next call: %+v", m)
	}
}

// A click on a row puts its prompt in; a name no longer valid puts nothing
// in. The palette's Insert Prompt opens the picker on a / of its own above
// the draft, or on the draft's own call (ADR-0051).
func TestChooseAndSummon(t *testing.T) {
	var p PromptPicker
	if text, caret, ok := p.Choose("ship-it", "/s now", savedPrompts); !ok || text != "Ship it.\n\nnow" || caret != 13 {
		t.Fatalf("choose: %q %d %v", text, caret, ok)
	}
	for _, name := range []string{"broken", "gone"} {
		if text, _, ok := p.Choose(name, "/b", savedPrompts); ok || text != "/b" {
			t.Errorf("%s: %q %v", name, text, ok)
		}
	}
	for _, tc := range []struct {
		draft, want string
		caret       int
	}{
		{"", "/", 1},
		{"The CI is red.", "/\nThe CI is red.", 1},
		{"/rev 42\nbelow", "/rev 42\nbelow", 7},
	} {
		var q PromptPicker
		q.View(tc.draft, savedPrompts, ListLoaded, "")
		q.Key(PickerEscape, tc.draft, savedPrompts)
		text, caret := q.Summon(tc.draft)
		if text != tc.want || caret != tc.caret {
			t.Errorf("summon %q: %q at %d, want %q at %d", tc.draft, text, caret, tc.want, tc.caret)
		}
		if m := q.View(text, savedPrompts, ListLoaded, ""); !m.Open || !m.Opened {
			t.Errorf("summon %q does not open it: %+v", tc.draft, m)
		}
	}
}

// The picker says why it offers none: the list loads or failed to load,
// only files with a problem match, the owner has no prompt yet, or none
// matches the name typed; and pocketd's problem with the folder whenever
// it has one (ADR-0051).
func TestThePickersNote(t *testing.T) {
	var p PromptPicker
	only := proto.Prompts{Prompts: savedPrompts.Prompts[:1]}
	for _, tc := range []struct {
		draft   string
		prompts proto.Prompts
		status  ListStatus
		err     string
		want    string
	}{
		{"/", proto.Prompts{}, ListLoading, "", "Loading your saved prompts…"},
		{"/", proto.Prompts{}, ListFailed, "bad gateway", "Your saved prompts did not load: bad gateway."},
		{"/", savedPrompts, ListFailed, "bad gateway", ""},
		{"/", savedPrompts, ListLoaded, "", ""},
		{"/zz", savedPrompts, ListLoaded, "", "No saved prompt matches /zz."},
		{"/bro", savedPrompts, ListLoaded, "", "No saved prompt you can use matches /bro: fix the files below."},
		{"/", only, ListLoaded, "", "No saved prompt you can use: fix the files below."},
		{"/", proto.Prompts{}, ListLoaded, "", "You have no saved prompts yet. Add one as prompts/<name>.md in the profile folder."},
		{"/", proto.Prompts{Problem: "the prompts folder is a symbolic link"}, ListLoaded, "", "Your saved prompts: the prompts folder is a symbolic link."},
		{"/", proto.Prompts{Prompts: savedPrompts.Prompts, Problem: "the prompts folder holds 102 prompt files."}, ListLoaded, "",
			"Your saved prompts: the prompts folder holds 102 prompt files."},
	} {
		if got := PromptNote(p.View(tc.draft, tc.prompts, tc.status, tc.err)); got != tc.want {
			t.Errorf("%q, %+v, %v: %q, want %q", tc.draft, tc.prompts, tc.status, got, tc.want)
		}
	}
}

// Only a message takes a saved prompt: an answer to a question is the
// owner's own words, as it takes no file.
func TestOnlyAMessageTakesAPrompt(t *testing.T) {
	if !(Composer{}).TakesPrompts() || (Composer{Question: &proto.Question{ID: "q1"}}).TakesPrompts() {
		t.Fatal("the composer's prompts follow its attachments")
	}
}

// The store loads the prompts as it connects, and each LoadPrompts asks
// again: the newest answer wins, and a failure keeps the prompts an
// earlier answer brought, saying why (ADR-0051).
func TestThePromptsLoadAndAFailureKeepsThem(t *testing.T) {
	h := newHarness(t)
	h.srv.SetPrompts(savedPrompts)
	h.connect(t)
	if status, _ := h.store.Data.PromptsStatus(); status != ListLoaded || len(h.store.Data.Prompts().Prompts) != 4 {
		t.Fatalf("after connecting: %v %+v", status, h.store.Data.Prompts())
	}
	h.srv.SetPrompts(proto.Prompts{Prompts: savedPrompts.Prompts[:1], Problem: "the prompts folder holds 102 prompt files"})
	h.store.LoadPrompts()
	h.settle(t)
	if got := h.store.Data.Prompts(); len(got.Prompts) != 1 || !strings.Contains(got.Problem, "102") {
		t.Fatalf("after a change: %+v", got)
	}
	h.srv.FailOnce(http.MethodGet, proto.PathPrompts, http.StatusBadGateway)
	h.store.LoadPrompts()
	h.settle(t)
	if status, err := h.store.Data.PromptsStatus(); status != ListFailed || err == "" || len(h.store.Data.Prompts().Prompts) != 1 {
		t.Fatalf("after a failure: %v %q %+v", status, err, h.store.Data.Prompts())
	}
	// A stream that comes back fetches again what failed.
	h.store.retryFailed()
	h.settle(t)
	if status, _ := h.store.Data.PromptsStatus(); status != ListLoaded {
		t.Fatalf("after the retry: %v", status)
	}
	// An answer of a fetch begun before one applied is dropped.
	var d Data
	older, newer := d.Begin(), d.Begin()
	d.SetPrompts(newer, savedPrompts)
	d.SetPrompts(older, proto.Prompts{})
	d.SetPromptsError(older, "late")
	if status, _ := d.PromptsStatus(); status != ListLoaded || len(d.Prompts().Prompts) != 4 {
		t.Fatalf("an older answer won: %v %+v", status, d.Prompts())
	}
}
