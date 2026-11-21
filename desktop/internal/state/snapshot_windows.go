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

// createSnapshot makes an attached file's copy in dir (the system's
// temporary folder when empty), open for reading and writing, under a name
// of its own that Windows deletes as its handle closes, the process's end
// included, so a crash leaves nothing of it on disk (Codex review of #49,
// rounds 6 and 7).
func createSnapshot(dir string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "pocketgo-attachment-"+hex.EncodeToString(b[:]))
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "create", Path: path, Err: err}
		}
		return os.NewFile(uintptr(h), path), nil
	}
	return nil, errors.New("no free name for an attached file's copy")
}
