// Package changes holds the rules of a session's Changes view (ADR-0053):
// the git a read runs on the session's checkout and its options, the base
// its changes are counted from, the parse of git's numstat, and the bounds
// of what an answer carries. The API runs the git, in the sandbox, through
// the engine (AGENTS.md); this package decides what it runs and reads.
package changes

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// EmptyTree is git's empty tree: the base of a branch with no commit yet.
const EmptyTree = team.EmptyTree

// Args are the git arguments of a read of a session's checkout: a
// cross-review's options (team.ReviewGitOptions: no replacement object,
// hook, fsmonitor, pager, external diff or attribute the checkout sets),
// no optional lock, so a read never writes the index, literal pathspecs
// and no pager, then args.
func Args(args ...string) []string {
	out := append(slices.Clone(team.ReviewGitOptions), "--no-optional-locks", "--literal-pathspecs", "--no-pager")
	return append(out, args...)
}

// diffOptions are the options of every git diff a read runs: no external
// diff or text conversion, no color, paths from the top whatever
// diff.relative says, renames found, and a submodule shown by its commit
// alone, so no git runs in it.
var diffOptions = []string{"--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", "-M", "--ignore-submodules=dirty"}

// TopArgs name the top folder of the checkout a session works in; git
// that answers on its own that it cannot (its exit status) says there is
// no checkout.
func TopArgs() []string { return Args("rev-parse", "--show-toplevel") }

// HeadArgs name HEAD's commit; git's status 1 says there is none yet.
func HeadArgs() []string { return Args("rev-parse", "--verify", "--quiet", "HEAD^{commit}") }

// BranchArgs name the branch checked out; git's status 1 says HEAD is
// detached.
func BranchArgs() []string { return Args("symbolic-ref", "--quiet", "HEAD") }

// remote is the remote whose branches a base is read from first.
const remote = "origin"

// defaultBranches are the branches tried, in order, as a checkout's
// default when origin's HEAD names none.
var defaultBranches = []string{"refs/remotes/" + remote + "/main", "refs/remotes/" + remote + "/master", "refs/heads/main", "refs/heads/master"}

// originHead is the ref that names origin's default branch.
const originHead = "refs/remotes/" + remote + "/HEAD"

// Candidates are the refs a base is chosen from, for a thread whose pull
// request has base branch pullBase ("" for none): that branch on origin
// and here, origin's HEAD, then the default branches.
func Candidates(pullBase string) []string {
	var out []string
	if ValidBranch(pullBase) {
		out = append(out, "refs/remotes/"+remote+"/"+pullBase, "refs/heads/"+pullBase)
	}
	out = append(out, originHead)
	return append(out, defaultBranches...)
}

// RefsArgs list the candidates' refs, each as its name, its commit and,
// for a symbolic ref, its target, NUL apart, a line each (ParseRefs).
func RefsArgs(candidates []string) []string {
	return Args(append([]string{"for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)"}, candidates...)...)
}

// Ref is a ref for-each-ref listed.
type Ref struct {
	// Commit is the object it names, its target's for a symbolic ref, and
	// Target the symbolic ref's target, "" for any other.
	Commit, Target string
}

// ParseRefs reads RefsArgs' output, by ref name.
func ParseRefs(out string) (map[string]Ref, error) {
	refs := map[string]Ref{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 || parts[0] == "" || !fullCommit(parts[1]) {
			return nil, fmt.Errorf("git listed a ref as %q", team.Clip(line, 80))
		}
		refs[parts[0]] = Ref{Commit: parts[1], Target: parts[2]}
	}
	return refs, nil
}

// Base is the base a session's changes are counted against.
type Base struct {
	// Name is the branch as git names it ("origin/main"), Commit its
	// commit; both "" for the rule head.
	Name, Commit string
	Rule         proto.ChangesBase
}

// ChooseBase is the base of a session's changes, from refs of the
// candidates (Candidates): the thread's pull request's base branch
// pullBase, on origin and then here; else the checkout's default branch,
// origin's HEAD and then main and master, on origin and then here; else
// none, and the changes are those since HEAD.
func ChooseBase(pullBase string, refs map[string]Ref) Base {
	if ValidBranch(pullBase) {
		for _, name := range []string{"refs/remotes/" + remote + "/" + pullBase, "refs/heads/" + pullBase} {
			if r, ok := refs[name]; ok {
				return Base{Name: short(name), Commit: r.Commit, Rule: proto.ChangesBasePull}
			}
		}
	}
	if r, ok := refs[originHead]; ok && strings.HasPrefix(r.Target, "refs/remotes/"+remote+"/") && r.Target != originHead {
		return Base{Name: short(r.Target), Commit: r.Commit, Rule: proto.ChangesBaseDefault}
	}
	for _, name := range defaultBranches {
		if r, ok := refs[name]; ok {
			return Base{Name: short(name), Commit: r.Commit, Rule: proto.ChangesBaseDefault}
		}
	}
	return Base{Rule: proto.ChangesBaseHead}
}

// short is a ref's name as git shows a branch: origin/main, main.
func short(ref string) string {
	if name, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		return name
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

// ShortBranch is the branch symbolic-ref named (BranchArgs), "" for a
// ref that is no branch.
func ShortBranch(ref string) string {
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return ""
	}
	return name
}

// MergeBaseArgs name where head leaves base; git's status 1 says the two
// share no commit.
func MergeBaseArgs(base, head string) []string {
	return Args("merge-base", "--end-of-options", base, head)
}

// NumstatArgs list each tracked file changed from the merge base to the
// folder, with its lines added and removed (Numstat).
func NumstatArgs(mergeBase string) []string {
	return Args(append(append([]string{"diff", "--numstat", "-z"}, diffOptions...), mergeBase, "--")...)
}

// CommittedArgs name the files commits changed from the merge base to
// head, NUL apart.
func CommittedArgs(mergeBase, head string) []string {
	return Args(append(append([]string{"diff", "--name-only", "-z"}, diffOptions...), mergeBase, head, "--")...)
}

// UncommittedArgs name the tracked files the folder changes since head,
// staged or not, NUL apart.
func UncommittedArgs(head string) []string {
	return Args(append(append([]string{"diff", "--name-only", "-z"}, diffOptions...), head, "--")...)
}

// NewArgs name the files git does not track and ignores not, NUL apart,
// each of paths when there are paths, else every one.
func NewArgs(paths ...string) []string {
	args := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	return Args(args...)
}

// NewNumstatArgs count the lines of a new file, as git diff counts them
// against nothing. git ends such a diff with status 1 when it printed one
// (Differs).
func NewNumstatArgs(path string) []string {
	return Args("diff", "--no-index", "--numstat", "-z", "--no-ext-diff", "--no-textconv", "--no-color", "--", "/dev/null", path)
}

// PatchArgs are the unified diff of the tracked file at path from the
// merge base to the folder, from oldPath for a renamed one ("" for none).
func PatchArgs(mergeBase, oldPath, path string) []string {
	args := append(append([]string{"diff", "--src-prefix=a/", "--dst-prefix=b/"}, diffOptions...), mergeBase, "--")
	if oldPath != "" && oldPath != path {
		args = append(args, oldPath)
	}
	return Args(append(args, path)...)
}

// NewPatchArgs are the unified diff of a new file against nothing; git
// ends it with status 1 when it printed one (Differs).
func NewPatchArgs(path string) []string {
	return Args("diff", "--no-index", "--src-prefix=a/", "--dst-prefix=b/", "--no-ext-diff", "--no-textconv", "--no-color", "--", "/dev/null", path)
}

// Differs is what a git diff --no-index answered: its status 1 says it
// printed a difference, so out is its answer then. Status 1 with nothing
// printed is git that could not read a file, and stays an error, as does
// any other.
func Differs(out string, err error) (string, error) {
	var exit *worktree.GitExit
	if errors.As(err, &exit) && exit.Status == 1 && out != "" {
		return out, nil
	}
	return out, err
}

// File is a file of git's numstat: its lines added and removed, none for
// a binary one, and a renamed one's old path.
type File struct {
	Path, OldPath  string
	Added, Removed int
	Binary         bool
}

// Numstat reads git diff --numstat -z, record by record (records split at
// NUL): a file is "added\tremoved\tpath"; a renamed one is
// "added\tremoved\t", then its old path, then its new one; a binary file
// counts "-" for both. The zero value is ready.
type Numstat struct {
	pending *File
	// old is set once a rename's old path was read.
	old bool
}

// Next reads record, and gives the file it ends, ok false when it ends
// none yet.
func (n *Numstat) Next(record string) (f File, ok bool, err error) {
	if p := n.pending; p != nil {
		if record == "" {
			return File{}, false, errors.New("git named a renamed file by an empty path")
		}
		if !n.old {
			p.OldPath, n.old = record, true
			return File{}, false, nil
		}
		p.Path = record
		n.pending, n.old = nil, false
		return *p, true, nil
	}
	added, rest, ok1 := strings.Cut(record, "\t")
	removed, path, ok2 := strings.Cut(rest, "\t")
	if !ok1 || !ok2 {
		return File{}, false, fmt.Errorf("git's numstat has the record %q", team.Clip(record, 80))
	}
	f = File{Path: path}
	switch {
	case added == "-" && removed == "-":
		f.Binary = true
	default:
		a, errA := strconv.Atoi(added)
		r, errR := strconv.Atoi(removed)
		if errA != nil || errR != nil || a < 0 || r < 0 {
			return File{}, false, fmt.Errorf("git's numstat counts %q", team.Clip(record, 80))
		}
		f.Added, f.Removed = a, r
	}
	if path == "" {
		pending := f
		n.pending = &pending
		return File{}, false, nil
	}
	return f, true, nil
}

// Done says the records read end no file halfway: a rename's paths came.
func (n *Numstat) Done() error {
	if n.pending != nil {
		return errors.New("git's numstat ended inside a renamed file's record")
	}
	return nil
}

// NewCount is a new file's lines from NewNumstatArgs' output: git names it
// a rename from /dev/null, which a parse reads as any other file.
func NewCount(out string) (File, error) {
	var n Numstat
	for _, record := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		f, ok, err := n.Next(record)
		if err != nil {
			return File{}, err
		}
		if ok {
			return f, nil
		}
	}
	return File{}, errors.New("git counted no lines of the new file")
}

// CutAtLine is a diff page that holds less than the whole diff, cut after
// its last whole line: a line cut halfway would read as another change.
// A page of no whole line stays as it is.
func CutAtLine(page string) string {
	if i := strings.LastIndexByte(page, '\n'); i >= 0 {
		return page[:i+1]
	}
	return page
}

// MaxPath is the most bytes of a path a file's diff is asked for by.
const MaxPath = 4096

// CheckPath refuses a path no listed change can have: empty, too long, not
// UTF-8, holding a NUL, absolute, or with a part that leaves the
// checkout. Only a path git lists as changed is then read (rule 64).
func CheckPath(path string) error {
	switch {
	case path == "" || len(path) > MaxPath || !utf8.ValidString(path) || strings.ContainsRune(path, 0):
		return fmt.Errorf("path must be 1 to %d bytes of UTF-8 without a NUL", MaxPath)
	case strings.HasPrefix(path, "/"):
		return errors.New("path names a file in the checkout, from its top, not an absolute path")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("path %q is not a file's path from the checkout's top", team.Clip(path, 60))
		}
	}
	return nil
}

// ValidBranch says whether name is a branch git could hold, as a base
// branch GitHub named: a ref component list with nothing git refuses
// (check-ref-format) and nothing that reads as an option.
func ValidBranch(name string) bool {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") || name == "@" {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// fullCommit says whether s is a full object name, SHA-1 or SHA-256.
func fullCommit(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// FullCommit says whether s is a full object name, as git prints one.
func FullCommit(s string) bool { return fullCommit(s) }

// Pull is what a base is chosen by of a pull request linked to the
// session's thread.
type Pull struct {
	ID   int64
	Open bool
	Base string
}

// PullBase is the base branch of the thread's pull request a session's
// changes are counted against: the newest open one's, else the newest's;
// "" for none.
func PullBase(pulls []Pull) string {
	best := Pull{}
	for _, p := range pulls {
		if p.Base == "" {
			continue
		}
		if best.Base == "" || (p.Open && !best.Open) || (p.Open == best.Open && p.ID > best.ID) {
			best = p
		}
	}
	return best.Base
}
