package state

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

func labelsOf(choices []RepositoryChoice) string {
	out := make([]string, len(choices))
	for i, ch := range choices {
		out[i] = ch.Label
	}
	return strings.Join(out, " | ")
}

// New Session offers the chosen profile's repositories; with no profile,
// every repository a profile names, each folder once, a folder in its
// label when a name has two; and none for a standing or a private
// profile, or one pocketd no longer offers (ADR-0027).
func TestRepositoryChoices(t *testing.T) {
	duncan := proto.Profile{ID: "duncan", Repositories: []proto.Repository{
		{Name: "frostyard/chairlift", Folder: "/srv/chairlift"}, {Name: "frostyard/snow", Folder: "/srv/snow"}}}
	teg := proto.Profile{ID: "teg", Repositories: []proto.Repository{
		{Name: "Frostyard/Chairlift", Folder: "/home/teg/chairlift"}, {Name: "frostyard/nsl", Folder: "/srv/nsl"}, {Name: "frostyard/snow", Folder: "/srv/snow"}}}
	odrade := proto.Profile{ID: "odrade", Standing: true, Repositories: duncan.Repositories}
	irulan := proto.Profile{ID: "irulan", Private: true, Repositories: duncan.Repositories}
	all := []proto.Profile{duncan, teg, odrade, irulan}
	if got := labelsOf(RepositoryChoices(&duncan, all)); got != "None | frostyard/chairlift | frostyard/snow" {
		t.Errorf("duncan's: %s", got)
	}
	got := RepositoryChoices(nil, all)
	if labelsOf(got) != "None | frostyard/chairlift · /srv/chairlift | frostyard/chairlift · /home/teg/chairlift | frostyard/snow | frostyard/nsl" {
		t.Errorf("the owner's: %s", labelsOf(got))
	}
	if got[2].Name != "frostyard/chairlift" || got[2].Folder != "/home/teg/chairlift" {
		t.Errorf("teg's chairlift: %+v", got[2])
	}
	for _, p := range []proto.Profile{odrade, irulan} {
		if got := labelsOf(RepositoryChoices(&p, all)); got != "None" {
			t.Errorf("%s's: %s", p.ID, got)
		}
	}
	if got := labelsOf(RepositoryChoicesFor(nil, all)); !strings.Contains(got, "frostyard/nsl") {
		t.Errorf("before the profiles loaded: %s", got)
	}
	if got := labelsOf(RepositoryChoicesFor(&ProfileChoice{Label: NoProfile}, all)); !strings.Contains(got, "frostyard/nsl") {
		t.Errorf("No profile's: %s", got)
	}
	if got := labelsOf(RepositoryChoicesFor(&ProfileChoice{Label: "Duncan (no longer available)", ID: "duncan", Gone: true}, all)); got != "None" {
		t.Errorf("a gone profile's: %s", got)
	}
	if got := labelsOf(RepositoryChoicesFor(&ProfileChoice{Label: "Duncan", ID: "duncan", Profile: &duncan}, all)); got != "None | frostyard/chairlift | frostyard/snow" {
		t.Errorf("duncan's, by its choice: %s", got)
	}
}

// The form fills in a repository's folder when it is chosen, and the
// profile's when it goes; a folder the owner types ends the choice, and
// the worktree switch with it.
func TestSessionFolderFollowsTheRepository(t *testing.T) {
	duncan := []RepositoryChoice{{Label: NoRepository}, {Label: "frostyard/chairlift", Name: "frostyard/chairlift", Folder: "/srv/chairlift"}}
	f := NewSessionFolder("/last")
	f.Sync("duncan", "/home/duncan", duncan)
	if f.Cwd != "/home/duncan" || f.Chosen().Name != "" {
		t.Fatalf("duncan chosen: %+v", f)
	}
	f.Repository = "frostyard/chairlift"
	f.Sync("duncan", "/home/duncan", duncan)
	if f.Cwd != "/srv/chairlift" || f.Chosen().Folder != "/srv/chairlift" {
		t.Fatalf("chairlift chosen: %+v", f)
	}
	f.Worktree = true
	f.Sync("duncan", "/home/duncan", duncan)
	if !f.Worktree || f.Cwd != "/srv/chairlift" {
		t.Fatalf("the switch on: %+v", f)
	}
	// Typed by hand: the choice and the switch go, the folder stays.
	f.Cwd = "/srv/chairlift-old"
	f.Sync("duncan", "/home/duncan", duncan)
	if f.Repository != NoRepository || f.Worktree || f.Chosen().Name != "" || f.Cwd != "/srv/chairlift-old" {
		t.Fatalf("after an edit: %+v", f)
	}
	// Chosen again, then None: the folder typed comes back.
	f.Repository = "frostyard/chairlift"
	f.Sync("duncan", "/home/duncan", duncan)
	f.Worktree = true
	f.Repository = NoRepository
	f.Sync("duncan", "/home/duncan", duncan)
	if f.Cwd != "/srv/chairlift-old" || f.Worktree {
		t.Fatalf("after None: %+v", f)
	}
	// Emptied, the folder typed is none: None after a repository brings
	// back the profile's.
	f.Cwd = ""
	f.Sync("duncan", "/home/duncan", duncan)
	f.Repository = "frostyard/chairlift"
	f.Sync("duncan", "/home/duncan", duncan)
	f.Repository = NoRepository
	f.Sync("duncan", "/home/duncan", duncan)
	if f.Cwd != "/home/duncan" {
		t.Fatalf("after None, nothing typed: %+v", f)
	}
	// Its folder moves: the folder follows.
	f.Repository = "frostyard/chairlift"
	f.Sync("duncan", "/home/duncan", duncan)
	moved := []RepositoryChoice{{Label: NoRepository}, {Label: "frostyard/chairlift", Name: "frostyard/chairlift", Folder: "/srv/chairlift2"}}
	f.Sync("duncan", "/home/duncan", moved)
	if f.Cwd != "/srv/chairlift2" || f.Repository != "frostyard/chairlift" {
		t.Fatalf("after the folder moved: %+v", f)
	}
	// Another profile without it: None, and that profile's folder.
	f.Worktree = true
	f.Sync("vera", "/home/vera", []RepositoryChoice{{Label: NoRepository}})
	if f.Cwd != "/home/vera" || f.Repository != NoRepository || f.Worktree {
		t.Fatalf("on vera: %+v", f)
	}
	// No profile, whose list names the folder with its label once a second
	// folder of the name shows: the same repository stays chosen.
	owner := []RepositoryChoice{{Label: NoRepository}, {Label: "frostyard/chairlift", Name: "frostyard/chairlift", Folder: "/srv/chairlift2"}}
	f.Sync("", "", owner)
	if f.Cwd != "/last" {
		t.Fatalf("no profile: %+v", f)
	}
	f.Repository = "frostyard/chairlift"
	f.Sync("", "", owner)
	two := []RepositoryChoice{{Label: NoRepository},
		{Label: "frostyard/chairlift · /srv/chairlift2", Name: "frostyard/chairlift", Folder: "/srv/chairlift2"},
		{Label: "frostyard/chairlift · /home/teg/chairlift", Name: "frostyard/chairlift", Folder: "/home/teg/chairlift"}}
	f.Sync("", "", two)
	if f.Repository != "frostyard/chairlift · /srv/chairlift2" || f.Cwd != "/srv/chairlift2" {
		t.Fatalf("after a second folder came: %+v", f)
	}
	// A folder typed without a repository stays through a profile change.
	f.Repository = NoRepository
	f.Sync("", "", two)
	f.Cwd = "/mine"
	f.Sync("duncan", "/home/duncan", duncan)
	if f.Cwd != "/mine" {
		t.Fatalf("a typed folder after a profile change: %+v", f)
	}
}

// None brings back the folder for the profile chosen now, never another
// profile's: one typed under the first profile, else the second's own
// (Codex review of #46, round 2).
func TestNoneBringsBackTheFolderOfTheProfileChosenNow(t *testing.T) {
	vera := []RepositoryChoice{{Label: NoRepository}, {Label: "frostyard/snow", Name: "frostyard/snow", Folder: "/srv/snow"}}
	for _, typed := range []string{"", "/mine"} {
		f := NewSessionFolder("/last")
		f.Sync("duncan", "/home/duncan", []RepositoryChoice{{Label: NoRepository}})
		if typed != "" {
			f.Cwd = typed
			f.Sync("duncan", "/home/duncan", []RepositoryChoice{{Label: NoRepository}})
		}
		f.Sync("vera", "/home/vera", vera)
		f.Repository = "frostyard/snow"
		f.Sync("vera", "/home/vera", vera)
		if f.Cwd != "/srv/snow" {
			t.Fatalf("typed %q: snow chosen: %+v", typed, f)
		}
		f.Repository = NoRepository
		f.Sync("vera", "/home/vera", vera)
		want := typed
		if want == "" {
			want = "/home/vera"
		}
		if f.Cwd != want {
			t.Errorf("typed %q: None brings back %q, want %q", typed, f.Cwd, want)
		}
	}
}

// The request names the repository while its folder shows, with its
// checkout and the switch; a folder typed since names none.
func TestNewSessionRequestOnARepository(t *testing.T) {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	duncan := &proto.Profile{ID: "duncan", Repositories: []proto.Repository{{Name: "frostyard/chairlift", Folder: "/srv/chairlift"}}}
	f := NewSessionFolder("/last")
	choices := RepositoryChoices(duncan, nil)
	f.Repository = "frostyard/chairlift"
	f.Sync("duncan", "", choices)
	f.Worktree = true
	got := NewSessionRequest(duncan, f, "Fix CI", model, "")
	if got != (proto.CreateSession{Profile: "duncan", Repository: "frostyard/chairlift", Cwd: "/srv/chairlift", Worktree: true, Title: "Fix CI", Model: model}) {
		t.Errorf("on chairlift: %+v", got)
	}
	f.Cwd = "/srv/other"
	if got := NewSessionRequest(duncan, f, "", model, ""); got.Repository != "" || got.Worktree || got.Cwd != "/srv/other" {
		t.Errorf("a typed folder: %+v", got)
	}
	if err := ValidateNewSession(proto.CreateSession{Cwd: "/w", Model: model, Worktree: true}, nil); err == nil {
		t.Error("a worktree without a repository passed")
	}
}

// The preview is the worktree pocketd makes from the request: the
// profile's, or the owner's with none, from its title and request id.
func TestWorktreePreview(t *testing.T) {
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Cwd: "/srv/chairlift", Worktree: true, Title: "Fix CI"}
	path, branch, ok := WorktreePreview(request)
	if !ok || path != "/srv/chairlift--duncan-fix-ci-qlz6tr" || branch != "duncan/fix-ci-qlz6tr" {
		t.Errorf("duncan's: %q %q %v", path, branch, ok)
	}
	request.Profile = ""
	if path, branch, _ := WorktreePreview(request); path != "/srv/chairlift--owner-fix-ci-qlz6tr" || branch != "owner/fix-ci-qlz6tr" {
		t.Errorf("the owner's: %q %q", path, branch)
	}
	request.Worktree = false
	if _, _, ok := WorktreePreview(request); ok {
		t.Error("a request without a worktree previews one")
	}
	if WorktreeLine("duncan/fix-ci-qlz6tr") != "(a new worktree on duncan/fix-ci-qlz6tr)" {
		t.Errorf("line %q", WorktreeLine("duncan/fix-ci-qlz6tr"))
	}
}

// deadlineTransport records the deadline of each new session's request.
type deadlineTransport struct {
	base      http.RoundTripper
	deadlines *[]time.Time
}

func (d deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && r.URL.Path == proto.PathSessions {
		deadline, _ := r.Context().Deadline()
		*d.deadlines = append(*d.deadlines, deadline)
	}
	return d.base.RoundTrip(r)
}

// A new session that makes a worktree waits for pocketd's git, which may
// take up to 5 minutes, past the client's usual deadline: a slow checkout
// is no failure (Codex review of #46, round 1). The two deadlines are
// compared with each other, never with a clock.
func TestAWorktreesStartWaitsForItsGit(t *testing.T) {
	h := newHarness(t)
	var deadlines []time.Time
	h.store = h.newStore(t, deadlineTransport{base: h.srv.Transport(), deadlines: &deadlines})
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	h.srv.AddProfile(proto.Profile{ID: "duncan", Runs: opus, Repositories: []proto.Repository{{Name: "frostyard/chairlift", Folder: "/srv/chairlift"}}})
	h.connect(t)
	create := func(r proto.CreateSession) {
		done := false
		h.store.CreateSession(r, func(_ proto.Session, err error) {
			if err != nil {
				t.Errorf("%+v: %v", r, err)
			}
			done = true
		})
		h.loop.RunUntil(t, func() bool { return done })
	}
	create(proto.CreateSession{Profile: "duncan", Cwd: "/srv/chairlift"})
	create(proto.CreateSession{Profile: "duncan", Repository: "frostyard/chairlift", Cwd: "/srv/chairlift", Worktree: true})
	if len(deadlines) != 2 {
		t.Fatalf("%d deadlines", len(deadlines))
	}
	if waited := deadlines[1].Sub(deadlines[0]); waited < 5*time.Minute {
		t.Errorf("the worktree's start waits %s longer than another, not past pocketd's 5 minutes of git", waited)
	}
}

// A repository's folder goes as its profile names it, a space at its end
// included, and the id shown with it is the one sent: only a folder typed
// is trimmed (Codex review of #46, round 1).
func TestARepositorysFolderGoesAsItIs(t *testing.T) {
	h := newHarness(t)
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	duncan := proto.Profile{ID: "duncan", Runs: opus, Repositories: []proto.Repository{{Name: "frostyard/chairlift", Folder: "/srv/chair lift "}}}
	h.srv.AddProfile(duncan)
	h.connect(t)
	f := NewSessionFolder("/last")
	choices := RepositoryChoices(&duncan, nil)
	f.Repository = "frostyard/chairlift"
	f.Sync("duncan", "", choices)
	f.Worktree = true
	request := NewSessionRequest(&duncan, f, "Fix CI", proto.ModelRef{}, "")
	shown := request
	shown.RequestID = h.store.NextCreateID(request)
	path, _, _ := WorktreePreview(shown)
	var got proto.Session
	done := false
	h.store.CreateSession(request, func(s proto.Session, err error) {
		if err != nil {
			t.Error(err)
		}
		got, done = s, true
	})
	h.loop.RunUntil(t, func() bool { return done })
	posts := h.srv.RequestsTo(http.MethodPost, proto.PathSessions)
	var sent proto.CreateSession
	if err := json.Unmarshal(posts[len(posts)-1].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Cwd != "/srv/chair lift " || sent.RequestID != shown.RequestID || got.Cwd != path {
		t.Errorf("sent %q with %s, shown %s; the session works in %q, shown %q", sent.Cwd, sent.RequestID, shown.RequestID, got.Cwd, path)
	}
	if typed, _ := createAction(proto.CreateSession{Cwd: " /srv/typed "}); typed.Cwd != "/srv/typed" {
		t.Errorf("a typed folder goes as %q", typed.Cwd)
	}
}

// Peek shows the id For then sends, for the same action and for an action
// changed before it goes; a failed action's id shows again while the
// action is the same, and a sign-out forgets it.
func TestPeekShowsTheIDThatGoes(t *testing.T) {
	n := 0
	ids := requestIDs{newID: func() string { n++; return strings.Repeat("x", n) }}
	shown := ids.Peek("create", "a")
	if again := ids.Peek("create", "b"); again != shown {
		t.Errorf("an edit before it goes shows %q, then %q", shown, again)
	}
	if sent := ids.For("create", "b"); sent != shown {
		t.Errorf("sent %q, shown %q", sent, shown)
	}
	// It failed: the same action shows and sends its id again.
	if ids.Peek("create", "b") != shown || ids.For("create", "b") != shown {
		t.Error("a failed action's id did not show again")
	}
	other := ids.Peek("create", "c")
	if other == shown {
		t.Error("a changed action after a try shows the old id")
	}
	if ids.For("create", "c") != other {
		t.Error("the changed action's id is not the one shown")
	}
	ids.Done("create")
	if ids.Peek("create", "c") == other {
		t.Error("an action after a success shows the old id")
	}
	fresh := ids.Peek("create", "d")
	ids.reset()
	if ids.Peek("create", "d") == fresh {
		t.Error("a sign-out kept the id shown")
	}
}
