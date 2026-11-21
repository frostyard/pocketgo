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
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// pidfdOpen opens a pidfd of a process (pidfd_open(2)); tests make it
// fail as a kernel before 5.3 does.
var pidfdOpen = unix.PidfdOpen

// spellingOf is the path the kernel names the open file f by, its own
// spelling of each name on the way (/proc/self/fd).
func spellingOf(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}

// walkDirFlags open a folder on the way for mkdirNoFollow: for its
// identity alone, a folder only, never through a link.
const walkDirFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openHeld opens path to hold it, for its identity alone (O_PATH): not
// through a link at its end, without waiting on a named pipe, and with no
// right to read it, which a folder pocketd may only pass through denies
// (Codex review of #63, round 6).
func openHeld(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// readProc reads a file of /proc; tests make it fail.
var readProc = os.ReadFile

// pidfdWait waits until every process of fds has ended, at most d, and
// gives back the pidfds of those that have not; tests make it time out.
var pidfdWait = waitEnded

// stopWait bounds how long a stop waits for the sandboxes it killed to
// end.
const stopWait = 10 * time.Second

// pendingStops are the pidfds of processes a stop killed that had not
// ended when it gave up waiting: the next stop signals and waits for them
// again, since it could not find them anew once their bwrap is gone and
// they are no child of it (Codex review of #63, round 5).
var pendingStops struct {
	sync.Mutex
	fds []int
}

// stopSandboxes kills every command running in a sandbox this process
// started, and waits until each has ended (ADR-0035). A sandbox is a child
// of pocketd that runs bwrap, by the name it was started as, carrying the
// sandbox's mark (sandboxMark), and that child's own children: the first
// process of its process namespace, whose end ends every process of that
// namespace before its own end shows. Both are signalled, so none outlives
// the stop, and only through a pidfd opened before the process was
// checked again, so a process number used again meanwhile is never
// signalled: where the kernel opens no pidfd, nothing is signalled, and
// the stop fails (Codex review of #63, round 2). A process it cannot look
// at again is a failure too, never one stopped (round 3), and so is one
// that has not ended within stopWait (round 4), which the next stop waits
// for again (round 5). A sandbox with no mark stops nothing.
func stopSandboxes(bwrap, mark string) error {
	pendingStops.Lock()
	defer pendingStops.Unlock()
	fds := pendingStops.fds
	pendingStops.fds = nil
	var errs []error
	if mark != "" {
		found, err := findSandboxes(bwrap, mark)
		fds = append(fds, found...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, fd := range fds {
		if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			errs = append(errs, err)
		}
	}
	if len(fds) > 0 {
		left, err := pidfdWait(fds, stopWait)
		if err != nil {
			errs = append(errs, err)
		}
		for _, fd := range fds {
			if !slices.Contains(left, fd) {
				_ = unix.Close(fd)
			}
		}
		pendingStops.fds = left
	}
	if len(errs) > 0 {
		return fmt.Errorf("stop the commands running in the sandbox: %w", errors.Join(errs...))
	}
	return nil
}

// findSandboxes opens a pidfd of each running sandbox's bwrap and of its
// namespace's first process, each checked again through its pidfd.
func findSandboxes(bwrap, mark string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
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
			if runs, err := runsSandbox(pid, self, bwrap, mark); err != nil {
				errs = append(errs, err)
			} else if runs {
				outers[pid] = true
			}
		}
	}
	var fds []int
	// A sandbox is held whole, or not at all: its bwrap, and its first
	// process, while that is still the bwrap's child. One that cannot be
	// held whole is not signalled, so its bwrap, still running, is found
	// again by the next stop; were the bwrap killed alone, its first
	// process, no child of it then, could be found by none (Codex review
	// of #63, round 6).
	for outer := range outers {
		fd, ok, err := holdProcess(outer, func() (bool, error) { return runsSandbox(outer, self, bwrap, mark) })
		if err != nil {
			errs = append(errs, err)
		}
		if !ok {
			continue
		}
		whole := []int{fd}
		for _, p := range procs {
			if p.parent != outer {
				continue
			}
			child, ok, err := holdProcess(p.pid, func() (bool, error) {
				parent, err := parentOf(p.pid)
				return err == nil && parent == outer, err
			})
			if err != nil {
				errs = append(errs, err)
				for _, fd := range whole {
					_ = unix.Close(fd)
				}
				whole = nil
				break
			}
			if ok {
				whole = append(whole, child)
			}
		}
		// A bwrap with no first process held, as one whose first process
		// started after the look at /proc, or could not be looked at, is
		// left running and found again: killed alone, it would leave that
		// process for no stop to find (Codex review of #63, round 7).
		if len(whole) == 1 {
			_ = unix.Close(whole[0])
			errs = append(errs, fmt.Errorf("the sandbox of process %d has no first process held yet", outer))
			continue
		}
		fds = append(fds, whole...)
	}
	return fds, errors.Join(errs...)
}

// holdProcess opens a pidfd of pid, and keeps it while the process lives
// and is, checked again through it, what is still meant: ok says it is
// held, and err is a failure to tell.
func holdProcess(pid int, meant func() (bool, error)) (fd int, ok bool, err error) {
	fd, err = pidfdOpen(pid, 0)
	switch {
	case errors.Is(err, unix.ESRCH):
		return -1, false, nil
	case err != nil:
		return -1, false, err
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); errors.Is(err, unix.ESRCH) {
		_ = unix.Close(fd) // it ended
		return -1, false, nil
	} else if err != nil {
		_ = unix.Close(fd)
		return -1, false, err
	}
	ok, err = meant()
	if err != nil || !ok {
		_ = unix.Close(fd)
		return -1, false, err
	}
	return fd, true, nil
}

// waitEnded waits until every process of fds has ended, at most d: a
// pidfd is readable once its process has ended. It gives back the pidfds
// of the processes that have not.
func waitEnded(fds []int, d time.Duration) ([]int, error) {
	deadline := time.Now().Add(d)
	pending := slices.Clone(fds)
	for len(pending) > 0 {
		left := time.Until(deadline)
		if left <= 0 {
			return pending, fmt.Errorf("%d killed processes of the sandbox did not end within %s", len(pending), d)
		}
		polls := make([]unix.PollFd, len(pending))
		for i, fd := range pending {
			polls[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
		}
		if _, err := unix.Poll(polls, int(left.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return pending, err
		}
		var next []int
		for i, p := range polls {
			if p.Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
				next = append(next, pending[i])
			}
		}
		pending = next
	}
	return nil, nil
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
// bwrap with the sandbox's mark: its parent is parent, its first argument
// is bwrap's path, and its arguments set sandboxMark to mark. A process
// that is gone runs none; one whose files cannot be read is an error.
func runsSandbox(pid, parent int, bwrap, mark string) (bool, error) {
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
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(args) == 0 || args[0] != bwrap {
		return false, nil
	}
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == sandboxMark && args[i+2] == mark {
			return true, nil
		}
	}
	return false, nil
}
