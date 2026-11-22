//go:build unix

package pig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// mkdirNoFollow makes the folder path, and each folder above it that is
// missing, with mode 0700. It walks from the root, opening each folder
// relative to the one above without following a link (walkDirFlags), so a
// link put on the way fails it and never leads it elsewhere (Codex review
// of #63, round 10).
func mkdirNoFollow(path string) error {
	fd, err := walkMakeDirs(path)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// walkMakeDirs makes every folder on the way to path, as mkdirNoFollow
// does, and gives the last of them, the folder path names, open as an
// O_PATH descriptor the caller closes.
func walkMakeDirs(path string) (fd int, err error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return -1, fmt.Errorf("%s is not an absolute path", path)
	}
	fd, err = unix.Open("/", walkDirFlags, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: "/", Err: err}
	}
	at := "/"
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name == "" {
			continue
		}
		at = filepath.Join(at, name)
		next, err := unix.Openat(fd, name, walkDirFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, name, 0o700); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, name, walkDirFlags, 0)
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, &os.PathError{Op: "make", Path: at, Err: err}
		}
		fd = next
	}
	return fd, nil
}

// makeFileNoFollow makes path an empty regular file with mode 0600,
// and each folder above it that is missing with mode 0700, unless a
// regular file is there already, which it leaves as it is, never
// truncated. It walks from the root as mkdirNoFollow does, never
// following a link, and opens the file without following a link and
// without waiting on a named pipe: a link, a folder or any other kind
// of file at path fails it (ADR-0045).
func makeFileNoFollow(path string) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	fd, err := walkMakeDirs(dir)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	made, err := unix.Openat(fd, base, unix.O_RDONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &os.PathError{Op: "make", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(made) }()
	var st unix.Stat_t
	if err := unix.Fstat(made, &st); err != nil {
		return &os.PathError{Op: "make", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &os.PathError{Op: "make", Path: path, Err: errors.New("no regular file")}
	}
	return nil
}

// writable reports whether this process may change dir's entries: create,
// rename or remove them, now or after changing the folder's mode, which its
// owner may always do.
func writable(dir string) bool {
	if syscall.Access(dir, 0o2) == nil { // W_OK
		return true
	}
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// statOwner is a path's owner and permission bits, following links; tests
// replace it, since a user namespace can show root's files as another
// user's.
var statOwner = func(path string) (uint32, fs.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	return ownerOf(path, info)
}

func ownerOf(path string, info fs.FileInfo) (uint32, fs.FileMode, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s: no owner information", path)
	}
	return st.Uid, info.Mode().Perm(), nil
}

// rootOwned checks that only root can change path: it and every folder
// above it belong to root and are writable by nobody else.
func rootOwned(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		uid, perm, err := statOwner(p)
		if err != nil {
			return err
		}
		if uid != 0 || perm&0o022 != 0 {
			return fmt.Errorf("%s can be changed by a user other than root", p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// rootOwnedEntry checks that only root can change the file info describes,
// itself (not what it links to).
func rootOwnedEntry(path string, info fs.FileInfo) error {
	uid, perm, err := ownerOf(path, info)
	if err != nil {
		return err
	}
	if uid != 0 || perm&0o022 != 0 {
		return fmt.Errorf("%s can be changed by a user other than root", path)
	}
	return nil
}
