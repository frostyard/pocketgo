package nsgoclient

import (
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestASearchShowsItsHitsBySession checks a search's results: each
// session's hits in its card, a hit by its snippet with where it was said,
// and a press that opens the session at the hit (TestTheSearchSnapshot
// holds this for the MyGo client).
func TestASearchShowsItsHitsBySession(t *testing.T) {
	s := pocketdtest.New()
	// Fixed days, so that the kept picture's dates do not move.
	start := time.Date(2026, 3, 4, 9, 30, 0, 0, time.Local)
	s.AddSession(proto.Session{ID: 9, Title: "Snow: first boot hangs", Cwd: "/home/team/projects/snow", State: proto.SessionIdle, CreatedAt: start, UpdatedAt: start.Add(time.Hour)},
		proto.Entry{ID: 100, SessionID: 9, Kind: proto.EntryUser, Text: "What do the boot logs say now?", At: start},
		proto.Entry{ID: 101, SessionID: 9, Kind: proto.EntryAssistant, Text: "Found it: the flux capacitor in the initramfs drifted out of tune.", At: start.Add(time.Minute)})
	s.AddSession(proto.Session{ID: 10, Title: "Port the capacitor tests", Cwd: "/home/team/projects/snow", State: proto.SessionIdle, UpdatedAt: start.Add(48 * time.Hour)})
	app := signedInApp(t, s, 1230, 800)
	app.Key("mod+shift+f")
	app.WaitFor("text:Search every session", 5*time.Second)
	app.Type("label:Search all sessions", "capacitor")
	app.Press("text:Search sessions")
	app.WaitFor("text:2 hits in 2 sessions", 5*time.Second)
	// Each session's title heads its card (and names it in the sidebar).
	for _, title := range []string{"Snow: first boot hangs", "Port the capacitor tests"} {
		if n := len(app.Query("text:" + title)); n < 2 {
			t.Errorf("%q shows %d times, want it in the sidebar and on its card", title, n)
		}
	}
	// A reply's hit says it is a reply, and opens its session at it.
	app.WaitFor("text:Reply · ", 5*time.Second)
	app.Find("text:Title")
	app.Screenshot("search-hits")
	hit := "label:Snow: first boot hangs: Found it: the flux capacitor in the initramfs drifted out of tune."
	app.Press(hit)
	app.WaitFor("label:Message", 5*time.Second)
	app.WaitFor("text:What do the boot logs say now?", 5*time.Second)
}

// TestTheActivityPlaceShowsEachLinesLevelAndCopies checks the Activity
// place: its lines say their level, an error among them, on Ctrl+J, and
// Copy puts them on the clipboard with their time and level.
func TestTheActivityPlaceShowsEachLinesLevelAndCopies(t *testing.T) {
	s := pocketdtest.New()
	s.FailOnce("GET", proto.PathThreads, 502)
	app := signedInApp(t, s, 1230, 800)
	app.Key("mod+j")
	app.WaitFor("label:Copy the activity", 5*time.Second)
	if n := len(app.Query("label:/^info: Signed in to pocketd/")); n != 1 {
		t.Errorf("%d lines say the sign-in at the info level, want 1", n)
	}
	if n := len(app.Query("label:/^error: Loading the threads/")); n != 1 {
		t.Errorf("%d lines say the threads' failure at the error level, want 1", n)
	}
	app.Press("label:Copy the activity")
	if !strings.Contains(app.Clipboard, " info Signed in to pocketd") || !strings.Contains(app.Clipboard, " error Loading the threads") {
		t.Errorf("the copied activity is %q", app.Clipboard)
	}
}
