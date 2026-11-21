package app

import (
	"fmt"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// The composer's attachments, frostyard/pocket's whole (ADR-0029): Attach
// files opens the native chooser, a paste of files or of a PNG attaches
// them, and so do files dropped on the field. Each file shows as a chip;
// its rules are state.Store's.

// attachFiles uploads the files at paths to session id's next message.
func (a *App) attachFiles(id int64, paths []string) { a.st.AttachFiles(id, paths) }

// pasteAttachments attaches what a paste holds: its files, else its PNG.
// It says whether it took the paste; a paste of text alone goes to the
// field.
func (a *App) pasteAttachments(id int64, data transfer.Data) bool {
	if paths, err := data.Files(); err == nil && len(paths) > 0 {
		a.attachFiles(id, paths)
		return true
	}
	if png, err := data.Read(transfer.PNG); err == nil {
		a.st.Attach(id, state.PastedImageName, state.BytesSource(png))
		return true
	}
	return false
}

// pickAttachments asks the native chooser for files to attach to session
// id, and attaches the files chosen while the sign-in is the one that
// asked (state.Store.AttachPicked).
func (a *App) pickAttachments(id int64) {
	pick := a.opts.PickFiles
	if pick == nil || a.composer.picking {
		return
	}
	a.composer.picking = true
	a.st.AttachPicked(id, pick, func(err error) {
		a.composer.picking = false
		if err != nil {
			a.toast("Attaching the chosen files failed: " + err.Error())
		}
	})
}

// attachmentChips are session id's files: each its name, whether it
// uploads or failed to, Try Again when it failed, and Remove.
func (a *App) attachmentChips(c *ui.Context, id int64) {
	files := a.st.Attachments(id)
	if len(files) == 0 {
		return
	}
	ui.Row(c).Wrap().Gap(frostui.SpaceS).Children(func() {
		for _, f := range files {
			ui.Row(c.Key(fmt.Sprint("attachment", f.Key))).Gap(frostui.SpaceXS).AlignItems(ui.Center).Padding(4, 8).Radius(6).
				Background(c.Theme().Surface).Children(func() {
				label := f.Name
				if f.Uploading {
					label += " · uploading…"
				}
				if f.Err != "" {
					label += " · upload failed"
				}
				frostui.Caption(c, label).Tooltip(f.Err)
				if f.Err != "" && ui.Button(c, "Try Again").Label("Upload "+f.Name+" again").Clicked() {
					a.st.RetryAttachment(id, f.Key)
				}
				if ui.Button(c, "Remove").Label("Remove attachment " + f.Name).Clicked() {
					a.st.RemoveAttachment(id, f.Key)
				}
			})
		}
	})
}
