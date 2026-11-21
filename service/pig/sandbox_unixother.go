//go:build unix && !linux

package pig

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openHeld opens path to hold it: not through a link at its end, and
// without waiting for a writer when it is a named pipe.
func openHeld(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// walkDirFlags open a folder on the way for mkdirNoFollow: a folder only,
// never through a link.
const walkDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
