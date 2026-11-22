package team

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func intp(n int) *int           { return &n }
func floatp(f float64) *float64 { return &f }

// A shift is checked in code, and gets its defaults: a limit left out is
// the default, and one given as 0 is refused (ADR-0024; Codex review of
// #36).
func TestCheckShift(t *testing.T) {
	got, err := CheckShift(Shift{Stop: "  The tests pass.  "})
	if err != nil || got.Stop != "The tests pass." || got.MaxRounds != DefaultShiftRounds || got.Hours != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := CheckShift(Shift{Stop: "x", MaxRounds: intp(100), Hours: floatp(24), CI: true}); err != nil || got.MaxRounds != 100 || got.Hours != 24 || !got.CI {
		t.Fatalf("the bounds: %+v, %v", got, err)
	}
	for name, tc := range map[string]struct {
		s    Shift
		want string
	}{
		"no stop":        {Shift{Stop: " "}, "stop condition is empty"},
		"a long stop":    {Shift{Stop: strings.Repeat("x", MaxShiftStop+1)}, "more than 3000"},
		"too many":       {Shift{Stop: "x", MaxRounds: intp(101)}, "not 1 to 100"},
		"zero rounds":    {Shift{Stop: "x", MaxRounds: intp(0)}, "max_rounds is 0"},
		"negative":       {Shift{Stop: "x", MaxRounds: intp(-1)}, "not 1 to 100"},
		"too long":       {Shift{Stop: "x", Hours: floatp(24.5)}, "at most 24"},
		"zero hours":     {Shift{Stop: "x", Hours: floatp(0)}, "above 0"},
		"negative hours": {Shift{Stop: "x", Hours: floatp(-1)}, "above 0"},
	} {
		if _, err := CheckShift(tc.s); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	if d := (ShiftPlan{Hours: 1.5}).Deadline(at); !d.Equal(at.Add(90 * time.Minute)) {
		t.Errorf("deadline %v", d)
	}
	if d := (ShiftPlan{}).Deadline(at); !d.IsZero() {
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
	s := ShiftPlan{Stop: "The tests pass.", MaxRounds: 8, CI: true}
	for name, tc := range map[string]struct {
		got  string
		want []string
	}{
		"rules": {ShiftRules(s, deadline), []string{"at most 8", "until 2026-11-21 07:00 UTC", "round 1", "done when: The tests pass.", "CI green",
			"STATUS: DONE", "STATUS: BLOCKED: <why>", "Send no team_message with status done or blocked"}},
		"a round":   {ShiftRoundText(4, 3, 8, deadline, "The tests pass.", []string{"lint", "test"}), []string{"[shift] T4 · Round 3 of 8", "until 2026-11-21 07:00 UTC", "lint, test fail", "STATUS: DONE"}},
		"a summary": {ShiftSummaryText(4, "it reached its 8 rounds", 8, 8, ShiftMaxRounds, nil, nil), []string{"has stopped: it reached its 8 rounds", "8 of 8 rounds", "status blocked", "as an ask"}},
		"done":      {ShiftSummaryText(4, "the stop condition holds", 3, 8, ShiftDone, nil, nil), []string{"status done"}},
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

// A shift takes a final report only in its summary round, with the status
// its stop asks for; a working report goes any time (ADR-0024; Codex
// review of #36).
func TestCheckShiftReport(t *testing.T) {
	for name, tc := range map[string]struct {
		state, reason, status string
		want                  string
	}{
		"working while running": {ShiftRunning, "", StatusWorking, ""},
		"done while running":    {ShiftRunning, "", StatusDone, "runs a shift, round 2 of 5"},
		"blocked while running": {ShiftRunning, "", StatusBlocked, "runs a shift"},
		"done for done":         {ShiftSummary, ShiftDone, StatusDone, ""},
		"blocked for done":      {ShiftSummary, ShiftDone, StatusBlocked, "report with status done"},
		"done for a deadline":   {ShiftSummary, ShiftDeadline, StatusDone, "stopped as deadline: report with status blocked"},
		"blocked for failed CI": {ShiftSummary, ShiftFailedCheck, StatusBlocked, ""},
		"working in a summary":  {ShiftSummary, ShiftBlocked, StatusWorking, ""},
		"done once paused":      {ShiftPaused, ShiftPerson, StatusDone, ""},
		"done once ended":       {ShiftEnded, ShiftStopped, StatusDone, ""},
	} {
		err := CheckShiftReport(7, tc.state, tc.reason, 2, 5, tc.status)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// #97: summary messages name existing asks and answers within per-list bounds.
func TestShiftSummaryNamesTheThreadsAsks(t *testing.T) {
	summary := func(open, answered []ShiftAsk) string {
		return ShiftSummaryText(1, "blocked", 1, 2, ShiftBlocked, open, answered)
	}
	empty := summary(nil, nil)
	if strings.Contains(empty, "wait for the owner already") || strings.Contains(empty, "answered these asks") {
		t.Fatalf("empty summary %q", empty)
	}
	if text := summary([]ShiftAsk{{ID: 3, Question: "Which branch?"}}, nil); !strings.Contains(text, `- A3: "Which branch?"`) || !strings.Contains(text, "Do not file them again") {
		t.Fatalf("open summary %q", text)
	}
	if text := summary(nil, []ShiftAsk{{ID: 2, Question: "Release igloo?", Answer: "yes, v0.3.0"}}); !strings.Contains(text, `- A2: "Release igloo?" → "yes, v0.3.0"`) {
		t.Fatalf("answered summary %q", text)
	}
	for _, answered := range []bool{false, true} {
		for _, n := range []int{MaxSummaryAsks, MaxSummaryAsks + 1, MaxSummaryAsks + MaxSummaryNumbers, MaxSummaryAsks + MaxSummaryNumbers + 1} {
			t.Run(fmt.Sprintf("answered=%t/count=%d", answered, n), func(t *testing.T) {
				asks := make([]ShiftAsk, n)
				for i := range asks {
					asks[i] = ShiftAsk{ID: int64(i + 1), Question: "Q", Answer: "A"}
				}
				var text string
				if answered {
					text = summary(nil, asks)
				} else {
					text = summary(asks, nil)
				}
				if got := strings.Count(text, `: "Q"`); got != MaxSummaryAsks {
					t.Fatalf("detailed asks %d: %q", got, text)
				}
				if strings.Contains(text, "- also") != (n > MaxSummaryAsks) {
					t.Fatalf("number list %q", text)
				}
				if n == MaxSummaryAsks+1 && !strings.Contains(text, fmt.Sprintf("- also A%d", n)) {
					t.Fatalf("extra ask %q", text)
				}
				if strings.Contains(text, "- and") != (n > MaxSummaryAsks+MaxSummaryNumbers) {
					t.Fatalf("overflow %q", text)
				}
				if n > MaxSummaryAsks+MaxSummaryNumbers && !strings.HasSuffix(text, "- and 1 more") {
					t.Fatalf("overflow count %q", text)
				}
				if n >= MaxSummaryAsks+MaxSummaryNumbers {
					lines := strings.Split(text, "\n")
					for _, line := range lines {
						if strings.HasPrefix(line, "- also ") && len(strings.Split(strings.TrimPrefix(line, "- also "), ", ")) != MaxSummaryNumbers {
							t.Fatalf("number count %q", line)
						}
					}
				}
			})
		}
	}
	question := strings.Repeat("é", MaxSummaryQuestion+1)
	answer := strings.Repeat("é", MaxSummaryAnswer+1)
	wantQ := strings.Repeat("é", MaxSummaryQuestion-1) + "…"
	wantA := strings.Repeat("é", MaxSummaryAnswer-1) + "…"
	for _, text := range []string{summary([]ShiftAsk{{ID: 3, Question: question}}, nil), summary(nil, []ShiftAsk{{ID: 2, Question: question, Answer: answer}})} {
		if !strings.Contains(text, fmt.Sprintf("%q", wantQ)) || strings.Contains(text, fmt.Sprintf("%q", question)) {
			t.Fatalf("question not rune-cut %q", text)
		}
	}
	if text := summary(nil, []ShiftAsk{{ID: 2, Question: question, Answer: answer}}); !strings.Contains(text, fmt.Sprintf(" → %q", wantA)) || strings.Contains(text, fmt.Sprintf("%q", answer)) {
		t.Fatalf("answer not rune-cut %q", text)
	}
}
