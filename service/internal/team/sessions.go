package team

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A session the owner starts on a repository (ADR-0027, protocol rule 6a)
// works in the checkout a profile names for it, or in a new git worktree
// of that checkout, named as proto.SessionWorktree names it, as a thread's
// is beside the checkout (ADR-0014).

// SessionCheckout is the checkout a session the owner starts on
// repository works from. With profile, it is the folder the profile names
// for repository (CheckoutKey: GitHub's names ignore case). With none
// (nil), it is one of the folders every, the valid profiles, names for
// it (RepositoryFolders): the one cwd names, which may be empty only when
// they name one. A cwd that is not empty must be that folder, cleaned.
func SessionCheckout(profile *profiles.Profile, every []profiles.Profile, repository, cwd string) (string, error) {
	if cwd != "" {
		if !filepath.IsAbs(cwd) {
			return "", errors.New("cwd must be an absolute path")
		}
		cwd = filepath.Clean(cwd)
	}
	if profile != nil {
		key, ok := CheckoutKey(*profile, repository)
		if !ok {
			names := repositoryNames(*profile)
			if len(names) == 0 {
				return "", fmt.Errorf("profile %s works on no repository", profile.ID)
			}
			return "", fmt.Errorf("profile %s does not work on %s; its repositories are %s", profile.ID, repository, strings.Join(names, ", "))
		}
		folder := profile.Repositories[key]
		if cwd != "" && cwd != folder {
			return "", fmt.Errorf("profile %s's checkout of %s is %s, not %s", profile.ID, key, folder, cwd)
		}
		return folder, nil
	}
	folders := RepositoryFolders(every, repository)
	switch {
	case len(folders) == 0:
		return "", fmt.Errorf("no profile names the repository %s", repository)
	case cwd != "" && !slices.Contains(folders, cwd):
		return "", fmt.Errorf("no profile names %s in %s; the profiles name it in %s", repository, cwd, strings.Join(folders, ", "))
	case cwd != "":
		return cwd, nil
	case len(folders) > 1:
		return "", fmt.Errorf("the profiles name %s in %d folders, %s: name the cwd", repository, len(folders), strings.Join(folders, ", "))
	}
	return folders[0], nil
}

// RepositoryFolders are the folders every names for repository, its name
// in any case, as GitHub compares it: each once, sorted.
func RepositoryFolders(every []profiles.Profile, repository string) []string {
	var out []string
	for _, p := range every {
		if key, ok := CheckoutKey(p, repository); ok && !slices.Contains(out, p.Repositories[key]) {
			out = append(out, p.Repositories[key])
		}
	}
	slices.Sort(out)
	return out
}

// SessionWorktree is the new worktree a session on checkout works in, and
// its branch: proto.SessionWorktree, for profile's id, or for the owner
// (proto.OwnerAgent) with no profile, from the request's title and id.
func SessionWorktree(profile *profiles.Profile, checkout, title, requestID string) (path, branch string) {
	agent := proto.OwnerAgent
	if profile != nil {
		agent = profile.ID
	}
	return proto.SessionWorktree(checkout, agent, title, requestID)
}

// repositoryNames are the names of p's repositories, sorted.
func repositoryNames(p profiles.Profile) []string {
	names := make([]string, 0, len(p.Repositories))
	for name := range p.Repositories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
