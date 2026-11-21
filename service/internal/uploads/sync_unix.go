//go:build unix

package uploads

import "golang.org/x/sys/unix"

// syncDir writes folder dir's entries to disk; tests count its calls.
var syncDir = func(dir int) error { return unix.Fsync(dir) }
