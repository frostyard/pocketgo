//go:build linux

package uploads

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// errNoUnnamed is a file system that makes no unnamed file: the file is
// staged in the staging folder instead.
var errNoUnnamed = errors.New("no unnamed file here")

// createUnnamed makes a file with no name in folder dir (O_TMPFILE): until
// linkUnnamed names it, no entry shows it, and a stop of pocketd leaves
// nothing.
func createUnnamed(dir int) (*os.File, error) {
	fd, err := unix.Openat(dir, ".", unix.O_TMPFILE|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EINVAL) {
		return nil, errNoUnnamed
	} else if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "upload"), nil
}

// linkUnnamed names file, of folder dir, name, where no entry is (EEXIST),
// through its descriptor's name in /proc, which takes no privilege.
func linkUnnamed(dir int, file *os.File, name string) error {
	return unix.Linkat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(int(file.Fd())), dir, name, unix.AT_SYMLINK_FOLLOW)
}
