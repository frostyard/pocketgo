//go:build js && wasm

package state

import (
	"bytes"
	"os"
)

// Browsers cannot create anonymous OS files. Keep the same bounded upload
// snapshot until the attachment is removed or the store drops the sign-in.
type snapshotBacking struct {
	bytes.Buffer
	closed bool
}

func createSnapshot(string) (*snapshotBacking, error) { return &snapshotBacking{}, nil }
func (s *snapshotBacking) ReadAt(p []byte, off int64) (int, error) {
	if s.closed {
		return 0, os.ErrClosed
	}
	return bytes.NewReader(s.Bytes()).ReadAt(p, off)
}
func (s *snapshotBacking) Close() error { s.closed = true; s.Buffer = bytes.Buffer{}; return nil }
