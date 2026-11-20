//go:build !unix

package profiles

import (
	"errors"
	"io/fs"
	"os"
)

// openNoFollow opens path for reading, failing on a symbolic link.
func openNoFollow(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err != nil {
		return nil, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("a symbolic link")
	}
	return os.Open(path)
}

// openInRoot opens name in root as openNoFollow opens a path.
func openInRoot(root *os.Root, name string) (*os.File, error) {
	if info, err := root.Lstat(name); err != nil {
		return nil, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("a symbolic link")
	}
	return root.Open(name)
}

// links is 1 where the system does not count a file's names.
func links(fs.FileInfo) uint64 { return 1 }
