//go:build unix

package pig

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// identity is the device, the inode and the birth time of the folder fd
// names: a folder removed and made again at the same inode, as a file
// system reuses one, is born at another time (birth; 0 where the system
// keeps none).
func identity(fd int) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", uint64(st.Dev), uint64(st.Ino), birth(fd)), nil //nolint:unconvert // the fields' types differ by system
}

// identify is ReadBeneathHelper's look at root: its identity, "" for no
// folder there.
func identify(root string) (string, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if notThere(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(fd) }()
	return identity(fd)
}

// readBeneath is ReadBeneathHelper's read of parts below root, which id
// must name: each folder opened from the one above it by its descriptor,
// with no symbolic link followed, and the file the same way, without
// waiting for a writer. A part that is missing, a link, no folder or not
// readable ends at "none", but a link at the last part, whose target it
// gives.
func readBeneath(root, id string, parts []string, max int64) (string, []byte, error) {
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if notThere(err) {
		return "moved", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	if got, err := identity(dir); err != nil {
		_ = unix.Close(dir)
		return "", nil, err
	} else if got != id {
		_ = unix.Close(dir)
		return "moved", nil, nil
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dir)
		if notThere(err) {
			return "none", nil, nil
		} else if err != nil {
			return "", nil, err
		}
		dir = next
	}
	defer func() { _ = unix.Close(dir) }()
	name := parts[len(parts)-1]
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		// A symbolic link: its target, as git shows a link.
		buf := make([]byte, linkTarget)
		n, err := unix.Readlinkat(dir, name, buf)
		if err != nil {
			return "none", nil, nil
		}
		return "link", buf[:n], nil
	}
	if notThere(err) {
		return "none", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "none", nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return "", nil, err
	}
	// git records a file its owner may run as 100755.
	if info.Mode().Perm()&0o100 != 0 {
		return "exec", data, nil
	}
	return "file", data, nil
}

// notThere says an open found no entry it may read there: missing, a link
// or no folder on the way, not readable, or a socket.
func notThere(err error) bool {
	for _, errno := range []error{unix.ENOENT, unix.ENOTDIR, unix.ELOOP, unix.EACCES, unix.EPERM, unix.ENXIO} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
