//go:build unix

package profiles

import (
	"os"
	"syscall"
)

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// nonblocking says f's descriptor has O_NONBLOCK, read without changing
// its mode (Fd would make it blocking).
func nonblocking(f *os.File) (bool, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var flags uintptr
	var errno syscall.Errno
	if err := raw.Control(func(fd uintptr) { flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0) }); err != nil {
		return false, err
	}
	if errno != 0 {
		return false, errno
	}
	return flags&syscall.O_NONBLOCK != 0, nil
}
