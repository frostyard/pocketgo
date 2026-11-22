package signins

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A token counts only in a success: an answer that is no 2xx, or that
// names an error beside the token, fails the flow instead of reaching the
// exchange; an error GitHub sends with a 400 is still read (Codex review
// of #123, round 2).
func TestATokenCountsOnlyInASuccess(t *testing.T) {
	for _, c := range []struct {
		body   string
		status int
	}{
		{granted, 400},
		{granted, 302},
		{granted, 404},
		{`{"access_token":"gho_x","error":"access_denied"}`, 200},
	} {
		if poll, err := parsePoll([]byte(c.body), c.status); err == nil {
			t.Errorf("parsePoll(%s, %d) took %+v", c.body, c.status, poll)
		}
	}
	if poll, err := parsePoll([]byte(pending), 400); err != nil || !poll.Pending {
		t.Fatalf("a pending answer with 400: %+v %v", poll, err)
	}
	g := newFakeGitHub(t, answer{status: 400, body: granted})
	k := &keeper{}
	f, clock, ch := flowsOn(t, g, k)
	flow, _, _ := f.Start(Copilot, "r2")
	ch.next(t)
	clock.next(t, 5*time.Second)
	if flow = waitEnded(t, f, ch, flow.ID); flow.State != proto.FlowFailed || len(k.kept) != 0 {
		t.Fatalf("a token in a 400 ended the flow %+v, kept %v", flow, k.kept)
	}
}
