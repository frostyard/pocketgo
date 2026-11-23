//go:build !unix

package files

import "errors"

// errUnsupported reports a platform without descriptor-relative walking.
var errUnsupported = errors.New("file viewing is not supported on this platform")

// List is unavailable on this platform.
func List(root, rel string, maxEntries, maxNames int) (Folder, error) {
	return Folder{}, errUnsupported
}

// Read is unavailable on this platform.
func Read(root, rel string, max int64) (File, error) { return File{}, errUnsupported }
