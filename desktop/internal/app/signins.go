package app

import (
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The Sign-ins page of the Settings window and its sheet (ADR-0062): the
// owner signs pocketd in to GitHub Copilot with the device flow pocketd
// runs. The rules are state's (signins.go): this draws them.

// signInsPage is the Settings window's Sign-ins page: a row for each
// provider pocketd signs in to, Copilot first, with its state, and Sign
// In… or Sign Out for one the desktop signs in to.
func (a *App) signInsPage() frostui.SettingsPage {
	rows := a.st.Data.SignIns()
	settings := make([]frostui.Setting, 0, len(rows))
	for _, row := range rows {
		s := frostui.Setting{Title: row.Name, Subtitle: state.SignInCaption(row), Keywords: row.Provider}
		if row.DeviceFlow {
			s.Control = func(c *ui.Context) { a.signInButton(c, row) }
		}
		settings = append(settings, s)
	}
	page := frostui.SettingsPage{ID: "signins", Title: "Sign-ins", Icon: "dialog-password",
		Keywords: "sign in login logout account subscription provider copilot github models",
		Groups: []frostui.SettingsGroup{{
			Note:     "The model subscriptions pocketd on " + host(a.st.Conn.Server) + " signs in to. pocketd keeps each sign-in and refreshes it; PocketGo never sees a token.",
			Settings: settings,
		}}}
	status, err := a.st.Data.SignInsStatus()
	switch status {
	case state.ListFailed:
		page.Body = func(c *ui.Context) {
			ui.Column(c).Gap(frostui.SpaceS).AlignItems(ui.Start).Children(func() {
				frostui.Problem(c, "pocketd's sign-ins did not load: "+err).Wrap()
				if ui.Button(c, "Try Again").Clicked() {
					a.st.LoadSignIns()
				}
			})
		}
	case state.ListLoading:
		page.Body = func(c *ui.Context) { frostui.Loading(c, "Loading the sign-ins…") }
	}
	return page
}

// signInButton is a provider's Sign In… or Sign Out.
func (a *App) signInButton(c *ui.Context, row proto.SignIn) {
	if row.State == proto.SignInSignedIn {
		if ui.Button(c.Key("signout:"+row.Provider), "Sign Out").Disabled(a.st.SigningOut(row.Provider).Busy).Clicked() {
			ask := row
			a.prefs.signOut = &ask
		}
		return
	}
	if frostui.PrimaryButton(c.Key("signin:"+row.Provider), "Sign In…").Disabled(!a.st.MaySignIn(row)).Clicked() {
		a.st.StartSignIn(row.Provider)
	}
}

// signInOverlays are what the Sign-ins page shows over the Settings
// window: the sheet of a device flow, and the question before a sign-out.
// The window draws them after Settings, so they show over it.
func (a *App) signInOverlays(c *ui.Context) {
	a.signOutConfirm(c)
	a.signInSheet(c)
}

// signOutConfirm asks before pocketd signs out of a provider.
func (a *App) signOutConfirm(c *ui.Context) {
	row := a.prefs.signOut
	if row == nil {
		return
	}
	decided, confirmed := compactConfirm(c, frostui.Confirm{
		Title: "Sign pocketd out of " + row.Name + "?",
		Body: "pocketd forgets its " + row.Name + " sign-in, and New Session no longer offers its models. " +
			"A session on one of them fails its next call until you sign in again.",
		Action: "Sign Out of " + row.Name, Destructive: true,
	})
	if !decided {
		return
	}
	a.prefs.signOut = nil
	if confirmed {
		a.st.SignOutOf(row.Provider)
	}
}

// signInSheet is the sheet of the device flow the owner started: the code,
// GitHub's device page, the wait with the code's time left, and how it
// ended, with Try Again. Cancel, Close and Escape at any step cancel the
// flow on pocketd while it waits.
func (a *App) signInSheet(c *ui.Context) {
	v := a.st.SignInSheet()
	if v.Phase == state.SheetClosed {
		return
	}
	t := c.Theme()
	page, shown, _ := state.DevicePage(v.ProviderID)
	title := "Sign in to " + v.Provider
	open := true
	ui.Modal(c, &open, func() {
		ui.Column(c).Width(a.fitWidth(c, 460)).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Role(ui.RoleDialog).Label(title).Children(func() {
			ui.Text(c, title).FontSize(18).Bold().Wrap()
			switch v.Phase {
			case state.SheetStarting:
				frostui.Loading(c, "Asking GitHub for a code…")
			case state.SheetCode:
				frostui.Muted(c, "Enter this code at GitHub's device page, and approve the sign-in there. pocketd keeps it; PocketGo never sees a token.").Wrap()
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					frostui.Mono(c, v.Flow.UserCode).FontSize(28).Bold().Selectable().Grow(1).Label("Code " + v.Flow.UserCode)
					if ui.Button(c.Key("signin:copy"), "Copy").Clicked() {
						c.Services().WriteClipboard(v.Flow.UserCode)
						a.toast("Copied the code")
					}
				})
				if page != "" {
					ui.Row(c).Children(func() {
						if frostui.PrimaryButton(c.Key("signin:open"), "Open "+shown).Tooltip(page).Clicked() {
							a.openURL(page)
						}
					})
					frostui.Caption(c, "On another computer, go to "+page+" and enter the code.").Wrap().Selectable()
				}
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c).Size(16, 16).Label("Waiting")
					frostui.Muted(c, "Waiting for approval… "+state.FormatLeft(v.Left)+" left").Grow(1)
				})
				// The time left counts down.
				c.After(time.Second)
			case state.SheetSignedIn:
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					frostui.Icon(c, "object-select").TextColor(t.Success)
					ui.Text(c, v.Text).Wrap().Grow(1)
				})
			case state.SheetFailed:
				frostui.Problem(c, v.Text).Wrap()
			}
			if v.Phase == state.SheetCode && v.Text != "" {
				frostui.Problem(c, v.Text).Wrap()
			}
			ui.Row(c).Gap(frostui.SpaceS).Justify(ui.End).Children(func() {
				switch v.Phase {
				case state.SheetFailed:
					if ui.Button(c.Key("signin:close"), "Close").Clicked() {
						open = false
					}
					if frostui.PrimaryButton(c.Key("signin:again"), "Try Again").Clicked() {
						a.st.StartSignIn(v.ProviderID)
					}
				case state.SheetSignedIn:
					if frostui.PrimaryButton(c.Key("signin:done"), "Done").AutoFocus().Clicked() {
						open = false
					}
				default:
					if ui.Button(c.Key("signin:cancel"), "Cancel").Clicked() {
						open = false
					}
				}
			})
		})
	})
	if !open {
		a.st.CancelSignIn()
	}
}
