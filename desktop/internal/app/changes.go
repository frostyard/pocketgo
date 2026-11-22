package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The Changes view (ADR-0053): what the open session changed in its git
// checkout, its files and a file's diff, read from pocketd. A desktop's
// window shows it as a pane beside the session, a compact one as a sheet;
// the count in the session's header opens it. Its rules are state's
// (state.ChangesSummary, state.FileState, state.CutNote and the rest);
// frostui's DiffView draws the diff.

// changesView is the Changes view's state.
type changesView struct {
	// open shows the pane beside the session on a desktop's window; a
	// compact one's sheet opens on phoneNav.changes, the window's own
	// flag, as mobile.Sheet keeps its state by its address.
	open bool
	// diff draws the file's diff; one per window.
	diff frostui.DiffView
}

// changesLabel is the open session's count, "" when it has none to show.
func (a *App) changesLabel(id int64) string {
	c, ok := a.st.Data.Changes(id)
	return state.ChangesSummary(c, ok)
}

// changesButton is the count in the session's header, which opens the
// Changes view and closes the pane again; none for a session whose folder
// is in no checkout.
func (a *App) changesButton(c *ui.Context, id int64) {
	label := a.changesLabel(id)
	if label == "" {
		return
	}
	tip := "Show what this session changed"
	if label == state.NotRead {
		tip = "Show why the changes were not read"
	}
	if a.changes.open {
		tip = "Hide the changes"
	}
	if frostui.LabeledButton(c, "pocket-changes", label, false).Tooltip(tip).Label("Changes: " + label).Clicked() {
		a.toggleChanges(id)
	}
}

// toggleChanges opens the Changes view of session id, fetching its
// changes afresh, or closes the desktop's pane.
func (a *App) toggleChanges(id int64) {
	if a.compact {
		a.phone.changes = true
		a.st.RefreshChanges(id)
		return
	}
	a.changes.open = !a.changes.open
	if a.changes.open {
		a.st.RefreshChanges(id)
	}
}

// changesPaneShown says the desktop's window draws the Changes pane: the
// owner opened it, a session shows, and that session has changes to show.
func (a *App) changesPaneShown() bool {
	o := a.st.Data.Open()
	return a.changes.open && a.place == placeSession && o != nil && a.changesLabel(o.ID) != ""
}

// changesPane is the pane beside the session. Its segment of the top bar
// stays empty: the bar's end, the palette and the main menu, lies over the
// last pane's, so its title and buttons are in its content.
func (a *App) changesPane(c *ui.Context) frostui.Pane {
	return frostui.Pane{ID: "changes", Label: "Changes", Width: 460, Min: 320, Max: 900,
		Content: func() { a.changesContent(c) },
	}
}

// changesContent is the pane: its count, Refresh and Close, where the
// changes are counted from, the files, and the diff of the file chosen.
func (a *App) changesContent(c *ui.Context) {
	t := c.Theme()
	id := a.st.Data.OpenID()
	ui.Column(c).Grow(1).AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(frostui.SpaceXS, frostui.SpaceS, 0, frostui.SpaceL).Shrink(0).Children(func() {
			frostui.Strong(c, a.changesLabel(id)).SingleLine().Grow(1).Shrink(1).MinWidth(0)
			if frostui.IconButton(c, "view-refresh", "Refresh the changes").Disabled(a.st.ChangesLoading(id)).Clicked() {
				a.st.RefreshChanges(id)
			}
			if frostui.IconButton(c, "window-close", "Close the changes").Clicked() {
				a.changes.open = false
			}
		})
		a.changesFiles(c, id, 280)
		ui.Box(c).Height(1).Background(t.Border).Shrink(0)
		a.changesDiff(c, id)
	})
}

// changesFiles is the list of the session's changed files, under where
// they are counted from, at most maxHeight DIPs tall (0 for no bound);
// a file's row shows its diff.
func (a *App) changesFiles(c *ui.Context, id int64, maxHeight float32) {
	t := c.Theme()
	sc, _ := a.st.Data.Changes(id)
	ch := sc.Changes
	shown, _ := a.st.Data.Diff()
	scroll := ui.Scroll(c).Shrink(0)
	if maxHeight > 0 {
		scroll.MaxHeight(maxHeight)
	} else {
		scroll.Grow(1).Basis(0).MinHeight(0)
	}
	scroll.Children(func() {
		ui.Column(c).Gap(2).Padding(frostui.SpaceS, 0).AlignItems(ui.Stretch).Children(func() {
			ui.Column(c).Gap(frostui.SpaceXS).Padding(0, frostui.SpaceL, frostui.SpaceXS, frostui.SpaceL).Children(func() {
				// A failed read says why first; what an earlier one listed
				// shows under it, if any did (state.ChangesSummary).
				if sc.Status == state.ListFailed {
					frostui.Problem(c, "Reading the changes failed: "+sc.Err).Wrap()
				}
				if ch.Repository {
					frostui.Caption(c, state.BaseNote(ch)).Wrap()
				}
			})
			if !ch.Repository {
				return
			}
			if len(ch.Files) == 0 {
				frostui.Muted(c, "Nothing changed since then.").Padding(frostui.SpaceXS, frostui.SpaceL)
			}
			for _, f := range ch.Files {
				a.changedFileRow(c, id, f, shown.Session == id && shown.Path == f.Path, t)
			}
			if more := state.MoreFiles(ch); more != "" {
				frostui.Caption(c, more).Wrap().Padding(frostui.SpaceXS, frostui.SpaceL)
			}
		})
	})
}

// changedFileRow is a changed file: its path, where its change is, and its
// lines; chosen marks the one whose diff shows.
func (a *App) changedFileRow(c *ui.Context, id int64, f proto.ChangedFile, chosen bool, t *ui.Theme) {
	b := ui.ButtonBase(c.Key("file:"+f.Path)).Row().Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(frostui.SpaceXS, frostui.SpaceL).
		Cursor(ui.CursorPointer).Label(state.FileName(f) + ", " + state.FileState(f) + ", " + state.FileCounts(f))
	if !state.CanDiff(f) {
		b.Cursor(ui.CursorDefault)
	}
	if chosen {
		b.Background(frostui.ToneFill(t, frostui.Accent))
	} else if b.Hovered() {
		b.Background(frostui.Panel(t))
	}
	b.Children(func() {
		icon := "text-x-generic"
		if f.New {
			icon = "document-new"
		}
		frostui.Icon(c, icon).TextColor(t.TextMuted).AccessHidden(true)
		ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Children(func() {
			frostui.Mono(c, state.FileName(f)).SingleLine()
			frostui.Caption(c, state.FileState(f)).SingleLine()
		})
		fileCounts(c, f)
	})
	// A repository inside the checkout has no diff to show (state.CanDiff).
	if b.Clicked() && state.CanDiff(f) {
		a.st.ShowDiff(id, f.Path)
	}
}

// fileCounts is a file's lines, added in the success tone and removed in
// the danger tone, or why there are none.
func fileCounts(c *ui.Context, f proto.ChangedFile) {
	if f.Binary || f.Uncounted {
		frostui.Caption(c, state.FileCounts(f)).SingleLine()
		return
	}
	ui.Row(c).Gap(frostui.SpaceXS).Shrink(0).Children(func() {
		frostui.Toned(c, "+"+itoa(f.Added), frostui.Success).Font("monospace")
		frostui.Toned(c, "−"+itoa(f.Removed), frostui.Danger).Font("monospace")
	})
}

// changesDiff is the diff of the file chosen, or a word to choose one.
func (a *App) changesDiff(c *ui.Context, id int64) {
	d, ok := a.st.Data.Diff()
	if !ok || d.Session != id {
		frostui.Empty(c, "pocket-changes", "Choose a file", "Its diff from where the session's branch left its base shows here.").Grow(1)
		return
	}
	ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
		ui.Column(c).Gap(frostui.SpaceXS).Padding(frostui.SpaceS, frostui.SpaceL).Shrink(0).Children(func() {
			frostui.Mono(c, d.Path).Bold().Wrap()
			if cut := state.CutNote(d.Diff); cut != "" {
				frostui.Toned(c, cut, frostui.Warning).Wrap()
			}
			if d.Err != "" {
				frostui.Problem(c, "Reading the diff failed: "+d.Err).Wrap()
			}
		})
		switch {
		case d.Status == state.ListLoading:
			ui.Column(c).Grow(1).Center().Children(func() { frostui.Loading(c, "Reading the diff…") })
		case d.Status == state.ListFailed && d.Diff.Diff == "":
			ui.Box(c).Grow(1)
		case state.NoDiff(d.Diff) != "":
			frostui.Muted(c, state.NoDiff(d.Diff)).Padding(frostui.SpaceS, frostui.SpaceL)
		default:
			a.changes.diff.SetPatch(d.Diff.Diff)
			a.changes.diff.View(c).Grow(1).Basis(0).MinHeight(0)
		}
	})
}

// changesSheet is the Changes view on a compact window: a sheet of the
// files, and a file's diff in its place with a way back.
func (a *App) changesSheet(c *ui.Context) {
	o := a.shownSession()
	if o == nil {
		a.phone.changes = false
	}
	mobile.Sheet(c, &a.phone.changes, mobile.SheetOptions{Detents: []float32{changesDetent}, Label: "Changes"}, func() {
		if o == nil {
			return
		}
		t := c.Theme()
		id := o.ID
		d, showing := a.st.Data.Diff()
		showing = showing && d.Session == id
		ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(0, frostui.SpaceS, 0, frostui.SpaceL).Shrink(0).Children(func() {
				if showing {
					if barButton(c, "go-previous", "Back to the files").Clicked() {
						a.st.Data.CloseDiff()
					}
				}
				ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Children(func() {
					ui.Text(c, "Changes").FontSize(17).FontWeight(600).SingleLine()
					frostui.Caption(c, a.changesLabel(id)).SingleLine()
				})
				if barButton(c, "view-refresh", "Refresh the changes").Clicked() {
					a.st.RefreshChanges(id)
				}
				if barButton(c, "window-close", "Close the changes").Clicked() {
					a.phone.changes = false
				}
			})
			ui.Box(c).Height(1).Background(t.Border).Shrink(0)
			if showing {
				a.changesDiff(c, id)
				return
			}
			a.changesFiles(c, id, 0)
		})
	})
}

// changesDetent is the Changes sheet's one height, as the Activity's: a
// sheet lays what it holds out at its tallest height.
const changesDetent = 0.9

// changesChip is the count under a compact window's top bar, which opens
// the Changes sheet; none for a session whose folder is in no checkout.
func (a *App) changesChip(c *ui.Context, id int64) {
	label := a.changesLabel(id)
	if label == "" {
		return
	}
	t := c.Theme()
	b := ui.ButtonBase(c.Key("changes-chip")).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(2, frostui.SpaceS).
		Radius(frostui.RadiusChip).Border(1, t.Border).Cursor(ui.CursorPointer).Label("Changes: " + label).Shrink(0)
	if b.Pressed() {
		b.Opacity(0.4)
	}
	b.Children(func() {
		frostui.Icon(c, "pocket-changes").TextColor(t.Accent).FontSize(frostui.TextSize(c, 12))
		frostui.Caption(c, label).TextColor(t.Accent).SingleLine()
	})
	if b.Clicked() {
		a.toggleChanges(id)
	}
}

// threadChanges is a thread card's count of its session's changes, which
// opens the session with its Changes view; none when there is none.
func (a *App) threadChanges(c *ui.Context, key string, th proto.Thread) {
	sc, ok := a.st.Data.Changes(th.Session)
	label := state.CardChanges(sc, ok)
	if label == "" || th.Session == 0 {
		return
	}
	t := c.Theme()
	b := ui.ButtonBase(c.Key(key + ":changes")).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).Cursor(ui.CursorPointer).AlignSelf(ui.Start).
		Label("Changes: " + label).Tooltip("Show what this thread's session changed")
	b.Children(func() {
		frostui.Icon(c, "pocket-changes").TextColor(t.Accent).FontSize(frostui.TextSize(c, 12)).AccessHidden(true)
		frostui.Caption(c, label).TextColor(t.Accent).SingleLine()
	})
	if b.Clicked() {
		a.openSession(th.Session)
		if a.compact {
			a.phone.changes = true
		} else {
			a.changes.open = true
		}
		a.st.RefreshChanges(th.Session)
	}
}
