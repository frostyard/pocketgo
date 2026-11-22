package app

import (
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
