package pig

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/changes"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// A read of a session's changes runs in the sandbox, with the isolation
// options, the trusted git and bounded output (ADR-0053): the checkout's
// fsmonitor never runs, and a new file is read by pocketd's own read in the
// sandbox, through no link.
func TestAChangesReadRunsInTheSandbox(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	l := newLayout(t)
	s := usableSandbox(t, l)
	e := &Engine{opts: Options{Sandbox: s}, commands: allowedEnv()}
	isolateGit(e)
	checkout := filepath.Join(l.home, "projects", "example")
	marker := filepath.Join(l.home, "fsmonitor-ran")
	monitor := filepath.Join(l.home, "fsmonitor")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", checkout},
		{"-C", checkout, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "first"},
		{"-C", checkout, "config", "core.fsmonitor", monitor},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(checkout, "new.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tools := worktree.Tools{Git: e.Git, Kind: e.Kind, Read: e.Read, Same: e.Same, Each: e.GitEach}
	if err := worktree.AttributesIsolated(ctx, tools, checkout); err != nil {
		t.Skipf("this git cannot read a checkout isolated: %v", err)
	}
	// A read runs git 2.45 or later, which takes --no-lazy-fetch (Codex
	// review of #109, round 2).
	if version, err := e.Git(ctx, checkout, changes.VersionArgs()...); err != nil || !changes.GitVersionOK(version) {
		t.Skipf("%s: a read of a session's changes needs git %s or later (%v)", version, changes.MinGit, err)
	}
	head, err := e.Git(ctx, checkout, changes.HeadArgs()...)
	if err != nil {
		t.Fatal(err)
	}
	var numstat []string
	if err := e.GitEach(ctx, checkout, 0, func(r string) error { numstat = append(numstat, r); return nil }, changes.NumstatArgs(head)...); err != nil {
		t.Fatal(err)
	}
	var fresh []string
	if err := e.GitEach(ctx, checkout, 0, func(r string) error { fresh = append(fresh, r); return nil }, changes.NewArgs()...); err != nil {
		t.Fatal(err)
	}
	if len(numstat) != 0 || !slices.Equal(fresh, []string{"new.txt"}) {
		t.Fatalf("numstat %q, new %q", numstat, fresh)
	}
	id, ok, err := e.DirID(ctx, checkout)
	if err != nil || !ok {
		t.Fatalf("the top's identity %q %v %v", id, ok, err)
	}
	kind, data, err := e.ReadIn(ctx, checkout, id, "new.txt", 4096)
	if err != nil || kind != changes.KindFile || string(data) != "one\ntwo\n" {
		t.Fatalf("the new file read %q %q %v", kind, data, err)
	}
	if added, _, counted := changes.CountNew(kind, data); added != 2 || !counted {
		t.Fatalf("count %d %v", added, counted)
	}
	// pocketd's sign-ins, behind a link in the checkout, stay hidden.
	if err := os.Symlink(filepath.Join(l.data, "auth.json"), filepath.Join(checkout, "keys")); err != nil {
		t.Fatal(err)
	}
	if kind, data, err := e.ReadIn(ctx, checkout, id, "keys", 4096); err != nil || kind != changes.KindLink || strings.Contains(string(data), "sk-secret") {
		t.Fatalf("a link to the sign-ins read %q %q %v", kind, data, err)
	}
	// Another folder at the top's path is not the one the read began in.
	if kind, _, err := e.ReadIn(ctx, l.home, id, "new.txt", 4096); err != nil || kind != changes.KindMoved {
		t.Fatalf("another top read %q %v", kind, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checkout's fsmonitor ran")
	}
}

// pocketd's read of a checkout's file goes below its root through no link,
// whichever part a link is, never waits on a named pipe, reads no folder,
// gives a link's target, as git shows a link, and says whether its owner
// may run the file; and it reads only below the folder the read began in,
// never another one at the root's path (ADR-0053; Codex review of #109,
// rounds 1 and 2).
func TestReadBeneathFollowsNoLink(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for path, text := range map[string]string{
		filepath.Join(root, "a", "b.txt"):       "in\n",
		filepath.Join(outside, "b.txt"):         "outside\n",
		filepath.Join(root, "dir", "inner.txt"): "x",
		filepath.Join(root, "run.sh"):           "#!/bin/sh\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(root, "a", "ln")); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(root, "pipe")
	havePipe := mkfifo(pipe) == nil
	var idOut strings.Builder
	if code := RunReadBeneathHelper([]string{root}, &idOut); code != 0 || !strings.HasPrefix(idOut.String(), "dir ") {
		t.Fatalf("the root's identity: %q %d", idOut.String(), code)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(idOut.String(), "dir "), "\n")
	run := func(top, rel, max string) (string, int) {
		var out strings.Builder
		code := RunReadBeneathHelper([]string{top, id, rel, max}, &out)
		return out.String(), code
	}
	for rel, want := range map[string]string{
		"a/b.txt":    "file\nin\n",
		"run.sh":     "exec\n#!/bin/sh\n",
		"link/b.txt": "none\n",
		"a/ln":       "link\n../target",
		"dir":        "none\n",
		"missing":    "none\n",
		"a/b.txt/x":  "none\n",
	} {
		if got, code := run(root, rel, "100"); got != want || code != 0 {
			t.Errorf("%s: %q, status %d", rel, got, code)
		}
	}
	if got, code := run(root, "a/b.txt", "2"); got != "file\nin" || code != 0 {
		t.Errorf("a bounded read: %q %d", got, code)
	}
	if havePipe {
		if got, code := run(root, "pipe", "100"); got != "none\n" || code != 0 {
			t.Errorf("a named pipe: %q %d", got, code)
		}
	}
	// The root swapped for a link to another folder is not the root the
	// read began in.
	if err := os.Rename(root, root+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if got, code := run(root, "b.txt", "100"); got != "moved\n" || code != 0 {
		t.Errorf("a swapped root: %q %d", got, code)
	}
	if got, code := run(filepath.Join(base, "gone"), "b.txt", "100"); got != "moved\n" || code != 0 {
		t.Errorf("a root gone: %q %d", got, code)
	}
	var none strings.Builder
	if code := RunReadBeneathHelper([]string{filepath.Join(base, "gone")}, &none); code != 0 || none.String() != "none\n" {
		t.Errorf("no root's identity: %q %d", none.String(), code)
	}
	for _, bad := range [][]string{{root, id, "../x", "1"}, {root, id, "/etc/passwd", "1"}, {"", id, "a", "1"}, {root, id, "a", "-1"},
		{root, "notanid", "a", "1"}, {root, "a", "1"}, {""}} {
		if code := RunReadBeneathHelper(bad, io.Discard); code != 2 {
			t.Errorf("%q: status %d", bad, code)
		}
	}
}

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

// In the sandbox, gh's configuration folder is pocketd's data folder,
// which the sandbox shows empty: no file of it, and no setting an agent
// planted in the service user's gh configuration, reaches gh (Codex
// review of #20, round 2).
func TestGHSeesAnEmptyConfigurationInTheSandbox(t *testing.T) {
	l := newLayout(t)
	s := usableSandbox(t, l)
	bin := filepath.Join(l.home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "gh")
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo tok; exit 0; fi\necho \"config=[$(ls -A \"$GH_CONFIG_DIR\")]\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Sandbox: s, GH: fake, DataDir: l.data}, commands: allowedEnv()}
	out, err := e.GH(context.Background(), "api", "repos/frostyard/x/pulls/3")
	if err != nil || out != "config=[]\n" {
		t.Fatalf("gh saw its configuration as %q, %v", out, err)
	}
	if entries, _ := os.ReadDir(l.data); len(entries) == 0 {
		t.Fatal("the data folder holds nothing: the check proves nothing")
	}
}

// A gather's skill is read in the sandbox (Engine.ReadUpTo): one an agent
// linked to pocketd's sign-ins reads as no file, a named pipe as none at
// once, and a regular one up to its bound and a byte more (Codex review of
// #57, round 1).
func TestASkillIsReadInTheSandbox(t *testing.T) {
	l := newLayout(t)
	s := usableSandbox(t, l)
	e := &Engine{opts: Options{Sandbox: s}, commands: allowedEnv()}
	skills := filepath.Join(l.home, ".pi", "agent", "skills")
	for _, name := range []string{"leak", "grooming", "pipe"} {
		if err := os.MkdirAll(filepath.Join(skills, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	leak, grooming, pipe := filepath.Join(skills, "leak", "SKILL.md"), filepath.Join(skills, "grooming", "SKILL.md"), filepath.Join(skills, "pipe", "SKILL.md")
	if err := os.Symlink(filepath.Join(l.data, "auth.json"), leak); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grooming, []byte("Find dead code."), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if data, ok, err := e.ReadUpTo(ctx, leak, 100); err != nil || ok || strings.Contains(string(data), "sk-secret-value") {
		t.Fatalf("a skill linked to the sign-ins: %q %v %v", data, ok, err)
	}
	if data, ok, err := e.ReadUpTo(ctx, grooming, 100); err != nil || !ok || string(data) != "Find dead code." {
		t.Fatalf("a skill: %q %v %v", data, ok, err)
	}
	if data, ok, err := e.ReadUpTo(ctx, grooming, 4); err != nil || !ok || string(data) != "Find " {
		t.Fatalf("a skill past its bound: %q %v %v", data, ok, err)
	}
	if err := mkfifo(pipe); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	if data, ok, err := e.ReadUpTo(ctx, pipe, 100); err != nil || ok || len(data) != 0 {
		t.Fatalf("a named pipe: %q %v %v", data, ok, err)
	}
}
