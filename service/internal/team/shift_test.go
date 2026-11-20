package team

import (
	"strings"
	"testing"
	"time"
)

// A shift is checked in code, and gets its defaults (ADR-0024).
func TestCheckShift(t *testing.T) {
	got, err := CheckShift(Shift{Stop: "  The tests pass.  "})
	if err != nil || got.Stop != "The tests pass." || got.MaxRounds != DefaultShiftRounds || got.Hours != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := CheckShift(Shift{Stop: "x", MaxRounds: 100, Hours: 24, CI: true}); err != nil || got.MaxRounds != 100 || !got.CI {
		t.Fatalf("the bounds: %+v, %v", got, err)
	}
	for name, tc := range map[string]struct {
		s    Shift
		want string
	}{
		"no stop":        {Shift{Stop: " "}, "stop condition is empty"},
		"a long stop":    {Shift{Stop: strings.Repeat("x", MaxShiftStop+1)}, "more than 3000"},
		"too many":       {Shift{Stop: "x", MaxRounds: 101}, "not 1 to 100"},
		"negative":       {Shift{Stop: "x", MaxRounds: -1}, "not 1 to 100"},
		"too long":       {Shift{Stop: "x", Hours: 24.5}, "at most 24"},
		"negative hours": {Shift{Stop: "x", Hours: -1}, "at most 24"},
	} {
		if _, err := CheckShift(tc.s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	if d := (Shift{Hours: 1.5}).Deadline(at); !d.Equal(at.Add(90 * time.Minute)) {
		t.Errorf("deadline %v", d)
	}
	if d := (Shift{}).Deadline(at); !d.IsZero() {
		t.Errorf("no hours has a deadline %v", d)
	}
}

// A round's verdict is its answer's last STATUS line, read in code: no
// line goes on as continue (ADR-0024).
func TestParseShiftStatus(t *testing.T) {
	for text, want := range map[string][2]string{
		"Done the work.\nSTATUS: DONE":                        {VerdictDone, ""},
		"STATUS: CONTINUE\nmore text\nSTATUS: done":           {VerdictDone, ""},
		"**STATUS: BLOCKED: needs the deploy key**":           {VerdictBlocked, "needs the deploy key"},
		"status: continue: next the docs":                     {VerdictContinue, "next the docs"},
		"`STATUS: DONE.`":                                     {VerdictDone, ""},
		"I am done.":                                          {VerdictNone, ""},
		"STATUS: FINISHED":                                    {VerdictNone, ""},
		"Earlier STATUS: DONE in a sentence\nThen more work.": {VerdictNone, ""},
		"": {VerdictNone, ""},
	} {
		verdict, reason := ParseShiftStatus(text)
		if verdict != want[0] || reason != want[1] {
			t.Errorf("%q: %q %q, want %q %q", text, verdict, reason, want[0], want[1])
		}
	}
	if _, reason := ParseShiftStatus("STATUS: BLOCKED: " + strings.Repeat("y", 900)); len([]rune(reason)) > MaxShiftWhy {
		t.Errorf("a long reason is %d characters", len([]rune(reason)))
	}
}

func TestShiftTexts(t *testing.T) {
	deadline := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	s := Shift{Stop: "The tests pass.", MaxRounds: 8, CI: true}
	for name, tc := range map[string]struct {
		got  string
		want []string
	}{
		"rules": {ShiftRules(s, deadline), []string{"at most 8", "until 2026-11-21 07:00 UTC", "round 1", "done when: The tests pass.", "CI green",
			"STATUS: DONE", "STATUS: BLOCKED: <why>", "Send no team_message with status done or blocked"}},
		"a round":   {ShiftRoundText(4, 3, 8, deadline, "The tests pass.", []string{"lint", "test"}), []string{"[shift] T4 · Round 3 of 8", "until 2026-11-21 07:00 UTC", "lint, test fail", "STATUS: DONE"}},
		"a summary": {ShiftSummaryText(4, "it reached its 8 rounds", 8, 8, ShiftMaxRounds), []string{"has stopped: it reached its 8 rounds", "8 of 8 rounds", "status blocked", "as an ask"}},
		"done":      {ShiftSummaryText(4, "the stop condition holds", 3, 8, ShiftDone), []string{"status done"}},
		"a report":  {ShiftEndedReport("the owner stopped it", 2, 8), []string{"The shift ended: the owner stopped it. It took 2 of 8 rounds."}},
		"why":       {ShiftWhy(ShiftBlocked, "needs a key", 2, 8), []string{"the work is blocked: needs a key"}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.got, w) {
				t.Errorf("%s: %q lacks %q", name, tc.got, w)
			}
		}
	}
	if ShiftFingerprint([]string{"test", "lint"}) != ShiftFingerprint([]string{"lint", "test"}) || ShiftFingerprint([]string{"lint"}) == ShiftFingerprint([]string{"test"}) {
		t.Error("a fingerprint depends on the order, or not on the checks")
	}
}
