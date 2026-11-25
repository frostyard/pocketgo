package nsgoclient

import (
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// A session is pinned and renamed where it shows (the owner's choice,
// 2026-11-25, as the MyGo client has it): from its sidebar row's menu, at
// a secondary click or a long press, and by the pencil beside its title,
// a double click on the title, or F2. The title becomes a field in place
// of the title over the transcript. The MyGo client put the field of a
// rename from the row in the row; NSGo's sidebar builds its rows anew at
// every change of them (ui.Sidebar, v0.3.0), which would end such an edit
// at pocketd's next event, so the field is beside the title for every way
// to start it. The session's details keep their Pin and Rename. Only a
// session pocketd says may be renamed offers a rename (ADR-0058).

// togglePin pins session id, or unpins it, and keeps the pins: its row
// moves between Pinned and Recent.
func (c *client) togglePin(id int64) {
	c.prefs.Pinned = c.prefs.Pinned.Toggle(id)
	c.savePrefs()
	c.changed()
}

// sessionMenu is the menu of sidebar row k: a session's row pins its
// session, with a check that says whether it is pinned, and a session
// that may be renamed has Rename…, off while a rename of it is on its way
// (state.Store.MayRename). Another row has no menu.
func (c *client) sessionMenu(k state.SidebarKey) []core.Node {
	id, ok := state.PinOf(k)
	if !ok {
		return nil
	}
	items := []core.Node{ui.MenuItem{Text: "Pinned", Checked: func() bool { c.revision.Get(); return c.prefs.Pinned.Has(id) }, OnSelect: func() { c.togglePin(id) }}}
	if _, ok := c.store.Data.RenameOf(k); ok {
		items = append(items, ui.MenuItem{Text: "Rename…", Icon: lucide.Pencil, Disabled: func() bool { c.revision.Get(); return !c.store.MayRename(id) },
			OnSelect: func() { c.renameRow(id) }})
	}
	return items
}

// renameRow renames a row's session, which it shows first, as choosing
// the row would: the title's field is where the session shows.
func (c *client) renameRow(id int64) {
	if !c.store.MayRename(id) {
		return
	}
	c.openSession(id)
	c.startRename(id)
}

// startRename turns the shown session id's title into a field, when an
// edit of it may start; an edit of another session's title ends first, as
// its field leaving would end it.
func (c *client) startRename(id int64) {
	s, ok := c.store.Data.Session(id)
	if !ok || !c.store.MayRename(id) {
		return
	}
	if !c.titleEdit.Editing(id) {
		c.finishRename()
	}
	c.titleEdit.Start(s)
	c.titleDraft.Set(s.Title)
	c.changed()
}

// finishRename ends the edit of a session's title, and renames the
// session when its title changed (state.TitleEdit.Finish), unless pocketd
// no longer takes it (state.TitleEdit.Check).
func (c *client) finishRename() {
	if c.titleEdit.Session == 0 {
		return
	}
	c.titleEdit.Check(&c.store.Data)
	if id, title, send := c.titleEdit.Finish(); send {
		if err := state.ValidateTitle(title); err != nil {
			c.showError(capitalize(err.Error()))
		} else {
			c.store.Rename(id, title)
		}
	}
	c.changed()
}

// cancelRename ends the edit and sends nothing, as Escape does.
func (c *client) cancelRename() {
	if c.titleEdit.Session == 0 {
		return
	}
	c.titleEdit.Cancel()
	c.changed()
}

// renamingTitle says whether session id's title is a field.
func (c *client) renamingTitle(id int64) bool {
	c.revision.Get()
	return c.titleEdit.Editing(id)
}

// titleField is the field of a session's title while the owner renames
// it: Enter, or the focus leaving it, renames the session when the title
// changed, and Escape gives up. A compact window, whose keyboard may have
// no Escape, has Cancel beside it, and there the focus leaving the field
// keeps the edit, or pressing Cancel would rename first.
func (c *client) titleField(cx *core.Scope) core.Node {
	cx.OnKeyWith("escape", core.KeyOptions{When: core.KeyAlways}, c.cancelRename)
	field := ui.Input{Label: "Session title", Placeholder: "Session title", AutoFocus: true, Value: c.titleDraft.Get,
		Style:    core.Style{Width: core.Pt(titleRoom), MaxWidth: core.Pct(100)},
		OnChange: func(s string) { c.titleDraft.Set(s); c.titleEdit.Draft = s }, OnSubmit: func(string) { c.finishRename() }}
	if !c.compact() {
		field.OnBlur = c.finishRename
	}
	return core.View{Style: core.Style{FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{field}}
}

// titleRoom is the width the title's field asks for: room for a title of
// a few words. It shrinks where the window has less.
const titleRoom core.Points = 360

// renameButton is the pencil beside the open session's title: it shows on
// a session that may be renamed, while its title is not a field, and is
// off while a rename of it is on its way.
func (c *client) renameButton(open func() (proto.Session, bool)) core.Node {
	return core.Show{When: func() bool {
		s, ok := open()
		return ok && s.Renamable && !c.titleEdit.Editing(s.ID)
	}, Then: func(*core.Scope) core.Node {
		return ui.IconButton{Label: "Rename this session", Icon: lucide.Pencil,
			Disabled: func() bool { c.revision.Get(); return !c.store.MayRename(c.session.Get()) },
			OnPress:  func() { c.startRename(c.session.Get()) }}
	}}
}
