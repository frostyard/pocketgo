package changes

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/team"
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

func TestParseRefReadsNameCommitAndTarget(t *testing.T) {
	name, ref, err := ParseRef("refs/heads/main\x00" + shaMain + "\x00")
	if err != nil || name != "refs/heads/main" || ref != (Ref{Commit: shaMain}) {
		t.Fatalf("%q %+v %v", name, ref, err)
	}
	name, ref, err = ParseRef("refs/remotes/origin/HEAD\x00" + shaOrigin + "\x00refs/remotes/origin/main")
	if err != nil || name != "refs/remotes/origin/HEAD" || ref != (Ref{Commit: shaOrigin, Target: "refs/remotes/origin/main"}) {
		t.Fatalf("%q %+v %v", name, ref, err)
	}
	if _, _, err := ParseRef("refs/heads/main\x00notasha\x00"); err == nil {
		t.Fatal("a ref without a commit was read")
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

// A new file's lines are counted from its bytes as git counts them
// against nothing: a link is one line, a binary file none, and a file past
// MaxCountBytes, or nothing found, is not counted.
func TestANewFilesLinesAreCounted(t *testing.T) {
	for _, tc := range []struct {
		name            string
		kind            string
		data            []byte
		added           int
		binary, counted bool
	}{
		{"lines", KindFile, []byte("a\nb\nc\n"), 3, false, true},
		{"no newline at its end", KindFile, []byte("a\nb"), 2, false, true},
		{"empty", KindFile, nil, 0, false, true},
		{"binary", KindFile, []byte("PNG\x00\x01"), 0, true, true},
		{"a link", KindLink, []byte("../target"), 1, false, true},
		{"too large", KindFile, make([]byte, MaxCountBytes+1), 0, false, false},
		{"nothing", KindNone, nil, 0, false, false},
	} {
		added, binary, counted := CountNew(tc.kind, tc.data)
		if added != tc.added || binary != tc.binary || counted != tc.counted {
			t.Errorf("%s: %d %v %v", tc.name, added, binary, counted)
		}
	}
}

// A new file's diff is written from its bytes as git writes one against
// nothing, which no filter of the checkout changes, and it fits its bound
// at a whole line (ADR-0053, Codex review of #109, round 1).
func TestANewFilesDiffIsWrittenFromItsBytes(t *testing.T) {
	for _, tc := range []struct {
		name, kind, data string
		limit            int
		want             string
		cut              bool
	}{
		{"lines", KindFile, "one\ntwo\n", 4096,
			"diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n", false},
		{"one line with no newline", KindFile, "one", 4096,
			"diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+one\n\\ No newline at end of file\n", false},
		{"empty", KindFile, "", 4096, "diff --git a/n.txt b/n.txt\nnew file mode 100644\n", false},
		{"binary", KindFile, "a\x00b", 4096, "diff --git a/n.txt b/n.txt\nnew file mode 100644\nBinary files /dev/null and b/n.txt differ\n", false},
		{"a link", KindLink, "../t", 4096,
			"diff --git a/n.txt b/n.txt\nnew file mode 120000\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+../t\n\\ No newline at end of file\n", false},
		{"cut", KindFile, "aaaa\nbbbb\ncccc\n", 100,
			"diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+aaaa\n", true},
		{"not UTF-8", KindFile, "caf\xe9\n", 4096,
			"diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+caf�\n", false},
	} {
		got, cut := NewFileDiff("n.txt", tc.kind, []byte(tc.data), tc.limit)
		if got != tc.want || cut != tc.cut || len(got) > tc.limit {
			t.Errorf("%s: %q %v", tc.name, got, cut)
		}
	}
	if got, _ := NewFileDiff("n.txt", KindNone, nil, 4096); got != "" {
		t.Errorf("nothing found has a diff: %q", got)
	}
	// A file longer than the bound comes as limit+1 bytes of a read: the
	// diff holds its whole lines alone, within the bound, and is cut.
	var long strings.Builder
	for i := range 40 {
		long.WriteString(strings.Repeat(string(rune('a'+i%26)), 9) + "\n")
	}
	const limit = 200
	got, cut := NewFileDiff("n.txt", KindFile, []byte(long.String()[:limit+1]), limit)
	if !cut || len(got) > limit || !strings.HasSuffix(got, "\n") || strings.Contains(got, "No newline") {
		t.Fatalf("a long file: %d bytes, cut %v: %q", len(got), cut, got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n")[5:] {
		if len(line) != 10 {
			t.Fatalf("a line cut halfway: %q in %q", line, got)
		}
	}
}

// A page an answer carries is valid UTF-8 within its bound, though each
// byte git printed that is no UTF-8 takes three as U+FFFD; and a page cut
// ends at a whole line (Codex review of #109, round 1).
func TestAPageFitsItsBoundAsTheClientGetsIt(t *testing.T) {
	latin := strings.Repeat("+caf\xe9\n", 20) // 6 bytes a line, 8 once valid
	got, cut := Fit(latin, false, 100)
	if !cut || len(got) > 100 || !utf8.ValidString(got) || !strings.HasSuffix(got, "\n") || !strings.HasPrefix(got, "+caf�\n") {
		t.Fatalf("a Latin-1 page: %d bytes, cut %v: %q", len(got), cut, got)
	}
	if got, cut := Fit("@@ -1 +1 @@\n-a\n+bcd", true, 100); got != "@@ -1 +1 @@\n-a\n" || !cut {
		t.Fatalf("a page git cut: %q %v", got, cut)
	}
	if got, cut := Fit("+a\n", false, 100); got != "+a\n" || cut {
		t.Fatalf("a whole page: %q %v", got, cut)
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
// absolute or leaving path is (proto.CheckChangedPath, which pocketd and the
// desktop's fake share).
func TestCheckChangedPathRefusesWhatNoChangeNames(t *testing.T) {
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "./a", "a/", "a\x00b", "\xff", strings.Repeat("a", proto.MaxChangedPath+1)} {
		if proto.CheckChangedPath(bad) == nil {
			t.Errorf("%q passed", bad)
		}
	}
	for _, good := range []string{"README.md", "a/b c/d.go", "-dash", "-", ":colon", "ünï.txt"} {
		if err := proto.CheckChangedPath(good); err != nil {
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

// git 2.45 or later reads: it takes --no-lazy-fetch.
func TestGitVersionOK(t *testing.T) {
	for v, want := range map[string]bool{"git version 2.45.0": true, "git version 2.47.3\n": true, "git version 3.0.0": true,
		"git version 2.44.1": false, "git version 2.43.0": false, "git version 1.99": false, "nonsense": false} {
		if GitVersionOK(v) != want {
			t.Errorf("%q: %v", v, !want)
		}
	}
}

// The empty tree a branch with no commit counts from is the one of its
// repository's object format (Codex review of #109, round 1).
func TestTheEmptyTreeIsTheObjectFormats(t *testing.T) {
	if got, err := EmptyTreeOf("sha1"); err != nil || got != team.EmptyTree {
		t.Fatalf("sha1: %q %v", got, err)
	}
	if got, err := EmptyTreeOf("sha256"); err != nil || got != EmptyTree256 || len(got) != 64 {
		t.Fatalf("sha256: %q %v", got, err)
	}
	if _, err := EmptyTreeOf("md5"); err == nil {
		t.Fatal("an unknown format has an empty tree")
	}
}

// Every read runs git with a cross-review's options, takes no optional
// lock, so it never writes the index, fetches nothing a partial clone
// lacks, and names each path literally; a diff runs no external diff or
// text conversion, no git in a submodule, and shows a submodule by its
// commits (ADR-0053; Codex review of #109, round 1).
func TestEveryReadRunsGitIsolated(t *testing.T) {
	prefix := append(slices.Clone(team.ReviewGitOptions), "-c", "core.quotePath=true", "--no-optional-locks", "--no-lazy-fetch", "--no-pager")
	all := [][]string{ObjectFormatArgs(), HeadArgs(), BranchArgs(), RefsArgs(Candidates("main")), MergeBaseArgs(shaMain, shaDev),
		NumstatArgs(shaMain), CommittedArgs(shaMain, shaDev), NewArgs(), NewArgs("a"), PatchArgs(shaMain, "old", "a")}
	for _, args := range all {
		if !slices.Equal(args[:len(prefix)], prefix) {
			t.Errorf("%q does not start with the isolation options", args)
		}
	}
	if v := VersionArgs(); slices.Contains(v, "--no-lazy-fetch") || v[len(v)-1] != "version" {
		t.Errorf("the version is asked with %q, which an older git refuses", v)
	}
	// The top is named before the version is checked: with no option an
	// older git refuses, so a folder in no checkout says so whatever the git.
	if top := TopArgs(); slices.Contains(top, "--no-lazy-fetch") || !slices.Equal(top[:len(team.ReviewGitOptions)], team.ReviewGitOptions) {
		t.Errorf("the top is named with %q", top)
	}
	for _, args := range [][]string{NumstatArgs(shaMain), CommittedArgs(shaMain, shaDev), PatchArgs(shaMain, "", "a")} {
		for _, want := range []string{"--no-ext-diff", "--no-textconv", "--no-color", "--ignore-submodules=dirty", "--submodule=short", "--no-relative"} {
			if !slices.Contains(args, want) {
				t.Errorf("%q lacks %s", args, want)
			}
		}
	}
	// A path goes after --, literal by its own magic, so none reads as an
	// option or a glob, and a file's patch leaves out what a folder of its
	// name holds.
	for _, args := range [][]string{NewArgs("-x*"), PatchArgs(shaMain, "-o", "-x*")} {
		dash := slices.Index(args, "--")
		if dash < 0 || !slices.Contains(args[dash:], ":(top,literal)-x*") || slices.Contains(args, "-x*") {
			t.Errorf("%q names a path unliterally or before --", args)
		}
	}
	// Each end leaves out what a folder of its name holds, as git runs,
	// with no glob of its own name: never the end itself, a submodule's
	// gitlink included (Codex review of #109, round 2).
	got := PatchArgs(shaMain, "old", "new")
	if want := []string{"--", ":(top,literal)old", ":(top,literal)new", ":(exclude,top,glob)old/**", ":(exclude,top,glob)new/**"}; !slices.Equal(got[len(got)-5:], want) {
		t.Errorf("a rename's patch names %q", got)
	}
	if got := Below(`a*b?[c]\d`); got != `:(exclude,top,glob)a\*b\?\[c]\\d/**` {
		t.Errorf("a glob's own characters below %q", got)
	}
}

// A change names a path at either end: a rename's old path too.
func TestAChangeNamesBothEnds(t *testing.T) {
	f := File{Path: "new.go", OldPath: "old.go"}
	if !f.Names("new.go") || !f.Names("old.go") || f.Names("other.go") || (File{Path: "a"}).Names("") {
		t.Fatal("ends")
	}
}

// A new file's diff says what git would record of it: an executable is
// 100755, and a path git quotes is quoted as git quotes it, a newline in it
// part of the name, never a line of its own (Codex review of #109, round
// 2).
func TestANewFilesHeaderIsGits(t *testing.T) {
	got, _ := NewFileDiff("run.sh", KindExec, []byte("#!/bin/sh\n"), 4096)
	if !strings.HasPrefix(got, "diff --git a/run.sh b/run.sh\nnew file mode 100755\n") {
		t.Fatalf("an executable: %q", got)
	}
	if got, _ := NewFileDiff("tool", KindExec, []byte("\x7fELF\x00"), 4096); !strings.Contains(got, "new file mode 100755\nBinary files /dev/null and b/tool differ\n") {
		t.Fatalf("an executable binary: %q", got)
	}
	if added, binary, counted := CountNew(KindExec, []byte("a\nb\n")); added != 2 || binary || !counted {
		t.Fatalf("an executable's lines: %d %v %v", added, binary, counted)
	}
	got, _ = NewFileDiff("a\nb \"c\"\\é.txt", KindFile, []byte("x\n"), 4096)
	want := "diff --git \"a/a\\nb \\\"c\\\"\\\\\\303\\251.txt\" \"b/a\\nb \\\"c\\\"\\\\\\303\\251.txt\"\nnew file mode 100644\n" +
		"--- /dev/null\n+++ \"b/a\\nb \\\"c\\\"\\\\\\303\\251.txt\"\t\n@@ -0,0 +1 @@\n+x\n"
	if got != want {
		t.Fatalf("a path git quotes:\n%q\nwant\n%q", got, want)
	}
	if got := Quote("plain/path.go"); got != "plain/path.go" {
		t.Fatalf("a plain path: %q", got)
	}
	if got, _ := NewFileDiff("a b.txt", KindFile, []byte("x\n"), 4096); !strings.Contains(got, "+++ b/a b.txt\t\n") {
		t.Fatalf("a path with a space: %q", got)
	}
}

// Each byte that is no UTF-8 is a U+FFFD of its own, as the client's JSON
// decoder makes it, before a diff's bound applies (Codex review of #109,
// round 2).
func TestEachByteThatIsNoUTF8IsAReplacement(t *testing.T) {
	if got := ValidUTF8("a\xe9\xe8b"); got != "a��b" {
		t.Fatalf("%q", got)
	}
	if got := ValidUTF8("café"); got != "café" {
		t.Fatalf("%q", got)
	}
	got, cut := Fit(strings.Repeat("+\xe9\xe9\n", 10), false, 40)
	if !cut || len(got) > 40 || got != strings.Repeat("+��\n", 5) {
		t.Fatalf("a page of Latin-1: %q %v", got, cut)
	}
}

// A rename across a folder's boundary keeps both its ends: an end's folder
// is left out only where the other end does not lie below it, and the
// patch keeps the change's own section alone, whatever siblings git
// printed (Codex review of #109, round 3).
func TestARenameAcrossAFolderKeepsBothEnds(t *testing.T) {
	got := PatchArgs(shaMain, "config", "config/settings")
	if want := []string{"--", ":(top,literal)config", ":(top,literal)config/settings", ":(exclude,top,glob)config/settings/**"}; !slices.Equal(got[len(got)-4:], want) {
		t.Errorf("a file renamed into a folder of its name: %q", got)
	}
	got = PatchArgs(shaMain, "config/settings", "config")
	if want := []string{"--", ":(top,literal)config/settings", ":(top,literal)config", ":(exclude,top,glob)config/settings/**"}; !slices.Equal(got[len(got)-4:], want) {
		t.Errorf("a file renamed out of a folder to its name: %q", got)
	}
	page := "diff --git a/config/other b/config/other\nnew file mode 100644\n--- /dev/null\n+++ b/config/other\n@@ -0,0 +1 @@\n+x\n" +
		"diff --git a/config b/config/settings\nsimilarity index 100%\nrename from config\nrename to config/settings\n" +
		"diff --git a/config/zz b/config/zz\nnew file mode 100644\n"
	if got := OwnPatch(page, "config", "config/settings"); got != "diff --git a/config b/config/settings\nsimilarity index 100%\nrename from config\nrename to config/settings\n" {
		t.Errorf("own patch %q", got)
	}
	if got := OwnPatch("diff --git \"a/caf\\303\\251\" \"b/caf\\303\\251\"\n+x\n", "", "caf\u00e9"); got == "" {
		t.Error("a quoted path's section was not kept")
	}
	if got := PatchHeader("", "a b"); got != "diff --git a/a b b/a b" {
		t.Errorf("header %q", got)
	}
}

// A link's target is its content: a target with a newline is two lines,
// and one ending in a newline has no marker (Codex review of #109, round 3).
func TestALinksTargetIsItsLines(t *testing.T) {
	got, _ := NewFileDiff("ln", KindLink, []byte("a\nb"), 4096)
	if !strings.HasSuffix(got, "new file mode 120000\n--- /dev/null\n+++ b/ln\n@@ -0,0 +1,2 @@\n+a\n+b\n\\ No newline at end of file\n") {
		t.Fatalf("a two-line target: %q", got)
	}
	got, _ = NewFileDiff("ln", KindLink, []byte("t\n"), 4096)
	if !strings.HasSuffix(got, "@@ -0,0 +1 @@\n+t\n") {
		t.Fatalf("a target ending in a newline: %q", got)
	}
	if added, _, counted := CountNew(KindLink, []byte("a\nb")); added != 2 || !counted {
		t.Fatalf("a two-line target counts %d", added)
	}
}
