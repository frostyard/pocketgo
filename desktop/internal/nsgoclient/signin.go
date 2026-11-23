package nsgoclient

import (
	"strings"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

func (c *client) signIn(cx *core.Scope) core.Node {
	last, _ := cx.Storage().GetItem("last-server")
	if last == "" {
		last = defaultServiceURL()
	}
	address := core.NewSignal(cx, last)
	token := core.NewSignal(cx, "")
	busy := core.NewSignal(cx, false)
	submit := func() {
		if busy.Get() {
			return
		}
		server, err := state.ParseServer(address.Get())
		if err != nil {
			c.showNotice(ui.ToastError, "Couldn’t sign in", err.Error())
			return
		}
		secret := strings.TrimSpace(token.Get())
		if secret == "" {
			c.showNotice(ui.ToastWarning, "Device token required", "Enter the token printed by pocketd device add.")
			return
		}
		connect := func() {
			c.notices.Set(nil)
			busy.Set(true)
			c.generation++
			c.store.SignIn(server, secret, func(err error) {
				busy.Set(false)
				if err != nil {
					c.showNotice(ui.ToastError, "Couldn’t sign in", err.Error())
					return
				}
				token.Set("")
				c.keep(server, secret)
				c.changed()
			})
		}
		if state.NeedsHTTPWarning(server) {
			c.confirm("Connect over plain HTTP?", "The device token will travel to "+server+" without HTTPS.", "Connect anyway", connect)
			return
		}
		connect()
	}
	return core.Scroll{Style: ui.FillRest, Children: []core.Node{core.View{
		Style: core.Style{Width: core.Pct(100), MaxWidth: core.Pt(458), AlignSelf: core.AlignCenter, Margin: core.Edges{Top: 64}, Padding: core.All(24), Gap: 13},
		Children: []core.Node{
			core.Icon{SVG: lucide.MessagesSquare(), Style: core.Style{Width: core.Pt(38), Height: core.Pt(38)}, StyleFunc: ui.Colored(ui.ColorsOf(cx).Accent)},
			core.Text{Content: "PocketGo", Style: ui.TypeHeading}, core.Text{Content: "Connect to pocketd with a device token from your service host."},
			core.Text{Content: "Service URL", Style: ui.TypeLabel}, ui.Input{Label: "Service URL", Placeholder: "https://pocketd.example.ts.net", Value: address.Get, OnChange: address.Set, Disabled: busy.Get},
			core.Text{Content: "Device token", Style: ui.TypeLabel}, ui.Input{Label: "Device token", Masked: true, Value: token.Get, OnChange: token.Set, Disabled: busy.Get, OnSubmit: func(string) { submit() }},
			ui.Button{Text: "Connect", Busy: busy.Get, Disabled: busy.Get, OnPress: submit},
		},
	}}}
}
