package pig

import (
	"os"
	"testing"
)

// TestMain runs the test binary as pocketd's ReadHelper when Engine.ReadUpTo
// runs it so, and as its ContextHelper when Engine.loadContext does
// (Options.Self is empty in tests: os.Executable() is this binary), as its GateHelper when a sandbox's command starts through it
// (layout.paths names this binary), as identityHelper when a test runs it
// under a second name for a folder, and the tests otherwise.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ReadHelper {
		os.Exit(RunReadHelper(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == ReadBeneathHelper {
		os.Exit(RunReadBeneathHelper(os.Args[2:], os.Stdout))
	}
	// The file viewer reads agent homes through its sandbox helper (ADR-0066).
	if len(os.Args) > 1 && os.Args[1] == ReadFilesHelper {
		os.Exit(RunReadFilesHelper(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == ContextHelper {
		os.Exit(RunContextHelper(os.Args[2:], os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == GateHelper {
		os.Exit(RunGate(os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == identityHelper {
		os.Exit(runIdentityHelper(os.Args[2:], os.Stdout))
	}
	os.Exit(m.Run())
}
