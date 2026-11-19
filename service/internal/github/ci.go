package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// CI states at a commit (ADR-0017).
const (
	// CIGreen: every check run completed with success, neutral or skipped,
	// every commit status is success, and one at least exists.
	CIGreen = "green"
	// CIPending: one is not completed yet, or GitHub cut the list short.
	CIPending = "pending"
	// CIFailing: one completed otherwise, or a status failed.
	CIFailing = "failing"
	// CINone: nothing ran at the commit.
	CINone = "none"
)

// IgnoredChecks are the check runs CI leaves out: core's pr-triage job runs
// on the label's own labeled event, as core's readiness step leaves it out
// (core ADR-0077). A reusable workflow's job is named "<caller job> /
// <job>", as "pr-triage / pr-triage": a name that is one of these, or
// starts with one and " / ", is left out.
var IgnoredChecks = []string{"pr-triage"}

// checkRuns is the part of GitHub's check runs that pocketd reads.
type checkRuns struct {
	TotalCount *int `json:"total_count"`
	CheckRuns  *[]struct {
		Name       string  `json:"name"`
		Status     string  `json:"status"`
		Conclusion *string `json:"conclusion"`
	} `json:"check_runs"`
}

// combined is the part of GitHub's combined status that pocketd reads.
type combined struct {
	TotalCount *int `json:"total_count"`
	Statuses   *[]struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

// CI reads CI's state at commit sha of repository: its check runs and its
// commit statuses (CIGreen and the others). An answer it cannot read is an
// error, never green.
func CI(ctx context.Context, gh GH, repository, sha string) (string, error) {
	if err := CheckRepository(repository); err != nil {
		return "", err
	}
	if !commitSHA.MatchString(sha) {
		return "", fmt.Errorf("%q is not a commit", sha)
	}
	out, err := gh(ctx, "api", "--hostname", "github.com", "repos/"+repository+"/commits/"+sha+"/check-runs?per_page=100")
	if err != nil {
		return "", fmt.Errorf("read the checks of %s at %s on GitHub: %w", repository, sha[:12], err)
	}
	var runs checkRuns
	if err := json.Unmarshal([]byte(out), &runs); err != nil || runs.TotalCount == nil || runs.CheckRuns == nil {
		return "", fmt.Errorf("GitHub's checks of %s at %s are not a list of check runs", repository, sha[:12])
	}
	out, err = gh(ctx, "api", "--hostname", "github.com", "repos/"+repository+"/commits/"+sha+"/status?per_page=100")
	if err != nil {
		return "", fmt.Errorf("read the statuses of %s at %s on GitHub: %w", repository, sha[:12], err)
	}
	var status combined
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.TotalCount == nil || status.Statuses == nil {
		return "", fmt.Errorf("GitHub's statuses of %s at %s are not a combined status", repository, sha[:12])
	}
	return ciState(runs, status)
}

// ciState decides CI's state from what GitHub said (ADR-0017). A check run
// with no name or status, or a status with no context or state, is no
// answer: pocketd cannot tell what it is, so it never counts as green.
func ciState(runs checkRuns, status combined) (string, error) {
	for _, r := range *runs.CheckRuns {
		if r.Name == "" || r.Status == "" {
			return "", fmt.Errorf("GitHub's check run %q has no name or no status", r.Name)
		}
	}
	for _, st := range *status.Statuses {
		if st.Context == "" || st.State == "" {
			return "", fmt.Errorf("GitHub's commit status %q has no context or no state", st.Context)
		}
	}
	// A list GitHub cut short may hide a failure.
	if *runs.TotalCount > len(*runs.CheckRuns) || *status.TotalCount > len(*status.Statuses) {
		return CIPending, nil
	}
	pending, failing, counted := false, false, 0
	for _, r := range *runs.CheckRuns {
		if ignored(r.Name) {
			continue
		}
		counted++
		switch {
		case r.Status != "completed":
			pending = true
		case r.Conclusion == nil:
			pending = true
		default:
			switch *r.Conclusion {
			case "success", "neutral", "skipped":
			default:
				failing = true
			}
		}
	}
	for _, st := range *status.Statuses {
		counted++
		switch st.State {
		case "success":
		case "pending":
			pending = true
		default:
			failing = true
		}
	}
	switch {
	case failing:
		return CIFailing, nil
	case pending:
		return CIPending, nil
	case counted == 0:
		return CINone, nil
	}
	return CIGreen, nil
}

func ignored(name string) bool {
	for _, n := range IgnoredChecks {
		if strings.EqualFold(name, n) || len(name) > len(n)+3 && strings.EqualFold(name[:len(n)+3], n+" / ") {
			return true
		}
	}
	return false
}

// SetLabel puts label on issue or pull request number of repository, or
// takes it off, as the signed-in account (ADR-0017). Taking off a label it
// does not have changes nothing.
func SetLabel(ctx context.Context, gh GH, repository string, number int64, label string, on bool) error {
	if err := CheckRepository(repository); err != nil {
		return err
	}
	if number <= 0 || label == "" {
		return fmt.Errorf("label %q on #%d: nothing to label", label, number)
	}
	issue := "repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/labels"
	if on {
		out, err := gh(ctx, "api", "--hostname", "github.com", "--method", "POST", issue, "-f", "labels[]="+label)
		if err != nil {
			return fmt.Errorf("add %s to %s#%d on GitHub: %w", label, repository, number, err)
		}
		var labels []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(out), &labels); err != nil {
			return fmt.Errorf("GitHub's answer to adding %s to %s#%d is not a list of labels", label, repository, number)
		}
		for _, l := range labels {
			if l.Name == label {
				return nil
			}
		}
		return fmt.Errorf("GitHub's labels of %s#%d lack %s after adding it", repository, number, label)
	}
	_, err := gh(ctx, "api", "--hostname", "github.com", "--method", "DELETE", issue+"/"+url.PathEscape(label))
	if err != nil && !strings.Contains(err.Error(), "HTTP 404") {
		return fmt.Errorf("remove %s from %s#%d on GitHub: %w", label, repository, number, err)
	}
	return nil
}
