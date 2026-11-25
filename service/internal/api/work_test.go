package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// workHarness is a team with Agnes, an agent with a home and one
// repository, frostyard/example, and Agnes's own session, which the owner
// started; Rhombur names two repositories, and Chani is private.
func workHarness(t *testing.T) (*teamHarness, proto.Session) {
	t.Helper()
	h := newTeamHarness(t)
	h.addProfiles(map[string]string{
		"agnes.md":   "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Agnes\n",
		"rhombur.md": "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/example: " + h.checkout + "\n  frostyard/other: " + h.checkout + "-other\n---\n# Rhombur\n",
		"chani.md":   "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Chani\n",
	})
	agnes, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "agnes", Profile: "agnes"})
	if err != nil {
		t.Fatal(err)
	}
	return h, agnes
}

// startedWork is the work Agnes's call w1 started, with its task.
func startedWork(t *testing.T, h *teamHarness, caller int64) store.WorkRow {
	t.Helper()
	ctx := context.Background()
	call := TeamCall{Session: caller, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix the flaky test.\nIt times out."}); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Session == 0 {
		t.Fatalf("the work %+v, %v", row, err)
	}
	return row
}

// workAnswersTo are the work sessions' answers the engine got for
// session, in no order, and how many times one was sent.
func workAnswersTo(h *teamHarness, session int64) (texts []string, sends int) {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, key := range h.engine.order {
		if strings.HasPrefix(key, team.WorkReplyPrefix) && h.engine.to[key] == session {
			sends++
			texts = append(texts, h.engine.texts[key])
		}
	}
	slices.Sort(texts)
	return texts, sends
}

// team_work makes a fresh worktree of the caller's repository, named from
// the call's key, starts a session of the caller's profile at its real
// place, recorded as the team's and linked to the caller, and gives it the
// task through the outbox; the same call run again makes no second
// worktree, session or task, and answers the same (ADR-0073).
func TestTeamWorkStartsAWorkSessionInAFreshWorktree(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	work := team.Work{Task: "Fix the flaky test.\nIt times out."}
	answer, err := h.server.TeamWork(ctx, call, work)
	if err != nil {
		t.Fatal(err)
	}
	key := workKey(call)
	row, err := h.store.WorkByKey(ctx, key)
	path, branch := team.WorkWorktree(profiles.Profile{ID: "agnes"}, h.checkout, "Fix the flaky test.", key)
	if err != nil || row.Session == 0 || row.Caller != agnes.ID || row.Path != path || row.Branch != branch || row.Repository != "frostyard/example" ||
		row.Agent != "agnes" || row.Title != "Fix the flaky test." {
		t.Fatalf("the work %+v, %v; want %s on %s", row, err, path, branch)
	}
	if head, err := h.engine.Git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != branch {
		t.Fatalf("the worktree is on %q, %v", head, err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	spec := h.engine.specs[len(h.engine.specs)-1]
	task, to := h.engine.texts[workTaskKey(key)], h.engine.to[workTaskKey(key)]
	watched := h.engine.watching[row.Session]
	created := h.engine.created
	h.engine.mu.Unlock()
	if !spec.Work || spec.Profile == nil || spec.Profile.ID != "agnes" || spec.Cwd != real || spec.Title != "Agnes: Fix the flaky test." ||
		spec.Model != model || spec.Route != "profile" || spec.Thread != 0 {
		t.Fatalf("the work session's spec %+v", spec)
	}
	session, err := h.store.Session(ctx, row.Session)
	if err != nil || session.CreatedBy != teamCreator || session.Profile != "agnes" || session.Cwd != real || session.Title != "Agnes: Fix the flaky test." {
		t.Fatalf("the work session %+v, %v", session, err)
	}
	if to != row.Session || !strings.Contains(task, "\n\nThe task:\nFix the flaky test.\nIt times out.") || !strings.Contains(task, branch) ||
		!strings.Contains(task, "session "+strconv.FormatInt(agnes.ID, 10)) || !watched {
		t.Fatalf("the task %q went to %d (watched %v)", task, to, watched)
	}
	for _, want := range []string{"work session " + strconv.FormatInt(row.Session, 10), real, branch, "faux/faux-1", "frostyard/example"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the answer lacks %q: %s", want, answer)
		}
	}
	if waits, err := h.server.workTaskWaits(ctx, row.Session); err != nil || waits {
		t.Fatalf("the task placed still waits: %v, %v", waits, err)
	}
	// The same call run again, after a restart too.
	for _, s := range []*Server{h.server, h.restarted(t)} {
		again, err := s.TeamWork(ctx, call, work)
		if err != nil || again != answer {
			t.Fatalf("a run again: %q, %v", again, err)
		}
	}
	h.engine.mu.Lock()
	again := h.engine.created
	h.engine.mu.Unlock()
	if again != created {
		t.Fatalf("%d sessions after %d", again, created)
	}
	list, err := h.engine.Git(ctx, h.checkout, "worktree", "list", "--porcelain")
	if err != nil || strings.Count(list, "branch refs/heads/agnes/") != 1 {
		t.Fatalf("the worktrees:\n%s\n%v", list, err)
	}
	// pocketd started it for the team's work: no rename, and no starter
	// on the wire (protocol rule 23a).
	detail, err := h.client.Session(ctx, row.Session)
	if err != nil || detail.Session.Renamable || detail.Session.StartedBy != 0 || detail.Session.Profile != "agnes" {
		t.Fatalf("the work session on the wire %+v, %v", detail.Session, err)
	}
	// A call of its own makes a worktree of its own.
	other, err := h.server.TeamWork(ctx, TeamCall{Session: agnes.ID, Task: 8, CallID: "w2"}, team.Work{Task: "Fix the flaky test.\nIt times out."})
	if err != nil || other == answer {
		t.Fatalf("another call: %q, %v", other, err)
	}
}

// team_work refuses, before anything is recorded or made: a supervisor, a
// session of no profile, a private agent, a work session itself, a
// repository the profile does not name or one left out of two, an empty
// task, and a checkout that is no git work tree (ADR-0073).
func TestTeamWorkRefuses(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.work, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	session := func(id, profile string) int64 {
		t.Helper()
		s, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: id, Profile: profile, Cwd: h.work})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	rhombur, nogit := session("rhombur", "rhombur"), session("nogit", "nogit")
	h.addProfiles(map[string]string{"ghost.md": "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/example: " + h.checkout + "\n---\n# Ghost\n"})
	ghost := session("ghost", "ghost")
	if err := os.Remove(filepath.Join(h.profileDir, "ghost.md")); err != nil {
		t.Fatal(err)
	}
	chani, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "chani", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	worked := startedWork(t, h, agnes.ID)
	h.engine.mu.Lock()
	created := h.engine.created
	h.engine.mu.Unlock()
	for name, tc := range map[string]struct {
		session int64
		w       team.Work
		want    string
	}{
		"a supervisor":                 {h.super.ID, team.Work{Task: "Go."}, "delegate the work with team_delegate"},
		"a session of no profile":      {plain.ID, team.Work{Task: "Go."}, "started from no profile"},
		"a profile that is gone":       {ghost, team.Work{Task: "Go."}, "profile ghost is gone or invalid now"},
		"a private agent":              {chani.ID, team.Work{Task: "Go."}, "chani is private"},
		"a work session":               {worked.Session, team.Work{Task: "Go."}, "report the plan to the session that started you"},
		"a repository left out of two": {rhombur, team.Work{Task: "Go."}, "name the repository: rhombur works on frostyard/example, frostyard/other"},
		"another repository":           {agnes.ID, team.Work{Task: "Go.", Repository: "someone/else"}, "agnes does not work on someone/else"},
		"an empty task":                {agnes.ID, team.Work{Task: "  "}, "the task is empty"},
		"no git work tree":             {nogit, team.Work{Task: "Go."}, "no worktree for frostyard/nogit"},
	} {
		call := TeamCall{Session: tc.session, Task: 31, CallID: "refused"}
		if _, err := h.server.TeamWork(ctx, call, tc.w); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if _, err := h.store.WorkByKey(ctx, workKey(call)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: a refused call was recorded: %v", name, err)
		}
		if _, held, err := h.store.WorktreeOf(ctx, workKey(call)); err != nil || held {
			t.Errorf("%s: a refused call claimed a worktree: %v %v", name, held, err)
		}
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.created != created {
		t.Fatalf("a refused call made a session: %d after %d", h.engine.created, created)
	}
}

// Each run of a work session that ends sends its answer to the session
// that started it once, as a run's end, a delivery pass and a restarted
// pocketd read it; a run the owner's message started sends its own, and a
// run that ended with no answer says so (ADR-0073).
func TestAWorkSessionsAnswersReachItsCallerOnce(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	row := startedWork(t, h, agnes.ID)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{row.Session: {
		{ID: 1, Kind: proto.EntryUser, Text: "The task."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Reading.", StopReason: "toolUse"},
		{ID: 3, Kind: proto.EntryToolResult},
		{ID: 4, Kind: proto.EntryAssistant, Text: "Fixed it; make ci passes.", StopReason: "stop"},
	}}
	h.engine.mu.Unlock()
	endRun(t, h, row.Session)
	endRun(t, h, row.Session)
	prefix := "[S" + strconv.FormatInt(row.Session, 10) + " · work · Fix the flaky test.] "
	if got, sends := workAnswersTo(h, agnes.ID); !slices.Equal(got, []string{prefix + "Fixed it; make ci passes."}) || sends != 1 {
		t.Fatalf("answers %q, sent %d times", got, sends)
	}
	// A delivery pass, and a restarted pocketd's first, read every work
	// session again, and send nothing twice.
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.restarted(t).DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, sends := workAnswersTo(h, agnes.ID); sends != 1 {
		t.Fatalf("sent %d times after the passes", sends)
	}
	// The owner's message starts a run of its own, whose answer goes too;
	// one that ended with none says so.
	h.engine.mu.Lock()
	h.engine.transcripts[row.Session] = append(h.engine.transcripts[row.Session],
		proto.Entry{ID: 5, Kind: proto.EntryUser, Text: "Also the docs."},
		proto.Entry{ID: 6, Kind: proto.EntryAssistant, Text: "Docs done.", StopReason: "stop"},
		proto.Entry{ID: 7, Kind: proto.EntryUser, Text: "And this."})
	h.engine.mu.Unlock()
	endRun(t, h, row.Session)
	want := []string{prefix + "Docs done.", prefix + "Fixed it; make ci passes.", prefix + "The work session's run ended with no reply: no reason was given."}
	if got, sends := workAnswersTo(h, agnes.ID); !slices.Equal(got, want) || sends != 3 {
		t.Fatalf("answers %q, sent %d times; want %q", got, sends, want)
	}
	if got, err := h.store.WorkOfSession(ctx, row.Session); err != nil || got.Cursor != 7 {
		t.Fatalf("the cursor %+v, %v", got, err)
	}
}

// An answer to a calling session at work waits in its queue: a pass sends
// it no second time while it waits, and notes it sent once it is placed;
// pocketd removes no session, so the caller is always there (ADR-0073).
func TestAnAnswerToABusyCallerIsPlacedOnce(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	row := startedWork(t, h, agnes.ID)
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.mu.Lock()
	h.engine.queued, h.engine.withdrawn = map[int64]bool{}, map[int64]bool{}
	h.engine.busy = map[int64]bool{agnes.ID: true}
	h.engine.transcripts = map[int64][]proto.Entry{row.Session: {
		{ID: 1, Kind: proto.EntryUser, Text: "The task."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Done.", StopReason: "stop"},
	}}
	h.engine.mu.Unlock()
	var queued int64
	h.engine.onSend = func(_ int64, key string) {
		if strings.HasPrefix(key, team.WorkReplyPrefix) {
			h.engine.mu.Lock()
			queued = int64(len(h.engine.sent) + 100)
			h.engine.queued[queued] = true
			h.engine.mu.Unlock()
		}
	}
	h.server.followWork(row.Session)
	h.engine.onSend = nil
	key := workReplyKey(row.Session, 2)
	if note, err := h.store.Note(ctx, key); err != nil || note.SentAt != nil || note.SessionID != agnes.ID {
		t.Fatalf("the answer waiting %+v, %v", note, err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	// Its run places it.
	h.engine.mu.Lock()
	delete(h.engine.queued, queued)
	delete(h.engine.busy, agnes.ID)
	h.engine.mu.Unlock()
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if note, err := h.store.Note(ctx, key); err != nil || note.SentAt == nil {
		t.Fatalf("the answer placed %+v, %v", note, err)
	}
	if _, sends := workAnswersTo(h, agnes.ID); sends != 1 {
		t.Fatalf("sent %d times", sends)
	}
}

// A work session takes no message from the owner until its task is in
// its transcript: the task is its first message (protocol rule 10,
// ADR-0073).
func TestTheOwnersMessageWaitsForTheWorkSessionsTask(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.server.retry = func(func()) func() { return func() {} }
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Work {
			h.engine.mu.Lock()
			h.engine.failTo = map[int64]bool{h.engine.next + 1: true}
			h.engine.mu.Unlock()
		}
	}
	row := startedWork(t, h, agnes.ID)
	h.engine.onCreate = nil
	var pe *proto.Error
	_, err := h.client.Send(ctx, row.Session, proto.SendMessage{RequestID: "o1", Text: "Start with the docs."})
	if !errors.As(err, &pe) || pe.Status != http.StatusConflict || !strings.Contains(pe.Message, "does not have its task yet") {
		t.Fatalf("the owner's message before the task: %v", err)
	}
	h.failTo(0)
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, row.Session, proto.SendMessage{RequestID: "o1", Text: "Start with the docs."}); err != nil {
		t.Fatalf("the owner's message after the task: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	task, owner := slices.Index(h.engine.order, workTaskKey(row.Key)), slices.IndexFunc(h.engine.order, func(k string) bool { return strings.HasSuffix(k, ":o1") })
	if task < 0 || owner < task {
		t.Fatalf("the task at %d, the owner's message at %d: %q", task, owner, h.engine.order)
	}
}

// A session the engine made for the work key, whose answer was lost, is
// recorded as it was made, by the same call or by its run again: one
// session, with its task (ADR-0073).
func TestAWorkSessionMadeButNotRecordedIsRecordedAsMade(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createLost = errors.New("pocketd stopped")
	h.engine.mu.Unlock()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	work := team.Work{Task: "Fix the flaky test."}
	if _, err := h.server.TeamWork(ctx, call, work); err != nil {
		t.Fatalf("a lost create's session: %v", err)
	}
	h.engine.mu.Lock()
	made := h.engine.created
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWork(ctx, call, work); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Session == 0 {
		t.Fatalf("the work %+v, %v", row, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.created != made || h.engine.byKey[workKey(call)] != row.Session || h.engine.to[workTaskKey(row.Key)] != row.Session {
		t.Fatalf("%d sessions after %d; the work's is %d, the task went to %d", h.engine.created, made, row.Session, h.engine.to[workTaskKey(row.Key)])
	}
}

// A work session starts only from a profile that still names its
// repository with the checkout its worktree was made from, read again
// when its call runs again (ADR-0073).
func TestAWorkSessionStartsOnlyFromAProfileThatStillNamesItsRepository(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createErr = errors.New("the engine failed")
	h.engine.mu.Unlock()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil {
		t.Fatal("a failed create answered")
	}
	h.addProfiles(map[string]string{"agnes.md": "---\nmodel: faux/faux-1\nhome: ~\nrepositories:\n  frostyard/example: " + h.checkout + "-moved\n---\n# Agnes\n"})
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), "-moved now, not "+h.checkout+", which the work was recorded from") {
		t.Fatalf("a moved checkout: %v", err)
	}
	h.addProfiles(map[string]string{"agnes.md": "---\nmodel: faux/faux-1\nhome: ~\n---\n# Agnes\n"})
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), "works on no repository") {
		t.Fatalf("a repository gone: %v", err)
	}
	if row, err := h.store.WorkByKey(ctx, workKey(call)); err != nil || row.Session != 0 {
		t.Fatalf("the work %+v, %v", row, err)
	}
}

// An agent's own session started before team_work gets it at start; a
// supervisor's, a thread's specialist's and a work session do not, and a
// standing one takes it at its idle refresh (ADR-0073).
func TestAnOlderAgentsSessionGetsTeamWorkAtStart(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	row := startedWork(t, h, agnes.ID)
	thread := veraThread(t, h)
	duncan, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "duncan", Profile: "duncan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.UpgradeTeamTools(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for id, want := range map[int64]bool{agnes.ID: true, duncan.ID: true, h.super.ID: false, thread.Session: false, row.Session: false} {
		if h.engine.workTool[id] != want {
			t.Errorf("session %d given team_work: %v, want %v", id, h.engine.workTool[id], want)
		}
	}
}

// A work session's worktree is one pocketd never removes: it is recorded
// as a session's worktree, which a closed thread's cleanup never takes
// (ADR-0027, ADR-0054, ADR-0073).
func TestAWorkSessionsWorktreeIsKept(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	row := startedWork(t, h, agnes.ID)
	real, err := filepath.EvalSymlinks(row.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{row.Path, real} {
		if kept, err := h.store.SessionWorktreeAt(ctx, path); err != nil || !kept {
			t.Fatalf("%s is no session's worktree: %v, %v", path, kept, err)
		}
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatal(err)
	}
}

// writeAgnes rewrites Agnes's profile with front matter front.
func writeAgnes(h *teamHarness, front string) {
	h.addProfiles(map[string]string{"agnes.md": "---\nmodel: faux/faux-1\nhome: ~\n" + front + "---\n# Agnes\n"})
}

// recordedWork is the work of Agnes's call as its first run recorded it
// before it stopped: its record, and no worktree or session yet.
func recordedWork(t *testing.T, h *teamHarness, call TeamCall, title, task string) store.WorkRow {
	t.Helper()
	key := workKey(call)
	path, branch := team.WorkWorktree(profiles.Profile{ID: "agnes"}, h.checkout, title, key)
	row, err := h.store.StartWork(context.Background(), store.WorkRow{Key: key, Caller: call.Session, Agent: "agnes", Repository: "frostyard/example",
		Checkout: h.checkout, Path: path, Branch: branch, Title: title, Task: task})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// A call stopped while it waits to start its session, after its worktree
// was made, creates no session and sends no task; its run again starts it
// in the same worktree (Codex review of #174, round 1).
func TestAStoppedCallStartsNoWorkSession(t *testing.T) {
	h, agnes := workHarness(t)
	h.engine.mu.Lock()
	created := h.engine.created
	h.engine.mu.Unlock()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.server.beforeCreateLock = cancel
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), "team_work was stopped") {
		t.Fatalf("a stopped call: %v", err)
	}
	h.server.beforeCreateLock = nil
	h.engine.mu.Lock()
	after := h.engine.created
	h.engine.mu.Unlock()
	row, err := h.store.WorkByKey(context.Background(), workKey(call))
	if after != created || err != nil || row.Session != 0 {
		t.Fatalf("a stopped call made %d sessions; the work %+v, %v", after-created, row, err)
	}
	if _, err := os.Stat(row.Path); err != nil {
		t.Fatalf("the worktree made before the stop: %v", err)
	}
	if _, err := h.server.TeamWork(context.Background(), call, team.Work{Task: "Fix it."}); err != nil {
		t.Fatalf("the call run again: %v", err)
	}
	if row, err := h.store.WorkByKey(context.Background(), workKey(call)); err != nil || row.Session == 0 {
		t.Fatalf("the work after its run again %+v, %v", row, err)
	}
}

// A task not placed yet is submitted only while its worktree is the one
// its work made: one removed before the task went is dropped, and the
// caller hears so once, by the delivery pass the drop wakes; a task placed
// already is not checked again (Codex review of #174, rounds 1 and 2).
func TestADelayedTaskChecksItsWorktreeFirst(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.engine.onCreate = func(spec SessionSpec) {
		if spec.Work {
			h.engine.mu.Lock()
			h.engine.failTo = map[int64]bool{h.engine.next + 1: true}
			h.engine.mu.Unlock()
		}
	}
	row := startedWork(t, h, agnes.ID)
	h.engine.onCreate = nil
	if _, err := h.engine.Git(ctx, h.checkout, "worktree", "remove", "--force", row.Path); err != nil {
		t.Fatal(err)
	}
	h.failTo(0)
	// The pass the failed task asked for drops it, with nothing else asked
	// for since, and nothing else owed to the caller (the work session's
	// transcript is empty, so it has no answer); the notice is another
	// session's note, which that pass read its list of sessions before:
	// only the drop's wake brings it.
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{row.Session: {}}
	h.engine.mu.Unlock()
	h.server.sessionMu.Lock()
	h.server.again = false
	h.server.sessionMu.Unlock()
	h.runScheduled(1)
	h.runScheduled(10)
	h.engine.mu.Lock()
	_, sent := h.engine.texts[workTaskKey(row.Key)]
	notice, to := h.engine.texts[row.Key+workDroppedSuffix], h.engine.to[row.Key+workDroppedSuffix]
	h.engine.mu.Unlock()
	if sent {
		t.Fatal("the task went to a work session whose worktree is gone")
	}
	if to != agnes.ID || !strings.Contains(notice, "dropped this work session's task") {
		t.Fatalf("the caller's notice %q went to %d", notice, to)
	}
	if note, err := h.store.Note(ctx, workTaskKey(row.Key)); err != nil || note.DroppedAt == nil {
		t.Fatalf("the task %+v, %v", note, err)
	}
	if waits, err := h.server.workTaskWaits(ctx, row.Session); err != nil || waits {
		t.Fatalf("a dropped task still waits: %v %v", waits, err)
	}
	// A task placed already needs no check: its worktree's removal later
	// drops nothing.
	call := TeamCall{Session: agnes.ID, Task: 8, CallID: "w2"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix the docs."}); err != nil {
		t.Fatal(err)
	}
	placed, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Git(ctx, h.checkout, "worktree", "remove", "--force", placed.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	if note, err := h.store.Note(ctx, workTaskKey(placed.Key)); err != nil || note.SentAt == nil || note.DroppedAt != nil {
		t.Fatalf("the placed task %+v, %v", note, err)
	}
	if _, err := h.store.Note(ctx, placed.Key+workDroppedSuffix); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a placed task's notice: %v", err)
	}
}

// A run that ended with its answer, then a compaction (the wire's own
// kind), sends that answer, never "ended with no reply" (Codex review of
// #174, round 1).
func TestAWorkSessionsAnswerSurvivesATrailingCompaction(t *testing.T) {
	h, agnes := workHarness(t)
	row := startedWork(t, h, agnes.ID)
	h.engine.mu.Lock()
	h.engine.transcripts = map[int64][]proto.Entry{row.Session: {
		{ID: 1, Kind: proto.EntryUser, Text: "The task."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "Fixed it.", StopReason: "stop"},
		{ID: 3, Kind: proto.EntryCompaction, Text: "Summary."},
	}}
	h.engine.mu.Unlock()
	endRun(t, h, row.Session)
	want := "[S" + strconv.FormatInt(row.Session, 10) + " · work · Fix the flaky test.] Fixed it."
	if got, sends := workAnswersTo(h, agnes.ID); !slices.Equal(got, []string{want}) || sends != 1 {
		t.Fatalf("answers %q, sent %d times", got, sends)
	}
}

// A call run again that finds its work recorded and no session checks its
// caller and repository again before it makes the worktree: a profile
// that became a supervisor's, or lost the repository, gets no branch or
// folder (Codex review of #174, round 1).
func TestAReplayChecksItsCallerBeforeItsWorktree(t *testing.T) {
	for name, tc := range map[string]struct{ front, want string }{
		"a supervisor now":  {"role: supervisor\n", "delegate the work with team_delegate"},
		"no repository now": {"", "agnes works on no repository"},
		"another checkout":  {"repositories:\n  frostyard/example: /elsewhere/example\n", "which the work was recorded from"},
		"a private one now": {"private: true\n", "agnes is private"},
	} {
		t.Run(name, func(t *testing.T) {
			h, agnes := workHarness(t)
			ctx := context.Background()
			call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
			row := recordedWork(t, h, call, "Fix it.", "Fix it.")
			writeAgnes(h, tc.front)
			if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the run again: %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(row.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the worktree's folder was made: %v", err)
			}
			if _, held, err := h.store.WorktreeOf(ctx, row.Key); err != nil || held {
				t.Fatalf("the worktree was claimed: %v %v", held, err)
			}
			if branch, err := h.engine.Git(ctx, h.checkout, "branch", "--list", row.Branch); err != nil || branch != "" {
				t.Fatalf("the branch %q, %v", branch, err)
			}
		})
	}
}

// The work session's profile is read again after the models, right before
// the session starts: one made private while the models were read starts
// no session (Codex review of #174, round 1).
func TestTheWorkSessionsProfileIsReadAfterTheModels(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	path, _ := team.WorkWorktree(profiles.Profile{ID: "agnes"}, h.checkout, "Fix it.", workKey(call))
	changed := false
	h.engine.mu.Lock()
	h.engine.onModels = func() {
		if _, err := os.Stat(path); err == nil && !changed {
			changed = true
			writeAgnes(h, "private: true\nrepositories:\n  frostyard/example: "+h.checkout+"\n")
		}
	}
	created := h.engine.created
	h.engine.mu.Unlock()
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), "takes no work of its own now") {
		t.Fatalf("a profile made private as the models were read: %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if !changed || h.engine.created != created {
		t.Fatalf("changed %v; %d sessions after %d", changed, h.engine.created, created)
	}
}

// A profile no session can start from gets no work: a standing profile
// whose compactAt is not below its routed model's window, as
// Server.startable refuses it (Codex review of #174, round 1).
func TestAProfileThatCannotStartGetsNoWork(t *testing.T) {
	h, _ := workHarness(t)
	ctx := context.Background()
	front := "---\nmodel: faux/faux-1\nhome: ~\nsession: standing\nrepositories:\n  frostyard/example: " + h.checkout + "\n"
	h.addProfiles(map[string]string{"irulan.md": front + "---\n# Irulan\n"})
	irulan, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "irulan", Profile: "irulan"})
	if err != nil {
		t.Fatal(err)
	}
	h.addProfiles(map[string]string{"irulan.md": front + "compactAt: 1000\n---\n# Irulan\n"})
	call := TeamCall{Session: irulan.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), "compacts at 1000 tokens") {
		t.Fatalf("a profile that cannot start: %v", err)
	}
	if _, err := h.store.WorkByKey(ctx, workKey(call)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("its work was recorded: %v", err)
	}
}

// A work session recovered as the engine made it, whose lessons section
// is unknown, has its profile's lessons given it by the delivery pass
// (Codex review of #174, round 1).
func TestARecoveredWorkSessionGetsItsLessons(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createLost = errors.New("pocketd stopped")
	h.engine.mu.Unlock()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Session == 0 {
		t.Fatalf("the work %+v, %v", row, err)
	}
	if _, err := h.server.deliverAll(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if _, given := h.engine.lessons[row.Session]; !given {
		t.Fatalf("the recovered work session got no lessons: %v", h.engine.lessons)
	}
}

// Why a work's branch starts from the checkout's HEAD is kept with its
// record: a task its call's run again sends still says to fetch and
// compare (Codex review of #174, round 1).
func TestAWorkKeepsWhyItsBranchIsStale(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.engine.mu.Lock()
	h.engine.createErr = errors.New("the engine failed")
	h.engine.mu.Unlock()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil {
		t.Fatal("a failed create answered")
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Stale == "" {
		t.Fatalf("the work %+v, %v: no stale base kept", row, err)
	}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if task := h.engine.texts[workTaskKey(row.Key)]; !strings.Contains(task, team.StaleBase(row.Stale)) {
		t.Fatalf("the task of the run again:\n%s", task)
	}
}

// A call run again answers with the model the session was created on,
// whatever its model is now (Codex review of #174, round 1).
func TestAWorkReplayAnswersWithItsCreatedModel(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	first, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."})
	if err != nil {
		t.Fatal(err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSessionModel(ctx, row.Session, "faux", "faux-2", "", "owner"); err != nil {
		t.Fatal(err)
	}
	again, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."})
	if err != nil || again != first || strings.Contains(again, "faux-2") {
		t.Fatalf("a run again %q, %v; the first %q", again, err, first)
	}
}

// Why a work's branch starts from the checkout's HEAD is kept before its
// worktree is made: a keep that fails makes nothing, and the call run
// again decides the base again, so its task says to fetch and compare;
// never a made worktree whose replay decides no base (Codex review of
// #174, round 2).
func TestAWorkKeepsItsBaseBeforeItsWorktree(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	keep := h.server.keepWorkStale
	failed := false
	h.server.keepWorkStale = func(ctx context.Context, key, stale string) error {
		if !failed {
			failed = true
			return errors.New("the work database refused the write")
		}
		return keep(ctx, key, stale)
	}
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil || !strings.Contains(err.Error(), "the work database refused the write") {
		t.Fatalf("a base that could not be kept: %v", err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(row.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree was made before its base was kept: %v", err)
	}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	if row, err = h.store.WorkByKey(ctx, workKey(call)); err != nil || row.Stale == "" {
		t.Fatalf("the work %+v, %v: no stale base kept", row, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if task := h.engine.texts[workTaskKey(row.Key)]; !strings.Contains(task, team.StaleBase(row.Stale)) {
		t.Fatalf("the task of the run again:\n%s", task)
	}
}

// A call that started its work session answers from what it created the
// session with: a lookup of the creation record that fails once the task
// went reports no failed start of work that runs (Codex review of #174,
// round 2).
func TestAFreshWorkAnswersWithoutLookingItsSessionUp(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	h.engine.onWatch = func(id int64) {
		if id != agnes.ID {
			h.engine.mu.Lock()
			h.engine.createdErr = errors.New("the durable store is busy")
			h.engine.mu.Unlock()
		}
	}
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	answer, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."})
	h.engine.onWatch = nil
	h.engine.mu.Lock()
	h.engine.createdErr = nil
	h.engine.mu.Unlock()
	if err != nil {
		t.Fatalf("a call whose task went: %v", err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Session == 0 || !strings.Contains(answer, "work session "+strconv.FormatInt(row.Session, 10)) || !strings.Contains(answer, "faux/faux-1") {
		t.Fatalf("the answer %q; the work %+v, %v", answer, row, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.to[workTaskKey(row.Key)] != row.Session {
		t.Fatal("the task did not go")
	}
}

// A work whose session the engine made, but whose record failed, is
// recorded by a restarted pocketd's first delivery pass, with its task; a
// work the engine made no session for, as a call stopped before the
// session's creation, gets none (Codex review of #174, round 3).
func TestAPendingWorkIsRecordedAtStart(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	failed := false
	h.server.recordWorkSession = func(ctx context.Context, key string, session store.SessionRow, task store.NoteRow) (store.WorkRow, error) {
		if !failed {
			failed = true
			return store.WorkRow{}, errors.New("the work database refused the write")
		}
		return h.store.RecordWorkSession(ctx, key, session, task)
	}
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil {
		t.Fatal("a failed record answered")
	}
	stopped := recordedWork(t, h, TeamCall{Session: agnes.ID, Task: 8, CallID: "w2"}, "Stopped.", "Stopped.")
	h.engine.mu.Lock()
	made, created := h.engine.byKey[workKey(call)], h.engine.created
	h.engine.mu.Unlock()
	if row, err := h.store.WorkByKey(ctx, workKey(call)); err != nil || row.Session != 0 || made == 0 {
		t.Fatalf("the work %+v, %v; the engine's session %d", row, err, made)
	}
	if err := h.restarted(t).DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Session != made {
		t.Fatalf("the work after the start %+v, %v; the engine's session %d", row, err, made)
	}
	if left, err := h.store.WorkByKey(ctx, stopped.Key); err != nil || left.Session != 0 {
		t.Fatalf("the stopped work %+v, %v", left, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.to[workTaskKey(row.Key)] != made || h.engine.created != created {
		t.Fatalf("the task went to %d; %d sessions after %d", h.engine.to[workTaskKey(row.Key)], h.engine.created, created)
	}
}

// A call run again after its session was recorded answers from the work's
// record, with no lookup of the engine's creation record that could fail
// once the task went (Codex review of #174, round 3).
func TestAWorkReplayAnswersFromItsRecord(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	first, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.createdErr = errors.New("the durable store is busy")
	h.engine.mu.Unlock()
	again, err := h.restarted(t).TeamWork(ctx, call, team.Work{Task: "Fix it."})
	h.engine.mu.Lock()
	h.engine.createdErr = nil
	h.engine.mu.Unlock()
	if err != nil || again != first {
		t.Fatalf("a run again %q, %v; the first %q", again, err, first)
	}
}

// A base decided again replaces the one kept before, a fresh fetch's
// clearing an earlier failure's reason: a call stopped after its base was
// kept, before its worktree was made, and run again once origin answers,
// tells its work session nothing of a stale base (Codex review of #174,
// round 3).
func TestANewBaseReplacesTheKeptOne(t *testing.T) {
	h, agnes := workHarness(t)
	ctx := context.Background()
	keep := h.server.keepWorkStale
	stopped := false
	h.server.keepWorkStale = func(ctx context.Context, key, stale string) error {
		if err := keep(ctx, key, stale); err != nil {
			return err
		}
		if !stopped {
			stopped = true
			return errors.New("pocketd stopped")
		}
		return nil
	}
	call := TeamCall{Session: agnes.ID, Task: 7, CallID: "w1"}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err == nil {
		t.Fatal("a stopped call answered")
	}
	if row, err := h.store.WorkByKey(ctx, workKey(call)); err != nil || row.Stale == "" {
		t.Fatalf("the first base kept %+v, %v", row, err)
	}
	bare := filepath.Join(t.TempDir(), "origin.git")
	if _, err := h.engine.Git(ctx, h.checkout, "clone", "-q", "--bare", h.checkout, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Git(ctx, h.checkout, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.TeamWork(ctx, call, team.Work{Task: "Fix it."}); err != nil {
		t.Fatal(err)
	}
	row, err := h.store.WorkByKey(ctx, workKey(call))
	if err != nil || row.Stale != "" {
		t.Fatalf("the work after a fresh fetch %+v, %v", row, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if task := h.engine.texts[workTaskKey(row.Key)]; task == "" || strings.Contains(task, "could not fetch") {
		t.Fatalf("the task after a fresh fetch:\n%s", task)
	}
}
