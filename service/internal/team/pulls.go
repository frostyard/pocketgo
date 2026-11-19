package team

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxPulls is the most pull requests one report names (ADR-0016).
const MaxPulls = 10

// PullRef is a pull request a report names: its number, and its
// repository when the thread has none.
type PullRef struct {
	Repository string `json:"repository,omitempty"`
	Number     int64  `json:"number"`
}

// PullRepository is the repository of ref in a thread of repository ("" for
// a thread in the specialist's home): a worktree thread's pull requests are
// in its repository, and a home thread's name theirs.
func PullRepository(ref PullRef, repository string) (string, error) {
	switch {
	case ref.Number <= 0:
		return "", fmt.Errorf("%d is not a pull request number", ref.Number)
	case repository == "" && ref.Repository == "":
		return "", fmt.Errorf("pull request #%d needs its repository: this thread works in no repository", ref.Number)
	case repository == "":
		return ref.Repository, nil
	case ref.Repository != "" && !strings.EqualFold(ref.Repository, repository):
		return "", fmt.Errorf("pull request %s#%d is not in this thread's repository %s", ref.Repository, ref.Number, repository)
	}
	return repository, nil
}

// PullFacts are what GitHub says of a pull request that the thread's rules
// look at.
type PullFacts struct {
	Repository     string
	Number         int64
	Head           string
	HeadRepository string
}

// CheckPull says whether a pull request, as GitHub has it, belongs to a
// thread of repository on branch ("" both for a home thread), whose
// specialist's profile names repositories: it is in one of them (the
// thread's own, for a worktree thread), and a worktree thread's comes from
// the thread's branch in that same repository, not from a fork.
func CheckPull(repository, branch string, repositories []string, p PullFacts) error {
	ref := p.Repository + "#" + strconv.FormatInt(p.Number, 10)
	if repository != "" {
		switch {
		case !strings.EqualFold(p.Repository, repository):
			return fmt.Errorf("%s is not in this thread's repository %s", ref, repository)
		case p.Head != branch:
			return fmt.Errorf("%s comes from the branch %q, not this thread's %s", ref, p.Head, branch)
		case !strings.EqualFold(p.HeadRepository, repository):
			return fmt.Errorf("%s comes from %q, not from %s itself", ref, p.HeadRepository, repository)
		}
		return nil
	}
	for _, r := range repositories {
		if strings.EqualFold(r, p.Repository) {
			return nil
		}
	}
	return fmt.Errorf("%s is in a repository this agent's profile does not name", ref)
}

// CheckPullRefs checks a report's pull requests before any is read: at most
// MaxPulls, each named once.
func CheckPullRefs(refs []PullRef, repository string) error {
	if len(refs) > MaxPulls {
		return fmt.Errorf("%d pull requests, more than %d", len(refs), MaxPulls)
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		repo, err := PullRepository(ref, repository)
		if err != nil {
			return err
		}
		key := strings.ToLower(repo) + "#" + strconv.FormatInt(ref.Number, 10)
		if seen[key] {
			return fmt.Errorf("pull request %s#%d is named twice", repo, ref.Number)
		}
		seen[key] = true
	}
	return nil
}

// MergedText is the report of a thread pocketd closed because its pull
// requests merged.
func MergedText(pulls []string) string {
	if len(pulls) == 0 {
		return "Closed: its pull requests merged."
	}
	return "Closed: its pull requests merged (" + strings.Join(pulls, ", ") + ")."
}
