//go:build !windows

package state

import "os"

// createSnapshot makes an attached file's copy in dir (the system's
// temporary folder when empty), open for reading and writing, and removes
// its name at once: the system frees the copy as its handle closes, the
// process's end included, so a crash leaves nothing of it on disk (Codex
// review of #49, round 7).
func createSnapshot(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, "pocketgo-attachment-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
