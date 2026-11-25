package nsgoclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// A list that failed to load says so, says what may be missing for it,
// and offers Try Again; no view shows it as empty. These hold for the
// NSGo client what TestASessionListThatFailedToLoadSaysSo,
// TestABoardWhoseThreadsFailedToLoadSaysSo and
// TestAnInboxWhoseLessonsFailedToLoadSaysSo hold for the MyGo client.

func TestASessionListThatFailedToLoadSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.FailOnce(http.MethodGet, proto.PathSessions, http.StatusBadGateway)
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("sidebar-panel >> text:Loading the sessions failed", 5*time.Second)
	if app.Exists("text:No sessions yet. Start one with New session.") {
		t.Error("the sidebar says there is no session though the list did not load")
	}
	// The Inbox says what may wait unseen.
	app.WaitFor("text:A session may wait for your answer to its question that this list does not show.", 5*time.Second)
	app.Press("sidebar-panel >> role:button Try Again")
	app.WaitFor("text:No sessions yet. Start one with New session.", 5*time.Second)
	app.WaitForGone("text:Loading the sessions failed", 5*time.Second)
}

func TestABoardWhoseThreadsFailedToLoadSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.FailOnce(http.MethodGet, proto.PathThreads, http.StatusBadGateway)
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Work board")
	app.WaitFor("text:The threads are not loaded", 5*time.Second)
	app.WaitFor("text:Threads may be missing or out of date.", 5*time.Second)
	if app.Exists("text:No threads yet") {
		t.Error("a board that failed to load says there are no threads")
	}
	app.Press("role:button Try Again")
	app.WaitFor("text:No threads yet", 5*time.Second)
	app.WaitFor("text:No thread is on the desk", 5*time.Second)
	app.WaitForGone("text:Loading the threads failed", 5*time.Second)
}

func TestAnInboxWhoseListsFailedToLoadSaysSo(t *testing.T) {
	for _, list := range []struct{ path, says string }{
		{proto.PathLessons, "Loading the lessons failed"},
		{proto.PathAsks, "Loading the asks failed"},
		{proto.PathPulls, "Loading the pull requests failed"},
	} {
		s := pocketdtest.New()
		s.FailOnce(http.MethodGet, list.path, http.StatusBadGateway)
		app := signedInApp(t, s, 1230, 800)
		app.WaitFor("text:"+list.says, 5*time.Second)
		if app.Exists("text:You’re caught up") {
			t.Errorf("%s: the Inbox says nothing waits though a list did not load", list.path)
		}
		app.Press("role:button Try Again")
		app.WaitFor("text:You’re caught up", 5*time.Second)
		app.WaitForGone("text:"+list.says, 5*time.Second)
	}
}

func TestSchedulesThatFailedToLoadSaySo(t *testing.T) {
	s := pocketdtest.New()
	s.FailOnce(http.MethodGet, proto.PathSchedules, http.StatusBadGateway)
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Work board")
	app.WaitFor("text:Loading the schedules failed", 5*time.Second)
	app.Press("role:button Try Again")
	app.WaitForGone("text:Loading the schedules failed", 5*time.Second)

}

func TestASchedulesFileThatDoesNotGoOutSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.SetSchedules(proto.Schedules{Problem: "line 3: no such agent"})
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Work board")
	app.WaitFor("text:None of your schedules file goes out: line 3: no such agent", 5*time.Second)
}
