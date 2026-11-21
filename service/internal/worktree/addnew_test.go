package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// steps records the steps AddNew reports.
type steps []Step

func (s *steps) record(step Step) error { *s = append(*s, step); return nil }

const token = "request-token"

// AddNew makes a worktree on a new branch in a folder it makes itself,
// marks it as its request's, and takes no worktree, folder or branch that
// is there, as one another pocketd made in a race (Codex review of #46,
// rounds 2 to 4). A folder that is no checkout says so as Add does.
func TestAddNewTakesNothingThatIsThere(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path, branch := checkout+"--owner-session-qlz6tr", "owner/session-qlz6tr"
	var got steps
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepNone, got.record); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, steps{StepFolder, StepGit, StepWorktree}) {
		t.Fatalf("steps %v", got)
	}
	if head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != branch {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	if err := Marked(ctx, tools, path, token); err != nil {
		t.Fatalf("its mark: %v", err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, "another", StepNone, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("its worktree there, to a new request: %v", err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepWorktree, got.record); err != nil {
		t.Fatalf("its worktree there, to its own request: %v", err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, "another", StepWorktree, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("its worktree there, to another request that says it made it: %v", err)
	}
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	// The branch is checked before anything is made (round 6).
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepNone, got.record); !errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "the branch "+branch) {
		t.Fatalf("its branch there: %v", err)
	}
	if kind, _ := kind(ctx, path); kind != "absent" {
		t.Fatalf("a branch there left a folder: %s", kind)
	}
	// An empty folder another made is not the request's (round 4).
	other := checkout + "--owner-session-7rgwm3"
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, other, "owner/session-7rgwm3", token, StepNone, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("an empty folder another made: %v", err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, plain, plain+"--owner-x", "owner/x", token, StepNone, got.record); err == nil || Unsure(err) || !strings.Contains(err.Error(), "is not a git checkout") {
		t.Fatalf("a folder that is no checkout: %v", err)
	}
}

// A try whose git made the worktree records it before any check that may
// fail after it (Codex review of #46, round 4). Its retry checks the
// worktree as it is, by its mark, and never makes it again: one made again
// at its place on its branch since is not the request's (rounds 5 and 6).
// A try that made only its folder is not resumed, since another folder may
// be in its place (round 5).
func TestAddNewGoesOnFromItsStep(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path, branch := checkout+"--owner-session-qlz6tr", "owner/session-qlz6tr"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var got steps
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepFolder, got.record); !errors.Is(err, ErrUnfinished) || len(got) != 0 || strings.Contains(err.Error(), "remove it") || !strings.Contains(err.Error(), "change the title") {
		t.Fatalf("from its folder: %v, steps %v", err, got)
	}
	if entries, _ := os.ReadDir(path); len(entries) != 0 {
		t.Fatal("a folder an earlier try left was filled")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepNone, got.record); err != nil {
		t.Fatal(err)
	}
	// Made again at its place, on its branch: no mark.
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, checkout, "worktree", "add", path, branch); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepWorktree, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("a worktree made again at its place: %v", err)
	}
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, token, StepWorktree, got.record); err == nil {
		t.Fatal("its worktree, gone, passed")
	}
	if kind, _ := kind(ctx, path); kind != "absent" {
		t.Fatalf("its worktree, gone, was made again: %s", kind)
	}
	// From StepGit, a worktree with no mark may be one made again there
	// since: it is not taken, nor marked (round 7). One whose mark the
	// try made before it stopped goes on.
	gitOnly := checkout + "--owner-g"
	if _, err := git(ctx, checkout, "worktree", "add", "-b", "owner/g", gitOnly, "HEAD"); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := AddNew(ctx, tools, checkout, gitOnly, "owner/g", token, StepGit, got.record); !errors.Is(err, ErrTaken) || !slices.Equal(got, steps{StepRefused}) {
		t.Fatalf("from StepGit, unmarked: %v, steps %v", err, got)
	}
	if err := Marked(ctx, tools, gitOnly, token); !errors.Is(err, ErrTaken) {
		t.Fatalf("an unmarked worktree was marked: %v", err)
	}
	// Refused, it stays refused, whatever is marked there since (round 8).
	if err := mark(ctx, tools, gitOnly, token); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, gitOnly, "owner/g", token, StepRefused, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("from StepRefused, marked since: %v", err)
	}
	got = nil
	if err := AddNew(ctx, tools, checkout, gitOnly, "owner/g", token, StepGit, got.record); err != nil || !slices.Equal(got, steps{StepWorktree}) {
		t.Fatalf("from StepGit, marked: %v, steps %v", err, got)
	}
	// A mark is made once: an entry of any kind there already is not this
	// try's (round 7; a named pipe in addnew_unix_test.go).
	if err := mark(ctx, tools, gitOnly, token); !errors.Is(err, ErrTaken) {
		t.Fatalf("a mark made again: %v", err)
	}
	// A hook that locks the new worktree fails the check after git: the
	// worktree was made and marked, and recorded so first.
	hooked := repo(t)
	hook := filepath.Join(hooked, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ngit worktree lock \"$(pwd)\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got = nil
	err := AddNew(ctx, tools, hooked, hooked+"--owner-x", "owner/x", token, StepNone, got.record)
	if err == nil || !slices.Equal(got, steps{StepFolder, StepGit, StepWorktree}) {
		t.Fatalf("a worktree its hook locked: %v, steps %v", err, got)
	}
	// A record that fails stops it there.
	failing := func(Step) error { return errors.New("the work database is locked") }
	if err := AddNew(ctx, tools, checkout, checkout+"--owner-y", "owner/y", token, StepNone, failing); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a record that failed: %v", err)
	}
	if exists, _ := hasBranch(ctx, git, checkout, "owner/y"); exists {
		t.Fatal("git ran after a record that failed")
	}
}

// A mark that was there before the request marked its new worktree, as a
// hook may leave, refuses the worktree for good: the refusal is recorded,
// so a retry never finds the mark and takes it (Codex review of #46, round
// 8).
func TestAMarkThereBeforeRefusesForGood(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	hook := filepath.Join(checkout, ".git", "hooks", "post-checkout")
	script := "#!/bin/sh\nmkdir \"$(git rev-parse --absolute-git-dir)/" + MarkPrefix + token + "\"\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var got steps
	path, branch := checkout+"--owner-m", "owner/m"
	err := AddNew(ctx, tools, checkout, path, branch, token, StepNone, got.record)
	if !errors.Is(err, ErrTaken) || !slices.Equal(got, steps{StepFolder, StepGit, StepRefused}) {
		t.Fatalf("a mark there before: %v, steps %v", err, got)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, token, got[len(got)-1], got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("its retry: %v", err)
	}
}

// git that made the branch and failed after it, in a hook, says why
// itself: it is no race lost (Codex review of #46, round 6).
func TestAddNewKeepsGitsOwnFailure(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	hook := filepath.Join(checkout, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nrm -f .git\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var got steps
	err := AddNew(ctx, tools, checkout, checkout+"--owner-x", "owner/x", token, StepNone, got.record)
	if err == nil || errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "git could not make the worktree") {
		t.Fatalf("a hook that failed: %v", err)
	}
	// Its branch is left: no retry takes the name, and git's word stays
	// (round 7).
	if !errors.Is(err, ErrBranchLeft) || !strings.Contains(err.Error(), "change the title") {
		t.Fatalf("a hook that failed, its branch left: %v", err)
	}
}

// Top names the checkout as git does, through a link to a folder above it
// too, so a link moved since names another top (Codex review of #46,
// round 3).
func TestTopResolvesLinks(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(checkout), alias); err != nil {
		t.Fatal(err)
	}
	real, err := Top(ctx, tools, checkout)
	if err != nil {
		t.Fatal(err)
	}
	through, err := Top(ctx, tools, filepath.Join(alias, filepath.Base(checkout)))
	if err != nil || through != real {
		t.Fatalf("Top through a link = %q, %v; want %q", through, err, real)
	}
	other := repo(t)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(other), alias); err != nil {
		t.Fatal(err)
	}
	if moved, err := Top(ctx, tools, filepath.Join(alias, filepath.Base(other))); err != nil || moved == real {
		t.Fatalf("Top after the link moved = %q, %v", moved, err)
	}
}
