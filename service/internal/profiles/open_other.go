//go:build !unix

package profiles

import (
	"errors"
	"io/fs"
	"os"
)

// refusedLink says err is openNoFollow's refusal of a symbolic link: here
// none is a PathError, so Unreadable never takes one.
func refusedLink(error) bool { return false }

// openNoFollow opens path for reading, failing on a symbolic link.
func openNoFollow(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err != nil {
		return nil, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("a symbolic link")
	}
	return os.Open(path)
}

// openFollow opens path for reading, following its links.
func openFollow(path string) (*os.File, error) { return os.Open(path) }

// openInRoot opens name in root, failing on a symbolic link: os.Root
// resolves a final link whose target stays in the folder, whatever
// O_NOFOLLOW says (Codex review of #34, round 3). The file opened must be
// the one looked at.
func openInRoot(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("a symbolic link")
	}
	f, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errors.New("the file changed while it was opened")
	}
	return f, nil
}

// links is 1 where the system does not count a file's names.
func links(fs.FileInfo) uint64 { return 1 }
