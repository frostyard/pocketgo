package nsgoclient

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// at is a time around the fake pocketd's now.
func at(d time.Duration) time.Time { return pocketdtest.Now.Add(d) }

// seedTeam gives the fake pocketd a supervisor, a specialist and their
// threads, as the MyGo client's seedTeam does: one of each place of the
// board, with a question, a pull request to review, a draft and a merged
// one.
func seedTeam(s *pocketdtest.Server) {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	s.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor", Home: "/home/team/agents/odrade"})
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist", Home: "/home/team/agents/vera"})
	s.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/agents/odrade", Model: model, Profile: "odrade",
		Route: "profile", UpdatedAt: at(-time.Hour)})
	for i, title := range []string{"Fix the flaky arm64 job", "Pin the runner image", "Draft the release notes", "Bump Go to 1.27",
		"Find why snow fails to boot", "Document the fork"} {
		id := int64(i + 1)
		s.AddSession(proto.Session{ID: 10 + id, Title: fmt.Sprintf("T%d · %s", id, title), Cwd: "/home/team/projects/nsl", Model: model,
			Profile: "vera", Route: "profile", UpdatedAt: at(-time.Duration(id) * time.Minute)})
	}
	thread := func(id int64, title string, st proto.ThreadState, repo, report string, updated time.Duration) {
		th := proto.Thread{ID: id, Title: title, Agent: "vera", Supervisor: 1, Session: 10 + id, Task: "Do it.", State: st, Report: report,
			CreatedAt: at(-2 * time.Hour), UpdatedAt: at(updated)}
		if repo != "" {
			th.Repository, th.Branch = repo, fmt.Sprintf("vera/%s-t%d", strings.ToLower(strings.ReplaceAll(title, " ", "-")), id)
		}
		s.AddThread(th)
	}
	thread(1, "Fix the flaky arm64 job", proto.ThreadBlocked, "frostyard/nsl", "The job times out on the 2-year-old runner image.", -time.Minute)
	thread(2, "Pin the runner image", proto.ThreadDone, "frostyard/nsl",
		"Pinned the runner to ubuntu-24.04 and raised the timeout to 30 minutes.\n\nThe last 20 runs passed.\nOne flaked on the network.\nNothing else changed.",
		-2*time.Minute)
	thread(3, "Draft the release notes", proto.ThreadOpen, "frostyard/frostread", "", -3*time.Minute)
	thread(4, "Bump Go to 1.27", proto.ThreadStarting, "frostyard/nsl", "", -4*time.Minute)
	thread(5, "Find why snow fails to boot", proto.ThreadDone, "frostyard/snow", "The bootloader entry points at a removed kernel.", -5*time.Minute)
	s.AddThread(proto.Thread{ID: 6, Title: "Document the fork", Agent: "vera", Supervisor: 1, Session: 16, Cwd: "/home/team/agents/vera",
		Task: "Do it.", State: proto.ThreadClosed, Report: "Closed: its pull requests merged (frostyard/hotedge#1).",
		CreatedAt: at(-2 * time.Hour), UpdatedAt: at(-6 * time.Minute)})
	s.AddAsk(proto.Ask{ID: 1, Thread: 1, SessionID: 11, Question: "Pin the runner to ubuntu-24.04?", Options: []string{"Yes", "No"},
		State: proto.AskOpen, CreatedAt: at(-time.Minute)})
	s.AddPull(proto.PullRequest{ID: 1, Thread: 2, Repository: "frostyard/nsl", Number: 31, Title: "Pin the runner image to ubuntu-24.04",
		URL: "https://evil.example/frostyard/nsl/pull/31", State: proto.PullOpen, Head: "vera/pin-the-runner-image-t2", Base: "main",
		HeadSHA: strings.Repeat("c", 40), Ready: true, Checks: proto.ChecksGreen,
		Review: &proto.PullReview{ID: 4, Thread: 7, Agent: "teg", Model: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"},
			Family: "openai", Head: strings.Repeat("c", 40), Verdict: proto.ReviewReady, Summary: "Read the diff."},
		CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	s.AddPull(proto.PullRequest{ID: 2, Thread: 3, Repository: "frostyard/frostread", Number: 12, Title: "Release notes for 0.9",
		URL: "https://github.com/frostyard/frostread/pull/12", State: proto.PullOpen, Draft: true, Head: "vera/draft-the-release-notes-t3",
		Base: "main", CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
	s.AddPull(proto.PullRequest{ID: 4, Thread: 5, Repository: "frostyard/snow", Number: 9, Title: "Point the bootloader at the current kernel",
		URL: "https://github.com/frostyard/snow/pull/9", State: proto.PullMerged, Head: "vera/find-why-snow-fails-to-boot-t5", Base: "main",
		CheckedAt: at(-time.Minute), CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
}

// card is the selector of thread id's card on the board.
func card(id int64) string { return fmt.Sprintf("id:thread-%d", id) }

// The board says of each thread who works on it, where, and why it is
// where it is, as TestTheWorkBoardSnapshot holds for the MyGo client. The
// lanes stay the NSGo client's own: by the thread's state.
func TestTheWorkBoardSaysWhyEachThreadIsWhereItIs(t *testing.T) {
	s := pocketdtest.New()
	seedTeam(s)
	app := signedInApp(t, s, 1230, 900)
	app.Press("sidebar-panel >> text:Work board")
	app.WaitFor("text:5 threads on the desk, 2 wait for you", 5*time.Second)
	for id, says := range map[int64][]string{
		1: {"T1 · Vera · frostyard/nsl on vera/fix-the-flaky-arm64-job-t1", "Your move", `"A1 waits for your answer."`},
		2: {"T2 · Vera · frostyard/nsl on vera/pin-the-runner-image-t2", "Your move", `"frostyard/nsl#31 waits for your review."`},
		3: {"Specialist working", `"Vera works on it."`},
		4: {"Specialist working", `"pocketd sets it up for Vera."`},
		5: {"T5 · Vera · frostyard/snow on vera/find-why-snow-fails-to-boot-t5", "Supervisor review", `"Vera reported it done; Odrade reads the report."`},
	} {
		for _, text := range says {
			app.WaitFor(card(id)+" >> text:"+text, 5*time.Second)
		}
	}
	// The lanes are by the thread's state, each thread in its own.
	for lane, id := range map[string]int64{"starting": 4, "open": 3, "blocked": 1, "done": 2} {
		app.Find("id:board-lane-" + lane + " >> " + card(id))
	}
	if app.Exists(card(6)) {
		t.Error("a closed thread shows before its section is unfolded")
	}
	app.Screenshot("work-board-sentences")
	app.Press("label:Closed")
	app.WaitFor(card(6)+` >> text:"Closed."`, 5*time.Second)
	app.Find(card(6) + " >> text:T6 · Vera · in agents/vera")
}

// plan1 is a plan led by duncan in its thread T7, in review with its
// graph, as the MyGo client's seedPlan gives it.
func plan1() proto.Plan {
	rev := strings.Repeat("ab", 32)
	return proto.Plan{ID: 1, Thread: 7, Supervisor: 1, Lead: "duncan", Title: "Sign the ISO", State: proto.PlanReview, Rev: rev,
		Text: "# Sign the ISO\n\nSign each release with the TPM key; keep the public key in the repository.",
		Graph: &proto.PlanGraph{Version: 2, Rev: rev, Tasks: []proto.PlanTask{
			{ID: "t1", Title: "Make the signing key", Agent: "vera", Repository: "frostyard/snow", Acceptance: []string{"The key is in the TPM"}, State: proto.TaskWaiting},
			{ID: "t2", Title: "Sign the release ISO", Agent: "vera", Repository: "frostyard/snow", Acceptance: []string{"The ISO verifies"},
				Deps: []string{"t1"}, State: proto.TaskWaiting},
			{ID: "t3", Title: "Write the runbook", Agent: "duncan", Acceptance: []string{"It names every step"}, State: proto.TaskWaiting},
		}},
		CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)}
}

// seedPlan gives the fake pocketd the team that works on plan1, and the
// plan's own thread, T7.
func seedPlan(s *pocketdtest.Server) {
	s.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor"})
	s.AddProfile(proto.Profile{ID: "duncan", Name: "Duncan", Role: "agent", Home: "/home/team/agents/duncan"})
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "agent"})
	s.AddSession(proto.Session{ID: 1, Title: "Plan the week", Profile: "odrade"})
	s.AddThread(proto.Thread{ID: 7, Title: "Sign the ISO", Agent: "duncan", Supervisor: 1, Session: 17, Cwd: "/home/team/agents/duncan",
		Task: "Plan the signing.", State: proto.ThreadOpen, Plan: 1, CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)})
}

// Each sentence that says why a thread is where it is shows for its case,
// in the MyGo client's words (its threadWhy).
func TestEachSentenceOfWhyAThreadIsWhereItIs(t *testing.T) {
	// A thread of vera's that odrade's session delegated.
	thread := func(id int64, st proto.ThreadState) proto.Thread {
		return proto.Thread{ID: id, Title: "Fix the flaky tests", Agent: "vera", Supervisor: 1, Session: 9, State: st,
			CreatedAt: at(-time.Hour), UpdatedAt: at(-time.Minute)}
	}
	team := func(s *pocketdtest.Server) {
		s.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor"})
		s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
		s.AddSession(proto.Session{ID: 1, Title: "Plan the week", Profile: "odrade"})
	}
	pull := func(id, number int64) proto.PullRequest {
		return proto.PullRequest{ID: id, Thread: 4, Repository: "frostyard/nsl", Number: number, Title: "Fix it", State: proto.PullOpen, Head: "vera/fix", Base: "main"}
	}
	paused := func(round int) *proto.Shift {
		return &proto.Shift{Stop: "the tests pass", MaxRounds: 8, State: proto.ShiftPaused, Round: round, Why: "the owner wrote to its session"}
	}
	// The plan built: its graph approved, its tasks as given.
	building := func(state proto.PlanState, tasks ...proto.TaskState) proto.Plan {
		p := plan1()
		p.State, p.Graph.Approved = state, true
		for i, st := range tasks {
			p.Graph.Tasks[i].State = st
		}
		return p
	}
	for _, c := range []struct {
		name string
		seed func(s *pocketdtest.Server)
		id   int64
		// closed says the thread is in the Closed section, which starts
		// folded.
		closed bool
		want   string
	}{
		// The owner moves it.
		{name: "a plan to approve", id: 7, want: "P1 waits for your approval.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			s.AddPlan(plan1())
		}},
		{name: "a plan whose gate holds an open ask", id: 7, want: "P1 waits for your answer: A3 open.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			p := building(proto.PlanBuilding, proto.TaskReady)
			p.Waiting = []string{"A3 open"}
			s.AddPlan(p)
		}},
		{name: "a plan whose task's thread waits for the owner", id: 7, want: "Its tasks wait for you: T8.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			p := building(proto.PlanBuilding, proto.TaskDelegated)
			p.Graph.Tasks[0].Thread = 8
			s.AddPlan(p)
			s.AddThread(proto.Thread{ID: 8, Title: "P1 t1 · Make the signing key", Agent: "vera", Supervisor: 1, State: proto.ThreadOpen, Plan: 1, PlanTask: "t1"})
			s.AddAsk(proto.Ask{ID: 5, Thread: 8, Question: "Which TPM?", State: proto.AskOpen})
		}},
		{name: "one ask", id: 4, want: "A1 waits for your answer.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadBlocked))
			s.AddAsk(proto.Ask{ID: 1, Thread: 4, Question: "Pin it?", State: proto.AskOpen})
		}},
		{name: "two asks", id: 4, want: "A1 and A2 wait for your answers.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadBlocked))
			s.AddAsk(proto.Ask{ID: 1, Thread: 4, Question: "Pin it?", State: proto.AskOpen})
			s.AddAsk(proto.Ask{ID: 2, Thread: 4, Question: "To which image?", State: proto.AskOpen})
		}},
		{name: "one pull request to review", id: 4, want: "frostyard/nsl#31 waits for your review.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadDone))
			s.AddPull(pull(1, 31))
		}},
		{name: "two pull requests to review", id: 4, want: "frostyard/nsl#31 and frostyard/nsl#32 wait for your review.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadDone))
			s.AddPull(pull(1, 31))
			s.AddPull(pull(2, 32))
		}},
		{name: "a shift paused with rounds left", id: 4, want: "Its shift is paused for your message: resume or stop it.", seed: func(s *pocketdtest.Server) {
			team(s)
			th := thread(4, proto.ThreadOpen)
			th.Shift = paused(2)
			s.AddThread(th)
		}},
		{name: "a shift paused in its last round", id: 4, want: "Its shift is paused for your message in its last round: stop it.", seed: func(s *pocketdtest.Server) {
			team(s)
			th := thread(4, proto.ThreadOpen)
			th.Shift = paused(8)
			s.AddThread(th)
		}},
		{name: "a shift paused that the card offers no change of", id: 4, want: "Its shift is paused for your message.", seed: func(s *pocketdtest.Server) {
			team(s)
			th := thread(4, proto.ThreadOpen)
			th.Session, th.Shift = 0, paused(2)
			s.AddThread(th)
		}},
		// Its specialist moves it.
		{name: "a thread that starts", id: 4, want: "pocketd sets it up for Vera.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadStarting))
		}},
		{name: "a plan whose tasks are delivered", id: 7, want: "Its tasks are at work: t1 in T8.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			p := building(proto.PlanBuilding, proto.TaskDelegated)
			p.Graph.Tasks[0].Thread = 8
			s.AddPlan(p)
			s.AddThread(proto.Thread{ID: 8, Title: "P1 t1 · Make the signing key", Agent: "vera", Supervisor: 1, State: proto.ThreadOpen, Plan: 1, PlanTask: "t1"})
		}},
		{name: "an approved plan with no task delivered or ready", id: 7, want: "Its tasks are at work.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			s.AddPlan(building(proto.PlanApproved))
		}},
		{name: "a plan its lead drafts", id: 7, want: "Duncan plans it.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			p := plan1()
			p.State = proto.PlanDrafting
			s.AddPlan(p)
		}},
		{name: "an open thread", id: 4, want: "Vera works on it.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadOpen))
		}},
		// Its supervisor moves it.
		{name: "a plan with ready tasks", id: 7, want: "Odrade delivers its ready tasks: t1 and t3.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			s.AddPlan(building(proto.PlanApproved, proto.TaskReady, proto.TaskWaiting, proto.TaskReady))
		}},
		{name: "a plan whose task's thread waits for the supervisor", id: 7, want: "Its tasks wait for Odrade: T8.", seed: func(s *pocketdtest.Server) {
			seedPlan(s)
			p := building(proto.PlanBuilding, proto.TaskDelegated)
			p.Graph.Tasks[0].Thread = 8
			s.AddPlan(p)
			s.AddThread(proto.Thread{ID: 8, Title: "P1 t1 · Make the signing key", Agent: "vera", Supervisor: 1, State: proto.ThreadDone, Plan: 1, PlanTask: "t1"})
		}},
		{name: "a thread reported done", id: 4, want: "Vera reported it done; Odrade reads the report.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadDone))
		}},
		{name: "a thread reported blocked", id: 4, want: "Vera is blocked; Odrade reads the report.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadBlocked))
		}},
		{name: "a state this client does not know", id: 4, want: "Odrade reads the report.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, "resting"))
		}},
		// The supervisor by its session's title, with no profile, and by
		// no name, with no session.
		{name: "a supervisor's session with no profile", id: 4, want: "Vera is blocked; Plan the week reads the report.", seed: func(s *pocketdtest.Server) {
			s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
			s.AddSession(proto.Session{ID: 1, Title: "Plan the week"})
			s.AddThread(thread(4, proto.ThreadBlocked))
		}},
		{name: "a supervisor whose session is not listed", id: 4, want: "Vera is blocked; the supervisor reads the report.", seed: func(s *pocketdtest.Server) {
			s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: "specialist"})
			s.AddThread(thread(4, proto.ThreadBlocked))
		}},
		// Nobody moves it.
		{name: "a closed thread", id: 4, closed: true, want: "Closed.", seed: func(s *pocketdtest.Server) {
			team(s)
			s.AddThread(thread(4, proto.ThreadClosed))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := pocketdtest.New()
			c.seed(s)
			app := signedInApp(t, s, 1230, 800)
			app.Press("sidebar-panel >> text:Work board")
			if c.closed {
				app.Press("label:Closed")
			}
			app.WaitFor(card(c.id)+` >> text:"`+c.want+`"`, 5*time.Second)
		})
	}
}
