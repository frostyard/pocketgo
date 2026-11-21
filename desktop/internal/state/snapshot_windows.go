//go:build windows

package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createSnapshot makes an attached file's copy in dir (the system's
// temporary folder when empty), open for reading and writing. No other
// handle may open it, so nothing changes its bytes once their digest is
// taken, and its name is removed at once, as a Unix system removes it,
// where the file system takes POSIX deletion; elsewhere Windows removes
// it as its handle closes, the process's end included (Codex review of
// #49, rounds 6, 7 and 9).
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
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
			0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "create", Path: path, Err: err}
		}
		// Its name goes now; the handle keeps the file.
		flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
		_ = windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
		return os.NewFile(uintptr(h), path), nil
	}
	return nil, errors.New("no free name for an attached file's copy")
}
