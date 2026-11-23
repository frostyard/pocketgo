package app

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/proto"
)

var (
	opusRef     = proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	deepseekRef = proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4"}
)

// seedUsage gives the fake pocketd the team (seedTeam), the models'
// context windows, what the threads' sessions used, an owner's session on
// DeepSeek and a standing session near its compactAt, each with a measured
// answer, and the days' usage (ADR-0052).
func (x *testApp) seedUsage() {
	x.seedTeam()
	x.srv.SetModels([]proto.Model{
		{ModelRef: opusRef, Name: "Claude Opus 5.5", Reasoning: true, ThinkingLevels: []string{"off", "high"}, ContextWindow: 200_000},
		{ModelRef: deepseekRef, Name: "DeepSeek V4", ThinkingLevels: []string{"off"}, ContextWindow: 128_000},
	})
	x.srv.AddProfile(proto.Profile{ID: "chani", Name: "Chani", Role: "supervisor", Home: "/home/team/agents/chani", Standing: true, CompactAt: 150_000})
	// T1 on a subscription costs nothing; T2's session and its worker on
	// DeepSeek do.
	x.srv.AddSession(proto.Session{ID: 11, Title: "T1 · Fix the flaky arm64 job", Cwd: "/home/team/projects/nsl", Model: opusRef,
		Profile: "vera", Route: "profile", UpdatedAt: at(-time.Minute), Usage: proto.Usage{Input: 5_678, Output: 40_000, CacheRead: 300_000, Total: 345_678}})
	x.srv.AddSession(proto.Session{ID: 12, Title: "T2 · Pin the runner image", Cwd: "/home/team/projects/nsl", Model: deepseekRef,
		Profile: "vera", Route: "profile", UpdatedAt: at(-2 * time.Minute), Usage: proto.Usage{Total: 900_000, Cost: 1.234}})
	x.srv.AddSession(proto.Session{ID: 30, Title: "T2 worker", Cwd: "/home/team/projects/nsl", Model: deepseekRef, Route: "worker",
		StartedBy: 12, UpdatedAt: at(-3 * time.Minute), Usage: proto.Usage{Total: 400_000, Cost: 0.5}})
	x.srv.AddSession(proto.Session{ID: 21, Title: "Port the usage meter", Cwd: "/home/team/projects/pocketgo", Model: deepseekRef,
		State: proto.SessionRunning, UpdatedAt: at(-30 * time.Second),
		Usage: proto.Usage{Input: 40_000, Output: 9_000, CacheRead: 600_000, CacheWrite: 30_000, Total: 679_000, Cost: 0.42}},
		proto.Entry{ID: 1, SessionID: 21, Kind: proto.EntryUser, Text: "Show each session's context fill in its header.", At: at(-3 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 21, Kind: proto.EntryAssistant, Text: "The header now shows the meter; the tests pass.", Model: &deepseekRef,
			Usage: &proto.Usage{Input: 2_100, Output: 900, CacheRead: 74_000, CacheWrite: 1_200, Total: 78_200, Cost: 0.0123}, At: at(-time.Minute)})
	x.srv.AddSession(proto.Session{ID: 22, Title: "Chani", Cwd: "/home/team/agents/chani", Model: opusRef, Profile: "chani", Route: "profile",
		Standing: true, UpdatedAt: at(-4 * time.Minute), Usage: proto.Usage{Input: 90_000, Output: 120_000, CacheRead: 2_100_000, Total: 2_310_000}},
		proto.Entry{ID: 1, SessionID: 22, Kind: proto.EntryUser, Text: "What is on the desk today?", At: at(-6 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 22, Kind: proto.EntryAssistant, Text: "Five threads: two wait for you.", Model: &opusRef,
			Usage: &proto.Usage{Input: 3_000, Output: 1_000, CacheRead: 124_000, Total: 128_000}, At: at(-5 * time.Minute)})
	x.srv.SetUsage(proto.UsageReport{Zone: "America/New_York", Today: "2026-11-18", Days: []proto.UsageDay{
		{Day: "2026-11-12", Models: []proto.ModelUsage{
			{Model: opusRef, Usage: proto.Usage{Total: 3_100_000}},
			{Model: deepseekRef, Usage: proto.Usage{Total: 900_000, Cost: 2.1}},
		}},
		{Day: "2026-11-18", Models: []proto.ModelUsage{
			{Model: opusRef, Usage: proto.Usage{Total: 1_000_000}},
			{Model: deepseekRef, Usage: proto.Usage{Total: 240_000, Cost: 0.84}},
			{Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-5.5"}, Usage: proto.Usage{Total: 120_000}},
		}},
	}})
}

// The context mark below the composer shows the fill and exact tooltip;
// the header keeps the session's tokens and cost, and the status bar shows
// today's total and the week's (ADR-0052, ADR-0067).
func TestTheUsageMeterSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedUsage()
	x.connect(t)
	x.until(t, func() bool { _, _, shown := x.a.st.UsageTotals(); return shown })
	x.a.openSession(21)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 21 })
	x.hasAll(t, "Port the usage meter", "61%", "679k tokens · $0.42", "Today 1.4M tokens · $0.84", "7 days 5.4M tokens · $2.94")
	meter, ok := x.tt.AccessByRole(ui.RoleMeter, "Context")
	if !ok || !strings.HasPrefix(meter.Description, "Context: 78,200 tokens of 128,000 (61%)") {
		t.Errorf("the meter %+v, found %v", meter, ok)
	}
	field, found := x.tt.Access("Message")
	if !found || meter.Bounds.Y <= field.Bounds.Y+field.Bounds.H {
		t.Errorf("context meter %+v is not below Message %+v (found %v)", meter.Bounds, field.Bounds, found)
	}
	x.snapshot(t, "usage-1-session")

	// A standing session near its compactAt: tokens alone, as its model
	// costs nothing.
	x.a.openSession(22)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 22 })
	x.hasAll(t, "64%", "2.3M tokens")
	if x.tt.HasText("2.3M tokens · $0.00") {
		t.Error("a session that cost nothing shows its cost")
	}
	if meter, ok := x.tt.AccessByRole(ui.RoleMeter, "Context"); !ok || !strings.HasSuffix(meter.Description, "pocketd compacts this session at 150,000 tokens.") {
		t.Errorf("the standing session's meter %+v, found %v", meter, ok)
	}
	x.snapshot(t, "usage-2-standing")

	// The board's cards say what their threads used: T1 its tokens, T2
	// the cost of its session and its worker.
	x.click(t, "Work Board")
	x.hasAll(t, "Vera · frostyard/nsl on vera/fix-the-flaky-arm64-job-t1 · 346k tokens", "Vera · frostyard/nsl on vera/pin-the-runner-image-t2 · $1.73")
	x.snapshot(t, "usage-3-board")
}

// More on a phone shows today's total and the week's, by provider
// (ADR-0052).
func TestThePhonesUsageSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.srv.SetUsage(proto.UsageReport{Zone: "America/New_York", Today: "2026-11-18", Days: []proto.UsageDay{
		{Day: "2026-11-12", Models: []proto.ModelUsage{{Model: opusRef, Usage: proto.Usage{Total: 3_100_000}}, {Model: deepseekRef, Usage: proto.Usage{Total: 900_000, Cost: 2.1}}}},
		{Day: "2026-11-18", Models: []proto.ModelUsage{{Model: opusRef, Usage: proto.Usage{Total: 1_000_000}}, {Model: deepseekRef, Usage: proto.Usage{Total: 240_000, Cost: 0.84}}}},
	}})
	x.phoneApp(t, true, nil)
	x.tap(t, "More")
	x.hasAll(t, "Usage", "Today", "1.2M tokens · $0.84", "anthropic: 1.0M tokens", "deepseek: 240k tokens · $0.84",
		"Last 7 days", "5.2M tokens · $2.94", "anthropic: 4.1M tokens", "deepseek: 1.1M tokens · $2.94", "Appearance")
	x.snapshot(t, "phone-7-usage")
}
