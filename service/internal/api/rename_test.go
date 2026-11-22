package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// chani is a private agent's profile, at home (ADR-0042).
const chani = "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Chani\n"

// sessionEvents records the session events of the harness's server.
func sessionEvents(h *harness) func() []proto.Session {
	var mu sync.Mutex
	var seen []proto.Session
	emit := h.server.emit
	h.server.emit = func(e proto.Event) {
		if e.Type == proto.EventSession {
			mu.Lock()
			seen = append(seen, *e.Session)
			mu.Unlock()
		}
		emit(e)
	}
	return func() []proto.Session {
		mu.Lock()
		defer mu.Unlock()
		return append([]proto.Session(nil), seen...)
	}
}

// wantError fails unless err is pocketd's status and code.
func wantError(t *testing.T, what string, err error, status int, code proto.ErrorCode) *proto.Error {
	t.Helper()
	var e *proto.Error
	if !errors.As(err, &e) || e.Status != status || e.Code != code {
		t.Fatalf("%s: %v, want %d %s", what, err, status, code)
	}
	return e
}

// The owner renames a private agent's session (ADR-0058, rule 68): the
// title is trimmed, the session carries renamable in every copy, its event
// goes out with the new title, and it keeps its updatedAt and its place in
// the list, since a rename is not activity. At most 200 bytes as sent; an
// empty title gives the default. A retry answers the session as it is and
// renames nothing again, and publishes it again; the id reused with
// another title, or for another session, is refused.
func TestTheOwnerRenamesAPrivateAgentsSession(t *testing.T) {
	h := newHarness(t)
	h.writeProfiles(map[string]string{"chani.md": chani, "vera.md": "---\nmodel: faux/faux-1\nhome: ~\n---\n# Vera\n"})
	ctx := context.Background()
	mine, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Profile: "chani", Title: "Notes"})
	if err != nil || !mine.Renamable {
		t.Fatalf("chani's session %+v, %v", mine, err)
	}
	h.clock.Advance(time.Minute)
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v", Profile: "vera"})
	if err != nil || vera.Renamable {
		t.Fatalf("vera's session %+v, %v", vera, err)
	}
	events := sessionEvents(h)
	h.clock.Advance(time.Minute)
	renamed, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r1", Title: "  Garden plan  "})
	if err != nil || renamed.Title != "Garden plan" || !renamed.Renamable || !renamed.UpdatedAt.Equal(mine.UpdatedAt) || renamed.Cwd != mine.Cwd {
		t.Fatalf("the rename %+v, %v", renamed, err)
	}
	if got := events(); len(got) != 1 || got[0].ID != mine.ID || got[0].Title != "Garden plan" || !got[0].Renamable || !got[0].UpdatedAt.Equal(mine.UpdatedAt) {
		t.Fatalf("the session events %+v", got)
	}
	list, err := h.client.Sessions(ctx)
	if err != nil || len(list) != 2 || list[0].ID != vera.ID || list[1].ID != mine.ID || list[1].Title != "Garden plan" || !list[1].Renamable || list[0].Renamable {
		t.Fatalf("the list %+v, %v", list, err)
	}
	if detail, err := h.client.Session(ctx, mine.ID); err != nil || detail.Session.Title != "Garden plan" || !detail.Session.Renamable {
		t.Fatalf("the detail %+v, %v", detail.Session, err)
	}

	// 200 bytes as sent, white space included.
	long := strings.Repeat("a", proto.MaxTitle)
	if got, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r2", Title: long}); err != nil || got.Title != long {
		t.Fatalf("200 bytes: %q, %v", got.Title, err)
	}
	_, err = h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r3", Title: " " + long})
	wantError(t, "201 bytes", err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	// An empty title gives the default.
	if got, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r4", Title: " \t "}); err != nil || got.Title != proto.DefaultTitle(mine.ID) {
		t.Fatalf("an empty title: %q, %v", got.Title, err)
	}

	// A retry answers as the session is now, and publishes it again.
	before := len(events())
	again, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r1", Title: "  Garden plan  "})
	if err != nil || again.Title != proto.DefaultTitle(mine.ID) {
		t.Fatalf("the retry %+v, %v", again, err)
	}
	if got := events(); len(got) != before+1 || got[len(got)-1].Title != proto.DefaultTitle(mine.ID) {
		t.Fatalf("the retry's events %+v", got[before:])
	}
	_, err = h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r1", Title: "Beds"})
	wantError(t, "the id with another title", err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.RenameSession(ctx, vera.ID, proto.RenameSession{RequestID: "r1", Title: "  Garden plan  "})
	wantError(t, "the id for another session", err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.RenameSession(ctx, 999, proto.RenameSession{RequestID: "r5", Title: "Gone"})
	wantError(t, "no session", err, http.StatusNotFound, proto.ErrorCodeNotFound)
	_, err = h.client.RenameSession(ctx, mine.ID, proto.RenameSession{Title: "No id"})
	wantError(t, "no request id", err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if row, _ := h.store.Session(ctx, mine.ID); row.Title != proto.DefaultTitle(mine.ID) || !row.UpdatedAt.Equal(mine.UpdatedAt) {
		t.Fatalf("the row %+v", row)
	}
}

// Only a private agent's session is renamed, its profile as loaded now
// (ADR-0058): the owner's own session, a team agent's, the supervisor's
// standing one, a thread's session whose agent became private since, a
// gather's, a private supervisor's and one whose profile is gone are
// refused with the rule's reason, change nothing and keep no record; none
// carries renamable. A team agent's own session renames once its profile
// is private, and no longer once it is not.
func TestOnlyAPrivateAgentsSessionIsRenamed(t *testing.T) {
	h := newTeamHarness(t)
	ctx := context.Background()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(h.profileDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("alia.md", "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Alia\n")
	write("ghola.md", chani)
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "plain", Cwd: h.home, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	duncan := duncanSession(t, h, "d1")
	alia, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "alia", Profile: "alia"})
	if err != nil {
		t.Fatal(err)
	}
	ghola, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "ghola", Profile: "ghola"})
	if err != nil || !ghola.Renamable {
		t.Fatalf("ghola's session %+v, %v", ghola, err)
	}
	if _, err := h.server.TeamDelegate(ctx, TeamCall{Session: h.super.ID, CallID: "c1"}, team.Delegation{Agent: "duncan", Task: "Tidy up"}); err != nil {
		t.Fatal(err)
	}
	thread, err := h.store.Thread(ctx, 1)
	if err != nil || thread.Session == 0 {
		t.Fatalf("thread %+v, %v", thread, err)
	}
	gather, err := h.store.InsertSession(ctx, store.SessionRow{ID: 900, Title: "G1 · proposals", Cwd: h.home, Provider: "faux", Model: "faux-1", Profile: "ghola", CreatedBy: gatherCreator})
	if err != nil {
		t.Fatal(err)
	}
	// duncan becomes private, and ghola's profile goes.
	write("duncan.md", "---\nmodel: faux/faux-1\nhome: ~\nprivate: true\n---\n# Duncan\n")
	if err := os.Remove(filepath.Join(h.profileDir, "ghola.md")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		session int64
		why     string
	}{
		{"the owner's own", plain.ID, "is your own, started from no profile"},
		{"the supervisor's standing one", h.super.ID, "is odrade's, which is on the team"},
		{"a thread's", thread.Session, "is the team's: pocketd started it for a thread"},
		{"a gather's", gather.ID, "is the team's: pocketd started it for a thread"},
		{"a private supervisor's", alia.ID, "is supervisor alia's"},
		{"one whose profile is gone", ghola.ID, "profile ghola is gone or invalid now"},
	} {
		before, err := h.store.Session(ctx, tc.session)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.client.RenameSession(ctx, tc.session, proto.RenameSession{RequestID: "x", Title: "Renamed"})
		e := wantError(t, tc.name, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
		if !strings.Contains(e.Message, tc.why) || !strings.HasSuffix(e.Message, "only a private agent's session may be renamed") {
			t.Errorf("%s: %q, want %q", tc.name, e.Message, tc.why)
		}
		if after, _ := h.store.Session(ctx, tc.session); after.Title != before.Title {
			t.Errorf("%s was renamed %q", tc.name, after.Title)
		}
		if s, err := h.server.session(ctx, tc.session); err != nil || s.Renamable {
			t.Errorf("%s carries renamable: %+v, %v", tc.name, s, err)
		}
	}
	// A refusal kept no record: the same id renames duncan's own session,
	// whose profile is private now.
	got, err := h.client.RenameSession(ctx, duncan, proto.RenameSession{RequestID: "x", Title: "Renamed"})
	if err != nil || got.Title != "Renamed" || !got.Renamable {
		t.Fatalf("duncan's own session, private now: %+v, %v", got, err)
	}
	write("duncan.md", "---\nmodel: faux/faux-1\nhome: ~\n---\n# Duncan\n")
	_, err = h.client.RenameSession(ctx, duncan, proto.RenameSession{RequestID: "y", Title: "Again"})
	wantError(t, "duncan's own session, on the team again", err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	list, err := h.client.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Renamable {
			t.Errorf("session %d (%s) lists renamable", s.ID, s.Profile)
		}
	}
}

// A rename changes the title alone (ADR-0058): a worktree session's
// folder, branch and recorded worktree, named from its first title at its
// start (ADR-0027), stay as they are, and no git runs.
func TestARenameLeavesAWorktreeSessionsWorktree(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	request := proto.CreateSession{RequestID: "w1", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true, Title: "Fix CI"}
	s, err := h.client.CreateSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	path, branch := proto.SessionWorktree(h.chairlift, "duncan", "Fix CI", "w1")
	key := "u:" + h.device + ":w1"
	kept, ok, err := h.store.WorktreeOf(ctx, key)
	if err != nil || !ok || s.Cwd != path {
		t.Fatalf("the worktree %+v %v %v, session %+v", kept, ok, err, s)
	}
	h.writeProfile(t, "duncan.md", h.duncan(h.chairlift, true))
	h.mu.Lock()
	gits := len(h.gits)
	h.mu.Unlock()
	renamed, err := h.client.RenameSession(ctx, s.ID, proto.RenameSession{RequestID: "n1", Title: "Chairlift release"})
	if err != nil || renamed.Title != "Chairlift release" || renamed.Cwd != path {
		t.Fatalf("the rename %+v, %v", renamed, err)
	}
	h.mu.Lock()
	ran := h.gits[gits:]
	h.mu.Unlock()
	if len(ran) != 0 {
		t.Fatalf("the rename ran git %v", ran)
	}
	if again, ok, err := h.store.WorktreeOf(ctx, key); err != nil || !ok || again != kept {
		t.Fatalf("the recorded worktree %+v, want %+v (%v %v)", again, kept, ok, err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("the worktree's folder: %v", err)
	}
	if got := hostGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("the worktree is on %q, want %q", got, branch)
	}
	if row, _ := h.store.Session(ctx, s.ID); row.Cwd != path {
		t.Fatalf("the session works in %q", row.Cwd)
	}
}

// failingProfiles is a profile folder whose reads fail with err.
type failingProfiles struct {
	ProfileSource
	err error
}

func (f failingProfiles) Find(string) (profiles.Profile, error) { return profiles.Profile{}, f.err }
func (f failingProfiles) Load() ([]profiles.Profile, []profiles.Problem, error) {
	return nil, nil, f.err
}

// A profile pocketd cannot read now refuses a rename as internal, which a
// later one tries again, and a profile folder that is no longer the one
// pocketd protects as conflict; neither renames, and the session carries
// no renamable. Without profiles, a rename is refused with why.
func TestARenameWhoseProfileCannotBeReadChangesNothing(t *testing.T) {
	h := newHarness(t)
	dir, _ := h.writeProfiles(map[string]string{"chani.md": chani})
	ctx := context.Background()
	mine, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Profile: "chani"})
	if err != nil {
		t.Fatal(err)
	}
	folder := profiles.Folder{Dir: dir, Home: mine.Cwd}
	for _, tc := range []struct {
		err    error
		status int
		code   proto.ErrorCode
	}{
		{&fs.PathError{Op: "open", Path: filepath.Join(dir, "chani.md"), Err: syscall.EACCES}, http.StatusInternalServerError, proto.ErrorCodeInternal},
		{profiles.ErrFolderChanged, http.StatusConflict, proto.ErrorCodeConflict},
	} {
		h.server.SetProfiles(failingProfiles{ProfileSource: folder, err: tc.err})
		_, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r", Title: "Renamed"})
		wantError(t, tc.err.Error(), err, tc.status, tc.code)
		if s, err := h.server.session(ctx, mine.ID); err != nil || s.Renamable || s.Title != mine.Title {
			t.Fatalf("%v: the session %+v, %v", tc.err, s, err)
		}
		if list, err := h.client.Sessions(ctx); err != nil || len(list) != 1 || list[0].Renamable {
			t.Fatalf("%v: the list %+v, %v", tc.err, list, err)
		}
	}
	h.server.SetProfiles(nil)
	_, err = h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r", Title: "Renamed"})
	if e := wantError(t, "without profiles", err, http.StatusBadRequest, proto.ErrorCodeBadRequest); !strings.HasPrefix(e.Message, "pocketd runs without profiles") {
		t.Fatalf("without profiles: %q", e.Message)
	}
	h.server.SetProfiles(folder)
	if got, err := h.client.RenameSession(ctx, mine.ID, proto.RenameSession{RequestID: "r", Title: "Renamed"}); err != nil || got.Title != "Renamed" {
		t.Fatalf("once the profile reads: %+v, %v", got, err)
	}
}
