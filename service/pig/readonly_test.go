package pig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/uploads"
)

// newShownLayout is a layout whose sandbox shows the skills folder and the
// uploads folder read-only, as pocketd serve names them (ADR-0035), an
// empty name among them; neither exists yet, as on a host's first start.
func newShownLayout(t *testing.T) (l layout, skills, uploadsDir string) {
	t.Helper()
	l = newLayout(t)
	skills = filepath.Join(l.home, ".pi", "agent", "skills")
	uploadsDir = uploads.Dir(l.home)
	l.shown = []string{skills, "", uploadsDir}
	return l, skills, uploadsDir
}

// The sandbox shows the skills folder and the uploads folder read-only: it
// makes each that is missing, empty, so a host without them starts and no
// command makes one first; it binds them read-only after pocketd's start
// files and before the masks; it pins the folders above them that the
// user could rename; and it never hides them, nor has the file tools
// refuse to read them (ADR-0035).
func TestNewSandboxShowsTheSkillsAndUploadsReadOnly(t *testing.T) {
	l, skills, up := newShownLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{skills, up}; !slices.Equal(s.Shown, want) {
		t.Fatalf("Shown %q, want %q", s.Shown, want)
	}
	for _, dir := range s.Shown {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s was not made a private folder: %v, %v", dir, info, err)
		}
		if s.Hides(dir) || slices.Contains(s.Guarded(), dir) {
			t.Errorf("%s is hidden from the agents or refused to the file tools", dir)
		}
	}
	pi := filepath.Join(l.home, ".pi")
	for _, dir := range []string{pi, filepath.Join(pi, "agent"), filepath.Join(l.home, ".local", "share")} {
		if !slices.Contains(s.Pin, dir) {
			t.Errorf("Pin %q lacks %s", s.Pin, dir)
		}
	}
	args, err := s.Command("true")
	if err != nil {
		t.Fatal(err)
	}
	at := func(flag, path string) int {
		for i := 0; i+2 < len(args); i++ {
			if args[i] == flag && args[i+1] == path && args[i+2] == path {
				return i
			}
		}
		return -1
	}
	lastReadOnly := at("--ro-bind", s.ReadOnly[len(s.ReadOnly)-1])
	firstMask := slices.Index(args, "--tmpfs")
	for _, dir := range s.Shown {
		if i := at("--ro-bind", dir); i < 0 || i < lastReadOnly || i > firstMask {
			t.Errorf("%s is not bound read-only between the start files and the masks: %q", dir, args)
		}
	}
}

// A protected tree a bind mount also shows below a folder of the user's
// keeps that folder pinned too: renamed, it would take the alias out from
// under its mask or read-only bind (Codex review of #63, round 1).
func TestTheFoldersAboveAnAliasArePinned(t *testing.T) {
	l, skills, _ := newShownLayout(t)
	skillsAlias := filepath.Join(l.home, "mnt", "skills")
	dataAlias := filepath.Join(l.home, "other", "state")
	for _, dir := range []string{skills, skillsAlias, dataAlias} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := l.paths("/usr/bin/bwrap")
	p.Mounts = []mount{{Device: "d", Root: "/", Point: "/"}, {Device: "d", Root: skills, Point: skillsAlias}, {Device: "d", Root: l.data, Point: dataAlias}}
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Shown, skillsAlias) || !slices.Contains(s.Hide, dataAlias) {
		t.Fatalf("the aliases are not protected: Shown %q, Hide %q", s.Shown, s.Hide)
	}
	for _, dir := range []string{filepath.Join(l.home, "mnt"), filepath.Join(l.home, "other")} {
		if !slices.Contains(s.Pin, dir) {
			t.Errorf("Pin %q lacks %s, above an alias", s.Pin, dir)
		}
	}
}

// pocketd refuses to start when a link the user could change lies on the
// way to the skills folder: an agent could point it at a folder of its
// own, which the next start would show as the skills (ADR-0035). A link
// only root can change is fine, and the folder it leads to is the one kept
// read-only.
func TestNewSandboxRefusesALinkedShownFolder(t *testing.T) {
	l, skills, _ := newShownLayout(t)
	checkout := filepath.Join(l.home, "config", "skills")
	for _, dir := range []string{checkout, filepath.Dir(skills)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(checkout, skills); err != nil {
		t.Fatal(err)
	}
	if _, err := newSandbox(l.paths("/usr/bin/bwrap")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a skills folder linked from a folder the user may change: %v", err)
	}

	l, _, up := newShownLayout(t)
	system := filepath.Join(l.root, "srv")
	if err := os.MkdirAll(system, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(system, "skills")
	real := filepath.Join(l.home, "skills")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	l.shown = []string{link, up}
	s, err := newSandbox(l.paths("/usr/bin/bwrap"))
	if err != nil {
		t.Fatalf("a skills folder linked from a folder only root may change: %v", err)
	}
	if want := []string{real, up}; !slices.Equal(s.Shown, want) {
		t.Fatalf("Shown %q, want %q", s.Shown, want)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		t.Fatalf("the folder the link leads to was not made: %v", err)
	}
}

// A shown folder the owner removed by hand while pocketd runs is made
// again, empty, before the next command: bwrap cannot bind a folder that
// is not there, and a command must not be the one to make it (ADR-0035).
func TestAShownFolderRemovedIsMadeAgain(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "share", "pocketgo-uploads")
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/sh", Shown: []string{dir}}
	args, err := s.Command("true")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the removed folder was not made again: %v, %v", info, err)
	}
	if !slices.Contains(args, dir) || args[slices.Index(args, dir)-1] != "--ro-bind" {
		t.Fatalf("the folder is not bound read-only: %q", args)
	}
	// One that cannot be made stops the command, rather than run it with
	// the folder unbound.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Shown = []string{filepath.Join(blocked, "skills")}
	if args, err := s.Command("true"); err == nil {
		t.Fatalf("a folder that cannot be made gave %q", args)
	}
}

// A replaced shown folder is kept anew only once the running commands
// were stopped: a stop that failed is tried again at the next look, which
// still finds the replacement (Codex review of #63, round 2).
func TestAReplacementIsKeptOnlyOnceTheCommandsStopped(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pocketgo-uploads")
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/sh", Shown: []string{dir}}
	stops := 0
	saved := stopRunning
	t.Cleanup(func() { stopRunning = saved })
	stopRunning = func(string) error {
		stops++
		if stops == 1 {
			return errors.New("out of file descriptors")
		}
		return nil
	}
	// The folder is missing: the stop before it is made fails, so nothing
	// is made or kept.
	if err := s.keepShown(); err == nil || stops != 1 {
		t.Fatalf("the first look: %v, %d stops", err, stops)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the folder was made though the stop failed: %v", err)
	}
	if err := s.keepShown(); err != nil || stops != 2 {
		t.Fatalf("the look after a failed stop: %v, %d stops", err, stops)
	}
	if err := s.keepShown(); err != nil || stops != 2 {
		t.Fatalf("a look with nothing changed: %v, %d stops", err, stops)
	}
	// The owner replaces it; the stop fails once, then works.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stops = 0
	if err := s.keepShown(); err == nil || stops != 1 {
		t.Fatalf("a replacement whose stop failed: %v, %d stops", err, stops)
	}
	if err := s.keepShown(); err != nil || stops != 2 {
		t.Fatalf("the look after: %v, %d stops", err, stops)
	}
	if err := s.keepShown(); err != nil || stops != 2 {
		t.Fatalf("the replacement kept: %v, %d stops", err, stops)
	}
}

// A replaced shown folder that a mount shows somewhere this sandbox does
// not keep read-only, as one bind-mounted elsewhere before the owner moved
// it in, is not kept: commands stop, and so do the file tools' changes,
// until the mount goes or a restart binds it too (Codex review of #63,
// round 3).
func TestAReplacementAMountShowsElsewhereIsNotKept(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pocketgo-uploads")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "elsewhere")
	table := "1 0 0:9 / / rw - btrfs /dev/sda2 rw\n"
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/sh", Shown: []string{dir},
		mounts: func() ([]mount, error) { return parseMountinfo(strings.NewReader(table)) }}
	saved := stopRunning
	t.Cleanup(func() { stopRunning = saved })
	stopRunning = func(string) error { return nil }
	if err := s.keepShown(); err != nil {
		t.Fatal(err)
	}
	// The owner moves in a folder a mount shows elsewhere too.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	table += "2 1 0:9 " + dir + " " + alias + " rw - btrfs /dev/sda2 rw\n"
	if err := s.keepShown(); err == nil || !strings.Contains(err.Error(), "a mount shows it at "+alias) {
		t.Fatalf("a replacement shown elsewhere: %v", err)
	}
	work := t.TempDir()
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), readOnly: s.Shown, sandbox: s}
	if err := g.WriteFile(context.Background(), filepath.Join(work, "notes.txt"), "x"); !errors.Is(err, errReadOnly) {
		t.Fatalf("a file tool's change while the replacement is not kept: %v", err)
	}
	if args, err := s.Command("true"); err == nil {
		t.Fatalf("a command while the replacement is not kept: %q", args)
	}
	// The mount goes: the replacement is kept, and work goes on.
	table = "1 0 0:9 / / rw - btrfs /dev/sda2 rw\n"
	if err := s.keepShown(); err != nil {
		t.Fatal(err)
	}
	if err := g.WriteFile(context.Background(), filepath.Join(work, "notes.txt"), "x"); err != nil {
		t.Fatal(err)
	}
}

// A shown folder's aliases come from the mount the folder is on as the
// kernel walks to it: a mount with a longer point that a later mount
// covers shows another file system (Codex review of #63, round 3).
func TestAliasesComeFromTheMountAPathIsOn(t *testing.T) {
	mounts, err := parseMountinfo(strings.NewReader(`1 0 0:1 / / rw - tmpfs tmpfs rw
2 1 0:2 / / rw - btrfs /dev/sda2 rw
3 2 0:3 / /home/u/a/b rw - ext4 /dev/sdb rw
4 2 0:4 / /home/u/a rw - ext4 /dev/sdc rw
5 2 0:4 / /mnt/c rw - ext4 /dev/sdc rw
6 2 0:3 / /mnt/covered rw - ext4 /dev/sdb rw
`))
	if err != nil {
		t.Fatal(err)
	}
	got := withAliases([]string{"/home/u/a/b/skills"}, mounts)
	if want := []string{"/home/u/a/b/skills", "/mnt/c/b/skills"}; !slices.Equal(got, want) {
		t.Fatalf("withAliases = %q, want %q", got, want)
	}
}

// A single file a mount shows from a shown folder is held and kept as
// such; a folder pocketd named must stay a folder (Codex review of #63,
// round 3).
func TestAFileAMountShowsIsKept(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "skills")
	file := filepath.Join(root, "SKILL.md")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("Find dead code."), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/sh", Shown: []string{dir, file}, roots: []string{dir}}
	if err := s.keepShown(); err != nil {
		t.Fatalf("a file a mount shows: %v", err)
	}
	if s.kept.held[file] == nil || s.kept.held[dir] == nil {
		t.Fatalf("held %v", s.kept.held)
	}
	// The folder pocketd named becomes a file: it is not kept.
	saved := stopRunning
	t.Cleanup(func() { stopRunning = saved })
	stopRunning = func(string) error { return nil }
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.keepShown(); err == nil || !strings.Contains(err.Error(), "no folder") {
		t.Fatalf("a named folder made a file: %v", err)
	}
}

// The file tools watch the shown paths as the sandbox checked them, never
// resolved again: a link the owner put in one's place between the check
// and the engine's open is seen, and stops their changes (Codex review of
// #63, round 3).
func TestTheFileToolsWatchTheCheckedPaths(t *testing.T) {
	l, skills, _ := newShownLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap")) // never run: no command starts
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(l.home, "moved-skills")
	if err := os.Rename(skills, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, skills); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	e, err := open(context.Background(), Options{DataDir: f.dataDir, Approver: f.approve, NeedsApproval: approvals.NeedsApproval, Sandbox: s}, fauxModels())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	if !slices.Equal(e.readOnly, s.Shown) {
		t.Fatalf("the file tools watch %q, the sandbox checked %q", e.readOnly, s.Shown)
	}
}

// Each kept shown folder is held open, so its inode cannot be used again
// while it is kept: a folder made at its path after it went is always
// told apart (Codex review of #63, round 2).
func TestAKeptFolderIsHeldOpen(t *testing.T) {
	l, skills, up := newShownLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{skills, up} {
		held := s.kept.held[dir]
		if held == nil {
			t.Fatalf("%s is not held", dir)
		}
		heldInfo, err := held.Stat()
		named, err2 := os.Stat(dir)
		if err != nil || err2 != nil || !os.SameFile(heldInfo, named) {
			t.Fatalf("%s is held as another folder: %v, %v", dir, err, err2)
		}
	}
	saved := stopRunning
	t.Cleanup(func() { stopRunning = saved })
	stopRunning = func(string) error { return nil }
	if err := os.Remove(up); err != nil {
		t.Fatal(err)
	}
	old := s.kept.held[up]
	if err := os.Mkdir(up, 0o700); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := old.Stat()
	if err != nil {
		t.Fatalf("the removed folder is no longer held: %v", err)
	}
	if named, err := os.Stat(up); err != nil || os.SameFile(oldInfo, named) {
		t.Fatalf("the new folder took the held one's inode: %v", err)
	}
	if err := s.keepShown(); err != nil {
		t.Fatal(err)
	}
	if heldInfo, err := s.kept.held[up].Stat(); err != nil || os.SameFile(heldInfo, oldInfo) {
		t.Fatalf("the new folder is not the one held: %v", err)
	}
}

// A shown folder, or a folder above it, that the owner replaced with a
// symbolic link while pocketd runs stops every command until a restart:
// the bind would follow the link where the file tools do not guard, and
// the next start refuses a link the user can change (ADR-0035).
func TestALinkPutOnTheWayToAShownFolderStopsCommands(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(root, "share")
	dir := filepath.Join(share, "pocketgo-uploads")
	elsewhere := filepath.Join(root, "elsewhere")
	for _, d := range []string{dir, filepath.Join(elsewhere, "pocketgo-uploads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/sh", Shown: []string{dir}}
	if _, err := s.Command("true"); err != nil {
		t.Fatalf("the folder where it was: %v", err)
	}
	// The folder itself becomes a link.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "pocketgo-uploads"), dir); err != nil {
		t.Fatal(err)
	}
	if args, err := s.Command("true"); err == nil || !strings.Contains(err.Error(), "restart pocketd") {
		t.Fatalf("a folder replaced with a link gave %q, %v", args, err)
	}
	// A folder above it becomes a link.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(share, filepath.Join(root, "old-share")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, share); err != nil {
		t.Fatal(err)
	}
	if args, err := s.Command("true"); err == nil || !strings.Contains(err.Error(), "restart pocketd") {
		t.Fatalf("a folder above it replaced with a link gave %q, %v", args, err)
	}
}

// A command in the sandbox reads the uploads and the skills, but cannot
// write, remove, rename or link anything in them, nor move the folders
// above them; pocketd itself still stores uploads there; and a folder the
// owner removed is there again, read-only, for the next command
// (ADR-0035).
func TestCommandsCannotChangeTheShownFolders(t *testing.T) {
	l, skills, up := newShownLayout(t)
	work := t.TempDir()
	s := usableSandbox(t, l)
	folder := uploads.New(up, filepath.Join(l.data, "upload-staging"), nil)
	file, _, err := folder.Store(7, "shot.png", strings.NewReader("PNG-BYTES"))
	if err != nil {
		t.Fatalf("pocketd cannot store an upload once the sandbox shows the folder: %v", err)
	}
	upload := folder.Path(7, file)
	skill := filepath.Join(skills, "triage", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("Triage the issues."), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	run := func(command string) string {
		t.Helper()
		var mu sync.Mutex
		var out strings.Builder
		if _, err := g.Exec(context.Background(), command, &env.ShellExecOptions{OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
			mu.Lock()
			out.WriteString(text)
			mu.Unlock()
		}}); err != nil {
			t.Fatalf("Exec(%q): %v", command, err)
		}
		return out.String()
	}
	q := func(path string) string { return `"` + path + `"` }
	pi := filepath.Join(l.home, ".pi")
	out := run(`echo x >> ` + q(upload) + ` && echo LEAK-1; ` +
		`rm -f ` + q(upload) + ` && echo LEAK-2; ` +
		`touch ` + q(filepath.Join(up, "7", "new")) + ` && echo LEAK-3; ` +
		`mkdir ` + q(filepath.Join(up, "8")) + ` && echo LEAK-4; ` +
		`mv ` + q(filepath.Join(up, "7")) + ` ` + q(filepath.Join(up, "moved")) + ` && echo LEAK-5; ` +
		`mv ` + q(up) + ` ` + q(filepath.Join(l.home, "moved")) + ` && echo LEAK-6; ` +
		`ln ` + q(upload) + ` ` + q(filepath.Join(work, "hard")) + ` && echo LEAK-7; ` +
		`echo evil > ` + q(skill) + ` && echo LEAK-8; ` +
		`mkdir ` + q(filepath.Join(skills, "evil")) + ` && echo LEAK-9; ` +
		`rm -rf ` + q(filepath.Join(skills, "triage")) + ` && echo LEAK-10; ` +
		`ln -s /etc ` + q(filepath.Join(skills, "link")) + ` && echo LEAK-11; ` +
		`mv ` + q(pi) + ` ` + q(filepath.Join(l.home, "moved-pi")) + ` && echo LEAK-12; ` +
		`mv ` + q(filepath.Join(pi, "agent")) + ` ` + q(filepath.Join(pi, "moved")) + ` && echo LEAK-13; ` +
		`touch ` + q(filepath.Join(l.home, ".local", "share", "other")) + ` && echo OK-share; ` +
		`cat ` + q(upload) + `; echo; cat ` + q(skill))
	for _, want := range []string{"OK-share", "PNG-BYTES", "Triage the issues."} {
		if !strings.Contains(out, want) {
			t.Errorf("the command's output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LEAK-") {
		t.Errorf("a command changed a shown folder:\n%s", out)
	}
	if b, err := os.ReadFile(upload); err != nil || string(b) != "PNG-BYTES" {
		t.Errorf("the upload changed on the host: %q, %v", b, err)
	}
	if b, err := os.ReadFile(skill); err != nil || string(b) != "Triage the issues." {
		t.Errorf("the skill changed on the host: %q, %v", b, err)
	}
	for _, path := range []string{filepath.Join(up, "8"), filepath.Join(skills, "evil"), filepath.Join(skills, "link"), filepath.Join(work, "hard")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was made: %v", path, err)
		}
	}

	// The owner removes the whole uploads folder by hand: the next command
	// runs, and finds it there again, empty and read-only.
	if err := os.RemoveAll(up); err != nil {
		t.Fatal(err)
	}
	out = run(`touch ` + q(filepath.Join(up, "planted")) + ` && echo LEAK-14; echo "uploads=$(ls -A ` + q(up) + ` | wc -l)"`)
	if strings.Contains(out, "LEAK-") || !strings.Contains(out, "uploads=0") {
		t.Errorf("after the owner removed the uploads folder:\n%s", out)
	}
	if entries, err := os.ReadDir(up); err != nil || len(entries) != 0 {
		t.Errorf("the uploads folder made again holds %v, %v", entries, err)
	}
}

// The file tools read the uploads and the skills, but refuse to change
// them: a write, an append, a truncation, a flush or a new folder there, a
// write through a link that leads there, a rename out of or into one, a
// removal there, and a rename or removal of a folder that holds one. A
// session's write tool is refused so too, and its read is not
// (ADR-0035).
func TestFileToolsReadButNeverChangeTheShownFolders(t *testing.T) {
	l, skills, up := newShownLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap")) // never run: file tools do not start commands
	if err != nil {
		t.Fatal(err)
	}
	folder := uploads.New(up, filepath.Join(l.data, "upload-staging"), nil)
	file, _, err := folder.Store(7, "build.log", strings.NewReader("FAIL: TestX"))
	if err != nil {
		t.Fatal(err)
	}
	upload := folder.Path(7, file)
	skill := filepath.Join(skills, "triage", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("Triage the issues."), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFixture(t)
	e, err := open(context.Background(), Options{DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve,
		NeedsApproval: approvals.NeedsApproval, Sandbox: s},
		fauxModels(toolTurn("write", map[string]any{"path": upload, "content": "PASS"}, "w1"), answer("done"),
			toolTurn("read", map[string]any{"path": upload}, "r1"), answer("done")))
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
	last := func() *proto.ToolResult {
		t.Helper()
		waitIdle(t, e, id)
		entries, _, _ := e.Transcript(ctx, id)
		var result *proto.ToolResult
		for _, entry := range entries {
			if entry.ToolResult != nil {
				result = entry.ToolResult
			}
		}
		return result
	}
	if _, err := e.Send(ctx, id, "u:dev:w", "fix the log"); err != nil {
		t.Fatal(err)
	}
	if result := last(); result == nil || !result.IsError {
		t.Fatalf("a write to an upload: %+v", result)
	}
	if _, err := e.Send(ctx, id, "u:dev:r", "read the log"); err != nil {
		t.Fatal(err)
	}
	if result := last(); result == nil || result.IsError {
		t.Fatalf("a read of an upload: %+v", result)
	}

	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: f.work}), protected: s.Guarded(), readOnly: s.Shown}
	for _, path := range []string{upload, skill} {
		if _, err := g.ReadTextFile(ctx, path); err != nil {
			t.Errorf("reading %s: %v", path, err)
		}
	}
	link := filepath.Join(f.work, "log")
	if err := os.Symlink(upload, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.work, "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	away := filepath.Join(skills, "away")
	if err := os.Symlink(outside, away); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func() error{
		"write":                   func() error { return g.WriteFile(ctx, upload, "PASS") },
		"append":                  func() error { return g.AppendFile(ctx, upload, "PASS") },
		"truncate":                func() error { return g.TruncateFile(ctx, upload, 0) },
		"flush":                   func() error { return g.FlushFile(ctx, upload) },
		"write a new skill":       func() error { return g.WriteFile(ctx, filepath.Join(skills, "evil", "SKILL.md"), "evil") },
		"make a folder":           func() error { return g.CreateDir(ctx, filepath.Join(up, "8"), nil) },
		"write through a link":    func() error { return g.WriteFile(ctx, link, "PASS") },
		"rename out":              func() error { return g.RenameFile(ctx, upload, filepath.Join(f.work, "taken")) },
		"rename in":               func() error { return g.RenameFile(ctx, outside, filepath.Join(up, "7", "planted")) },
		"remove":                  func() error { return g.Remove(ctx, skill, nil) },
		"remove a link inside":    func() error { return g.Remove(ctx, away, nil) },
		"remove the folder above": func() error { return g.Remove(ctx, filepath.Join(l.home, ".pi"), &env.RemoveOptions{Recursive: true}) },
		"rename the folder above": func() error {
			return g.RenameFile(ctx, filepath.Join(l.home, ".pi", "agent"), filepath.Join(l.home, "moved"))
		},
		"rename the uploads folder": func() error { return g.RenameFile(ctx, up, filepath.Join(l.home, "moved")) },
	} {
		if err := change(); !errors.Is(err, errReadOnly) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if b, err := os.ReadFile(upload); err != nil || string(b) != "FAIL: TestX" {
		t.Errorf("the upload changed: %q, %v", b, err)
	}
	if b, err := os.ReadFile(skill); err != nil || string(b) != "Triage the issues." {
		t.Errorf("the skill changed: %q, %v", b, err)
	}
	// Beside them, the file tools write as before.
	if err := g.WriteFile(ctx, filepath.Join(l.home, ".local", "share", "notes.txt"), "kept"); err != nil {
		t.Errorf("a write beside the uploads folder: %v", err)
	}
}

// While a symbolic link the owner put in a shown folder's place leads its
// path elsewhere, the file tools change nothing at all, as no command
// runs, until a restart: the folder it leads to may show under other
// paths a mount gives it, which only the start's mount table named, as
// /home and /var/home do on the VM (Codex review of #63, rounds 1 and 2).
// Reads go on.
func TestFileToolsKeepAShownFolderALinkNowLeadsTo(t *testing.T) {
	l, skills, _ := newShownLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap")) // never run: file tools do not start commands
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(l.home, "moved-skills")
	if err := os.Rename(skills, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, skills); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work := t.TempDir()
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), protected: s.Guarded(), readOnly: s.Shown}
	for name, change := range map[string]func() error{
		"a write through the path":   func() error { return g.WriteFile(ctx, filepath.Join(skills, "evil", "SKILL.md"), "evil") },
		"a write where it went":      func() error { return g.WriteFile(ctx, filepath.Join(moved, "evil", "SKILL.md"), "evil") },
		"a new folder where it went": func() error { return g.CreateDir(ctx, filepath.Join(moved, "evil"), nil) },
		"a removal of where it went": func() error { return g.Remove(ctx, moved, &env.RemoveOptions{Recursive: true}) },
		"a write anywhere else":      func() error { return g.WriteFile(ctx, filepath.Join(work, "notes.txt"), "x") },
	} {
		if err := change(); !errors.Is(err, errReadOnly) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(moved, "evil")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a skill was written where the folder went: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "read.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := g.ReadTextFile(ctx, filepath.Join(work, "read.txt")); err != nil || got != "kept" {
		t.Fatalf("a read while the folder is led away: %q, %v", got, err)
	}
}

// pocketd's start check proves every read-only path and every shown folder
// on a read-only mount in the sandbox: a bwrap that binds one writable, as
// one that missed a bind would leave it, fails the check, though it runs
// as process 1 and hides what it must (ADR-0035), and so does one whose
// probe does not run.
func TestCheckProvesTheReadOnlyPaths(t *testing.T) {
	l, skills, up := newShownLayout(t)
	s := usableSandbox(t, l)
	ctx := context.Background()
	// wrapped is the sandbox with a bwrap that runs script.
	wrapped := func(script string) *Sandbox {
		wrapper := filepath.Join(t.TempDir(), "bwrap")
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		broken := *s
		broken.Bwrap = wrapper
		return &broken
	}
	envFile := filepath.Join(l.home, ".config", "pocketd.env")
	for _, path := range []string{up, envFile, skills} {
		if path == skills {
			// A folder nobody may write, on a writable mount: its files
			// may still be written, so a write refused proves nothing
			// (Codex review of #63, round 1).
			if err := os.Chmod(skills, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(skills, 0o700) })
		}
		// The wrapper turns --ro-bind PATH into --bind PATH, and runs the
		// real bwrap with every other argument as it was.
		script := "#!/bin/sh\nheld=\nfirst=1\nfor a do\n" +
			"  if [ \"$first\" = 1 ]; then set --; first=0; fi\n" +
			"  if [ -n \"$held\" ]; then held=\n" +
			"    if [ \"$a\" = '" + path + "' ]; then set -- \"$@\" --bind \"$a\"; else set -- \"$@\" --ro-bind \"$a\"; fi\n" +
			"    continue\n  fi\n" +
			"  if [ \"$a\" = --ro-bind ]; then held=1; continue; fi\n" +
			"  set -- \"$@\" \"$a\"\ndone\nexec '" + s.Bwrap + "' \"$@\"\n"
		if err := wrapped(script).Check(ctx); err == nil || !strings.Contains(err.Error(), path+" is on a mount that is not read-only") {
			t.Errorf("a sandbox that binds %s writable passed its check: %v", path, err)
		}
	}
	// A probe that did not run proves nothing: a bwrap that runs the first
	// probes, and fails every other one as bwrap fails to start, fails the
	// check (Codex review of #63, round 1).
	script := "#!/bin/sh\nfor a do\n  case \"$a\" in /proc/1/comm|-A) exec '" + s.Bwrap + "' \"$@\";; esac\ndone\nexit 1\n"
	if err := wrapped(script).Check(ctx); err == nil {
		t.Error("a sandbox whose later probes did not run passed its check")
	}
}

// mountAt walks the mount table as the kernel walks a path: a mount
// stacked on another at one point is the one a path finds there, and one
// covered by a mount at a folder above it is not on the way (Codex review
// of #63, round 1).
func TestMountAtFindsTheMountAPathIsOn(t *testing.T) {
	mounts, err := parseMountinfo(strings.NewReader(`1 0 0:1 / / rw - tmpfs tmpfs rw
2 1 0:2 / / rw,relatime - btrfs /dev/sda2 rw
3 2 0:2 /home/u/.local /home/u/.local rw,relatime - btrfs /dev/sda2 rw
4 3 0:2 /home/u/.local/share/up /home/u/.local/share/up ro,relatime - btrfs /dev/sda2 rw
5 2 0:2 /home/u/skills /home/u/skills rw,relatime - btrfs /dev/sda2 rw
6 5 0:2 /home/u/skills /home/u/skills ro,relatime - btrfs /dev/sda2 rw
7 2 0:2 /home/u/a/b /home/u/a/b ro,relatime - btrfs /dev/sda2 rw
8 2 0:2 /home/u/a /home/u/a rw,relatime - btrfs /dev/sda2 rw
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, id string
		ro       bool
	}{
		{"/home/u/.local/share/up/7/shot.png", "4", true},
		{"/home/u/.local/share/other", "3", false},
		{"/home/u/skills/grooming/SKILL.md", "6", true}, // the top of the stack at /home/u/skills
		{"/home/u/a/b/c", "8", false},                   // 7 is covered by 8, mounted at a folder above it
		{"/etc/passwd", "2", false},
	} {
		m, ok := mountAt(mounts, c.path)
		if !ok || m.ID != c.id || m.readOnly() != c.ro {
			t.Errorf("mountAt(%s) = %+v, %v; want mount %s, read-only %v", c.path, m, ok, c.id, c.ro)
		}
	}
	if _, ok := mountAt(nil, "/x"); ok {
		t.Error("a table with no root gave a mount")
	}
}

// A shown folder a mask hides is an empty tmpfs in the sandbox, which
// writes nothing to the host: the start check passes it, as a data folder
// named as the skills folder, which hides the skills from the gathers,
// does not stop the start (ADR-0035).
func TestCheckPassesAShownFolderAMaskHides(t *testing.T) {
	l := newLayout(t)
	l.shown = []string{l.data}
	usableSandbox(t, l)
}
