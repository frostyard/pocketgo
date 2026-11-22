package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"
)

// confirms are the window's confirmations, one at a time in the order
// they were asked. A desktop's window shows them with frostui's dialog,
// which the pane shell draws (shown mirrors the queue); a compact window
// shows them within the screen (compactConfirm), since frostui's is wider
// than a phone's.
type confirms struct {
	queue []frostui.Confirm
	shown frostui.Dialogs
}

// Ask queues a confirmation.
func (d *confirms) Ask(req frostui.Confirm) {
	d.queue = append(d.queue, req)
	d.shown.Ask(d.mirror(req))
}

// Showing reports whether a confirmation is up.
func (d *confirms) Showing() bool { return len(d.queue) > 0 }

// mirror is req as frostui's dialog asks it: deciding it there takes it
// off the queue too.
func (d *confirms) mirror(req frostui.Confirm) frostui.Confirm {
	m := req
	m.OnConfirm = func() {
		d.queue = d.queue[1:]
		if req.OnConfirm != nil {
			req.OnConfirm()
		}
	}
	m.OnCancel = func() {
		d.queue = d.queue[1:]
		if req.OnCancel != nil {
			req.OnCancel()
		}
	}
	return m
}

// View draws the first confirmation outside the pane shell: frostui's
// dialog, or the compact one.
func (d *confirms) View(c *ui.Context, compact bool) {
	if !compact {
		d.shown.View(c)
		return
	}
	if len(d.queue) == 0 {
		return
	}
	req := d.queue[0]
	decided, confirmed := compactConfirm(c, req)
	if !decided {
		return
	}
	d.queue = d.queue[1:]
	// frostui's dialog holds what is left, without the one decided here.
	d.shown = frostui.Dialogs{}
	for _, r := range d.queue {
		d.shown.Ask(d.mirror(r))
	}
	switch {
	case confirmed && req.OnConfirm != nil:
		req.OnConfirm()
	case !confirmed && req.OnCancel != nil:
		req.OnCancel()
	}
}

// compactConfirm draws req as frostui's dialog does, within a compact
// window: as wide as the screen leaves room for, its buttons sharing its
// width. It reports a decision in the frame it happens: Cancel, Escape and
// a click outside cancel.
func compactConfirm(c *ui.Context, req frostui.Confirm) (decided, confirmed bool) {
	t := c.Theme()
	open := true
	ui.Modal(c, &open, func() {
		ui.Column(c).Width(dialogWidth(c, 440)).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Role(ui.RoleAlertDialog).Label(req.Title).Children(func() {
			ui.Text(c, req.Title).FontSize(18).Bold().Wrap()
			if req.Body != "" {
				frostui.Body(c, req.Body).Wrap().TextColor(t.TextMuted)
			}
			if len(req.Details) > 0 {
				frostui.KeyValues(c, req.Details)
			}
			if req.Note != "" {
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					frostui.Icon(c, "dialog-password").TextColor(t.TextMuted)
					frostui.Muted(c, req.Note).Wrap().Grow(1)
				})
			}
			ui.Row(c).Gap(frostui.SpaceS).Padding(frostui.SpaceS, 0, 0, 0).Children(func() {
				cancel := req.Cancel
				if cancel == "" {
					cancel = "Cancel"
				}
				if ui.Button(c, cancel).Grow(1).Basis(0).Clicked() {
					decided = true
				}
				action := frostui.PrimaryButton(c, req.Action).Grow(1).Basis(0).AutoFocus()
				if req.Destructive {
					action.Background(frostui.ToneFill(t, frostui.Danger)).TextColor(t.Danger).Border(1, frostui.ToneLine(t, frostui.Danger))
				}
				if action.Clicked() {
					decided, confirmed = true, true
				}
			})
		})
	})
	if !open {
		decided = true
	}
	return decided, confirmed
}

// dialogWidth is the width of a dialog's content that wants want DIPs,
// within the window: a modal's panel keeps Space(5) from the window's
// edges and pads its content by Space(5) (ui.Modal).
func dialogWidth(c *ui.Context, want float32) float32 {
	w, _ := c.Size()
	return max(0, min(want, w-c.Theme().Space(20)))
}
