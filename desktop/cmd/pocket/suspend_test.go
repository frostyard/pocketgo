package main

import (
	"slices"
	"testing"
)

// calls records what the hooks did, in order.
type calls []string

type fakeWindow struct{ log *calls }

func (w fakeWindow) Suspend() { *w.log = append(*w.log, "window keeps") }

type fakeSignIn struct{ log *calls }

func (s fakeSignIn) Suspend() { *s.log = append(*s.log, "sign-in holds") }
func (s fakeSignIn) Resume()  { *s.log = append(*s.log, "sign-in resumes") }

// On iOS the suspend saves the window first, then holds the GitHub
// sign-in's refresh; the resume lets it go on. A desktop gets no hook and
// saves as its window closes (ADR-0041).
func TestSuspendHooksByPlatform(t *testing.T) {
	var log calls
	onSuspend, onResume := suspendHooks("ios", fakeWindow{&log}, fakeSignIn{&log})
	if onSuspend == nil || onResume == nil {
		t.Fatal("iOS has no suspend hooks")
	}
	onSuspend()
	onResume()
	if want := (calls{"window keeps", "sign-in holds", "sign-in resumes"}); !slices.Equal(log, want) {
		t.Errorf("the hooks did %q, want %q", log, want)
	}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		if onSuspend, onResume := suspendHooks(goos, fakeWindow{&log}, fakeSignIn{&log}); onSuspend != nil || onResume != nil {
			t.Errorf("%s has suspend hooks", goos)
		}
	}
}
