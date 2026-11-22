//go:build unix && !linux

package pig

// birth is 0 here: reads run in the sandbox, on Linux only (ADR-0012).
func birth(int) uint64 { return 0 }
