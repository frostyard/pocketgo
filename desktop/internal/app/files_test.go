package app

import (
	"bytes"
	"github.com/egoist/mygo/ui"
	"image"
	"image/png"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// viewerReport is the fixture's report path; all content is invented.
const viewerReport = "reports/2026-11-23-viewer/README.md"

// seedFiles gives the fake viewer homes, listings and invented content without reading a home.
func (x *testApp) seedFiles() {
	x.srv.SetFileRoots(proto.FileRoots{Home: "/home/team", Roots: []proto.FileRoot{
		{ID: "odrade", Name: "Odrade", Path: "/home/team/agents/odrade"},
		{ID: "chani", Name: "Chani", Path: "/home/team/agents/chani", Private: true},
	}})
	x.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Entries: []proto.FileEntry{
		{Name: "reports", Kind: proto.FileEntryFolder}, {Name: "README.md", Kind: proto.FileEntryFile, Size: 2400},
		{Name: "api-token.txt", Kind: proto.FileEntryFile, Withheld: true}, {Name: "out", Kind: proto.FileEntryOther, Link: true},
	}})
	x.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Path: "reports", Entries: []proto.FileEntry{{Name: "2026-11-23-viewer", Kind: proto.FileEntryFolder}}})
	x.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Path: "reports/2026-11-23-viewer", Entries: []proto.FileEntry{
		{Name: "README.md", Kind: proto.FileEntryFile, Size: 2400}, {Name: "notes.md", Kind: proto.FileEntryFile, Size: 100},
	}})
	x.srv.SetFileContent(proto.FileContent{Root: "odrade", Path: viewerReport, Real: viewerReport, Size: 2400, Modified: pocketdtest.Now, Kind: proto.FileMarkdown,
		Text: "# File viewer report\n\n- Read-only browsing\n- Bounded file reads\n\nSee `AGENTS.md` and [notes.md](notes.md)."})
	x.srv.SetFileContent(proto.FileContent{Root: "odrade", Path: "reports/2026-11-23-viewer/notes.md", Real: "reports/2026-11-23-viewer/notes.md", Size: 100, Modified: pocketdtest.Now, Kind: proto.FileMarkdown, Text: "# Viewer notes\n\nThese are invented fixture notes."})
}

// untilFiles waits for every viewer fetch to finish and draws its result.
func (x *testApp) untilFiles(t *testing.T) {
	t.Helper()
	x.until(t, func() bool { return x.a.st.InFlight() == 0 })
}

// browseReport walks the visible folder rows to the report.
func (x *testApp) browseReport(t *testing.T) {
	t.Helper()
	for _, label := range []string{"Odrade", "reports", "2026-11-23-viewer", "README.md"} {
		x.click(t, label)
		x.untilFiles(t)
	}
	x.hasAll(t, "File viewer report", "Read-only browsing", "Bounded file reads")
}

// TestTheFilesSnapshot browses homes and a split file view through its command and rows.
func TestTheFilesSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.seedFiles()
	x.connect(t)
	cmd, ok := x.a.table.Command("files")
	if !ok || cmd.Title != "Browse Files…" || len(cmd.Bindings) != 0 {
		t.Fatalf("%+v %v", cmd, ok)
	}
	cmd.Run()
	x.untilFiles(t)
	x.hasAll(t, "Odrade", "Chani", "private")
	x.click(t, "Odrade")
	x.untilFiles(t)
	before := len(x.srv.RequestsTo("GET", proto.PathFilesRead))
	x.click(t, "api-token.txt")
	x.untilFiles(t)
	if got := len(x.srv.RequestsTo("GET", proto.PathFilesRead)); got != before {
		t.Fatal("withheld entry was read")
	}
	for _, label := range []string{"reports", "2026-11-23-viewer", "README.md"} {
		x.click(t, label)
		x.untilFiles(t)
	}
	x.hasAll(t, "File viewer report", "Read-only browsing", "notes.md")
	if x.a.compact {
		t.Fatal("desktop drew compact view")
	}
	if f, _ := x.a.st.Data.FilesFolder(); f.Path != "reports/2026-11-23-viewer" {
		t.Fatal(f)
	}
	x.snapshot(t, "files")
	beforeLists := len(x.srv.RequestsTo("GET", proto.PathFilesList))
	link, ok := x.tt.AccessByRole(ui.RoleLink, "notes.md")
	if !ok {
		t.Fatal("the Markdown notes link is missing")
	}
	x.tt.ClickAt(link.Bounds.X+link.Bounds.W/2, link.Bounds.Y+link.Bounds.H/2)
	x.untilFiles(t)
	if got := len(x.srv.RequestsTo("GET", proto.PathFilesList)); got != beforeLists+1 {
		t.Fatalf("Markdown link sent %d folder reads, want 1", got-beforeLists)
	}
	x.hasAll(t, "Viewer notes")
	if f, _ := x.a.st.Data.FilesFile(); f.Path != "reports/2026-11-23-viewer/notes.md" {
		t.Fatal(f)
	}
}

// TestThePhoneFilesSnapshot keeps tabs visible and walks back one breadcrumb at a time.
func TestThePhoneFilesSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.seedFiles()
	x.phoneApp(t, true, nil)
	x.tap(t, "More")
	x.tap(t, "Files")
	x.untilFiles(t)
	x.browseReport(t)
	if !x.hasTabs() {
		t.Fatal("Files hid the tabs")
	}
	x.snapshot(t, "phone-files")
	for _, label := range []string{"Back to 2026-11-23-viewer", "Back to reports", "Back to Odrade", "Back to Homes"} {
		x.tap(t, label)
		x.untilFiles(t)
	}
	x.hasAll(t, "Odrade", "Chani")
	x.tap(t, "Back to More")
	if x.a.place != placeMore {
		t.Fatal(x.a.place)
	}
}

// TestTheIPadFilesSnapshot uses the desktop split layout in a wide iPad window.
func TestTheIPadFilesSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.seedFiles()
	loads := 0
	x.iPadApp(t, &loads)
	x.tt.SetSize(1280, 820)
	x.connect(t)
	x.a.goFiles()
	x.untilFiles(t)
	x.browseReport(t)
	if x.a.compact {
		t.Fatal("wide iPad drew compact view")
	}
	x.hasAll(t, "notes.md", "File viewer report")
	x.snapshot(t, "ipad-files")
}

// TestAPathInAMessageOpensTheFiles clicks a transcript code span, never a guessed path.
func TestAPathInAMessageOpensTheFiles(t *testing.T) {
	x := newTestApp(t)
	x.seedFiles()
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Home: "/home/team/agents/odrade", Runs: model})
	x.srv.AddSession(proto.Session{ID: 9, Title: "Viewer report", Profile: "odrade", Model: model, State: proto.SessionIdle, UpdatedAt: pocketdtest.Now},
		proto.Entry{ID: 90, SessionID: 9, Kind: proto.EntryAssistant, At: pocketdtest.Now, Text: "The report is in `" + viewerReport + "`. Not a home path: `service/internal/api`."})
	x.connect(t)
	x.a.openSession(9)
	x.until(t, func() bool {
		o := x.a.st.Data.Open()
		_, known := x.a.st.Data.FileTop("odrade")
		return o != nil && o.Loaded && known && x.a.st.InFlight() == 0
	})
	// A plain code span is not an actionable link.
	if err := x.tt.Click("service/internal/api"); err == nil && x.a.place != placeSession {
		t.Fatal("unknown code opened a place")
	}
	x.click(t, viewerReport)
	x.untilFiles(t)
	if x.a.place != placeFiles {
		t.Fatal(x.a.place)
	}
	if f, ok := x.a.st.Data.FilesFile(); !ok || f.Path != viewerReport {
		t.Fatal(f)
	}
	x.hasAll(t, "File viewer report")
}

// TestTheFileViewerDrawsEachKind draws only prepared text and images, with notes for the rest.
func TestTheFileViewerDrawsEachKind(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.seedFiles()
	x.connect(t)
	x.a.goFiles()
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, kind, text, note, want string }{
		{"plain.txt", proto.FileText, "Plain text from the fixture.", "", "Plain text from the fixture."},
		{"pic.png", proto.FileImage, "", "", ""},
		{"binary.bin", proto.FileBinary, "", "not shown: not UTF-8 text", "Not shown: not UTF-8 text"},
		{"large.txt", proto.FileLarge, "", "over 1,048,576 bytes: not shown", "Over 1,048,576 bytes: not shown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := proto.FileContent{Root: "odrade", Path: tt.name, Real: tt.name, Size: 42, Modified: pocketdtest.Now, Kind: tt.kind, Text: tt.text, Note: tt.note}
			if tt.kind == proto.FileImage {
				content.Image = pngBytes.Bytes()
				content.MimeType = "image/png"
			}
			x.srv.SetFileContent(content)
			x.a.st.OpenFiles(state.FileSpot{Root: "odrade", Path: tt.name, File: true})
			x.untilFiles(t)
			if tt.want != "" {
				x.hasAll(t, tt.want)
			}
			if tt.kind == proto.FileImage && (x.a.files.image == nil || x.a.files.imageErr) {
				t.Fatal("image was not decoded")
			}
			if tt.kind == proto.FileBinary || tt.kind == proto.FileLarge {
				if x.tt.HasText("Plain text from the fixture.") {
					t.Fatal("old content survived")
				}
			}
		})
	}
}
