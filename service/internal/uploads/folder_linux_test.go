//go:build linux

package uploads

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A file being written has no name until Link gives it one, so a stop of
// pocketd before it, or a cut-off upload whose folder no longer takes a
// removal, leaves nothing for anyone to find (Codex review of #49, round
// 3).
func TestAFileHasNoNameUntilItIsWhole(t *testing.T) {
	f, _ := testFolder(t)
	staged, err := f.Stage(7, strings.NewReader("whole"))
	if err != nil {
		t.Fatal(err)
	}
	dir := f.Path(7, "")
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a staged file shows as %v, %v", entries, err)
	}
	staged.Close()
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a staged file dropped left %v, %v", entries, err)
	}

	// The folder takes no removal from the middle of the body on: the cut
	// upload is still nowhere.
	cut := &lockingReader{dir: dir}
	if _, err := f.Stage(7, cut); err == nil {
		t.Fatal("a cut-off upload was staged")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a cut-off upload left %v, %v", entries, err)
	}
}

// lockingReader gives some bytes, then makes dir take no change, then
// fails, as a connection that goes while the disk turns read-only.
type lockingReader struct {
	dir  string
	read bool
}

func (r *lockingReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, "half"), nil
	}
	_ = os.Chmod(r.dir, 0o500)
	return 0, errors.New("the connection went")
}
