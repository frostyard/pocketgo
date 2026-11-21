//go:build !unix

package pig

import "os"

// openNonblocking refuses every path: commands, and the reads that run as
// commands, run in the sandbox, on Linux only (ADR-0012).
func openNonblocking(string) (*os.File, error) { return nil, errNoBwrap }
