//go:build !linux

package pig

// stopSandboxes has nothing to stop where bubblewrap does not run: pocketd
// serve runs commands on Linux only (ADR-0012).
func stopSandboxes(string, string) error { return nil }
