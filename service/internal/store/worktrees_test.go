package store

import (
	"errors"
	"testing"
)

// A session's worktree belongs to the request that claimed it first: that
// request claims it again as often as it is tried, and any other is
// refused, by its branch or its path, after a reopen too.
func TestASessionsWorktreeBelongsToOneRequest(t *testing.T) {
	s, path := open(t)
	const wt, branch = "/srv/chairlift--owner-session-qlz6tr", "owner/session-qlz6tr"
	if err := s.ClaimWorktree(ctx, "u:d1:r1", wt, branch); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r1", wt, branch); err != nil {
		t.Fatalf("the same request again: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "u:d2:r1", wt, branch); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("another device's request: %v", err)
	}
	// Another spelling of the checkout names the same branch (Codex review
	// of #46, round 1).
	if err := s.ClaimWorktree(ctx, "u:d2:r1", "/var/srv/chairlift--owner-session-qlz6tr", branch); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("the same branch at another path: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "u:d2:r1", "/srv/chairlift--owner-session-7rgwm3", "owner/session-7rgwm3"); err != nil {
		t.Fatalf("another worktree: %v", err)
	}
	_ = s.Close()
	again, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if err := again.ClaimWorktree(ctx, "u:d3:other", wt, "owner/other"); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("its path after a reopen: %v", err)
	}
	if err := again.ClaimWorktree(ctx, "u:d1:r1", wt, branch); err != nil {
		t.Fatalf("its own request after a reopen: %v", err)
	}
}

// A request makes one worktree at most: tried again where its checkout
// moved, it is refused, and holds only the first (Codex review of #46,
// round 1).
func TestARequestClaimsOneWorktree(t *testing.T) {
	s, _ := open(t)
	if err := s.ClaimWorktree(ctx, "u:d1:r1", "/srv/a--duncan-x-qlz6tr", "duncan/x-qlz6tr"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r1", "/srv/b--duncan-x-qlz6tr", "duncan/x-qlz6tr"); !errors.Is(err, ErrRequestHasWorktree) {
		t.Fatalf("the request at another checkout: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r1", "/srv/a--duncan-y-qlz6tr", "duncan/y-qlz6tr"); !errors.Is(err, ErrRequestHasWorktree) {
		t.Fatalf("the request with another branch: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r1", "/srv/a--duncan-x-qlz6tr", "duncan/x-qlz6tr"); err != nil {
		t.Fatalf("its own worktree again: %v", err)
	}
}
