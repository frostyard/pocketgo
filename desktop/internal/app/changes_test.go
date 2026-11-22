package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// t1Diff is the diff of T1's first changed file.
const t1Diff = `diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml
index 3b18e51..9d1c4a2 100644
--- a/.github/workflows/ci.yml
+++ b/.github/workflows/ci.yml
@@ -12,9 +12,12 @@ jobs:
   arm64:
-    runs-on: ubuntu-22.04-arm
+    runs-on: ubuntu-24.04-arm
+    timeout-minutes: 30
     steps:
       - uses: actions/checkout@v4
-      - run: make test
+      - name: Test
+        run: make test
+        env:
+          GOFLAGS: -count=1
       - uses: actions/upload-artifact@v4
         with:
           name: logs
`

// seedChanges gives T1's session, 11, what it changed in its worktree, and
// two of its files' diffs, the second cut.
func (x *testApp) seedChanges() {
	x.srv.SetChanges(proto.Changes{SessionID: 11, Repository: true, Top: "/home/team/projects/nsl--vera-fix-the-flaky-arm64-job-t1",
		Branch: "vera/fix-the-flaky-arm64-job-t1", Head: "9f3c2a1b7e6d5c4b3a291807f6e5d4c3b2a19087", Base: "origin/main",
		BaseRule: proto.ChangesBaseDefault, MergeBase: "4b1e0c9d8a7f6e5d4c3b2a190807f6e5d4c3b2a1", Added: 54, Removed: 9,
		Files: []proto.ChangedFile{
			{Path: ".github/workflows/ci.yml", Added: 6, Removed: 2, Committed: true, Uncommitted: true},
			{Path: "internal/run/retry.go", Added: 30, Removed: 7, Committed: true},
			{Path: "internal/run/retry_test.go", Added: 18, New: true, Uncommitted: true},
			{Path: "docs/arm64-runner.png", Binary: true, New: true, Uncommitted: true},
		}})
	x.srv.SetFileDiff(proto.FileDiff{SessionID: 11, Path: ".github/workflows/ci.yml", MergeBase: "4b1e0c9d8a7f6e5d4c3b2a190807f6e5d4c3b2a1", Diff: t1Diff})
	x.srv.SetFileDiff(proto.FileDiff{SessionID: 11, Path: "internal/run/retry.go", Diff: "@@ -1,2 +1,3 @@\n package run\n+\n+// Retry runs f again.\n", Cut: true})
}

// fileRow is the access label of a changed file's row.
func fileRow(f proto.ChangedFile) string {
	return state.FileName(f) + ", " + state.FileState(f) + ", " + state.FileCounts(f)
}

// The Changes view (ADR-0053): a thread's card counts its session's
// changes; the session's header shows the count and opens the pane beside
// it, with the files, where they are counted from, and a file's diff with
// its lines numbered and colored; a cut diff says so; Refresh reads them
// again; a session in no checkout shows no count. The issue's Done when,
// through the desktop.
func TestTheChangesSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.seedChanges()
	x.connect(t)
	x.a.goBoard()
	x.until(t, func() bool { _, ok := x.a.st.Data.Changes(11); return ok && !x.a.st.ChangesLoading(11) })
	x.hasAll(t, "4 files +54 −9")
	x.snapshot(t, "board-changes")

	x.click(t, "Changes: 4 files +54 −9")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 11 })
	if !x.a.changesPaneShown() {
		t.Fatal("the card's count opened no Changes pane")
	}
	sc, _ := x.a.st.Data.Changes(11)
	files := sc.Changes.Files
	x.hasAll(t, "Changes", "vera/fix-the-flaky-arm64-job-t1 against origin/main since 4b1e0c9d", ".github/workflows/ci.yml",
		"internal/run/retry_test.go", "new, not committed", "committed, and changed since", "binary", "Choose a file")
	x.click(t, fileRow(files[0]))
	x.until(t, func() bool { d, ok := x.a.st.Data.Diff(); return ok && d.Status == state.ListLoaded })
	x.hasAll(t, "+    timeout-minutes: 30", "-    runs-on: ubuntu-22.04-arm")
	if strings.Contains(strings.Join(x.tt.Texts(), "\n"), "Only the first") {
		t.Error("a whole diff says it is cut")
	}
	x.snapshot(t, "changes")

	// A cut diff says so.
	x.click(t, fileRow(files[1]))
	x.until(t, func() bool {
		d, ok := x.a.st.Data.Diff()
		return ok && d.Path == files[1].Path && d.Status == state.ListLoaded
	})
	x.hasAll(t, "Only the first 64 KB of this diff show: the rest is in the checkout.")

	// Refresh reads the changes and the diff shown again.
	before := len(x.srv.RequestsTo("GET", "/v1/sessions/11/changes"))
	x.click(t, "Refresh the changes")
	x.until(t, func() bool {
		return len(x.srv.RequestsTo("GET", "/v1/sessions/11/changes")) == before+1 && x.a.st.InFlight() == 0
	})

	// The header's count hides the pane, and shows it again.
	x.click(t, "Changes: 4 files +54 −9")
	if x.a.changesPaneShown() {
		t.Fatal("the header's count left the pane open")
	}
	x.click(t, "Changes: 4 files +54 −9")
	if !x.a.changesPaneShown() {
		t.Fatal("the header's count did not open the pane")
	}
	x.click(t, "Close the changes")
	if x.a.changesPaneShown() {
		t.Fatal("Close left the pane open")
	}

	// A session whose folder is in no checkout shows no count, and no pane.
	x.a.changes.open = true
	x.a.openSession(1)
	x.until(t, func() bool { c, ok := x.a.st.Data.Changes(1); return ok && c.Status == state.ListLoaded })
	if _, ok := x.tt.AccessByRole(ui.RoleButton, "Changes: 4 files +54 −9"); ok || x.a.changesPaneShown() {
		t.Fatal("a session in no checkout shows the Changes")
	}
}

// The Changes view on the iPhone (ADR-0053): the count under the top bar
// opens a sheet of the files; a file shows its diff in the sheet, with a
// way back to the files.
func TestThePhoneChangesSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.seedChanges()
	x.phoneApp(t, true, nil)
	x.tap(t, "Sessions")
	x.tap(t, "T1 · Fix the flaky arm64 job")
	x.until(t, func() bool {
		o := x.a.st.Data.Open()
		_, ok := x.a.st.Data.Changes(11)
		return o != nil && o.Loaded && o.ID == 11 && ok && !x.a.st.ChangesLoading(11)
	})
	x.within(t, ui.RoleButton, "Changes: 4 files +54 −9")
	x.tap(t, "Changes: 4 files +54 −9")
	sc, _ := x.a.st.Data.Changes(11)
	files := sc.Changes.Files
	x.hasAll(t, "Changes", ".github/workflows/ci.yml", "internal/run/retry_test.go", "vera/fix-the-flaky-arm64-job-t1 against origin/main since 4b1e0c9d")
	x.within(t, ui.RoleButton, "Close the changes")
	x.snapshot(t, "phone-changes-1-files")

	x.tap(t, fileRow(files[0]))
	x.until(t, func() bool { d, ok := x.a.st.Data.Diff(); return ok && d.Status == state.ListLoaded })
	x.hasAll(t, "+    timeout-minutes: 30", "Back to the files")
	x.snapshot(t, "phone-changes-2-diff")

	x.tap(t, "Back to the files")
	if _, ok := x.a.st.Data.Diff(); ok {
		t.Fatal("Back left the diff shown")
	}
	x.hasAll(t, "internal/run/retry_test.go")
	x.tap(t, "Close the changes")
	x.settle()
	if x.a.phone.changes {
		t.Fatal("Close left the sheet open")
	}
}

// A read of a session's changes that failed shows in its header, not as a
// session in no checkout: the pane says why, and Refresh reads again
// (Codex review of #109, round 1).
func TestAFailedChangesReadSaysWhy(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.seedChanges()
	path := "/v1/sessions/11/changes"
	x.srv.FailOnce("GET", path, http.StatusInternalServerError)
	x.connect(t)
	x.a.openSession(11)
	x.until(t, func() bool {
		c, ok := x.a.st.Data.Changes(11)
		return ok && c.Status == state.ListFailed && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "Changes not read")
	x.srv.FailOnce("GET", path, http.StatusInternalServerError)
	x.click(t, "Changes: Changes not read")
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
	if !x.a.changesPaneShown() {
		t.Fatal("a failed read opened no pane")
	}
	x.hasAll(t, "Reading the changes failed: internal: made to fail")
	x.click(t, "Refresh the changes")
	x.until(t, func() bool { c, ok := x.a.st.Data.Changes(11); return ok && c.Status == state.ListLoaded })
	x.hasAll(t, "4 files +54 −9", ".github/workflows/ci.yml")
}
