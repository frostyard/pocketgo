package proto

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The spec names the protocol version as Version does, everywhere it
// names one as the header's value: its title, the header table, the
// errors and rule 1 (Codex review of #25, round 1).
func TestTheSpecNamesThisVersion(t *testing.T) {
	spec, err := os.ReadFile("../docs/specs/protocol.md")
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(Version)
	found := 0
	for _, pattern := range []string{
		`# Spec: pocketgo protocol, version (\d+)`,
		"`Pocket-Protocol: (\\d+)`",
		"`(\\d+)` \\(`proto.Version`\\)",
	} {
		for _, m := range regexp.MustCompile(pattern).FindAllSubmatch(spec, -1) {
			found++
			if string(m[1]) != want {
				t.Errorf("the spec says %q, and proto.Version is %s", m[0], want)
			}
		}
	}
	if found < 4 {
		t.Fatalf("the spec names the version %d times; want its title, the header table, the errors and rule 1", found)
	}
}
