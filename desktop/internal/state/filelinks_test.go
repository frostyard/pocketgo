package state

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// linkRoots names only imaginary homes: these tests never read a file system.
func linkRoots() proto.FileRoots {
	return proto.FileRoots{Home: "/var/home/team", Roots: []proto.FileRoot{
		{ID: "odrade", Name: "Odrade", Path: "/var/home/team/agents/odrade"},
		{ID: "odrade2", Name: "Odrade 2", Path: "/var/home/team/agents/odrade2"},
	}}
}

// TestFileLinkDestRoundTrips escapes every component without admitting unreadable paths.
func TestFileLinkDestRoundTrips(t *testing.T) {
	for _, tt := range []struct{ root, p string }{
		{"odrade", ""}, {"a b", "a/b c/d.md"}, {"r#?%", "p#?%.md"}, {"équipe", "報告/été.md"}, {"a/b", "notes/a.md"},
	} {
		t.Run(tt.root+tt.p, func(t *testing.T) {
			dest := FileLinkDest(tt.root, tt.p)
			r, p, ok := ParseFileLink(dest)
			if !ok || r != tt.root || p != tt.p {
				t.Fatalf("%s: %q %q %v", dest, r, p, ok)
			}
		})
	}
	for _, dest := range []string{"https://example.com/x", "pocketgo-file:/x", "pocketgo-file:odrade/../x", "pocketgo-file:odrade/.git/config", "pocketgo-file:odrade/x.key", "pocketgo-file:odrade/%FF", "pocketgo-file:odrade/%", "pocketgo-file:odrade"} {
		t.Run(dest, func(t *testing.T) {
			if _, _, ok := ParseFileLink(dest); ok {
				t.Fatal(dest)
			}
		})
	}
	if got := FileLinkDest("a b", "x#?/y%.md"); got != "pocketgo-file:a%20b/x%23%3F/y%25.md" {
		t.Fatal(got)
	}
}

// TestFileLinkNamesPathsBelowARoot matches absolute boundaries and known relative top names.
func TestFileLinkNamesPathsBelowARoot(t *testing.T) {
	roots := linkRoots()
	top := []string{"reports", "plans", "README.md"}
	for _, tt := range []struct{ code, open, root, p string }{
		{"reports/x/README.md", "odrade", "odrade", "reports/x/README.md"},
		{"/var/home/team/agents/odrade/plans/p.md", "odrade", "odrade", "plans/p.md"},
		{"/var/home/team/agents/odrade2/a.md", "odrade", "odrade2", "a.md"},
		{"~/agents/odrade/README.md", "odrade", "odrade", "README.md"},
		{"/var/home/team/agents/odrade", "odrade", "odrade", ""},
		{"plans/./p.md", "odrade", "odrade", "plans/p.md"},
		{"service/internal/api", "odrade", "", ""}, {"reports/../../x", "odrade", "", ""},
		{"/etc/passwd", "odrade", "", ""}, {"/var/home/team/agents/odrade/.git/config", "odrade", "", ""},
		{"reports/my-token.md", "odrade", "", ""}, {"go test ./...", "odrade", "", ""},
		{"reports/a\u00a0b.md", "odrade", "", ""}, {"https://example.com/x", "odrade", "", ""},
		{"~", "odrade", "", ""}, {"~bob/x", "odrade", "", ""}, {"", "odrade", "", ""},
		{"reports/a.md", "", "", ""}, {"reports/a.md", "unknown", "", ""},
		{strings.Repeat("a", proto.MaxFilePath+1), "odrade", "", ""},
	} {
		t.Run(tt.code+tt.open, func(t *testing.T) {
			dest, ok := FileLink(tt.code, roots, tt.open, top)
			if tt.root == "" {
				if ok {
					t.Fatal(dest)
				}
				return
			}
			r, p, parsed := ParseFileLink(dest)
			if !ok || !parsed || r != tt.root || p != tt.p {
				t.Fatalf("%s %v", dest, ok)
			}
		})
	}
	// A nested root wins over its parent, regardless of list order.
	roots.Roots = append(roots.Roots, proto.FileRoot{ID: "nested", Path: "/var/home/team/agents/odrade/plans"})
	dest, ok := FileLink("/var/home/team/agents/odrade/plans/a.md", roots, "", nil)
	if !ok || dest != FileLinkDest("nested", "a.md") {
		t.Fatalf("%s %v", dest, ok)
	}
	roots.Home = ""
	if _, ok := FileLink("~/agents/odrade/a.md", roots, "", nil); ok {
		t.Fatal("expanded without home")
	}
}

// TestOpenFileRootIsTheOpenSessionsAgent uses profile identity, never the session's folder.
func TestOpenFileRootIsTheOpenSessionsAgent(t *testing.T) {
	var d Data
	d.SetFileRoots(d.BeginFileRoots(), linkRoots())
	d.SetSessions(d.Begin(), []proto.Session{{ID: 1, Profile: "odrade"}, {ID: 2, Profile: "unknown"}, {ID: 3}})
	for _, tt := range []struct {
		id   int64
		want string
	}{{1, "odrade"}, {2, ""}, {0, ""}, {3, ""}, {9, ""}} {
		d.SetOpen(tt.id)
		if got := d.OpenFileRoot(); got != tt.want {
			t.Fatalf("%d: %s", tt.id, got)
		}
	}
}

// TestTheTopOfARootComesFromAnyListingOfIt shares ordering between the viewer and name-only reads.
func TestTheTopOfARootComesFromAnyListingOfIt(t *testing.T) {
	var d Data
	if _, ok := d.FileTop("odrade"); ok {
		t.Fatal("names known before a listing")
	}
	first := d.BeginFolder("odrade", "")
	d.SetFolder(first, "odrade", "", proto.FileFolder{Entries: []proto.FileEntry{{Name: "reports", Kind: proto.FileEntryFolder}, {Name: "api-token.txt", Withheld: true}, {Name: "README.md", Kind: proto.FileEntryFile}}})
	if names, ok := d.FileTop("odrade"); !ok || !slices.Equal(names, []string{"reports", "api-token.txt", "README.md"}) {
		t.Fatal(names, ok)
	}
	older := d.Begin()
	newer := d.BeginFolder("odrade", "")
	d.SetFolder(newer, "odrade", "", proto.FileFolder{Entries: []proto.FileEntry{{Name: "new"}}})
	d.setFileTop(older, "odrade", proto.FileFolder{Entries: []proto.FileEntry{{Name: "old"}}})
	if names, _ := d.FileTop("odrade"); !slices.Equal(names, []string{"new"}) {
		t.Fatal(names)
	}
	// Name-only reads also prevent a delayed viewer listing from replacing them.
	viewer := d.BeginFolder("odrade", "")
	d.setFileTop(d.Begin(), "odrade", proto.FileFolder{Entries: []proto.FileEntry{{Name: "latest"}}})
	d.SetFolder(viewer, "odrade", "", proto.FileFolder{Entries: []proto.FileEntry{{Name: "stale"}}})
	if names, _ := d.FileTop("odrade"); !slices.Equal(names, []string{"latest"}) {
		t.Fatal(names)
	}
	h := newHarness(t)
	h.srv.SetFileRoots(linkRoots())
	h.connect(t)
	h.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Entries: []proto.FileEntry{{Name: "reports"}}})
	h.store.LoadFileTop("odrade")
	h.settle(t)
	if names, ok := h.store.Data.FileTop("odrade"); !ok || !slices.Equal(names, []string{"reports"}) {
		t.Fatal(names, ok)
	}
	if _, ok := h.store.Data.FilesFolder(); ok {
		t.Fatal("name-only fetch changed viewer")
	}
	h.srv.FailOnce(http.MethodGet, proto.PathFilesList, 502)
	h.store.LoadFileTop("odrade")
	h.settle(t)
	if names, _ := h.store.Data.FileTop("odrade"); !slices.Equal(names, []string{"reports"}) {
		t.Fatal(names)
	}
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathFilesList)
	defer release()
	h.store.LoadFileTop("odrade")
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathFilesList) })
	h.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Entries: []proto.FileEntry{{Name: "plans"}}})
	h.store.LoadFileTop("odrade")
	h.loop.RunUntil(t, func() bool { names, _ := h.store.Data.FileTop("odrade"); return slices.Equal(names, []string{"plans"}) })
	release()
	h.settle(t)
	if names, _ := h.store.Data.FileTop("odrade"); !slices.Equal(names, []string{"plans"}) {
		t.Fatal(names)
	}
	h.store.Data.Reset()
	if _, ok := h.store.Data.FileTop("odrade"); ok {
		t.Fatal("names survived reset")
	}
}

// TestOpeningASessionLoadsItsRootsTop fetches once, then links relative transcript paths.
func TestOpeningASessionLoadsItsRootsTop(t *testing.T) {
	h := newHarness(t)
	h.srv.SetFileRoots(linkRoots())
	row := session(1, "Supervisor", 0)
	row.Profile = "odrade"
	h.srv.AddSession(row)
	h.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Entries: []proto.FileEntry{{Name: "reports"}}})
	h.connect(t)
	if _, ok := h.store.Data.FileLinkOf("reports/x.md"); ok {
		t.Fatal("linked without an open session")
	}
	h.store.Open(1)
	h.settle(t)
	if got, ok := h.store.Data.FileLinkOf("reports/x.md"); !ok || got != FileLinkDest("odrade", "reports/x.md") {
		t.Fatal(got, ok)
	}
	count := func() int {
		n := 0
		for _, r := range h.srv.RequestsTo(http.MethodGet, proto.PathFilesList) {
			q, _ := url.ParseQuery(r.Query)
			if q.Get("root") == "odrade" && q.Get("path") == "" {
				n++
			}
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatal(n)
	}
	h.store.Open(1)
	h.settle(t)
	if n := count(); n != 1 {
		t.Fatal(n)
	}
	h.srv.SetFileFolder(proto.FileFolder{Root: "odrade", Entries: []proto.FileEntry{{Name: "plans"}}})
	h.store.Refresh()
	h.settle(t)
	if n := count(); n != 2 {
		t.Fatal(n)
	}
	if names, _ := h.store.Data.FileTop("odrade"); !slices.Equal(names, []string{"plans"}) {
		t.Fatal(names)
	}
	// Opening before the homes answer arrives is repaired by that answer.
	h.store.Data.SetFileRoots(h.store.Data.BeginFileRoots(), proto.FileRoots{})
	h.store.Data.fileTops = nil
	h.store.Open(1)
	h.settle(t)
	h.store.LoadFileRoots()
	h.settle(t)
	if names, ok := h.store.Data.FileTop("odrade"); !ok || !slices.Equal(names, []string{"plans"}) {
		t.Fatal(names, ok)
	}
}

// TestResolveFileHrefFollowsTheFilesFolder handles Markdown fragments and relative traversal.
func TestResolveFileHrefFollowsTheFilesFolder(t *testing.T) {
	spot := FileSpot{Root: "odrade", Path: "reports/x/README.md", File: true}
	for _, tt := range []struct{ href, root, p string }{
		{"notes.md", "odrade", "reports/x/notes.md"}, {"../y/a.md", "odrade", "reports/y/a.md"},
		{"../../../x", "", ""}, {"#heading", "", ""}, {"https://x", "", ""}, {"mailto:a@b", "", ""},
		{"a.md#sec", "odrade", "reports/x/a.md"}, {"a.md?q=x#sec", "odrade", "reports/x/a.md"},
		{"b%20c.md", "odrade", "reports/x/b c.md"},
		{"~notes.md", "odrade", "reports/x/~notes.md"}, {"a%23b.md", "odrade", "reports/x/a#b.md"},
		{"/var/home/team/agents/odrade2/a.md", "odrade2", "a.md"},
		{"~/agents/odrade2/a.md", "odrade2", "a.md"},
		{FileLinkDest("odrade2", "plans/a.md"), "odrade2", "plans/a.md"},
		{"", "", ""}, {"bad%2", "", ""}, {"../.env", "", ""}, {"../../../x", "", ""},
	} {
		t.Run(tt.href, func(t *testing.T) {
			root, p, ok := ResolveFileHref(linkRoots(), spot, tt.href)
			if ok != (tt.root != "") || root != tt.root || p != tt.p {
				t.Fatalf("%q %q %v", root, p, ok)
			}
		})
	}
	if _, _, ok := ResolveFileHref(linkRoots(), FileSpot{Root: "odrade"}, "a.md"); ok {
		t.Fatal("folder is not a Markdown file")
	}
}
