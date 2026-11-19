package main

import (
	"errors"
	"runtime"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/config"
)

// memRing is a keyring in memory.
type memRing map[string][]byte

var errNoKey = errors.New("no such secret")

func (r memRing) Set(k string, v []byte) error { r[k] = v; return nil }
func (r memRing) Get(k string) ([]byte, error) {
	if v, ok := r[k]; ok {
		return v, nil
	}
	return nil, errNoKey
}
func (r memRing) Delete(k string) error { delete(r, k); return nil }

// The sign-in holds its lock from Open to Close, also across a garbage
// collection: no second PocketGo spends the same refresh token (Codex
// review of #24, round 1).
func TestTheSignInHoldsItsLockUntilClose(t *testing.T) {
	dir := t.TempDir()
	g := newGitHubAuthWith(config.GitHubApp{Ring: memRing{}, NotFound: errNoKey}, dir)
	if err := g.Open(); err != nil && !errors.Is(err, config.ErrNoSignIn) {
		t.Fatal(err)
	}
	if g.release == nil {
		t.Fatal("the sign-in keeps no hold on its lock")
	}
	runtime.GC()
	runtime.GC()
	if _, err := config.LockSignIn(dir); !errors.Is(err, config.ErrSignInHeld) {
		t.Fatalf("another PocketGo takes the lock while this one holds it: %v", err)
	}
	g.Close()
	release, err := config.LockSignIn(dir)
	if err != nil {
		t.Fatalf("the lock after Close: %v", err)
	}
	release()
}
