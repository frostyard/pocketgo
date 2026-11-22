package team

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Only a supervisor reads the owner's Inbox (ADR-0046).
func TestCheckInboxCaller(t *testing.T) {
	if err := CheckInboxCaller(&profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}); err != nil {
		t.Fatalf("a supervisor: %v", err)
	}
	for name, caller := range map[string]*profiles.Profile{
		"an agent":   {ID: "vera", Role: profiles.RoleAgent},
		"a clerk":    {ID: "clerk", Role: profiles.RoleClerk},
		"no profile": nil,
	} {
		if err := CheckInboxCaller(caller); err == nil || !strings.Contains(err.Error(), "only a supervisor") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// inboxNow is the fixtures' clock: every time is read from it, never the
// wall clock.
var inboxNow = time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return inboxNow.Add(-d) }

func atPtr(d time.Duration) *time.Time { t := at(d); return &t }

// inboxFixture holds one item of each kind that waits, one that looks
// alike and does not, and settled items on both sides of the window's
// edge.
func inboxFixture() Inbox {
	rev := strings.Repeat("ab", 32)
	return Inbox{
		Now: inboxNow, Zone: time.UTC, PullsStarted: true, PullEvery: 5 * time.Minute,
		Threads: map[int64]InboxThread{
			5: {Title: "Fix CI", Agent: "vera"},
			6: {Title: "Tidy the docs", Agent: "duncan"},
			7: {Title: "Signing", Agent: "duncan"},
		},
		Asks: []proto.Ask{
			{ID: 3, Thread: 5, Question: "Squash the\ncommits?", Options: []string{"yes", "no"}, State: proto.AskOpen, CreatedAt: at(2 * time.Hour)},
		},
		Pulls: []proto.PullRequest{
			{ID: 1, Thread: 5, Repository: "frostyard/example", Number: 12, Title: "Fix the flaky test", State: proto.PullOpen, Ready: true, Checks: proto.ChecksGreen,
				Review: &proto.PullReview{Agent: "irulan", Family: "openai", Verdict: proto.ReviewReady}, CheckedAt: at(3 * time.Minute)},
			{ID: 2, Thread: 5, Repository: "frostyard/example", Number: 13, Title: "A draft", State: proto.PullOpen, Draft: true, CheckedAt: at(time.Minute)},
			{ID: 3, Thread: 6, Repository: "frostyard/docs", Number: 4, Title: "Tidy", State: proto.PullOpen, CheckedAt: at(10 * time.Minute)},
			// Listed for its thread, but merged: it waits for no one.
			{ID: 4, Thread: 5, Repository: "frostyard/example", Number: 9, Title: "Old fix", State: proto.PullMerged},
		},
		Plans: []proto.Plan{
			{ID: 2, Thread: 7, Lead: "duncan", Title: "Sign the ISO", State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Version: 3, Rev: rev}, UpdatedAt: at(4 * time.Hour)},
			// Its gate waits for an ask: not for the owner's approval.
			{ID: 3, Thread: 7, Lead: "duncan", Title: "Gated", State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev}, Waiting: []string{"A9 open"}},
			{ID: 4, Thread: 7, Lead: "duncan", Title: "Approved", State: proto.PlanApproved, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev, Approved: true}},
		},
		Lessons: []proto.Lesson{
			{ID: 4, Agent: "duncan", Writer: "duncan", Text: "Run make verify before you report.", State: proto.LessonPending},
			{ID: 5, Agent: "vera", Text: "Rebase first.", State: proto.LessonActive, Change: &proto.LessonChange{Text: "Rebase on main first.", Writer: "vera"}},
			{ID: 6, Agent: "vera", Text: "Ask before merging.", State: proto.LessonActive, Change: &proto.LessonChange{Forget: true, Writer: "odrade"}},
			// In force with nothing to decide.
			{ID: 7, Agent: "vera", Text: "Keep it short.", State: proto.LessonActive},
		},
		Approvals: []proto.Approval{{ID: "a3f0", SessionID: 12, Tool: "write", State: proto.ApprovalPending, RequestedAt: at(5 * time.Minute)}},
		Answered: []InboxAsk{
			{Ask: proto.Ask{ID: 1, Thread: 5, Question: "Squash?", State: proto.AskAnswered, Answer: "yes, squash it", AnsweredBy: "laptop", AnsweredAt: atPtr(InboxWindow - 30*time.Minute)}},
			{Ask: proto.Ask{ID: 2, Thread: 6, Question: "Which branch?", State: proto.AskWithdrawn}, WithdrawnAt: atPtr(2 * 24 * time.Hour)},
			{Ask: proto.Ask{ID: 8, Thread: 5, Question: "Ship it?", State: proto.AskAnswered, Answer: "not yet", AnsweredBy: "laptop", AnsweredAt: atPtr(26 * time.Hour)}},
			// Answered before the window.
			{Ask: proto.Ask{ID: 0, Thread: 5, Question: "Too old?", State: proto.AskAnswered, Answer: "old", AnsweredAt: atPtr(InboxWindow + time.Hour)}},
		},
		Closed: []proto.PullRequest{
			{ID: 5, Thread: 6, Repository: "frostyard/docs", Number: 2, Title: "Add the docs", State: proto.PullMerged, UpdatedAt: at(2 * 24 * time.Hour)},
			{ID: 6, Thread: 5, Repository: "frostyard/example", Number: 11, Title: "Abandoned", State: proto.PullClosed, UpdatedAt: at(InboxWindow - time.Minute)},
			// Recorded before the window.
			{ID: 7, Thread: 5, Repository: "frostyard/example", Number: 8, Title: "Ancient", State: proto.PullMerged, UpdatedAt: at(InboxWindow + time.Minute)},
		},
	}
}

// The answer lists what waits for the owner now, as the desktop's Inbox
// counts it, and what was settled in the window the clock given sets, the
// newest first, with when pocketd last read each pull request (ADR-0046).
func TestInboxTextListsWhatWaitsAndWhatWasSettled(t *testing.T) {
	text := InboxText(inboxFixture())
	for _, want := range []string{
		"team_inbox at Sat 21 Nov 15:00 UTC, from pocketd's records alone: nothing was read on GitHub for this answer.",
		"pocketd reads each open pull request on GitHub again every 5m;",
		"\nWaiting for the owner now:\nQuestions (1):\n" +
			`- A3 · T5 "Fix CI" · vera asked 2h ago (Sat 21 Nov 13:00 UTC): "Squash the commits?" Options: yes | no.` + "\n",
		"Pull requests to review (2):\n" +
			`- frostyard/example#12 · T5 "Fix CI" · "Fix the flaky test" · open · ready-for-human: yes · verdict at its head: ready, by irulan (openai) · CI: green · read 3m ago (Sat 21 Nov 14:57 UTC)` + "\n" +
			`- frostyard/docs#4 · T6 "Tidy the docs" · "Tidy" · open · ready-for-human: no · verdict at its head: none · CI: not read yet · read 10m ago (Sat 21 Nov 14:50 UTC)` + "\n",
		"Plans to approve (1):\n" +
			`- P2 · T7 "Signing" · "Sign the ISO" · lead duncan · rev abababababab, tasks version 3 · in review, changed 4h ago (Sat 21 Nov 11:00 UTC)` + "\n",
		"Lessons to decide (3):\n" +
			`- L4 · duncan · new, by duncan: "Run make verify before you report."` + "\n" +
			`- L5 · vera · change, by vera: "Rebase first." to "Rebase on main first."` + "\n" +
			`- L6 · vera · forget, asked by odrade: "Ask before merging."` + "\n",
		"Tool calls to approve (1):\n- a3f0 · session 12 · write · asked 5m ago (Sat 21 Nov 14:55 UTC)\n",
		"\nSettled in the last 7 days, since Sat 14 Nov 15:00 UTC:\nQuestions answered or withdrawn (3):\n" +
			`- A8 · T5 "Fix CI" · vera: "Ship it?" · answered 26h ago (Fri 20 Nov 13:00 UTC) on laptop: "not yet"` + "\n" +
			`- A2 · T6 "Tidy the docs" · duncan: "Which branch?" · withdrawn 2d ago (Thu 19 Nov 15:00 UTC) by a supervisor's message to the thread, unanswered` + "\n" +
			`- A1 · T5 "Fix CI" · vera: "Squash?" · answered 6d ago (Sat 14 Nov 15:30 UTC) on laptop: "yes, squash it"` + "\n",
		"Pull requests merged or closed whose record changed in the window (2):\n" +
			`- frostyard/docs#2 · T6 "Tidy the docs" · "Add the docs" · merged · record last changed 2d ago (Thu 19 Nov 15:00 UTC)` + "\n" +
			`- frostyard/example#11 · T5 "Fix CI" · "Abandoned" · closed · record last changed 6d ago (Sat 14 Nov 15:01 UTC)` + "\n",
		"pocketd keeps no time of a pull request's merge or close: a merged or closed one's time is when pocketd last changed its record",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"#13", "#9 ", "P3", "P4", "L7", "A0", "Too old", "#8", "Ancient", "[team_inbox: cut"} {
		if strings.Contains(text, not) {
			t.Errorf("the answer holds %q:\n%s", not, text)
		}
	}
	// Moving the clock moves the window: an hour later A1 and #11 are out.
	later := inboxFixture()
	later.Now = inboxNow.Add(time.Hour)
	text = InboxText(later)
	if strings.Contains(text, "A1 ·") || strings.Contains(text, "#11") || !strings.Contains(text, "A8 ·") || !strings.Contains(text, "#2 ") {
		t.Errorf("an hour later:\n%s", text)
	}
	// What was settled exactly at the window's start is in it.
	edge := inboxFixture()
	edge.Now = at(InboxWindow - 30*time.Minute).Add(InboxWindow)
	if text := InboxText(edge); !strings.Contains(text, "A1 ·") {
		t.Errorf("at the window's edge:\n%s", text)
	}
}

// Every list says when it is empty, and the answer says how fresh pocketd's
// pull requests are: rounds every interval, none, or not begun yet.
func TestInboxTextSaysWhatIsEmptyAndHowFresh(t *testing.T) {
	empty := Inbox{Now: inboxNow, PullsStarted: true, PullEvery: time.Hour}
	text := InboxText(empty)
	for _, want := range []string{"Questions: none.", "Pull requests to review: none.", "Plans to approve: none.", "Lessons to decide: none.",
		"Tool calls to approve: none.", "Questions answered or withdrawn: none.", "Pull requests merged or closed whose record changed in the window: none.", "again every 1h;"} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}
	empty.PullEvery = 0
	if text := InboxText(empty); !strings.Contains(text, "in no rounds (-pr-every 0)") {
		t.Errorf("no rounds:\n%s", text)
	}
	empty.PullsStarted = false
	if text := InboxText(empty); !strings.Contains(text, "have not begun since it started") {
		t.Errorf("rounds not begun:\n%s", text)
	}
}

// An answer past MaxInboxBytes keeps the same number of first items of
// every list, the most that fit, so the settled lists are never pushed
// out by the waiting ones; it says it was cut, and each cut list how many
// it has (ADR-0046).
func TestInboxTextIsCutToFitItsBounds(t *testing.T) {
	in := Inbox{Now: inboxNow, Zone: time.UTC, PullsStarted: true, PullEvery: 5 * time.Minute, Threads: map[int64]InboxThread{}}
	long := strings.Repeat("\U0001F600", 400)
	for i := range 200 {
		id := int64(i + 1)
		in.Threads[id] = InboxThread{Title: long, Agent: "vera"}
		in.Asks = append(in.Asks, proto.Ask{ID: id, Thread: id, Question: long, State: proto.AskOpen, CreatedAt: at(time.Hour)})
		in.Pulls = append(in.Pulls, proto.PullRequest{ID: id, Thread: id, Repository: "frostyard/example", Number: id, Title: long, State: proto.PullOpen, CheckedAt: at(time.Minute)})
	}
	for i := range 3 {
		id := int64(i + 1)
		in.Answered = append(in.Answered, InboxAsk{Ask: proto.Ask{ID: 1000 + id, Thread: id, Question: long, State: proto.AskAnswered, Answer: long, AnsweredAt: atPtr(time.Duration(id) * time.Hour)}})
		in.Closed = append(in.Closed, proto.PullRequest{ID: 1000 + id, Thread: id, Repository: "frostyard/example", Number: 1000 + id, Title: long, State: proto.PullMerged, UpdatedAt: at(time.Duration(id) * time.Hour)})
	}
	text := InboxText(in)
	if len(text) > MaxInboxBytes || strings.Count(text, "\n")+1 > MaxInboxLines {
		t.Fatalf("%d bytes and %d lines", len(text), strings.Count(text, "\n")+1)
	}
	keep := 0
	if _, err := fmt.Sscanf(text[strings.Index(text, "each list shows its first "):], "each list shows its first %d items", &keep); err != nil || keep == 0 {
		t.Fatalf("no cut note (%v):\n%s", err, text)
	}
	for _, want := range []string{
		fmt.Sprintf("Questions (200; the first %d shown):", keep),
		fmt.Sprintf("Pull requests to review (200; the first %d shown):", keep),
		fmt.Sprintf("A%d ·", keep), fmt.Sprintf("#%d ·", keep),
		"Questions answered or withdrawn (3", "A1001 ·", "#1001 ·",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the cut answer lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, fmt.Sprintf("A%d ·", keep+1)) {
		t.Errorf("A%d is past the cut", keep+1)
	}
	// One more item each would not have fitted.
	if fits := (inboxBounds{MaxInboxBytes, MaxInboxLines}).fits(cutWith(in, keep+1)); fits {
		t.Errorf("%d items each would have fitted too", keep+1)
	}

	// An answer within its bounds is whole, with no note.
	if text := InboxText(inboxFixture()); strings.Contains(text, "[team_inbox: cut") {
		t.Errorf("a short answer was cut:\n%s", text)
	}
	// Bounds even the headings break are met at a whole line, and said.
	tiny := inboxFixture().text(400, 6)
	if len(tiny) > 400 || strings.Count(tiny, "\n")+1 > 6 || !strings.HasSuffix(tiny, "[team_inbox: cut at a whole line to fit its bounds.]") {
		t.Errorf("tiny bounds: %d bytes, %d lines:\n%s", len(tiny), strings.Count(tiny, "\n")+1, tiny)
	}
}

// cutWith is in's answer with keep items of each list, as cut.
func cutWith(in Inbox, keep int) string {
	since := InboxSince(in.Now)
	waiting, settled := in.lists(since)
	return in.render(waiting, settled, since, keep, &inboxBounds{MaxInboxBytes, MaxInboxLines})
}

// A pull request waits for review when it is open and no draft, and a plan
// for approval when it is in review with its revision's graph and an empty
// gate: the desktop's Inbox counts them so (state.ToReview,
// state.ToApprove).
func TestPullAndPlanWait(t *testing.T) {
	for _, tc := range []struct {
		p    proto.PullRequest
		want bool
	}{
		{proto.PullRequest{State: proto.PullOpen}, true},
		{proto.PullRequest{State: proto.PullOpen, Draft: true}, false},
		{proto.PullRequest{State: proto.PullMerged}, false},
		{proto.PullRequest{State: proto.PullClosed}, false},
	} {
		if got := PullWaits(tc.p); got != tc.want {
			t.Errorf("%+v: %v", tc.p, got)
		}
	}
	rev, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for name, tc := range map[string]struct {
		p    proto.Plan
		want bool
	}{
		"ready":             {proto.Plan{State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Rev: rev}}, true},
		"drafting":          {proto.Plan{State: proto.PlanDrafting, Rev: rev, Graph: &proto.PlanGraph{Rev: rev}}, false},
		"no graph":          {proto.Plan{State: proto.PlanReview, Rev: rev}, false},
		"an older graph":    {proto.Plan{State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Rev: other}}, false},
		"approved":          {proto.Plan{State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Rev: rev, Approved: true}}, false},
		"a gate that waits": {proto.Plan{State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Rev: rev}, Waiting: []string{"K1 running"}}, false},
		"no text":           {proto.Plan{State: proto.PlanReview, Graph: &proto.PlanGraph{}}, false},
	} {
		if got := PlanWaits(tc.p); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// pocketd keeps no time of a merge or a close, and a closed pull request
// of a thread not closed is read in every round, so a change of its title
// moves its record's time: the answer names that time as the record's last
// change, never as when it was closed or first read so (Codex review of
// #91, round 1).
func TestInboxTextNamesAPullsTimeAsItsRecordsLastChange(t *testing.T) {
	in := Inbox{Now: inboxNow, Zone: time.UTC, PullsStarted: true, PullEvery: 5 * time.Minute,
		Closed: []proto.PullRequest{{ID: 1, Thread: 5, Repository: "frostyard/example", Number: 7, Title: "Renamed today", State: proto.PullClosed, UpdatedAt: at(time.Hour)}}}
	text := InboxText(in)
	for _, want := range []string{
		"Pull requests merged or closed whose record changed in the window (1):",
		`"Renamed today" · closed · record last changed 1h ago (Sat 21 Nov 14:00 UTC)`,
		"so one closed long ago may be listed for a recent change",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"recorded 1h", "closed 1h ago", "first read"} {
		if strings.Contains(text, not) {
			t.Errorf("the answer says %q:\n%s", not, text)
		}
	}
}

// The name of the device that answered is one short line, as a question
// and an answer are: a name with a line break, or a long one, neither
// splits its item nor pushes every other item out of the answer (Codex
// review of #91, round 1).
func TestInboxTextKeepsTheDevicesNameShort(t *testing.T) {
	in := inboxFixture()
	in.Answered = []InboxAsk{{Ask: proto.Ask{ID: 1, Thread: 5, Question: "Squash?", State: proto.AskAnswered, Answer: "yes",
		AnsweredBy: "lap\ntop " + strings.Repeat("\U0001F600", MaxInboxBytes), AnsweredAt: atPtr(time.Hour)}}}
	text := InboxText(in)
	if strings.Contains(text, "[team_inbox: cut") || !strings.Contains(text, "A3 ·") || !strings.Contains(text, "frostyard/example#12 ·") {
		t.Fatalf("a long device name cut the answer:\n%s", text)
	}
	line := text[strings.Index(text, "- A1 ·"):]
	line = line[:strings.IndexByte(line, '\n')]
	if !strings.Contains(line, " on lap top ") || !strings.HasSuffix(line, `…: "yes"`) || len(line) > 600 {
		t.Fatalf("the answered ask's line (%d bytes): %q", len(line), line)
	}
}
