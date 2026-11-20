//go:build linux

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// The signals an ended shell's process group gets.
const (
	hangUp = unix.SIGHUP
	kill   = unix.SIGKILL
)

// shell is a login shell on a pseudo-terminal. Its master stays on Go's
// poller, so closing it ends a read or write that waits: no ioctl here
// takes the descriptor out of it (os.File.Fd would).
type shell struct {
	cmd    *exec.Cmd
	master *os.File
	once   sync.Once
}

// startShell starts path as a login shell ("-bash") in its own session,
// with a new pseudo-terminal of rows by cols as its controlling terminal,
// in folder dir, else home, else /, as pi-pocket-team's helper does, and
// returns the folder it started in. A folder the shell cannot enter, or
// one removed since it was checked, gives the next: the child's chdir is
// what decides (Codex review of #41, round 4).
func startShell(path, dir, home string, env []string, rows, cols int) (*shell, string, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = slave.Close() }()
	sh := &shell{master: master}
	if err := sh.resize(rows, cols); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	var last error
	for _, d := range []string{dir, home, "/"} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			continue
		}
		sh.cmd = &exec.Cmd{
			Path: path, Args: []string{"-" + filepath.Base(path)}, Dir: d, Env: env,
			Stdin: slave, Stdout: slave, Stderr: slave,
			// Its own session, with the pseudo-terminal (its fd 0) as its
			// controlling terminal: the shell leads its process group.
			SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0},
		}
		err := sh.cmd.Start()
		if err == nil {
			return sh, d, nil
		}
		last = err
		// The child's chdir failed (no such folder, no right to enter
		// it): the next folder. Any other failure is the shell's own.
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.ENOTDIR) {
			break
		}
	}
	_ = master.Close()
	if last == nil {
		last = errors.New("no folder to start in")
	}
	return nil, "", fmt.Errorf("start %s: %w", path, last)
}

// openPTY opens a pseudo-terminal: its master, pollable, and its slave,
// blocking, as the shell reads it.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n uint32
	if err := control(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return err
		}
		n, err = unix.IoctlGetUint32(fd, unix.TIOCGPTN)
		return err
	}); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("pseudo-terminal: %w", err)
	}
	name := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("pseudo-terminal %s: %w", name, err)
	}
	return master, os.NewFile(uintptr(fd), name), nil
}

// control runs f on f's descriptor without taking it off the poller.
func control(f *os.File, fn func(fd int) error) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := raw.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return err
	}
	return inner
}

func (s *shell) read(p []byte) (int, error)  { return s.master.Read(p) }
func (s *shell) write(p []byte) (int, error) { return s.master.Write(p) }

// resize sets the pseudo-terminal's size: the shell gets SIGWINCH.
func (s *shell) resize(rows, cols int) error {
	return control(s.master, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	})
}

// signal sends sig to the shell's process group; call it only while the
// shell is not reaped, so its id names no other group.
func (s *shell) signal(sig syscall.Signal) {
	_ = unix.Kill(-s.cmd.Process.Pid, sig)
}

func (s *shell) pid() int { return s.cmd.Process.Pid }

// awaitExit waits until the shell exited, and leaves it unreaped: its id
// stays its own until reap.
func (s *shell) awaitExit() {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, s.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return
		}
	}
}

// reap reaps the shell and returns its exit code: 128 and the signal for
// one a signal ended, nil when there is none.
func (s *shell) reap() *int {
	_ = s.cmd.Wait()
	state := s.cmd.ProcessState
	if state == nil {
		return nil
	}
	code := state.ExitCode()
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = 128 + int(status.Signal())
	}
	if code < 0 {
		return nil
	}
	return &code
}

// close closes the master: the slave hangs up, and a read or write that
// waits on it ends.
func (s *shell) close() { s.once.Do(func() { _ = s.master.Close() }) }
