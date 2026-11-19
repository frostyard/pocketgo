package store

import (
	"strings"
	"testing"
)

// linkedPull is a thread with one linked open pull request.
func linkedPull(t *testing.T, s *Store) PullRow {
	t.Helper()
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _, _, linked, err := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{pull("frostyard/x", 3, PullOpen)}, pullReportText)
	if err != nil || len(linked) != 1 {
		t.Fatalf("link %+v %v", linked, err)
	}
	return linked[0]
}

func review(key string, p PullRow, verdict string) ReviewRow {
	return ReviewRow{Key: key, PullID: p.ID, ThreadID: 9, SessionID: 90, Agent: "teg", Provider: "openai-codex", Model: "gpt-6-astra",
		Family: "openai", HeadSHA: p.HeadSHA, Verdict: verdict, Summary: "s", Findings: []string{"f"}}
}

// A review is recorded with the read that checked its head, once per call;
// the newest at a head decides.
func TestAReviewIsRecordedOncePerCall(t *testing.T) {
	s, _ := open(t)
	p := linkedPull(t, s)
	read := p
	read.ReadSeq, read.Title = p.ReadSeq+1, "renamed"
	r, row, changed, replayed, err := s.AddReview(ctx, read, review("c1", p, "not-ready"))
	if err != nil || replayed || !changed || row.Title != "renamed" || r.ID == 0 || r.Findings[0] != "f" {
		t.Fatalf("first: %+v %+v %v %v %v", r, row, changed, replayed, err)
	}
	again, _, _, replayed, err := s.AddReview(ctx, read, review("c1", p, "ready"))
	if err != nil || !replayed || again.ID != r.ID || again.Verdict != "not-ready" {
		t.Fatalf("the call run again: %+v %v %v", again, replayed, err)
	}
	if got, found, _ := s.ReviewCall(ctx, "c1"); !found || got.ID != r.ID {
		t.Fatalf("ReviewCall %+v %v", got, found)
	}
	r2, _, _, _, _ := s.AddReview(ctx, read, review("c2", p, "ready"))
	if got, found, err := s.LatestReview(ctx, p.ID, p.HeadSHA); err != nil || !found || got.ID != r2.ID || got.Verdict != "ready" {
		t.Fatalf("latest %+v %v %v", got, found, err)
	}
	if _, found, _ := s.LatestReview(ctx, p.ID, strings.Repeat("b", 40)); found {
		t.Fatal("a review counts at another head")
	}
	none := review("c3", p, "ready")
	none.PullID = 999
	if _, _, _, _, err := s.AddReview(ctx, read, none); err == nil {
		t.Fatal("a review of no linked pull request was recorded")
	}
}

// The label's state follows the head: a gate set for an old head changes
// nothing, and a new head forgets CI's state.
func TestThePullGateFollowsTheHead(t *testing.T) {
	s, _ := open(t)
	p := linkedPull(t, s)
	if got, found, err := s.PullByRef(ctx, "Frostyard/X", 3); err != nil || !found || got.ID != p.ID {
		t.Fatalf("PullByRef %+v %v %v", got, found, err)
	}
	row, changed, err := s.SetPullGate(ctx, p.ID, p.HeadSHA, true, "green")
	if err != nil || !changed || !row.Ready || row.Checks != "green" {
		t.Fatalf("gate %+v %v %v", row, changed, err)
	}
	if _, changed, _ := s.SetPullGate(ctx, p.ID, p.HeadSHA, true, "green"); changed {
		t.Fatal("the same gate changed the row")
	}
	if _, changed, _ := s.SetPullGate(ctx, p.ID, strings.Repeat("b", 40), false, ""); changed {
		t.Fatal("a gate for another head changed the row")
	}
	moved := pull("frostyard/x", 3, PullOpen)
	moved.ReadSeq, moved.HeadSHA, moved.Ready = row.ReadSeq+1, strings.Repeat("b", 40), true
	row, changed, err = s.UpdatePull(ctx, p.ID, moved)
	if err != nil || !changed || row.Checks != "" || !row.Ready {
		t.Fatalf("a new head: %+v %v %v", row, changed, err)
	}
	// The label read off GitHub is a change.
	off := moved
	off.ReadSeq, off.Ready = moved.ReadSeq+1, false
	if row, changed, _ = s.UpdatePull(ctx, p.ID, off); !changed || row.Ready {
		t.Fatalf("the label gone: %+v %v", row, changed)
	}
}
