//go:build windows

package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// shareAll lets a copy be removed while a handle is open on it: Windows
// then removes it as its last handle closes, the process's end included,
// as a Unix system does (Codex review of #49, round 6).
const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// createSnapshot makes an attached file's copy in dir (the system's
// temporary folder when empty), under a name of its own.
func createSnapshot(dir string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		f, err := openShared(filepath.Join(dir, "pocketgo-attachment-"+hex.EncodeToString(b[:])), windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		return f, err
	}
	return nil, errors.New("no free name for an attached file's copy")
}

// openSnapshot opens a copy for reading.
func openSnapshot(path string) (*os.File, error) {
	return openShared(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
}

// openShared opens path with access and disposition, sharing every access.
func openShared(path string, access, disposition uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, access, shareAll, nil, disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
