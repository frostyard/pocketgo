package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
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

// ciPages bounds the pages of check runs, and of statuses, CI reads: a
// list longer than that is cut, and never green.
const ciPages = 10

// MaxCIRequests is the most requests one CIChecks makes: every page of
// check runs and of statuses.
const MaxCIRequests = 2 * ciPages

// IgnoredChecks are the check runs CI leaves out, by their exact names:
// core's pr-triage job runs on the label's own labeled event, as core's
// readiness step leaves it out (core ADR-0077). Called as a reusable
// workflow, its check run is named "<caller job> / <job>": "pr-triage /
// pr-triage". Any other job counts, whatever its name looks like.
var IgnoredChecks = []string{"pr-triage", "pr-triage / pr-triage"}

// checkRuns is the part of GitHub's check runs that pocketd reads.
type checkRuns struct {
	TotalCount *int `json:"total_count"`
	CheckRuns  *[]struct {
		ID         int64   `json:"id"`
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
	state, _, err := CIChecks(ctx, gh, repository, sha)
	return state, err
}

// CIChecks is CI with the names of the check runs and statuses that
// failed, sorted: a shift tells the same failure from another by them
// (ADR-0024).
func CIChecks(ctx context.Context, gh GH, repository, sha string) (state string, failing []string, err error) {
	if err := CheckRepository(repository); err != nil {
		return "", nil, err
	}
	if !commitSHA.MatchString(sha) {
		return "", nil, fmt.Errorf("%q is not a commit", sha)
	}
	// Every page, up to ciPages: a list GitHub has more of than one page
	// is read whole, so CI over a hundred checks decides (Codex review of
	// #36). gh keeps only what ciState reads, so a page of runs with long
	// output stays far under the runner's limit on what gh prints.
	// A total that changes between pages says checks came or went as the
	// pages were read: the reading stops there, the list stays short of
	// its first total, and is never green. A check run, or a status's
	// context, a page repeats says one moved between pages, and another
	// may be missed: the read is cut too (Codex review of #36).
	cut := false
	runIDs, contexts := map[int64]bool{}, map[string]bool{}
	var runs checkRuns
	for page := 1; page <= ciPages; page++ {
		out, err := gh(ctx, "api", "--hostname", "github.com", "repos/"+repository+"/commits/"+sha+"/check-runs?per_page=100&page="+strconv.Itoa(page),
			"--jq", `{total_count, check_runs: (.check_runs | if type == "array" then map({id, name, status, conclusion}) else . end)}`)
		if err != nil {
			return "", nil, fmt.Errorf("read the checks of %s at %s on GitHub: %w", repository, sha[:12], err)
		}
		var got checkRuns
		if err := json.Unmarshal([]byte(out), &got); err != nil || got.TotalCount == nil || got.CheckRuns == nil {
			return "", nil, fmt.Errorf("GitHub's checks of %s at %s are not a list of check runs", repository, sha[:12])
		}
		for _, r := range *got.CheckRuns {
			if r.ID != 0 && runIDs[r.ID] {
				cut = true
			}
			runIDs[r.ID] = true
		}
		if cut {
			break
		}
		if runs.TotalCount == nil {
			runs = got
		} else {
			if *got.TotalCount != *runs.TotalCount {
				break
			}
			all := append(*runs.CheckRuns, *got.CheckRuns...)
			runs.CheckRuns = &all
		}
		if len(*got.CheckRuns) < 100 || len(*runs.CheckRuns) >= *runs.TotalCount {
			break
		}
	}
	// Pages tell apart only check runs with an id: a list of more than one
	// page with a run that has none may repeat it unseen, and is cut
	// (Codex review of #36).
	if !cut && runs.CheckRuns != nil && len(*runs.CheckRuns) > 100 {
		for _, r := range *runs.CheckRuns {
			if r.ID <= 0 {
				cut = true
				break
			}
		}
	}
	var status combined
	for page := 1; page <= ciPages; page++ {
		out, err := gh(ctx, "api", "--hostname", "github.com", "repos/"+repository+"/commits/"+sha+"/status?per_page=100&page="+strconv.Itoa(page),
			"--jq", `{total_count, statuses: (.statuses | if type == "array" then map({context, state}) else . end)}`)
		if err != nil {
			return "", nil, fmt.Errorf("read the statuses of %s at %s on GitHub: %w", repository, sha[:12], err)
		}
		var got combined
		if err := json.Unmarshal([]byte(out), &got); err != nil || got.TotalCount == nil || got.Statuses == nil {
			return "", nil, fmt.Errorf("GitHub's statuses of %s at %s are not a combined status", repository, sha[:12])
		}
		for _, st := range *got.Statuses {
			if contexts[st.Context] {
				cut = true
			}
			contexts[st.Context] = true
		}
		if cut {
			break
		}
		if status.TotalCount == nil {
			status = got
		} else {
			if *got.TotalCount != *status.TotalCount {
				break
			}
			all := append(*status.Statuses, *got.Statuses...)
			status.Statuses = &all
		}
		if len(*got.Statuses) < 100 || len(*status.Statuses) >= *status.TotalCount {
			break
		}
	}
	if cut || runs.TotalCount == nil || status.TotalCount == nil {
		return CIPending, nil, nil
	}
	state, err = ciState(runs, status)
	if err != nil || state != CIFailing {
		return state, nil, err
	}
	return state, failingChecks(runs, status), nil
}

// failingChecks names the check runs and statuses that failed, sorted and
// each once.
func failingChecks(runs checkRuns, status combined) []string {
	var out []string
	for _, r := range *runs.CheckRuns {
		if ignored(r.Name) || r.Status != "completed" || r.Conclusion == nil {
			continue
		}
		switch *r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			out = append(out, r.Name)
		}
	}
	for _, st := range *status.Statuses {
		if st.State != "success" && st.State != "pending" {
			out = append(out, st.Context)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
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
	// A total smaller than the list, or negative, says nothing GitHub
	// checked; a list GitHub cut short may hide a failure.
	if *runs.TotalCount < len(*runs.CheckRuns) || *status.TotalCount < len(*status.Statuses) {
		return "", fmt.Errorf("GitHub's totals (%d check runs, %d statuses) are smaller than its lists", *runs.TotalCount, *status.TotalCount)
	}
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

func ignored(name string) bool { return slices.Contains(IgnoredChecks, name) }

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
			if strings.EqualFold(l.Name, label) {
				return nil
			}
		}
		return fmt.Errorf("GitHub's labels of %s#%d lack %s after adding it", repository, number, label)
	}
	_, err := gh(ctx, "api", "--hostname", "github.com", "--method", "DELETE", issue+"/"+url.PathEscape(label))
	// Only GitHub's own word that the label is not there makes a removal
	// done: a 404 for the repository, as after its access went, is not.
	if err != nil && !strings.Contains(err.Error(), "Label does not exist") {
		return fmt.Errorf("remove %s from %s#%d on GitHub: %w", label, repository, number, err)
	}
	return nil
}
