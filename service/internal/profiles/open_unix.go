//go:build unix

package profiles

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading, failing on a symbolic link.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
