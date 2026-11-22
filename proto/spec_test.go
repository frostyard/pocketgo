package proto

import (
	"encoding/json"
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
	for _, path := range []string{PathHello, PathModels, PathProfiles, PathSessions, PathSession, PathMessages, PathAbort, PathStartOver, PathModel, PathUploads, PathQuestion,
		PathApprovals, PathApproval, PathThreads, PathCloseThread, PathAsks, PathAsk, PathLessons, PathLesson, PathPulls, PathPull, PathPlans, PathApprovePlan, PathSchedules, PathGathers, PathProposals, PathPrompts, PathEvents,
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

// The origin rule decides where the device token and the owner's uploads
// go, so a change to it is read line by line: risk level L1, as the
// desktop's url.go (Codex review of #49, round 7).
func TestTheOriginRuleIsAnL1Path(t *testing.T) {
	raw, err := os.ReadFile("../.github/risk-paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Paths []struct{ Glob, Level string }
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Paths {
		if p.Glob == "proto/origin.go" && p.Level == "L1" {
			return
		}
	}
	t.Fatal("proto/origin.go is no L1 risk path")
}

// Rule 1 says what a version 23 client does with a withdrawn ask, as its
// code does: it drops the ask's card as an answered one's, since it keeps
// only open asks, and never keeps it (Codex review of #62, round 3).
func TestTheSpecSaysWhatAVersion23ClientDoesWithAWithdrawnAsk(t *testing.T) {
	raw, err := os.ReadFile("../docs/specs/protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	if strings.Contains(spec, "would keep a withdrawn ask") {
		t.Error("the spec says a version 23 client keeps a withdrawn ask: it drops every ask that is not open")
	}
	if !strings.Contains(spec, "a version 23 client knows no `withdrawn` state: it drops a withdrawn ask's card as an answered one's, with no word of why") {
		t.Error("the spec does not say what a version 23 client does with a withdrawn ask")
	}
}
