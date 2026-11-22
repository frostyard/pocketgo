package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The owner's saved prompts in the composer (ADR-0051): / at the start of
// a message opens a picker over the field, the palette's Insert Prompt
// opens it too, and a phone's composer has a button beside Attach that
// opens the same list as a sheet. A prompt goes into the composer, and
// nothing is sent until the owner sends it. The rules are state's
// (state.PromptPicker, state.InsertPrompt, state.PromptNote); this file
// draws them.

// fieldEdit is a text a prompt put in the composer's field key: the next
// frame that draws the field replaces its text with it, as a step of undo
// of its own, and puts the caret at an offset, in characters, after the
// prompt (Codex review of #108, round 1).
type fieldEdit struct {
	key any
	state.PromptEdit
}

// hidePicker says to session id's picker that its composer left the
// window (state.PromptPicker.Hide), so it asks pocketd again as it shows
// (Codex review of #108, round 1).
func (cp *composer) hidePicker(id int64) {
	if p := cp.pickers[id]; p != nil {
		p.Hide()
	}
}

// picker is session id's / picker, made when missing.
func (cp *composer) picker(id int64) *state.PromptPicker {
	if cp.pickers == nil {
		cp.pickers = map[int64]*state.PromptPicker{}
	}
	p := cp.pickers[id]
	if p == nil {
		p = &state.PromptPicker{}
		cp.pickers[id] = p
	}
	return p
}

// pickerKey is the picker's key for k, a key pressed in the field with no
// modifier: Up, Down, Enter, Tab and Escape.
func pickerKey(k ui.TextKey) (state.PickerKey, bool) {
	if k.Mods != 0 {
		return 0, false
	}
	switch k.Key {
	case ui.KeyUp:
		return state.PickerUp, true
	case ui.KeyDown:
		return state.PickerDown, true
	case ui.KeyEnter:
		return state.PickerEnter, true
	case ui.KeyTab:
		return state.PickerTab, true
	case ui.KeyEscape:
		return state.PickerEscape, true
	}
	return 0, false
}

// promptMenu is the / picker over session id's composer: the saved prompts
// whose name matches the one typed, the highlighted one marked, each a
// row that puts it in the composer; then the files of that name with a
// problem, with why; what it says when it offers none; and, on a
// desktop's window, its keys.
func (a *App) promptMenu(c *ui.Context, id int64, draft *string, picker *state.PromptPicker, menu state.PromptMenu) {
	t := c.Theme()
	cp := &a.composer
	ui.Column(c.Key("prompt-menu")).AlignItems(ui.Stretch).Radius(10).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).
		Background(frostui.Panel(t)).Padding(frostui.SpaceXS, 0).Role(ui.RoleList).Label("Saved prompts").Children(func() {
		if note := state.PromptNote(menu); note != "" {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(frostui.SpaceXS, frostui.SpaceM).Children(func() {
				switch {
				case menu.Status == state.ListLoading && len(menu.Items) == 0:
					frostui.Loading(c, note)
				case menu.Status == state.ListFailed && len(menu.Items) == 0:
					frostui.Problem(c, note).Wrap().Grow(1)
					if ui.Button(c.Key("prompts-again"), "Try Again").Clicked() {
						a.st.LoadPrompts()
					}
				default:
					frostui.Caption(c, note).Wrap().Grow(1)
				}
			})
		}
		if len(menu.Items) == 0 && len(menu.Broken) == 0 {
			return
		}
		ui.Scroll(c).MaxHeight(240).Children(func() {
			ui.Column(c).AlignItems(ui.Stretch).Children(func() {
				for i, p := range menu.Items {
					row := a.promptRow(c, p, i == menu.Highlight)
					if i == menu.Highlight && cp.shownHighlight != p.Name {
						// Moved by a key: it comes into view.
						row.ScrollIntoView()
						cp.shownHighlight = p.Name
					}
					if row.Clicked() {
						if text, caret, ok := picker.Choose(p.Name, *draft, a.st.Data.Prompts()); ok {
							cp.edit = &fieldEdit{key: id, PromptEdit: state.PromptEdit{Text: text, Caret: caret}}
							cp.refocus = true
							// A row the keyboard chose holds Enter, as the
							// field's Enter does (Codex review of #108,
							// round 2).
							if c.KeyDown(ui.KeyEnter) {
								picker.HoldEnter()
							}
						}
					}
				}
				for _, p := range menu.Broken {
					ui.Column(c.Key("broken:"+p.Name)).Gap(2).Padding(6, frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
						frostui.Mono(c, "/"+p.Name).TextColor(t.TextMuted).SingleLine()
						frostui.Problem(c, p.Problem).Wrap()
					})
				}
			})
		})
		if !a.compact {
			ui.Row(c).Padding(frostui.SpaceXS, frostui.SpaceM, 0, frostui.SpaceM).Children(func() {
				frostui.Caption(c, "↑ ↓ choose · Enter or Tab puts it in your message · Esc closes").SingleLine()
			})
		}
	})
}

// promptRow is a saved prompt's row in the picker: its name as typed after
// /, what may follow it, and what it is for, on one line on a desktop's
// window and on two on a compact one; highlighted, in the selection's
// tint.
func (a *App) promptRow(c *ui.Context, p proto.Prompt, highlighted bool) ui.Element {
	t := c.Theme()
	row := ui.ButtonBase(c.Key("prompt:"+p.Name)).Row().Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(6, frostui.SpaceM).
		Cursor(ui.CursorPointer).Label("/" + p.Name).Role(ui.RoleListItem)
	if a.compact {
		row.Column().Gap(2).AlignItems(ui.Stretch)
	}
	switch {
	case highlighted:
		row.Background(frostui.SoftSelection(t).Accent)
	case row.Hovered():
		row.Background(t.SurfaceHover)
	}
	row.Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Shrink(0).Children(func() {
			frostui.Mono(c, "/"+p.Name).FontWeight(600).SingleLine().Shrink(0)
			if p.ArgumentHint != "" {
				frostui.Mono(c, p.ArgumentHint).TextColor(t.TextMuted).SingleLine().Shrink(1).MinWidth(0)
			}
		})
		if p.Description != "" {
			frostui.Muted(c, p.Description).SingleLine().Grow(1).Shrink(1).MinWidth(0)
		}
	})
	return row
}

// summonPrompts is the palette's Insert Prompt: the shown session's
// composer gets a / of its own (state.PromptPicker.Summon), which opens
// the picker, and the keyboard focus.
func (a *App) summonPrompts() {
	id, ok := a.promptsComposer()
	if !ok {
		return
	}
	cp := &a.composer
	draft := cp.draft(id)
	text, caret := cp.picker(id).Summon(*draft)
	cp.edit = &fieldEdit{key: id, PromptEdit: state.PromptEdit{Text: text, Caret: caret}}
	cp.refocus = true
}

// promptsComposer is the session whose composer takes a saved prompt now:
// the one shown, whose composer writes a message; none while it shows no
// session, or one whose composer is read only or answers a question.
func (a *App) promptsComposer() (int64, bool) {
	o := a.st.Data.Open()
	if a.place != placeSession || o == nil {
		return 0, false
	}
	if readOnly, _ := a.st.Data.ReadOnly(o.ID); readOnly {
		return 0, false
	}
	return o.ID, a.st.Data.ComposerOf(o.ID).TakesPrompts()
}

// openPromptSheet opens the phone's list of the saved prompts for session
// id's composer, and asks pocketd for them again, so a change shows.
func (a *App) openPromptSheet(id int64) {
	a.phone.prompts, a.phone.promptsFor = true, id
	a.st.LoadPrompts()
}

// phonePrompts is the phone's sheet of the saved prompts: the same list as
// the picker, every one when the draft holds no call, else those of the
// name it types; a tap puts one in the composer and closes the sheet. The
// files with a problem are listed apart, with why.
func (a *App) phonePrompts(c *ui.Context) {
	t := c.Theme()
	id := a.phone.promptsFor
	draft := a.composer.draft(id)
	picker := a.composer.picker(id)
	prompts := a.st.Data.Prompts()
	status, err := a.st.Data.PromptsStatus()
	usable, broken := state.PromptList(*draft, prompts.Prompts)
	menu := state.PromptMenu{Open: true, Items: usable, Broken: broken, Status: status, Err: err, Problem: prompts.Problem,
		None: state.NoPrompts(prompts), Refused: picker.Refused(*draft)}
	ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
		ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(0, frostui.SpaceS, 0, frostui.SpaceL).Shrink(0).Children(func() {
			ui.Text(c, "Saved Prompts").FontSize(17).FontWeight(600).Grow(1)
			if barButton(c, "window-close", "Close Saved Prompts").Clicked() {
				a.phone.prompts = false
			}
		})
		ui.Box(c).Height(1).Background(t.Border).Shrink(0)
		ui.Scroll(c).Grow(1).Basis(0).MinHeight(0).Children(func() {
			ui.Column(c).Gap(frostui.SpaceL).Padding(frostui.SpaceM, 0, frostui.SpaceL, 0).AlignItems(ui.Stretch).Children(func() {
				if note := state.PromptNote(menu); note != "" {
					ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(0, frostui.SpaceL).Children(func() {
						if status == state.ListFailed && len(usable) == 0 {
							frostui.Problem(c, note).Wrap().Grow(1)
							if ui.Button(c.Key("prompts-again"), "Try Again").Clicked() {
								a.st.LoadPrompts()
							}
							return
						}
						frostui.Caption(c, note).Wrap().Grow(1)
					})
				}
				if len(usable) > 0 {
					mobile.Section(c.Key("prompts"), "Prompts", func() {
						for _, p := range usable {
							subtitle := joinNonEmpty(p.ArgumentHint, p.Description)
							if mobile.ListRow(c.Key("prompt:"+p.Name), mobile.RowOptions{Title: "/" + p.Name, Subtitle: subtitle,
								Leading: func() { frostui.Icon(c, "pocket-prompt").FontSize(20).TextColor(t.Accent) }}).Focusable().Clicked() {
								// Only into the composer shown, while it writes a
								// message (Codex review of #108, round 1).
								if shown, ok := a.promptsComposer(); !ok || shown != id {
									a.phone.prompts = false
									return
								}
								// With its caret after it, as the picker's; one too
								// long stays out, and the sheet says why.
								if text, caret, ok := picker.Choose(p.Name, *draft, prompts); ok {
									a.composer.edit = &fieldEdit{key: id, PromptEdit: state.PromptEdit{Text: text, Caret: caret}}
									a.composer.refocus = true
									if c.KeyDown(ui.KeyEnter) {
										picker.HoldEnter()
									}
									a.phone.prompts = false
								}
							}
						}
					})
				}
				if len(broken) > 0 {
					mobile.Section(c.Key("broken"), "Need Fixing", func() {
						for _, p := range broken {
							mobile.ListRow(c.Key("broken:"+p.Name), mobile.RowOptions{Title: "/" + p.Name, Subtitle: p.Problem,
								Leading: func() { frostui.Icon(c, "dialog-warning").FontSize(20).TextColor(t.Warning) }})
						}
					})
				}
			})
		})
	})
}
