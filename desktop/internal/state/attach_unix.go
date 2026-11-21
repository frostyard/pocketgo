//go:build unix

package state

import (
	"os"
	"syscall"
)

// openUploadFile opens a file to upload without waiting on a named pipe:
// FileSource then refuses anything but a regular file.
func openUploadFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
