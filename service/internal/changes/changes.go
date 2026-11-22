// Package changes holds the rules of a session's Changes view (ADR-0053):
// the git a read runs on the session's checkout and its options, the base
// its changes are counted from, the parse of git's numstat, a new file's
// count and diff, and the bounds of what an answer carries. The API runs
// the git, in the sandbox, through the engine (AGENTS.md); this package
// decides what it runs and reads.
package changes

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// EmptyTree is git's empty tree in a SHA-1 repository, and EmptyTree256 in
// a SHA-256 one: the base of a branch with no commit yet.
const (
	EmptyTree    = team.EmptyTree
	EmptyTree256 = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
)

// EmptyTreeOf is the empty tree of a repository of object format format,
// as git rev-parse --show-object-format names it (ObjectFormatArgs).
func EmptyTreeOf(format string) (string, error) {
	switch format {
	case "sha1":
		return EmptyTree, nil
	case "sha256":
		return EmptyTree256, nil
	}
	return "", fmt.Errorf("git named the object format %q", team.Clip(format, 40))
}

// MinGit is the oldest git a read runs: 2.45 takes --no-lazy-fetch, so a
// read of a partial clone never fetches what it lacks into the checkout.
const MinGit = "2.45"

// VersionArgs name git's version, with no option an older git refuses.
func VersionArgs() []string { return append(slices.Clone(team.ReviewGitOptions), "version") }

// GitVersionOK says whether version, as git version prints it ("git
// version 2.47.3"), is MinGit or later.
func GitVersionOK(version string) bool {
	m := gitVersion.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	return err1 == nil && err2 == nil && (major > 2 || major == 2 && minor >= 45)
}

var gitVersion = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)`)

// Args are the git arguments of a read of a session's checkout: a
// cross-review's options (team.ReviewGitOptions: no replacement object,
// hook, fsmonitor, pager, external diff or attribute the checkout sets),
// no optional lock, so a read never writes the index, no lazy fetch, so a
// partial clone fetches nothing into it, and no pager, then args. Each
// pathspec a read names is literal by its own magic (Literal).
func Args(args ...string) []string {
	out := append(slices.Clone(team.ReviewGitOptions), "--no-optional-locks", "--no-lazy-fetch", "--no-pager")
	return append(out, args...)
}

// Literal is path as a pathspec that names it alone, from the top, with
// no glob: the path and, if it is a folder, what it holds.
func Literal(path string) string { return ":(top,literal)" + path }

// Below is a pathspec that leaves out what a folder at path holds, never
// path itself: a file that became a folder, or the other way, names its own
// change alone, and a submodule at path, which a pathspec of path/ would
// leave out too, stays in.
func Below(path string) string { return ":(exclude,top,glob)" + globEscape(path) + "/**" }

// diffOptions are the options of every git diff a read runs: no external
// diff or text conversion, no color, paths from the top whatever
// diff.relative says, renames found, and a submodule shown by its commit
// alone, so no git runs in it and its patch is its commits, whatever
// diff.submodule says.
var diffOptions = []string{"--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", "-M", "--ignore-submodules=dirty", "--submodule=short"}

// TopArgs name the top folder of the checkout a session works in. It runs
// before the version is checked, so it takes no option an older git
// refuses: a folder in no checkout says so whatever the git, and
// rev-parse fetches nothing.
func TopArgs() []string {
	return append(slices.Clone(team.ReviewGitOptions), "--no-optional-locks", "--no-pager", "rev-parse", "--show-toplevel")
}

// ObjectFormatArgs name the repository's object format, sha1 or sha256.
func ObjectFormatArgs() []string { return Args("rev-parse", "--show-object-format") }

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

// NumstatArgs list each tracked file changed from from to the folder, with
// its lines added and removed (Numstat): from the merge base, every
// change; from HEAD, what is not committed.
func NumstatArgs(from string) []string {
	return Args(append(append([]string{"diff", "--numstat", "-z"}, diffOptions...), from, "--")...)
}

// CommittedArgs list each file commits changed from the merge base to
// head, as NumstatArgs does.
func CommittedArgs(mergeBase, head string) []string {
	return Args(append(append([]string{"diff", "--numstat", "-z"}, diffOptions...), mergeBase, head, "--")...)
}

// NewArgs name the files git does not track and ignores not, NUL apart:
// each of paths when there are paths, else every one.
func NewArgs(paths ...string) []string {
	args := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	if len(paths) > 0 {
		args = append(args, "--")
		for _, p := range paths {
			args = append(args, Literal(p))
		}
	}
	return Args(args...)
}

// PatchArgs are the unified diff of the tracked file at path from the
// merge base to the folder, from oldPath for a renamed one ("" for none):
// that file's change alone, whatever the folder holds as git runs. A
// pathspec of a path names what a folder of that name holds too, so each
// end leaves that out (Below), never the end itself, a submodule included.
func PatchArgs(mergeBase, oldPath, path string) []string {
	args := append(append([]string{"diff", "--src-prefix=a/", "--dst-prefix=b/"}, diffOptions...), mergeBase, "--")
	if oldPath != "" && oldPath != path {
		args = append(args, Literal(oldPath), Below(oldPath))
	}
	return Args(append(args, Literal(path), Below(path))...)
}

// globEscape is path with each character glob magic reads escaped.
func globEscape(path string) string {
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// File is a file of git's numstat: its lines added and removed, none for
// a binary one, and a renamed one's old path.
type File struct {
	Path, OldPath  string
	Added, Removed int
	Binary         bool
}

// Names says whether f is a change of path, at either of its ends.
func (f File) Names(path string) bool {
	return f.Path == path || (f.OldPath != "" && f.OldPath == path)
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

// The kinds of entry a read of a new file finds (Engine.ReadIn): a regular
// file, one its owner may run, a symbolic link, nothing it may read, and a
// checkout's top that is no longer the folder the read began in.
const (
	KindFile  = "file"
	KindExec  = "exec"
	KindLink  = "link"
	KindNone  = "none"
	KindMoved = "moved"
)

// regular says a read found a regular file, one its owner may run or not.
func regular(kind string) bool { return kind == KindFile || kind == KindExec }

// MaxCountBytes is the most bytes of a new file whose lines pocketd counts.
const MaxCountBytes = 1 << 20

// binaryProbe is how far into a file git looks for a NUL, which makes it
// binary.
const binaryProbe = 8000

// binary says data is binary as git says it: a NUL in its first bytes.
func binary(data []byte) bool { return bytes.IndexByte(data[:min(len(data), binaryProbe)], 0) >= 0 }

// CountNew is a new file's lines as git counts them against nothing, from
// what a read found (kind and data, at most MaxCountBytes+1 bytes): a link
// is one line, its target; a binary file none; counted is false for a file
// past MaxCountBytes and for nothing found.
func CountNew(kind string, data []byte) (added int, isBinary, counted bool) {
	switch {
	case kind == KindLink:
		return 1, false, true
	case !regular(kind) || len(data) > MaxCountBytes:
		return 0, false, false
	case binary(data):
		return 0, true, true
	}
	return lines(data), false, true
}

// lines is how many lines data holds, a last one with no newline included.
func lines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// Quote is a path as git writes it in a patch's header (core.quotePath): as
// it is, or, when it holds a byte that is a control, a quote, a backslash
// or not ASCII, in double quotes, with C's escapes and the rest in octal.
func Quote(path string) string {
	needs := false
	for i := 0; i < len(path); i++ {
		if c := path[i]; c < 0x20 || c == '"' || c == '\\' || c >= 0x7f {
			needs = true
			break
		}
	}
	if !needs {
		return path
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(path); i++ {
		switch c := path[i]; c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// side is a path as a patch's ---/+++ line names it, with its prefix: git
// ends one that holds a space with a tab, for GNU patch.
func side(prefix, path string) string {
	name := Quote(prefix + path)
	if strings.Contains(name, " ") {
		name += "\t"
	}
	return name
}

// NewFileDiff is the unified diff of the new file at path against nothing,
// as git writes one, from what a read found (kind and data, at most
// limit+1 bytes), at most limit bytes: the whole lines that fit, cut when
// some did not. pocketd writes it from the file's bytes, which no filter
// or attribute of the checkout changes.
func NewFileDiff(path, kind string, data []byte, limit int) (diff string, cut bool) {
	mode := "100644"
	switch {
	case kind == KindExec:
		mode = "100755"
	case kind == KindLink:
		mode = "120000"
	case kind != KindFile:
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\nnew file mode %s\n", Quote("a/"+path), Quote("b/"+path), mode)
	switch {
	case kind == KindLink:
		fmt.Fprintf(&b, "--- /dev/null\n+++ %s\n@@ -0,0 +1 @@\n+%s\n\\ No newline at end of file\n", side("b/", path), data)
		return Fit(b.String(), false, limit)
	case binary(data):
		fmt.Fprintf(&b, "Binary files /dev/null and %s differ\n", Quote("b/"+path))
		return Fit(b.String(), false, limit)
	case len(data) == 0:
		return Fit(b.String(), false, limit)
	}
	head := b.String() + fmt.Sprintf("--- /dev/null\n+++ %s\n", side("b/", path))
	whole := len(data) <= limit
	if !whole {
		data = data[:limit]
	}
	text := ValidUTF8(string(data))
	all := strings.SplitAfter(text, "\n")
	if all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	// Room for the hunk's header, which counts the lines that fit.
	room := limit - len(head) - len(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(all)))
	var body strings.Builder
	shown := 0
	for i, line := range all {
		last := i == len(all)-1
		ends := strings.HasSuffix(line, "\n")
		if !ends && (!last || !whole) {
			// A line the bound cut halfway is not shown.
			cut = true
			break
		}
		add := "+" + line
		if !ends {
			add += "\n\\ No newline at end of file\n"
		}
		if body.Len()+len(add) > room {
			cut = true
			break
		}
		body.WriteString(add)
		shown++
	}
	if shown == 0 {
		// No whole line fits: the file's header alone, cut.
		return Fit(b.String(), true, limit)
	}
	hunk := "@@ -0,0 +1 @@\n"
	if shown != 1 {
		hunk = fmt.Sprintf("@@ -0,0 +1,%d @@\n", shown)
	}
	return Fit(head+hunk+body.String(), cut || !whole, limit)
}

// ValidUTF8 is text with each byte that is no UTF-8 replaced by U+FFFD, one
// each, as a JSON encoder replaces them: what the client decodes.
func ValidUTF8(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString("�")
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Fit is a diff page as an answer carries it: valid UTF-8, each byte git
// printed that is not a U+FFFD of its own (ValidUTF8), at most limit bytes,
// and, cut when git printed more (more) or the replacements took it past
// limit, ending after its last whole line: a line cut halfway would read
// as another change. A page of no whole line keeps what fits.
func Fit(page string, more bool, limit int) (string, bool) {
	text := ValidUTF8(page)
	cut := more
	if len(text) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		text, cut = text[:end], true
	}
	if cut {
		text = CutAtLine(text)
	}
	return text, cut
}

// CutAtLine is a diff page that holds less than the whole diff, cut after
// its last whole line. A page of no whole line stays as it is.
func CutAtLine(page string) string {
	if i := strings.LastIndexByte(page, '\n'); i >= 0 {
		return page[:i+1]
	}
	return page
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
