package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// seedTeam gives the fake pocketd a supervisor, a specialist and their
// threads: one of each group of the board, with a question, a pull request
// to review, a draft and a merged one.
func (x *testApp) seedTeam() {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor", Home: "/home/team/agents/odrade"})
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist", Home: "/home/team/agents/vera"})
	x.srv.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/agents/odrade", Model: model, Profile: "odrade",
		Route: "profile", UpdatedAt: at(-time.Hour)})
	for i, title := range []string{"Fix the flaky arm64 job", "Pin the runner image", "Draft the release notes", "Bump Go to 1.27",
		"Find why snow fails to boot", "Document the fork"} {
		id := int64(i + 1)
		x.srv.AddSession(proto.Session{ID: 10 + id, Title: "T" + itoa(int(id)) + " · " + title, Cwd: "/home/team/projects/nsl", Model: model,
			Profile: "vera", Route: "profile", UpdatedAt: at(-time.Duration(id) * time.Minute)})
	}
	thread := func(id int64, title string, st proto.ThreadState, repo, report string, updated time.Duration) {
		th := proto.Thread{ID: id, Title: title, Agent: "vera", Supervisor: 1, Session: 10 + id, Task: "Do it.", State: st, Report: report,
			CreatedAt: at(-2 * time.Hour), UpdatedAt: at(updated)}
		if repo != "" {
			th.Repository, th.Branch = repo, "vera/"+strings.ToLower(strings.ReplaceAll(title, " ", "-"))+"-t"+itoa(int(id))
		}
		x.srv.AddThread(th)
	}
	thread(1, "Fix the flaky arm64 job", proto.ThreadBlocked, "frostyard/nsl", "The job times out on the 2-year-old runner image.", -time.Minute)
	thread(2, "Pin the runner image", proto.ThreadDone, "frostyard/nsl",
		"Pinned the runner to ubuntu-24.04 and raised the timeout to 30 minutes.\n\nThe last 20 runs passed.\nOne flaked on the network.\nNothing else changed.",
		-2*time.Minute)
	thread(3, "Draft the release notes", proto.ThreadOpen, "frostyard/frostread", "", -3*time.Minute)
	thread(4, "Bump Go to 1.27", proto.ThreadStarting, "frostyard/nsl", "", -4*time.Minute)
	thread(5, "Find why snow fails to boot", proto.ThreadDone, "frostyard/snow", "The bootloader entry points at a removed kernel.", -5*time.Minute)
	thread(6, "Document the fork", proto.ThreadClosed, "", "Closed: its pull requests merged (frostyard/hotedge#1).", -6*time.Minute)
	x.srv.AddThread(proto.Thread{ID: 6, Title: "Document the fork", Agent: "vera", Supervisor: 1, Session: 16, Cwd: "/home/team/agents/vera",
		Task: "Do it.", State: proto.ThreadClosed, Report: "Closed: its pull requests merged (frostyard/hotedge#1).",
		CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-6 * time.Minute)})
	x.srv.AddAsk(proto.Ask{ID: 1, Thread: 1, SessionID: 11, Question: "Pin the runner to ubuntu-24.04?", Options: []string{"Yes", "No"},
		State: proto.AskOpen, CreatedAt: at(-time.Minute)})
	x.srv.AddPull(proto.PullRequest{ID: 1, Thread: 2, Repository: "frostyard/nsl", Number: 31, Title: "Pin the runner image to ubuntu-24.04",
		URL: "https://evil.example/frostyard/nsl/pull/31", State: proto.PullOpen, Head: "vera/pin-the-runner-image-t2", Base: "main",
		HeadSHA: strings.Repeat("c", 40), Ready: true, Checks: proto.ChecksGreen,
		Review: &proto.PullReview{ID: 4, Thread: 7, Agent: "teg", Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"},
			Family: "openai", Head: strings.Repeat("c", 40), Verdict: proto.ReviewReady, Summary: "Read the diff."},
		CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	x.srv.AddPull(proto.PullRequest{ID: 2, Thread: 3, Repository: "frostyard/frostread", Number: 12, Title: "Release notes for 0.9",
		URL: "https://github.com/frostyard/frostread/pull/12", State: proto.PullOpen, Draft: true, Head: "vera/draft-the-release-notes-t3",
		Base: "main", CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	x.srv.AddPull(proto.PullRequest{ID: 4, Thread: 5, Repository: "frostyard/snow", Number: 9, Title: "Point the bootloader at the current kernel",
		URL: "https://github.com/frostyard/snow/pull/9", State: proto.PullMerged, Head: "vera/find-why-snow-fails-to-boot-t5", Base: "main",
		CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	x.srv.AddPull(proto.PullRequest{ID: 3, Thread: 6, Repository: "frostyard/hotedge", Number: 1, Title: "Document hotedge fork divergence",
		URL: "https://github.com/frostyard/hotedge/pull/1", State: proto.PullMerged, Head: "docs", Base: "main",
		CheckedAt: at(-time.Hour), CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-time.Hour)})
}

func TestTheWorkBoardSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.tt.SetSize(1280, 1900)
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "Work Board", "5 threads on the desk, 2 wait for you",
		"Waits for you", "With the specialists", "With the supervisors", "Closed",
		"T1 · Fix the flaky arm64 job", "A1 waits for your answer.",
		"T2 · Pin the runner image", "frostyard/nsl#31 waits for your review.",
		"Vera works on it.", "pocketd sets it up for Vera.",
		"Vera reported it done; Odrade reads the report.",
		"Vera · frostyard/nsl on vera/fix-the-flaky-arm64-job-t1", "Vera · frostyard/snow on vera/find-why-snow-fails-to-boot-t5",
		"frostyard/frostread#12", "Release notes for 0.9", "draft", "Open the Inbox",
		// A merged pull request of a thread on the desk shows; it waits for
		// nobody's review, so T5 stays with its supervisor.
		"frostyard/snow#9", "Point the bootloader at the current kernel")
	if x.tt.HasText("Thread T6") {
		t.Error("a closed thread shows before its group is unfolded")
	}
	x.snapshot(t, "board")

	x.click(t, "Closed")
	x.hasAll(t, "Thread T6", "Closed.", "Vera · in agents/vera")
	// GET /v1/pulls leaves out a closed thread's pull requests once they
	// are not open: its report names them.
	if x.tt.HasText("Document hotedge fork divergence") {
		t.Error("a closed thread shows a merged pull request pocketd does not list")
	}

	// The pull request opens on GitHub at the link the client builds,
	// never the URL pocketd sent.
	x.click(t, "Open on GitHub")
	if got := x.tt.OpenedURLs(); !slices.Equal(got, []string{"https://github.com/frostyard/nsl/pull/31"}) {
		t.Errorf("opened %q", got)
	}

	// A specialist's session opens from its thread.
	x.click(t, "Open Vera's Session")
	if x.a.place != placeSession || x.a.st.Data.OpenID() != 11 {
		t.Errorf("place %v, session %d; want T1's session 11", x.a.place, x.a.st.Data.OpenID())
	}
}

// A pull request that waits for review shows in the Inbox with its thread,
// and opens on GitHub at the link the client builds.
func TestAPullRequestInTheInboxSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.connect(t)
	x.hasAll(t, "frostyard/nsl#31 waits for your review", "T2 · Pin the runner image", "Pin the runner image to ubuntu-24.04",
		"vera/pin-the-runner-image-t2 → main", "1 ask and 1 pull request wait for you", "1 pull request waits",
		"Cross-review R4 by teg (gpt-6-astra): ready at cccccccccccc · CI green", "ready-for-human")
	if x.tt.HasText("frostyard/frostread#12 waits for your review") {
		t.Error("a draft waits for review")
	}
	x.snapshot(t, "inbox-pull-request")
	x.click(t, "Open on GitHub")
	if got := x.tt.OpenedURLs(); !slices.Equal(got, []string{"https://github.com/frostyard/nsl/pull/31"}) {
		t.Errorf("opened %q", got)
	}
	// Merged on GitHub, it leaves the Inbox.
	merged := proto.PullRequest{ID: 1, Thread: 2, Repository: "frostyard/nsl", Number: 31, Title: "Pin the runner image to ubuntu-24.04",
		State: proto.PullMerged, Head: "vera/pin-the-runner-image-t2", Base: "main"}
	x.srv.Emit(proto.Event{Type: proto.EventPull, Pull: &merged})
	x.until(t, func() bool { return len(x.a.st.Data.PullsToReview()) == 0 })
	if x.tt.HasText("frostyard/nsl#31 waits for your review") {
		t.Error("a merged pull request still waits for review")
	}
	x.hasAll(t, "1 ask waits for you")
}

// The owner closes a thread from the board, after confirming: the request
// goes once, and the thread moves to the closed group.
func TestTheOwnerClosesAThreadFromTheBoard(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	x.srv.AddThread(proto.Thread{ID: 4, Title: "Bump Go to 1.27", Agent: "vera", Supervisor: 1, State: proto.ThreadBlocked,
		Report: "The module proxy refuses 1.27.", CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "1 thread on the desk", "Vera is blocked; the supervisor reads the report.")
	x.click(t, "Close Thread")
	x.hasAll(t, "Close T4?")
	if len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 4))) != 0 {
		t.Fatal("the close went before the owner confirmed it")
	}
	x.click(t, "Close T4")
	x.until(t, func() bool {
		th, _ := x.a.st.Data.Thread(4)
		return th.State == proto.ThreadClosed && x.a.st.InFlight() == 0
	})
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 4))
	var body proto.CloseThread
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.RequestID == "" {
		t.Fatalf("the close sent %+v", reqs)
	}
	x.hasAll(t, "No thread is on the desk", "Closed")
	if x.tt.HasText("Close Thread") {
		t.Error("a closed thread offers Close Thread")
	}
}

// A board whose threads failed to load says so, with Try Again, and never
// that there are none.
func TestABoardWhoseThreadsFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.FailOnce(http.MethodGet, proto.PathThreads, http.StatusBadGateway)
	x.connect(t)
	x.click(t, "Work Board")
	if st, _ := x.a.st.Data.ThreadsStatus(); st != state.ListFailed {
		t.Fatalf("threads %v", st)
	}
	x.hasAll(t, "The threads are not loaded", "Try Again")
	if x.tt.HasText("No threads yet") {
		t.Error("a board that failed to load says there are no threads")
	}
	x.click(t, "Try Again")
	x.until(t, func() bool {
		st, _ := x.a.st.Data.ThreadsStatus()
		return st == state.ListLoaded && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "No threads yet", "No thread is on the desk")
}

// An Inbox whose pull requests failed to load says so, and never that
// nothing waits.
func TestAnInboxWhosePullRequestsFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.FailOnce(http.MethodGet, proto.PathPulls, http.StatusBadGateway)
	x.connect(t)
	x.hasAll(t, "The pull requests are not loaded", "Try Again")
	if x.tt.HasText("Nothing waits for you") {
		t.Error("an Inbox whose pull requests failed says nothing waits")
	}
}

// A confirmation belongs to its sign-in: when pocketd signs the desktop
// out while Close Thread waits for the owner's word, the dialog goes, and
// signing in again never closes the thread with the old confirmation.
func TestASignOutDropsAPendingCloseConfirmation(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	x.srv.AddThread(proto.Thread{ID: 4, Title: "Bump Go to 1.27", Agent: "vera", Supervisor: 1, State: proto.ThreadOpen,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	x.connect(t)
	x.click(t, "Work Board")
	x.click(t, "Close Thread")
	x.hasAll(t, "Close T4?")
	x.srv.Revoke()
	x.a.st.Refresh()
	x.until(t, func() bool { return !x.a.st.SignedIn() })
	if x.a.dialogs.Showing() {
		t.Error("the close's confirmation outlived its sign-in")
	}
	x.srv.Reinstate()
	x.connect(t)
	x.click(t, "Work Board")
	if x.tt.HasText("Close T4?") {
		t.Error("the old confirmation shows under the new sign-in")
	}
	if reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathCloseThread, 4)); len(reqs) != 0 {
		t.Errorf("a close went without the owner's word under this sign-in: %+v", reqs)
	}
}

// A short report shows whole on the board, however it wraps: only one
// that folds is cut to four lines, and that one offers Show the Whole
// Report.
func TestAShortReportThatWrapsShowsWhole(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	report := strings.TrimSpace(strings.Repeat("The runner image is old. ", 11))
	if state.ReportFolds(report) {
		t.Fatalf("the report folds: %d characters", len(report))
	}
	x.srv.AddThread(proto.Thread{ID: 1, Title: "Fix CI", Agent: "vera", Supervisor: 1, State: proto.ThreadDone, Report: report,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	x.srv.AddThread(proto.Thread{ID: 2, Title: "Pin it", Agent: "vera", Supervisor: 1, State: proto.ThreadDone, Report: "Done.",
		CreatedAt: at(-time.Hour), UpdatedAt: at(-2 * time.Minute)})
	x.tt.SetSize(720, 1000)
	x.connect(t)
	x.click(t, "Work Board")
	whole, ok := x.tt.Find(report)
	line, ok2 := x.tt.Find("Done.")
	if !ok || !ok2 {
		t.Fatalf("report found %v, one line found %v (texts %q)", ok, ok2, x.tt.Texts())
	}
	if whole.H < 4.5*line.H {
		t.Errorf("the report is %.0f high, %.1f lines: a short report that wraps past four lines is cut", whole.H, whole.H/line.H)
	}
	if x.tt.HasText("Show the Whole Report") {
		t.Error("a short report offers Show the Whole Report")
	}
}

// The pill says what GitHub shows, and claims a ready review and green CI
// only when pocketd said both at the head (Codex review of #23, round 1).
func TestTheReadyPillClaimsOnlyWhatPocketdSaw(t *testing.T) {
	head := strings.Repeat("c", 40)
	ready := &proto.PullReview{Verdict: proto.ReviewReady, Head: head}
	for name, c := range map[string]struct {
		p      proto.PullRequest
		backed bool
	}{
		"a label by hand":      {proto.PullRequest{Ready: true, HeadSHA: head}, false},
		"a ready review, red":  {proto.PullRequest{Ready: true, HeadSHA: head, Review: ready, Checks: proto.ChecksFailing}, false},
		"a review of old head": {proto.PullRequest{Ready: true, HeadSHA: strings.Repeat("d", 40), Review: ready, Checks: proto.ChecksGreen}, false},
		"not ready, green": {proto.PullRequest{Ready: true, HeadSHA: head, Checks: proto.ChecksGreen,
			Review: &proto.PullReview{Verdict: proto.ReviewNotReady, Head: head}}, false},
		"ready and green": {proto.PullRequest{Ready: true, HeadSHA: head, Review: ready, Checks: proto.ChecksGreen}, true},
	} {
		got := readyTooltip(c.p)
		if !strings.HasPrefix(got, "It has ready-for-human on GitHub.") || strings.Contains(got, "said ready") != c.backed ||
			strings.Contains(got, "back it") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// A thread a message reopened shows reopened with its specialist, and a
// thread's card names the thread it carries on and the one that carries it
// on (ADR-0061).
func TestAReopenedThreadOnTheBoardSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "lucilla", Name: "Lucilla", Role: "specialist"})
	x.srv.AddProfile(proto.Profile{ID: "murbella", Name: "Murbella", Role: "specialist"})
	reopened := at(-time.Minute)
	x.srv.AddThread(proto.Thread{ID: 77, Title: "Take the review points on core#283", Agent: "lucilla", Supervisor: 1, Session: 9,
		Repository: "frostyard/core", Branch: "lucilla/core-283-t77", State: proto.ThreadOpen, Report: "Done at e0bfba6.",
		Successor: 84, ReopenedAt: &reopened, CreatedAt: at(-3 * time.Hour), UpdatedAt: at(-time.Minute)})
	x.srv.AddThread(proto.Thread{ID: 84, Title: "Re-check Murbella's blocking point", Agent: "murbella", Supervisor: 1, Session: 12,
		State: proto.ThreadOpen, Follows: 77, CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-2 * time.Minute)})
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "T77 · Take the review points on core#283", "reopened", "Carried on in T84.",
		"T84 · Re-check Murbella's blocking point", "Carries on T77.")
	x.snapshot(t, "board-reopened")
}

// A thread's shift shows on the board, where it is and why it stopped; the
// owner stops it after confirming, and resumes it, each request once
// (ADR-0024).
func TestAShiftOnTheBoardSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	deadline := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	x.srv.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", Supervisor: 1, Session: 9, State: proto.ThreadOpen,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute),
		Shift: &proto.Shift{Stop: "the tests pass", MaxRounds: 8, Deadline: &deadline, State: proto.ShiftRunning, Round: 3,
			Rounds: []proto.ShiftRound{{N: 1, Verdict: "continue"}, {N: 2, Verdict: "continue"}}}})
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "Shift running: round 3 of 8, until Sat 07:00 UTC.", "Stop Shift")
	if x.tt.HasText("Resume Shift") {
		t.Error("a running shift offers Resume Shift")
	}
	x.snapshot(t, "board-shift")
	x.click(t, "Stop Shift")
	x.hasAll(t, "Stop T4's shift?")
	if len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathStopShift, 4))) != 0 {
		t.Fatal("the stop went before the owner confirmed it")
	}
	x.click(t, "Stop the Shift")
	x.until(t, func() bool {
		th, _ := x.a.st.Data.Thread(4)
		return th.Shift != nil && th.Shift.State == proto.ShiftEnded && x.a.st.InFlight() == 0
	})
	if reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathStopShift, 4)); len(reqs) != 1 {
		t.Fatalf("the stop sent %+v", reqs)
	}
	x.hasAll(t, "Shift ended at round 3 of 8: the owner stopped it.", "Resume Shift")
	if x.tt.HasText("Stop Shift") {
		t.Error("an ended shift offers Stop Shift")
	}
	x.click(t, "Resume Shift")
	x.until(t, func() bool {
		th, _ := x.a.st.Data.Thread(4)
		return th.Shift.State == proto.ShiftRunning && x.a.st.InFlight() == 0
	})
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathResumeShift, 4))
	var body proto.ResumeShift
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.RequestID == "" {
		t.Fatalf("the resume sent %+v", reqs)
	}
	x.hasAll(t, "Shift running: round 4 of 8")
}

// A shift paused by the owner's message waits for the owner, who alone
// resumes or stops it, and a thread still starting offers no shift action
// pocketd would refuse (Codex review of #37).
func TestAPausedShiftWaitsForTheOwner(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
	x.srv.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", Supervisor: 1, Session: 9, State: proto.ThreadOpen,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute),
		Shift: &proto.Shift{Stop: "the tests pass", MaxRounds: 8, State: proto.ShiftPaused, Round: 2, Why: "the owner wrote to its session"}})
	x.connect(t)
	x.click(t, "Work Board")
	x.hasAll(t, "Waits for you", "Its shift is paused for your message: resume or stop it.", "Resume Shift", "Stop Shift")
	if x.tt.HasText("With the specialists") {
		t.Error("a paused shift's thread shows with the specialists")
	}

	// Paused in its last round, it has no round to resume: the card says
	// only what it offers (Codex review of #37, round 2).
	x.srv.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", Supervisor: 1, Session: 9, State: proto.ThreadOpen,
		CreatedAt: at(-time.Hour), UpdatedAt: at(-30 * time.Second),
		Shift: &proto.Shift{Stop: "the tests pass", MaxRounds: 8, State: proto.ShiftPaused, Round: 8, Why: "the owner wrote to its session"}})
	x.a.st.Refresh()
	x.until(t, func() bool { th, _ := x.a.st.Data.Thread(4); return th.Shift.Round == 8 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Its shift is paused for your message in its last round: stop it.", "Stop Shift")
	if x.tt.HasText("Resume Shift") || x.tt.HasText("resume or stop it") {
		t.Error("a pause in the last round offers a resume")
	}

	x.srv.AddThread(proto.Thread{ID: 4, Title: "Fix the flaky tests", Agent: "vera", Supervisor: 1, State: proto.ThreadStarting,
		CreatedAt: at(-time.Hour), UpdatedAt: at(0),
		Shift: &proto.Shift{Stop: "the tests pass", MaxRounds: 8, State: proto.ShiftRunning, Round: 1}})
	x.a.st.Refresh()
	x.until(t, func() bool {
		th, _ := x.a.st.Data.Thread(4)
		return th.State == proto.ThreadStarting && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "Shift running: round 1 of 8.")
	if x.tt.HasText("Stop Shift") || x.tt.HasText("Resume Shift") {
		t.Error("a starting thread offers a shift action")
	}
}
