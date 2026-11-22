//go:build !unix

package pig

// readBeneath reads nothing: commands, and the reads that run as
// commands, run in the sandbox, on Linux only (ADR-0012).
func readBeneath(string, []string, int64) (string, []byte, error) { return "", nil, errNoBwrap }
