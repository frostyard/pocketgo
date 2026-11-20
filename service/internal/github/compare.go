package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MergeBase is the commit where head leaves the branch base of repository,
// as GitHub's compare reads it with the service user's sign-in: the base of
// a cross-review's change set (ADR-0019). No git fetch runs, so a private
// repository needs no git credential. The answer must be a full commit id.
func MergeBase(ctx context.Context, gh GH, repository, base, head string) (string, error) {
	if err := CheckRepository(repository); err != nil {
		return "", err
	}
	if !commitSHA.MatchString(head) {
		return "", fmt.Errorf("%q is not a commit", head)
	}
	if base == "" || strings.ContainsAny(base, " \t\n~^:?*[\\") || strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("%q is not a branch name", base)
	}
	out, err := gh(ctx, "api", "--hostname", "github.com", "repos/"+repository+"/compare/"+url.PathEscape(base)+"..."+head,
		"--jq", `{merge_base_commit: {sha: .merge_base_commit.sha}}`)
	if err != nil {
		return "", fmt.Errorf("compare %s with %s on GitHub: %w", head[:12], base, err)
	}
	var answer struct {
		MergeBaseCommit *struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil || answer.MergeBaseCommit == nil || !commitSHA.MatchString(answer.MergeBaseCommit.SHA) {
		return "", fmt.Errorf("GitHub's compare of %s with %s names no merge base", head[:12], base)
	}
	return answer.MergeBaseCommit.SHA, nil
}
