//go:build !unix

package pig

// identify and readBeneath read nothing: commands, and the reads that run
// as commands, run in the sandbox, on Linux only (ADR-0012).
func identify(string) (string, error) { return "", errNoBwrap }

func readBeneath(string, string, []string, int64) (string, []byte, error) { return "", nil, errNoBwrap }
