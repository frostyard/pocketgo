package nsgoclient

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestNewSessionShowsItsWorktreeBeforeItGoes checks New session on a
// repository in a new worktree: the worktree's folder and branch show
// before the request goes, in place of the folder's field, and the request
// goes with the id they were named from (ADR-0027). It holds for the NSGo
// client what TestNewSessionOnARepositoryInAWorktreeSnapshot holds for
// the MyGo client.
func TestNewSessionShowsItsWorktreeBeforeItGoes(t *testing.T) {
	s := pocketdtest.New()
	model := proto.ModelRef{Provider: "test", ID: "default"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Default"})
	s.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan Idaho", Role: proto.RoleAgent, Model: model, Runs: model,
		Home: "/var/home/team/agents/duncan", Cwd: "/var/home/team/agents/duncan", Repositories: []proto.Repository{
			{Name: "frostyard/chairlift", Folder: "/var/home/team/projects/frostyard/chairlift"},
			{Name: "frostyard/snow", Folder: "/var/home/team/projects/frostyard/snow"}}})
	app := signedInApp(t, s, 1230, 900)
	app.Press("label:New session")
	app.WaitFor("text:No profile", 5*time.Second)
	app.Press("text:No profile")
	app.Press("text:Duncan Idaho · agent")
	app.Type("label:Title", "Fix CI")
	app.Press("text:None")
	app.Press("text:frostyard/chairlift")
	app.WaitUntil(5*time.Second, func() bool {
		return app.Find("label:Working directory").Text == "/var/home/team/projects/frostyard/chairlift"
	})
	app.Press("label:New worktree")
	const start = "/var/home/team/projects/frostyard/chairlift--duncan-fix-ci-"
	// A selector that starts with a slash is a pattern, so the folder is
	// looked for by its end.
	app.WaitFor("text:frostyard/chairlift--duncan-fix-ci-", 5*time.Second)
	app.WaitFor("text:(a new worktree on ", 5*time.Second)
	if app.Exists("label:Working directory") {
		t.Error("the worktree's folder is a field to type in")
	}
	app.Press("text:Start session")
	app.WaitFor("label:Message", 5*time.Second)
	posts := s.RequestsTo(http.MethodPost, proto.PathSessions)
	var sent proto.CreateSession
	if len(posts) == 0 || json.Unmarshal(posts[len(posts)-1].Body, &sent) != nil {
		t.Fatalf("no new session went: %d requests", len(posts))
	}
	if !sent.Worktree || sent.Repository != "frostyard/chairlift" || sent.Cwd != "/var/home/team/projects/frostyard/chairlift" || sent.Title != "Fix CI" {
		t.Errorf("sent %+v", sent)
	}
	// The folder shown was named from the id the request went with.
	if created, ok := s.Session(1); !ok || !strings.HasPrefix(created.Session.Cwd, start) || !strings.HasSuffix(created.Session.Cwd, proto.WorktreeSuffix(sent.RequestID)) {
		t.Errorf("the session is in %q, want the worktree shown, of request %s", created.Session.Cwd, sent.RequestID)
	}
}

// TestNewSessionSaysWhatAStandingProfileOpens checks a standing profile:
// the form says it opens the profile's one session, asks for no folder or
// title, and its button opens.
func TestNewSessionSaysWhatAStandingProfileOpens(t *testing.T) {
	s := pocketdtest.New()
	model := proto.ModelRef{Provider: "test", ID: "default"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Default"})
	s.AddProfile(proto.Profile{ID: "leto", Name: "Leto", Role: proto.RoleSupervisor, Standing: true, Model: model, Runs: model, Cwd: "/work"})
	app := signedInApp(t, s, 1230, 900)
	app.Press("label:New session")
	app.WaitFor("text:No profile", 5*time.Second)
	app.Press("text:No profile")
	app.Press("text:Leto · supervisor")
	app.WaitFor("text:Leto keeps one standing session: this opens it, with its own folder, model and title.", 5*time.Second)
	app.WaitFor("text:Open session", 5*time.Second)
	if app.Exists("label:Title") || app.Exists("label:Working directory") {
		t.Error("a standing profile's form asks for a title or a folder")
	}
}

// TestNewSessionSaysWhenNoModelRunsAndWhenTheyFailedToLoad checks the
// notes under the model's choice, and Reload Models.
func TestNewSessionSaysWhenNoModelRunsAndWhenTheyFailedToLoad(t *testing.T) {
	s := pocketdtest.New()
	app := signedInApp(t, s, 1230, 900)
	app.Press("label:New session")
	app.WaitFor("text:pocketd runs no model now: sign it in to a provider (pocketd login PROVIDER), then reload.", 5*time.Second)
	s.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "test", ID: "default"}, Name: "Default"})
	s.FailOnce(http.MethodGet, proto.PathModels, http.StatusBadGateway)
	app.Press("role:button Try Again")
	app.WaitFor("text:Loading the models failed", 5*time.Second)
	app.Press("role:button Try Again")
	app.WaitForGone("text:Loading the models failed", 5*time.Second)
	app.WaitForGone("text:pocketd runs no model now", 5*time.Second)
}

// TestNewSessionIsADialogOverThePlaceShown checks New session's place: a
// dialog over the place the owner was in, which stays, opened by the
// sidebar's button and by its key, with Cancel. A form opened again is
// empty, a start that failed says so in the dialog and keeps the form, and
// the way back from the new session goes to the place it was started
// from. It holds for the NSGo client what TestNewSessionSnapshot holds
// for the MyGo client's dialog.
func TestNewSessionIsADialogOverThePlaceShown(t *testing.T) {
	s := pocketdtest.New()
	model := proto.ModelRef{Provider: "test", ID: "default"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Default"})
	s.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan Idaho", Role: proto.RoleAgent, Model: model, Runs: model,
		Home: "/var/home/team/agents/duncan", Cwd: "/var/home/team/agents/duncan"})
	app := signedInApp(t, s, 1230, 900)
	app.Press("text:Work board")
	app.WaitFor("text:No thread is on the desk", 5*time.Second)
	// One thing only is named New session, the sidebar's button: a press
	// of a name that two things have fails.
	app.Press("label:New session")
	app.WaitFor("text:An agent session on the service host, in a working directory there.", 5*time.Second)
	// The place under the dialog stays.
	app.Find("text:No thread is on the desk")
	app.Type("label:Title", "Fix CI")
	app.Press("role:button Cancel")
	app.WaitForGone("text:An agent session on the service host, in a working directory there.", 5*time.Second)
	app.Find("text:No thread is on the desk")

	// Its key opens it again, with an empty form.
	app.Key("mod+n")
	app.WaitFor("text:An agent session on the service host, in a working directory there.", 5*time.Second)
	if got := app.Find("label:Title").Text; got != "" {
		t.Errorf("a form opened again holds the title %q", got)
	}
	app.Press("text:No profile")
	app.Press("text:Duncan Idaho · agent")
	app.Type("label:Title", "Fix CI")
	app.Screenshot("new-session")

	// A start that failed says so in the dialog, and the form stays.
	s.FailOnce(http.MethodPost, proto.PathSessions, http.StatusBadGateway)
	app.Press("text:Start session")
	app.WaitFor("text:Internal: made to fail", 5*time.Second)
	if app.Exists("text:PocketGo error") {
		t.Error("a failed start shows a notice as well as the dialog's line")
	}
	if got := app.Find("label:Title").Text; got != "Fix CI" {
		t.Errorf("after a failed start the title is %q", got)
	}
	app.Press("text:Start session")
	app.WaitFor("label:Message", 5*time.Second)
	app.WaitForGone("text:An agent session on the service host, in a working directory there.", 5*time.Second)
	if posts := s.RequestsTo(http.MethodPost, proto.PathSessions); len(posts) != 2 {
		t.Errorf("%d requests for a new session, want the failed one and the next", len(posts))
	}
	// The way back goes to the place the session was started from.
	app.Press("label:Back")
	app.WaitFor("text:No thread is on the desk", 5*time.Second)
}

// TestNewSessionFitsASmallWindow checks the dialog in a window 760 by 480,
// the MyGo client's smallest: its fields scroll, and its buttons stay
// inside the window. It holds for the NSGo client what
// TestNewSessionFitsTheSmallestWindowSnapshot holds for the MyGo client.
func TestNewSessionFitsASmallWindow(t *testing.T) {
	const width, height = 760, 480
	s := pocketdtest.New()
	model := proto.ModelRef{Provider: "test", ID: "default"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Default"})
	app := signedInApp(t, s, width, height)
	app.Key("mod+n")
	app.WaitFor("text:An agent session on the service host, in a working directory there.", 5*time.Second)
	start := app.Find("text:Start session").Bounds()
	if bottom := start.Origin.Y + start.Size.Height; bottom > height {
		t.Errorf("Start session ends at %v in a window %v high", bottom, height)
	}
	app.Screenshot("new-session-small")
}
