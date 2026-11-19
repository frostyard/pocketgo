package pig

import (
	"context"
	"errors"
	"fmt"
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
	// point cannot be renamed or removed, so the paths below stay where
	// Hide and ReadOnly cover them, from one command to the next.
	Pin []string
	// ReadOnly are what decides how pocketd starts: its program, its
	// environment file and the user's systemd configuration.
	ReadOnly []string
	// Hide are folders replaced by an empty tmpfs: the data folder, which
	// holds the sign-ins and transcripts.
	Hide []string
	// HideIfPresent are folders hidden whenever they exist as a command
	// starts: the user's runtime folder, with its D-Bus and systemd sockets,
	// which a login can create after pocketd started.
	HideIfPresent []string
	// exists reports whether a path exists; nil is os.Stat.
	exists func(string) bool
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
	exists := s.exists
	if exists == nil {
		exists = func(path string) bool { _, err := os.Stat(path); return err == nil }
	}
	for _, dir := range s.HideIfPresent {
		if exists(dir) {
			args = append(args, "--tmpfs", dir)
		}
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
	return slices.Concat(s.Hide, s.HideIfPresent, s.ReadOnly)
}

// sandboxPaths are the host paths a sandbox protects.
type sandboxPaths struct {
	Bwrap, Shell string
	// Data is pocketd's data folder; Home the service user's home; Runtime
	// the user's runtime folder; Program pocketd's executable.
	Data, Home, Runtime, Program string
	// ConfigHomes and DataHomes are the XDG configuration and data folders
	// the user's systemd manager reads: the defaults, and the ones the
	// environment names.
	ConfigHomes, DataHomes []string
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
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	program, err := os.Executable()
	if err != nil {
		return nil, err
	}
	p := sandboxPaths{Bwrap: bwrap, Shell: shell(), Data: data, Home: home, Runtime: runtime, Program: program,
		ConfigHomes: []string{filepath.Join(home, ".config")}, DataHomes: []string{filepath.Join(home, ".local", "share")},
		Writable: writable}
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
	data, err := absolute(p.Data)
	if err != nil {
		return nil, err
	}
	s.Hide = []string{data}
	runtime, err := filepath.Abs(p.Runtime)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(runtime); err == nil {
		runtime = resolved
	}
	s.HideIfPresent = []string{runtime}

	var dirs []string
	for _, home := range p.ConfigHomes {
		dirs = append(dirs, filepath.Join(home, "systemd"), filepath.Join(home, "environment.d"))
	}
	for _, home := range p.DataHomes {
		dirs = append(dirs, filepath.Join(home, "systemd"))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	envFile := filepath.Join(p.Home, ".config", "pocketd.env")
	f, err := os.OpenFile(envFile, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	for _, path := range append([]string{p.Program, envFile}, dirs...) {
		resolved, err := absolute(path)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(s.ReadOnly, resolved) {
			s.ReadOnly = append(s.ReadOnly, resolved)
		}
	}

	// Every folder above a protected path whose parent the user may
	// change could be renamed, taking the protected path with it.
	for _, path := range append(slices.Clone(s.Hide), s.ReadOnly...) {
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

// absolute is path made absolute, with its symbolic links resolved.
func absolute(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
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

// bwrapPaths are where a system installs bwrap. The PATH is never searched:
// a folder on it that the service user owns could hold a fake bwrap.
var bwrapPaths = []string{"/usr/bin/bwrap", "/bin/bwrap", "/usr/local/bin/bwrap"}

// trustedBwrap is the first installed bwrap that only root can change.
func trustedBwrap() (string, error) {
	var problems []string
	for _, candidate := range bwrapPaths {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if err := rootOwned(resolved); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		return resolved, nil
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("commands run under bubblewrap (ADR-0012), and no bwrap only root can change is installed: %s", strings.Join(problems, "; "))
	}
	return "", fmt.Errorf("commands run under bubblewrap (ADR-0012), and bwrap is not installed in %s", strings.Join(bwrapPaths, ", "))
}

// Check runs one command in the sandbox and reports whether it worked.
// pocketd serve calls it before anything can start a command, and refuses
// to start when it fails (ADR-0012).
func (s *Sandbox) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args, err := s.Command("true")
	if err != nil {
		return err
	}
	//nolint:gosec // G204: the program is a root-owned bwrap, and the arguments are the sandbox's own.
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("commands run under bubblewrap (ADR-0012), and a trial command failed: %w: %s", err, out)
	}
	return nil
}
