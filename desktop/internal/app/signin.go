package app

import (
	"fmt"
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
			frostui.Body(c, "Enter the address pocketd serves on, such as its tailnet name and port. For the device token, run pocketd device add NAME on the service host and paste only the pgd_… value it prints. PocketGo keeps the token in your desktop's keyring.").Wrap()
			locked := s.busy || s.confirm != ""
			frostui.Label(c, "Service URL")
			url := ui.TextInput(c, &s.server).Placeholder("e.g. http://pocketd.example.ts.net:8788").
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
				frostui.Problem(c, "This service uses plain HTTP, and its host is neither this computer nor a tailnet name (*.ts.net): the device token could travel in clear text. Use https://, or the service's MagicDNS name.").Wrap()
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
				// Connect waits only for a sign-in that runs. With a field
				// left empty, a click or Return says which one
				// (startSignIn): a disabled button would not say why, and
				// the URL field's placeholder reads like a value.
				if ui.PrimaryButton(c, "Connect").Disabled(s.busy).Clicked() || url.Submitted() || token.Submitted() {
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
		a.settings.LastServer = server
		if err := a.opts.Creds.SetToken(server, token); err != nil {
			// No automatic sign-in at the next start: not to this service,
			// whose token is not kept, nor to the one signed in to before,
			// whose token the keyring may still hold.
			a.settings.Server, a.settings.LastSession = "", 0
			a.toast("Signed in, but the keyring would not keep the token: you will sign in again next time")
			a.console.Add(frostui.LevelWarn, "app", "Keeping the token: "+err.Error())
		} else {
			a.settings.Server = server
		}
		_ = a.saveSettings()
		a.goInbox()
	})
}

// signedOut runs when pocketd refuses the device token: the sign-in ends,
// its token is forgotten, and the sign-in screen shows why.
func (a *App) signedOut(server, reason string) {
	a.resetViews()
	a.signIn.server, a.signIn.err = server, reason
	if problem := signOutProblem(a.forgetSignIn(server)); problem != "" {
		a.signIn.err = reason + " " + problem
	}
}

// signOut ends the sign-in on the owner's word.
func (a *App) signOut() {
	server := a.st.Conn.Server
	a.st.SignOut()
	a.resetViews()
	a.signIn.server, a.signIn.err = server, ""
	if problem := signOutProblem(a.forgetSignIn(server)); problem != "" {
		a.signIn.err = "Signed out, but " + problem
	}
}

// forgetSignIn ends the automatic sign-in to server, then forgets its
// token: the settings change first, and whatever the keyring does, so a
// token it failed to delete is not used at the next start. It returns what
// failed of each step.
func (a *App) forgetSignIn(server string) (saveErr, forgetErr error) {
	a.settings.Server, a.settings.LastSession = "", 0
	if server != "" {
		a.settings.LastServer = server
	}
	saveErr = a.saveSettings()
	if server != "" {
		if forgetErr = a.opts.Creds.Forget(server); forgetErr != nil {
			a.console.Add(frostui.LevelError, "app", "Forgetting the token: "+forgetErr.Error())
		}
	}
	return saveErr, forgetErr
}

// signOutProblem says what a sign-out left behind, "" for nothing. The
// next start signs in by itself only when both steps failed: the settings
// still name the service, and the keyring still holds its token.
func signOutProblem(saveErr, forgetErr error) string {
	switch {
	case saveErr != nil && forgetErr != nil:
		return fmt.Sprintf("signing out did not finish: saving the settings failed (%v) and %v. The next start may sign in again by itself. Sign out again once the keyring and the config folder work, or revoke the device with pocketd device revoke.", saveErr, forgetErr)
	case forgetErr != nil:
		return fmt.Sprintf("%v. PocketGo will not sign in with it again; remove it with your keyring's app, or revoke the device with pocketd device revoke.", forgetErr)
	case saveErr != nil:
		return fmt.Sprintf("saving the settings failed (%v). The token is gone, so the next start cannot sign in by itself.", saveErr)
	}
	return ""
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
