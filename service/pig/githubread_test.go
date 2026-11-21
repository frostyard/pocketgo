package pig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/github"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// githubReadThroughPiG is what a gather's proposal session gets from a
// team_github call with args, which the team answers with text, and the
// calls the team saw.
func githubReadThroughPiG(t *testing.T, args map[string]any, text string) (string, *fakeTeam) {
	t.Helper()
	f := newFixture(t)
	e := f.open(fauxModels(toolTurn(toolGitHub, args, "r1"), answer("ok")))
	defer func() { _ = e.Close(context.Background()) }()
	fake := &fakeTeam{readResult: text}
	e.SetTeam(fake)
	ctx := context.Background()
	id, err := e.CreateSession(ctx, "ask", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}, Gather: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "read", "read"); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitIdle(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) > 0 && (fake.calls[0].Session != id || fake.calls[0].CallID != "r1" || fake.calls[0].Task == 0) {
		t.Fatalf("calls %+v", fake.calls)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ToolResult != nil {
			return entry.ToolResult.Text, fake
		}
	}
	t.Fatal("no tool result")
	return "", fake
}

// A proposal session's team_github call reaches the team with its path
// and query, a number among them as text, keyed by the calling session,
// its task and the call's id (ADR-0036).
func TestTeamGitHubCallsTheTeam(t *testing.T) {
	got, fake := githubReadThroughPiG(t, map[string]any{"path": "repos/frostyard/example/issues", "query": map[string]any{"state": "open", "per_page": 30}}, `[{"number":7}]`)
	if got != `[{"number":7}]` {
		t.Fatalf("the answer %q", got)
	}
	if len(fake.reads) != 1 || fake.reads[0].Path != "repos/frostyard/example/issues" || fake.reads[0].Query["state"] != "open" || fake.reads[0].Query["per_page"] != "30" {
		t.Fatalf("reads %+v", fake.reads)
	}
}

// team_github's longest answer, its note included, reaches the agent
// whole: compacted JSON on one line, which PiG drops whole past its byte
// limit, or text that is all line ends (ADR-0036).
func TestAGitHubAnswerReachesTheAgentWhole(t *testing.T) {
	wide, err := team.GitHubAnswer(`{"body":"` + strings.Repeat("\U0001F600", team.MaxGitHubAnswer) + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	tall, err := team.GitHubAnswer("x" + strings.Repeat("\n", team.MaxGitHubAnswer*2) + "x")
	if err != nil {
		t.Fatal(err)
	}
	var items []string
	for i := range 2000 {
		items = append(items, fmt.Sprintf(`{"number":%d,"title":%q}`, i, strings.Repeat("é", 40)))
	}
	array, err := team.GitHubAnswer("[" + strings.Join(items, ",") + "]")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"wide": wide, "tall": tall, "array": array} {
		if !strings.Contains(text, "\n[team_github: the first") || len(text) < team.MaxGitHubAnswer-4 {
			t.Fatalf("%s: the answer is not a full cut one: %d bytes", name, len(text))
		}
		if got, _ := githubReadThroughPiG(t, map[string]any{"path": "repos/frostyard/example/issues"}, text); got != text {
			t.Errorf("%s: the answer came through with %d of its %d bytes, %d of its %d lines", name, len(got), len(text), strings.Count(got, "\n")+1, strings.Count(text, "\n")+1)
		}
	}
}

// gh is stopped at the first byte past its output bound, and says why: it
// does not go on downloading an answer pocketd refuses already (Codex
// review of #61, round 1). The fake gh prints past the bound, then would
// wait and leave a mark; it never gets there.
func TestGHStopsAtItsOutputBound(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	marker := filepath.Join(dir, "went-on")
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo tok-123; exit 0; fi\n" +
		fmt.Sprintf("head -c %d /dev/zero\nsleep 30\ntouch '%s'\n", ghOutput+1, marker)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Unsandboxed: true, GH: filepath.Join(dir, "gh"), DataDir: data}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	if _, err := e.GH(context.Background(), "api", "repos/frostyard/example/tarball"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("printed more than %d bytes", ghOutput)) {
		t.Fatalf("an answer past the bound: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("gh went on past its output bound")
	}
	// An answer at the bound comes whole.
	exact := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo tok-123; exit 0; fi\n" + fmt.Sprintf("head -c %d /dev/zero\n", ghOutput)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(exact), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := e.GH(context.Background(), "api", "x"); err != nil || len(out) != ghOutput {
		t.Fatalf("an answer at the bound: %d bytes, %v", len(out), err)
	}
}

// A plan's lead started before team_github gets it, in the place a new
// lead has it, once; a consult, a thread's specialist and a supervisor
// are left as they are (ADR-0036).
func TestSetGitHubToolUpgradesAnOlderPlanLead(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels())
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		spec api.SessionSpec
		want []string
	}{
		"a plan's lead": {api.SessionSpec{Profile: &profiles.Profile{ID: "duncan", Role: profiles.RoleAgent}, Thread: 4, Plan: 1},
			[]string{"read", toolMessage, toolPlan, toolGitHub, toolMemory}},
		"a supervisor's plan lead": {api.SessionSpec{Profile: &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}, Thread: 5, Plan: 2},
			[]string{"read", toolDelegate, toolMessage, toolClose, toolPlan, toolSchedule, toolSend, toolGather, toolBoard, toolLaunch, toolGitHub, toolMemory}},
	} {
		id, err := e.CreateSession(ctx, name, api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: tc.spec.Profile, Thread: tc.spec.Thread, Plan: tc.spec.Plan})
		if err != nil {
			t.Fatal(err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: a new session's tools %q, want %q", name, got, tc.want)
		}
		// As a session made before team_github was.
		conversation, err := e.conversation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		agent, err := conversation.Agent(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var older []*durable.ToolRegistration
		for _, tool := range agent.Tools {
			if tool.Name != toolGitHub {
				older = append(older, tool)
			}
		}
		if err := conversation.Configure(ctx, harness.AgentChange{Tools: harness.SetTo(harness.ToolChange{List: older})}); err != nil {
			t.Fatal(err)
		}
		if changed, err := e.SetGitHubTool(ctx, id); err != nil || !changed {
			t.Fatalf("%s: the upgrade: %v %v", name, changed, err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: tools %q, want %q", name, got, tc.want)
		}
		if changed, err := e.SetGitHubTool(ctx, id); err != nil || changed {
			t.Fatalf("%s: again: %v %v", name, changed, err)
		}
	}
	for name, spec := range map[string]api.SessionSpec{
		"a consult":             {Profile: &profiles.Profile{ID: "irulan", Role: profiles.RoleAgent}, Consult: 3},
		"a thread's specialist": {Profile: &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}, Thread: 6},
		"a supervisor":          {Profile: &profiles.Profile{ID: "leto", Role: profiles.RoleSupervisor}},
	} {
		spec.Cwd, spec.Model = f.work, fauxRef
		id, err := e.CreateSession(ctx, name, spec)
		if err != nil {
			t.Fatal(err)
		}
		before := agentTools(t, e, id)
		if changed, err := e.SetGitHubTool(ctx, id); err != nil || changed {
			t.Fatalf("%s: %v %v", name, changed, err)
		}
		if got := agentTools(t, e, id); !slices.Equal(got, before) {
			t.Fatalf("%s: tools %q, were %q", name, got, before)
		}
	}
}

// team_github's read runs gh through Engine.GH as pocketd's own pull
// request reads do: the trusted program, pocketd's empty configuration
// folder, the host github.com and the service user's token, with the
// arguments github.Get built, GET alone (ADR-0016, ADR-0036).
func TestAGitHubReadRunsThroughTheEnginesGH(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo tok-123; exit 0; fi\n" +
		"printf '{\"config\": \"%s\", \"host\": \"%s\", \"token\": \"%s\", \"args\": \"%s\"}\\n' \"$GH_CONFIG_DIR\" \"$GH_HOST\" \"$GH_TOKEN\" \"$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{opts: Options{Unsandboxed: true, GH: filepath.Join(dir, "gh"), DataDir: data}, commands: map[string]string{"PATH": dir + ":/usr/bin:/bin"}}
	out, err := github.Get(context.Background(), e.GH, "repos/frostyard/example/issues", map[string]string{"state": "open"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := team.GitHubAnswer(out)
	want := `{"config":"` + data + `","host":"github.com","token":"tok-123","args":"api --hostname github.com --method GET -- repos/frostyard/example/issues?state=open"}`
	if err != nil || answer != want {
		t.Fatalf("the answer %q, %v; want %q", answer, err, want)
	}
}
