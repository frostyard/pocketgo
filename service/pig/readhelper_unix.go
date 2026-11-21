//go:build unix

package pig

import (
	"io/fs"
	"os"
	"syscall"
)

// names is how many names (hard links) the file info describes has.
func names(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // Nlink's type differs by system
	}
	return 1
}

// openNonblocking opens path for reading without waiting for a writer when
// it is a named pipe: the caller checks what it opened.
func openNonblocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
