package proto

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// The spec names the version proto speaks wherever it names one: its
// title, rule 1, the header a request sends and the error for another, and
// it lists every endpoint path (Codex review of pocketgo#26, round 7).
func TestTheSpecNamesThisVersion(t *testing.T) {
	raw, err := os.ReadFile("../docs/specs/protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	v := strconv.Itoa(Version)
	for _, want := range []string{
		"# Spec: pocketgo protocol, version " + v + "\n",
		"without `Pocket-Protocol: " + v + "`",
		"| `Pocket-Protocol` | yes | `" + v + "` (`proto.Version`) |",
		"`Pocket-Protocol` is missing or is not `" + v + "` (`proto.Version`)",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("the spec lacks %q", want)
		}
	}
	for _, path := range []string{PathHello, PathModels, PathProfiles, PathSessions, PathSession, PathMessages, PathAbort,
		PathApprovals, PathApproval, PathThreads, PathCloseThread, PathAsks, PathAsk, PathLessons, PathLesson, PathPulls, PathPull, PathPlans, PathApprovePlan, PathEvents} {
		if !strings.Contains(spec, " "+path+"` |") {
			t.Errorf("the spec's endpoints lack %s", path)
		}
	}
}
