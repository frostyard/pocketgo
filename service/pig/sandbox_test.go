package pig

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
)

func TestSandboxCommandLine(t *testing.T) {
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/bash", Pin: []string{"/h/.local", "/h/.local/state"},
		ReadOnly: []string{"/h/.local/bin/pocketd"}, Hide: []string{"/h/.local/state/pocketgo", "/run/user"}}
	prefix := []string{"/usr/bin/bwrap", "--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc", "--die-with-parent",
		"--bind", "/h/.local", "/h/.local", "--bind", "/h/.local/state", "/h/.local/state",
		"--ro-bind", "/h/.local/bin/pocketd", "/h/.local/bin/pocketd",
		"--tmpfs", "/h/.local/state/pocketgo", "--tmpfs", "/run/user", "--"}
	for _, tc := range []struct {
		command any
		want    []string
	}{
		{"cat x | wc -l", append(slices.Clone(prefix), "/bin/bash", "-c", "cat x | wc -l")},
		{[]string{"git", "status", "--short"}, append(slices.Clone(prefix), "git", "status", "--short")},
	} {
		got, err := s.Command(tc.command)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("Command(%q) = %q, %v\nwant %q", tc.command, got, err, tc.want)
		}
	}
	for _, bad := range []any{[]string{}, 42, nil} {
		if got, err := s.Command(bad); err == nil {
			t.Errorf("Command(%#v) = %q, want an error", bad, got)
		}
	}
	if want := []string{"/h/.local/state/pocketgo", "/run/user", "/h/.local/bin/pocketd"}; !slices.Equal(s.Guarded(), want) {
		t.Errorf("Guarded %q, want %q", s.Guarded(), want)
	}
}

// layout is a service user's home under a temporary root, laid out as the
// team VM's: the data folder in ~/.local/state, the program in ~/.local/bin.
type layout struct {
	root, home, data, runUser, runtime, program string
	// shown are the folders the sandbox shows read-only (ADR-0035): none
	// unless a test names them.
	shown []string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := t.TempDir()
	l := layout{root: root, home: filepath.Join(root, "home"), runUser: filepath.Join(root, "run", "user")}
	l.runtime = filepath.Join(l.runUser, "7")
	l.data = filepath.Join(l.home, ".local", "state", "pocketgo")
	l.program = filepath.Join(l.home, ".local", "bin", "pocketd")
	for _, dir := range []string{l.data, filepath.Dir(l.program), l.runUser} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(l.program, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.data, "auth.json"), []byte(`{"anthropic":"sk-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// paths are the layout's sandbox paths. The folders inside the home are
// the user's; the root is not, as /var/home is not on the VM.
func (l layout) paths(bwrap string) sandboxPaths {
	return sandboxPaths{Bwrap: bwrap, Shell: shell(), Data: l.data, Home: l.home, Runtime: []string{l.runUser}, Program: l.program,
		Profiles: filepath.Join(l.home, ".config", "pi-pocket-team", "agents"), Shown: l.shown,
		ConfigHomes: []string{filepath.Join(l.home, ".config")}, DataHomes: []string{filepath.Join(l.home, ".local", "share")},
		Writable: func(dir string) bool { return within(dir, l.home) }}
}

// The sandbox hides the data folder and the runtime folder, binds what
// decides pocketd's next start read-only, creates the configuration paths a
// command could otherwise create first, and pins every folder above them
// that the user could rename.
func TestNewSandboxNamesPocketdsFiles(t *testing.T) {
	l := newLayout(t)
	p := l.paths("/usr/bin/bwrap")
	p.ConfigHomes = append(p.ConfigHomes, filepath.Join(l.home, "xdg-config"))
	// A relative data folder is the absolute one (pocketd serve -data state).
	t.Chdir(filepath.Join(l.home, ".local"))
	p.Data = filepath.Join("state", "pocketgo")
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{l.data, l.runUser}; !slices.Equal(s.Hide, want) {
		t.Errorf("Hide %q, want %q", s.Hide, want)
	}
	cfg, xdg, share := filepath.Join(l.home, ".config"), filepath.Join(l.home, "xdg-config"), filepath.Join(l.home, ".local", "share")
	envFile := filepath.Join(cfg, "pocketd.env")
	agents := filepath.Join(cfg, "pi-pocket-team", "agents")
	want := []string{l.program, envFile,
		filepath.Join(cfg, "systemd"), filepath.Join(cfg, "environment.d"),
		filepath.Join(xdg, "systemd"), filepath.Join(xdg, "environment.d"),
		filepath.Join(share, "systemd"), agents}
	if !slices.Equal(s.ReadOnly, want) {
		t.Errorf("ReadOnly %q\nwant %q", s.ReadOnly, want)
	}
	for _, path := range want[1:] {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not created: %v", path, err)
		}
	}
	local := filepath.Join(l.home, ".local")
	wantPin := []string{cfg, local, xdg, filepath.Join(cfg, "pi-pocket-team"), filepath.Join(local, "bin"), share, filepath.Join(local, "state")}
	if !slices.Equal(s.Pin, wantPin) {
		t.Errorf("Pin %q\nwant %q", s.Pin, wantPin)
	}

	// A runtime folder that does not exist cannot be hidden: no start.
	p.Runtime = []string{filepath.Join(l.root, "absent")}
	if _, err := newSandbox(p); err == nil {
		t.Error("a missing runtime folder was accepted")
	}
}

// Paths resolve as the kernel resolves them: a link first, then the ".."
// after it, from the link's target.
func TestResolvePathFollowsLinksLikeTheKernel(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"a/b", "x"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../a/b", filepath.Join(root, "x", "link")); err != nil {
		t.Fatal(err)
	}
	// Dangling: c/new does not exist. The kernel goes x/link -> a/b, then
	// .. -> a, then c/new; filepath.Clean would say x/c/new.
	if err := os.Symlink("link/../c/new", filepath.Join(root, "x", "dangling")); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePath(filepath.Join(root, "x", "dangling"))
	if want := filepath.Join(root, "a", "c", "new"); err != nil || got != want {
		t.Fatalf("resolvePath = %q, %v, want %q", got, err, want)
	}
	if got, err := resolvePath(filepath.Join(root, "x", "link", "..", "..", "x")); err != nil || got != filepath.Join(root, "x") {
		t.Fatalf("resolvePath through a link and two .. = %q, %v", got, err)
	}
}

// A folder its owner made read-only is still the owner's to change: it is
// pinned, and a link in it is refused, as in a writable one.
func TestWritableCountsFoldersTheUserOwns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pocketd runs commands on Unix hosts only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root may change any folder")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if !writable(dir) {
		t.Error("a read-only folder the user owns is not writable")
	}
	if writable("/") {
		t.Error("/ is writable")
	}
}

func TestTrustedBwrapIsRootOwned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pocketd runs commands on Unix hosts only")
	}
	saved := statOwner
	t.Cleanup(func() { statOwner = saved })
	statOwner = func(string) (uint32, fs.FileMode, error) { return 0, 0o755, nil }
	if err := rootOwned("/usr/bin"); err != nil {
		t.Errorf("rootOwned with root's folders = %v", err)
	}
	statOwner = func(path string) (uint32, fs.FileMode, error) {
		if path == "/usr" {
			return 1000, 0o755, nil
		}
		return 0, 0o755, nil
	}
	if err := rootOwned("/usr/bin"); err == nil {
		t.Error("a folder a user owns is trusted")
	}
	statOwner = func(path string) (uint32, fs.FileMode, error) { return 0, 0o775, nil }
	if err := rootOwned("/usr/bin"); err == nil {
		t.Error("a folder its group may change is trusted")
	}
	statOwner = saved

	// A link in a folder the user owns leads to root's own program (true,
	// which would pass a trial that only runs a command): refused, since
	// the user could point the link anywhere.
	truth, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true program")
	}
	link := filepath.Join(t.TempDir(), "bwrap")
	if err := os.Symlink(truth, link); err != nil {
		t.Fatal(err)
	}
	if got, err := trustedProgram(link); err == nil {
		t.Errorf("trustedProgram(%s) = %q through a link the user owns", link, got)
	}
	savedPaths := bwrapPaths
	t.Cleanup(func() { bwrapPaths = savedPaths })
	bwrapPaths = []string{link}
	if got, err := trustedBwrap(); err == nil {
		t.Errorf("trustedBwrap = %q with only a user-owned link", got)
	}
}

// A fake bwrap fails the startup check, even one that exits 0.
func TestCheckRefusesAFakeBwrap(t *testing.T) {
	truth, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true program")
	}
	s := &Sandbox{Bwrap: truth, Shell: "sh", Hide: []string{t.TempDir()}}
	if err := s.Check(context.Background()); err == nil {
		t.Fatal("Check passed with true as bwrap")
	}
}

// pocketd refuses to start when a name systemd or pocketd opens passes
// through a link the user could change.
func TestNewSandboxRefusesLinksTheUserCanChange(t *testing.T) {
	l := newLayout(t)
	cfg := filepath.Join(l.home, ".config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(l.root, "elsewhere.env")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(cfg, "pocketd.env")); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a linked env file: %v", err)
	}

	l = newLayout(t)
	real := filepath.Join(l.root, "real-config")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(l.home, ".config")); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a linked ~/.config: %v", err)
	}

	// A unit link in the systemd folder is read where it points: inside the
	// folder it is fine, outside it to a file the user owns it is not.
	l = newLayout(t)
	units := filepath.Join(l.home, ".config", "systemd", "user")
	if err := os.MkdirAll(filepath.Join(units, "default.target.wants"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "own.service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(units, "own.service"), filepath.Join(units, "default.target.wants", "own.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err != nil {
		t.Fatalf("a link inside the systemd folder: %v", err)
	}
	// A link to a file only root can change is fine, as the VM's links to
	// /etc/systemd/user are; so is a unit masked to /dev/null.
	if truth, err := systemProgram("true"); err == nil {
		if err := os.Symlink(truth, filepath.Join(units, "default.target.wants", "root.service")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(os.DevNull, filepath.Join(units, "masked.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err != nil {
		t.Fatalf("unit links to root's files and to /dev/null: %v", err)
	}
	// A link that passes through a link the user owns is refused, wherever
	// it ends.
	hop := filepath.Join(l.root, "hop")
	if err := os.Symlink(os.DevNull, hop); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(units, "default.target.wants", "chain.service")
	if err := os.Symlink(hop, chain); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err == nil || !strings.Contains(err.Error(), "leads through") {
		t.Fatalf("a unit link through a link the user owns: %v", err)
	}
	if err := os.Remove(chain); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(l.root, "outside.service")
	// (Every other link above stays: the next newSandbox sees them too.)
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(units, "default.target.wants", "outside.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err == nil || !strings.Contains(err.Error(), "leads through") {
		t.Fatalf("a unit link to a file the user owns: %v", err)
	}

	// A unit link into the profile folder is refused: that folder is
	// read-only, but systemd would not check the file's other names.
	l = newLayout(t)
	agentsDir := filepath.Join(l.home, ".config", "pi-pocket-team", "agents")
	unitsDir := filepath.Join(l.home, ".config", "systemd", "user")
	for _, dir := range []string{agentsDir, unitsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "unit.service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agentsDir, "unit.service"), filepath.Join(unitsDir, "into-profiles.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err == nil || !strings.Contains(err.Error(), "leads through") {
		t.Fatalf("a unit link into the profile folder: %v", err)
	}

	// The data folder named through a link in a folder only root may
	// change, which leads through a link the user may change: refused.
	l = newLayout(t)
	sys := filepath.Join(l.root, "sys")
	if err := os.MkdirAll(sys, 0o700); err != nil {
		t.Fatal(err)
	}
	userLink := filepath.Join(l.home, ".local", "state", "link")
	if err := os.Symlink(l.data, userLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userLink, filepath.Join(sys, "data")); err != nil {
		t.Fatal(err)
	}
	p := l.paths("/usr/bin/bwrap")
	p.Data = filepath.Join(sys, "data")
	if _, err := newSandbox(p); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a data folder reached through a link the user owns: %v", err)
	}

	// A link in the profile folder to a file the user can change does not
	// stop the start: profiles are read only as regular files, so it is
	// no profile, and the folder itself stays read-only.
	l = newLayout(t)
	agents := filepath.Join(l.home, ".config", "pi-pocket-team", "agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(l.root, "stray.md")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stray, filepath.Join(agents, "vera.md.bak")); err != nil {
		t.Fatal(err)
	}
	if s, err := newSandbox(l.paths("/usr/bin/bwrap")); err != nil || !slices.Contains(s.ReadOnly, agents) {
		t.Fatalf("a stray link in the profile folder: %v", err)
	}

	// A bind mount of one protected file cannot be masked by a tmpfs: no
	// start.
	l = newLayout(t)
	alias := filepath.Join(l.root, "alias.json")
	if err := os.WriteFile(alias, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p = l.paths("/usr/bin/bwrap")
	p.Mounts = []mount{{Device: "d", Root: "/", Point: "/"}, {Device: "d", Root: filepath.Join(l.data, "auth.json"), Point: alias}}
	if _, err := newSandbox(p); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("a bind mount of a protected file: %v", err)
	}
}

// Every mount that shows a protected path is masked: on the team VM, /home
// is a bind mount of /var/home.
func TestWithAliasesFindsEveryMountOfAPath(t *testing.T) {
	mounts, err := parseMountinfo(strings.NewReader(`26 1 0:39 / / rw,relatime - btrfs /dev/sda2 rw
30 26 0:34 /@/state/os/default/var /var rw,relatime - btrfs /dev/sda2 rw
31 26 0:34 /@/state/os/default/var/home /home rw,relatime - btrfs /dev/sda2 rw
32 26 0:34 /@/state/deploy/abc/etc /etc rw,relatime - btrfs /dev/sda2 rw
33 26 0:34 /@/state/os/default/var/srv /srv\040data rw,relatime - btrfs /dev/sda2 rw
34 26 0:34 /@/state/os/default/var/home/team/.local/state/pocketgo/sub /mnt/sub rw,relatime - btrfs /dev/sda2 rw
35 26 0:34 /@/state/os/default/var/home/team/.config/pocketd.env /etc/alias.env rw,relatime - btrfs /dev/sda2 rw
40 26 0:25 / /run rw,nosuid - tmpfs tmpfs rw
41 40 0:50 / /run/user/1001 rw,nosuid - tmpfs tmpfs rw
42 26 0:50 / /srv/runtime rw,nosuid - tmpfs tmpfs rw
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := mounts[4].Point; got != "/srv data" {
		t.Errorf("unescaped mount point %q", got)
	}
	for _, tc := range []struct {
		path string
		want []string
	}{
		// A bind mount of a folder inside the data folder shows part of it.
		{"/var/home/team/.local/state/pocketgo", []string{"/var/home/team/.local/state/pocketgo", "/home/team/.local/state/pocketgo", "/mnt/sub"}},
		// A bind mount of the one file shows it.
		{"/home/team/.config/pocketd.env", []string{"/home/team/.config/pocketd.env", "/var/home/team/.config/pocketd.env", "/etc/alias.env"}},
		{"/var/srv/x", []string{"/var/srv/x", "/srv data/x"}},
		{"/etc/pocketd.env", []string{"/etc/pocketd.env"}},
		// A runtime folder's own tmpfs inside /run/user, bound elsewhere.
		{"/run/user", []string{"/run/user", "/srv/runtime"}},
		{"/usr/bin/pocketd", []string{"/usr/bin/pocketd"}},
	} {
		if got := withAliases([]string{tc.path}, mounts); !slices.Equal(got, tc.want) {
			t.Errorf("withAliases(%s) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestOpenRequiresASandbox(t *testing.T) {
	f := newFixture(t)
	_, err := open(context.Background(), Options{DataDir: f.dataDir, Approver: f.approve, NeedsApproval: approveChanges}, fauxModels())
	if err == nil || !strings.Contains(err.Error(), "Sandbox") {
		t.Fatalf("open without a sandbox: %v", err)
	}
}

// usableSandbox is the layout's sandbox. It skips only where bwrap cannot
// make any sandbox here (not installed, or user namespaces off); on a host
// that can, a failure of the sandbox's own check fails the test.
func usableSandbox(t *testing.T, l layout) *Sandbox {
	t.Helper()
	bwrap, err := trustedBwrap()
	if err != nil {
		t.Skipf("no usable bwrap: %v", err)
	}
	if out, err := exec.Command(bwrap, "--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc", "--", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot make a sandbox here (user namespaces may be off): %v: %s", err, out)
	}
	s, err := newSandbox(l.paths(bwrap))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Fatalf("the sandbox fails on a host that can run bwrap: %v", err)
	}
	return s
}

// A command in the sandbox cannot see the data folder or the runtime
// folder, cannot change pocketd's start files, cannot move a folder above
// them, and sees only its own processes; the next command finds them all as
// they were.
func TestCommandsCannotReachPocketdsFiles(t *testing.T) {
	l, work := newLayout(t), t.TempDir()
	s := usableSandbox(t, l)
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	run := func(command any) (string, int) {
		t.Helper()
		var mu sync.Mutex
		var out strings.Builder
		result, err := g.Exec(context.Background(), command, &env.ShellExecOptions{OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
			mu.Lock()
			out.WriteString(text)
			mu.Unlock()
		}})
		if err != nil {
			t.Fatalf("Exec(%q): %v", command, err)
		}
		return out.String(), result.ExitCode
	}
	// A runtime folder appears after pocketd started, as at a login: the
	// whole of /run/user is masked, so it stays hidden.
	if err := os.MkdirAll(l.runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.runtime, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	local, cfg := filepath.Join(l.home, ".local"), filepath.Join(l.home, ".config")
	out, _ := run(`mv "` + filepath.Join(local, "state") + `" "` + filepath.Join(local, "moved") + `" && echo LEAK-1; ` +
		`mv "` + local + `" "` + filepath.Join(l.home, "moved") + `" && echo LEAK-2; ` +
		`mv "` + cfg + `" "` + filepath.Join(l.home, "moved-config") + `" && echo LEAK-3; ` +
		`echo POCKETD_LISTEN=0.0.0.0:8788 >> "` + filepath.Join(cfg, "pocketd.env") + `" && echo LEAK-4; ` +
		`touch "` + filepath.Join(cfg, "systemd", "evil.service") + `" && echo LEAK-5; ` +
		`echo "role: supervisor" >> "` + filepath.Join(cfg, "pi-pocket-team", "agents", "vera.md") + `" && echo LEAK-6; ` +
		`touch "` + filepath.Join(local, "state", "other") + `" && echo OK-state; ` +
		`echo "runtime=$(ls -A "` + l.runUser + `" | wc -l)"; pwd; echo "init=$(cat /proc/1/comm)"`)
	for _, want := range []string{"OK-state", "runtime=0", work + "\n", "init=bwrap\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("first command's output lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"LEAK-"} {
		if strings.Contains(out, leak) {
			t.Errorf("first command's output has %q:\n%s", leak, out)
		}
	}

	// The next command finds the data folder hidden where it was, as a
	// string and as an argv.
	out, _ = run(`cat "` + filepath.Join(l.data, "auth.json") + `"; echo "files=$(ls -A "` + l.data + `" | wc -l)"`)
	if strings.Contains(out, "sk-secret-value") || !strings.Contains(out, "files=0") {
		t.Errorf("second command's output:\n%s", out)
	}
	if out, code := run([]string{"cat", filepath.Join(l.data, "auth.json")}); code == 0 || strings.Contains(out, "sk-secret-value") {
		t.Errorf("argv cat exited %d with %q", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(l.data, "auth.json")); err != nil || !strings.Contains(string(b), "sk-secret-value") {
		t.Errorf("the sign-ins moved or changed on the host: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(cfg, "pocketd.env")); err != nil || len(b) != 0 {
		t.Errorf("the env file changed: %q, %v", b, err)
	}
}

// The file tools refuse what the sandbox protects from commands: a write
// to pocketd's env file, and a rename or removal of a folder that holds the
// data folder.
func TestFileToolsRefuseWhatTheSandboxProtects(t *testing.T) {
	l := newLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap")) // never run: file tools do not start commands
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	envFile := filepath.Join(l.home, ".config", "pocketd.env")
	// A link in the workspace to a unit that does not exist yet: writing
	// it would create the unit.
	unit := filepath.Join(l.home, ".config", "systemd", "evil.service")
	if err := os.Symlink(unit, filepath.Join(f.work, "unit.service")); err != nil {
		t.Fatal(err)
	}
	e, err := open(context.Background(), Options{DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve,
		NeedsApproval: approvals.NeedsApproval, Sandbox: s},
		fauxModels(toolTurn("write", map[string]any{"path": envFile, "content": "POCKETD_LISTEN=0.0.0.0:8788\n"}, "w1"), answer("done"),
			toolTurn("write", map[string]any{"path": "unit.service", "content": "[Service]\nExecStart=/bin/true\n"}, "w2"), answer("done")))
	if err != nil {
		t.Fatal(err)
	}
	e.Resume()
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:w", "change pocketd's settings"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, _, _ := e.Transcript(ctx, id)
	var result *proto.ToolResult
	for _, entry := range entries {
		if entry.ToolResult != nil {
			result = entry.ToolResult
		}
	}
	if result == nil || !result.IsError {
		t.Fatalf("write to the env file: %+v", result)
	}
	if b, err := os.ReadFile(envFile); err != nil || len(b) != 0 {
		t.Fatalf("the env file changed: %q, %v", b, err)
	}
	if _, err := e.Send(ctx, id, "u:dev:w2", "write the unit through the link"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, _, _ = e.Transcript(ctx, id)
	result = nil
	for _, entry := range entries {
		if entry.ToolResult != nil {
			result = entry.ToolResult
		}
	}
	if result == nil || !result.IsError {
		t.Fatalf("write through a dangling link: %+v", result)
	}
	if _, err := os.Lstat(unit); !os.IsNotExist(err) {
		t.Fatalf("the unit was created through the link: %v", err)
	}

	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: f.work}), protected: s.Guarded()}
	local := filepath.Join(l.home, ".local")
	if err := g.RenameFile(ctx, local, filepath.Join(l.home, "moved")); !errors.Is(err, errProtected) {
		t.Errorf("renaming the folder above the data folder: %v", err)
	}
	if err := g.Remove(ctx, filepath.Join(local, "state"), &env.RemoveOptions{Recursive: true}); !errors.Is(err, errProtected) {
		t.Errorf("removing the folder above the data folder: %v", err)
	}
	// A link inside a protected folder that leads outside it: removing or
	// renaming acts on the link, which is protected.
	inside := filepath.Join(l.home, ".config", "systemd", "out.service")
	if err := os.Symlink(filepath.Join(f.work, "elsewhere"), inside); err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(ctx, inside, nil); !errors.Is(err, errProtected) {
		t.Errorf("removing a link in a protected folder: %v", err)
	}
	if err := g.RenameFile(ctx, inside, filepath.Join(f.work, "taken")); !errors.Is(err, errProtected) {
		t.Errorf("renaming a link in a protected folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.data, "auth.json")); err != nil {
		t.Fatalf("the data folder moved: %v", err)
	}
}

// With pocketd's rule, a session's bash call runs at once, without an
// approval, and in the sandbox.
func TestABashCallRunsSandboxedWithoutWaiting(t *testing.T) {
	l := newLayout(t)
	s := usableSandbox(t, l)
	f := newFixture(t)
	e, err := open(context.Background(), Options{
		DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve,
		NeedsApproval: approvals.NeedsApproval, Sandbox: s,
	}, fauxModels(toolTurn("bash", map[string]any{"command": "cat " + filepath.Join(l.data, "auth.json")}, "b1"), answer("done")))
	if err != nil {
		t.Fatal(err)
	}
	e.Resume()
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:cat", "read the sign-ins"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	select {
	case a := <-f.requests:
		t.Fatalf("the call waited for an approval: %+v", a)
	default:
	}
	entries, _, _ := e.Transcript(ctx, id)
	var result *proto.ToolResult
	for _, entry := range entries {
		if entry.ToolResult != nil {
			result = entry.ToolResult
		}
	}
	if result == nil || strings.Contains(result.Text, "sk-secret-value") || !strings.Contains(result.Text, "No such file") {
		t.Fatalf("bash result %+v", result)
	}
}

// A call that asked for an approval under the old rule still waits for it
// after an upgrade to a rule that asks for none, and the owner's denial
// applies.
func TestAnApprovalAskedBeforeAnUpgradeStillApplies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.open(fauxModels(toolTurn("write", map[string]any{"path": "after.txt", "content": "x\n"}, "w1")))
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:r1", "write after.txt"); err != nil {
		t.Fatal(err)
	}
	before := f.nextApproval()
	closeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := e.Close(closeCtx); err != nil {
		t.Fatal(err)
	}

	upgraded := f.openWith(fauxModels(answer("Not written.")), approvals.NeedsApproval)
	defer func() { _ = upgraded.Close(context.Background()) }()
	if err := upgraded.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if again := f.nextApproval(); again.ID != before.ID {
		t.Fatalf("the resumed call asked for %s, not %s", again.ID, before.ID)
	}
	if _, _, err := f.approve.Decide(ctx, "k-"+before.ID, before.ID, false, "laptop", "not now"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, upgraded, id)
	if _, err := os.Stat(filepath.Join(f.work, "after.txt")); !os.IsNotExist(err) {
		t.Fatalf("the denied call ran after the upgrade: %v", err)
	}
	entries, _, _ := upgraded.Transcript(ctx, id)
	var result *proto.ToolResult
	for _, entry := range entries {
		if entry.ToolResult != nil {
			result = entry.ToolResult
		}
	}
	if result == nil || !result.IsError || !strings.Contains(result.Text, "not now") {
		t.Fatalf("the denied call's result %+v", result)
	}
}

// The agents' instructions promise no approval: whether a call waits is
// NeedsApproval's to say, in code (ADR-0011, ADR-0012).
func TestInstructionsPromiseNoApproval(t *testing.T) {
	if text := instructions("/work"); strings.Contains(strings.ToLower(text), "approv") {
		t.Errorf("instructions mention an approval: %q", text)
	}
}

// A session created while approvals waited still carried instructions that
// promised them: Watch, which runs for every session at start, replaces
// them with the current ones. Instructions someone changed stay.
func TestWatchRefreshesInstructionsThatPromisedApprovals(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	instructionsOf := func(id int64) string {
		t.Helper()
		conversation, err := e.conversation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		agent, err := conversation.Agent(ctx)
		if err != nil || agent.Instructions == nil {
			t.Fatalf("agent %+v, %v", agent, err)
		}
		return *agent.Instructions
	}
	set := func(id int64, text string) {
		t.Helper()
		conversation, err := e.conversation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := conversation.Configure(ctx, harness.AgentChange{Instructions: harness.SetTo(text)}); err != nil {
			t.Fatal(err)
		}
	}
	old, err := e.CreateSession(ctx, t.Name()+"-old", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	set(old, approvalInstructions(f.work))
	custom, err := e.CreateSession(ctx, t.Name()+"-custom", api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	set(custom, "Wait for the owner before every write.")
	for _, id := range []int64{old, custom} {
		if err := e.Watch(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := instructionsOf(old); got != instructions(f.work) {
		t.Errorf("old session's instructions %q", got)
	}
	if got := instructionsOf(custom); got != "Wait for the owner before every write." {
		t.Errorf("changed instructions were replaced: %q", got)
	}
}
