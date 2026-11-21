//go:build unix && !linux

package pig

import (
	"os"
	"syscall"
)

// openHeld opens path to hold it: not through a link at its end, and
// without waiting for a writer when it is a named pipe.
func openHeld(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
