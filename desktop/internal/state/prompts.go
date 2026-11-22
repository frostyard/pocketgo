package state

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's saved prompts in the composer (ADR-0051): pocketd lists the
// prompts of the owner's profile folder (GET /v1/prompts), and typing / at
// the very start of a message opens a picker over the composer, filtered
// as the owner types the name. Tab or Enter puts the highlighted prompt's
// text in the composer, with $ARGUMENTS replaced by what followed the name
// on that line, and Escape closes the picker, the text left as typed.
// Nothing is ever sent on its own: the owner edits the text and sends it.
// These are the picker's rules; the app draws them.

// PromptArguments is where a prompt's text takes what follows its name.
const PromptArguments = "$ARGUMENTS"

// TakesPrompts says the composer takes a saved prompt: while its text is a
// message, as it takes files (Attaches); an answer to a question is the
// owner's own words.
func (c Composer) TakesPrompts() bool { return !c.Answers() }

// SlashHead is a draft's first line read as a call of a prompt: / at the
// very start, then the name typed so far, then, after white space, its
// arguments.
type SlashHead struct {
	// Name is the name typed so far, as typed: "" right after the /.
	Name string
	// Args are what follows the name on the line, without the white space
	// around them.
	Args string
}

// ParseSlashHead reads draft's first line as a call of a prompt: it starts
// with /, then letters, digits and dashes, the name typed so far, then the
// end of the line or white space before the arguments. Anything else, as a
// path (/etc/hosts) or text before the /, is no call.
func ParseSlashHead(draft string) (SlashHead, bool) {
	rest, ok := strings.CutPrefix(draft, "/")
	if !ok {
		return SlashHead{}, false
	}
	line, _, _ := strings.Cut(rest, "\n")
	n := 0
	for n < len(line) && nameByte(line[n]) {
		n++
	}
	name, after := line[:n], line[n:]
	if after != "" && after[0] != ' ' && after[0] != '\t' {
		return SlashHead{}, false
	}
	return SlashHead{Name: name, Args: strings.TrimSpace(after)}, true
}

// nameByte says b may be in a prompt's name as the owner types it: the
// name's lowercase letters, digits and dashes, and capitals, which match
// in any case, as a phone's keyboard may capitalize.
func nameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-'
}

// PromptMatches are the prompts of list whose name matches typed, in any
// case: those whose name starts with it, by name, then those that hold it
// elsewhere, by name. usable are the valid ones, which the composer takes;
// broken the files with a problem, which it shows with their problem and
// never takes.
func PromptMatches(list []proto.Prompt, typed string) (usable, broken []proto.Prompt) {
	typed = strings.ToLower(typed)
	var first, later []proto.Prompt
	for _, p := range list {
		name := strings.ToLower(p.Name)
		switch {
		case strings.HasPrefix(name, typed):
			first = append(first, p)
		case strings.Contains(name, typed):
			later = append(later, p)
		}
	}
	byName := func(a, b proto.Prompt) int { return strings.Compare(a.Name, b.Name) }
	slices.SortStableFunc(first, byName)
	slices.SortStableFunc(later, byName)
	for _, p := range append(first, later...) {
		if p.Problem != "" || p.Text == "" {
			broken = append(broken, p)
		} else {
			usable = append(usable, p)
		}
	}
	return usable, broken
}

// PromptList is what the composer offers for draft: the prompts whose name
// matches the name its call types, else, with no call, every one (the
// phone's sheet).
func PromptList(draft string, list []proto.Prompt) (usable, broken []proto.Prompt) {
	head, _ := ParseSlashHead(draft)
	return PromptMatches(list, head.Name)
}

// ExpandPrompt is a prompt's text with args, what followed its name:
// in place of each $ARGUMENTS it holds, else after it, below a blank line,
// so no word the owner typed is lost.
func ExpandPrompt(text, args string) string {
	if strings.Contains(text, PromptArguments) {
		return strings.ReplaceAll(text, PromptArguments, args)
	}
	if args == "" {
		return text
	}
	return text + "\n\n" + args
}

// InsertPrompt is the composer's text once prompt p goes into draft, and
// where its caret goes, in characters: after the prompt's text. A call
// (ParseSlashHead) is replaced by the prompt with its arguments, and the
// lines after it stay below; a draft with no call keeps its text below
// the prompt, after a blank line.
func InsertPrompt(draft string, p proto.Prompt) (text string, caret int) {
	if head, ok := ParseSlashHead(draft); ok {
		text = ExpandPrompt(p.Text, head.Args)
		caret = utf8.RuneCountInString(text)
		if _, rest, more := strings.Cut(draft, "\n"); more {
			text += "\n" + rest
		}
		return text, caret
	}
	text = ExpandPrompt(p.Text, "")
	caret = utf8.RuneCountInString(text)
	if strings.TrimSpace(draft) != "" {
		text += "\n\n" + draft
	}
	return text, caret
}

// PromptPicker is the / picker of one composer field: whether it shows,
// the call the owner closed it on, and which prompt is highlighted, by
// name, so a list that changes keeps it.
type PromptPicker struct {
	shown bool
	// closedOn is the first line of the field's text when the picker
	// closed, "" while it is not closed: it stays closed while the text
	// starts with it (closed).
	closedOn  string
	highlight string
}

// firstLine is s up to its first line break.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// closed says the owner closed the picker on a call that draft still
// starts with: typing on after it keeps it closed, and erasing into it,
// or a text that no longer starts with it, opens it again.
func (p *PromptPicker) closed(draft string) bool {
	return p.closedOn != "" && strings.HasPrefix(firstLine(draft), p.closedOn)
}

// close closes the picker on text's call.
func (p *PromptPicker) close(text string) {
	p.closedOn, p.highlight, p.shown = firstLine(text), "", false
}

// PromptMenu is what the picker shows over the composer.
type PromptMenu struct {
	// Open says it shows: the draft starts with a call (ParseSlashHead)
	// and the owner did not close it.
	Open bool
	// Opened says it opened with this draft: the composer asks pocketd for
	// the prompts again then, so a change to the folder shows.
	Opened bool
	Head   SlashHead
	// Items are the prompts it offers, best first, and Broken the files
	// whose name matches that have a problem (PromptMatches).
	Items, Broken []proto.Prompt
	// Highlight is the place in Items of the prompt Tab or Enter takes,
	// -1 when it offers none.
	Highlight int
	// Status is where the list stands, and Err why its last fetch failed;
	// Problem is pocketd's, with the folder.
	Status       ListStatus
	Err, Problem string
	// None says pocketd lists no valid prompt at all (NoPrompts).
	None bool
}

// NoPrompts says prompts holds no valid prompt.
func NoPrompts(prompts proto.Prompts) bool {
	return !slices.ContainsFunc(prompts.Prompts, func(p proto.Prompt) bool { return p.Problem == "" && p.Text != "" })
}

// PromptNote is what the picker says above its rows, "" for nothing:
// while it offers none, that the list loads or why it failed to load;
// pocketd's problem with the folder, whenever it has one; else, while it
// offers none, that only files with a problem match, that the owner has
// no prompt yet and where one goes, or that none matches the name typed.
func PromptNote(m PromptMenu) string {
	if len(m.Items) == 0 {
		switch m.Status {
		case ListLoading:
			return "Loading your saved prompts…"
		case ListFailed:
			return "Your saved prompts did not load: " + strings.TrimSuffix(m.Err, ".") + "."
		}
	}
	if m.Problem != "" {
		return "Your saved prompts: " + strings.TrimSuffix(m.Problem, ".") + "."
	}
	called := ""
	if m.Head.Name != "" {
		called = " matches /" + m.Head.Name
	}
	switch {
	case len(m.Items) > 0:
		return ""
	case len(m.Broken) > 0:
		return "No saved prompt you can use" + called + ": fix the files below."
	case m.None:
		return "You have no saved prompts yet. Add one as prompts/<name>.md in the profile folder."
	}
	return "No saved prompt" + called + "."
}

// PickerKey is a key the picker may take while it shows.
type PickerKey int

const (
	PickerUp PickerKey = iota
	PickerDown
	// PickerEnter and PickerTab put the highlighted prompt in.
	PickerEnter
	PickerTab
	// PickerEscape closes the picker.
	PickerEscape
)

// PickerResult is what a key did in the picker.
type PickerResult struct {
	// Took says the picker took the key: the field does nothing more
	// with it.
	Took bool
	// Insert says a prompt went in: Text is the composer's text now, and
	// Caret where its caret goes, in characters.
	Insert bool
	Text   string
	Caret  int
}

// menu is the picker's menu for draft, without changing what it records.
func (p *PromptPicker) menu(draft string, prompts proto.Prompts, status ListStatus, err string) PromptMenu {
	head, ok := ParseSlashHead(draft)
	m := PromptMenu{Head: head, Highlight: -1, Status: status, Err: err, Problem: prompts.Problem, None: NoPrompts(prompts)}
	if !ok || p.closed(draft) {
		return m
	}
	m.Open = true
	m.Items, m.Broken = PromptMatches(prompts.Prompts, head.Name)
	if len(m.Items) > 0 {
		m.Highlight = max(slices.IndexFunc(m.Items, func(q proto.Prompt) bool { return q.Name == p.highlight }), 0)
	}
	return m
}

// View is the picker for draft as the frame draws it, with the prompts
// pocketd listed, where their list stands and why its last fetch failed.
// It opens when the draft starts with a call, and says so once (Opened);
// a draft that no longer starts with the call it was closed on ends the
// close, so the picker opens again on the next call.
func (p *PromptPicker) View(draft string, prompts proto.Prompts, status ListStatus, err string) PromptMenu {
	if !p.closed(draft) {
		p.closedOn = ""
	}
	m := p.menu(draft, prompts, status, err)
	m.Opened = m.Open && !p.shown
	p.shown = m.Open
	if !m.Open {
		p.highlight = ""
	}
	return m
}

// Key is what key does in the picker with text, the field's text as the
// key comes: while the picker shows, Up and Down move the highlight
// through the prompts it offers, around at the ends; Enter and Tab put the
// highlighted one in (InsertPrompt) and close it; Enter with none to put
// in does nothing, so a call typed before its prompt shows is never sent,
// and Tab moves the focus as it would; Escape closes it, the text as it
// was, until the text no longer starts with the call's line as it was
// then. While it does not show, it takes no key.
func (p *PromptPicker) Key(key PickerKey, text string, prompts proto.Prompts) PickerResult {
	m := p.menu(text, prompts, ListLoaded, "")
	if !m.Open {
		return PickerResult{}
	}
	switch key {
	case PickerUp, PickerDown:
		if len(m.Items) == 0 {
			return PickerResult{}
		}
		step := 1
		if key == PickerUp {
			step = -1
		}
		i := (m.Highlight + step + len(m.Items)) % len(m.Items)
		p.highlight = m.Items[i].Name
		return PickerResult{Took: true}
	case PickerEnter, PickerTab:
		if m.Highlight < 0 {
			return PickerResult{Took: key == PickerEnter}
		}
		out, caret := p.insert(text, m.Items[m.Highlight])
		return PickerResult{Took: true, Insert: true, Text: out, Caret: caret}
	case PickerEscape:
		p.close(text)
		return PickerResult{Took: true}
	}
	return PickerResult{}
}

// Choose puts prompt name in draft, as a click on its row does: the text
// and caret of InsertPrompt, and the picker closed. A name the list no
// longer offers as a valid prompt puts nothing in.
func (p *PromptPicker) Choose(name, draft string, prompts proto.Prompts) (text string, caret int, ok bool) {
	i := slices.IndexFunc(prompts.Prompts, func(q proto.Prompt) bool { return q.Name == name })
	if i < 0 || prompts.Prompts[i].Problem != "" || prompts.Prompts[i].Text == "" {
		return draft, 0, false
	}
	text, caret = p.insert(draft, prompts.Prompts[i])
	return text, caret, true
}

// insert puts prompt q in draft and closes the picker: a prompt whose own
// text starts as a call keeps it closed.
func (p *PromptPicker) insert(draft string, q proto.Prompt) (string, int) {
	text, caret := InsertPrompt(draft, q)
	p.close(text)
	return text, caret
}

// Summon opens the picker on draft, as the palette's Insert Prompt does:
// a draft that starts with a call keeps it, its caret at the call's line's
// end; another gets a / of its own on a first line, its text below, and
// the caret after the /.
func (p *PromptPicker) Summon(draft string) (text string, caret int) {
	p.closedOn, p.highlight, p.shown = "", "", false
	if _, ok := ParseSlashHead(draft); ok {
		line, _, _ := strings.Cut(draft, "\n")
		return draft, utf8.RuneCountInString(line)
	}
	if draft == "" {
		return "/", 1
	}
	return "/\n" + draft, 1
}

// Prompts are the owner's saved prompts of pocketd's newest answer
// (ADR-0051), where their list stands, and why its last fetch failed.
func (d *Data) Prompts() proto.Prompts { return d.prompts }

// PromptsStatus is where the saved prompts stand, and why their last fetch
// failed; a failed fetch keeps the prompts an earlier one brought.
func (d *Data) PromptsStatus() (ListStatus, string) {
	return d.promptsList.status, d.promptsList.err
}

// SetPrompts records fetch f's prompts, unless a fetch begun later already
// answered.
func (d *Data) SetPrompts(f Fetch, prompts proto.Prompts) {
	if !d.promptsList.answer(f) {
		return
	}
	d.rev++
	d.promptsList.status, d.promptsList.err = ListLoaded, ""
	d.prompts = proto.Prompts{Problem: prompts.Problem, Prompts: slices.Clone(prompts.Prompts)}
}

// SetPromptsError records that the prompts' fetch f failed, unless a fetch
// begun later already answered; the prompts stay.
func (d *Data) SetPromptsError(f Fetch, err string) {
	if d.promptsList.answer(f) {
		d.rev++
		d.promptsList.status, d.promptsList.err = ListFailed, err
	}
}

// LoadPrompts asks pocketd for the owner's saved prompts (ADR-0051). Every
// call fetches, even with an earlier fetch in flight: the owner may have
// changed the folder since, and the picker asks again each time it opens.
// The newest answer wins (Data.SetPrompts), and a failure keeps the
// prompts an earlier answer brought.
func (s *Store) LoadPrompts() {
	f := s.Data.Begin()
	var prompts proto.Prompts
	s.do("Loading the saved prompts", func(ctx context.Context, c *proto.Client) (err error) {
		prompts, err = c.Prompts(ctx)
		return err
	}, s.track("prompts", func(err error) {
		if err != nil {
			s.Data.SetPromptsError(f, err.Error())
			return
		}
		s.Data.SetPrompts(f, prompts)
	}))
}
