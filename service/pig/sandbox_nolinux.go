//go:build !linux

package pig

// stopRunning has nothing to stop where bubblewrap does not run: pocketd
// serve runs commands on Linux only (ADR-0012).
func stopRunning(string) error { return nil }
