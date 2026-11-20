package store

import (
	"errors"
	"testing"
)

// A session's worktree belongs to the request that claimed it first: that
// request claims it again as often as it is tried, and any other is
// refused, after a reopen too.
func TestASessionsWorktreeBelongsToOneRequest(t *testing.T) {
	s, path := open(t)
	const wt = "/srv/chairlift--owner-session-qlz6tr"
	if err := s.ClaimWorktree(ctx, wt, "u:d1:r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorktree(ctx, wt, "u:d1:r1"); err != nil {
		t.Fatalf("the same request again: %v", err)
	}
	if err := s.ClaimWorktree(ctx, wt, "u:d2:r1"); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("another device's request: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "/srv/chairlift--owner-session-7rgwm3", "u:d2:r1"); err != nil {
		t.Fatalf("another path: %v", err)
	}
	_ = s.Close()
	again, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if err := again.ClaimWorktree(ctx, wt, "u:d3:other"); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("after a reopen: %v", err)
	}
	if err := again.ClaimWorktree(ctx, wt, "u:d1:r1"); err != nil {
		t.Fatalf("its own request after a reopen: %v", err)
	}
}
