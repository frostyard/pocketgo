package nsgoclient

import (
	"strings"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

// signIn is the screen a device shows before it is signed in to pocketd,
// with the MyGo client's texts (desktop/internal/app/signin.go): which
// field is missing, a malformed address, why a sign-in failed and why
// pocketd signed the device out, each on the screen (c.signInProblem).
// The keyboard starts in the address, or in the token when the address
// is filled, and Return in either field connects.
func (c *client) signIn(cx *core.Scope) core.Node {
	last, _ := cx.Storage().GetItem("last-server")
	if last == "" {
		last = defaultServiceURL()
	}
	address := core.NewSignal(cx, last)
	token := core.NewSignal(cx, "")
	busy := core.NewSignal(cx, false)
	problem := c.signInProblem
	// The fields wait while a sign-in runs and while the plain-HTTP
	// question shows, as the MyGo client's do: a Return in a field then
	// asks nothing more.
	locked := func() bool { return busy.Get() || c.asking.Get() != nil }
	submit := func() {
		if locked() {
			return
		}
		server, err := state.ParseServer(address.Get())
		if err != nil {
			problem.Set(sentence(err.Error()))
			return
		}
		secret := strings.TrimSpace(token.Get())
		if secret == "" {
			problem.Set("Paste the device token that pocketd device add printed.")
			return
		}
		connect := func() {
			c.notices.Set(nil)
			problem.Set("")
			busy.Set(true)
			c.generation++
			c.store.SignIn(server, secret, func(err error) {
				busy.Set(false)
				if err != nil {
					problem.Set(sentence(err.Error()))
					return
				}
				token.Set("")
				c.keep(server, secret)
				c.changed()
			})
		}
		if state.NeedsHTTPWarning(server) {
			problem.Set("")
			c.confirm("Connect over plain HTTP?", plainHTTPWarning(server), "Connect anyway", connect)
			return
		}
		connect()
	}
	colors := ui.ColorsOf(cx)
	muted := func(s string) core.Node {
		return core.Text{Content: s, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)}
	}
	return core.Scroll{Style: ui.FillRest, Children: []core.Node{core.View{
		Style: core.Style{Width: core.Pct(100), MaxWidth: core.Pt(500), AlignSelf: core.AlignCenter, Margin: core.Edges{Top: 64}, Padding: core.All(workspaceui.SpaceXL)},
		Children: []core.Node{workspaceui.Card{Children: []core.Node{
			core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceM, Margin: core.Edges{Bottom: workspaceui.SpaceXS}}, Children: []core.Node{
				core.Icon{SVG: lucide.Server(), Style: core.Style{Width: core.Pt(32), Height: core.Pt(32)}, StyleFunc: ui.Colored(colors.Accent)},
				core.View{Style: core.Style{Gap: workspaceui.SpaceXS, FlexShrink: 1}, Children: []core.Node{
					core.Text{Content: "Connect to pocketd", Style: ui.TypeHeading}, muted("Your agents' service, on this device."),
				}},
			}},
			body(c.signInGuide()),
			core.Text{Content: "Service URL", Style: ui.TypeLabel},
			ui.Input{Label: "Service URL", Placeholder: "e.g. http://pocketd.example.ts.net:8788", Value: address.Get, OnChange: address.Set, Disabled: locked,
				AutoFocus: last == "", OnSubmit: func(string) { submit() }},
			core.Text{Content: "Device token", Style: ui.TypeLabel},
			ui.Input{Label: "Device token", Placeholder: "pgd_…", Masked: true, Value: token.Get, OnChange: token.Set, Disabled: locked,
				AutoFocus: last != "", OnSubmit: func(string) { submit() }},
			core.Dynamic(func(*core.Scope) core.Node {
				if p := problem.Get(); p != "" {
					return c.problemBanner(workspaceui.Danger, p, nil)
				}
				return nil
			}),
			core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS, Margin: core.Edges{Top: workspaceui.SpaceXS}}, Children: []core.Node{
				core.Show{When: busy.Get, Then: func(*core.Scope) core.Node { return muted("Signing in…") }},
				core.View{Style: core.Style{FlexGrow: 1}},
				ui.Button{Text: "Connect", Busy: busy.Get, Disabled: busy.Get, OnPress: submit},
			}},
		}}},
	}}}
}

// signInGuide says where the address and the token come from, and where
// the token is kept: in the device's keyring when the client has a store
// for it (platformAccounts), else nowhere, as in a browser.
func (c *client) signInGuide() string {
	guide := "Enter the address pocketd serves on, such as its tailnet name and port. For the device token, run pocketd device add NAME on the service host and paste only the pgd_… value it prints. "
	if c.creds == nil {
		return guide + "PocketGo does not keep the token here: you sign in again next time."
	}
	return guide + "PocketGo keeps the token in this device's keyring."
}

// plainHTTPWarning is the question's text before the device token goes to
// server over plain HTTP (state.NeedsHTTPWarning): the address it goes
// to, and the MyGo client's warning.
func plainHTTPWarning(server string) string {
	return "The device token will travel to " + server + " without HTTPS. This service uses plain HTTP, and its host is neither this computer nor a tailnet name (*.ts.net): the device token could travel in clear text. Use https://, or the service's MagicDNS name."
}

// keyringNote is the title of the note after a sign-in whose token the
// keyring would not keep (keep).
const keyringNote = "Signed in, but the keyring would not keep the token"

// forgetProblem says what a token the keyring would not forget means
// (forget): connected-server is gone already, so no start signs in with
// it.
func forgetProblem(err error) string {
	return err.Error() + ". PocketGo will not sign in with it again; remove it with your keyring's app, or revoke the device with pocketd device revoke."
}

// sentence starts a message of the rules as a sentence, and keeps
// pocketd's name as it is written.
func sentence(s string) string {
	if strings.HasPrefix(s, "pocketd ") {
		return s
	}
	return capitalize(s)
}
