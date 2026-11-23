package state

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// TestFileCrumbsNameEachStep covers homes, unknown roots, folders and files.
func TestFileCrumbsNameEachStep(t *testing.T) {
	roots := proto.FileRoots{Roots: []proto.FileRoot{{ID: "a", Name: "Agent"}}}
	for _, tt := range []struct {
		name string
		spot FileSpot
		want []Crumb
	}{
		{"homes", FileSpot{}, []Crumb{{Label: "Homes"}}},
		{"root", FileSpot{Root: "a"}, []Crumb{{Label: "Homes"}, {Label: "Agent", Spot: FileSpot{Root: "a"}}}},
		{"unknown", FileSpot{Root: "b"}, []Crumb{{Label: "Homes"}, {Label: "b", Spot: FileSpot{Root: "b"}}}},
		{"folder", FileSpot{Root: "a", Path: "reports/x"}, []Crumb{{Label: "Homes"}, {Label: "Agent", Spot: FileSpot{Root: "a"}}, {Label: "reports", Spot: FileSpot{Root: "a", Path: "reports"}}, {Label: "x", Spot: FileSpot{Root: "a", Path: "reports/x"}}}},
		{"file", FileSpot{Root: "a", Path: "reports/x.md", File: true}, []Crumb{{Label: "Homes"}, {Label: "Agent", Spot: FileSpot{Root: "a"}}, {Label: "reports", Spot: FileSpot{Root: "a", Path: "reports"}}, {Label: "x.md", Spot: FileSpot{Root: "a", Path: "reports/x.md", File: true}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FileCrumbs(roots, tt.spot); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// TestSortFileEntriesPutsFoldersFirst preserves server order without changing its slice.
func TestSortFileEntriesPutsFoldersFirst(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []proto.FileEntry
		want []string
	}{
		{"empty", nil, nil},
		{"mixed", []proto.FileEntry{{Name: "b", Kind: proto.FileEntryFile}, {Name: "z", Kind: proto.FileEntryFolder}, {Name: "a", Kind: proto.FileEntryFile}, {Name: "x", Kind: proto.FileEntryFolder}, {Name: "o", Kind: proto.FileEntryOther}}, []string{"z", "x", "b", "a", "o"}},
		{"files", []proto.FileEntry{{Name: "b"}, {Name: "a"}}, []string{"b", "a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.in)
			out := SortFileEntries(tt.in)
			var names []string
			for _, e := range out {
				names = append(names, e.Name)
			}
			if !slices.Equal(names, tt.want) || !reflect.DeepEqual(tt.in, before) {
				t.Fatalf("%+v", out)
			}
			if len(out) > 0 {
				out[0].Name = "changed"
				if !reflect.DeepEqual(tt.in, before) {
					t.Fatal("aliased input")
				}
			}
		})
	}
}

// TestEntryOpensOnlyWhatPocketdReads excludes secrets and unreadable entry kinds.
func TestEntryOpensOnlyWhatPocketdReads(t *testing.T) {
	for _, tt := range []struct {
		kind                 string
		withheld, link, want bool
	}{
		{proto.FileEntryFolder, false, false, true}, {proto.FileEntryFile, false, false, true}, {proto.FileEntryFile, false, true, true}, {proto.FileEntryFolder, true, false, false}, {proto.FileEntryFile, true, true, false}, {proto.FileEntryOther, false, true, false}, {"unknown", false, false, false},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			if got := EntryOpens(proto.FileEntry{Kind: tt.kind, Withheld: tt.withheld, Link: tt.link}); got != tt.want {
				t.Fatal(got)
			}
		})
	}
}

// TestFileSizeIsBinaryUnits checks each unit boundary.
func TestFileSizeIsBinaryUnits(t *testing.T) {
	for _, tt := range []struct {
		n    int64
		want string
	}{{-1, ""}, {0, "0 bytes"}, {1, "1 byte"}, {1023, "1023 bytes"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"}, {1048576, "1.0 MiB"}, {1 << 30, "1.0 GiB"}} {
		t.Run(tt.want, func(t *testing.T) {
			if got := FileSize(tt.n); got != tt.want {
				t.Fatal(got)
			}
		})
	}
}

// TestEntryCaptionSaysWhy includes link identity even for withheld names.
func TestEntryCaptionSaysWhy(t *testing.T) {
	for _, tt := range []struct {
		e    proto.FileEntry
		want string
	}{
		{proto.FileEntry{Kind: proto.FileEntryFolder}, "Folder"},
		{proto.FileEntry{Kind: proto.FileEntryFolder, Link: true}, "Folder, a link"},
		{proto.FileEntry{Kind: proto.FileEntryFile, Size: 1}, "1 byte"},
		{proto.FileEntry{Kind: proto.FileEntryOther}, "Not shown"},
		{proto.FileEntry{Kind: proto.FileEntryOther, Link: true}, "Not shown, a link"},
		{proto.FileEntry{Withheld: true}, "Withheld: its name looks like it holds a secret"},
		{proto.FileEntry{Withheld: true, Link: true}, "Withheld: its name looks like it holds a secret, a link"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			if got := EntryCaption(tt.e); got != tt.want {
				t.Fatal(got)
			}
		})
	}
}

// TestFolderNotesSayWhatIsLeftOut names both listing bounds.
func TestFolderNotesSayWhatIsLeftOut(t *testing.T) {
	partial := "This folder holds more than 10,000 names: these are from the first 10,000 read."
	for _, tt := range []struct {
		f    proto.FileFolder
		want []string
	}{
		{proto.FileFolder{}, nil}, {proto.FileFolder{More: 1}, []string{"1 more entry is not listed."}},
		{proto.FileFolder{More: 2}, []string{"2 more entries are not listed."}},
		{proto.FileFolder{Partial: true}, []string{partial}},
		{proto.FileFolder{More: 1, Partial: true}, []string{"1 more entry is not listed.", partial}},
	} {
		t.Run(strings.Join(tt.want, " "), func(t *testing.T) {
			if got := FolderNotes(tt.f); !reflect.DeepEqual(got, tt.want) {
				t.Fatal(got)
			}
		})
	}
}

// TestFileShowsAndFileNote keeps unsupported content out of the renderer.
func TestFileShowsAndFileNote(t *testing.T) {
	for _, tt := range []struct{ kind, note, shows, want string }{
		{proto.FileMarkdown, "ignored", "markdown", ""}, {proto.FileText, "ignored", "text", ""},
		{proto.FileImage, "", "image", ""}, {proto.FileImage, "resized", "image", "Resized"},
		{proto.FileBinary, "not UTF-8", "", "Not UTF-8"}, {proto.FileLarge, "over limit", "", "Over limit"},
		{proto.FileBinary, "", "", "Not shown."}, {proto.FileLarge, "", "", "Not shown."},
		{"unknown", "ignored", "", ""}, {proto.FileImage, "éclair", "image", "Éclair"},
	} {
		t.Run(tt.kind+tt.note, func(t *testing.T) {
			c := proto.FileContent{Kind: tt.kind, Note: tt.note}
			if FileShows(c) != tt.shows || FileNote(c) != tt.want {
				t.Fatalf("%q %q", FileShows(c), FileNote(c))
			}
		})
	}
}

// TestParentAndJoinFilePath handles the home's empty path.
func TestParentAndJoinFilePath(t *testing.T) {
	for _, tt := range []struct{ p, want string }{{"", ""}, {"a", ""}, {"a/b", "a"}, {"a/b/c", "a/b"}} {
		t.Run("parent "+tt.p, func(t *testing.T) {
			if ParentPath(tt.p) != tt.want {
				t.Fatal(ParentPath(tt.p))
			}
		})
	}
	for _, tt := range []struct{ dir, name, want string }{{"", "a", "a"}, {"a", "b", "a/b"}, {"a/b", "c.md", "a/b/c.md"}} {
		t.Run("join "+tt.want, func(t *testing.T) {
			if JoinFilePath(tt.dir, tt.name) != tt.want {
				t.Fatal(tt)
			}
		})
	}
}

// TestALateFolderAnswerNeverUndoesANewerOne also refuses answers after leaving and revisiting.
func TestALateFolderAnswerNeverUndoesANewerOne(t *testing.T) {
	var d Data
	old, newer := d.BeginFolder("a", "x"), d.BeginFolder("a", "x")
	d.SetFolder(newer, "a", "x", proto.FileFolder{Real: "new"})
	d.SetFolder(old, "a", "x", proto.FileFolder{Real: "old"})
	d.SetFolderError(old, "a", "x", "late")
	if f, _ := d.FilesFolder(); f.Folder.Real != "new" || f.Status != ListLoaded || f.Folder.Entries == nil {
		t.Fatal(f)
	}
	d.BeginFolder("a", "y")
	d.SetFolder(newer, "a", "x", proto.FileFolder{Real: "wrong"})
	if f, _ := d.FilesFolder(); f.Path != "y" || f.Status != ListLoading {
		t.Fatal(f)
	}
	d.BeginFolder("a", "x")
	d.SetFolder(newer, "a", "x", proto.FileFolder{Real: "wrong"})
	if f, _ := d.FilesFolder(); f.Folder.Real != "" {
		t.Fatal(f)
	}
}

// TestAFolderErrorKeepsTheListing keeps stale data visible with its failure.
func TestAFolderErrorKeepsTheListing(t *testing.T) {
	var d Data
	f := d.BeginFolder("a", "")
	d.SetFolder(f, "a", "", proto.FileFolder{Real: "kept"})
	d.SetFolderError(d.BeginFolder("a", ""), "a", "", "failed")
	if got, _ := d.FilesFolder(); got.Folder.Real != "kept" || got.Status != ListFailed || got.Err != "failed" {
		t.Fatal(got)
	}
}

// TestAFileErrorKeepsTheContent rejects older answers and preserves a file on failure.
func TestAFileErrorKeepsTheContent(t *testing.T) {
	var d Data
	old, newer := d.BeginFile("a", "x"), d.BeginFile("a", "x")
	d.SetFile(newer, "a", "x", proto.FileContent{Text: "kept"})
	d.SetFile(old, "a", "x", proto.FileContent{Text: "old"})
	d.SetFileError(old, "a", "x", "late")
	d.SetFileError(d.BeginFile("a", "x"), "a", "x", "failed")
	if got, _ := d.FilesFile(); got.Content.Text != "kept" || got.Status != ListFailed || got.Err != "failed" {
		t.Fatal(got)
	}
	d.BeginFile("a", "y")
	d.SetFile(newer, "a", "x", proto.FileContent{Text: "wrong"})
	if got, _ := d.FilesFile(); got.Path != "y" || got.Content.Text != "" {
		t.Fatal(got)
	}
}

// filesFixture gives the store a home without reading any file system path.
func filesFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.SetFileRoots(proto.FileRoots{Roots: []proto.FileRoot{{ID: "a", Name: "Agent", Path: "/example/agent"}}})
	h.connect(t)
	return h
}

// TestOpeningAFileLoadsItsFolderToo reuses the sibling folder and refreshes both selections.
func TestOpeningAFileLoadsItsFolderToo(t *testing.T) {
	h := filesFixture(t)
	h.srv.SetFileFolder(proto.FileFolder{Root: "a", Path: "reports/x", Entries: []proto.FileEntry{{Name: "README.md", Kind: proto.FileEntryFile}}})
	for _, p := range []string{"reports/x/README.md", "reports/x/other.md"} {
		h.srv.SetFileContent(proto.FileContent{Root: "a", Path: p, Kind: proto.FileMarkdown, Text: p})
	}
	h.store.OpenFiles(FileSpot{Root: "a", Path: "reports/x/README.md", File: true})
	h.settle(t)
	folder, ok := h.store.Data.FilesFolder()
	if !ok || folder.Path != "reports/x" || folder.Status != ListLoaded {
		t.Fatal(folder)
	}
	if file, ok := h.store.Data.FilesFile(); !ok || file.Status != ListLoaded || file.Content.Text != "reports/x/README.md" {
		t.Fatal(file)
	}
	token := folder.list.applied
	h.store.OpenFiles(FileSpot{Root: "a", Path: "reports/x/other.md", File: true})
	h.settle(t)
	if f, _ := h.store.Data.FilesFolder(); f.list.applied != token {
		t.Fatal("sibling refetched folder")
	}
	h.srv.SetFileContent(proto.FileContent{Root: "a", Path: "reports/x/other.md", Kind: proto.FileText, Text: "new"})
	h.store.RefreshFiles()
	h.settle(t)
	if f, _ := h.store.Data.FilesFile(); f.Content.Text != "new" {
		t.Fatal(f)
	}
	if f, _ := h.store.Data.FilesFolder(); f.list.applied == token {
		t.Fatal("refresh did not fetch folder")
	}
	h.store.OpenFiles(FileSpot{})
	h.settle(t)
	if h.store.Data.FilesSpot() != (FileSpot{}) {
		t.Fatal(h.store.Data.FilesSpot())
	}
}

// TestOpenFilePathFindsTheEntrysKind follows only the freshly listed entry.
func TestOpenFilePathFindsTheEntrysKind(t *testing.T) {
	for _, tt := range []struct {
		name, kind string
		withheld   bool
		more       int
		partial    bool
		note       string
	}{
		{"folder", proto.FileEntryFolder, false, 0, false, ""}, {"file", proto.FileEntryFile, false, 0, false, ""},
		{"withheld", proto.FileEntryFile, true, 0, false, "withheld is withheld: its name looks like it holds a secret."},
		{"other", proto.FileEntryOther, false, 0, false, "other cannot be shown."},
		{"missing", "", false, 0, false, "missing is not in this folder."},
		{"past", "", false, 1, false, "past is not in this folder. It may be past the entries listed."},
		{"partial", "", false, 0, true, "partial is not in this folder. It may be past the entries listed."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := filesFixture(t)
			var entries []proto.FileEntry
			if tt.kind != "" {
				entries = []proto.FileEntry{{Name: tt.name, Kind: tt.kind, Withheld: tt.withheld}}
			}
			h.srv.SetFileFolder(proto.FileFolder{Root: "a", Entries: entries, More: tt.more, Partial: tt.partial})
			h.srv.SetFileFolder(proto.FileFolder{Root: "a", Path: tt.name})
			h.srv.SetFileContent(proto.FileContent{Root: "a", Path: tt.name, Kind: proto.FileText, Text: "hello"})
			h.store.OpenFilePath("a", tt.name)
			h.settle(t)
			if note := h.store.Data.FilesNote(); note != tt.note {
				t.Fatal(note)
			}
			want := FileSpot{Root: "a"}
			if tt.note == "" {
				want.Path = tt.name
				want.File = tt.kind == proto.FileEntryFile
			}
			if got := h.store.Data.FilesSpot(); got != want {
				t.Fatalf("%+v want %+v", got, want)
			}
		})
	}
}

// TestTheFileRootsLoadAtRefreshAndResync fetches again after failures and in-flight answers.
func TestTheFileRootsLoadAtRefreshAndResync(t *testing.T) {
	h := filesFixture(t)
	if got := h.store.Data.FileRoots(); got.Status != ListLoaded || len(got.Roots.Roots) != 1 {
		t.Fatal(got)
	}
	h.srv.SetFileRoots(proto.FileRoots{Home: "/new"})
	h.store.Refresh()
	h.settle(t)
	if got := h.store.Data.FileRoots(); got.Roots.Home != "/new" || got.Roots.Roots == nil {
		t.Fatal(got)
	}
	h.srv.FailOnce(http.MethodGet, proto.PathFiles, 502)
	h.store.LoadFileRoots()
	h.settle(t)
	if got := h.store.Data.FileRoots(); got.Status != ListFailed || got.Roots.Home != "/new" {
		t.Fatal(got)
	}
	h.srv.SetFileRoots(proto.FileRoots{Home: "/retry"})
	h.store.retryFailed()
	h.settle(t)
	if got := h.store.Data.FileRoots(); got.Status != ListLoaded || got.Roots.Home != "/retry" {
		t.Fatal(got)
	}
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathFiles)
	defer release()
	h.store.LoadFileRoots()
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathFiles) })
	h.srv.SetFileRoots(proto.FileRoots{Home: "/newer"})
	h.store.retryFailed()
	h.loop.RunUntil(t, func() bool { return h.store.Data.FileRoots().Roots.Home == "/newer" })
	release()
	h.settle(t)
	if got := h.store.Data.FileRoots(); got.Roots.Home != "/newer" {
		t.Fatal(got)
	}
	h.store.Data.Reset()
	if _, ok := h.store.Data.FilesFolder(); ok {
		t.Fatal("folder survived reset")
	}
	if _, ok := h.store.Data.FilesFile(); ok {
		t.Fatal("file survived reset")
	}
	if h.store.Data.FileRoots().Status != ListLoading {
		t.Fatal("homes survived reset")
	}
}

// TestFilesRootsAndResetKeepFetchOrdering checks the homes and all reset fields.
func TestFilesRootsAndResetKeepFetchOrdering(t *testing.T) {
	var d Data
	old, newer := d.BeginFileRoots(), d.BeginFileRoots()
	d.SetFileRoots(newer, proto.FileRoots{Home: "new"})
	d.SetFileRoots(old, proto.FileRoots{Home: "old"})
	d.SetFileRootsError(old, "late")
	if got := d.FileRoots(); got.Status != ListLoaded || got.Roots.Home != "new" || got.Roots.Roots == nil {
		t.Fatal(got)
	}
	d.SetFileRootsError(d.BeginFileRoots(), "failed")
	if got := d.FileRoots(); got.Status != ListFailed || got.Roots.Home != "new" {
		t.Fatal(got)
	}
	d.BeginFolder("a", "")
	d.BeginFile("a", "x")
	d.setFilesNote("note")
	d.Reset()
	if _, ok := d.FilesFolder(); ok {
		t.Fatal("folder survived")
	}
	if _, ok := d.FilesFile(); ok {
		t.Fatal("file survived")
	}
	if d.FilesNote() != "" || d.FilesSpot() != (FileSpot{}) || d.FileRoots().Status != ListLoading {
		t.Fatal("viewer survived reset")
	}
}

// TestAnOldFilePathLookupCannotNavigateAfterANewerAnswer guards delayed folder lookups.
func TestAnOldFilePathLookupCannotNavigateAfterANewerAnswer(t *testing.T) {
	h := filesFixture(t)
	h.srv.SetFileFolder(proto.FileFolder{Root: "a", Entries: []proto.FileEntry{{Name: "first", Kind: proto.FileEntryFile}, {Name: "second", Kind: proto.FileEntryFile}}})
	for _, p := range []string{"first", "second"} {
		h.srv.SetFileContent(proto.FileContent{Root: "a", Path: p, Kind: proto.FileText})
	}
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathFilesList)
	defer release()
	h.store.OpenFilePath("a", "first")
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathFilesList) })
	h.store.OpenFilePath("a", "second")
	h.loop.RunUntil(t, func() bool {
		f, ok := h.store.Data.FilesFile()
		return ok && f.Path == "second" && f.Status == ListLoaded
	})
	release()
	h.settle(t)
	if spot := h.store.Data.FilesSpot(); spot != (FileSpot{Root: "a", Path: "second", File: true}) {
		t.Fatal(spot)
	}
	// A failure leaves the parent listing and its error, not a navigation note.
	h.srv.FailOnce(http.MethodGet, proto.PathFilesList, 502)
	h.store.OpenFilePath("a", "first")
	h.settle(t)
	if f, _ := h.store.Data.FilesFolder(); f.Status != ListFailed || f.Err == "" || len(f.Folder.Entries) != 2 {
		t.Fatal(f)
	}
	if h.store.Data.FilesNote() != "" {
		t.Fatal(h.store.Data.FilesNote())
	}
}
