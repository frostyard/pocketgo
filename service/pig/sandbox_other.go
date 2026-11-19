//go:build !unix

package pig

import "errors"

// writable is false where pocketd cannot run commands at all.
func writable(string) bool { return false }

// rootOwned refuses every path: bubblewrap runs on Linux only (ADR-0012).
func rootOwned(string) error {
	return errors.New("commands run under bubblewrap, which runs on Linux only")
}
