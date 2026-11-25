package team

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// An answer's note names its thread, its ask, its agent, the question and
// the answer cut short, that the answer goes to the agent's session, and
// the thread's next step, the first that holds (ADR-0071).
func TestAnsweredNote(t *testing.T) {
	base := Answered{Thread: 5, Title: "Fix CI", Agent: "vera", Ask: 3, Question: "Squash the\ncommits?", Answer: "Yes, squash."}
	text := AnsweredNote(base)
	for _, want := range []string{
		"[T5 · A3 answered] The owner answered vera's ask A3 in T5 \"Fix CI\".\n",
		"Question: \"Squash the commits?\"\nAnswer: \"Yes, squash.\"\n",
		"pocketd brings the answer to vera's session. vera goes on with T5 once the answer is in its session, and its next report comes to you.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the note lacks %q:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, "[T5 · A3 answered]") {
		t.Errorf("the note's header: %q", text)
	}

	// A long question and answer are cut short, each on one line.
	long := base
	long.Question, long.Answer = strings.Repeat("q\n", 400), strings.Repeat("a ", 400)
	text = AnsweredNote(long)
	lines := strings.Split(text, "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "Question: ") || utf8.RuneCountInString(lines[1]) > len(`Question: ""`)+answeredQuestion ||
		!strings.HasPrefix(lines[2], "Answer: ") || utf8.RuneCountInString(lines[2]) > len(`Answer: ""`)+answeredAnswer || !strings.Contains(lines[2], "…") {
		t.Errorf("a long note:\n%s", text)
	}

	ready := []Task{{ID: "t2", Title: "Sign", Agent: "vera", Repository: "frostyard/example"}}
	for name, tc := range map[string]struct {
		change func(*Answered)
		want   string
	}{
		"closed": {func(a *Answered) { a.Closed, a.Open = true, []int64{4} },
			"T5 is closed, so vera files no report on it: if the answer needs more work, team_send reopens T5 where pocketd allows it, or delegate a thread that follows T5."},
		"closed, a placed message reopens it": {func(a *Answered) { a.Closed, a.ReopenHeld, a.Open = true, true, []int64{4} },
			"T5 is closed, but a message already in vera's session reopens it once its worker has stopped; then vera goes on with T5, and its next report comes to you."},
		"one more ask":  {func(a *Answered) { a.Open = []int64{4} }, "A4 of T5 still waits for the owner."},
		"two more asks": {func(a *Answered) { a.Open = []int64{4, 6} }, "A4 and A6 of T5 still wait for the owner."},
		// At most MaxAnsweredOpenAsks named, the rest counted (Codex review
		// of #175, round 4).
		"a backlog of asks": {func(a *Answered) { a.Open, a.MoreOpen = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 5 },
			"A1, A2, A3, A4, A5, A6, A7, A8, A9, A10 and 5 more of T5 still wait for the owner."},
		"more named than the bound": {func(a *Answered) { a.Open = []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12} },
			"A1, A2, A3, A4, A5, A6, A7, A8, A9, A10 and 2 more of T5 still wait for the owner."}, "a plan drafted": {func(a *Answered) { a.Plan = &AnsweredPlan{ID: 2, State: PlanDrafting} }, "vera writes the answer into plan P2 with team_plan;"},
		"a plan cleared": {func(a *Answered) { a.Plan = &AnsweredPlan{ID: 2, State: PlanApproved, Cleared: true, Ready: ready} },
			"Plan P2 has no open question now: its ready tasks may go. Deliver each with team_delegate, plan {\"id\": 2, \"task\": \"<id>\"}, and the task's agent and repository:\n- t2 · Sign: agent vera, repository frostyard/example"},
		"a plan cleared, nothing ready": {func(a *Answered) { a.Plan = &AnsweredPlan{ID: 2, State: PlanBuilding, Cleared: true} },
			"Plan P2 has no open question now, and none of its tasks is ready to deliver."},
		"a plan's ask still open": {func(a *Answered) { a.Open, a.Plan = []int64{4}, &AnsweredPlan{ID: 2, State: PlanApproved} }, "A4 of T5 still waits for the owner."},
		"a plan ended":            {func(a *Answered) { a.Plan = &AnsweredPlan{ID: 2, State: PlanDone} }, "vera goes on with T5 once the answer is in its session"},
	} {
		a := base
		tc.change(&a)
		if text := AnsweredNote(a); !strings.Contains(text, "pocketd brings the answer to vera's session. "+tc.want) {
			t.Errorf("%s: the note lacks %q:\n%s", name, tc.want, text)
		}
	}
}

// An answer's key names the thread's owner, the ask's key and the answer
// it carries, never a row number; it is neither a supervisor's nor a
// device's key, so the note shows as the service's own message
// (ADR-0065, ADR-0071).
func TestAnsweredKeyNamesTheAnswer(t *testing.T) {
	key := AnsweredKey("abc123", "team:5:3:c1:ask:0", "Yes.")
	if key != AnsweredKey("abc123", "team:5:3:c1:ask:0", "Yes.") || !strings.HasPrefix(key, AnsweredPrefix+"abc123:team:5:3:c1:ask:0:") {
		t.Fatalf("the key %q", key)
	}
	for _, other := range []string{AnsweredKey("abc124", "team:5:3:c1:ask:0", "Yes."), AnsweredKey("abc123", "team:5:3:c1:ask:1", "Yes."),
		AnsweredKey("abc123", "team:5:3:c1:ask:0", "No.")} {
		if other == key {
			t.Errorf("%q is the key of another note", other)
		}
	}
	if _, ok := SentBy(key); ok || IsOwnerKey(key) || DroppedAtClose(key) {
		t.Errorf("the key %q reads as a supervisor's, a device's or one a close drops", key)
	}
}
