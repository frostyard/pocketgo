//go:build linux

package uploads

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// createUnnamed makes a file with no name in folder dir (O_TMPFILE): until
// linkUnnamed names it, no entry shows it, and a stop of pocketd leaves
// nothing. A file system that makes none gets a visible part file
// (createPart).
func createUnnamed(dir int, random func() string) (*os.File, string, error) {
	fd, err := unix.Openat(dir, ".", unix.O_TMPFILE|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EINVAL) {
		return createPart(dir, random)
	} else if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), "upload"), "", nil
}

// linkUnnamed names file, of folder dir, file name, where no entry is
// (EEXIST): an unnamed file through its descriptor's name in /proc, which
// takes no privilege, a part file by a link of its part name, which it
// then drops.
func linkUnnamed(dir int, file *os.File, part, name string) error {
	if part != "" {
		return linkPart(dir, part, name)
	}
	return unix.Linkat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(int(file.Fd())), dir, name, unix.AT_SYMLINK_FOLLOW)
}
