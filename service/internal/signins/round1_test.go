package signins

import (
	"strings"
	"testing"
	"time"
)

// A flow GitHub ended with an error it does not name in RFC 8628 says so
// in pocketd's words: never GitHub's description, nor a code that is no
// OAuth error code, either of which could echo the device code (Codex
// review of #123, round 1).
func TestARefusalSaysNoTextGitHubSent(t *testing.T) {
	for _, body := range []string{
		`{"error":"incorrect_device_code","error_description":"device code dev-code-secret is wrong"}`,
		`{"error":"dev-code-secret","error_description":"x"}`,
		`{"error":"Bad_dev-code-secret"}`,
	} {
		poll, err := parsePoll([]byte(body), 200)
		if err != nil || poll.Refused == "" {
			t.Fatalf("parsePoll(%s) = %+v %v", body, poll, err)
		}
		if strings.Contains(poll.Refused, "dev-code-secret") {
			t.Fatalf("the refusal %q echoes the device code", poll.Refused)
		}
	}
	if got := refusal("incorrect_device_code"); got != "GitHub ended the device flow: incorrect_device_code." {
		t.Fatalf("a known code: %q", got)
	}
}

// A slow_down widens the wait whatever it is, and a longer wait GitHub
// names is honored past the first wait's bound: the code's expiry bounds
// every wait (Codex review of #123, round 1).
func TestASlowDownAlwaysWidensTheWait(t *testing.T) {
	if got := SlowedDown(maxInterval, 0); got != maxInterval+slowDownStep {
		t.Fatalf("a slow_down at the first wait's bound: %s", got)
	}
	poll, err := parsePoll([]byte(`{"error":"slow_down","interval":400}`), 200)
	if err != nil || poll.Interval != 400*time.Second || SlowedDown(maxInterval, poll.Interval) != 400*time.Second {
		t.Fatalf("a slow_down naming 400 seconds: %+v %v", poll, err)
	}
}

// A managed user's login, with its enterprise's shortcode after an
// underscore, is a login; the length is counted in characters (Codex
// review of #123, round 1).
func TestLoginTakesManagedUsers(t *testing.T) {
	for _, login := range []string{"octocat", "octo-cat", "octo-cat_acme", "a", strings.Repeat("a", maxLogin), "Mona-Lisa_ENT1"} {
		if !Login(login) {
			t.Errorf("%q is no login", login)
		}
	}
	for _, login := range []string{"", "-a", "a-", "a--b", "a_", "_a", "a_b_c", "a b", strings.Repeat("a-", 32) + "a", strings.Repeat("a", maxLogin+1)} {
		if Login(login) {
			t.Errorf("%q is a login", login)
		}
	}
}
