package team

import (
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// team_send is a supervisor's, names a thread, and carries 1 to 6,000
// characters, as a report does (ADR-0032).
func TestCheckSend(t *testing.T) {
	good := Send{Thread: 5, Text: "core#281 took ADR-0088: renumber yours."}
	if err := CheckSend(odrade, good); err != nil {
		t.Fatalf("a good message: %v", err)
	}
	if err := CheckSend(odrade, Send{Thread: 5, Text: strings.Repeat("é", MaxSend)}); err != nil {
		t.Fatalf("a message of %d characters: %v", MaxSend, err)
	}
	agent := &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}
	clerk := &profiles.Profile{ID: "clerk", Role: profiles.RoleClerk}
	for name, tc := range map[string]struct {
		caller *profiles.Profile
		s      Send
		want   string
	}{
		"no caller profile": {nil, good, "only a supervisor"},
		"an agent sends":    {agent, good, "only a supervisor"},
		"a clerk sends":     {clerk, good, "only a supervisor"},
		"no thread":         {odrade, Send{Text: "x"}, "name the thread"},
		"a negative thread": {odrade, Send{Thread: -1, Text: "x"}, "name the thread"},
		"an empty message":  {odrade, Send{Thread: 5, Text: " \n "}, "empty"},
		"a long message":    {odrade, Send{Thread: 5, Text: strings.Repeat("x", MaxSend+1)}, "6001 characters, more than 6000"},
	} {
		if err := CheckSend(tc.caller, tc.s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
}

// A message goes to an open, a done or a blocked thread the caller's
// profile delegated; never to a cross-review, another supervisor's thread,
// a closed one, or one whose specialist has no session yet (ADR-0032).
func TestCheckSendTarget(t *testing.T) {
	for _, state := range []string{"open", "done", "blocked"} {
		if err := CheckSendTarget(SendTarget{ID: 5, State: state, Own: true}); err != nil {
			t.Errorf("a %s thread: %v", state, err)
		}
	}
	for name, tc := range map[string]struct {
		target SendTarget
		want   string
	}{
		"a cross-review":         {SendTarget{ID: 5, State: "open", Review: true, Own: true}, "cross-review"},
		"another's review":       {SendTarget{ID: 5, State: "open", Review: true}, "cross-review"},
		"another's thread":       {SendTarget{ID: 5, State: "open"}, "another supervisor"},
		"another's closed one":   {SendTarget{ID: 5, State: "closed"}, "another supervisor"},
		"a closed thread":        {SendTarget{ID: 5, State: "closed", Own: true}, "T5 is closed"},
		"a thread that starts":   {SendTarget{ID: 5, State: "starting", Own: true}, "still starting"},
		"another's starting one": {SendTarget{ID: 5, State: "starting"}, "another supervisor"},
	} {
		if err := CheckSendTarget(tc.target); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
}

// A message's key names the supervisor's session, and only it: the
// request key of each attempt that carries it says who sent it, and no
// other key does (ADR-0032).
func TestSendKeyNamesItsSupervisor(t *testing.T) {
	key := SendKey(12, "team:12:7:call_1")
	if !IsSendKey(key) {
		t.Fatalf("%q is not a send key", key)
	}
	for _, request := range []string{key, key + ":report", key + ":report:2"} {
		if got, ok := SentBy(request); !ok || got != 12 {
			t.Errorf("SentBy(%q) = %d, %v", request, got, ok)
		}
	}
	for _, other := range []string{"u:laptop:team-send:12:x", "team:12:7:call_1:report", "thread:abc:brief", "team-send:", "team-send:12",
		"team-send:x:y", "team-send:012:y", "team-send:-3:y", "team-send:0:y", "worker-msg:team:3:4:w:report"} {
		if got, ok := SentBy(other); ok {
			t.Errorf("SentBy(%q) = %d, true", other, got)
		}
	}
	if IsSendKey("u:laptop:team-send:12:x") || IsSendKey("team:12:7:c") {
		t.Error("an owner's or a report's key reads as a send key")
	}
}

func TestSendTexts(t *testing.T) {
	// The message says what it does to a blocked thread, as a rule: its
	// text is kept before pocketd knows the thread's state at its
	// placement (ADR-0037).
	if got := SendText(5, "Odrade", "  Renumber to 0089.\n"); got != "[T5 · Odrade, the supervisor] Renumber to 0089.\n\n"+
		"If T5 is blocked when this reaches you, this message unblocks it: pocketd withdraws its asks that wait for the owner, who will not answer them, "+
		"and T5 is open again. When you have acted on it, report with team_message." {
		t.Errorf("the message %q", got)
	}
	delivered := SentReceipt{Thread: 5, Agent: "Lucilla", State: SendDelivered, ThreadState: "open", Caller: 3, Delegator: 3}
	if got := SentText(delivered); got != "Delivered to thread T5: the message is in Lucilla's session. T5 is open: its next report is expected. Its reports come back to you as messages." {
		t.Errorf("delivered %q", got)
	}
	// The state now, never which message moved it (Codex review of #56,
	// round 1).
	done := delivered
	done.ThreadState = "done"
	if got := SentText(done); !strings.Contains(got, "T5 is done now.") || strings.Contains(got, "again") {
		t.Errorf("delivered to a thread done since %q", got)
	}
	queued := SentReceipt{Thread: 5, Agent: "Lucilla", State: SendQueued, Caller: 3, Delegator: 3}
	if got := SentText(queued); !strings.HasPrefix(got, "Queued for thread T5:") || !strings.Contains(got, "a done or blocked thread is open again then") ||
		!strings.Contains(got, "a blocked one's asks that wait for the owner are withdrawn") {
		t.Errorf("queued %q", got)
	}
	// A delivery that unblocked the thread says so, and names each ask it
	// withdrew from the owner's Inbox (ADR-0037).
	unblocked := delivered
	unblocked.Unblocked = true
	unblocked.Withdrawn = []WithdrawnAsk{{ID: 7, Question: "Which branch?"}}
	if got := SentText(unblocked); got != "Delivered to thread T5: the message is in Lucilla's session. It unblocked T5, which was blocked: "+
		"pocketd withdrew A7 (\"Which branch?\") from the owner's Inbox, and the owner will not answer it. "+
		"T5 is open: its next report is expected. Its reports come back to you as messages." {
		t.Errorf("unblocked %q", got)
	}
	unblocked.Withdrawn = append(unblocked.Withdrawn, WithdrawnAsk{ID: 8, Question: "Squash?"}, WithdrawnAsk{ID: 9, Question: "Tag?"})
	if got := SentText(unblocked); !strings.Contains(got, "withdrew A7 (\"Which branch?\"), A8 (\"Squash?\") and A9 (\"Tag?\") from the owner's Inbox, and the owner will not answer them.") {
		t.Errorf("unblocked, three asks %q", got)
	}
	unblocked.Withdrawn = nil
	if got := SentText(unblocked); !strings.Contains(got, "It unblocked T5, which was blocked; no ask of it waited for the owner.") {
		t.Errorf("unblocked, no ask %q", got)
	}
	unblocked.WithdrawnUnread = true
	if got := SentText(unblocked); !strings.Contains(got, "could not read which") {
		t.Errorf("unblocked, asks unread %q", got)
	}
	if got := SentText(delivered); strings.Contains(got, "unblocked") {
		t.Errorf("a delivery that unblocked nothing says it did: %q", got)
	}
	// However many asks the thread had, the answer stays within a tool
	// result's budget, and keeps its end (Codex review of #62, round 1).
	unblocked.WithdrawnUnread = false
	unblocked.Withdrawn = nil
	for i := range 300 {
		unblocked.Withdrawn = append(unblocked.Withdrawn, WithdrawnAsk{ID: int64(i + 1), Question: strings.Repeat("é", MaxQuestion)})
	}
	many := SentText(unblocked)
	if len(many) > 8<<10 || !strings.Contains(many, "A8 (\"") || strings.Contains(many, "A9 (\"") || !strings.Contains(many, ", A108 and 192 more from the owner's Inbox") ||
		strings.Contains(many, "A109") || !strings.HasSuffix(many, "T5 is open: its next report is expected. Its reports come back to you as messages.") {
		t.Errorf("unblocked, 300 asks: %d bytes %q", len(many), many)
	}
	if got := AnswerWithdrawn(7, 5); !strings.HasPrefix(got, "A7 was withdrawn: the supervisor's message reached T5's specialist") || !strings.Contains(got, "did not send your answer") {
		t.Errorf("the refusal of an answer %q", got)
	}
	// A message kept is never told as failed: its place unknown, it says
	// not to send it again (Codex review of #56, round 1).
	unknown := SentReceipt{Thread: 5, Agent: "Lucilla", State: SendUnconfirmed, Caller: 3, Delegator: 3}
	if got := SentText(unknown); !strings.HasPrefix(got, "Kept for thread T5:") || !strings.Contains(got, "Do not send it again.") {
		t.Errorf("unconfirmed %q", got)
	}
	if got := SentText(SentReceipt{State: SendUnconfirmed}); !strings.Contains(got, "Do not send it again.") || strings.Contains(got, "T0") {
		t.Errorf("unconfirmed with no thread %q", got)
	}
	// Another session of the profile hears where the reports go (Codex
	// review of #56, round 1).
	other := queued
	other.Caller = 9
	if got := SentText(other); !strings.Contains(got, "Its reports go to session 3, which delegated T5, not to this one.") || strings.Contains(got, "to you") {
		t.Errorf("from another session %q", got)
	}
}
