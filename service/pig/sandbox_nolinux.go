//go:build !linux

package pig

import "os"

// stopSandboxes has nothing to stop where bubblewrap does not run: pocketd
// serve runs commands on Linux only (ADR-0012).
func stopSandboxes(string, string) error { return nil }

// spellingOf is the name f was opened by, where no /proc/self/fd tells the
// kernel's own.
func spellingOf(f *os.File) (string, error) { return f.Name(), nil }
