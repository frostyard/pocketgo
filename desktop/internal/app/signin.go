package app

import (
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// signInForm is the sign-in screen's state.
type signInForm struct {
	server, token string
	busy          bool
	err           string
	// confirm is the plain-HTTP service the owner is asked about, and
	// confirmed the one they agreed to; the token waits for the answer.
	confirm, confirmed string
}

// signInView fills the window with the sign-in card.
func (a *App) signInView(c *ui.Context) {
	t := c.Theme()
	s := &a.signIn
	ui.Column(c).Fill().Center().Background(t.Background).Children(func() {
		ui.Column(c).Width(500).Gap(frostui.SpaceL).Padding(frostui.SpaceXL).AlignItems(ui.Stretch).
			Background(frostui.Panel(t)).Radius(12).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
			ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Center).Children(func() {
				frostui.IconTile(c, "network-server", frostui.Accent, 44)
				ui.Column(c).Gap(2).Children(func() {
					frostui.Title(c, "Connect to pocketd")
					frostui.Muted(c, "Your agents' service, on your desktop.")
				})
			})
			frostui.Body(c, "On the service host, run pocketd device add NAME, then paste the service URL and the device token it printed. PocketGo keeps the token in your desktop's keyring.").Wrap()
			locked := s.busy || s.confirm != ""
			frostui.Label(c, "Service URL")
			url := ui.TextInput(c, &s.server).Placeholder("https://pocketd.example.ts.net:8443").
				Label("Service URL").Disabled(locked)
			if s.server == "" {
				url.AutoFocus()
			}
			frostui.Label(c, "Device token")
			token := ui.TextInput(c, &s.token).Placeholder("pgd_…").Password().Label("Device token").Disabled(locked)
			if s.server != "" {
				token.AutoFocus()
			}
			if s.err != "" {
				frostui.Problem(c, s.err).Wrap()
			}
			if s.confirm != "" {
				frostui.Problem(c, "This service uses plain HTTP to a host outside this computer and your tailnet: the device token would travel in clear text. Use https://, or the service's tailnet address.").Wrap()
			}
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				if s.confirm != "" {
					ui.Spacer(c)
					if ui.Button(c, "Cancel").Clicked() {
						s.confirm = ""
					}
					if ui.PrimaryButton(c, "Connect Anyway").Clicked() {
						s.confirmed, s.confirm = s.confirm, ""
						a.startSignIn()
					}
					return
				}
				if s.busy {
					ui.Spinner(c).Label("Signing in")
					frostui.Muted(c, "Signing in…")
				}
				ui.Spacer(c)
				ready := strings.TrimSpace(s.server) != "" && strings.TrimSpace(s.token) != "" && !s.busy
				if (ui.PrimaryButton(c, "Connect").Disabled(!ready).Clicked() || url.Submitted() || token.Submitted()) && ready {
					a.startSignIn()
				}
			})
		})
	})
}

// startSignIn checks the form before anything leaves the device: the URL's
// shape, and the owner's word before a token goes out over plain HTTP
// (state.NeedsHTTPWarning).
func (a *App) startSignIn() {
	s := &a.signIn
	if s.busy {
		return
	}
	server, err := state.ParseServer(s.server)
	if err != nil {
		s.err = capitalize(err.Error())
		return
	}
	token := strings.TrimSpace(s.token)
	if token == "" {
		s.err = "Paste the device token that pocketd device add printed."
		return
	}
	if state.NeedsHTTPWarning(server) && s.confirmed != server {
		s.confirm, s.err = server, ""
		return
	}
	s.server, s.busy, s.err = server, true, ""
	a.st.SignIn(server, token, func(err error) {
		s.busy = false
		if err != nil {
			s.err = capitalize(err.Error())
			return
		}
		s.token, s.confirmed = "", ""
		if err := a.opts.Creds.SetToken(server, token); err != nil {
			a.toast("Signed in, but the keyring would not keep the token: you will sign in again next time")
			a.console.Add(frostui.LevelWarn, "app", "Keeping the token: "+err.Error())
		}
		a.settings.Server = server
		a.saveSettings()
		a.goInbox()
	})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func itoa(n int) string { return strconv.Itoa(n) }
