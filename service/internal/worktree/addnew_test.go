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

// AddNew makes a worktree on a new branch in a folder it makes itself, and
// takes no worktree, folder or branch that is there, as one another pocketd
// made in a race (Codex review of #46, rounds 2 to 4). A folder that is no
// checkout says so as Add does.
func TestAddNewTakesNothingThatIsThere(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path, branch := checkout+"--owner-session-qlz6tr", "owner/session-qlz6tr"
	var got steps
	if err := AddNew(ctx, tools, checkout, path, branch, StepNone, got.record); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, steps{StepFolder, StepWorktree}) {
		t.Fatalf("steps %v", got)
	}
	if head, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != branch {
		t.Fatalf("worktree on %q, %v", head, err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, StepNone, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("its worktree there, to a new request: %v", err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, StepWorktree, got.record); err != nil {
		t.Fatalf("its worktree there, to its own request: %v", err)
	}
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, StepNone, got.record); !errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "the branch "+branch) {
		t.Fatalf("its branch there: %v", err)
	}
	// An empty folder another made is not the request's (round 4).
	other := checkout + "--owner-session-7rgwm3"
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, other, "owner/session-7rgwm3", StepNone, got.record); !errors.Is(err, ErrTaken) {
		t.Fatalf("an empty folder another made: %v", err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, plain, plain+"--owner-x", "owner/x", StepNone, got.record); err == nil || Unsure(err) || !strings.Contains(err.Error(), "is not a git checkout") {
		t.Fatalf("a folder that is no checkout: %v", err)
	}
}

// A try whose git made the worktree records it before any check that may
// fail after it (Codex review of #46, round 4). Its retry checks the
// worktree as it is, and never makes it again; a try that made only its
// folder is not resumed, since another folder may be in its place (round
// 5).
func TestAddNewGoesOnFromItsStep(t *testing.T) {
	ctx := context.Background()
	checkout := repo(t)
	path, branch := checkout+"--owner-session-qlz6tr", "owner/session-qlz6tr"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var got steps
	if err := AddNew(ctx, tools, checkout, path, branch, StepFolder, got.record); !errors.Is(err, ErrUnfinished) || len(got) != 0 {
		t.Fatalf("from its folder: %v, steps %v", err, got)
	}
	if entries, _ := os.ReadDir(path); len(entries) != 0 {
		t.Fatal("a folder an earlier try left was filled")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, StepNone, got.record); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, StepWorktree, got.record); err != nil {
		t.Fatalf("its worktree, as it is: %v", err)
	}
	if _, err := git(ctx, checkout, "worktree", "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	if err := AddNew(ctx, tools, checkout, path, branch, StepWorktree, got.record); err == nil {
		t.Fatal("its worktree, gone, passed")
	}
	if kind, _ := kind(ctx, path); kind != "absent" {
		t.Fatalf("its worktree, gone, was made again: %s", kind)
	}
	// A hook that locks the new worktree fails the check after git: the
	// worktree was made, and recorded as made first.
	hooked := repo(t)
	hook := filepath.Join(hooked, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ngit worktree lock \"$(pwd)\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got = nil
	err := AddNew(ctx, tools, hooked, hooked+"--owner-x", "owner/x", StepNone, got.record)
	if err == nil || !slices.Equal(got, steps{StepFolder, StepWorktree}) {
		t.Fatalf("a worktree its hook locked: %v, steps %v", err, got)
	}
	// A record that fails stops it there.
	failing := func(Step) error { return errors.New("the work database is locked") }
	if err := AddNew(ctx, tools, checkout, checkout+"--owner-y", "owner/y", StepNone, failing); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a record that failed: %v", err)
	}
	if exists, _ := hasBranch(ctx, git, checkout, "owner/y"); exists {
		t.Fatal("git ran after a record that failed")
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
