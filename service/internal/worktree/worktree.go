// Package worktree makes the git worktree a thread works in (ADR-0014):
// next to the repository's checkout, on a new branch from its HEAD, or,
// for a cross-review, detached at the pull request's head (ADR-0019).
package worktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// Git runs git with args in dir and returns its output, trimmed. pocketd
// runs it in the sandbox (ADR-0012), so a hook or a filter the checkout
// configures runs with an agent's rights, never with pocketd's. An error
// that wraps a *GitExit is git's own answer; any other says git did not
// run or was stopped (the sandbox around it failed, say).
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// GitExit is git's own exit with a status other than 0: git ran and
// answered. Only a Git that knows git itself ran returns one.
type GitExit struct{ Status int }

func (e *GitExit) Error() string { return fmt.Sprintf("git exited with status %d", e.Status) }

// unsure marks a check that could not tell: a probe that failed, git that
// did not run, a deadline that passed. The worktree may be fine.
type unsure struct{ error }

func (u unsure) Unwrap() error { return u.error }

// Unsure says whether err is a check that could not tell, as opposed to a
// worktree or a checkout that is not as it must be: the first may pass
// when it is tried again, the second does not.
func Unsure(err error) bool {
	var u unsure
	return errors.As(err, &u)
}

// Kind is what path names, as a command in the sandbox sees it, without
// following a link at path itself: "absent", "link", "file", "dir" or
// "other". pocketd looks at no checkout path with its own rights.
type Kind func(ctx context.Context, path string) (string, error)

// Same says whether two paths name one entry, as a command in the sandbox
// resolves them (the shell's test -ef), bound by ctx.
type Same func(ctx context.Context, a, b string) (bool, error)

// Read is the first 4 KiB of the regular file at path as a command in the
// sandbox reads it; ok is false when path is no regular file there.
type Read func(ctx context.Context, path string) (text string, ok bool, err error)

// Tools are what Add, AddDetached and their checks run: git, a look at an
// entry, a short read and a path comparison, all in the sandbox and bound
// by ctx.
type Tools struct {
	Git  Git
	Kind Kind
	Read Read
	Same Same
	// Each runs git as Git does, and gives each record of its output,
	// split at sep, to each, with bounded memory: a listing of every
	// tracked file may be any length (Codex review of #26, round 5).
	Each Each
}

// Each runs git with args in dir, in the sandbox, and calls each with
// every record of its output split at sep; an error of each stops git and
// comes back.
type Each func(ctx context.Context, dir string, sep byte, each func(record string) error, args ...string) error

// Add makes a worktree of checkout at path on branch, from the checkout's
// HEAD. The path's and the branch's names end with the thread's random
// owner (team.Slug), so nothing else makes them: a path or a branch of the
// name is what an earlier try of the same thread made, and a retry finishes
// from it. Every worktree it hands over is finished: of the repository, on
// branch with a commit, registered by git at path, and not locked.
// checkout must be the top of its repository.
func Add(ctx context.Context, t Tools, checkout, path, branch string) error {
	git := t.Git
	if !filepath.IsAbs(checkout) || !filepath.IsAbs(path) {
		return errors.New("the checkout and the worktree must be absolute paths")
	}
	common, err := checkoutCommon(ctx, t, checkout)
	if err != nil {
		return err
	}
	kind, err := t.Kind(ctx, path)
	if err != nil {
		return unsure{fmt.Errorf("look at %s: %w", path, err)}
	}
	if kind == "absent" {
		// A lookup that could not tell makes nothing: only a branch git
		// says is not there is made.
		exists, err := hasBranch(ctx, git, checkout, branch)
		if err != nil {
			return explain(ctx, checkout, err, "")
		}
		if exists {
			_, err = git(ctx, checkout, "worktree", "add", path, branch)
		} else {
			_, err = git(ctx, checkout, "worktree", "add", "-b", branch, path, "HEAD")
		}
		if err != nil {
			return explain(ctx, path, no(err), "git could not make the worktree "+path)
		}
	}
	return finished(ctx, t, checkout, path, common, branch)
}

// ErrTaken is a worktree's path or branch that is there already, which the
// request that names it did not make: a session's new worktree never takes
// it (ADR-0027).
var ErrTaken = errors.New("is there already, and its request did not make it")

// Free checks, before a session's request first claims its worktree
// (ADR-0027), that checkout is the top of its repository and that neither
// path nor branch is there yet: one that is was made by something other
// than that request (another data folder, or a work database restored from
// before), and is ErrTaken. A check that could not tell is Unsure.
func Free(ctx context.Context, t Tools, checkout, path, branch string) error {
	if _, err := checkoutCommon(ctx, t, checkout); err != nil {
		return err
	}
	kind, err := t.Kind(ctx, path)
	if err != nil {
		return unsure{fmt.Errorf("look at %s: %w", path, err)}
	}
	if kind != "absent" {
		return fmt.Errorf("%s %w", path, ErrTaken)
	}
	exists, err := hasBranch(ctx, t.Git, checkout, branch)
	if err != nil {
		return unsure{err}
	}
	if exists {
		return fmt.Errorf("the branch %s %w", branch, ErrTaken)
	}
	return nil
}

// Usable checks the worktree at path of checkout as a specialist gets it
// now (finished), and the checkout as Add does, without making anything:
// pocketd checks it again when the specialist's session starts.
func Usable(ctx context.Context, t Tools, checkout, path, branch string) error {
	common, err := checkoutCommon(ctx, t, checkout)
	if err != nil {
		return err
	}
	return finished(ctx, t, checkout, path, common, branch)
}

// AddDetached makes a worktree of checkout at path, detached at the commit
// sha: the folder of a cross-review thread, at the pull request's head
// (ADR-0019). sha is a commit's full id, 40 lowercase hex digits, as
// GitHub names a head: a shorter or another name could resolve to another
// commit. When git says the checkout's repository does not hold the
// commit, AddDetached fetches ref (refs/pull/<number>/head) from fetchURL
// first, and checks that ref named sha and that the commit is there now: a
// ref that names another commit is a head that moved since pocketd read
// it, and the review would not be of sha. The path's name is the review
// thread's, so nothing else makes it: a path that exists is what an
// earlier try of the same thread made, and AddDetached finishes from it,
// fetching and making nothing, as Add does. Every worktree it hands over
// is finished: of the repository, registered by git at path, detached at
// exactly sha, and not locked. checkout must be the top of its repository.
func AddDetached(ctx context.Context, t Tools, checkout, path, sha, fetchURL, ref string) error {
	t = reviewTools(t)
	if !filepath.IsAbs(checkout) || !filepath.IsAbs(path) {
		return errors.New("the checkout and the worktree must be absolute paths")
	}
	if !isCommitID(sha) {
		return notCommitID(sha)
	}
	// Both go to git fetch as arguments: neither may read as an option.
	if !pullHead.MatchString(ref) {
		return fmt.Errorf("%q is not a pull request's head ref, refs/pull/<number>/head", ref)
	}
	if fetchURL == "" || strings.HasPrefix(fetchURL, "-") {
		return fmt.Errorf("%q is not a repository's URL to fetch from", fetchURL)
	}
	common, err := checkoutCommon(ctx, t, checkout)
	if err != nil {
		return err
	}
	kind, err := t.Kind(ctx, path)
	if err != nil {
		return unsure{fmt.Errorf("look at %s: %w", path, err)}
	}
	if kind == "absent" {
		// Checked before git writes a file: the checkout's own attributes
		// would choose a filter for it (Codex review of #26, round 7).
		if err := attributesIsolated(ctx, t, checkout); err != nil {
			return err
		}
		if err := fetchCommit(ctx, t, checkout, sha, fetchURL, ref); err != nil {
			return err
		}
		// And again right before git writes a file: a fetch may take long,
		// and attributes added meanwhile would choose a filter (Codex
		// review of #26, round 12). What changes between this check and
		// the add, a moment, the check after it finds when it stays.
		if err := attributesIsolated(ctx, t, checkout); err != nil {
			return err
		}
		if _, err := t.Git(ctx, checkout, "worktree", "add", "--detach", path, sha); err != nil {
			return explain(ctx, path, no(err), "git could not make the worktree "+path)
		}
	}
	return finishedDetached(ctx, t, checkout, path, common, sha)
}

// UsableDetached checks the worktree at path of checkout as a reviewer
// gets it now (finishedDetached), and the checkout as AddDetached does,
// without fetching or making anything: pocketd checks it again when the
// review's session starts, as Usable is for Add.
func UsableDetached(ctx context.Context, t Tools, checkout, path, sha string) error {
	t = reviewTools(t)
	if !isCommitID(sha) {
		return notCommitID(sha)
	}
	common, err := checkoutCommon(ctx, t, checkout)
	if err != nil {
		return err
	}
	return finishedDetached(ctx, t, checkout, path, common, sha)
}

// pullHead is the ref GitHub keeps a pull request's head at, which a
// cross-review fetches (ADR-0019).
var pullHead = regexp.MustCompile(`^refs/pull/[1-9][0-9]*/head$`)

// isCommitID says whether s is a commit's full id as git prints it and
// GitHub names a head: 40 lowercase hex digits. Only that is compared
// whole with the id git reads at a worktree's HEAD.
func isCommitID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func notCommitID(sha string) error {
	return fmt.Errorf("%q is not a commit's full id: 40 lowercase hex digits", sha)
}

// fetchCommit makes sure the repository of checkout holds the commit sha.
// It fetches ref from fetchURL only when git says the commit is not there:
// a lookup that could not tell fetches nothing. After the fetch, ref must
// have named sha (fetchedHead) and the commit must be there. git's exit
// does not tell a ref that is not there from a network that failed, so a
// fetch that failed may pass when it is tried again (Unsure); a ref that
// named another commit does not.
func fetchCommit(ctx context.Context, t Tools, checkout, sha, fetchURL, ref string) error {
	there, err := hasCommit(ctx, t.Git, checkout, sha)
	if err != nil {
		return explain(ctx, checkout, err, "")
	}
	if there {
		return nil
	}
	// FETCH_HEAD is written whatever the checkout's configuration says:
	// what the fetch got is read back from it.
	if _, err := t.Git(ctx, checkout, "fetch", "--no-tags", "--write-fetch-head", "--", fetchURL, ref); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return unsure{fmt.Errorf("the fetch of %s from %s did not finish: %w", ref, fetchURL, cerr)}
		}
		return unsure{fmt.Errorf("git could not fetch %s from %s: %w", ref, fetchURL, err)}
	}
	fetched, err := fetchedHead(ctx, t, checkout, ref)
	if err != nil {
		return explain(ctx, checkout, err, "")
	}
	if fetched != sha {
		return fmt.Errorf("%s of %s names the commit %s, not %s: the pull request's head moved, so a review there would not be of %s", ref, fetchURL, fetched, sha, sha)
	}
	there, err = hasCommit(ctx, t.Git, checkout, sha)
	if err != nil {
		return explain(ctx, checkout, err, "")
	}
	if !there {
		return fmt.Errorf("%s of %s names %s, which is not a commit the repository of %s holds", ref, fetchURL, sha, checkout)
	}
	return nil
}

// hasCommit says whether the repository of checkout holds the commit sha.
// git cat-file -e answers no with an exit of its own; git that did not run
// is a lookup that could not tell, never a missing commit.
func hasCommit(ctx context.Context, git Git, checkout, sha string) (bool, error) {
	_, err := git(ctx, checkout, "cat-file", "-e", sha+"^{commit}")
	if err == nil {
		return true, nil
	}
	var exit *GitExit
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, fmt.Errorf("look up the commit %s: %w", sha, err)
}

// fetchedHead is the commit ref named in the fetch git just ran in
// checkout, read back from the checkout's FETCH_HEAD, read in the sandbox.
// git writes it a line "<id>\t<merge mark>\t'<ref>' of <url>" for each ref
// it fetched (a ref outside refs/heads, refs/tags and refs/remotes is
// quoted whole). The file is the checkout's own, so another fetch there
// may have written it since: only a file of that one line counts, and any
// other is a read that could not tell, never a head that moved.
func fetchedHead(ctx context.Context, t Tools, checkout, ref string) (string, error) {
	// FETCH_HEAD is in the git folder of the worktree the fetch ran in.
	dir, err := t.Git(ctx, checkout, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("git cannot name its git folder: %w", err)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("git named its git folder %s, not an absolute path", dir)
	}
	name := filepath.Join(dir, "FETCH_HEAD")
	text, ok, err := t.Read(ctx, name)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	if !ok {
		return "", fmt.Errorf("%s is not a file", name)
	}
	// One line, ended: a file cut at the read's limit ends in none.
	line, ended := strings.CutSuffix(text, "\n")
	id, rest, _ := strings.Cut(line, "\t")
	mark, what, _ := strings.Cut(rest, "\t")
	if !ended || strings.Contains(line, "\n") || !isCommitID(id) ||
		(mark != "" && mark != "not-for-merge") || !strings.HasPrefix(what, "'"+ref+"' of ") {
		return "", fmt.Errorf("%s does not hold the one line of the fetch of %s: another fetch in %s may have written it since", name, ref, checkout)
	}
	return id, nil
}

// checkoutCommon checks that checkout is the top of its repository, as a
// command in the sandbox sees it, and returns the repository's common git
// folder: a folder inside a checkout shares its common folder, so that
// alone does not tell.
func checkoutCommon(ctx context.Context, t Tools, checkout string) (string, error) {
	top, err := t.Git(ctx, checkout, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", explain(ctx, checkout, no(err), checkout+" is not a git checkout")
	}
	if err := isSame(ctx, t, top, checkout); err != nil {
		return "", explain(ctx, checkout, err, fmt.Sprintf("%s is a folder inside the checkout %s, not a checkout", checkout, top))
	}
	common, err := commonDir(ctx, t.Git, checkout)
	if err != nil {
		return "", explain(ctx, checkout, no(err), checkout+" is not a git checkout")
	}
	return common, nil
}

// hasBranch says whether checkout's repository has branch. git lists the
// refs of the name, none for a missing branch, so any error is a lookup
// that could not tell, never a missing branch.
func hasBranch(ctx context.Context, git Git, checkout, branch string) (bool, error) {
	ref := "refs/heads/" + branch
	out, err := git(ctx, checkout, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return false, fmt.Errorf("look up the branch %s: %w", branch, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == ref {
			return true, nil
		}
	}
	return false, nil
}

// finished checks the worktree at path as a specialist gets it: a worktree
// of the repository, on branch, and not locked. A new one is checked too:
// the checkout's post-checkout hook runs in git worktree add, and may
// switch its branch or lock it. A check that could not run (a probe that
// failed, git that did not start, a timeout) says so with its cause, apart
// from a worktree that is not as it must be: that one may be fine.
func finished(ctx context.Context, t Tools, checkout, path, common, branch string) error {
	if err := isWorktree(ctx, t, path, common, branch); err != nil {
		return explain(ctx, path, err, fmt.Sprintf("%s exists and is not a worktree of %s on the branch %s", path, checkout, branch))
	}
	// git registered this folder as the worktree, not a copy of one made
	// elsewhere: the worktree's git folder names it back (its gitdir file).
	if err := registered(ctx, t, path, common); err != nil {
		return explain(ctx, path, err, fmt.Sprintf("%s is not the worktree git registered: a copy, or a moved one", path))
	}
	lock, err := lockOf(ctx, t, path)
	if err != nil {
		return explain(ctx, path, err, "")
	}
	if lock != "" {
		return fmt.Errorf("the worktree %s is locked (see %s), as git leaves one a stopped try did not finish: look at it before you unlock or remove it, then delegate again", path, lock)
	}
	return nil
}

// negative is a check's answer that the worktree is not as it must be,
// with git's error when git gave that answer. Any other error of a check
// says the check could not run.
type negative struct{ cause error }

func (n negative) Error() string {
	if n.cause == nil {
		return "no"
	}
	return n.cause.Error()
}

func (n negative) Unwrap() error { return n.cause }

// no is a negative answer: none (cause nil), or git's that ran and said
// no (a *GitExit). git that did not run, or was stopped, is a check that
// could not run.
func no(cause error) error {
	var exit *GitExit
	if cause != nil && !errors.As(cause, &exit) {
		return cause
	}
	return negative{cause}
}

// explain words a failed check of path: a negative answer as what, with
// git's word for it, and a check that could not run, or ran out of time,
// as one that could not tell.
func explain(ctx context.Context, path string, err error, what string) error {
	if cerr := ctx.Err(); cerr != nil {
		return unsure{fmt.Errorf("the check of %s did not finish: %w", path, cerr)}
	}
	var not negative
	if !errors.As(err, &not) {
		return unsure{fmt.Errorf("could not check %s: %w", path, err)}
	}
	if not.cause == nil {
		return errors.New(what)
	}
	return fmt.Errorf("%s: %w", what, not.cause)
}

// finishedDetached checks the worktree at path as a reviewer gets it: a
// worktree of the repository, detached at exactly sha, registered by git at
// path, and not locked. A new one is checked too, as finished says: the
// checkout's post-checkout hook runs in git worktree add, and may move its
// HEAD or lock it. Its answers are finished's.
func finishedDetached(ctx context.Context, t Tools, checkout, path, common, sha string) error {
	if err := attributesIsolated(ctx, t, checkout); err != nil {
		return err
	}
	if err := isDetachedAt(ctx, t, path, common, sha); err != nil {
		return explain(ctx, path, err, fmt.Sprintf("%s exists and is not a worktree of %s detached at %s", path, checkout, sha))
	}
	if err := registered(ctx, t, path, common); err != nil {
		return explain(ctx, path, err, fmt.Sprintf("%s is not the worktree git registered: a copy, or a moved one", path))
	}
	lock, err := lockOf(ctx, t, path)
	if err != nil {
		return explain(ctx, path, err, "")
	}
	if lock != "" {
		return fmt.Errorf("the worktree %s is locked (see %s), as git leaves one a stopped try did not finish: look at it before you unlock or remove it", path, lock)
	}
	// Its files are the commit's, as team_diff shows it: a change, or a
	// file git does not track, is no review of sha (Codex review of #26,
	// round 2).
	// Ignored files count too: the reviewer's read would show them (Codex
	// review of #26, round 3).
	status, err := t.Git(ctx, path, "status", "--porcelain=v1", "--untracked-files=normal", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		return explain(ctx, path, err, "")
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("the worktree %s has changes of its own: a review reads exactly %s, so look at it before you remove it", path, sha)
	}
	// An index entry marked assume-unchanged or skip-worktree hides a
	// file's change, or its absence, from status: none may be (Codex review
	// of #26, round 4).
	if t.Each == nil {
		return errors.New("no streaming git: a review's worktree cannot be checked")
	}
	var hidden error
	err = t.Each(ctx, path, 0, func(entry string) error {
		if entry != "" && !strings.HasPrefix(entry, "H ") {
			hidden = fmt.Errorf("the worktree %s marks %q so status cannot see it: a review reads exactly %s, so look at it before you remove it",
				path, entry, sha)
			return hidden
		}
		return nil
	}, "ls-files", "-v", "-z")
	if hidden != nil {
		return hidden
	}
	if err != nil {
		return explain(ctx, path, err, "")
	}
	return nil
}

// ReviewGitOptions are team.ReviewGitOptions: git's options for every
// command on a review's worktree and its checkout (ADR-0019).
var ReviewGitOptions = team.ReviewGitOptions

// AttributesIsolated is attributesIsolated for the repository of dir, with
// git run with ReviewGitOptions: team_diff checks it right before it runs.
func AttributesIsolated(ctx context.Context, t Tools, dir string) error {
	return attributesIsolated(ctx, reviewTools(t), dir)
}

// attributesIsolated checks what ReviewGitOptions cannot set, so git on a
// review's worktree reads attributes from the empty tree alone (Codex
// review of #26, round 7):
//   - git is 2.43 or later: an older git ignores attr.tree, and reads the
//     commit's .gitattributes;
//   - the checkout's repository has no info/attributes, which git reads
//     whatever its options say;
//   - there is no system attributes file.
//
// t is reviewTools'.
func attributesIsolated(ctx context.Context, t Tools, checkout string) error {
	version, err := t.Git(ctx, checkout, "version")
	if err != nil {
		return explain(ctx, checkout, err, "")
	}
	if !gitAtLeast(version, 2, 43) {
		return fmt.Errorf("%s is older than git 2.43, which ignores attr.tree: a review's folder could hold what the checkout's filters made, so pocketd makes none", strings.TrimSpace(version))
	}
	info, err := t.Git(ctx, checkout, "rev-parse", "--path-format=absolute", "--git-path", "info/attributes")
	if err != nil {
		return explain(ctx, checkout, err, "")
	}
	system, err := t.Git(ctx, checkout, "var", "GIT_ATTR_SYSTEM")
	if err != nil {
		return explain(ctx, checkout, err, "")
	}
	for _, file := range []string{strings.TrimSpace(info), strings.TrimSpace(system)} {
		if file == "" {
			continue
		}
		if !filepath.IsAbs(file) {
			return fmt.Errorf("git named the attributes file %q, not an absolute path", file)
		}
		kind, err := t.Kind(ctx, file)
		if err != nil {
			return unsure{fmt.Errorf("look at %s: %w", file, err)}
		}
		if kind != "absent" {
			return fmt.Errorf("git reads the attributes in %s whatever a review sets, so a review's folder could hold what a filter made: remove it to review %s's pull requests", file, checkout)
		}
	}
	return nil
}

// gitAtLeast says whether version, as git version prints it ("git version
// 2.47.3"), is major.minor or later.
func gitAtLeast(version string, major, minor int) bool {
	m := gitVersion.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	gotMajor, err1 := strconv.Atoi(m[1])
	gotMinor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return false
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
}

var gitVersion = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)`)

// reviewTools are t, with git run with ReviewGitOptions.
func reviewTools(t Tools) Tools {
	git, each := t.Git, t.Each
	t.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		return git(ctx, dir, append(slices.Clone(ReviewGitOptions), args...)...)
	}
	if each != nil {
		t.Each = func(ctx context.Context, dir string, sep byte, fn func(string) error, args ...string) error {
			return each(ctx, dir, sep, fn, append(slices.Clone(ReviewGitOptions), args...)...)
		}
	}
	return t
}

// lockOf is the lock of the worktree at path, "" when it has none: git
// worktree add locks a worktree until it has checked out every file, so a
// stopped try leaves it locked, and so does an owner who locked a finished
// one on purpose. pocketd uses neither, and never removes either: the
// owner looks. git names the worktree's own git folder, which holds its
// lock file "locked", so no list of every worktree is read, and a command
// in the sandbox says whether that name exists, a dangling link included,
// without reading it. An error says they could not tell.
func lockOf(ctx context.Context, t Tools, path string) (string, error) {
	// The worktree's git folder, and its lock's name joined to it: git's
	// own --git-path resolves a link at that name, which would look at the
	// link's target, not at the lock.
	dir, err := t.Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("git cannot name its lock: %w", err)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("git named its git folder %s, not an absolute path", dir)
	}
	lock := filepath.Join(dir, "locked")
	kind, err := t.Kind(ctx, lock)
	if err != nil {
		return "", fmt.Errorf("look at %s: %w", lock, err)
	}
	if kind == "absent" {
		return "", nil
	}
	return lock, nil
}

// isWorktree checks that path is the top of a worktree of the repository
// whose common git folder is common, on branch: nil when it is, a
// negative answer when it is not, and another error when it cannot tell.
func isWorktree(ctx context.Context, t Tools, path, common, branch string) error {
	if err := isTopOf(ctx, t, path, common); err != nil {
		return err
	}
	git := t.Git
	// The full ref: an abbreviation may carry heads/ where a tag has the
	// branch's name, and a detached HEAD has none.
	head, err := git(ctx, path, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return no(err)
	}
	if head != "refs/heads/"+branch {
		return no(nil)
	}
	// A branch with no commit (its ref gone) would start a history of its
	// own: the worktree's HEAD must name a commit.
	if _, err = git(ctx, path, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return no(err)
	}
	return nil
}

// isDetachedAt checks that path is the top of a worktree of the repository
// whose common git folder is common, with its HEAD detached at exactly sha.
// Its answers are isWorktree's.
func isDetachedAt(ctx context.Context, t Tools, path, common, sha string) error {
	if err := isTopOf(ctx, t, path, common); err != nil {
		return err
	}
	// HEAD names no branch: git names a detached HEAD "HEAD" in full, and
	// one on a branch by the branch's ref. A worktree on a branch at sha
	// would move when the branch does.
	name, err := t.Git(ctx, path, "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil {
		return no(err)
	}
	if name != "HEAD" {
		return no(nil)
	}
	// The full id, compared whole: the review is of that commit, and of no
	// other.
	head, err := t.Git(ctx, path, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return no(err)
	}
	if head != sha {
		return no(nil)
	}
	return nil
}

// isTopOf checks that path is the top of a worktree of the repository whose
// common git folder is common, whatever its HEAD names. Its answers are
// isWorktree's.
func isTopOf(ctx context.Context, t Tools, path, common string) error {
	git := t.Git
	// path itself is a folder, not a link to one: a link left where a
	// worktree was moved from would pass every check below through it, and
	// the specialist would work in an alias another could point elsewhere.
	kind, err := t.Kind(ctx, path)
	if err != nil {
		return fmt.Errorf("look at %s: %w", path, err)
	}
	if kind != "dir" {
		return no(nil)
	}
	top, err := git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return no(err)
	}
	if err := isSame(ctx, t, top, path); err != nil {
		return err
	}
	other, err := commonDir(ctx, git, path)
	if err != nil {
		return no(err)
	}
	return isSame(ctx, t, other, common)
}

// isSame checks that a and b name one entry: a negative answer when they do
// not, the probe's error when it could not tell.
func isSame(ctx context.Context, t Tools, a, b string) error {
	ok, err := t.Same(ctx, a, b)
	if err != nil {
		return fmt.Errorf("compare %s with %s: %w", a, b, err)
	}
	if !ok {
		return no(nil)
	}
	return nil
}

// registered checks that git registered path as its worktree: the
// worktree's git folder, as git names it from path, is in the registry
// of the repository whose common folder is common, and holds a gitdir file
// that names path's .git back, read in the sandbox. Its answers are
// isWorktree's.
func registered(ctx context.Context, t Tools, path, common string) error {
	dir, err := t.Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return no(err)
	}
	if !filepath.IsAbs(dir) {
		return no(nil)
	}
	// The git folder is in the repository's registry, where git lists its
	// worktrees from: common's worktrees folder, not one moved out of it.
	if err := isSame(ctx, t, filepath.Dir(dir), filepath.Join(common, "worktrees")); err != nil {
		return err
	}
	gitdir := filepath.Join(dir, "gitdir")
	text, ok, err := t.Read(ctx, gitdir)
	if err != nil {
		return fmt.Errorf("read %s: %w", gitdir, err)
	}
	if !ok {
		return no(nil)
	}
	// git writes the backlink absolute, or relative to the git folder
	// (worktree.useRelativePaths): both name path's .git. It is anchored
	// as written, never cleaned: a ".." after a missing folder or a link
	// resolves in the sandbox as git would resolve it, not as a string.
	back := strings.TrimRight(text, "\n")
	if !filepath.IsAbs(back) {
		back = dir + string(filepath.Separator) + back
	}
	// The backlink names .git in path itself: its folder, as written, is
	// path in the sandbox (a copy whose .git is a hard link to this one's
	// is another folder). And path's .git is the file git wrote there, not
	// a link to another worktree's.
	suffix := string(filepath.Separator) + ".git"
	if !strings.HasSuffix(back, suffix) {
		return no(nil)
	}
	if err := isSame(ctx, t, strings.TrimSuffix(back, suffix), path); err != nil {
		return err
	}
	dotGit := filepath.Join(path, ".git")
	kind, err := t.Kind(ctx, dotGit)
	if err != nil {
		return fmt.Errorf("look at %s: %w", dotGit, err)
	}
	if kind != "file" {
		return no(nil)
	}
	return nil
}

// commonDir is the git folder the checkout's worktrees share, absolute.
func commonDir(ctx context.Context, git Git, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return out, nil
}
