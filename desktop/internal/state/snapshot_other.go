//go:build !windows

package state

import "os"

// createSnapshot makes an attached file's copy in dir (the system's
// temporary folder when empty). A Unix system removes a copy while it is
// open: it goes as its last handle closes.
func createSnapshot(dir string) (*os.File, error) {
	return os.CreateTemp(dir, "pocketgo-attachment-*")
}

// openSnapshot opens a copy for reading.
func openSnapshot(path string) (*os.File, error) {
	return os.Open(path)
}
