//go:build linux

package uploads

import (
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

// Where statx gives no mount, a folder's mount is read from its fdinfo,
// never taken for its device (Codex review of #49, round 11).
func TestAMountIsReadWhereStatxGivesNone(t *testing.T) {
	was := statxMount
	defer func() { statxMount = was }()
	statxMount = func(int) (uint64, bool) { return 0, false }
	dir, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(dir) }()
	want, ok := was(dir)
	if !ok {
		t.Skip("statx gives no mount here")
	}
	if got, err := mountOf(dir); err != nil || got != want {
		t.Fatalf("mountOf = %d, %v; the folder's mount is %d", got, err, want)
	}
}
