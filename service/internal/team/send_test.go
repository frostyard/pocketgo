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
	if got := SendText(5, "Odrade", "  Renumber to 0089.\n"); got != "[T5 · Odrade, the supervisor] Renumber to 0089.\n\nWhen you have acted on it, report with team_message." {
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
	if got := SentText(queued); !strings.HasPrefix(got, "Queued for thread T5:") || !strings.Contains(got, "a done thread is open again then") {
		t.Errorf("queued %q", got)
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
