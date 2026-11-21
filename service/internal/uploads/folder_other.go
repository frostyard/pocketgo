//go:build !unix

package uploads

import (
	"errors"
	"io"
)

// errNoUnix is every folder operation where pocketd does not run: it runs
// its agents in bubblewrap, on Linux.
var errNoUnix = errors.New("the uploads folder needs a Unix host")

// Staged is an upload's file, written whole and not named yet.
type Staged struct{ Identity Identity }

// Stage fails here, whether or not the host makes unnamed files.
func (f *Folder) Stage(int64, io.Reader) (*Staged, error) {
	_ = f.noUnnamed
	return nil, errNoUnix
}

// Link fails here, as it fails for a name that is no file's.
func (s *Staged) Link(file string) error {
	if !oneName(file) {
		return errors.New("no upload's file name")
	}
	return errNoUnix
}

// Close does nothing here.
func (s *Staged) Close() {}

// Read fails here.
func (f *Folder) Read(int64, string, int64) ([]byte, error) { return nil, errNoUnix }

// Holds fails here.
func (f *Folder) Holds(int64, string, Identity) (bool, error) { return false, errNoUnix }

// ClearStaging does nothing here.
func (f *Folder) ClearStaging() error { return nil }
