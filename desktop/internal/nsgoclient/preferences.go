package nsgoclient

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
)

type preferences struct {
	Pinned      state.Pins
	Seen        state.Seen
	Notify      state.NotifyPrefs
	LastSession int64
}

func (c *client) loadJSON(key string, dest any) {
	if s, ok := c.cx.Storage().GetItem(key); ok {
		_ = json.Unmarshal([]byte(s), dest)
	}
}
func (c *client) saveJSON(key string, v any) {
	b, err := json.Marshal(v)
	if err == nil {
		err = c.cx.Storage().SetItem(key, string(b))
	}
	if err != nil {
		c.showError(err.Error())
	}
}
func (c *client) savePrefs() {
	if server := c.store.Conn.Server; server != "" {
		c.saveJSON("preferences/"+server, c.prefs)
	}
}
func (c *client) loadPrefs() {
	c.prefs = preferences{}
	if s := c.store.Conn.Server; s != "" {
		c.loadJSON("preferences/"+s, &c.prefs)
		c.prefs.Seen.Prune(time.Now())
	}
}
func (c *client) notify(n state.Notice) {
	// A platform the client shows no notifications on (state.Notifies).
	if !state.Notifies(c.goos()) {
		return
	}
	if !state.ShowNotice(n, c.prefs.Notify, c.cx.Env().Lifecycle() == core.LifecycleActive) {
		return
	}
	err := c.cx.ShowNotification(core.Notification{ID: state.NoticeID(c.store.Conn.Server, n.Target), Title: n.Title, Body: strings.Join(nonempty(n.Subtitle, n.Body), "\n")}, nil)
	if err != nil {
		c.showError(err.Error())
	}
}
func (c *client) notificationSettings(cx *core.Scope) []ui.SettingsGroup {
	permission := core.NewSignal(cx, "")
	update := func(p core.NotificationPermission) { permission.Set(p.String()) }
	request := func() { c.cx.RequestNotificationPermission(update) }
	save := func(p state.NotifyPrefs) { c.prefs.Notify = p; c.savePrefs(); c.changed() }
	master := ui.Switch{Label: "Show notifications", Checked: func() bool { c.revision.Get(); return !c.prefs.Notify.Off }, OnChange: func(on bool) {
		p := c.prefs.Notify
		p.Off = !on
		save(p)
		if on {
			request()
		}
	}}
	items := []ui.SettingsItem{
		{ID: "enabled", Title: "Show notifications", Control: master},
		{ID: "permission", Title: "System permission", Control: core.Func(func(cx *core.Scope) core.Node {
			cx.NotificationPermission(update)
			return workspaceui.Row(core.Text{ContentFunc: permission.Get}, ui.Button{Text: "Request permission", OnPress: request})
		})},
	}
	var kinds []ui.SettingsItem
	for _, k := range state.NoticeKinds {
		kinds = append(kinds, ui.SettingsItem{ID: strconv.Itoa(int(k)), Title: k.Title(), Description: k.Subtitle(),
			Control: ui.Switch{Label: k.Title(), Checked: func() bool { c.revision.Get(); return c.prefs.Notify.KindOn(k) }, OnChange: func(on bool) { save(c.prefs.Notify.WithKind(k, on)) }}})
	}
	return []ui.SettingsGroup{{Items: items}, {Title: "Notify me about", Items: kinds}}
}

// openNotice opens what the clicked notification id is about: its
// session, or the Inbox at its card. One of another service than the one
// signed in to opens nothing. One clicked while the kept sign-in still
// starts opens once it is connected (restore), in place of the session
// shown last (ADR-0049).
func (c *client) openNotice(id string) {
	server, target, ok := state.ParseNoticeID(id)
	if !ok {
		return
	}
	if !c.store.SignedIn() {
		if kept, _ := c.cx.Storage().GetItem("connected-server"); server == kept {
			c.pendingNotice = &target
		}
		return
	}
	if server != c.store.Conn.Server {
		return
	}
	c.openTarget(target)
}

func (c *client) openTarget(target state.NoticeTarget) {
	if target.Session != 0 {
		c.openSession(target.Session)
	} else {
		c.goTo(destination{page: "Inbox", card: target.Card})
	}
}
