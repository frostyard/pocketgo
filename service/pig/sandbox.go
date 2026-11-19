package pig

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sandbox runs each command a tool starts under bubblewrap (ADR-0012). The
// Hide folders are empty, the ReadOnly paths cannot change, the Pin folders
// cannot be renamed or removed, and the command sees only its own processes
// and dies with pocketd. Everything else of the host stays as it is: agents
// keep their tools, repositories, caches and network.
type Sandbox struct {
	// Bwrap is a root-owned bwrap program; Shell runs a command given as a
	// string, as PiG's local environment runs it.
	Bwrap, Shell string
	// Pin are the folders above a protected path that the service user
	// could rename, shallowest first. Each is bound onto itself: a mount
	// point cannot be renamed or removed, through any mount of it, so the
	// paths below stay where Hide and ReadOnly cover them, from one
	// command to the next.
	Pin []string
	// ReadOnly are what decides how pocketd starts, under every path a
	// mount exposes them at: its program, its environment file and the
	// user's systemd configuration.
	ReadOnly []string
	// Hide are folders replaced by an empty tmpfs, under every path a mount
	// exposes them at: the data folder, which holds the sign-ins and
	// transcripts, and /run/user, which holds every runtime folder with its
	// D-Bus and systemd sockets, so one a login creates while a command
	// runs stays hidden from it too.
	Hide []string
}

// Command is the bwrap command line that runs command in the sandbox: a
// string through the shell, an argv as it is. It never returns a command
// that runs outside the sandbox.
func (s *Sandbox) Command(command any) ([]string, error) {
	args := []string{s.Bwrap, "--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc", "--die-with-parent"}
	// Pins first: a bind of a folder shows the host's view of everything
	// below it, so the mounts below must come after.
	for _, dir := range s.Pin {
		args = append(args, "--bind", dir, dir)
	}
	for _, path := range s.ReadOnly {
		args = append(args, "--ro-bind", path, path)
	}
	for _, dir := range s.Hide {
		args = append(args, "--tmpfs", dir)
	}
	args = append(args, "--")
	switch c := command.(type) {
	case string:
		return append(args, s.Shell, "-c", c), nil
	case []string:
		if len(c) == 0 {
			return nil, errors.New("empty argv: no program to run")
		}
		return append(args, c...), nil
	}
	return nil, fmt.Errorf("a command is a string or a []string, not %T", command)
}

// Guarded are the paths the file tools refuse as well, so a write or an
// edit cannot do what the sandbox stops a command from doing.
func (s *Sandbox) Guarded() []string {
	return slices.Concat(s.Hide, s.ReadOnly)
}

// sandboxPaths are the host paths a sandbox protects.
type sandboxPaths struct {
	Bwrap, Shell string
	// Data is pocketd's data folder; Home the service user's home; Program
	// pocketd's running executable, and Launch the name its unit starts it
	// by (deploy/systemd/pocketd.service: ~/.local/bin/pocketd).
	Data, Home, Program, Launch string
	// Runtime are the folders that hold runtime folders: /run/user, and an
	// XDG_RUNTIME_DIR outside it.
	Runtime []string
	// ConfigHomes and DataHomes are the XDG configuration and data folders
	// the user's systemd manager reads: the defaults, and the ones the
	// environment names.
	ConfigHomes, DataHomes []string
	// Mounts are the host's mounts (/proc/self/mountinfo).
	Mounts []mount
	// Writable reports whether the service user may change a folder's
	// entries.
	Writable func(dir string) bool
}

// NewSandbox finds a root-owned bwrap and fills the paths ADR-0012 names for
// the data folder data.
func NewSandbox(data string) (*Sandbox, error) {
	bwrap, err := trustedBwrap()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	program, err := os.Executable()
	if err != nil {
		return nil, err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("commands run under bubblewrap (ADR-0012), which needs the host's mounts: %w", err)
	}
	mounts, err := parseMountinfo(f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	p := sandboxPaths{Bwrap: bwrap, Shell: shell(), Data: data, Home: home, Program: program,
		Launch: filepath.Join(home, ".local", "bin", "pocketd"), Runtime: []string{"/run/user"},
		ConfigHomes: []string{filepath.Join(home, ".config")}, DataHomes: []string{filepath.Join(home, ".local", "share")},
		Mounts: mounts, Writable: writable}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) && !within(dir, "/run/user") {
		p.Runtime = append(p.Runtime, dir)
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		p.ConfigHomes = append(p.ConfigHomes, dir)
	}
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		p.DataHomes = append(p.DataHomes, dir)
	}
	return newSandbox(p)
}

// newSandbox is the sandbox of p. It creates the configuration paths that
// do not exist yet, empty, so each can be bound read-only: a command could
// otherwise create one that pocketd's next start reads.
func newSandbox(p sandboxPaths) (*Sandbox, error) {
	s := &Sandbox{Bwrap: p.Bwrap, Shell: p.Shell}
	data, err := filepath.Abs(p.Data)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, home := range p.ConfigHomes {
		dirs = append(dirs, filepath.Join(home, "systemd"), filepath.Join(home, "environment.d"))
	}
	for _, home := range p.DataHomes {
		dirs = append(dirs, filepath.Join(home, "systemd"))
	}
	envFile := filepath.Join(p.Home, ".config", "pocketd.env")
	var launch []string
	if p.Launch != "" {
		if _, err := os.Lstat(p.Launch); err == nil {
			launch = []string{p.Launch}
		}
	}
	names := slices.Concat([]string{data, envFile}, dirs, launch)
	// systemd and pocketd open these by name: a link on the way that the
	// user could change would lead them past what the sandbox protects.
	for _, path := range names {
		if err := noUserLinks(path, p.Writable); err != nil {
			return nil, err
		}
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(envFile, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	resolved, err := filepath.EvalSymlinks(data)
	if err != nil {
		return nil, err
	}
	hide := []string{resolved}
	for _, dir := range p.Runtime {
		// A runtime folder that is missing now could appear under a
		// parent the mask does not cover: refuse rather than guess.
		if err := noUserLinks(dir, p.Writable); err != nil {
			return nil, err
		}
		dir, err := absolute(dir)
		if err != nil {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012), and the runtime folder cannot be hidden: %w", err)
		}
		if !slices.Contains(hide, dir) {
			hide = append(hide, dir)
		}
	}
	s.Hide = withAliases(hide, p.Mounts)
	// A tmpfs covers a folder only: a hidden path that is a file, as a
	// bind mount of one file can be, cannot be masked.
	for _, path := range s.Hide {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012), and %s, which shows a protected path, is not a folder pocketd can hide", path)
		}
	}
	var readOnly []string
	for _, path := range slices.Concat([]string{p.Program, envFile}, dirs, launch) {
		resolved, err := absolute(path)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(readOnly, resolved) {
			readOnly = append(readOnly, resolved)
		}
	}
	// A link in the systemd folders is read where it points: it must stay
	// inside them, or point to a file only root can change.
	for _, dir := range dirs {
		if err := linksStayProtected(dir, readOnly); err != nil {
			return nil, err
		}
	}
	s.ReadOnly = withAliases(readOnly, p.Mounts)

	// Every folder above a protected path whose parent the user may
	// change could be renamed, taking the protected path with it.
	for _, path := range slices.Concat(hide, readOnly) {
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if p.Writable(filepath.Dir(dir)) && !slices.Contains(s.Pin, dir) {
				s.Pin = append(s.Pin, dir)
			}
		}
	}
	// Shallowest first, so a pin never covers a mount below it; by name
	// within a depth, so the command line is the same on every start.
	slices.SortFunc(s.Pin, func(a, b string) int {
		if d := strings.Count(a, string(filepath.Separator)) - strings.Count(b, string(filepath.Separator)); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return s, nil
}

// noUserLinks refuses path when a symbolic link on its way, the links it
// leads through included, lives in a folder the user may change: the user
// could point it elsewhere.
func noUserLinks(path string, writable func(string) bool) error {
	_, err := walkPath(path, func(dir string, entry fs.FileInfo) error {
		if entry.Mode()&os.ModeSymlink != 0 && writable(dir) {
			return fmt.Errorf("commands run in a sandbox (ADR-0012), and %s is a symbolic link the service user can change: pocketd cannot protect %s",
				filepath.Join(dir, entry.Name()), path)
		}
		return nil
	})
	return err
}

// linksStayProtected checks every link under dir, since systemd follows
// it: each entry on the way, the links it leads through included, must be
// read-only or pinned in the sandbox, or in a folder only root can change,
// and so must the file it ends at. /dev/null, where systemctl mask points a
// unit, is fine.
func linksStayProtected(dir string, protected []string) error {
	inProtected := func(path string) bool {
		for _, p := range protected {
			if within(path, p) {
				return true
			}
		}
		return false
	}
	holdsProtected := func(path string) bool {
		for _, p := range protected {
			if within(p, path) {
				return true
			}
		}
		return false
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		refuse := func(err error) error {
			return fmt.Errorf("commands run in a sandbox (ADR-0012), and %s leads through something the service user can change: %w", path, err)
		}
		target, err := walkPath(path, func(dir string, entry fs.FileInfo) error {
			// An entry inside a protected path is read-only, and one that
			// holds a protected path is pinned: neither can change.
			next := filepath.Join(dir, entry.Name())
			if inProtected(next) || holdsProtected(next) {
				return nil
			}
			uid, perm, err := statOwner(dir)
			if err != nil {
				return err
			}
			if uid != 0 || perm&0o022 != 0 {
				return fmt.Errorf("%s can be changed by a user other than root", dir)
			}
			return nil
		})
		if err != nil {
			return refuse(err)
		}
		if inProtected(target) || target == os.DevNull {
			return nil
		}
		info, err := os.Lstat(target)
		if err != nil {
			return refuse(err)
		}
		if err := rootOwnedEntry(target, info); err != nil {
			return refuse(err)
		}
		return nil
	})
}

// absolute is path made absolute, with its symbolic links resolved.
func absolute(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// mount is a line of /proc/self/mountinfo: the filesystem's device, the
// folder of that filesystem the mount shows, and where.
type mount struct {
	Device, Root, Point string
}

// parseMountinfo reads /proc/self/mountinfo.
func parseMountinfo(r io.Reader) ([]mount, error) {
	var out []mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		out = append(out, mount{Device: fields[2], Root: unescapeMount(fields[3]), Point: unescapeMount(fields[4])})
	}
	return out, sc.Err()
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// withAliases is paths, each followed by every other path a mount exposes
// it at: a bind mount of /var/home at /home shows the same data folder
// twice, and a mask on one path leaves the other.
func withAliases(paths []string, mounts []mount) []string {
	var out []string
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	// A filesystem mounted inside a protected tree, such as a runtime
	// folder's own tmpfs under /run/user, is protected as well, and may be
	// mounted elsewhere too.
	roots := slices.Clone(paths)
	for _, path := range paths {
		for _, m := range mounts {
			if m.Point != path && within(m.Point, path) && !slices.Contains(roots, m.Point) {
				roots = append(roots, m.Point)
			}
		}
	}
	// An alias inside a path already masked is covered by that mask.
	addAlias := func(p string) {
		for _, path := range paths {
			if p != path && within(p, path) {
				return
			}
		}
		add(p)
	}
	for _, path := range roots {
		if slices.Contains(paths, path) {
			add(path)
		}
		// The mount the path is under: the one with the longest point
		// that holds it; the last such line wins, as it is on top.
		var under *mount
		for i := range mounts {
			m := &mounts[i]
			if within(path, m.Point) && (under == nil || len(m.Point) >= len(under.Point)) {
				under = m
			}
		}
		if under == nil {
			continue
		}
		rel, err := filepath.Rel(under.Point, path)
		if err != nil {
			continue
		}
		inFS := filepath.Join(under.Root, rel)
		for _, m := range mounts {
			if m.Device != under.Device {
				continue
			}
			switch {
			case within(inFS, m.Root):
				// The mount shows a folder that holds the path.
				if rest, err := filepath.Rel(m.Root, inFS); err == nil {
					addAlias(filepath.Join(m.Point, rest))
				}
			case within(m.Root, inFS):
				// The mount shows part of the path's tree on its own.
				addAlias(m.Point)
			}
		}
	}
	return out
}

// shell is the shell PiG's local environment runs a string with:
// /bin/bash, else bash on the PATH, else sh.
func shell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		return bash
	}
	return "sh"
}

// systemProgram is name in /usr/bin or /bin, if only root can change it.
func systemProgram(name string) (string, error) {
	var last error
	for _, dir := range []string{"/usr/bin", "/bin"} {
		path, err := trustedProgram(filepath.Join(dir, name))
		if err == nil {
			return path, nil
		}
		last = err
	}
	return "", fmt.Errorf("no %s only root can change: %w", name, last)
}

// bwrapPaths are where a system installs bwrap. The PATH is never searched:
// a folder on it that the service user owns could hold a fake bwrap.
var bwrapPaths = []string{"/usr/bin/bwrap", "/bin/bwrap", "/usr/local/bin/bwrap"}

// trustedBwrap is the first installed bwrap that only root can change,
// along every name that leads to it.
func trustedBwrap() (string, error) {
	var problems []string
	for _, candidate := range bwrapPaths {
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		program, err := trustedProgram(candidate)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		return program, nil
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("commands run under bubblewrap (ADR-0012), and no bwrap only root can change is installed: %s", strings.Join(problems, "; "))
	}
	return "", fmt.Errorf("commands run under bubblewrap (ADR-0012), and bwrap is not installed in %s", strings.Join(bwrapPaths, ", "))
}

// trustedProgram follows path to the program it names, the way the kernel
// does, and checks that only root can change each step: every folder it
// passes through, links included, and the program itself.
func trustedProgram(path string) (string, error) {
	resolved, err := walkPath(path, func(dir string, _ fs.FileInfo) error {
		uid, perm, err := statOwner(dir)
		if err != nil {
			return err
		}
		if uid != 0 || perm&0o022 != 0 {
			return fmt.Errorf("%s can be changed by a user other than root", dir)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a program", resolved)
	}
	if err := rootOwnedEntry(resolved, info); err != nil {
		return "", err
	}
	return resolved, nil
}

// Check proves the sandbox before pocketd starts (ADR-0012): it runs bwrap
// with no shell and the commands' own environment, and refuses unless the
// command runs as process 1's child of bwrap and finds every hidden folder
// empty. A fake bwrap, or a mount the masks missed, fails it.
func (s *Sandbox) Check(ctx context.Context) error {
	run := func(argv ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		args, err := s.Command(argv)
		if err != nil {
			return "", err
		}
		//nolint:gosec // G204: the program is a root-owned bwrap, and the arguments are the sandbox's own.
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Env = []string{}
		for name, value := range allowedEnv() {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		out, err := cmd.Output()
		return string(out), err
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("commands run under bubblewrap (ADR-0012), and the sandbox check failed: "+format, args...)
	}
	// The probes are root's own programs, never ones found on the PATH.
	cat, err := systemProgram("cat")
	if err != nil {
		return fail("%v", err)
	}
	ls, err := systemProgram("ls")
	if err != nil {
		return fail("%v", err)
	}
	if out, err := run(cat, "/proc/1/comm"); err != nil || out != "bwrap\n" {
		return fail("process 1 is %q, not bwrap (%v)", strings.TrimSpace(out), err)
	}
	for _, dir := range s.Hide {
		if out, err := run(ls, "-A", dir); err != nil || out != "" {
			return fail("%s is not empty in the sandbox (%v)", dir, err)
		}
	}
	return nil
}
