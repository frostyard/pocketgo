package app

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// New Session on a standing profile says so in its profile row, asks for
// no folder, model or title, and opens the profile's one session: the
// first time it is made, and every time after that one opens again
// (ADR-0025).
func TestNewSessionOpensTheStandingSessionSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Model: opus, Runs: opus,
		Home: "/home/team/agents/odrade", Cwd: "/home/team/agents/odrade", Standing: true, CompactAt: 300000})
	x.connect(t)
	opened := func() proto.Session {
		x.click(t, "New Session")
		x.a.newSession.profile = "Odrade · supervisor · standing session"
		x.settle()
		x.hasAll(t, "Odrade keeps one standing session: this opens it, with its own folder, model and title. Start Over in it gives it a fresh context.", "Open Session")
		if x.tt.HasText("Working directory") || x.tt.HasText("Title (optional)") {
			t.Errorf("a standing session's form asks for a folder or a title: %q", x.tt.Texts())
		}
		x.snapshot(t, "new-session-standing")
		x.click(t, "Open Session")
		x.until(t, func() bool {
			return x.a.place == placeSession && x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded && (x.a.newSession == nil || !x.a.newSession.open)
		})
		s, ok := x.a.st.Data.Session(x.a.st.Data.OpenID())
		if !ok {
			t.Fatal("no session opened")
		}
		return s
	}
	first := opened()
	if !first.Standing || first.Title != "Odrade" || first.Profile != "odrade" {
		t.Fatalf("opened %+v", first)
	}
	if again := opened(); again.ID != first.ID {
		t.Fatalf("New Session opened %d, then %d", first.ID, again.ID)
	}
	for _, r := range x.srv.RequestsTo(http.MethodPost, proto.PathSessions) {
		body := string(r.Body)
		if !strings.Contains(body, `"profile":"odrade"`) || strings.Contains(body, `"cwd"`) || strings.Contains(body, `"model"`) {
			t.Errorf("a standing session's request %s", r.Body)
		}
	}
}

// A standing session's transcript shows a compaction as a folded row with
// its summary, and Start Over asks first; once confirmed, the reset shows
// where the agent's context starts again.
func TestStartOverAsksFirstSnapshot(t *testing.T) {
	x := newTestApp(t)
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 9, Title: "Odrade", Cwd: "/home/team/agents/odrade", Model: opus, Profile: "odrade", Route: "profile",
		Standing: true, State: proto.SessionIdle, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 9, Kind: proto.EntryUser, Text: "Plan this week's releases.", At: at(-20 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 9, Kind: proto.EntryCompaction, Text: "## Goal\nPlan this week's releases.\n\n## Progress\n- [x] nsl 1.4 is out", At: at(-10 * time.Minute)},
		proto.Entry{ID: 3, SessionID: 9, Kind: proto.EntryAssistant, Text: "snow 2.1 waits for its CI.", At: at(-5 * time.Minute)},
	)
	x.connect(t)
	x.click(t, "Odrade, idle")
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.hasAll(t, "Compacted the context", "Goal")
	x.snapshot(t, "session-standing-compacted")
	x.click(t, "Start this session over")
	x.hasAll(t, "Start Odrade over?")
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathStartOver, 9))); n != 0 {
		t.Fatalf("%d Start Overs went before the owner confirmed", n)
	}
	x.click(t, "Start Over")
	x.until(t, func() bool { return x.tt.HasText("Started over: the agent's context begins here") })
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathStartOver, 9))); n != 1 {
		t.Fatalf("%d Start Overs", n)
	}
}

// A profile that comes to keep a standing session while New Session is
// open changes what the form asks and sends, though nothing else of it
// changed (Codex review of #43, round 1).
func TestAProfileThatBecomesStandingChangesTheForm(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	odrade := proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Model: opus, Runs: opus,
		Home: "/home/team/agents/odrade", Cwd: "/home/team/agents/odrade"}
	x.srv.AddProfile(odrade)
	x.connect(t)
	x.click(t, "New Session")
	x.a.newSession.profile = "Odrade · supervisor"
	x.settle()
	x.hasAll(t, "Start Session", "Working directory")
	odrade.Standing = true
	x.srv.SetProfiles([]proto.Profile{odrade})
	x.a.st.LoadProfiles()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	x.a.newSession.profile = "Odrade · supervisor · standing session"
	x.settle()
	x.hasAll(t, "Open Session")
	if x.tt.HasText("Working directory") {
		t.Fatal("the form still asks for a folder")
	}
}

// New Session offers a standing profile whose session the desktop knows,
// though its model no longer runs: pocketd opens that session all the
// same (Codex review of #43, round 1).
func TestAKnownStandingSessionIsOfferedDespiteItsProfilesProblem(t *testing.T) {
	x := newTestApp(t)
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 9, Title: "Odrade", Cwd: "/home/team/agents/odrade", Model: opus, Profile: "odrade", Route: "profile",
		Standing: true, State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Model: opus, Home: "/home/team/agents/odrade",
		Standing: true, Problem: "no sign-in runs anthropic/claude-opus-5-5"})
	x.connect(t)
	x.click(t, "New Session")
	if !slices.ContainsFunc(x.a.newSession.profileChoices, func(ch state.ProfileChoice) bool {
		return ch.ID == "odrade" && ch.Label == "Odrade · supervisor · standing session"
	}) {
		t.Fatalf("choices %+v", x.a.newSession.profileChoices)
	}
	if x.tt.HasText("1 profile cannot start: odrade") {
		t.Error("the dialog says the profile it offers cannot start")
	}
}
