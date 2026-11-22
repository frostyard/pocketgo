package signins

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A request to GitHub that fails says so in pocketd's words: Go's error can
// quote what GitHub sent, as a redirect's Location with the device code,
// and a flow's error goes to every client, at the start and after the
// third poll that fails (Codex review of #123, round 3).
func TestAFailedRequestQuotesNothingGitHubSent(t *testing.T) {
	quoted := answer{err: errors.New(`parse "https://evil.example/?code=dev-code-secret": invalid redirect`)}
	g := newFakeGitHub(t)
	g.code = quoted
	f, _, ch := flowsOn(t, g, &keeper{})
	flow, _, _ := f.Start(Copilot, "r3a")
	ch.next(t)
	if flow.State != proto.FlowFailed || strings.Contains(flow.Error, "dev-code-secret") || strings.Contains(flow.Error, "evil") {
		t.Fatalf("a failed start says %q", flow.Error)
	}

	g2 := newFakeGitHub(t, quoted)
	f2, clock, ch2 := flowsOn(t, g2, &keeper{})
	flow, _, _ = f2.Start(Copilot, "r3b")
	ch2.next(t)
	for range maxFailures {
		clock.next(t, 5*time.Second)
	}
	flow = waitEnded(t, f2, ch2, flow.ID)
	if flow.State != proto.FlowFailed || strings.Contains(flow.Error, "dev-code-secret") || !strings.Contains(flow.Error, "could not be reached") {
		t.Fatalf("polls that failed say %q", flow.Error)
	}
}
