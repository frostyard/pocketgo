package team

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A session on a repository works in the checkout its profile names, or,
// with no profile, the one of the profiles' checkouts the request names
// (rule 6a): pocketd never takes a folder no profile names.
func TestSessionCheckout(t *testing.T) {
	duncan := profiles.Profile{ID: "duncan", Repositories: map[string]string{"frostyard/chairlift": "/srv/chairlift", "frostyard/snow": "/srv/snow"}}
	teg := profiles.Profile{ID: "teg", Repositories: map[string]string{"Frostyard/Chairlift": "/home/teg/chairlift", "frostyard/snow": "/srv/snow"}}
	home := profiles.Profile{ID: "irulan", Home: "/home/irulan"}
	every := []profiles.Profile{duncan, teg, home}
	for _, c := range []struct {
		name       string
		profile    *profiles.Profile
		repository string
		cwd        string
		want       string
		refused    string
	}{
		{name: "the profile's checkout", profile: &duncan, repository: "frostyard/chairlift", want: "/srv/chairlift"},
		{name: "in another case", profile: &duncan, repository: "FROSTYARD/chairlift", want: "/srv/chairlift"},
		{name: "its folder named", profile: &duncan, repository: "frostyard/chairlift", cwd: "/srv/chairlift/", want: "/srv/chairlift"},
		{name: "another folder", profile: &duncan, repository: "frostyard/chairlift", cwd: "/srv/other", refused: "duncan's checkout of frostyard/chairlift is /srv/chairlift, not /srv/other"},
		{name: "a repository it lacks", profile: &duncan, repository: "frostyard/nsl", refused: "does not work on frostyard/nsl; its repositories are frostyard/chairlift, frostyard/snow"},
		{name: "a profile with none", profile: &home, repository: "frostyard/nsl", refused: "irulan works on no repository"},
		{name: "a relative folder", profile: &duncan, repository: "frostyard/chairlift", cwd: "srv/chairlift", refused: "absolute"},
		{name: "the owner's, one folder", repository: "frostyard/snow", want: "/srv/snow"},
		{name: "the owner's, two folders, one named", repository: "frostyard/chairlift", cwd: "/home/teg/chairlift", want: "/home/teg/chairlift"},
		{name: "the owner's, two folders, none named", repository: "frostyard/chairlift", refused: "the profiles name frostyard/chairlift in 2 folders, /home/teg/chairlift, /srv/chairlift: name the cwd"},
		{name: "the owner's, a folder no profile names", repository: "frostyard/snow", cwd: "/tmp/snow", refused: "no profile names frostyard/snow in /tmp/snow"},
		{name: "the owner's, a repository no profile names", repository: "frostyard/nsl", refused: "no profile names the repository frostyard/nsl"},
	} {
		got, err := SessionCheckout(c.profile, every, c.repository, c.cwd)
		switch {
		case c.refused == "" && (err != nil || got != c.want):
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s: %q, %v; want a refusal with %q", c.name, got, err, c.refused)
		}
	}
	if got := RepositoryFolders(every, "frostyard/CHAIRLIFT"); strings.Join(got, " ") != "/home/teg/chairlift /srv/chairlift" {
		t.Errorf("RepositoryFolders = %q", got)
	}
}

// A session's worktree is named for its profile, or for the owner with
// none, as the client computes it (proto.SessionWorktree), and as a
// thread's is beside its checkout (Worktree).
func TestSessionWorktree(t *testing.T) {
	duncan := profiles.Profile{ID: "duncan"}
	path, branch := SessionWorktree(&duncan, "/srv/chairlift", "Fix CI", "r1")
	if path != "/srv/chairlift--duncan-fix-ci-qlz6tr" || branch != "duncan/fix-ci-qlz6tr" {
		t.Errorf("for duncan: %q, %q", path, branch)
	}
	if p, b := Worktree("/srv/chairlift", "duncan", proto.SessionSlug("Fix CI", "r1")); p != path || b != branch {
		t.Errorf("a thread's naming gives %q, %q", p, b)
	}
	path, branch = SessionWorktree(nil, "/srv/chairlift", "", "r1")
	if path != "/srv/chairlift--owner-session-qlz6tr" || branch != "owner/session-qlz6tr" {
		t.Errorf("for the owner: %q, %q", path, branch)
	}
}
