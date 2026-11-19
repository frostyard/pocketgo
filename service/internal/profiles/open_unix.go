//go:build unix

package profiles

import (
	"io/fs"
	"os"
	"syscall"
)

// openNoFollow opens path for reading, failing on a symbolic link, and
// without waiting for a writer when it is a named pipe: the caller checks
// it is a regular file.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// links is how many names the file has: a profile with another name could
// be changed through it, outside the protected folder.
func links(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
