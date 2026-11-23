package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// addProposal gives the fake pocketd one proposal on the board.
func (x *testApp) addProposal() {
	x.srv.AddProposal(proto.Proposal{ID: 12, Gather: 3, GatheredAt: at(-2 * time.Hour), Agent: "vera", Repository: "frostyard/nsl",
		Title: "Pin the CI runner image", Task: "Do it.", StopWhen: "it is done", MaxRounds: 12, Kind: "ci-sweep",
		Note: "The arm64 job flakes on the 2-year-old image; pinning it should end the flakes.", Order: 1, Rev: 1,
		State: proto.ProposalProposed, By: "vera", CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-2 * time.Hour)})
}

// above fails unless the text first shows above the text second.
func (x *testApp) above(t *testing.T, first, second string) {
	t.Helper()
	a, ok := x.tt.Find(first)
	b, found := x.tt.Find(second)
	if !ok || !found {
		t.Fatalf("no %q (%v) or %q (%v) in %q", first, ok, second, found, x.tt.Texts())
	}
	if a.Y >= b.Y {
		t.Errorf("%q (%+v) is not above %q (%+v)", first, a, second, b)
	}
}

// On a phone, the board's Proposals and Schedules sections that hold
// nothing go after the threads, the owner's answer of 2026-11-21 to #85's
// question; one that holds something keeps its place above them
// (state.BoardOrder).
func TestAPhonesBoardPutsEmptySectionsAfterTheThreads(t *testing.T) {
	x := newTestApp(t)
	x.phoneApp(t, false, nil)
	x.tap(t, "Board")
	x.above(t, "T1 · Fix the flaky arm64 job", "Proposals")
	x.above(t, "T1 · Fix the flaky arm64 job", "Schedules")
	x.above(t, "Proposals", "Schedules")
}

// A launch's confirmation keeps the Proposals section above the threads
// on a phone once the last proposal it launched left the board: the owner
// waits on it there (Codex review of #90, round 1).
func TestAPhonesBoardKeepsALaunchAboveTheThreads(t *testing.T) {
	x := newTestApp(t)
	x.addProposal()
	x.phoneApp(t, false, nil)
	x.tap(t, "Board")
	x.tap(t, "Pick #12")
	x.tap(t, "Launch picks (1)")
	x.until(t, func() bool {
		return len(x.srv.Launches()) == 1 && x.a.st.InFlight() == 0 && len(x.a.st.Data.BoardProposals()) == 0
	})
	x.above(t, "Launched L1: 1 proposal until tomorrow 06:45, from the supervisor's standing session.", "T1 · Fix the flaky arm64 job")
}

// The board reads its proposals once a frame, for its order and its
// Proposals section alike (Codex review of #90, round 1).
func TestTheBoardReadsItsProposalsOnce(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	reads := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "BoardProposals" {
				reads++
			}
			return true
		})
	}
	if reads != 1 {
		t.Errorf("the app reads Data.BoardProposals %d times, want once, in boardPane", reads)
	}
}

// A proposal keeps the Proposals section above the threads on a phone,
// and the empty Schedules goes after them.
func TestAPhonesBoardKeepsAProposalAboveTheThreadsSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.addProposal()
	x.phoneApp(t, true, nil)
	x.tap(t, "Board")
	x.above(t, "#12 · Pin the CI runner image", "T1 · Fix the flaky arm64 job")
	x.above(t, "T1 · Fix the flaky arm64 job", "Schedules")
	x.snapshot(t, "phone-board-proposal")
}
