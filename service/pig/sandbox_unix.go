//go:build unix

package pig

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

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

// folderIDOf is the device and inode info describes.
func folderIDOf(info fs.FileInfo) folderID {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return folderID{}
	}
	return folderID{uint64(st.Dev), uint64(st.Ino)} //nolint:unconvert // the fields' types differ by system
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
