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
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	fd, err := unix.Open("/", walkDirFlags, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: "/", Err: err}
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
			return &os.PathError{Op: "make", Path: at, Err: err}
		}
		fd = next
	}
	return unix.Close(fd)
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
