package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// Notification is a system notification, as the program shows it
// (ADR-0049): its id comes back with a click (App.OpenNotification).
type Notification struct {
	ID, Title, Subtitle, Body string
}

// SetFocused tells the app whether its window has the keyboard: a
// notification shows only while it does not (state.ShowNotice). The
// program calls it on the UI thread as the window gains and loses focus.
func (a *App) SetFocused(focused bool) { a.focused = focused }

// notifyPrefs are the notification switches for the service signed in to.
func (a *App) notifyPrefs() state.NotifyPrefs {
	return state.NotifyPrefs(a.settings.Notify[a.st.Conn.Server])
}

// setNotifyPrefs keeps p as the switches for the service signed in to.
func (a *App) setNotifyPrefs(p state.NotifyPrefs) {
	server := a.st.Conn.Server
	if server == "" || p == a.notifyPrefs() {
		return
	}
	if a.settings.Notify == nil {
		a.settings.Notify = map[string]config.Notify{}
	}
	if p == (state.NotifyPrefs{}) {
		delete(a.settings.Notify, server)
	} else {
		a.settings.Notify[server] = config.Notify(p)
	}
	_ = a.saveSettings()
}

// notify shows notice n, as the store decided it, where the platform
// shows notifications and state.ShowNotice says it shows now.
func (a *App) notify(n state.Notice) {
	if a.opts.Notify == nil || !state.Notifies(a.opts.Platform) || !state.ShowNotice(n, a.notifyPrefs(), a.focused) {
		return
	}
	a.opts.Notify(Notification{ID: state.NoticeID(a.st.Conn.Server, n.Target), Title: n.Title, Subtitle: n.Subtitle, Body: n.Body},
		func(err error) {
			if err != nil {
				a.console.Add(frostui.LevelWarn, "app", "Showing a notification: "+err.Error())
			}
		})
}

// keepNotices keeps the keys of what the store notified for server, for
// the next run.
func (a *App) keepNotices(server string, keys []string) {
	if a.settings.Notified == nil {
		a.settings.Notified = map[string][]string{}
	}
	a.settings.Notified[server] = keys
	_ = a.saveSettings()
}

// updateBadge shows the Inbox's count on the app's icon (state.InboxCount)
// when it changed: none while signed out.
func (a *App) updateBadge() {
	if a.opts.SetBadge == nil {
		return
	}
	n := 0
	if a.st.SignedIn() {
		n = state.InboxCount(a.st.Data.Inbox())
	}
	if n != a.badge {
		a.badge = n
		a.opts.SetBadge(n)
	}
}

// OpenNotification opens what the clicked notification id is about: the
// Inbox at its card, or its session. One of another service than the one
// signed in to opens nothing; one clicked while the kept sign-in still
// starts opens once it is connected (Start).
func (a *App) OpenNotification(id string) {
	server, target, ok := state.ParseNoticeID(id)
	if !ok {
		return
	}
	if !a.st.SignedIn() {
		if server == a.settings.Server {
			a.pendingNotice = &target
		}
		return
	}
	if server != a.st.Conn.Server {
		a.console.Add(frostui.LevelInfo, "app", "A notification of "+host(server)+" was clicked; PocketGo is signed in to "+host(a.st.Conn.Server))
		return
	}
	a.openNotice(target)
}

// openNotice opens target: its session, or the Inbox scrolled to its card.
func (a *App) openNotice(t state.NoticeTarget) {
	if t.Session != 0 {
		a.openSession(t.Session)
		return
	}
	a.goPlaceNamed(state.PlaceInbox)
	a.phone.tab = state.TabInbox
	a.reveal = t.Card
}

// revealed scrolls el, the Inbox's card named card, into view in the
// frame after a click on its notification asked for it.
func (a *App) revealed(el ui.Element, card string) ui.Element {
	if a.reveal == "" || a.reveal != card || a.place != placeInbox {
		return el
	}
	a.reveal = ""
	return el.ScrollIntoView(ui.ScrollIntoViewOptions{Block: ui.ScrollStart})
}

// notificationsPage is the Settings window's Notifications page: the
// switches for the service signed in to, on by default.
func (a *App) notificationsPage() frostui.SettingsPage {
	p := a.notifyPrefs()
	a.prefs.notifyOn = !p.Off
	kinds := make([]frostui.Setting, len(state.NoticeKinds))
	for i, k := range state.NoticeKinds {
		a.prefs.notifyKinds[i] = p.KindOn(k)
		kinds[i] = frostui.Setting{Title: k.Title(), Subtitle: k.Subtitle(), Switch: &a.prefs.notifyKinds[i], Disabled: p.Off,
			OnChange: func() { a.setNotifyPrefs(a.notifyPrefs().WithKind(k, a.prefs.notifyKinds[i])) }}
	}
	note := "While PocketGo's window is not in front, it tells you when something new waits for you on " + host(a.st.Conn.Server) + "."
	if state.BadgesIcon(a.opts.Platform) {
		note += " Its icon counts what waits in the Inbox."
	}
	return frostui.SettingsPage{ID: "notifications", Title: "Notifications", Icon: "pocket-bell",
		Keywords: "notify notification alert badge dock ask question approval pull request plan lesson run",
		Groups: []frostui.SettingsGroup{
			{Note: note,
				Settings: []frostui.Setting{{Title: "Notifications", Subtitle: "Each thing notifies once", Switch: &a.prefs.notifyOn,
					OnChange: func() {
						p := a.notifyPrefs()
						p.Off = !a.prefs.notifyOn
						a.setNotifyPrefs(p)
					}}}},
			{Title: "Notify me when", Settings: kinds},
		}}
}
