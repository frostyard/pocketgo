package state

import (
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// A session on a repository (ADR-0027, protocol rule 6a): New Session
// offers the repositories of the profile chosen, or with no profile every
// repository a profile names; choosing one fills in its folder, and a
// session on one may start in a new worktree, whose folder and branch the
// form shows before it sends, as pocketd names them from the request.

// NoRepository is the label of the Repository choice of none.
const NoRepository = "None"

// RepositoryChoice is a repository as New Session offers it: owner/name,
// its checkout's folder on the service host, and its label. The first
// choice, None, has an empty Name.
type RepositoryChoice struct {
	Label  string
	Name   string
	Folder string
}

// RepositoryChoices are None, then the repositories a session from
// profile can work on, by name: the profile's own; with no profile (nil),
// every repository a profile of profiles names, the same name in any case
// counting as one, each folder once, named as the first profile listed
// names it, and labeled "name · folder" when the profiles name it in more
// than one folder. A standing profile's session has its own folder, and a
// private profile works only in its home (rules 7b and 7c): theirs offer
// None alone.
func RepositoryChoices(profile *proto.Profile, profiles []proto.Profile) []RepositoryChoice {
	out := []RepositoryChoice{{Label: NoRepository}}
	if profile != nil {
		if profile.Standing || profile.Private {
			return out
		}
		for _, r := range profile.Repositories {
			out = append(out, RepositoryChoice{Label: r.Name, Name: r.Name, Folder: r.Folder})
		}
		return out
	}
	type entry struct {
		name    string
		folders []string
	}
	var order []string
	byName := map[string]*entry{}
	for _, p := range profiles {
		for _, r := range p.Repositories {
			key := strings.ToLower(r.Name)
			e := byName[key]
			if e == nil {
				e = &entry{name: r.Name}
				byName[key] = e
				order = append(order, key)
			}
			if !slices.Contains(e.folders, r.Folder) {
				e.folders = append(e.folders, r.Folder)
			}
		}
	}
	for _, key := range order {
		e := byName[key]
		for _, folder := range e.folders {
			label := e.name
			if len(e.folders) > 1 {
				label += " · " + folder
			}
			out = append(out, RepositoryChoice{Label: label, Name: e.name, Folder: folder})
		}
	}
	return out
}

// RepositoryChoicesFor are the RepositoryChoices of choice, the profile
// chosen: nil (the profiles not loaded yet) or No profile offer every
// profile's, and a profile pocketd no longer offers (Gone) offers None
// alone, as nothing starts from it.
func RepositoryChoicesFor(choice *ProfileChoice, profiles []proto.Profile) []RepositoryChoice {
	switch {
	case choice == nil || choice.ID == "":
		return RepositoryChoices(nil, profiles)
	case choice.Gone || choice.Profile == nil:
		return []RepositoryChoice{{Label: NoRepository}}
	}
	return RepositoryChoices(choice.Profile, profiles)
}

// SessionFolder is New Session's working directory and repository: what
// the folder's input, the Repository select and the worktree switch hold,
// and what the form filled in itself, so it replaces only what the owner
// did not type. NewSessionFolder makes one.
type SessionFolder struct {
	// Cwd is the folder as its input holds it.
	Cwd string
	// Repository is the Repository select's label, NoRepository for none.
	Repository string
	// Worktree is the "In a new worktree" switch, on only with a
	// repository chosen.
	Worktree bool

	// original is the first suggestion, the last session's folder; fill
	// the folder the form fills in for the profile chosen: its start, else
	// original. typed is the folder the owner typed, "" for none: it stays
	// whatever the profile. shown is the folder the form last put in Cwd,
	// so a Cwd that differs is the owner's.
	original, fill, typed, shown string
	// profileID and profileStart are the profile the form last applied
	// and its folder; repo is the repository the folder was filled from,
	// zero for none.
	profileID, profileStart string
	repo                    RepositoryChoice
}

// NewSessionFolder is a form's folder that starts as cwd, its first
// suggestion, with no profile and no repository.
func NewSessionFolder(cwd string) SessionFolder {
	return SessionFolder{Cwd: cwd, Repository: NoRepository, original: cwd, fill: cwd, shown: cwd}
}

// Chosen is the repository chosen, zero for none.
func (f *SessionFolder) Chosen() RepositoryChoice { return f.repo }

// show puts cwd in the folder's input, as the form's own.
func (f *SessionFolder) show(cwd string) { f.Cwd, f.shown = cwd, cwd }

// Sync brings the folder in step with the profile chosen (its id, "" for
// none, and start, the folder a session from it starts in) and with
// choices, the repositories offered for it now, after the inputs took
// the owner's changes:
//   - A folder the owner edited is theirs, and stays whatever the profile;
//     edited while a repository is chosen, it ends the choice, and the
//     switch with it. An emptied one is none.
//   - Another profile, or its folder moved, fills in its folder (else the
//     first suggestion), unless the owner typed one or a repository's
//     folder shows.
//   - A repository chosen fills in its folder, and follows it when it
//     moves. None, or a choice no longer offered, brings back the folder
//     the owner typed, else the one the form fills in for the profile
//     chosen now (Codex review of #46, round 2), and turns the switch off.
func (f *SessionFolder) Sync(profileID, start string, choices []RepositoryChoice) {
	if f.Cwd != f.shown {
		f.typed, f.shown = f.Cwd, f.Cwd
		if f.repo.Name != "" {
			f.repo, f.Repository, f.Worktree = RepositoryChoice{}, NoRepository, false
		}
	}
	if profileID != f.profileID || start != f.profileStart {
		f.profileID, f.profileStart = profileID, start
		if f.fill = start; f.fill == "" {
			f.fill = f.original
		}
		if f.repo.Name == "" && f.typed == "" {
			f.show(f.fill)
		}
	}
	var chosen RepositoryChoice
	for _, ch := range choices {
		if ch.Name != "" && ch.Label == f.Repository {
			chosen = ch
		}
	}
	if chosen.Name == "" && f.Repository != NoRepository && f.repo.Name != "" {
		// Its label changed, as when a second folder of its name came:
		// the same repository and folder stay chosen.
		for _, ch := range choices {
			if ch.Name == f.repo.Name && ch.Folder == f.repo.Folder {
				chosen = ch
			}
		}
	}
	switch {
	case chosen.Name == "":
		if f.repo.Name != "" {
			if f.typed != "" {
				f.show(f.typed)
			} else {
				f.show(f.fill)
			}
		}
		f.repo, f.Repository, f.Worktree = RepositoryChoice{}, NoRepository, false
	case chosen != f.repo:
		f.show(chosen.Folder)
		f.repo, f.Repository = chosen, chosen.Label
	default:
		f.Repository = chosen.Label
	}
}

// WorktreePreview is the folder and branch of the worktree request makes
// (rule 6a), as pocketd names it from the request alone: for its profile,
// or for the owner with none, beside its checkout (Cwd), from its title
// and request id. ok is false for a request that makes none.
func WorktreePreview(request proto.CreateSession) (path, branch string, ok bool) {
	if !request.Worktree || request.Repository == "" || request.Cwd == "" || request.RequestID == "" {
		return "", "", false
	}
	agent := request.Profile
	if agent == "" {
		agent = proto.OwnerAgent
	}
	path, branch = proto.SessionWorktree(request.Cwd, agent, request.Title, request.RequestID)
	return path, branch, true
}

// WorktreeLine is the line under a worktree's folder in New Session.
func WorktreeLine(branch string) string {
	return "(a new worktree on " + branch + ")"
}
