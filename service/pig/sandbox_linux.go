//go:build linux

package pig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// pidfdOpen opens a pidfd of a process (pidfd_open(2)); tests make it
// fail as a kernel before 5.3 does.
var pidfdOpen = unix.PidfdOpen

// stopSandboxes kills every command running in a sandbox this process
// started: each child of pocketd that runs bwrap, by the name it was
// started as. bwrap's own child dies with it (--die-with-parent), and with
// that child, the first process of its process namespace, every process
// the command started (ADR-0035). A child is signalled only through a
// pidfd opened before it is checked again, so a process number used
// again meanwhile is never signalled: where the kernel opens no pidfd,
// nothing is signalled, and the stop fails (Codex review of #63, round 2).
func stopSandboxes(bwrap string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	self := os.Getpid()
	var errs []error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !runsSandbox(pid, self, bwrap) {
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
		if runsSandbox(pid, self, bwrap) {
			if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
				errs = append(errs, err)
			}
		}
		_ = unix.Close(fd)
	}
	if len(errs) > 0 {
		return fmt.Errorf("stop the commands running in the sandbox: %w", errors.Join(errs...))
	}
	return nil
}

// runsSandbox says whether process pid is a child of parent started as
// bwrap: its parent is parent, and its first argument is bwrap's path.
func runsSandbox(pid, parent int, bwrap string) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The command's name is in parentheses and may hold any byte: the
	// fields after its last ")" are the state, then the parent.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 2 || fields[1] != strconv.Itoa(parent) {
		return false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	argv0, _, _ := bytes.Cut(cmdline, []byte{0})
	return string(argv0) == bwrap
}
