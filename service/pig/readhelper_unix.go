//go:build unix

package pig

import (
	"os"
	"syscall"
)

// openNonblocking opens path for reading without waiting for a writer when
// it is a named pipe: the caller checks what it opened.
func openNonblocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
