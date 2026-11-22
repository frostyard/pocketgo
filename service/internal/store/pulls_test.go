package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func pull(repo string, n int64, state string) PullRow {
	return PullRow{Repository: repo, Number: n, Title: "t", URL: "u", State: state, Head: "h", HeadSHA: strings.Repeat("a", 40), Base: "main"}
}

func pullReportText([]int64, []PullRow) string { return "r" }

// A report links its pull requests in the transaction that files it: one
// another thread links refuses the whole report, and a report filed
// already links nothing again (ADR-0016).
func TestAReportLinksItsPullRequests(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	b, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _, _, linked, err := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{pull("Frostyard/X", 3, PullOpen)}, pullReportText)
	if err != nil || len(linked) != 1 || linked[0].ThreadID != a.ID || linked[0].Ref() != "Frostyard/X#3" {
		t.Fatalf("link %+v %v", linked, err)
	}
	// The same pull request, by another thread, in GitHub's other case.
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r2", b.ID, ThreadOpen, "m", nil, []PullRow{pull("frostyard/x", 3, PullOpen)}, pullReportText); !errors.Is(err, ErrPullTaken) || !strings.Contains(err.Error(), "T1") {
		t.Fatalf("another thread's pull request: %v", err)
	}
	if got, _ := s.Thread(ctx, b.ID); got.Report != "" {
		t.Fatalf("a refused report was kept: %+v", got)
	}
	// The report run again links nothing.
	_, _, _, linked, err = s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{pull("frostyard/x", 3, PullMerged)}, pullReportText)
	if err != nil || len(linked) != 0 {
		t.Fatalf("a report run again: %+v %v", linked, err)
	}
	// A new report of the same thread brings it up to date.
	_, _, _, linked, err = s.FileReportWithPulls(ctx, "r3", a.ID, ThreadDone, "m", nil, []PullRow{pull("frostyard/x", 3, PullMerged)}, pullReportText)
	if err != nil || len(linked) != 1 || linked[0].State != PullMerged || linked[0].Repository != "frostyard/x" {
		t.Fatalf("a newer report: %+v %v", linked, err)
	}
}

func TestUpdatePullSaysWhatChanged(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _, _, linked, _ := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{pull("frostyard/x", 3, PullOpen)}, pullReportText)
	id := linked[0].ID
	if _, changed, err := s.UpdatePull(ctx, id, pull("frostyard/x", 3, PullOpen)); err != nil || changed {
		t.Fatalf("the same: %v %v", changed, err)
	}
	moved := pull("frostyard/x", 3, PullOpen)
	moved.HeadSHA = strings.Repeat("b", 40)
	if row, changed, err := s.UpdatePull(ctx, id, moved); err != nil || !changed || row.HeadSHA != moved.HeadSHA {
		t.Fatalf("a new head: %+v %v %v", row, changed, err)
	}
	if open, _ := s.OpenPulls(ctx); len(open) != 1 {
		t.Fatalf("open %+v", open)
	}
	if _, _, err := s.UpdatePull(ctx, id, pull("frostyard/x", 3, PullMerged)); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenPulls(ctx); len(open) != 0 {
		t.Fatalf("a merged pull request is open: %+v", open)
	}
	// The thread is not closed: its pull requests are listed, merged too.
	if list, _ := s.ListPulls(ctx); len(list) != 1 {
		t.Fatalf("listed %+v", list)
	}
}

// A thread closes once its pull requests are all merged or closed, one at
// least merged; one open, or none merged, keeps it (ADR-0016).
func TestCloseMergedThread(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _ = s.AttachThread(ctx, a.ID, 5, "/w", "", "b")
	_, _, _, linked, _ := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadDone, "m", nil,
		[]PullRow{pull("frostyard/x", 3, PullMerged), pull("frostyard/x", 4, PullOpen)}, pullReportText)
	text := func(merged []PullRow) string { return "merged " + merged[0].Ref() }
	if _, closed, err := s.CloseMergedThread(ctx, a.ID, text); err != nil || closed {
		t.Fatalf("one open: %v %v", closed, err)
	}
	if _, _, err := s.UpdatePull(ctx, linked[1].ID, pull("frostyard/x", 4, PullClosed)); err != nil {
		t.Fatal(err)
	}
	row, closed, err := s.CloseMergedThread(ctx, a.ID, text)
	if err != nil || !closed || row.State != ThreadClosed || row.Report != "merged frostyard/x#3" {
		t.Fatalf("all merged or closed: %+v %v %v", row, closed, err)
	}
	if _, closed, _ := s.CloseMergedThread(ctx, a.ID, text); closed {
		t.Fatal("a closed thread closed again")
	}
	b, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k2", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	_, _, _, _, _ = s.FileReportWithPulls(ctx, "r2", b.ID, ThreadDone, "m", nil, []PullRow{pull("frostyard/x", 5, PullClosed)}, pullReportText)
	if _, closed, _ := s.CloseMergedThread(ctx, b.ID, text); closed {
		t.Fatal("a thread with none merged closed")
	}
	// The closed thread's merged pull request is no longer listed.
	if list, _ := s.ListPulls(ctx); len(list) != 1 || list[0].Number != 5 {
		t.Fatalf("listed %+v", list)
	}
}

func TestPullQueriesUseTheirIndexes(t *testing.T) {
	s, _ := openFresh(t)
	for query, index := range map[string]string{
		`SELECT ` + pullColumns + ` FROM pull_requests WHERE ` + openPulls + ` ORDER BY id`: "pull_requests_open",
		`SELECT ` + pullColumns + ` FROM pull_requests WHERE thread_id = 1 ORDER BY id`:     "pull_requests_thread",
	} {
		if plan := planOf(t, s, query); !strings.Contains(plan, index) || tableScan.MatchString(strings.ReplaceAll(plan, "pull_requests", "lessons")) {
			t.Errorf("%s:\n  plan %s, want %s", query, plan, index)
		}
	}
}

// A read of GitHub older than the one a row holds changes nothing: a poll
// that read open before a report read merged never puts open back (Codex
// review of #20, round 1).
func TestAnOlderReadNeverOverwritesANewerOne(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	first := pull("frostyard/x", 3, PullOpen)
	first.ReadSeq = 1
	_, _, _, linked, _ := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{first}, pullReportText)
	newer := pull("frostyard/x", 3, PullMerged)
	newer.ReadSeq = 3
	if _, changed, err := s.UpdatePull(ctx, linked[0].ID, newer); err != nil || !changed {
		t.Fatalf("the newer read: %v %v", changed, err)
	}
	older := pull("frostyard/x", 3, PullOpen)
	older.ReadSeq = 2
	row, changed, err := s.UpdatePull(ctx, linked[0].ID, older)
	if err != nil || changed || row.State != PullMerged {
		t.Fatalf("the older read: %+v %v %v", row, changed, err)
	}
	if ids, err := s.ThreadsMergedOut(ctx); err != nil || len(ids) != 1 || ids[0] != a.ID {
		t.Fatalf("threads merged out %v %v", ids, err)
	}
}

// The supervisor's message names the pull requests as the transaction keeps
// them: a newer read of one, recorded while the report was checked, wins
// over the report's own (Codex review of #20, round 2).
func TestAReportsMessageNamesThePullRequestsAsKept(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	first := pull("frostyard/x", 3, PullOpen)
	first.ReadSeq = 1
	_, _, _, linked, _ := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{first}, pullReportText)
	closed := pull("frostyard/x", 3, PullClosed)
	closed.ReadSeq = 3
	if _, _, err := s.UpdatePull(ctx, linked[0].ID, closed); err != nil {
		t.Fatal(err)
	}
	// A report whose read began before the newer one.
	stale := pull("frostyard/x", 3, PullOpen)
	stale.ReadSeq = 2
	var named []string
	text := func(_ []int64, pulls []PullRow) string {
		for _, p := range pulls {
			named = append(named, p.Ref()+" "+p.State)
		}
		return "r"
	}
	if _, _, _, _, err := s.FileReportWithPulls(ctx, "r2", a.ID, ThreadOpen, "m", nil, []PullRow{stale}, text); err != nil {
		t.Fatal(err)
	}
	if len(named) != 1 || named[0] != "frostyard/x#3 closed" {
		t.Fatalf("the message named %v", named)
	}
}

// Reads are ordered by the store's sequence, not the clock: a later read
// stamped earlier, after the clock moved back, still wins; and GitHub's
// answer with another pull request at a linked one's name is refused
// (Codex review of #20, round 3).
func TestReadsAreOrderedBySequenceAndIdentity(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.CreateThread(ctx, ThreadRow{Key: "k1", Title: "t", Agent: "vera", SupervisorSession: 1, Task: "x"})
	first := pull("frostyard/x", 3, PullClosed)
	first.GitHubID, first.ReadSeq, first.CheckedAt = 900, 1, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	_, _, _, linked, _ := s.FileReportWithPulls(ctx, "r1", a.ID, ThreadOpen, "m", nil, []PullRow{first}, pullReportText)
	reopened := pull("frostyard/x", 3, PullOpen)
	reopened.GitHubID, reopened.ReadSeq, reopened.CheckedAt = 900, 2, first.CheckedAt.Add(-time.Hour)
	if row, changed, err := s.UpdatePull(ctx, linked[0].ID, reopened); err != nil || !changed || row.State != PullOpen {
		t.Fatalf("a later read after the clock moved back: %+v %v %v", row, changed, err)
	}
	other := pull("frostyard/x", 3, PullMerged)
	other.GitHubID, other.ReadSeq = 901, 3
	if _, _, err := s.UpdatePull(ctx, linked[0].ID, other); !errors.Is(err, ErrPullReplaced) {
		t.Fatalf("another pull request at the name: %v", err)
	}
	if a, err := s.NextPullRead(ctx); err != nil {
		t.Fatal(err)
	} else if b, _ := s.NextPullRead(ctx); b != a+1 {
		t.Fatalf("read order %d then %d", a, b)
	}
}
