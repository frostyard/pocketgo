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

// A session's transcript that did not load says so with Try Again, and
// one that loaded and then failed to load again says it may be out of
// date (TestAStaleTranscriptSaysSo holds this for the MyGo client).
func TestATranscriptThatFailedToLoadSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work"})
	app := signedInApp(t, s, 1230, 800)
	s.FailOnce(http.MethodGet, "/v1/sessions/4", http.StatusBadGateway)
	app.Press("sidebar-panel >> text:Editor")
	app.WaitFor("text:Loading the session failed", 5*time.Second)
	// The banner says it; a toast would say the same over its button.
	if app.Exists("text:PocketGo error") {
		t.Error("a load whose failure shows in place also raised a toast")
	}
	app.Press("role:button Try Again")
	app.WaitForGone("text:Loading the session failed", 5*time.Second)
	app.WaitFor("label:Message", 5*time.Second)

	s.FailOnce(http.MethodGet, "/v1/sessions/4", http.StatusBadGateway)
	app.Press("label:Refresh")
	app.WaitFor("text:This transcript may be out of date: loading it again failed", 5*time.Second)
	app.Press("role:button Try Again")
	app.WaitForGone("text:This transcript may be out of date", 5*time.Second)
}

// A run that failed says so at the transcript's end.
func TestARunThatFailedSaysSo(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work"})
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Editor")
	app.WaitFor("label:Message", 5*time.Second)
	s.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 4, Failure: "the provider is overloaded", CompactionFailure: "no room left"}})
	app.WaitFor("text:The run failed: the provider is overloaded", 5*time.Second)
	app.WaitFor("text:Compacting the context failed: no room left", 5*time.Second)
}

// A search that failed says so, and never that nothing was found; a
// search that found nothing says that.
func TestASearchThatFailedSaysSo(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 800)
	app.Press("sidebar-panel >> text:Search")
	app.WaitFor("text:Search every session", 5*time.Second)
	s.FailOnce(http.MethodGet, proto.PathSearch, http.StatusInternalServerError)
	app.Type("label:Search all sessions", "capacitor")
	app.Press("text:Search sessions")
	app.WaitFor("text:Searching failed", 5*time.Second)
	if app.Exists("text:Nothing found") {
		t.Error("a search that failed says nothing was found")
	}
	app.Press("role:button Try Again")
	app.WaitFor("text:Nothing found", 5*time.Second)
	app.WaitFor("text:No session says “capacitor”.", 5*time.Second)
}

// A Terminal place whose shells failed to load says so.
func TestTerminalsThatFailedToLoadSaySo(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 800)
	// The place loads its shells again as it opens.
	s.FailOnce(http.MethodGet, proto.PathTerminals, http.StatusBadGateway)
	app.Press("sidebar-panel >> text:Terminal")
	app.WaitFor("text:Loading the terminals failed", 5*time.Second)
	app.Press("role:button Try Again")
	app.WaitFor("text:Start a terminal to open a shell on the service host.", 5*time.Second)
}
