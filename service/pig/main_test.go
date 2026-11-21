package pig

import (
	"os"
	"testing"
)

// TestMain runs the test binary as pocketd's ReadHelper when Engine.ReadUpTo
// runs it so (Options.Self is empty in tests: os.Executable() is this
// binary), as its GateHelper when a sandbox's command starts through it
// (layout.paths names this binary), and the tests otherwise.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ReadHelper {
		os.Exit(RunReadHelper(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == GateHelper {
		os.Exit(RunGate(os.Args[2:], os.Stderr))
	}
	os.Exit(m.Run())
}
