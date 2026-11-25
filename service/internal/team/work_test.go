package team

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// team_work is an agent's own session's: a profile that is no
// supervisor's and not private, working as none of the roles and as no
// work session, which is told to report to the session that started it
// (ADR-0073).
func TestOnlyAnAgentsOwnSessionMayWork(t *testing.T) {
	supervisor := &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}
	private := &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true}
	agent := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}
	clerk := &profiles.Profile{ID: "clerk", Role: profiles.RoleClerk}
	for name, tc := range map[string]struct {
		p    *profiles.Profile
		role SessionRole
		want bool
	}{
		"an agent's own session":   {agent, SessionRole{}, true},
		"a clerk's own session":    {clerk, SessionRole{}, true},
		"the supervisor":           {supervisor, SessionRole{}, false},
		"a private agent":          {private, SessionRole{}, false},
		"no profile":               {nil, SessionRole{}, false},
		"a thread's specialist":    {agent, SessionRole{Thread: true}, false},
		"a plan's lead":            {agent, SessionRole{Thread: true, Plan: true}, false},
		"a review":                 {agent, SessionRole{Thread: true, Review: true}, false},
		"a worker":                 {nil, SessionRole{Worker: true}, false},
		"a consult":                {agent, SessionRole{Consult: true}, false},
		"a gather's agent":         {agent, SessionRole{Gather: true}, false},
		"a work session":           {agent, SessionRole{Work: true}, false},
		"a supervisor in a thread": {supervisor, SessionRole{Thread: true}, false},
	} {
		if got := MayWork(tc.p, tc.role); got != tc.want {
			t.Errorf("%s: MayWork %v, want %v", name, got, tc.want)
		}
		if err := CheckWorkCaller(tc.p, tc.role); (err == nil) != tc.want {
			t.Errorf("%s: CheckWorkCaller %v", name, err)
		}
	}
	for _, tc := range []struct {
		p    *profiles.Profile
		role SessionRole
		want string
	}{
		{agent, SessionRole{Work: true}, "report the plan to the session that started you"},
		{supervisor, SessionRole{}, "delegate the work with team_delegate"},
		{nil, SessionRole{}, "started from no profile"},
		{private, SessionRole{}, "chani is private"},
		{agent, SessionRole{Thread: true}, "work where pocketd started them"},
	} {
		if err := CheckWorkCaller(tc.p, tc.role); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v %+v: %v, want %q", tc.p, tc.role, err, tc.want)
		}
	}
}

// A team_work call's task is 1 to 6000 characters and its title at most
// 120, both trimmed (ADR-0073).
func TestCheckWork(t *testing.T) {
	for name, tc := range map[string]struct {
		w    Work
		want string
	}{
		"a task":              {Work{Task: "Fix the flaky test."}, ""},
		"a task at the limit": {Work{Task: strings.Repeat("é", MaxTask)}, ""},
		"a title":             {Work{Task: "Go.", Title: strings.Repeat("t", MaxTitle)}, ""},
		"an empty task":       {Work{Task: " \n "}, "the task is empty"},
		"a long task":         {Work{Task: strings.Repeat("x", MaxTask+1)}, "more than 6000"},
		"a long title":        {Work{Task: "Go.", Title: strings.Repeat("t", MaxTitle+1)}, "more than 120"},
	} {
		err := CheckWork(tc.w)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// The repository is one the profile names, in any case; it may be left out
// only when the profile names exactly one (ADR-0073).
func TestWorkRepository(t *testing.T) {
	one := profiles.Profile{ID: "vera", Repositories: map[string]string{"frostyard/example": "/src/example"}}
	two := profiles.Profile{ID: "duncan", Repositories: map[string]string{"frostyard/nsl": "/src/nsl", "frostyard/igloo": "/src/igloo"}}
	none := profiles.Profile{ID: "moneo"}
	for name, tc := range map[string]struct {
		p                 profiles.Profile
		name, key, folder string
		refused           string
	}{
		"the only one, left out":        {p: one, key: "frostyard/example", folder: "/src/example"},
		"named":                         {p: two, name: "frostyard/igloo", key: "frostyard/igloo", folder: "/src/igloo"},
		"named in another case":         {p: two, name: "Frostyard/NSL", key: "frostyard/nsl", folder: "/src/nsl"},
		"left out of two":               {p: two, refused: "name the repository: duncan works on frostyard/igloo, frostyard/nsl"},
		"one the profile lacks":         {p: two, name: "someone/else", refused: "duncan does not work on someone/else; its repositories are frostyard/igloo, frostyard/nsl"},
		"a profile with none":           {p: none, refused: "moneo works on no repository"},
		"named, of a profile with none": {p: none, name: "frostyard/nsl", refused: "moneo works on no repository"},
	} {
		key, folder, err := WorkRepository(tc.p, tc.name)
		switch {
		case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
			t.Errorf("%s: %q %q %v, want %q", name, key, folder, err, tc.refused)
		case tc.refused == "" && (err != nil || key != tc.key || folder != tc.folder):
			t.Errorf("%s: %q %q %v, want %q %q", name, key, folder, err, tc.key, tc.folder)
		}
	}
}

// The work's title is the one given, else the task's first line; the
// session's names the agent first, within the protocol's title bound; the
// worktree is named as an owner's session worktree, from the work key
// (ADR-0073, ADR-0027).
func TestWorkTitlesAndWorktree(t *testing.T) {
	if got := WorkTitle(Work{Task: "\n Fix the flaky test.\nIt times out."}); got != "Fix the flaky test." {
		t.Errorf("from the task: %q", got)
	}
	if got := WorkTitle(Work{Task: "Go.", Title: " Ship it "}); got != "Ship it" {
		t.Errorf("given: %q", got)
	}
	if got := WorkSessionTitle("Vera", "Fix the flaky test."); got != "Vera: Fix the flaky test." {
		t.Errorf("session title %q", got)
	}
	long := WorkSessionTitle(strings.Repeat("é", 80), strings.Repeat("ü", 120))
	if len(long) > proto.MaxTitle || !utf8.ValidString(long) || !strings.HasSuffix(long, "…") {
		t.Errorf("a long session title: %d bytes, %q", len(long), long)
	}
	vera := profiles.Profile{ID: "vera"}
	path, branch := WorkWorktree(vera, "/src/example/", "Fix the flaky test.", "work:team:12:7:c1")
	wantPath, wantBranch := proto.SessionWorktree("/src/example", "vera", "Fix the flaky test.", "work:team:12:7:c1")
	if path != wantPath || branch != wantBranch || !strings.HasPrefix(path, "/src/example--vera-fix-the-flaky-test-") || !strings.HasPrefix(branch, "vera/fix-the-flaky-test-") {
		t.Errorf("worktree %q on %q", path, branch)
	}
	if again, _ := WorkWorktree(vera, "/src/example", "Fix the flaky test.", "work:team:12:7:c2"); again == path {
		t.Errorf("another call's worktree is the same: %q", again)
	}
}

// The work session's brief says where it works and where its answers go;
// an answer names its session and work, clipped; the tool's answer names
// the session, its worktree and branch; the guide says where a repository
// is changed (ADR-0073).
func TestWorkTexts(t *testing.T) {
	brief := WorkBrief(12, "frostyard/example", "/src/example--vera-x-abc123", "vera/x-abc123", "", "Fix it.\n")
	for _, want := range []string{"/src/example--vera-x-abc123", "frostyard/example", "vera/x-abc123", "session 12", "goes back to that session", "AGENTS.md", "\n\nThe task:\nFix it."} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "could not fetch") {
		t.Errorf("a fresh brief says its base is stale:\n%s", brief)
	}
	if stale := WorkBrief(12, "r/r", "/p", "b", "no origin", "Go."); !strings.Contains(stale, StaleBase("no origin")) {
		t.Errorf("a stale brief:\n%s", stale)
	}
	if got := WorkReplyText(31, "Fix the flaky test.", " Done: fixed it. "); got != "[S31 · work · Fix the flaky test.] Done: fixed it." {
		t.Errorf("answer %q", got)
	}
	if got := WorkReplyText(31, "T", ""); !strings.Contains(got, "no answer") {
		t.Errorf("an empty answer %q", got)
	}
	clipped := WorkReplyText(31, "T", strings.Repeat("a", MaxWorkReply+5))
	if !strings.Contains(clipped, "clipped the answer at 8000 characters") || strings.Count(clipped, "a") < MaxWorkReply || strings.Count(clipped, "a") > MaxWorkReply+20 {
		t.Errorf("a long answer is %d characters", utf8.RuneCountInString(clipped))
	}
	started := WorkStarted(31, "faux/faux-1", "frostyard/example", "/src/example--vera-x-abc123", "vera/x-abc123")
	for _, want := range []string{"work session 31", "faux/faux-1", "/src/example--vera-x-abc123", "vera/x-abc123", "frostyard/example", "[S31 · work"} {
		if !strings.Contains(started, want) {
			t.Errorf("answer lacks %q: %s", want, started)
		}
	}
	for _, want := range []string{"team_work", "never from your home", "AGENTS.md"} {
		if !strings.Contains(WorkGuide, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// The work guide goes to a session that may call team_work at the agent's
// home alone: one the owner opened in a checkout or a worktree keeps the
// tool, and works where the owner placed it, one opened on a repository
// whose checkout is the home too included (Codex review of #174, rounds
// 3 and 4).
func TestWorkGuideFor(t *testing.T) {
	vera := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent, Home: "/home/vera", Repositories: map[string]string{"frostyard/example": "/src/example"}}
	teg := &profiles.Profile{ID: "teg", Role: profiles.RoleAgent, Workspace: "/work"}
	home := &profiles.Profile{ID: "irulan", Role: profiles.RoleAgent, Home: "/src/notes", Repositories: map[string]string{"frostyard/notes": "/src/notes"}}
	for name, tc := range map[string]struct {
		p               *profiles.Profile
		role            SessionRole
		cwd, repository string
		want            bool
	}{
		"at its home":                   {vera, SessionRole{}, "/home/vera", "", true},
		"at its home, spelled long":     {vera, SessionRole{}, "/home/vera/./", "", true},
		"in its checkout":               {vera, SessionRole{}, "/src/example", "frostyard/example", false},
		"in a worktree":                 {vera, SessionRole{}, "/src/example--vera-fix-abc123", "frostyard/example", false},
		"in its checkout, typed":        {vera, SessionRole{}, "/src/example", "", false},
		"below its home":                {vera, SessionRole{}, "/home/vera/notes", "", false},
		"a profile with no home":        {teg, SessionRole{}, "/work", "", false},
		"a work session at home":        {vera, SessionRole{Work: true}, "/home/vera", "", false},
		"no folder":                     {vera, SessionRole{}, "", "", false},
		"no profile":                    {nil, SessionRole{}, "/home/vera", "", false},
		"at a home that is a checkout":  {home, SessionRole{}, "/src/notes", "", true},
		"on the repository at its home": {home, SessionRole{}, "/src/notes", "frostyard/notes", false},
	} {
		if got := WorkGuideFor(tc.p, tc.role, tc.cwd, tc.repository); (got == WorkGuide) != tc.want || (got != "" && got != WorkGuide) {
			t.Errorf("%s: %q, want the guide %v", name, got, tc.want)
		}
	}
}

// A start pocketd made, or may have made, and could not record says so,
// and that the caller must not call again; a start that made nothing
// says the work did not start (Codex review of #174, round 5).
func TestWorkStartOutcomeTexts(t *testing.T) {
	unrecorded := WorkUnrecorded(31, "Fix the flaky test.", "database busy")
	for _, want := range []string{"started work session 31", "Fix the flaky test.", "database busy", "Do not call team_work again", "[S31 · work"} {
		if !strings.Contains(unrecorded, want) {
			t.Errorf("unrecorded lacks %q: %s", want, unrecorded)
		}
	}
	uncertain := WorkUncertain("Fix it.", "lost; busy")
	for _, want := range []string{"cannot tell yet whether the work session", "Fix it.", "lost; busy", "Do not call team_work again", "If none comes"} {
		if !strings.Contains(uncertain, want) {
			t.Errorf("uncertain lacks %q: %s", want, uncertain)
		}
	}
	if failed := WorkNotStarted("no model"); !strings.Contains(failed, "did not start (no model)") || !strings.Contains(failed, "created no session") {
		t.Errorf("not started: %s", failed)
	}
}
