package pocketdtest

import (
	"fmt"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// AddBusyTeam seeds the same reproducible scene for interactive previews
// and NSGo host tests. Everything stays inside this in-memory service.
func AddBusyTeam(s *Server) {
	model := proto.ModelRef{Provider: "fixture", ID: "builder"}
	s.AddModel(proto.Model{ModelRef: model, Name: "Builder", ThinkingLevels: []string{"off", "low", "high"}, ContextWindow: 128000})
	s.AddModel(proto.Model{ModelRef: proto.ModelRef{Provider: "fixture", ID: "scribe"}, Name: "Scribe", ThinkingLevels: []string{"off"}})
	profiles := []proto.Profile{
		{ID: "lead", Name: "Coordinator", Role: proto.RoleSupervisor, Standing: true},
		{ID: "linux", Name: "Linux engineer", Role: proto.RoleAgent},
		{ID: "web", Name: "Browser engineer", Role: proto.RoleAgent},
	}
	for _, p := range profiles {
		p.Runs = model
		p.Cwd = "/workspace"
		s.AddProfile(p)
	}
	start := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	var history []proto.Entry
	for i := range 64 {
		id := int64(4*i + 1)
		at := start.Add(time.Duration(i) * time.Minute)
		call := fmt.Sprintf("inspect-%d", i)
		history = append(history, proto.Entry{ID: id, Kind: proto.EntryUser, Text: fmt.Sprintf("Check release candidate %d.", i+1), At: at},
			proto.Entry{ID: id + 1, Kind: proto.EntryAssistant, Thinking: "Checking both target reports.", ToolCalls: []proto.ToolCall{{ID: call, Name: "read", Arguments: []byte(`{"path":"checks.md"}`)}}, At: at.Add(time.Second)},
			proto.Entry{ID: id + 2, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: call, Text: "Linux and browser checks passed."}, At: at.Add(4 * time.Second)},
			proto.Entry{ID: id + 3, Kind: proto.EntryAssistant, Text: fmt.Sprintf("Candidate %d passed both target checks.", i+1), Usage: &proto.Usage{Input: 32000, Output: 120}, At: at.Add(time.Duration(5+i%12) * time.Second)})
	}
	s.AddSession(proto.Session{ID: 10, Title: "Release supervisor", Profile: "lead", Standing: true, State: proto.SessionIdle, Cwd: "/workspace", Model: model, Thinking: "high"}, history...)
	for _, item := range []struct {
		id             int64
		profile, title string
	}{{11, "linux", "Verify Linux packaging"}, {12, "web", "Verify browser storage and session reconnection"}} {
		s.AddSession(proto.Session{ID: item.id, Title: item.title, Profile: item.profile, Cwd: "/workspace", Model: model, State: proto.SessionRunning})
	}
	threads := []proto.Thread{
		{ID: 41, Title: "Stage the next candidate", State: proto.ThreadStarting, Agent: "linux", Session: 11},
		{ID: 42, Title: "Verify browser reconnection", State: proto.ThreadOpen, Agent: "web", Session: 12},
		{ID: 43, Title: "Choose the Linux release channel", State: proto.ThreadBlocked, Agent: "linux", Session: 11},
		{ID: 44, Title: "Review the upgrade report", State: proto.ThreadDone, Agent: "web", Session: 12, Report: "The offline upgrade passed."},
	}
	for _, t := range threads {
		t.Supervisor = 10
		s.AddThread(t)
	}
	for i, q := range []string{"Publish to the preview channel?", "Include offline results in the release notes?", "Keep the previous Linux package?", "Repeat the browser reconnect test?", "Attach the comparison screenshots?"} {
		t := threads[2-i%2]
		s.AddAsk(proto.Ask{ID: int64(42 + i), Thread: t.ID, SessionID: t.Session, Question: q, Options: []string{"Yes", "Defer"}, State: proto.AskOpen})
	}
}
