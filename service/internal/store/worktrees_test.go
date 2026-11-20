package store

import (
	"errors"
	"testing"
)

func claim(path, branch, top string) SessionWorktree {
	return SessionWorktree{Path: path, Branch: branch, Top: top, Dest: "/real" + path}
}

// A session's worktree belongs to the request that claimed it first: that
// request claims it again as often as it is tried, and any other is
// refused, by its branch or its path, after a reopen too.
func TestASessionsWorktreeBelongsToOneRequest(t *testing.T) {
	s, path := open(t)
	wt := claim("/srv/chairlift--owner-session-qlz6tr", "owner/session-qlz6tr", "/srv/chairlift")
	if err := s.ClaimWorktree(ctx, "u:d1:r1", wt); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r1", wt); err != nil {
		t.Fatalf("the same request again: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "u:d2:r1", wt); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("another device's request: %v", err)
	}
	// Another spelling of the checkout names the same branch (Codex review
	// of #46, round 1).
	if err := s.ClaimWorktree(ctx, "u:d2:r1", claim("/var/srv/chairlift--owner-session-qlz6tr", wt.Branch, wt.Top)); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("the same branch at another path: %v", err)
	}
	if err := s.ClaimWorktree(ctx, "u:d2:r1", claim("/srv/chairlift--owner-session-7rgwm3", "owner/session-7rgwm3", wt.Top)); err != nil {
		t.Fatalf("another worktree: %v", err)
	}
	_ = s.Close()
	again, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if err := again.ClaimWorktree(ctx, "u:d3:other", claim(wt.Path, "owner/other", wt.Top)); !errors.Is(err, ErrWorktreeTaken) {
		t.Fatalf("its path after a reopen: %v", err)
	}
	if err := again.ClaimWorktree(ctx, "u:d1:r1", wt); err != nil {
		t.Fatalf("its own request after a reopen: %v", err)
	}
}

// A request makes one worktree at most, from one checkout: tried again
// where its checkout moved, by its path or behind a link, it is refused,
// and holds only the first (Codex review of #46, rounds 1 and 3). Its own
// git's making is recorded.
func TestARequestClaimsOneWorktree(t *testing.T) {
	s, _ := open(t)
	first := claim("/srv/a--duncan-x-qlz6tr", "duncan/x-qlz6tr", "/real/a")
	if err := s.ClaimWorktree(ctx, "u:d1:r1", first); err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]SessionWorktree{
		"another checkout": claim("/srv/b--duncan-x-qlz6tr", first.Branch, "/real/b"),
		"another branch":   claim("/srv/a--duncan-y-qlz6tr", "duncan/y-qlz6tr", first.Top),
		"a link moved":     claim(first.Path, first.Branch, "/real/elsewhere"),
		// A link above the worktree's folder moved (round 5).
		"another place": {Path: first.Path, Branch: first.Branch, Top: first.Top, Dest: "/elsewhere/a--duncan-x-qlz6tr"},
	} {
		if err := s.ClaimWorktree(ctx, "u:d1:r1", other); !errors.Is(err, ErrRequestHasWorktree) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r1", first); err != nil {
		t.Fatalf("its own worktree again: %v", err)
	}
	got, ok, err := s.WorktreeOf(ctx, "u:d1:r1")
	if err != nil || !ok || got != first {
		t.Fatalf("WorktreeOf = %+v %v %v", got, ok, err)
	}
	if err := s.MarkWorktree(ctx, "u:d1:r1", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktree(ctx, "u:d1:r1", 1); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.WorktreeOf(ctx, "u:d1:r1"); got.Made != 2 {
		t.Fatal("the making is not recorded, or went back a step")
	}
	if _, ok, err := s.WorktreeOf(ctx, "u:d1:r2"); err != nil || ok {
		t.Fatalf("a request with no claim: %v %v", ok, err)
	}
	if err := s.MarkWorktree(ctx, "u:d1:r2", 1); err == nil {
		t.Fatal("a request with no claim marked made")
	}
}

// A worktree's claim reserves its request id for the CreateSession that
// made it, so no other endpoint takes the id while the session waits;
// an id another request holds claims nothing (Codex review of #46, round
// 3).
func TestAWorktreesClaimReservesItsRequest(t *testing.T) {
	s, _ := open(t)
	if err := s.ClaimWorktree(ctx, "u:d1:r1", claim("/srv/a--owner-x", "owner/x", "/srv/a")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRequest(ctx, "u:d1:r1", "send:3"); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("another endpoint with the id: %v", err)
	}
	if result, existed, err := s.LookupRequest(ctx, "u:d1:r1", "createSession"); err != nil || !existed || result != "" {
		t.Fatalf("the create's reservation: %q %v %v", result, existed, err)
	}
	if _, err := s.CreateSession(ctx, "u:d1:r1", SessionRow{ID: 4, Title: "t", Cwd: "/srv/a--owner-x", Provider: "p", Model: "m", CreatedBy: "d1"}); err != nil {
		t.Fatal(err)
	}
	if result, _, err := s.LookupRequest(ctx, "u:d1:r1", "createSession"); err != nil || result != "4" {
		t.Fatalf("the create's record: %q %v", result, err)
	}
	if _, _, err := s.ClaimRequest(ctx, "u:d1:r2", "abort:3"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorktree(ctx, "u:d1:r2", claim("/srv/a--owner-y", "owner/y", "/srv/a")); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("an id another request holds: %v", err)
	}
	if _, ok, _ := s.WorktreeOf(ctx, "u:d1:r2"); ok {
		t.Fatal("a refused claim was kept")
	}
}
