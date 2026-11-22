package changes

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

const (
	shaMain   = "1111111111111111111111111111111111111111"
	shaOrigin = "2222222222222222222222222222222222222222"
	shaDev    = "3333333333333333333333333333333333333333"
)

// The base is the thread's pull request's base branch, on origin first;
// else the checkout's default branch, origin's HEAD first; else none
// (ADR-0053).
func TestTheBaseIsChosenInOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pullBase string
		refs     map[string]Ref
		want     Base
	}{
		{"the pull request's base on origin", "develop", map[string]Ref{
			"refs/remotes/origin/develop": {Commit: shaDev}, "refs/heads/develop": {Commit: shaMain}, "refs/remotes/origin/main": {Commit: shaOrigin},
		}, Base{Name: "origin/develop", Commit: shaDev, Rule: proto.ChangesBasePull}},
		{"the pull request's base here", "develop", map[string]Ref{
			"refs/heads/develop": {Commit: shaDev}, "refs/remotes/origin/main": {Commit: shaOrigin},
		}, Base{Name: "develop", Commit: shaDev, Rule: proto.ChangesBasePull}},
		{"a pull request's base the checkout lacks", "gone", map[string]Ref{
			"refs/remotes/origin/main": {Commit: shaOrigin},
		}, Base{Name: "origin/main", Commit: shaOrigin, Rule: proto.ChangesBaseDefault}},
		{"origin's HEAD names the default", "", map[string]Ref{
			"refs/remotes/origin/HEAD": {Commit: shaDev, Target: "refs/remotes/origin/trunk"}, "refs/remotes/origin/main": {Commit: shaOrigin},
		}, Base{Name: "origin/trunk", Commit: shaDev, Rule: proto.ChangesBaseDefault}},
		{"origin's HEAD that names no branch of origin", "", map[string]Ref{
			"refs/remotes/origin/HEAD": {Commit: shaDev, Target: "refs/heads/elsewhere"}, "refs/heads/main": {Commit: shaMain},
		}, Base{Name: "main", Commit: shaMain, Rule: proto.ChangesBaseDefault}},
		{"origin's main before a local main", "", map[string]Ref{
			"refs/heads/main": {Commit: shaMain}, "refs/remotes/origin/main": {Commit: shaOrigin},
		}, Base{Name: "origin/main", Commit: shaOrigin, Rule: proto.ChangesBaseDefault}},
		{"master when there is no main", "", map[string]Ref{
			"refs/heads/master": {Commit: shaMain},
		}, Base{Name: "master", Commit: shaMain, Rule: proto.ChangesBaseDefault}},
		{"none", "", map[string]Ref{}, Base{Rule: proto.ChangesBaseHead}},
		{"a pull request's base git could not hold", "a..b", map[string]Ref{
			"refs/remotes/origin/a..b": {Commit: shaDev},
		}, Base{Rule: proto.ChangesBaseHead}},
	} {
		if got := ChooseBase(tc.pullBase, tc.refs); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The refs listed are exactly the candidates, the pull request's base
// first, and only a base git could hold is one.
func TestTheCandidatesNameThePullBaseFirst(t *testing.T) {
	got := Candidates("develop")
	want := []string{"refs/remotes/origin/develop", "refs/heads/develop", "refs/remotes/origin/HEAD",
		"refs/remotes/origin/main", "refs/remotes/origin/master", "refs/heads/main", "refs/heads/master"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates %q", got)
	}
	for _, bad := range []string{"-x", "--output=/tmp/x", "a b", "a..b", "x.lock", "x/", "/x", ".hidden", "a/.b", "a:b", "a*", "@", "a@{1}", ""} {
		if ValidBranch(bad) {
			t.Errorf("%q is a valid branch", bad)
		}
		if slices.Contains(Candidates(bad), "refs/heads/"+bad) {
			t.Errorf("%q is a candidate", bad)
		}
	}
	for _, good := range []string{"main", "release/1.2", "feature/x-y_z"} {
		if !ValidBranch(good) {
			t.Errorf("%q is no valid branch", good)
		}
	}
}

func TestParseRefsReadsNameCommitAndTarget(t *testing.T) {
	refs, err := ParseRefs("refs/heads/main\x00" + shaMain + "\x00\nrefs/remotes/origin/HEAD\x00" + shaOrigin + "\x00refs/remotes/origin/main\n")
	if err != nil {
		t.Fatal(err)
	}
	if refs["refs/heads/main"] != (Ref{Commit: shaMain}) || refs["refs/remotes/origin/HEAD"] != (Ref{Commit: shaOrigin, Target: "refs/remotes/origin/main"}) {
		t.Fatalf("refs %+v", refs)
	}
	if _, err := ParseRefs("refs/heads/main\x00notasha\x00\n"); err == nil {
		t.Fatal("a ref without a commit was read")
	}
	if refs, err := ParseRefs(""); err != nil || len(refs) != 0 {
		t.Fatalf("no refs: %v %v", refs, err)
	}
}

// git's numstat is read record by record: a file, a binary one, and a
// rename's two paths after its counts (ADR-0053).
func TestNumstatReadsFilesRenamesAndBinaries(t *testing.T) {
	records := []string{"3\t1\tREADME.md", "-\t-\tlogo.png", "4\t0\t", "old name.txt", "new name.txt", "0\t7\tgone.go"}
	var n Numstat
	var got []File
	for _, r := range records {
		f, ok, err := n.Next(r)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			got = append(got, f)
		}
	}
	if err := n.Done(); err != nil {
		t.Fatal(err)
	}
	want := []File{{Path: "README.md", Added: 3, Removed: 1}, {Path: "logo.png", Binary: true}, {Path: "new name.txt", OldPath: "old name.txt", Added: 4},
		{Path: "gone.go", Removed: 7}}
	if !slices.Equal(got, want) {
		t.Fatalf("files %+v", got)
	}
	for _, bad := range []string{"3 1 README", "x\t1\tREADME", "-1\t2\tREADME", "3\tREADME"} {
		var n Numstat
		if _, _, err := n.Next(bad); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
	var cut Numstat
	_, _, _ = cut.Next("1\t1\t")
	_, _, _ = cut.Next("old")
	if cut.Done() == nil {
		t.Error("a rename without its new path ended well")
	}
}

// A new file's lines come from git's diff against nothing, which names it
// a rename from /dev/null.
func TestANewFilesCountIsRead(t *testing.T) {
	f, err := NewCount("12\t0\t\x00/dev/null\x00notes.md\x00")
	if err != nil || f.Added != 12 || f.Path != "notes.md" || f.Binary {
		t.Fatalf("count %+v %v", f, err)
	}
	f, err = NewCount("-\t-\t\x00/dev/null\x00logo.png\x00")
	if err != nil || !f.Binary {
		t.Fatalf("binary %+v %v", f, err)
	}
	if _, err := NewCount(""); err == nil {
		t.Fatal("nothing was counted")
	}
}

// A diff of a new file ends with git's status 1: what it printed is the
// answer, and status 1 with nothing printed, or any other error, is not.
func TestDiffersTakesStatusOneWithADiff(t *testing.T) {
	one := fmt.Errorf("git diff: %w", &worktree.GitExit{Status: 1})
	if out, err := Differs("+a\n", one); err != nil || out != "+a\n" {
		t.Fatalf("status 1 with a diff: %q %v", out, err)
	}
	if _, err := Differs("", one); err == nil {
		t.Fatal("status 1 with nothing printed was taken")
	}
	if _, err := Differs("+a\n", fmt.Errorf("git: %w", &worktree.GitExit{Status: 128})); err == nil {
		t.Fatal("status 128 was taken")
	}
	if _, err := Differs("+a\n", errors.New("the sandbox failed")); err == nil {
		t.Fatal("a git that did not run was taken")
	}
}

// A diff that did not fit ends at its last whole line, so no half line
// reads as a change; one with no whole line stays as it is.
func TestACutDiffEndsAtAWholeLine(t *testing.T) {
	if got := CutAtLine("@@ -1 +1 @@\n-a\n+bcd"); got != "@@ -1 +1 @@\n-a\n" {
		t.Fatalf("cut %q", got)
	}
	if got := CutAtLine("+" + strings.Repeat("x", 10)); got != "+xxxxxxxxxx" {
		t.Fatalf("one line %q", got)
	}
}

// A file's diff is asked for by a path from the checkout's top, which no
// absolute or leaving path is.
func TestCheckPathRefusesWhatNoChangeNames(t *testing.T) {
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "./a", "a/", "a\x00b", "\xff", strings.Repeat("a", MaxPath+1)} {
		if CheckPath(bad) == nil {
			t.Errorf("%q passed", bad)
		}
	}
	for _, good := range []string{"README.md", "a/b c/d.go", "-dash", ":colon", "ünï.txt"} {
		if err := CheckPath(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

// The thread's pull request a base comes from is its newest open one,
// else its newest.
func TestPullBaseIsTheNewestOpenOnesBase(t *testing.T) {
	for _, tc := range []struct {
		pulls []Pull
		want  string
	}{
		{nil, ""},
		{[]Pull{{ID: 1, Base: "main"}, {ID: 2, Base: "develop"}}, "develop"},
		{[]Pull{{ID: 1, Open: true, Base: "main"}, {ID: 2, Base: "develop"}}, "main"},
		{[]Pull{{ID: 1, Open: true, Base: "main"}, {ID: 3, Open: true, Base: "release"}, {ID: 2, Base: "develop"}}, "release"},
		{[]Pull{{ID: 4, Open: true}}, ""},
	} {
		if got := PullBase(tc.pulls); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.pulls, got, tc.want)
		}
	}
}

// Every read runs git with a cross-review's options, takes no optional
// lock, so it never writes the index, and reads paths literally; a diff
// runs no external diff or text conversion and no git in a submodule
// (ADR-0053).
func TestEveryReadRunsGitIsolated(t *testing.T) {
	prefix := append(slices.Clone(team.ReviewGitOptions), "--no-optional-locks", "--literal-pathspecs", "--no-pager")
	all := [][]string{TopArgs(), HeadArgs(), BranchArgs(), RefsArgs(Candidates("main")), MergeBaseArgs(shaMain, shaDev),
		NumstatArgs(shaMain), CommittedArgs(shaMain, shaDev), UncommittedArgs(shaDev), NewArgs(), NewArgs("a"), NewNumstatArgs("a"),
		PatchArgs(shaMain, "old", "a"), NewPatchArgs("a")}
	for _, args := range all {
		if !slices.Equal(args[:len(prefix)], prefix) {
			t.Errorf("%q does not start with the isolation options", args)
		}
	}
	for _, args := range [][]string{NumstatArgs(shaMain), CommittedArgs(shaMain, shaDev), UncommittedArgs(shaDev), PatchArgs(shaMain, "", "a")} {
		for _, want := range []string{"--no-ext-diff", "--no-textconv", "--no-color", "--ignore-submodules=dirty", "--no-relative"} {
			if !slices.Contains(args, want) {
				t.Errorf("%q lacks %s", args, want)
			}
		}
	}
	for _, args := range [][]string{NewNumstatArgs("a"), NewPatchArgs("a")} {
		for _, want := range []string{"--no-index", "--no-ext-diff", "--no-textconv", "--no-color"} {
			if !slices.Contains(args, want) {
				t.Errorf("%q lacks %s", args, want)
			}
		}
	}
	// A path goes after --, so none reads as an option.
	for _, args := range [][]string{NewArgs("-x"), NewNumstatArgs("-x"), PatchArgs(shaMain, "-o", "-x"), NewPatchArgs("-x")} {
		dash := slices.Index(args, "--")
		if dash < 0 || slices.Index(args, "-x") < dash {
			t.Errorf("%q names a path before --", args)
		}
	}
	if got := PatchArgs(shaMain, "old", "new"); !slices.Equal(got[len(got)-3:], []string{"--", "old", "new"}) {
		t.Errorf("a rename's patch names %q", got)
	}
}
