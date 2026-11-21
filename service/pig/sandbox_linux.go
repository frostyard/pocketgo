//go:build linux

package pig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// pidfdOpen opens a pidfd of a process (pidfd_open(2)); tests make it
// fail as a kernel before 5.3 does.
var pidfdOpen = unix.PidfdOpen

// readProc reads a file of /proc; tests make it fail.
var readProc = os.ReadFile

// pidfdWait waits until every process of fds has ended, at most d; tests
// make it time out.
var pidfdWait = waitEnded

// stopWait bounds how long a stop waits for the sandboxes it killed to
// end.
const stopWait = 10 * time.Second

// stopSandboxes kills every command running in a sandbox this process
// started, and waits until each has ended (ADR-0035). A sandbox is a child
// of pocketd that runs bwrap, by the name it was started as, and that
// child's own children: the first process of its process namespace, whose
// end ends every process of that namespace before its own end shows. Both
// are signalled, so none outlives the stop, and only through a pidfd
// opened before the process was checked again, so a process number used
// again meanwhile is never signalled: where the kernel opens no pidfd,
// nothing is signalled, and the stop fails (Codex review of #63, round 2).
// A process it cannot look at again is a failure too, never one stopped
// (round 3), and so is one that has not ended within stopWait: a signal
// sent is no process ended (round 4).
func stopSandboxes(bwrap string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	self := os.Getpid()
	var errs []error
	type proc struct{ pid, parent int }
	var procs []proc
	outers := map[int]bool{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		parent, err := parentOf(pid)
		// pocketd may read its own processes' files always: one it may
		// not read is another user's (a /proc mounted with hidepid).
		switch {
		case errors.Is(err, fs.ErrPermission), err == nil && parent < 0:
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		procs = append(procs, proc{pid, parent})
		if parent == self {
			if runs, err := runsSandbox(pid, self, bwrap); err != nil {
				errs = append(errs, err)
			} else if runs {
				outers[pid] = true
			}
		}
	}
	var fds []int
	defer func() {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}()
	// hold opens a pidfd of pid and keeps it while the process lives and
	// is, checked again through it, what is still meant.
	hold := func(pid int, meant func() (bool, error)) {
		fd, err := pidfdOpen(pid, 0)
		switch {
		case errors.Is(err, unix.ESRCH):
			return
		case err != nil:
			errs = append(errs, err)
			return
		}
		if err := unix.PidfdSendSignal(fd, 0, nil, 0); errors.Is(err, unix.ESRCH) {
			_ = unix.Close(fd) // it ended
			return
		} else if err != nil {
			_ = unix.Close(fd)
			errs = append(errs, err)
			return
		}
		ok, err := meant()
		if err != nil || !ok {
			_ = unix.Close(fd)
			if err != nil {
				errs = append(errs, err)
			}
			return
		}
		fds = append(fds, fd)
	}
	for pid := range outers {
		hold(pid, func() (bool, error) { return runsSandbox(pid, self, bwrap) })
	}
	// Each sandbox's first process is held before any signal, while it is
	// still the child of the bwrap that started it.
	for _, p := range procs {
		if !outers[p.parent] {
			continue
		}
		hold(p.pid, func() (bool, error) {
			parent, err := parentOf(p.pid)
			return err == nil && parent == p.parent, err
		})
	}
	for _, fd := range fds {
		if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			errs = append(errs, err)
		}
	}
	if len(fds) > 0 {
		if err := pidfdWait(fds, stopWait); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("stop the commands running in the sandbox: %w", errors.Join(errs...))
	}
	return nil
}

// waitEnded waits until every process of fds has ended, at most d: a
// pidfd is readable once its process has ended.
func waitEnded(fds []int, d time.Duration) error {
	deadline := time.Now().Add(d)
	pending := slices.Clone(fds)
	for len(pending) > 0 {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("%d killed processes of the sandbox did not end within %s", len(pending), d)
		}
		polls := make([]unix.PollFd, len(pending))
		for i, fd := range pending {
			polls[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
		}
		if _, err := unix.Poll(polls, int(left.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		var next []int
		for i, p := range polls {
			if p.Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
				next = append(next, pending[i])
			}
		}
		pending = next
	}
	return nil
}

// parentOf is process pid's parent: -1 for a process that is gone.
func parentOf(pid int) (int, error) {
	stat, err := readProc("/proc/" + strconv.Itoa(pid) + "/stat")
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, unix.ESRCH):
		return -1, nil
	case err != nil:
		return 0, fmt.Errorf("look at process %d: %w", pid, err)
	}
	// The command's name is in parentheses and may hold any byte: the
	// fields after its last ")" are the state, then the parent.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("look at process %d: no command name in its stat", pid)
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("look at process %d: no parent in its stat", pid)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("look at process %d: %w", pid, err)
	}
	return parent, nil
}

// runsSandbox says whether process pid is a child of parent started as
// bwrap: its parent is parent, and its first argument is bwrap's path. A
// process that is gone runs none; one whose files cannot be read is an
// error.
func runsSandbox(pid, parent int, bwrap string) (bool, error) {
	got, err := parentOf(pid)
	if err != nil || got != parent {
		return false, err
	}
	cmdline, err := readProc("/proc/" + strconv.Itoa(pid) + "/cmdline")
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, unix.ESRCH):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("look at process %d: %w", pid, err)
	}
	argv0, _, _ := bytes.Cut(cmdline, []byte{0})
	return string(argv0) == bwrap, nil
}
