package nsgoclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// A session says and does in the NSGo client what it says and does in
// the MyGo client: these hold, for the NSGo client, the cases and the
// texts of the MyGo client's question_test.go, changes_test.go,
// workers_test.go, rename_test.go, send_test.go, copy_test.go and
// standing_test.go. Rename, pin, the workers and the changes are in the
// session's inspector, where the NSGo client has them.

var opus = proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}

func at(d time.Duration) time.Time { return pocketdtest.Now.Add(d) }

// questionOf is a question Odrade's session 1 puts to the owner from its
// call q1 (ADR-0048).
func questionOf(id, text string, options ...string) proto.Question {
	return proto.Question{ID: id, SessionID: 1, CallID: "q1", Question: text, Options: options, State: proto.QuestionWaiting,
		CreatedAt: at(-time.Minute), Deadline: at(29 * time.Minute)}
}

// seedQuestion gives Odrade, a supervisor, a session 1 whose run waits on
// its question q, as pocketd lists it.
func seedQuestion(s *pocketdtest.Server, q proto.Question) {
	s.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Home: "/home/team/agents/odrade"})
	args, _ := json.Marshal(map[string]any{"question": q.Question, "options": q.Options})
	s.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/agents/odrade", Model: opus, Profile: "odrade", Route: "profile",
		Standing: true, State: proto.SessionWaiting, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "Plan the week's work for the team.", At: at(-3 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant,
			Text:      "Vera has room for one of the two big items this week, and both are ready to start.",
			ToolCalls: []proto.ToolCall{{ID: "q1", Name: "question", Arguments: args}}, At: at(-time.Minute)},
	)
	s.SetLive(proto.Live{SessionID: 1, Busy: true, Tools: []proto.LiveTool{{CallID: "q1", Name: "question", Status: proto.ToolRunning}}})
	s.PutQuestion(q)
}

// heldRequest holds the requests of method to path back, before pocketd
// has them, until release is closed.
type heldRequest struct {
	http.RoundTripper
	method, path string
	release      chan struct{}
}

func (h *heldRequest) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == h.method && r.URL.Path == h.path {
		select {
		case <-h.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return h.RoundTripper.RoundTrip(r)
}

// openSession opens the session titled title from the sidebar, and waits
// for its header.
func openSession(app *nsgotest.App, title string) {
	app.WaitFor("sidebar-panel >> text:"+title, 5*time.Second)
	app.Press("sidebar-panel >> text:" + title)
	app.WaitFor("id:session-header >> text:"+title, 5*time.Second)
}

// A session's question shows in its transcript, after the agent's words:
// who waits, the question, that the message box takes any other answer
// and when the question is skipped; an answer on its way says so, and the
// card goes once pocketd took it.
func TestAQuestionInItsSession(t *testing.T) {
	s := pocketdtest.New()
	seedQuestion(s, questionOf("q-week", "Which goes first for Vera this week?", "Bump Go to 1.27", "Fix the snow boot", "Both, Go first"))
	path := proto.QuestionPath(1, "q-week")
	held := &heldRequest{RoundTripper: s.Transport(), method: http.MethodPost, path: path, release: make(chan struct{})}
	app := signedInOver(t, held, 1230, 800, nsgotest.WallTime(pocketdtest.Now))
	openSession(app, "Plan the week")
	app.WaitFor("label:Question for you", 5*time.Second)
	app.WaitFor("text:Odrade waits for your answer", 5*time.Second)
	app.Find("label:Question for you >> text:Which goes first for Vera this week?")
	app.Find("text:Or type another answer in the message box below. Skipped today 22:29 if no answer comes.")
	// The header says the session waits.
	app.Find(`id:session-header >> text:"waiting"`)
	app.Screenshot("session-question")

	// The answer is on its way until pocketd has it: the card says so.
	app.Press("text:Fix the snow boot")
	app.Press("text:Submit choices")
	app.WaitFor("label:Question for you >> text:Sending your answer…", 5*time.Second)
	close(held.release)
	app.WaitUntil(5*time.Second, func() bool { q, _ := s.Question("q-week"); return q.State == proto.QuestionAnswered })
	app.WaitForGone("label:Question for you", 5*time.Second)
	var body proto.AnswerQuestion
	if reqs := s.RequestsTo(http.MethodPost, path); len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil ||
		body.Answer != "" || !slices.Equal(body.Option, []int{1}) || body.Skipped {
		t.Errorf("the option's answer %+v, in %d requests", body, len(reqs))
	}

	// A question with no option says only where to type.
	why := questionOf("q-why", "Why the snow fix first?")
	why.Deadline = time.Time{}
	s.PutQuestion(why)
	detail, _ := s.Session(1)
	s.Emit(proto.Event{Type: proto.EventSession, Session: &detail.Session})
	app.WaitFor("text:Type your answer in the message box below.", 5*time.Second)
}

// An answer pocketd took whose reply was lost is checked before its words
// count as refused, and the session says so while the check is on its way.
func TestALostAnswerIsCheckedBeforeItsWordsAreRefused(t *testing.T) {
	s := pocketdtest.New()
	seedQuestion(s, questionOf("q-week", "Which goes first?", "Go", "Snow"))
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Plan the week")
	app.WaitFor("label:Question for you", 5*time.Second)
	path := proto.QuestionPath(1, "q-week")
	// pocketd takes the answer, its session's event comes, and the reply
	// is lost.
	lose := s.HoldAndLoseAnswerOnce(http.MethodPost, path)
	app.Press(`text:"Snow"`)
	app.Press("text:Submit choices")
	app.WaitUntil(5*time.Second, func() bool { return len(s.RequestsTo(http.MethodPost, path)) == 1 })
	app.WaitForGone("label:Question for you", 5*time.Second)
	// The check's answer waits: the session says it is checking.
	check := s.HoldAnswerOnce(http.MethodPost, path)
	lose()
	app.WaitFor("text:Checking whether pocketd took your answer…", 5*time.Second)
	check()
	app.WaitForGone("text:Checking whether pocketd took your answer…", 5*time.Second)
	if n := len(s.RequestsTo(http.MethodPost, path)); n != 2 {
		t.Errorf("%d answers went, want the answer and its check", n)
	}
	if app.Exists("text:Dismiss answer") {
		t.Error("an answer pocketd took is kept as refused")
	}
}

// The header over a session says what it used and its state.
func TestTheHeaderSaysWhatASessionUsedAndItsState(t *testing.T) {
	s := pocketdtest.New()
	usage := proto.Usage{Input: 1_100_000, Output: 100_000, Total: 1_200_000, Cost: 0.42}
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work", Model: opus, State: proto.SessionIdle, Usage: usage})
	app := signedInApp(t, s, 1230, 800)
	// No session shows: the header names the place alone.
	if app.Exists(`id:session-header >> text:"idle"`) {
		t.Error("the Inbox's header shows a session's state")
	}
	openSession(app, "Editor")
	if state.UsageLine(usage) == "" {
		t.Fatal("the fixture used nothing")
	}
	app.WaitFor("id:session-header >> text:"+state.UsageLine(usage), 5*time.Second)
	app.Find(`id:session-header >> text:"idle"`)
	s.Emit(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 4, Busy: true}})
	app.WaitFor(`id:session-header >> text:"running"`, 5*time.Second)
	// A narrow window keeps the title's room: the inspector says the usage.
	app.Resize(390, 730)
	app.WaitForGone("id:session-header >> text:"+state.UsageLine(usage), 5*time.Second)
	app.Find(`id:session-header >> text:"running"`)
}

// seedChanges gives session 11 what it changed in its worktree, and two
// of its files' diffs, the second cut.
func seedChanges(s *pocketdtest.Server) []proto.ChangedFile {
	s.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/agents/odrade", Model: opus, UpdatedAt: at(-time.Hour)})
	s.AddSession(proto.Session{ID: 11, Title: "T1 · Fix the flaky arm64 job", Cwd: "/home/team/projects/nsl", Model: opus, UpdatedAt: at(-time.Minute)})
	files := []proto.ChangedFile{
		{Path: ".github/workflows/ci.yml", Added: 6, Removed: 2, Committed: true, Uncommitted: true},
		{Path: "internal/run/retry.go", Added: 30, Removed: 7, Committed: true},
		{Path: "internal/run/retry_test.go", Added: 18, New: true, Uncommitted: true},
		{Path: "docs/arm64-runner.png", Binary: true, New: true, Uncommitted: true},
	}
	s.SetChanges(proto.Changes{SessionID: 11, Repository: true, Top: "/home/team/projects/nsl--vera-fix-the-flaky-arm64-job-t1",
		Branch: "vera/fix-the-flaky-arm64-job-t1", Head: "9f3c2a1b7e6d5c4b3a291807f6e5d4c3b2a19087", Base: "origin/main",
		BaseRule: proto.ChangesBaseDefault, MergeBase: "4b1e0c9d8a7f6e5d4c3b2a190807f6e5d4c3b2a1", Added: 54, Removed: 9, Files: files})
	s.SetFileDiff(proto.FileDiff{SessionID: 11, Path: ".github/workflows/ci.yml", MergeBase: "4b1e0c9d8a7f6e5d4c3b2a190807f6e5d4c3b2a1",
		Diff: "@@ -12,9 +12,12 @@ jobs:\n   arm64:\n-    runs-on: ubuntu-22.04-arm\n+    runs-on: ubuntu-24.04-arm\n+    timeout-minutes: 30\n     steps:\n"})
	s.SetFileDiff(proto.FileDiff{SessionID: 11, Path: "internal/run/retry.go", Diff: "@@ -1,2 +1,3 @@\n package run\n+\n+// Retry runs f again.\n", Cut: true})
	return files
}

// fileRow is the label of a changed file's row.
func fileRow(f proto.ChangedFile) string {
	return state.FileName(f) + ", " + state.FileState(f) + ", " + state.FileCounts(f)
}

// The session's inspector lists what the session changed (ADR-0053): the
// count, where the changes are counted from, each file with where its
// change is and its lines in their tones, and a file's diff; a cut diff
// says so; Refresh reads the changes and the diff shown again; a session
// in no checkout shows none.
func TestTheChangesOfASession(t *testing.T) {
	s := pocketdtest.New()
	files := seedChanges(s)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "T1 · Fix the flaky arm64 job")
	app.Press("label:Session details")
	app.WaitFor("id:right-panel >> text:4 files +54 −9", 5*time.Second)
	for _, text := range []string{"vera/fix-the-flaky-arm64-job-t1 against origin/main since 4b1e0c9d", "internal/run/retry_test.go",
		"new, not committed", "committed, and changed since", `"committed"`, `"binary"`} {
		if len(app.Query("id:right-panel >> text:"+text)) == 0 {
			t.Errorf("the changes do not say %s", text)
		}
	}
	// A file's lines added and removed show in their own tones.
	added, removed, plain := app.Find(`text:"+6"`).TextStyle.Color, app.Find(`text:"−2"`).TextStyle.Color, app.Find(`id:right-panel >> text:"binary"`).TextStyle.Color
	if added == removed || added == plain || removed == plain {
		t.Errorf("the lines added (%#x) and removed (%#x) do not show in tones of their own beside the small print (%#x)", uint32(added), uint32(removed), uint32(plain))
	}
	// A binary file says so in place of its lines.
	if app.Exists(`text:"+0"`) {
		t.Error("a binary file shows lines")
	}
	app.Scroll("id:right-panel", 420)
	app.Screenshot("session-changes")

	app.Press("label:" + fileRow(files[0]))
	app.WaitFor("text:+    timeout-minutes: 30", 5*time.Second)
	if app.Exists("text:Only the first") {
		t.Error("a whole diff says it is cut")
	}
	// A cut diff says so.
	app.Press("label:" + fileRow(files[1]))
	app.WaitFor("text:Only the first 64 KB of this diff show: the rest is in the checkout.", 5*time.Second)

	// Refresh reads the changes and the diff shown again.
	changes, diff := proto.Path(proto.PathChanges, 11), proto.Path(proto.PathChangesFile, 11)
	before, shown := len(s.RequestsTo(http.MethodGet, changes)), len(s.RequestsTo(http.MethodGet, diff))
	app.Press("label:Refresh the changes")
	app.WaitUntil(5*time.Second, func() bool {
		return len(s.RequestsTo(http.MethodGet, changes)) == before+1 && len(s.RequestsTo(http.MethodGet, diff)) == shown+1
	})
	app.Press("text:Close diff")
	app.WaitForGone("text:Only the first 64 KB", 5*time.Second)

	// A session whose folder is in no checkout shows no changes.
	openSession(app, "Plan the week")
	app.WaitFor("id:right-panel >> text:Pin session", 5*time.Second)
	app.WaitForGone("label:Refresh the changes", 5*time.Second)
}

// A read of a session's changes that failed says why, never a count or
// that nothing changed, and Refresh reads again.
func TestAFailedChangesReadSaysWhy(t *testing.T) {
	s := pocketdtest.New()
	seedChanges(s)
	path := proto.Path(proto.PathChanges, 11)
	s.FailOnce(http.MethodGet, path, http.StatusInternalServerError)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "T1 · Fix the flaky arm64 job")
	// The inspector reads the changes again as it opens; that fails too.
	app.WaitUntil(5*time.Second, func() bool { return len(s.RequestsTo(http.MethodGet, path)) == 1 })
	s.FailOnce(http.MethodGet, path, http.StatusInternalServerError)
	app.Press("label:Session details")
	app.WaitFor("id:right-panel >> text:"+state.NotRead, 5*time.Second)
	app.Find("id:right-panel >> text:Reading the changes failed: internal: made to fail")
	if app.Exists("text:Nothing changed since then.") {
		t.Error("a failed read says nothing changed")
	}
	if app.Exists("label:Dismiss message") {
		app.Press("label:Dismiss message")
	}
	app.Press("label:Refresh the changes")
	app.WaitFor("id:right-panel >> text:4 files +54 −9", 5*time.Second)
	app.WaitForGone("text:Reading the changes failed", 5*time.Second)
}

// After a read that found no change, a read that failed says nothing of
// no change: what the folder holds is not known.
func TestAFailedRefreshSaysNothingOfNoChange(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 11, Title: "T1 · Fix the flaky arm64 job", Cwd: "/home/team/projects/nsl", Model: opus})
	s.SetChanges(proto.Changes{SessionID: 11, Repository: true, Branch: "vera/fix", Head: "9f3c2a1b7e6d5c4b3a291807f6e5d4c3b2a19087",
		Base: "origin/main", BaseRule: proto.ChangesBaseDefault, MergeBase: "9f3c2a1b7e6d5c4b3a291807f6e5d4c3b2a19087", Files: []proto.ChangedFile{}})
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "T1 · Fix the flaky arm64 job")
	app.Press("label:Session details")
	app.WaitFor("id:right-panel >> text:No changes", 5*time.Second)
	app.Find("text:Nothing changed since then.")
	s.FailOnce(http.MethodGet, proto.Path(proto.PathChanges, 11), http.StatusInternalServerError)
	app.Press("label:Refresh the changes")
	app.WaitFor("id:right-panel >> text:Reading the changes failed: internal: made to fail", 5*time.Second)
	if app.Exists("text:Nothing changed since then.") {
		t.Error("a failed read says nothing changed")
	}
}

// seedWorker gives Duncan's session 2 a worker, session 6, and the owner
// a session of their own.
func seedWorker(s *pocketdtest.Server) {
	s.SetProfiles([]proto.Profile{{ID: "duncan", Name: "Duncan", Role: proto.RoleAgent, Home: "/home/team/agents/duncan", Runs: opus}})
	s.AddSession(proto.Session{ID: 2, Title: "T7 · Fix the flaky CI in nsl", Cwd: "/home/team/projects/nsl--duncan-fix-ci", Model: opus,
		Profile: "duncan", State: proto.SessionIdle, UpdatedAt: at(-2 * time.Minute)})
	s.AddSession(proto.Session{ID: 4, Title: "Homewiki: garden plan", Cwd: "/home/team/projects/homewiki", Model: opus,
		State: proto.SessionIdle, UpdatedAt: at(-4 * time.Minute)})
	s.AddSession(proto.Session{ID: 6, Title: "T7 · worker", Cwd: "/home/team/projects/nsl--duncan-fix-ci",
		Model: proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}, Route: "worker-default", StartedBy: 2,
		State: proto.SessionRunning, UpdatedAt: at(-90 * time.Second)})
}

// workerCards counts the worker cards shown, by their labels.
func workerCards(app *nsgotest.App) int { return len(app.Query("label:/^Open worker /")) }

// A specialist's session lists its worker, with its model and its state,
// which opens the worker's session; that one says it is a worker, of
// which model, and leads back to the specialist. Another session shows
// neither.
func TestAWorkerCardOpensItsSessionAndBackOpensTheSpecialist(t *testing.T) {
	s := pocketdtest.New()
	seedWorker(s)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "T7 · Fix the flaky CI in nsl")
	app.Press("label:Session details")
	app.WaitFor("label:Open worker T7 · worker, running, deepseek-flash", 5*time.Second)
	if n := workerCards(app); n != 1 {
		t.Fatalf("cards = %d, want 1", n)
	}
	app.Find("id:right-panel >> text:deepseek-flash")
	app.Find(`id:right-panel >> text:"running"`)
	app.Screenshot("session-workers")
	app.Press("label:Open worker T7 · worker, running, deepseek-flash")
	app.WaitFor("id:session-header >> text:T7 · worker", 5*time.Second)
	app.WaitFor("text:Back to specialist", 5*time.Second)
	app.Screenshot("session-worker")
	for _, text := range []string{"Worker:", "T7 · worker", "deepseek-flash"} {
		app.Find(`id:worker-header >> text:"` + text + `"`)
	}
	if n := workerCards(app); n != 0 {
		t.Errorf("worker has %d cards", n)
	}
	app.Press("label:Back to the specialist's session, T7 · Fix the flaky CI in nsl")
	app.WaitFor("id:session-header >> text:T7 · Fix the flaky CI in nsl", 5*time.Second)
	openSession(app, "Homewiki: garden plan")
	app.WaitFor("id:right-panel >> text:Pin session", 5*time.Second)
	if app.Exists("text:Back to specialist") || workerCards(app) != 0 || app.Exists(`id:right-panel >> text:"Workers"`) {
		t.Error("unrelated session shows worker controls")
	}
}

func TestMoreThanThreeWorkersFoldBehindMore(t *testing.T) {
	s := pocketdtest.New()
	seedWorker(s)
	for i := 7; i <= 10; i++ {
		s.AddSession(proto.Session{ID: int64(i), Title: fmt.Sprintf("Worker %d", i), Route: "worker-default", StartedBy: 2,
			State: proto.SessionIdle, UpdatedAt: at(0)})
	}
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "T7 · Fix the flaky CI in nsl")
	app.Press("label:Session details")
	app.WaitFor("text:+2 more", 5*time.Second)
	if n := workerCards(app); n != 3 {
		t.Fatalf("collapsed cards = %d, want 3", n)
	}
	app.Press("label:Show 2 more workers")
	app.WaitFor("text:Show fewer", 5*time.Second)
	if n := workerCards(app); n != 5 {
		t.Fatalf("expanded cards = %d, want 5", n)
	}
	app.Press("label:Show fewer workers")
	app.WaitFor("text:+2 more", 5*time.Second)
	if n := workerCards(app); n != 3 {
		t.Fatalf("folded cards = %d, want 3", n)
	}
}

// seedChani gives the fake pocketd a private agent, Chani, and her
// session, which pocketd says the owner may rename (ADR-0058).
func seedChani(s *pocketdtest.Server) {
	s.AddProfile(proto.Profile{ID: "chani", Name: "Chani", Role: proto.RoleAgent, Home: "/home/team/agents/chani", Cwd: "/home/team/agents/chani",
		Private: true, Runs: opus})
	s.AddSession(proto.Session{ID: 21, Title: "Session 21", Cwd: "/home/team/agents/chani", Model: opus, Profile: "chani", Route: "profile",
		Renamable: true, State: proto.SessionIdle, UpdatedAt: at(-30 * time.Second)})
	s.AddSession(proto.Session{ID: 4, Title: "Homewiki: garden plan", Cwd: "/home/team/projects/homewiki", Model: opus,
		State: proto.SessionIdle, UpdatedAt: at(-4 * time.Minute)})
}

// A refused rename keeps the title and says why where the session is, at
// the transcript's end.
func TestARefusedRenameSaysWhyInTheSession(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Session 21")
	app.Press("label:Session details")
	app.WaitFor("label:Session title", 5*time.Second)
	app.Clear("label:Session title")
	app.Type("label:Session title", "Reading list")
	// pocketd no longer takes it: the profile stopped being private.
	detail, _ := s.Session(21)
	detail.Session.Renamable = false
	s.AddSession(detail.Session)
	app.Press("role:button Rename")
	app.WaitFor("id:banner >> text:Renaming the session failed: bad_request: session 21 is not a private agent's: only a private agent's session may be renamed", 5*time.Second)
	app.Find("id:session-header >> text:Session 21")
	if got := s.RequestsTo(http.MethodPost, proto.Path(proto.PathTitle, 21)); len(got) != 1 {
		t.Errorf("%d renames went", len(got))
	}
}

// Rename is offered only where pocketd says the session may be renamed,
// and waits while a rename of it is on its way (state.Store.MayRename).
func TestRenameIsOfferedWhereTheSessionMayBeRenamed(t *testing.T) {
	s := pocketdtest.New()
	seedChani(s)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Session 21")
	app.Press("label:Session details")
	app.WaitFor("label:Session title", 5*time.Second)
	app.Clear("label:Session title")
	app.Type("label:Session title", "Chani: reading list")
	path := proto.Path(proto.PathTitle, 21)
	release := s.HoldAnswerOnce(http.MethodPost, path)
	app.Press("role:button Rename")
	app.WaitUntil(5*time.Second, func() bool { return s.Holding(http.MethodPost, path) })
	if app.Enabled("role:button Rename") {
		t.Error("Rename is offered while a rename is on its way")
	}
	release()
	app.WaitFor("id:session-header >> text:Chani: reading list", 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { return app.Enabled("role:button Rename") })
	if got := s.RequestsTo(http.MethodPost, path); len(got) != 1 {
		t.Errorf("%d renames went", len(got))
	}
	// A team's or the owner's own session offers no rename.
	openSession(app, "Homewiki: garden plan")
	app.WaitFor("id:right-panel >> text:Pin session", 5*time.Second)
	app.WaitForGone("label:Session title", 5*time.Second)
}

// A reply that failed shows as a problem, and a quiet line (a stopped
// run, a system entry) between two lines: neither as a caption among the
// agent's words.
func TestANoteAndAnErrorShowApartFromTheWords(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work", Model: opus, State: proto.SessionIdle},
		proto.Entry{ID: 1, SessionID: 4, Kind: proto.EntryUser, Text: "Fix the build."},
		proto.Entry{ID: 2, SessionID: 4, Kind: proto.EntryAssistant, Text: "Looking at the log.", Error: "the provider refused the request"},
		proto.Entry{ID: 3, SessionID: 4, Kind: proto.EntrySystem, Text: "The model changed to claude-opus-5-5"},
		proto.Entry{ID: 4, SessionID: 4, Kind: proto.EntryAssistant, Text: "Trying again.", StopReason: "aborted"},
	)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Editor")
	app.WaitFor("id:banner >> text:the provider refused the request", 5*time.Second)
	app.WaitFor("id:transcript-note >> text:The model changed to claude-opus-5-5", 5*time.Second)
	app.Find(`id:transcript-note >> text:"Stopped"`)
	app.Screenshot("session-notes")
}

// A supervisor's message shows in its thread's session as the
// supervisor's, with its name and a supervisor badge, placed and queued;
// the owner's own message, whatever its text, shows as theirs (ADR-0032).
func TestASupervisorsMessageInItsThread(t *testing.T) {
	s := pocketdtest.New()
	s.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Home: "/home/team/agents/odrade"})
	s.AddProfile(proto.Profile{ID: "vera", Name: "Vera", Role: proto.RoleAgent, Home: "/home/team/agents/vera"})
	s.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/agents/odrade", Model: opus, Profile: "odrade", Route: "profile", UpdatedAt: at(-time.Hour)})
	odrade := &proto.Sender{Kind: proto.SenderSupervisor, Session: 1}
	s.AddSession(proto.Session{ID: 30, Title: "T7 · Renumber the ADR", Cwd: "/home/team/projects/core--vera-renumber-t7", Model: opus,
		Profile: "vera", Route: "profile", State: proto.SessionRunning, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 30, Kind: proto.EntryUser, SubmissionID: 20, At: at(-20 * time.Minute),
			Text: "Odrade, the team's supervisor, delegated this to you in thread T7: Renumber the ADR\n\nAdd ADR-0088 for the cache keys."},
		proto.Entry{ID: 2, SessionID: 30, Kind: proto.EntryAssistant, Text: "Added ADR-0088 and opened frostyard/core#282.", At: at(-15 * time.Minute)},
		proto.Entry{ID: 3, SessionID: 30, Kind: proto.EntryUser, SubmissionID: 21, Sender: odrade, At: at(-5 * time.Minute),
			Text: "[T7 · Odrade, the supervisor] core#281 merged its own ADR-0088 first: renumber yours to 0089.\n\nWhen you have acted on it, report with team_message."},
		proto.Entry{ID: 4, SessionID: 30, Kind: proto.EntryAssistant, Text: "Renumbering to ADR-0089.", At: at(-4 * time.Minute)},
		proto.Entry{ID: 5, SessionID: 30, Kind: proto.EntryUser, SubmissionID: 22, At: at(-3 * time.Minute),
			Text: "[T7 · Odrade, the supervisor] Keep the old link working too."},
	)
	s.SetLive(proto.Live{SessionID: 30, Busy: true, Queued: []proto.QueuedMessage{
		{SubmissionID: 23, Sender: odrade, Text: "[T7 · Odrade, the supervisor] T80 found a blocking point: the index skips 0089."},
	}})
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "T7 · Renumber the ADR")
	app.WaitFor("text:T80 found a blocking point: the index skips 0089.", 5*time.Second)
	app.Find("text:core#281 merged its own ADR-0088 first: renumber yours to 0089.")
	// The placed message and the queued one are the supervisor's, by
	// pocketd's mark: the owner's own, whatever its text, is not.
	const from = "label:Open the session of Odrade, the supervisor"
	app.WaitUntil(5*time.Second, func() bool { return len(app.Query(from)) == 2 })
	if n := len(app.Query(`id:content >> text:"supervisor"`)); n != 2 {
		t.Errorf("%d supervisor badges, want 2", n)
	}
	if n := len(app.Query(`id:content >> text:"Odrade"`)); n != 2 {
		t.Errorf("the supervisor is named %d times, want 2", n)
	}
	if app.Exists("text:From supervisor") {
		t.Error("a supervisor's message names its session's number")
	}
	app.Screenshot("session-supervisor-message")
	// The name leads to the supervisor's session.
	app.Press(from + " >> nth=0")
	app.WaitFor("id:session-header >> text:Plan the week", 5*time.Second)
}

// A reply's Copy puts its Markdown on the clipboard, and shows Copied for
// state.CopyHold.
func TestCopyShowsCopiedForTheHold(t *testing.T) {
	const reply = "Use `go test`.\n\n- Run it twice."
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 4, Title: "Editor", Cwd: "/work", Model: opus, State: proto.SessionIdle},
		proto.Entry{ID: 1, SessionID: 4, Kind: proto.EntryUser, Text: "How do I test it?"},
		proto.Entry{ID: 2, SessionID: 4, Kind: proto.EntryAssistant, Text: reply, Thinking: "secret thought"},
	)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Editor")
	app.WaitUntil(5*time.Second, func() bool { return len(app.Query("label:Copy")) == 2 })
	app.Press("label:Copy >> nth=1")
	if app.Clipboard != reply {
		t.Fatalf("clipboard = %q, want Markdown source %q", app.Clipboard, reply)
	}
	if copies, copied := len(app.Query("label:Copy")), len(app.Query("label:Copied")); copies != 1 || copied != 1 {
		t.Fatalf("after a copy: %d Copy and %d Copied, want 1 and 1", copies, copied)
	}
	app.Advance(state.CopyHold - 100*time.Millisecond)
	if n := len(app.Query("label:Copied")); n != 1 {
		t.Fatalf("Copied left before the hold ended: %d show", n)
	}
	app.Advance(100 * time.Millisecond)
	if copies, copied := len(app.Query("label:Copy")), len(app.Query("label:Copied")); copies != 2 || copied != 0 {
		t.Fatalf("after the hold: %d Copy and %d Copied, want 2 and 0", copies, copied)
	}
}

// Start Over asks first, in its own words; once confirmed, the reset
// shows where the agent's context starts again.
func TestStartOverAsksFirst(t *testing.T) {
	s := pocketdtest.New()
	s.AddSession(proto.Session{ID: 9, Title: "Odrade", Cwd: "/home/team/agents/odrade", Model: opus, Profile: "odrade", Route: "profile",
		Standing: true, State: proto.SessionIdle, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 9, Kind: proto.EntryUser, Text: "Plan this week's releases.", At: at(-20 * time.Minute)},
		proto.Entry{ID: 3, SessionID: 9, Kind: proto.EntryAssistant, Text: "snow 2.1 waits for its CI.", At: at(-5 * time.Minute)},
	)
	app := signedInApp(t, s, 1230, 800)
	openSession(app, "Odrade")
	app.WaitFor("text:snow 2.1 waits for its CI.", 5*time.Second)
	app.Press("label:Session details")
	app.WaitUntil(5*time.Second, func() bool { return app.Enabled("role:button Start over") })
	app.Press("role:button Start over")
	app.WaitFor("role:alertdialog >> text:Start Odrade over?", 3*time.Second)
	app.Find("role:alertdialog >> text:" + state.StartOverBody)
	path := proto.Path(proto.PathStartOver, 9)
	if n := len(s.RequestsTo(http.MethodPost, path)); n != 0 {
		t.Fatalf("%d Start Overs went before the owner confirmed", n)
	}
	app.Press("role:alertdialog >> role:button Start Over")
	app.WaitFor("id:transcript-note >> text:"+state.StartedOverNote, 5*time.Second)
	if n := len(s.RequestsTo(http.MethodPost, path)); n != 1 {
		t.Fatalf("%d Start Overs", n)
	}
}
