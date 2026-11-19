package pig

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// A thread's worktree is made by git in the sandbox (ADR-0012, ADR-0014):
// a post-checkout hook an agent planted in its checkout runs, but cannot
// see pocketd's data folder, so it cannot carry the sign-ins out.
func TestAWorktreeIsMadeInTheSandbox(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	l := newLayout(t)
	s := usableSandbox(t, l)
	e := &Engine{opts: Options{Sandbox: s}, commands: allowedEnv()}
	isolateGit(e)
	checkout := filepath.Join(l.home, "projects", "example")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", checkout},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	report := filepath.Join(l.home, "hook-report")
	// The hook's paths come from the environment, quoted, never as shell
	// text: any temporary folder's name works.
	e.commands["POCKETGO_TEST_AUTH"] = filepath.Join(l.data, "auth.json")
	e.commands["POCKETGO_TEST_REPORT"] = report
	hook := "#!/bin/sh\n" +
		`{ echo ran; cat "$POCKETGO_TEST_AUTH" || echo hidden; echo "init=$(cat /proc/1/comm)"; } > "$POCKETGO_TEST_REPORT"` + "\n"
	if err := os.WriteFile(filepath.Join(checkout, ".git", "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	path := checkout + "--vera-fix-t1"
	if err := worktree.Add(context.Background(), worktree.Tools{Git: e.Git, Kind: e.Kind, Read: e.Read, Same: e.Same}, checkout, path, "vera/fix-t1"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	if text := string(got); !strings.Contains(text, "ran") || !strings.Contains(text, "hidden") ||
		strings.Contains(text, "sk-secret-value") || !strings.Contains(text, "init=bwrap") {
		t.Fatalf("the hook saw:\n%s", text)
	}
	if head, err := e.Git(context.Background(), path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != "vera/fix-t1" {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	// An agent points the worktree's lock at pocketd's sign-ins: the
	// retry reads the lock through the sandbox, which hides them.
	dir, err := e.Git(context.Background(), path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(l.data, "auth.json"), filepath.Join(dir, "locked")); err != nil {
		t.Fatal(err)
	}
	// The existence check runs in the sandbox: pocketd's sign-ins, on the
	// host, are not there.
	if there, err := e.Exists(context.Background(), filepath.Join(l.data, "auth.json")); err != nil || there {
		t.Fatalf("the sign-ins seen from the check: %v %v", there, err)
	}
	if text, ok, err := e.Read(context.Background(), filepath.Join(l.data, "auth.json")); err != nil || ok || text != "" {
		t.Fatalf("the sign-ins read from the check: %q %v %v", text, ok, err)
	}
	err = worktree.Add(context.Background(), worktree.Tools{Git: e.Git, Kind: e.Kind, Read: e.Read, Same: e.Same}, checkout, path, "vera/fix-t1")
	if err == nil || strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("a lock leading to the sign-ins: %v", err)
	}
}

// git's own answer is told apart from a sandbox that failed around it: a
// folder that is no checkout is git's exit (worktree.GitExit), a bwrap
// that cannot set up its mounts is not, so a check that could not run
// never reads as "not a git checkout" (Codex review round 24).
func TestGitsAnswerIsToldApartFromTheSandboxs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	l := newLayout(t)
	s := usableSandbox(t, l)
	e := &Engine{opts: Options{Sandbox: s}, commands: allowedEnv()}
	isolateGit(e)
	plain := filepath.Join(l.home, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := e.Git(context.Background(), plain, "rev-parse", "--show-toplevel")
	var exit *worktree.GitExit
	if !errors.As(err, &exit) || exit.Status != 128 {
		t.Fatalf("git outside a checkout: %v", err)
	}
	broken := *s
	broken.Pin = append(slices.Clone(s.Pin), filepath.Join(l.home, "no-such-folder"))
	e.opts.Sandbox = &broken
	_, err = e.Git(context.Background(), plain, "rev-parse", "--show-toplevel")
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("git in a sandbox that failed: %v", err)
	}
	if err := worktree.Usable(context.Background(), worktree.Tools{Git: e.Git, Kind: e.Kind, Read: e.Read, Same: e.Same}, plain, plain+"--x", "x"); !worktree.Unsure(err) {
		t.Fatalf("a check in a sandbox that failed: %v", err)
	}
}
