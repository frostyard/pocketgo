package github

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const at = "8869f09a1b2c3d4e5f60718293a4b5c6d7e8f901"

// The labels are read, and an answer without them is refused.
func TestReadReadsTheLabels(t *testing.T) {
	p, err := parse("frostyard/hotedge", 1, merged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Labels, ",") != "risk:high,ready-for-human" {
		t.Errorf("labels %q", p.Labels)
	}
	for name, out := range map[string]string{
		"no labels":       strings.Replace(merged, `"labels":[{"name":"risk:high"},{"name":"ready-for-human"}],`, ``, 1),
		"null labels":     strings.Replace(merged, `[{"name":"risk:high"},{"name":"ready-for-human"}]`, `null`, 1),
		"a nameless one":  strings.Replace(merged, `{"name":"risk:high"}`, `{"id":3}`, 1),
		"labels as a map": strings.Replace(merged, `[{"name":"risk:high"},{"name":"ready-for-human"}]`, `{"name":"x"}`, 1),
	} {
		if _, err := parse("frostyard/hotedge", 1, out); err == nil {
			t.Errorf("%s is taken", name)
		}
	}
}

// ghAnswers answers each gh call by its path.
func ghAnswers(answers map[string]string, calls *[]string) GH {
	return func(_ context.Context, a ...string) (string, error) {
		*calls = append(*calls, strings.Join(a, " "))
		for path, out := range answers {
			if strings.Contains(strings.Join(a, " "), path) {
				if strings.HasPrefix(out, "error:") {
					return "", errors.New(strings.TrimPrefix(out, "error:"))
				}
				return out, nil
			}
		}
		return "", errors.New("gh: Not Found (HTTP 404)")
	}
}

func runs(list ...string) string {
	return `{"total_count":` + itoa(len(list)) + `,"check_runs":[` + strings.Join(list, ",") + `]}`
}

func run(name, status, conclusion string) string {
	c := `null`
	if conclusion != "" {
		c = `"` + conclusion + `"`
	}
	return `{"name":"` + name + `","status":"` + status + `","conclusion":` + c + `}`
}

func statuses(list ...string) string {
	return `{"state":"x","total_count":` + itoa(len(list)) + `,"statuses":[` + strings.Join(list, ",") + `]}`
}

func itoa(n int) string { return strconv.Itoa(n) }

// CI is green only when every check and status passed and one at least
// ran; pr-triage is left out, and a cut list is not green.
func TestCIDecidesFromEveryCheckAndStatus(t *testing.T) {
	ok := run("verify", "completed", "success")
	for name, c := range map[string]struct {
		runs, statuses, want string
	}{
		"green":                 {runs(ok, run("lint", "completed", "skipped")), statuses(), CIGreen},
		"green by a status":     {runs(), statuses(`{"context":"ci/x","state":"success"}`), CIGreen},
		"nothing ran":           {runs(), statuses(), CINone},
		"only pr-triage ran":    {runs(run("pr-triage", "completed", "success")), statuses(), CINone},
		"pr-triage in progress": {runs(ok, run("pr-triage", "in_progress", "")), statuses(), CIGreen},
		// Core's reusable workflow names its job "<caller job> / <job>".
		"only core's pr-triage ran":    {runs(run("pr-triage / pr-triage", "completed", "success")), statuses(), CINone},
		"core's pr-triage in progress": {runs(ok, run("pr-triage / pr-triage", "in_progress", "")), statuses(), CIGreen},
		"a job named like it":          {runs(run("pr-triage-extra", "completed", "success")), statuses(), CIGreen},
		// Only the triage job's exact names are left out: a near match that
		// fails, fails (Codex review of #23, round 2).
		"another job of pr-triage fails":  {runs(ok, run("pr-triage / integration", "completed", "failure")), statuses(), CIFailing},
		"PR-Triage in another case fails": {runs(ok, run("PR-Triage / integration", "completed", "failure")), statuses(), CIFailing},
		"PR-Triage alone":                 {runs(run("PR-Triage", "completed", "success")), statuses(), CIGreen},
		"a check in progress":             {runs(ok, run("race", "in_progress", "")), statuses(), CIPending},
		"a completed check, no end":       {runs(run("race", "completed", "")), statuses(), CIPending},
		"a failed check":                  {runs(ok, run("race", "completed", "failure")), statuses(), CIFailing},
		"a cancelled check":               {runs(ok, run("race", "completed", "cancelled")), statuses(), CIFailing},
		"a pending status":                {runs(ok), statuses(`{"context":"ci/x","state":"pending"}`), CIPending},
		"a failed status":                 {runs(ok), statuses(`{"context":"ci/x","state":"error"}`), CIFailing},
		"a cut list":                      {strings.Replace(runs(ok), `"total_count":1`, `"total_count":101`, 1), statuses(), CIPending},
	} {
		var calls []string
		gh := ghAnswers(map[string]string{"/check-runs": c.runs, "/status": c.statuses}, &calls)
		got, err := CI(context.Background(), gh, "frostyard/hotedge", at)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, c.want)
		}
	}
	// CIChecks names what failed, sorted and once each, and nothing for a
	// state that is not failing; an ignored job never counts (ADR-0024).
	for name, c := range map[string]struct {
		runs, statuses string
		state          string
		want           []string
	}{
		"two failed": {runs(ok, run("test", "completed", "failure"), run("lint", "completed", "cancelled"), run("test", "completed", "timed_out")),
			statuses(`{"context":"ci/x","state":"error"}`), CIFailing, []string{"ci/x", "lint", "test"}},
		"one pending": {runs(ok, run("race", "in_progress", "")), statuses(), CIPending, nil},
		"green":       {runs(ok), statuses(), CIGreen, nil},
	} {
		var calls []string
		gh := ghAnswers(map[string]string{"/check-runs": c.runs, "/status": c.statuses}, &calls)
		state, failing, err := CIChecks(context.Background(), gh, "frostyard/hotedge", at)
		if err != nil || state != c.state || !slices.Equal(failing, c.want) {
			t.Errorf("%s: %q %q, %v; want %q %q", name, state, failing, err, c.state, c.want)
		}
	}
	// gh keeps only the fields ciState reads.
	var projected []string
	if _, err := CI(context.Background(), ghAnswers(map[string]string{"/check-runs": runs(ok), "/status": statuses()}, &projected), "frostyard/hotedge", at); err != nil {
		t.Fatal(err)
	}
	for _, call := range projected {
		if !strings.Contains(call, " --jq ") {
			t.Errorf("gh %q prints the whole answer", call)
		}
	}
	var calls []string
	for name, answers := range map[string]map[string]string{
		"not json":       {"/check-runs": "Not Found", "/status": statuses()},
		"no runs":        {"/check-runs": `{"total_count":0}`, "/status": statuses()},
		"no statuses":    {"/check-runs": runs(), "/status": `{"state":"success"}`},
		"a failed read":  {"/check-runs": "error:HTTP 502", "/status": statuses()},
		"statuses fails": {"/check-runs": runs(ok)},
		// Totals smaller than the lists say nothing GitHub checked (Codex
		// review of #23, round 4).
		"a total under the runs":     {"/check-runs": strings.Replace(runs(ok), `"total_count":1`, `"total_count":0`, 1), "/status": statuses()},
		"a negative total":           {"/check-runs": strings.Replace(runs(ok), `"total_count":1`, `"total_count":-1`, 1), "/status": statuses()},
		"a total under the statuses": {"/check-runs": runs(), "/status": strings.Replace(statuses(`{"context":"ci/x","state":"success"}`), `"total_count":1`, `"total_count":0`, 1)},
		// A run or a status pocketd cannot tell is no answer, never green.
		"a nameless run":       {"/check-runs": runs(run("", "completed", "success")), "/status": statuses()},
		"a run with no status": {"/check-runs": runs(`{"name":"verify","conclusion":"success"}`), "/status": statuses()},
		"a status, no context": {"/check-runs": runs(), "/status": statuses(`{"state":"success"}`)},
	} {
		if got, err := CI(context.Background(), ghAnswers(answers, &calls), "frostyard/hotedge", at); err == nil {
			t.Errorf("%s: %q, want an error", name, got)
		}
	}
	if _, err := CI(context.Background(), ghAnswers(nil, &calls), "frostyard/hotedge", "HEAD"); err == nil {
		t.Error("a head that is no commit is read")
	}
}

// The label goes on with a POST that GitHub's answer confirms, and comes
// off with a DELETE; one already off is no error.
func TestSetLabel(t *testing.T) {
	var calls []string
	gh := ghAnswers(map[string]string{"--method POST": `[{"name":"Ready-For-Human"}]`, "--method DELETE": "error:gh: Label does not exist (HTTP 404)"}, &calls)
	if err := SetLabel(context.Background(), gh, "frostyard/hotedge", 1, "ready-for-human", true); err != nil {
		t.Fatal(err)
	}
	if err := SetLabel(context.Background(), gh, "frostyard/hotedge", 1, "ready-for-human", false); err != nil {
		t.Fatalf("removing a label that is not there: %v", err)
	}
	want := []string{
		"api --hostname github.com --method POST repos/frostyard/hotedge/issues/1/labels -f labels[]=ready-for-human",
		"api --hostname github.com --method DELETE repos/frostyard/hotedge/issues/1/labels/ready-for-human",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("gh calls\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	calls = nil
	bad := ghAnswers(map[string]string{"--method POST": `[{"name":"bug"}]`, "--method DELETE": "error:HTTP 403"}, &calls)
	if err := SetLabel(context.Background(), bad, "frostyard/hotedge", 1, "ready-for-human", true); err == nil {
		t.Error("an answer without the label counts as added")
	}
	if err := SetLabel(context.Background(), bad, "frostyard/hotedge", 1, "ready-for-human", false); err == nil {
		t.Error("a refused removal counts as done")
	}
	// A 404 for the repository, as after its access went, is no removal.
	gone := ghAnswers(map[string]string{"--method DELETE": "error:gh: Not Found (HTTP 404)"}, &calls)
	if err := SetLabel(context.Background(), gone, "frostyard/hotedge", 1, "ready-for-human", false); err == nil {
		t.Error("a repository that answers Not Found counts as the label removed")
	}
	if err := SetLabel(context.Background(), bad, "evil", 1, "ready-for-human", true); err == nil {
		t.Error("a repository that is no owner/name is labelled")
	}
}

// CI reads every page of check runs: a hundred and one checks, all
// green, are green, and one failure on the second page fails (Codex
// review of #36).
func TestCIReadsEveryPage(t *testing.T) {
	ok := run("verify", "completed", "success")
	for name, c := range map[string]struct {
		last, want string
	}{
		"all green":             {ok, CIGreen},
		"a failure on page two": {run("race", "completed", "failure"), CIFailing},
	} {
		first := make([]string, 100)
		for i := range first {
			first[i] = ok
		}
		gh := func(_ context.Context, args ...string) (string, error) {
			call := strings.Join(args, " ")
			switch {
			case strings.Contains(call, "/check-runs") && strings.Contains(call, "page=1 "):
				return `{"total_count":101,"check_runs":[` + strings.Join(first, ",") + `]}`, nil
			case strings.Contains(call, "/check-runs") && strings.Contains(call, "page=2 "):
				return `{"total_count":101,"check_runs":[` + c.last + `]}`, nil
			case strings.Contains(call, "/status"):
				return statuses(), nil
			}
			return "", errors.New("gh: Not Found (HTTP 404)")
		}
		if got, err := CI(context.Background(), gh, "frostyard/hotedge", at); err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, c.want)
		}
	}
}

// A total that changes between pages, as a check that starts as they are
// read, leaves CI pending, never green (Codex review of #36).
func TestCIWhosePagesMovedIsPending(t *testing.T) {
	ok := run("verify", "completed", "success")
	page := make([]string, 100)
	for i := range page {
		page[i] = ok
	}
	// Page one says 200; a check starts, and page two says 201: the two
	// pages hold 200 checks, as the first total said, but not the new one.
	gh := func(_ context.Context, args ...string) (string, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "/check-runs") && strings.Contains(call, "page=1 "):
			return `{"total_count":200,"check_runs":[` + strings.Join(page, ",") + `]}`, nil
		case strings.Contains(call, "/check-runs") && strings.Contains(call, "page=2 "):
			return `{"total_count":201,"check_runs":[` + strings.Join(page, ",") + `]}`, nil
		case strings.Contains(call, "/status"):
			return statuses(), nil
		}
		return "", errors.New("gh: Not Found (HTTP 404)")
	}
	if got, err := CI(context.Background(), gh, "frostyard/hotedge", at); err != nil || got != CIPending {
		t.Fatalf("pages that moved: %q, %v", got, err)
	}
}
