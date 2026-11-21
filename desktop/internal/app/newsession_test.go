package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// repoProfiles gives the fake pocketd Duncan, on chairlift and snow, and
// Teg, on chairlift in a checkout of its own (ADR-0027).
func (x *testApp) repoProfiles() {
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan Idaho", Role: proto.RoleAgent, Model: opus, Runs: opus,
		Home: "/var/home/team/agents/duncan", Cwd: "/var/home/team/agents/duncan", Repositories: []proto.Repository{
			{Name: "frostyard/chairlift", Folder: "/var/home/team/projects/frostyard/chairlift"},
			{Name: "frostyard/snow", Folder: "/var/home/team/projects/frostyard/snow"}}})
	x.srv.AddProfile(proto.Profile{ID: "teg", Name: "Teg", Role: proto.RoleAgent, Model: opus, Runs: opus, Repositories: []proto.Repository{
		{Name: "frostyard/chairlift", Folder: "/var/home/team/teg/chairlift"}}})
}

// A repository under the profile fills in its folder; in a new worktree,
// the folder shows the worktree pocketd makes, read-only, with its branch
// under it, and the session starts there (ADR-0027, the owner's layout).
func TestNewSessionOnARepositoryInAWorktreeSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.repoProfiles()
	x.connect(t)
	x.click(t, "New Session")
	f := x.a.newSession
	f.profile = "Duncan Idaho · agent"
	x.settle()
	if got := f.repoLabels(); !slices.Equal(got, []string{state.NoRepository, "frostyard/chairlift", "frostyard/snow"}) {
		t.Fatalf("Duncan's repositories: %q", got)
	}
	x.hasAll(t, "Repository", "None")
	if x.tt.HasText("In a new worktree") {
		t.Error("the worktree switch shows with no repository chosen")
	}
	f.folder.Repository = "frostyard/chairlift"
	x.settle()
	if f.folder.Cwd != "/var/home/team/projects/frostyard/chairlift" {
		t.Fatalf("the folder after chairlift: %q", f.folder.Cwd)
	}
	x.hasAll(t, "frostyard/chairlift", "In a new worktree")
	x.snapshot(t, "new-session-repository")
	f.title = "Fix CI"
	x.click(t, "In a new worktree")
	if !f.folder.Worktree {
		t.Fatal("the switch did not turn on")
	}
	request := f.request()
	request.RequestID = x.a.st.NextCreateID(request)
	path, branch, ok := state.WorktreePreview(request)
	if !ok || path != "/var/home/team/projects/frostyard/chairlift--duncan-fix-ci-"+proto.WorktreeSuffix(request.RequestID) {
		t.Fatalf("preview %q %q %v", path, branch, ok)
	}
	x.hasAll(t, path, "(a new worktree on "+branch+")")
	if _, editable := x.tt.AccessByRole(ui.RoleTextField, "Working directory"); editable {
		t.Error("the worktree's folder is editable")
	}
	x.snapshot(t, "new-session-worktree")
	x.click(t, "Start Session")
	x.until(t, func() bool {
		return x.a.place == placeSession && x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded
	})
	s, _ := x.a.st.Data.Session(x.a.st.Data.OpenID())
	if s.Cwd != path || s.Profile != "duncan" {
		t.Fatalf("opened %+v, want it in %s", s, path)
	}
	posts := x.srv.RequestsTo(http.MethodPost, proto.PathSessions)
	var sent proto.CreateSession
	if err := json.Unmarshal(posts[len(posts)-1].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.RequestID != request.RequestID || sent.Repository != "frostyard/chairlift" || !sent.Worktree ||
		sent.Cwd != "/var/home/team/projects/frostyard/chairlift" || sent.Title != "Fix CI" {
		t.Errorf("sent %+v, shown with the id %s", sent, request.RequestID)
	}
}

// A folder typed by hand ends the repository choice; with no profile, a
// repository two profiles name in two folders is offered once a folder.
func TestATypedFolderEndsTheRepositoryChoice(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.repoProfiles()
	x.connect(t)
	x.click(t, "New Session")
	f := x.a.newSession
	if got := f.repoLabels(); !slices.Equal(got, []string{state.NoRepository,
		"frostyard/chairlift · /var/home/team/projects/frostyard/chairlift", "frostyard/chairlift · /var/home/team/teg/chairlift", "frostyard/snow"}) {
		t.Fatalf("the owner's repositories: %q", got)
	}
	f.folder.Repository = "frostyard/chairlift · /var/home/team/teg/chairlift"
	x.settle()
	if f.folder.Cwd != "/var/home/team/teg/chairlift" {
		t.Fatalf("the folder: %q", f.folder.Cwd)
	}
	x.typeInto(t, "Working directory", "-old")
	if f.folder.Repository != state.NoRepository || f.folder.Cwd != "/var/home/team/teg/chairlift-old" || x.tt.HasText("In a new worktree") {
		t.Fatalf("after typing: %q in %q", f.folder.Repository, f.folder.Cwd)
	}
}
