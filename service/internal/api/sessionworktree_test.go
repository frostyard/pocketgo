package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// repoHarness is a harness whose profiles name repositories (ADR-0027):
// duncan works on chairlift and on plain, a folder that is no checkout;
// teg on chairlift, spelled in another case, in a checkout of its own, and
// on snow; irulan is private, at home; odrade keeps a standing session.
type repoHarness struct {
	*harness
	chairlift, tegChairlift, snow, plain string
	root, profileDir                     string
	mu                                   sync.Mutex
	gits                                 [][]string
}

func newRepoHarness(t *testing.T) *repoHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	h := &repoHarness{harness: newHarness(t)}
	h.root = t.TempDir()
	root := h.root
	h.chairlift, h.tegChairlift, h.snow = checkoutAt(t, root, "chairlift"), checkoutAt(t, root, "teg/chairlift"), checkoutAt(t, root, "snow")
	h.plain = filepath.Join(root, "plain")
	if err := os.Mkdir(h.plain, 0o700); err != nil {
		t.Fatal(err)
	}
	h.profileDir, _ = h.writeProfiles(map[string]string{
		"duncan.md": h.duncan(h.chairlift, false),
		"teg.md":    "---\nmodel: faux/faux-1\nrepositories:\n  Frostyard/Chairlift: " + h.tegChairlift + "\n  frostyard/snow: " + h.snow + "\n---\n# Teg\n",
		"irulan.md": "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\nrepositories:\n  frostyard/snow: " + h.snow + "\n---\n# Irulan\n",
		"odrade.md": odrade,
	})
	// Every git the server runs goes through the engine (Engine.Git, in
	// the sandbox in pocketd), and is seen here.
	h.engine.onGit = func(args []string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.gits = append(h.gits, slices.Clone(args))
	}
	return h
}

// duncan is Duncan's profile with chairlift at checkout, private or not.
func (h *repoHarness) duncan(checkout string, private bool) string {
	text := "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/chairlift: " + checkout + "\n  frostyard/plain: " + h.plain + "\n"
	if private {
		text += "private: true\n"
	}
	return text + "---\n# Duncan Idaho\n"
}

// writeProfile writes one profile file of the harness's folder.
func (h *repoHarness) writeProfile(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.profileDir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkoutAt makes a git checkout with one commit at root/name.
func checkoutAt(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// adds counts the worktrees git was asked to make.
func (h *repoHarness) adds() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, args := range h.gits {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			n++
		}
	}
	return n
}

// hostGit runs git as the test, outside the server.
func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A session on a repository works in the checkout its profile names, or
// with no profile in the one of the profiles' checkouts it names; pocketd
// takes no folder no profile names for it (rule 6a).
func TestASessionOnARepositoryWorksInItsCheckout(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	s, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "d", Profile: "duncan", Repository: "frostyard/chairlift"})
	if err != nil || s.Cwd != h.chairlift || s.Profile != "duncan" {
		t.Fatalf("duncan on chairlift: %+v, %v", s, err)
	}
	s, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "d2", Profile: "duncan", Repository: "frostyard/chairlift", Cwd: h.chairlift + "/"})
	if err != nil || s.Cwd != h.chairlift {
		t.Fatalf("duncan on chairlift, its folder named: %+v, %v", s, err)
	}
	// The owner's own: one folder, or the one named of two.
	s, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "o", Repository: "frostyard/snow", Model: model})
	if err != nil || s.Cwd != h.snow || s.Profile != "" {
		t.Fatalf("the owner on snow: %+v, %v", s, err)
	}
	s, err = h.client.CreateSession(ctx, proto.CreateSession{RequestID: "o2", Repository: "frostyard/chairlift", Cwd: h.tegChairlift, Model: model})
	if err != nil || s.Cwd != h.tegChairlift {
		t.Fatalf("the owner on teg's chairlift: %+v, %v", s, err)
	}
	created := h.engine.created
	for name, request := range map[string]proto.CreateSession{
		"a repository the profile lacks":     {Profile: "duncan", Repository: "frostyard/snow"},
		"another folder":                     {Profile: "duncan", Repository: "frostyard/chairlift", Cwd: h.tegChairlift},
		"two folders, none named":            {Repository: "frostyard/chairlift", Model: model},
		"a folder no profile names":          {Repository: "frostyard/snow", Cwd: h.work, Model: model},
		"a repository no profile names":      {Repository: "frostyard/nsl", Model: model},
		"a worktree without a repository":    {Profile: "duncan", Worktree: true},
		"a private profile off its home":     {Profile: "irulan", Repository: "frostyard/snow"},
		"a private profile in a worktree":    {Profile: "irulan", Repository: "frostyard/snow", Worktree: true},
		"a standing profile on a repository": {Profile: "odrade", Repository: "frostyard/chairlift"},
		"a standing profile in a worktree":   {Profile: "odrade", Worktree: true},
	} {
		request.RequestID = "bad " + name
		_, err := h.client.CreateSession(ctx, request)
		var e *proto.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if h.engine.created != created || h.adds() != 0 {
		t.Fatalf("refused requests made %d sessions and %d worktrees", h.engine.created-created, h.adds())
	}
}

// A session in a new worktree works where the request names it, beside
// the checkout, on a new branch from the checkout's HEAD, in the sandbox's
// git; the same request again answers the same session and makes nothing
// more (rules 5 and 6a).
func TestASessionsWorktreeIsMadeForItsRequest(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true, Title: " Fix CI "}
	s, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "Fix CI", "r1")
	if s.Cwd != path || path != h.chairlift+"--duncan-fix-ci-qlz6tr" || s.Title != "Fix CI" {
		t.Fatalf("session %+v, want its folder %s", s, path)
	}
	if got := hostGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("the worktree is on %q, want %q", got, branch)
	}
	if hostGit(t, path, "rev-parse", "HEAD") != hostGit(t, h.chairlift, "rev-parse", "HEAD") {
		t.Error("the worktree's branch is not from the checkout's HEAD")
	}
	if spec := h.engine.specs[len(h.engine.specs)-1]; spec.Cwd != path || spec.Profile == nil || spec.Profile.ID != "duncan" {
		t.Errorf("the engine got %+v", spec)
	}
	again, err := h.client.CreateSession(ctx, request)
	if err != nil || again.ID != s.ID || h.adds() != 1 {
		t.Fatalf("the retry: %+v, %v, after %d adds", again, err, h.adds())
	}
	// The owner's own is named for the owner.
	owner, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r2", Repository: "frostyard/snow", Worktree: true, Model: model})
	if err != nil || owner.Cwd != h.snow+"--owner-session-"+proto.WorktreeSuffix("r2") {
		t.Fatalf("the owner's: %+v, %v", owner, err)
	}
	if got := hostGit(t, owner.Cwd, "rev-parse", "--abbrev-ref", "HEAD"); got != "owner/session-"+proto.WorktreeSuffix("r2") {
		t.Errorf("the owner's worktree is on %q", got)
	}
}

// A try that made the worktree and then failed leaves it for the retry,
// which finishes it and starts the session there, with no second git
// worktree add; git ran to its end though the client was gone.
func TestARetryFinishesTheWorktreeItsRequestMade(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	h.engine.createErr = errors.New("the durable store is locked")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	if _, err := h.client.CreateSession(ctx, request); err == nil {
		t.Fatal("the first try did not fail")
	}
	path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	if _, err := os.Stat(path); err != nil || h.adds() != 1 {
		t.Fatalf("after the failed try: %v, %d adds", err, h.adds())
	}
	s, err := h.client.CreateSession(ctx, request)
	if err != nil || s.Cwd != path || h.adds() != 1 {
		t.Fatalf("the retry: %+v, %v, %d adds", s, err, h.adds())
	}
}

// Another request whose worktree's name comes out the same, from another
// device, say, is refused before git runs, and never takes the worktree.
func TestAnotherRequestNeverTakesASessionsWorktree(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	if err := h.store.ClaimWorktree(ctx, "u:another-device:r1", path, branch); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.adds() != 0 || h.engine.created != 0 {
		t.Fatalf("a refused request made %d worktrees and %d sessions", h.adds(), h.engine.created)
	}
	if err := h.store.ClaimWorktree(ctx, "u:"+h.device+":r1", path, branch); !errors.Is(err, store.ErrWorktreeTaken) {
		t.Fatalf("the refused request holds the path: %v", err)
	}
}

// Two spellings of one checkout, through a link to its folder, name one
// worktree: the same request id from another device is refused, and
// never works in the first one's worktree (Codex review of #46, round 1).
func TestAnotherSpellingOfTheCheckoutNeverSharesAWorktree(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(h.root, alias); err != nil {
		t.Fatal(err)
	}
	h.writeProfile(t, "leto.md", "---\nmodel: faux/faux-1\nrepositories:\n  frostyard/chairlift: "+filepath.Join(alias, "chairlift")+"\n---\n# Leto\n")
	first, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Repository: "frostyard/chairlift", Cwd: h.chairlift, Worktree: true, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := auth.CreateDevice(ctx, h.store, "phone")
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := proto.NewClient(base, token, h.http)
	_, err = phone.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Repository: "frostyard/chairlift", Cwd: filepath.Join(alias, "chairlift"), Worktree: true, Model: model})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.engine.created != 1 || h.adds() != 1 {
		t.Fatalf("the second spelling made %d sessions and %d worktrees in all; the first works in %s", h.engine.created, h.adds(), first.Cwd)
	}
}

// A retry of a request whose worktree was made, after its profile's
// checkout moved, makes no second worktree: the request made its one
// (Codex review of #46, round 1).
func TestARetryAfterTheCheckoutMovedMakesNoSecondWorktree(t *testing.T) {
	// The folder left out, or named as the first try had it, as the
	// desktop sends it (Codex review of #46, round 2): both are conflict.
	for _, named := range []bool{false, true} {
		h := newRepoHarness(t)
		ctx := context.Background()
		h.engine.createErr = errors.New("the durable store is locked")
		request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
		if named {
			request.Cwd = h.chairlift
		}
		if _, err := h.client.CreateSession(ctx, request); err == nil {
			t.Fatal("the first try did not fail")
		}
		h.writeProfile(t, "duncan.md", h.duncan(h.tegChairlift, false))
		_, err := h.client.CreateSession(ctx, request)
		asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
		if h.adds() != 1 || h.engine.created != 0 {
			t.Fatalf("folder named %v: the retry made %d worktrees in all and %d sessions", named, h.adds(), h.engine.created)
		}
		moved, _ := proto.SessionWorktree(h.tegChairlift, "duncan", "", "r1")
		if _, err := os.Lstat(moved); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("folder named %v: a second worktree at %s: %v", named, moved, err)
		}
	}
	// A request that made no worktree and names a folder the profile does
	// not is bad_request still.
	h := newRepoHarness(t)
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r2", Profile: "duncan", Repository: "frostyard/chairlift", Cwd: h.tegChairlift, Worktree: true})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
}

// The profile is read again once the worktree is made, which may take
// minutes: one made private meanwhile starts nothing (bad_request), and
// one that no longer names the checkout starts nothing either, conflict as
// a retry of the request would be (Codex review of #46, round 1).
func TestAProfileChangedWhileTheWorktreeIsMadeStartsNoSession(t *testing.T) {
	for name, c := range map[string]struct {
		change func(h *repoHarness) string
		status int
	}{
		"made private":           {func(h *repoHarness) string { return h.duncan(h.chairlift, true) }, http.StatusBadRequest},
		"its checkout moved":     {func(h *repoHarness) string { return h.duncan(h.tegChairlift, false) }, http.StatusConflict},
		"no longer on chairlift": {func(*repoHarness) string { return "---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan Idaho\n" }, http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRepoHarness(t)
			h.server.afterSessionWorktree = func() { h.writeProfile(t, "duncan.md", c.change(h)) }
			_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
			var e *proto.Error
			if !errors.As(err, &e) || e.Status != c.status {
				t.Fatalf("error = %v, want %d", err, c.status)
			}
			if h.engine.created != 0 || h.adds() != 1 {
				t.Fatalf("%d sessions, %d worktrees", h.engine.created, h.adds())
			}
		})
	}
}

// A worktree that changed between its making and its session's start, as
// the start waited for createMu, is checked as it is then: one removed or
// switched to another branch starts nothing (Codex review of #46, round 2).
func TestAWorktreeChangedBeforeItsStartStartsNoSession(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, path string){
		"removed":        func(t *testing.T, path string) { hostGit(t, path, "worktree", "remove", "--force", path) },
		"another branch": func(t *testing.T, path string) { hostGit(t, path, "switch", "-q", "-c", "elsewhere") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newRepoHarness(t)
			path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
			h.server.afterSessionWorktree = func() { change(t, path) }
			_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
			asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
			if h.engine.created != 0 {
				t.Fatalf("%d sessions", h.engine.created)
			}
		})
	}
}

// A worktree or a branch of the name that this request did not make, as
// another data folder or a work database restored from before leaves, is
// never adopted: conflict, on every retry (Codex review of #46, round 2).
func TestAWorktreeTheRequestDidNotMakeIsNeverAdopted(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	hostGit(t, h.chairlift, "worktree", "add", "-q", "-b", branch, path, "HEAD")
	request := proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}
	for try := range 2 {
		_, err := h.client.CreateSession(ctx, request)
		asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
		if h.engine.created != 0 || h.adds() != 0 {
			t.Fatalf("try %d: %d sessions, %d worktrees", try, h.engine.created, h.adds())
		}
	}
	// A branch alone, its folder gone, is no more the request's.
	_, branch2 := proto.SessionWorktree(h.chairlift, "duncan", "", "r2")
	hostGit(t, h.chairlift, "branch", branch2)
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r2", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.engine.created != 0 || h.adds() != 0 {
		t.Fatalf("a branch alone: %d sessions, %d worktrees", h.engine.created, h.adds())
	}
}

// Every start reads the profiles under createMu: one made private while
// the start waited for it starts nothing outside its home (Codex review of
// #46, round 2).
func TestAStartReadsItsProfileUnderTheLock(t *testing.T) {
	h := newRepoHarness(t)
	h.server.createMu.Lock()
	waiting := make(chan struct{})
	h.server.beforeCreateLock = func() { close(waiting) }
	done := make(chan error, 1)
	go func() {
		_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "p", Profile: "duncan", Repository: "frostyard/chairlift"})
		done <- err
	}()
	<-waiting
	h.writeProfile(t, "duncan.md", h.duncan(h.chairlift, true))
	h.server.createMu.Unlock()
	err := <-done
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if h.engine.created != 0 {
		t.Fatalf("%d sessions", h.engine.created)
	}
}

// A repository whose folder is no checkout makes no worktree: git's word
// for it comes back, and no session starts.
func TestAWorktreeOfAFolderThatIsNoCheckoutIsRefused(t *testing.T) {
	h := newRepoHarness(t)
	_, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/plain", Worktree: true})
	var e *proto.Error
	if !errors.As(err, &e) || e.Status != http.StatusBadRequest || !strings.Contains(e.Message, "is not a git checkout") {
		t.Fatalf("error %v", err)
	}
	if h.engine.created != 0 {
		t.Fatal("a session started")
	}
}

// A worktree's git holds no other start: the lock that orders the starts
// is free while git makes the worktree (Codex review of #46, round 1: no
// deadline decides it).
func TestAWorktreesGitHoldsNoOtherStart(t *testing.T) {
	h := newRepoHarness(t)
	held := false
	h.engine.onGit = func(args []string) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			if !h.server.createMu.TryLock() {
				held = true
				return
			}
			h.server.createMu.Unlock()
		}
	}
	if _, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "wt", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true}); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("git made the worktree under the lock that orders the starts")
	}
}
