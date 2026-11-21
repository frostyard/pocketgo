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
	// The architecture names the version now too (Codex review of #46,
	// round 1).
	design, err := os.ReadFile("../docs/design/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := "The version is `proto.Version`, " + v + " now"; !strings.Contains(string(design), want) {
		t.Errorf("the architecture lacks %q", want)
	}
	for _, path := range []string{PathHello, PathModels, PathProfiles, PathSessions, PathSession, PathMessages, PathAbort, PathStartOver, PathModel, PathUploads,
		PathApprovals, PathApproval, PathThreads, PathCloseThread, PathAsks, PathAsk, PathLessons, PathLesson, PathPulls, PathPull, PathPlans, PathApprovePlan, PathSchedules, PathEvents,
		PathTerminals, PathTerminal, PathTerminalOutput, PathTerminalInput, PathTerminalResize} {
		if !strings.Contains(spec, " "+path+"` |") {
			t.Errorf("the spec's endpoints lack %s", path)
		}
	}
}

// The attachments' ADR names in its references the decisions its rules on
// which model gets a message's images build on: the owner's change of a
// session's model and a standing session's profile and Start Over (Codex
// review of #49, round 3).
func TestTheAttachmentsADRLinksWhatItBuildsOn(t *testing.T) {
	raw, err := os.ReadFile("../docs/adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md")
	if err != nil {
		t.Fatal(err)
	}
	_, refs, ok := strings.Cut(string(raw), "\n## References\n")
	if !ok {
		t.Fatal("the ADR has no references")
	}
	for _, link := range []string{
		"(0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)",
		"(0028-let-the-owner-change-a-sessions-model-from-the-composer.md)",
		"(../specs/protocol.md)",
	} {
		if !strings.Contains(refs, link) {
			t.Errorf("the ADR's references lack %s", link)
		}
	}
}
