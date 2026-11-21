//go:build linux

package pig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// pidfdOpen opens a pidfd of a process (pidfd_open(2)); tests make it
// fail as a kernel before 5.3 does.
var pidfdOpen = unix.PidfdOpen

// readProc reads a file of /proc; tests make it fail.
var readProc = os.ReadFile

// stopSandboxes kills every command running in a sandbox this process
// started: each child of pocketd that runs bwrap, by the name it was
// started as. bwrap's own child dies with it (--die-with-parent), and with
// that child, the first process of its process namespace, every process
// the command started (ADR-0035). A child is signalled only through a
// pidfd opened before it is checked again, so a process number used
// again meanwhile is never signalled: where the kernel opens no pidfd,
// nothing is signalled, and the stop fails (Codex review of #63, round 2).
// A child it cannot look at again is a failure too, never one stopped
// (round 3).
func stopSandboxes(bwrap string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	self := os.Getpid()
	var errs []error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// pocketd may read its own children's files always: one it may not
		// read is another user's process (a /proc mounted with hidepid),
		// never a sandbox it started.
		if runs, err := runsSandbox(pid, self, bwrap); errors.Is(err, fs.ErrPermission) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		} else if !runs {
			continue
		}
		fd, err := pidfdOpen(pid, 0)
		switch {
		case errors.Is(err, unix.ESRCH):
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if err := signalSandbox(fd, pid, self, bwrap); err != nil {
			errs = append(errs, err)
		}
		_ = unix.Close(fd)
	}
	if len(errs) > 0 {
		return fmt.Errorf("stop the commands running in the sandbox: %w", errors.Join(errs...))
	}
	return nil
}

// signalSandbox kills the process pidfd fd opened, pid when it was
// opened, once it is checked again as a child of parent run as bwrap: one
// that ended meanwhile, or whose number another process took, is left as
// it is.
func signalSandbox(fd, pid, parent int, bwrap string) error {
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); errors.Is(err, unix.ESRCH) {
		return nil // it ended
	} else if err != nil {
		return err
	}
	runs, err := runsSandbox(pid, parent, bwrap)
	if err != nil || !runs {
		return err
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// runsSandbox says whether process pid is a child of parent started as
// bwrap: its parent is parent, and its first argument is bwrap's path. A
// process that is gone runs none; one whose files cannot be read is an
// error.
func runsSandbox(pid, parent int, bwrap string) (bool, error) {
	read := func(name string) ([]byte, bool, error) {
		data, err := readProc("/proc/" + strconv.Itoa(pid) + "/" + name)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, unix.ESRCH):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("look at process %d: %w", pid, err)
		}
		return data, true, nil
	}
	stat, ok, err := read("stat")
	if err != nil || !ok {
		return false, err
	}
	// The command's name is in parentheses and may hold any byte: the
	// fields after its last ")" are the state, then the parent.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return false, fmt.Errorf("look at process %d: no command name in its stat", pid)
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 2 {
		return false, fmt.Errorf("look at process %d: no parent in its stat", pid)
	}
	if fields[1] != strconv.Itoa(parent) {
		return false, nil
	}
	cmdline, ok, err := read("cmdline")
	if err != nil || !ok {
		return false, err
	}
	argv0, _, _ := bytes.Cut(cmdline, []byte{0})
	return string(argv0) == bwrap, nil
}
