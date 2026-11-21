package github

import (
	"context"
	"testing"
	"time"
)

// The tests of Suspend and Resume (pocketgo ADR-0041): the system suspends
// the app, as iOS does when it goes to the background, and may end it
// there. No refresh starts meanwhile, and one in progress reaches the store
// before Suspend returns, within its wait.

// While the system suspends the app no refresh starts: a lease that needs
// one waits for Resume, which starts it.
func TestNoRefreshStartsWhileSuspended(t *testing.T) {
	if got := NewTokenSource(NewOAuth(), &store{}).suspendWait; got != 3*time.Second {
		t.Errorf("the suspend wait is %v, want 3s", got)
	}
	s := newSource(t)
	s.load(s.pair(oldAccess, time.Minute))
	if !s.ts.Suspend() {
		t.Fatal("Suspend with nothing in progress says the work did not end")
	}
	leased := make(chan string, 1)
	go func() {
		tok, _, release, err := s.ts.Lease(context.Background())
		if err != nil {
			leased <- "error: " + err.Error()
			return
		}
		release()
		leased <- tok
	}()
	waitFor(t, s.ts, "the lease to find the refresh due", func() bool { return s.ts.due })
	s.ts.mu.Lock()
	refreshing := s.ts.refreshing
	s.ts.mu.Unlock()
	if refreshing || s.sign.tokenRequests() != 0 {
		t.Fatalf("a refresh started while suspended: refreshing %v, %d requests", refreshing, s.sign.tokenRequests())
	}
	s.ts.Resume()
	if tok := <-leased; tok != newAccess {
		t.Fatalf("the lease after Resume gave %q", tok)
	}
	if got := s.sign.tokenRequests(); got != 1 {
		t.Errorf("refresh requests %d, want 1", got)
	}
}

// A lease of a pair that needs no refresh goes on while suspended.
func TestALeaseNeedingNoRefreshGoesOnWhileSuspended(t *testing.T) {
	s := newSource(t)
	s.load(s.pair(oldAccess, 8*time.Hour))
	s.ts.Suspend()
	tok, _, release := s.lease(context.Background())
	release()
	if tok != oldAccess {
		t.Errorf("the lease while suspended gave %q", tok)
	}
}

// Suspend waits for a refresh already in GitHub, so the pair GitHub
// refreshed is in the store before the app stops.
func TestSuspendWaitsForTheRunningRefreshToBeSaved(t *testing.T) {
	s := newSource(t)
	s.load(s.pair(oldAccess, time.Minute))
	release, entered := s.sign.blockToken(t, 200, goodPairAnswer())
	go func() {
		if _, _, rel, err := s.ts.Lease(context.Background()); err == nil {
			rel()
		}
	}()
	<-entered // the refresh is in GitHub
	asked := make(chan time.Duration, 1)
	s.ts.closeAfter = func(d time.Duration) <-chan time.Time { asked <- d; return nil }
	suspended := make(chan bool, 1)
	go func() { suspended <- s.ts.Suspend() }()
	// Suspend asks for its timer before it waits, or returns at once
	// without waiting, which fails.
	select {
	case d := <-asked:
		if d != s.ts.suspendWait {
			t.Errorf("Suspend waits %v, want %v", d, s.ts.suspendWait)
		}
	case <-suspended:
		t.Fatal("Suspend returned with the refresh still in GitHub")
	}
	select {
	case <-suspended:
		t.Fatal("Suspend returned with the refresh still in GitHub")
	default:
	}
	release()
	if !<-suspended {
		t.Error("Suspend says the refresh did not end")
	}
	if saved, ok := s.st.get(); !ok || saved.Access != newAccess {
		t.Errorf("the store holds %+v as Suspend returns, want the refreshed pair", saved)
	}
}

// Suspend's wait has an end, which its timer gives: a refresh that takes
// longer outlasts it, and Suspend says so.
func TestSuspendWaitsNoLongerThanItsWait(t *testing.T) {
	s := newSource(t)
	s.load(s.pair(oldAccess, time.Minute))
	release, entered := s.sign.blockToken(t, 200, goodPairAnswer())
	go func() {
		if _, _, rel, err := s.ts.Lease(context.Background()); err == nil {
			rel()
		}
	}()
	<-entered
	fire := make(chan time.Time, 1)
	s.ts.closeAfter = func(time.Duration) <-chan time.Time { return fire }
	suspended := make(chan bool, 1)
	go func() { suspended <- s.ts.Suspend() }()
	fire <- time.Time{} // buffered: a Suspend that never waits does not hold the test
	if <-suspended {
		t.Error("Suspend says the work ended with the refresh still in GitHub")
	}
	release()
}
