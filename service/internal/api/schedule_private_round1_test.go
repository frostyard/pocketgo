package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The schedule tools' upgrade reads a profile it cannot read once a pass,
// however many sessions the profile has, and says so once: serving waits
// for the start's pass, and a pass runs again every minute while one
// failed. A later pass reads it afresh (Codex review of #120, round 1).
func TestTheScheduleToolsPassReadsAnUnreadableProfileOnce(t *testing.T) {
	h := newScheduleHarness(t, "", true)
	for name, text := range map[string]string{"chani.md": chaniProfile, "leto.md": leto} {
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, profile := range []string{"chani", "leto"} {
		for _, n := range []string{"1", "2", "3"} {
			if _, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: profile + n, Profile: profile}); err != nil {
				t.Fatal(err)
			}
		}
	}
	flaky := &unreadableProfiles{Folder: profiles.Folder{Dir: h.dir, Home: h.home}, fail: map[string]bool{"chani": true, "leto": true}}
	for name, upgrade := range map[string]func(context.Context) error{
		"the private agents' schedule": h.server.upgradePrivateSchedules,
		"the supervisors' later tools": h.server.upgradeSupervisorTools,
	} {
		counting := &countingProfiles{ProfileSource: flaky, finds: map[string]int{}}
		h.server.SetProfiles(counting)
		h.server.scheduleMu.Lock()
		err := upgrade(ctx)
		h.server.scheduleMu.Unlock()
		if err == nil {
			t.Fatalf("%s: an upgrade with unreadable profiles succeeded", name)
		}
		for _, profile := range []string{"chani", "leto"} {
			if got := counting.finds[profile]; got != 1 {
				t.Errorf("%s: %s read %d times, want once", name, profile, got)
			}
			if got := strings.Count(err.Error(), "profile "+profile+":"); got != 1 {
				t.Errorf("%s: %s's failure said %d times: %v", name, profile, got, err)
			}
		}
	}
	// The next pass reads them afresh, and upgrades once they read.
	flaky.mu.Lock()
	flaky.fail = nil
	flaky.mu.Unlock()
	h.server.SetProfiles(flaky)
	h.server.scheduleMu.Lock()
	err := h.server.upgradeScheduleTools(ctx)
	h.server.scheduleMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.ownScheduleTool) != 3 || len(h.engine.scheduleTool) != 3 {
		t.Fatalf("given schedule %v, the supervisors' tools %v", h.engine.ownScheduleTool, h.engine.scheduleTool)
	}
}
