package pig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	checkout := filepath.Join(l.home, "projects", "example")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", checkout},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	report := filepath.Join(l.home, "hook-report")
	hook := "#!/bin/sh\n" +
		`{ echo ran; cat "` + filepath.Join(l.data, "auth.json") + `" || echo hidden; echo "init=$(cat /proc/1/comm)"; } > "` + report + "\"\n"
	if err := os.WriteFile(filepath.Join(checkout, ".git", "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	path := checkout + "--vera-fix-t1"
	if err := worktree.Add(context.Background(), e.Git, checkout, path, "vera/fix-t1", "team:1:5:c1"); err != nil {
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
}
