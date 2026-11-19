package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// inboxPane lists what waits for the owner: every pending approval, oldest
// first (state.Data.Approvals). It says that nothing waits only when the
// approvals' last fetch succeeded (state.Data.ApprovalsStatus).
func (a *App) inboxPane(c *ui.Context) {
	t := c.Theme()
	approvals := a.st.Data.Approvals()
	status, problem := a.st.Data.ApprovalsStatus()
	ui.Scroll(c).Grow(1).Fill().Background(t.Background).Children(func() {
		ui.Column(c).AlignItems(ui.Center).Padding(frostui.SpaceL, frostui.SpaceXL).Children(func() {
			ui.Column(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
				switch {
				case status == state.ListFailed:
					ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Center).Children(func() {
						frostui.Problem(c, "Loading the approvals failed: "+problem+". Calls may wait for you that this list does not show.").Wrap().Grow(1)
						if ui.Button(c, "Try Again").Clicked() {
							a.st.Refresh()
						}
					})
				case status == state.ListLoading && len(approvals) == 0:
					frostui.Loading(c, "Loading the approvals…")
				case len(approvals) == 0:
					frostui.Empty(c, "object-select", "Nothing waits for you",
						"Tool calls that need your approval show here, from every session.")
				}
				for _, ap := range approvals {
					a.approvalCard(c, ap, true)
				}
			})
		})
	})
}

// approvalCard shows a tool call that waits for the owner, with Allow and
// Deny and an optional reason for a denial. inInbox adds the session it
// belongs to and a way there.
func (a *App) approvalCard(c *ui.Context, ap proto.Approval, inInbox bool) {
	t := c.Theme()
	pending := a.st.Deciding(ap.ID)
	reason := a.reasons[ap.ID]
	if reason == nil {
		reason = new(string)
		a.reasons[ap.ID] = reason
	}
	summary := state.ToolSummary(ap.Tool, ap.Arguments)
	ui.Column(c.Key(ap.ID)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Warning)).
		Role(ui.RoleGroup).Label("Approval: " + ap.Tool).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "dialog-warning").TextColor(frostui.ToneColor(t, frostui.Warning))
			frostui.Strong(c, ap.Tool+" waits for your approval").Grow(1).SingleLine()
			if inInbox {
				title := "Session"
				if s, ok := a.st.Data.Session(ap.SessionID); ok {
					title = s.Title
				}
				frostui.Caption(c, title).SingleLine()
				if ui.Button(c, "Open Session").Clicked() {
					a.openSession(ap.SessionID)
				}
			}
		})
		if summary != "" {
			ui.Row(c).Padding(frostui.SpaceXS, frostui.SpaceS).Radius(6).Background(frostui.ColorsOf(t).Code).Children(func() {
				frostui.Mono(c, summary).Wrap().Selectable()
			})
		}
		if pretty := state.IndentArguments(ap.Arguments); pretty != "" {
			open := a.argsOpen[ap.ID]
			ui.Row(c).Children(func() {
				if ui.ButtonBase(c).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).Cursor(ui.CursorPointer).
					Label("Arguments").Expanded(open).Children(func() {
					frostui.Icon(c, chevron(open))
					frostui.Muted(c, "Arguments")
				}).Clicked() {
					a.argsOpen[ap.ID] = !open
				}
			})
			if open {
				codeBox(c, pretty, 240)
			}
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			field := ui.TextInput(c, reason).Placeholder("Reason for denying (optional)").Label("Reason for denying").
				Disabled(pending.Busy).Grow(1)
			if ui.Button(c, "Deny").Disabled(pending.Busy).Clicked() || field.Submitted() {
				a.st.Decide(ap.ID, false, *reason)
			}
			if ui.PrimaryButton(c, "Allow").Disabled(pending.Busy).Clicked() {
				a.st.Decide(ap.ID, true, "")
			}
		})
		if pending.Busy {
			frostui.Loading(c, "Sending your decision…")
		}
		if pending.Err != "" {
			frostui.Problem(c, pending.Err).Wrap()
		}
	})
}

func chevron(open bool) string {
	if open {
		return "pan-down"
	}
	return "pan-end"
}
