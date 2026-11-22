package team

import (
	"strings"
	"testing"
)

// A closed thread reopens for a message unless its plan settled it at its
// close or its specialist never had its brief; each refusal says where the
// message goes instead: the thread that carries the work on, else a new
// thread, in the words of who sends (ADR-0061).
func TestCheckReopen(t *testing.T) {
	plain := ReopenTarget{ID: 77, Briefed: true}
	for _, from := range []Sender{FromOwner, FromSupervisor} {
		if err := CheckReopen(plain, from); err != nil {
			t.Errorf("a briefed thread, from %d: %v", from, err)
		}
	}
	for name, tc := range map[string]struct {
		target ReopenTarget
		from   Sender
		want   []string
	}{
		"a plan's own thread": {ReopenTarget{ID: 7, Plan: 3, Briefed: true}, FromOwner,
			[]string{"T7 is plan P3's own thread and stays closed", "Ask its supervisor to delegate the work again in a new thread."}},
		"a plan task's thread": {ReopenTarget{ID: 8, Plan: 3, PlanTask: "t2", Briefed: true}, FromSupervisor,
			[]string{"T8 delivered task t2 of plan P3 and stays closed", "Delegate the work again in a new thread with follows 8"}},
		"never briefed": {ReopenTarget{ID: 9}, FromOwner, []string{"T9 closed before its specialist had its brief"}},
		"never briefed, carried on": {ReopenTarget{ID: 9, Successor: 12, SuccessorAgent: "Murbella"}, FromOwner,
			[]string{"Send it to T12 (Murbella's thread), which carries its work on."}},
		"a plan task carried on, from the supervisor": {ReopenTarget{ID: 8, Plan: 3, PlanTask: "t2", Briefed: true, Successor: 12}, FromSupervisor,
			[]string{"Send it to T12 with team_send: that thread carries its work on."}},
	} {
		err := CheckReopen(tc.target, tc.from)
		if err == nil {
			t.Errorf("%s: no refusal", name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q lacks %q", name, err, want)
			}
		}
	}
}

// A closed thread whose folder is gone is refused with why, cut short, and
// where to go instead (ADR-0061).
func TestFolderGone(t *testing.T) {
	err := FolderGone(ReopenTarget{ID: 77, Successor: 84}, FromOwner, "/x--lucilla-t77 is not a git worktree"+strings.Repeat("!", 400))
	for _, want := range []string{"T77 is closed and its folder is no longer its own (/x--lucilla-t77 is not a git worktree", "…)", "Send it to T84, which carries its work on."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
}

// team_message's refusal on a closed thread names what the specialist can
// do instead: the successor, its plan, or the owner (ADR-0061).
func TestReportRefusedSaysWhatToDo(t *testing.T) {
	for name, tc := range map[string]struct {
		target ReopenTarget
		want   string
	}{
		"carried on": {ReopenTarget{ID: 77, Successor: 84, SuccessorAgent: "Murbella"},
			"thread T77 is closed and takes no more reports. Its work goes on in thread T84 (Murbella's thread): end your turn with what you did in your reply, and ask the owner to pass it to T84."},
		"a plan's": {ReopenTarget{ID: 8, Plan: 3, PlanTask: "t1"},
			"thread T8 is closed and takes no more reports. Plan P3 settled it when it closed: end your turn with what you did in your reply, and ask the owner what comes next."},
		"alone": {ReopenTarget{ID: 77},
			"thread T77 is closed and takes no more reports. End your turn with what you did and what is left in your reply, and ask the owner: a message from the owner or the supervisor reopens T77, and you report then."},
	} {
		if got := ReportRefused(tc.target); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// A thread closes by its merged pull requests when none is open and one
// merged; a reopened one only by a merge a read after its reopen saw open
// (ADR-0016, ADR-0061; Codex review of #121, round 4).
func TestMergedOut(t *testing.T) {
	for _, tc := range []struct {
		open, merged, sinceOpen int
		reopened                bool
		want                    bool
	}{
		{0, 0, 0, false, false},
		{0, 1, 0, false, true},
		{1, 1, 0, false, false},
		{0, 1, 0, true, false},
		{0, 2, 1, true, true},
		{1, 2, 1, true, false},
	} {
		if got := MergedOut(tc.open, tc.merged, tc.sinceOpen, tc.reopened); got != tc.want {
			t.Errorf("MergedOut(%d, %d, %d, %v) = %v", tc.open, tc.merged, tc.sinceOpen, tc.reopened, got)
		}
	}
}

// A delegation carries on a thread the caller's profile delegated, in any
// state; never a cross-review, another supervisor's, or none (ADR-0061).
func TestCheckFollows(t *testing.T) {
	if err := CheckFollows(FollowTarget{ID: 77, Exists: true, Own: true}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		f    FollowTarget
		want string
	}{
		"no number":        {FollowTarget{ID: -1}, "by its number"},
		"no thread":        {FollowTarget{ID: 77}, "no thread T77"},
		"a cross-review":   {FollowTarget{ID: 77, Exists: true, Review: true, Own: true}, "cross-review"},
		"another's thread": {FollowTarget{ID: 77, Exists: true}, "another supervisor"},
	} {
		if err := CheckFollows(tc.f); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// A supervisor's message kept for a closed thread says it reopens the
// thread, and nothing of a blocked thread's asks (ADR-0061).
func TestSendTextToClosed(t *testing.T) {
	want := "[T77 · Odrade, the supervisor] Take Murbella's point.\n\nT77 was closed when this was sent: if it still is when this reaches you, this message reopens it. When you have acted on it, report with team_message."
	if got := SendTextToClosed(77, "Odrade", " Take Murbella's point. "); got != want {
		t.Fatalf("got %q", got)
	}
}

// team_send's answer for a message kept for a closed thread says its
// delivery reopens it (ADR-0061).
func TestSentTextOfAReopeningMessage(t *testing.T) {
	queued := SentText(SentReceipt{Thread: 77, Agent: "Lucilla", State: SendQueued, Reopens: true, Caller: 3, Delegator: 3})
	if !strings.Contains(queued, "the closed thread is open again then, unless a close comes first.") {
		t.Errorf("queued: %q", queued)
	}
	unnoted := SentText(SentReceipt{Thread: 77, Agent: "Lucilla", State: SendDelivered, Reopens: true, Unnoted: true})
	if !strings.Contains(unnoted, "a closed thread opens once pocketd notes it.") {
		t.Errorf("unnoted: %q", unnoted)
	}
}
