package nsgoclient

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func TestFilesWorkspaceAndSessionRail(t *testing.T) {
	s := pocketdtest.New()
	s.SetFileRoots(proto.FileRoots{SessionFolders: true, Roots: []proto.FileRoot{{ID: "maintainer", Name: "Maintainer", Path: "/agents/maintainer"}, {ID: "reviewer", Name: "Reviewer", Path: "/agents/reviewer"}}})
	s.AddSession(proto.Session{ID: 41, Title: "Review the release", Cwd: "/projects/pocketgo--maintainer-release", State: proto.SessionIdle}, proto.Entry{ID: 1, Kind: proto.EntryAssistant, Text: "The release notes are ready for review."})
	s.AddSession(proto.Session{ID: 42, Title: "Second checkout", Cwd: "/projects/other", State: proto.SessionIdle})
	root := proto.SessionFilesRoot(41)
	for _, src := range []string{"maintainer", root} {
		s.SetFileFolder(proto.FileFolder{Root: src, Entries: []proto.FileEntry{{Name: "docs", Kind: proto.FileEntryFolder}, {Name: "README.md", Kind: proto.FileEntryFile}}})
		s.SetFileFolder(proto.FileFolder{Root: src, Path: "docs", Entries: []proto.FileEntry{{Name: "release.md", Kind: proto.FileEntryFile}}})
		content := "Maintainer home notes."
		if src == root {
			content = "Session worktree notes."
		}
		notes := "# Release notes\n\n" + content + "\n\n## Before tagging\n\n- Verify the Linux build.\n- Check the browser preview.\n- Record the results."
		s.SetFileContent(proto.FileContent{Root: src, Path: "docs/release.md", Kind: proto.FileMarkdown, Text: notes, Size: int64(len(notes))})
	}
	s.SetFileFolder(proto.FileFolder{Root: "reviewer", Entries: []proto.FileEntry{{Name: "checklist.txt", Kind: proto.FileEntryFile}}})
	s.SetFileFolder(proto.FileFolder{Root: proto.SessionFilesRoot(42), Entries: []proto.FileEntry{{Name: "other.txt", Kind: proto.FileEntryFile}}})
	app := signedInApp(t, s, 1380, 820)
	app.Press("text:Files")
	app.WaitFor("text:docs", 5*time.Second)
	app.Press("text:docs")
	app.WaitFor("text:release.md", 5*time.Second)
	app.Press("text:release.md")
	app.WaitFor("text:Maintainer home notes.", 5*time.Second)
	if app.Find("id:files-tree").Bounds().Origin.X >= app.Find("text:Maintainer home notes.").Bounds().Origin.X {
		t.Fatal("tree should be left of preview")
	}
	app.Screenshot("files-workspace")
	app.Press("text:Reviewer")
	app.WaitFor("text:checklist.txt", 5*time.Second)
	app.Press("label:Back")
	app.WaitFor("text:Maintainer home notes.", 5*time.Second)
	app.Press("text:Review the release")
	app.Type("label:Message", "Draft in progress")
	app.Press("label:Session files")
	app.WaitFor("text:docs", 5*time.Second)
	app.Press("text:docs")
	app.WaitFor("text:release.md", 5*time.Second)
	app.Press("text:release.md")
	app.WaitFor("text:Session worktree notes.", 5*time.Second)
	if app.Find("label:Message").Text != "Draft in progress" {
		t.Fatal("lost draft")
	}
	app.Screenshot("session-files")
	app.Press("text:Second checkout")
	app.WaitFor("text:other.txt", 5*time.Second)
	if app.Exists("text:Session worktree notes.") {
		t.Fatal("previous session preview survived")
	}
	app.Press("label:Session details")
	app.WaitFor("text:Pin session", 5*time.Second)
	if app.Exists("id:files-tree") {
		t.Fatal("file rail did not close")
	}
}

func TestSessionFilesOnAnOlderService(t *testing.T) {
	s := pocketdtest.New()
	s.SetFileRoots(proto.FileRoots{Roots: []proto.FileRoot{{ID: "agent", Name: "Agent", Path: "/agents/one"}}})
	s.AddSession(proto.Session{ID: 5, Title: "Home project", Cwd: "/agents/one/project", State: proto.SessionIdle})
	s.AddSession(proto.Session{ID: 6, Title: "External checkout", Cwd: "/repos/external", State: proto.SessionIdle})
	s.SetFileFolder(proto.FileFolder{Root: "agent", Path: "project", Entries: []proto.FileEntry{{Name: "notes.txt", Kind: proto.FileEntryFile}}})
	s.SetFileContent(proto.FileContent{Root: "agent", Path: "project/notes.txt", Kind: proto.FileText, Text: "Legacy preview"})
	app := signedInApp(t, s, 1230, 800)
	app.Press("text:Home project")
	app.Press("label:Session files")
	app.WaitFor("text:notes.txt", 5*time.Second)
	app.Press("text:notes.txt")
	app.WaitFor("text:Legacy preview", 5*time.Second)
	app.Press("text:External checkout")
	app.WaitFor("text:Update pocketd to browse /repos/external outside the advertised agent homes.", 5*time.Second)
	if app.Exists("text:Legacy preview") {
		t.Fatal("showed the previous folder")
	}
	app.Press("text:Home project")
	app.Resize(500, 760)
	if !app.Exists("id:files-tree") {
		app.Press("label:Session files")
	}
	app.WaitFor("text:notes.txt", 5*time.Second)
	app.Press("text:notes.txt")
	app.WaitFor("text:Legacy preview", 5*time.Second)
	app.Press("text:File tree")
	app.WaitFor("text:notes.txt", 5*time.Second)
	if app.Exists("text:Legacy preview") {
		t.Fatal("compact tree did not return")
	}
}

func TestWideSessionRails(t *testing.T) {
	s := pocketdtest.New()
	s.SetFileRoots(proto.FileRoots{SessionFolders: true})
	s.AddSession(proto.Session{ID: 19, Title: "Wide workspace", Cwd: "/projects/pocketgo", State: proto.SessionIdle})
	s.SetFileFolder(proto.FileFolder{Root: proto.SessionFilesRoot(19)})
	app := signedInApp(t, s, 1760, 880)
	app.Press("text:Wide workspace")
	app.Press("label:Session details")
	app.Press("label:Session files")
	app.WaitFor("text:Empty folder", 5*time.Second)
	app.Find("text:Pin session")
	app.Find("label:Message")
	app.Resize(1250, 800)
	app.WaitUntil(3*time.Second, func() bool { return !app.Exists("text:Pin session") && app.Exists("id:files-tree") })
}
