package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// TestTheStatusBarCountsWhatWaitsAndRuns checks the status bar under the
// window's content: each kind that waits with its count, as a way to the
// Inbox, the sessions that run, and the service's version with this
// device's name. It holds for the NSGo client what the status bar's
// assertions of TestSessionsAndInboxSnapshot, TestAQuestionInTheInboxSnapshot,
// TestALessonInTheInboxSnapshot and TestAPullRequestInTheInboxSnapshot
// hold for the MyGo client.
func TestTheStatusBarCountsWhatWaitsAndRuns(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 1, Title: "Waits", Cwd: "/work", State: proto.SessionWaiting})
	s.AddSession(proto.Session{ID: 2, Title: "Waits too", Cwd: "/work", State: proto.SessionWaiting})
	s.AddSession(proto.Session{ID: 3, Title: "Runs", Cwd: "/work", State: proto.SessionRunning})
	s.AddThread(proto.Thread{ID: 11, Session: 9, Title: "Publish artifacts", State: proto.ThreadBlocked})
	s.AddAsk(proto.Ask{ID: 5, Thread: 11, SessionID: 9, State: proto.AskOpen, Question: "Approve the artifact?"})
	s.AddLesson(proto.Lesson{ID: 4, Agent: "vera", Text: "Squash before merging.", State: proto.LessonPending, Writer: "vera", CreatedAt: time.Now().Add(-time.Minute)})
	s.AddPull(proto.PullRequest{ID: 7, Repository: "example/mail", Number: 377, Title: "Keep the release guide portable", Base: "main", HeadSHA: "c1dab5e3a220128498a7608a8366fa3bb2f29ba4", State: proto.PullOpen, Checks: proto.ChecksPending})
	app := signedInApp(t, s, 1230, 800)
	for _, text := range []string{"2 sessions wait", "1 ask waits", "1 lesson waits", "1 pull request waits", "1 session runs", "pocketd test · laptop"} {
		app.WaitFor("id:status-bar >> text:"+text, 5*time.Second)
	}
	// From another place, what waits leads to the Inbox.
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor("text:1 thread on the desk", 5*time.Second)
	app.Press("id:status-bar >> text:1 ask waits")
	app.WaitFor("text:Approve the artifact?", 5*time.Second)
}

// TestTheStatusBarShowsUsage checks today's usage and the last 7 days', as
// TestTheUsageMeterSnapshot's status bar does for the MyGo client.
func TestTheStatusBarShowsUsage(t *testing.T) {
	s := pocketdtest.New()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus"}
	deepseek := proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4"}
	// The totals are as of the client's clock, so the days are told from now.
	now := time.Now().UTC()
	today, earlier := now.Format(time.DateOnly), now.AddDate(0, 0, -6).Format(time.DateOnly)
	s.SetUsage(proto.UsageReport{Zone: "UTC", Today: today, Days: []proto.UsageDay{
		{Day: earlier, Models: []proto.ModelUsage{
			{Model: opus, Usage: proto.Usage{Total: 3_100_000}},
			{Model: deepseek, Usage: proto.Usage{Total: 900_000, Cost: 2.1}},
		}},
		{Day: today, Models: []proto.ModelUsage{
			{Model: opus, Usage: proto.Usage{Total: 1_000_000}},
			{Model: deepseek, Usage: proto.Usage{Total: 240_000, Cost: 0.84}},
			{Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-5.5"}, Usage: proto.Usage{Total: 120_000}},
		}},
	}})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("id:status-bar >> text:Today 1.4M tokens · $0.84", 5*time.Second)
	app.WaitFor("id:status-bar >> text:7 days 5.4M tokens · $2.94", 5*time.Second)
}

// TestANarrowWindowsStatusBarKeepsWhatWaits checks a narrow window: what
// waits stays, and the usage and the service's version are left out.
func TestANarrowWindowsStatusBarKeepsWhatWaits(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 1, Title: "Waits", Cwd: "/work", State: proto.SessionWaiting})
	app := signedInApp(t, s, 620, 800)
	app.WaitFor("id:status-bar >> text:1 session waits", 5*time.Second)
	if app.Exists("text:pocketd test · laptop") {
		t.Error("a narrow window's status bar shows the service's version")
	}
}

// TestTheSidebarCountsTheDeskAndTheShells checks the places' rows: the
// Work board's counts the threads on the desk, and the Terminal's the
// shells that run. The filter's last row says what it searches for.
func TestTheSidebarCountsTheDeskAndTheShells(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 9, Title: "Publish", Cwd: "/work"})
	s.AddThread(proto.Thread{ID: 11, Session: 9, Title: "Publish artifacts", State: proto.ThreadOpen})
	s.AddThread(proto.Thread{ID: 12, Session: 9, Title: "Sign artifacts", State: proto.ThreadOpen})
	s.AddTerminal(proto.Terminal{ID: "t1", Title: "bash"})
	app := signedInApp(t, s, 1230, 800)
	app.WaitFor("label:Work board, 2 threads on the desk", 5*time.Second)
	app.WaitFor("label:Terminal, 1 shell running", 5*time.Second)
	app.Type("label:Filter sessions", "artifact")
	app.WaitFor("label:Search all sessions for “artifact”", 5*time.Second)
}
