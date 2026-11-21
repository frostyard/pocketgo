//go:build !unix

package pig

import (
	"io/fs"
	"os"
)

// names is one where pocketd cannot read a file in the sandbox at all.
func names(fs.FileInfo) uint64 { return 1 }

// openNonblocking refuses every path: commands, and the reads that run as
// commands, run in the sandbox, on Linux only (ADR-0012).
func openNonblocking(string) (*os.File, error) { return nil, errNoBwrap }
