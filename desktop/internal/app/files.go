package app

import (
	"fmt"
	"path"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// filesView keeps the Markdown renderer and one decoded image for this window.
type filesView struct {
	md       markdown.Options
	image    ui.ImageSource
	imageKey string
	imageErr bool
}

// filesCommand opens the read-only viewer without a keyboard binding.
func (a *App) filesCommand() frostui.Command {
	return frostui.Command{ID: "files", Title: "Browse Files…", Group: "Go to", Icon: "folder", Run: a.goFiles}
}

// goFiles leaves the conversation and shows the viewer's last location.
func (a *App) goFiles() {
	a.leaveSession()
	a.place = placeFiles
	a.st.Open(0)
	if a.st.Data.FileRoots().Status != state.ListLoaded {
		a.st.LoadFileRoots()
	}
	a.showInSidebar()
}

// openFileLink opens an unknown-kind path in the Files view.
func (a *App) openFileLink(root, p string) { a.goFiles(); a.st.OpenFilePath(root, p) }

// followLink keeps file destinations in the viewer and opens safe external URLs.
func (a *App) followLink(dest string) {
	if root, p, ok := state.ParseFileLink(dest); ok {
		a.openFileLink(root, p)
		return
	}
	if link, ok := a.st.Data.FileLinkOf(dest); ok {
		if root, p, ok := state.ParseFileLink(link); ok {
			a.openFileLink(root, p)
			return
		}
	}
	if ui.SafeToOpen(dest) {
		a.openURL(dest)
	}
}

// fileHref resolves Markdown links from the currently shown file.
func (a *App) fileHref(dest string) {
	if root, p, ok := state.ResolveFileHref(a.st.Data.FileRoots().Roots, a.st.Data.FilesSpot(), dest); ok {
		a.st.OpenFilePath(root, p)
	} else if ui.SafeToOpen(dest) {
		a.openURL(dest)
	}
}

// filesBar names the viewer and its current breadcrumb trail.
func (a *App) filesBar(c *ui.Context) {
	var labels []string
	for _, crumb := range state.FileCrumbs(a.st.Data.FileRoots().Roots, a.st.Data.FilesSpot()) {
		labels = append(labels, crumb.Label)
	}
	ui.Column(c).Grow(1).MinWidth(0).Children(func() {
		ui.Text(c, "Files").Bold().SingleLine()
		frostui.Caption(c, strings.Join(labels, " › ")).SingleLine()
	})
}

// filesPane draws navigation above homes, a folder, or a folder beside its file.
func (a *App) filesPane(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		crumbs := state.FileCrumbs(a.st.Data.FileRoots().Roots, a.st.Data.FilesSpot())
		ui.Row(c).Shrink(0).Gap(frostui.SpaceXS).Padding(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			if a.compact {
				label := "Agents' homes"
				if len(crumbs) > 1 {
					labels := make([]string, 0, len(crumbs)-1)
					for _, crumb := range crumbs[1:] {
						labels = append(labels, crumb.Label)
					}
					label = strings.Join(labels, " › ")
				}
				frostui.Caption(c, label).SingleLine().Grow(1).Shrink(1).MinWidth(0)
			} else {
				ui.Row(c).Grow(1).Shrink(1).MinWidth(0).Gap(frostui.SpaceXS).AlignItems(ui.Center).Children(func() {
					for i, crumb := range crumbs {
						if i > 0 {
							frostui.Caption(c, "›").Shrink(0)
						}
						if i == len(crumbs)-1 {
							frostui.Strong(c, crumb.Label).SingleLine().Shrink(1).MinWidth(0)
						} else if ui.Link(c.Key(i), crumb.Label, "").SingleLine().Padding(frostui.SpaceS, 0).Shrink(1).MinWidth(0).Clicked() {
							a.st.OpenFiles(crumb.Spot)
						}
					}
				})
			}
			if frostui.IconButton(c, "view-refresh", "Refresh the files").Clicked() {
				a.st.RefreshFiles()
			}
		})
		if note := a.st.Data.FilesNote(); note != "" {
			frostui.Muted(c, note).Wrap().Padding(0, frostui.SpaceL, frostui.SpaceS, frostui.SpaceL)
		}
		spot := a.st.Data.FilesSpot()
		switch {
		case spot.Root == "":
			a.filesHomes(c)
		case !spot.File:
			a.filesFolder(c)
		case a.compact:
			a.filesFile(c)
		default:
			ui.Row(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
				ui.Column(c).Width(300).Shrink(0).AlignItems(ui.Stretch).Children(func() { a.filesFolder(c) })
				ui.Box(c).Width(1).Background(t.Border).Shrink(0)
				a.filesFile(c)
			})
		}
	})
}

// filesHomes draws homes even when the latest refresh failed.
func (a *App) filesHomes(c *ui.Context) {
	view := a.st.Data.FileRoots()
	ui.Scroll(c).Grow(1).Basis(0).MinHeight(0).Fill().Children(func() {
		ui.Column(c).Padding(frostui.SpaceS).Gap(frostui.SpaceXS).AlignItems(ui.Stretch).Children(func() {
			switch view.Status {
			case state.ListLoading:
				frostui.Loading(c, "Loading the agents' homes…")
			case state.ListFailed:
				frostui.Problem(c, "Loading the agents' homes failed: "+view.Err).Wrap()
			case state.ListLoaded:
				if len(view.Roots.Roots) == 0 {
					frostui.Muted(c, "No agent has a home folder.").Wrap()
				}
			}
			for _, root := range view.Roots.Roots {
				b := ui.ButtonBase(c.Key("root:" + root.ID)).Row().FillWidth().Padding(frostui.SpaceM).Gap(frostui.SpaceM).AlignItems(ui.Center).Cursor(ui.CursorPointer).Label(root.Name)
				if b.Hovered() {
					b.Background(c.Theme().SurfaceHover)
				}
				b.Children(func() {
					frostui.Icon(c, "pocket-agent").AccessHidden(true)
					ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Children(func() {
						frostui.Strong(c, root.Name).SingleLine()
						frostui.Caption(c, root.Path).SingleLine()
					})
					if root.Private {
						frostui.Caption(c, "private")
					}
				})
				if b.Clicked() {
					a.st.OpenFiles(state.FileSpot{Root: root.ID})
				}
			}
		})
	})
}

// fileImageIcon is only an icon choice, never a content-decoding rule.
func fileImageIcon(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// filesFolder draws the listing with disabled, dimmed unreadable entries.
func (a *App) filesFolder(c *ui.Context) {
	shown, ok := a.st.Data.FilesFolder()
	if !ok {
		return
	}
	file, _ := a.st.Data.FilesFile()
	ui.Scroll(c.Key("folder:" + shown.Root + "/" + shown.Path)).Grow(1).Basis(0).MinHeight(0).Fill().Children(func() {
		ui.Column(c).Padding(frostui.SpaceS, 0).Gap(2).AlignItems(ui.Stretch).Children(func() {
			if shown.Status == state.ListLoading {
				frostui.Loading(c, "Reading the folder…").Padding(frostui.SpaceS)
			}
			if shown.Status == state.ListFailed {
				frostui.Problem(c, "Reading the folder failed: "+shown.Err).Wrap().Padding(frostui.SpaceS)
			}
			if shown.Status == state.ListLoaded && len(shown.Folder.Entries) == 0 {
				frostui.Muted(c, "Nothing to show here. Names that start with a dot are hidden.").Wrap().Padding(frostui.SpaceS)
			}
			for _, e := range state.SortFileEntries(shown.Folder.Entries) {
				p := state.JoinFilePath(shown.Path, e.Name)
				b := ui.ButtonBase(c.Key("entry:"+shown.Root+"/"+shown.Path+"/"+e.Name)).Row().FillWidth().Gap(frostui.SpaceS).Padding(frostui.SpaceS, frostui.SpaceL).AlignItems(ui.Center).Cursor(ui.CursorPointer).Label(e.Name)
				if !state.EntryOpens(e) {
					b.Opacity(0.55).Cursor(ui.CursorDefault)
				}
				if file.Root == shown.Root && file.Path == p {
					b.Background(frostui.ToneFill(c.Theme(), frostui.Accent))
				} else if b.Hovered() && state.EntryOpens(e) {
					b.Background(frostui.Panel(c.Theme()))
				}
				b.Children(func() {
					icon := "text-x-generic"
					switch {
					case e.Withheld:
						icon = "dialog-password"
					case e.Kind == proto.FileEntryFolder:
						icon = "folder"
					case e.Kind == proto.FileEntryOther:
						icon = "dialog-question"
					case fileImageIcon(e.Name):
						icon = "image-x-generic"
					}
					frostui.Icon(c, icon).TextColor(c.Theme().TextMuted).AccessHidden(true)
					ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Children(func() {
						ui.Text(c, e.Name).SingleLine()
						frostui.Caption(c, state.EntryCaption(e)).SingleLine()
					})
				})
				if b.Clicked() && state.EntryOpens(e) {
					a.st.OpenFiles(state.FileSpot{Root: shown.Root, Path: p, File: e.Kind == proto.FileEntryFile})
				}
			}
			for _, note := range state.FolderNotes(shown.Folder) {
				frostui.Caption(c, note).Wrap().Padding(frostui.SpaceS, frostui.SpaceL)
			}
		})
	})
}

// filesFile draws the current file's metadata and only its supported content.
func (a *App) filesFile(c *ui.Context) {
	shown, ok := a.st.Data.FilesFile()
	if !ok {
		return
	}
	content := shown.Content
	ui.Column(c.Key("content:" + shown.Root + "/" + shown.Path)).Grow(1).Basis(0).MinWidth(0).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
		ui.Column(c).Padding(frostui.SpaceM, frostui.SpaceL).Gap(frostui.SpaceXS).Shrink(0).Children(func() {
			frostui.Strong(c, path.Base(shown.Path)).SingleLine()
			if content.Kind != "" {
				frostui.Caption(c, state.FileSize(content.Size)+" · modified "+content.Modified.Local().Format("2 Jan 2006 15:04")).Wrap()
			}
			if note := state.FileNote(content); note != "" {
				frostui.Toned(c, note, frostui.Warning).Wrap()
			}
			if shown.Status == state.ListFailed {
				frostui.Problem(c, "Reading the file failed: "+shown.Err).Wrap()
			}
			if shown.Status == state.ListLoading {
				frostui.Loading(c, "Reading the file…")
			}
		})
		ui.Scroll(c).Grow(1).Basis(0).MinHeight(0).Fill().Children(func() {
			ui.Column(c).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).Children(func() {
				switch state.FileShows(content) {
				case "markdown":
					markdown.View(c.Key("file:"+shown.Root+"/"+shown.Path), content.Text, &a.files.md)
				case "text":
					frostui.Mono(c, content.Text).Wrap()
				case "image":
					key := fmt.Sprintf("%s/%s@%s#%d", shown.Root, shown.Path, content.Modified.String(), content.Size)
					if a.files.imageKey != key {
						var err error
						a.files.image, err = ui.DecodeImage(content.Image)
						a.files.imageErr = err != nil
						a.files.imageKey = key
					}
					if a.files.imageErr {
						frostui.Muted(c, "This image could not be drawn.").Wrap()
					} else {
						w, h := ui.ImageSize(a.files.image)
						ui.Image(c, a.files.image).FillWidth().AspectRatio(w / h).Fit(ui.Contain)
					}
				}
			})
		})
	})
}

// filesBack walks one breadcrumb back on compact windows, then returns to More.
func (a *App) filesBack(c *ui.Context) {
	crumbs := state.FileCrumbs(a.st.Data.FileRoots().Roots, a.st.Data.FilesSpot())
	if len(crumbs) == 1 {
		a.backTo(c, state.PlaceMore, state.TabMore.Title())
		return
	}
	previous := crumbs[len(crumbs)-2]
	if barButton(c, "go-previous", "Back to "+previous.Label).Clicked() {
		a.st.OpenFiles(previous.Spot)
	}
}
