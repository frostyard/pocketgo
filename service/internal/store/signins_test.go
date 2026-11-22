package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The account of a sign-in is kept with its credential's identity,
// replaced by the next, and forgotten by a sign-out; it is migration 24
// (ADR-0062).
func TestASignInsAccountIsKeptWithItsCredentialsIdentity(t *testing.T) {
	if SchemaVersion < 24 || !strings.Contains(migrations[23], "signin_accounts") {
		t.Fatalf("schema version %d: the sign-in accounts are not migration 24", SchemaVersion)
	}
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "w.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, _, ok, err := s.SignInAccount(ctx, "github-copilot"); ok || err != nil {
		t.Fatalf("a new database keeps an account: %v %v", ok, err)
	}
	if err := s.KeepSignInAccount(ctx, "github-copilot", "id1", "octocat"); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepSignInAccount(ctx, "github-copilot", "id2", "hubot"); err != nil {
		t.Fatal(err)
	}
	identity, account, ok, err := s.SignInAccount(ctx, "github-copilot")
	if err != nil || !ok || identity != "id2" || account != "hubot" {
		t.Fatalf("kept %q %q %v %v, want the second", identity, account, ok, err)
	}
	if err := s.ForgetSignInAccount(ctx, "github-copilot"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.SignInAccount(ctx, "github-copilot"); ok || err != nil {
		t.Fatalf("a sign-out keeps the account: %v %v", ok, err)
	}
}
