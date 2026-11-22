package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/app"
)

type fakeRaiser struct {
	minimized bool
	log       *calls
}

func (w fakeRaiser) IsMinimized() bool { return w.minimized }
func (w fakeRaiser) Restore()          { *w.log = append(*w.log, "restore") }
func (w fakeRaiser) Show()             { *w.log = append(*w.log, "show") }

type fakeOpener struct{ log *calls }

func (a fakeOpener) OpenNotification(id string) { *a.log = append(*a.log, "open "+id) }

// A click brings the window up, out of the Dock when minimized, then opens
// what the notification is about; before the window exists it still opens
// (ADR-0049).
func TestAClickRaisesTheWindowAndOpens(t *testing.T) {
	var log calls
	clicked(fakeRaiser{minimized: true, log: &log}, fakeOpener{&log}, "n1")
	clicked(fakeRaiser{log: &log}, fakeOpener{&log}, "n2")
	clicked(nil, fakeOpener{&log}, "n3")
	if want := (calls{"restore", "show", "open n1", "show", "open n2", "open n3"}); !slices.Equal(log, want) {
		t.Errorf("clicks did %q, want %q", log, want)
	}
}

// A notification shows off the window's thread, and its outcome comes back
// through post.
func TestANotificationShowsOffTheWindowsThread(t *testing.T) {
	posted := make(chan func(), 1)
	shown := make(chan app.Notification, 1)
	release := make(chan struct{})
	failed := errors.New("not allowed")
	notify := notifier(func(f func()) { posted <- f }, func(n app.Notification) error {
		shown <- n
		<-release // the system waits for the owner's answer
		return failed
	})
	var got error
	notify(app.Notification{ID: "n1", Title: "A2 asks you"}, func(err error) { got = err })
	if n := <-shown; n.ID != "n1" {
		t.Fatalf("shown %+v", n)
	}
	close(release)
	(<-posted)()
	if !errors.Is(got, failed) {
		t.Errorf("done got %v, want the system's error", got)
	}
}
