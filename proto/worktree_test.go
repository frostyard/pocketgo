package proto

import (
	"strings"
	"testing"
)

// A session's worktree is named from its request alone, the same on both
// sides (rule 6a): the digest is pinned, since a client that computed
// another would show the owner a folder pocketd does not make.
func TestASessionsWorktreeIsNamedFromItsRequest(t *testing.T) {
	for _, c := range []struct{ id, suffix string }{
		{"r1", "qlz6tr"},
		{"4f1c2a9e0b7d4c3a8e6f1b2c3d4e5f60", "7rgwm3"},
		{"", "4oymiq"},
	} {
		if got := WorktreeSuffix(c.id); got != c.suffix {
			t.Errorf("WorktreeSuffix(%q) = %q, want %q", c.id, got, c.suffix)
		}
	}
	path, branch := SessionWorktree("/var/home/team/projects/frostyard/chairlift/", "duncan", "  Fix CI! ", "r1")
	if path != "/var/home/team/projects/frostyard/chairlift--duncan-fix-ci-qlz6tr" || branch != "duncan/fix-ci-qlz6tr" {
		t.Errorf("SessionWorktree = %q, %q", path, branch)
	}
	// No profile: the owner's own; no words: "session".
	path, branch = SessionWorktree("/srv/chairlift", OwnerAgent, "Привет ✓", "r1")
	if path != "/srv/chairlift--owner-session-qlz6tr" || branch != "owner/session-qlz6tr" {
		t.Errorf("SessionWorktree with no words = %q, %q", path, branch)
	}
	// Another request id is another worktree, the same title or not.
	if a, _ := SessionWorktree("/srv/c", "duncan", "Fix CI", "r1"); a == func() string { p, _ := SessionWorktree("/srv/c", "duncan", "Fix CI", "r2"); return p }() {
		t.Error("two request ids name one worktree")
	}
}

// SlugWords keeps whole words of lowercase letters and digits while the
// next one and a dash fit in 40 characters, as a thread's slug always did.
func TestSlugWords(t *testing.T) {
	for title, want := range map[string]string{
		"Fix the CI on main!":               "fix-the-ci-on-main",
		"--":                                "",
		"Add v2 API, then docs":             "add-v2-api-then-docs",
		strings.Repeat("abcdefghij ", 5):    "abcdefghij-abcdefghij-abcdefghij",
		strings.Repeat("x", MaxSlugWords):   "",
		strings.Repeat("x", MaxSlugWords-1): strings.Repeat("x", MaxSlugWords-1),
		"Ünïcødé words, and ASCII ones":     "n-c-d-words-and-ascii-ones",
	} {
		if got := SlugWords(title); got != want {
			t.Errorf("SlugWords(%q) = %q, want %q", title, got, want)
		}
		if got := SlugWords(title); len(got) > MaxSlugWords {
			t.Errorf("SlugWords(%q) has %d characters", title, len(got))
		}
	}
}
