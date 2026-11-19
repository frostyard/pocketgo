package github

import (
	"context"
	"errors"
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
		"green":                     {runs(ok, run("lint", "completed", "skipped")), statuses(), CIGreen},
		"green by a status":         {runs(), statuses(`{"context":"ci/x","state":"success"}`), CIGreen},
		"nothing ran":               {runs(), statuses(), CINone},
		"only pr-triage ran":        {runs(run("pr-triage", "completed", "success")), statuses(), CINone},
		"pr-triage in progress":     {runs(ok, run("pr-triage", "in_progress", "")), statuses(), CIGreen},
		"a check in progress":       {runs(ok, run("race", "in_progress", "")), statuses(), CIPending},
		"a completed check, no end": {runs(run("race", "completed", "")), statuses(), CIPending},
		"a failed check":            {runs(ok, run("race", "completed", "failure")), statuses(), CIFailing},
		"a cancelled check":         {runs(ok, run("race", "completed", "cancelled")), statuses(), CIFailing},
		"a pending status":          {runs(ok), statuses(`{"context":"ci/x","state":"pending"}`), CIPending},
		"a failed status":           {runs(ok), statuses(`{"context":"ci/x","state":"error"}`), CIFailing},
		"a cut list":                {strings.Replace(runs(ok), `"total_count":1`, `"total_count":101`, 1), statuses(), CIPending},
	} {
		var calls []string
		gh := ghAnswers(map[string]string{"/check-runs": c.runs, "/status": c.statuses}, &calls)
		got, err := CI(context.Background(), gh, "frostyard/hotedge", at)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, c.want)
		}
	}
	var calls []string
	for name, answers := range map[string]map[string]string{
		"not json":       {"/check-runs": "Not Found", "/status": statuses()},
		"no runs":        {"/check-runs": `{"total_count":0}`, "/status": statuses()},
		"no statuses":    {"/check-runs": runs(), "/status": `{"state":"success"}`},
		"a failed read":  {"/check-runs": "error:HTTP 502", "/status": statuses()},
		"statuses fails": {"/check-runs": runs(ok)},
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
	gh := ghAnswers(map[string]string{"--method POST": `[{"name":"ready-for-human"}]`, "--method DELETE": "error:gh: Not Found (HTTP 404)"}, &calls)
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
	if err := SetLabel(context.Background(), bad, "evil", 1, "ready-for-human", true); err == nil {
		t.Error("a repository that is no owner/name is labelled")
	}
}
