package pig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Sandbox runs each command a tool starts under bubblewrap (ADR-0012). The
// Hide folders are empty, the ReadOnly paths cannot change, and the command
// sees only its own processes and dies with pocketd. The rest of the host
// stays as it is: agents keep their tools, repositories, caches and network.
type Sandbox struct {
	// Bwrap is the bwrap program, Shell the shell that runs a command
	// given as a string (PiG's local environment uses bash the same way).
	Bwrap, Shell string
	// Hide are folders replaced by an empty tmpfs: pocketd's data folder,
	// which holds the sign-ins and transcripts, and the user's runtime
	// folder, which holds the D-Bus and systemd sockets.
	Hide []string
	// ReadOnly are what decides how pocketd starts: its program, the user's
	// systemd configuration, and its environment file.
	ReadOnly []string
}

// Command is the bwrap command line that runs command in the sandbox: a
// string through the shell, an argv as it is. It never returns a command
// that runs outside the sandbox.
func (s *Sandbox) Command(command any) ([]string, error) {
	args := []string{s.Bwrap, "--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc", "--die-with-parent"}
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

// NewSandbox finds bwrap and the shell on this host and fills the paths
// ADR-0012 names for the data folder data. It creates the configuration
// paths that do not exist yet, empty, so each can be bound read-only: a
// command could otherwise create one that pocketd's next start reads.
func NewSandbox(data string) (*Sandbox, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("commands run under bubblewrap (ADR-0012), and bwrap is not installed: %w", err)
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
	return newSandbox(bwrap, shell(), data, home, runtime, program)
}

// newSandbox is NewSandbox for the given host paths.
func newSandbox(bwrap, shell, data, home, runtime, program string) (*Sandbox, error) {
	s := &Sandbox{Bwrap: bwrap, Shell: shell}
	for _, dir := range []string{data, runtime} {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue // nothing there to hide
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, err
		}
		s.Hide = append(s.Hide, resolved)
	}
	dirs := []string{
		filepath.Join(home, ".config", "systemd"),
		filepath.Join(home, ".local", "share", "systemd"),
		filepath.Join(home, ".config", "environment.d"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	envFile := filepath.Join(home, ".config", "pocketd.env")
	f, err := os.OpenFile(envFile, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	for _, path := range append([]string{program, envFile}, dirs...) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		s.ReadOnly = append(s.ReadOnly, resolved)
	}
	return s, nil
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
	//nolint:gosec // G204: the program is the bwrap NewSandbox found, and the arguments are fixed.
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("commands run under bubblewrap (ADR-0012), and a trial command failed: %w: %s", err, out)
	}
	return nil
}
