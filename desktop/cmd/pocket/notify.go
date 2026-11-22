package main

import (
	"github.com/egoist/mygo"

	"github.com/frostyard/pocketgo/desktop/internal/app"
)

// notifier shows the app's notifications (ADR-0049): show runs off the
// window's thread, since the system may first ask the owner to allow
// notifications and Show waits for the answer, and done goes back to the
// window's thread through post.
func notifier(post func(func()), show func(app.Notification) error) func(app.Notification, func(error)) {
	return func(n app.Notification, done func(error)) {
		go func() {
			err := show(n)
			post(func() { done(err) })
		}()
	}
}

// showNotification shows n as a system notification with MyGo.
func showNotification(n app.Notification) error {
	return mygo.NewNotification(mygo.NotificationOptions{ID: n.ID, Title: n.Title, Subtitle: n.Subtitle, Body: n.Body}).Show()
}

// raiser is the window as a click on a notification brings it up.
type raiser interface {
	IsMinimized() bool
	Restore()
	Show()
}

// opener is the app as a click on a notification opens what it is about.
type opener interface{ OpenNotification(id string) }

// clicked brings the window up, out of the Dock or the taskbar when it is
// minimized, and opens what notification id is about, on the window's
// thread. win is nil while the window does not exist yet.
func clicked(win raiser, a opener, id string) {
	if win != nil {
		if win.IsMinimized() {
			win.Restore()
		}
		win.Show()
	}
	a.OpenNotification(id)
}
