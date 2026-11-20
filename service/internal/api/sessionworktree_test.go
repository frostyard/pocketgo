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
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// repoHarness is a harness whose profiles name repositories (ADR-0027):
// duncan works on chairlift and on plain, a folder that is no checkout;
// teg on chairlift, spelled in another case, in a checkout of its own, and
// on snow; irulan is private, at home; odrade keeps a standing session.
type repoHarness struct {
	*harness
	chairlift, tegChairlift, snow, plain string
	mu                                   sync.Mutex
	gits                                 [][]string
}

func newRepoHarness(t *testing.T) *repoHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	h := &repoHarness{harness: newHarness(t)}
	root := t.TempDir()
	h.chairlift, h.tegChairlift, h.snow = checkoutAt(t, root, "chairlift"), checkoutAt(t, root, "teg/chairlift"), checkoutAt(t, root, "snow")
	h.plain = filepath.Join(root, "plain")
	if err := os.Mkdir(h.plain, 0o700); err != nil {
		t.Fatal(err)
	}
	h.writeProfiles(map[string]string{
		"duncan.md": "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/chairlift: " + h.chairlift + "\n  frostyard/plain: " + h.plain + "\n---\n# Duncan Idaho\n",
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
	path, _ := proto.SessionWorktree(h.chairlift, "duncan", "", "r1")
	if err := h.store.ClaimWorktree(ctx, path, "u:another-device:r1"); err != nil {
		t.Fatal(err)
	}
	_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "r1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if h.adds() != 0 || h.engine.created != 0 {
		t.Fatalf("a refused request made %d worktrees and %d sessions", h.adds(), h.engine.created)
	}
	if err := h.store.ClaimWorktree(ctx, path, "u:"+h.device+":r1"); !errors.Is(err, store.ErrWorktreeTaken) {
		t.Fatalf("the refused request holds the path: %v", err)
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

// A worktree's git holds no other start: a plain session starts while it
// runs.
func TestAWorktreesGitHoldsNoOtherStart(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	adding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	h.engine.onGit = func(args []string) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			close(adding)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "wt", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true})
		done <- err
	}()
	<-adding
	started := make(chan error, 1)
	go func() {
		_, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a plain session waited on a worktree's git")
	}
	free()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
