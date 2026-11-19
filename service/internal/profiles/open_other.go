//go:build !unix

package profiles

import (
	"errors"
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
