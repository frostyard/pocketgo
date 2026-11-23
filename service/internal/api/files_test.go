//go:build unix

package api

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/files"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// viewerHome makes a fixture root available through a valid profile.
func viewerHome(t *testing.T, h *harness) string {
	t.Helper()
	root := t.TempDir()
	h.writeProfiles(map[string]string{"a.md": "---\nmodel: faux/faux-1\nhome: " + root + "\n---\n# Agent\n"})
	h.server.SetFiles(FilesConfig{Home: t.TempDir(), Refused: []string{t.TempDir()}})
	h.server.withTimeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d != filesTimeout {
			t.Errorf("timeout %s", d)
		}
		return context.WithCancel(ctx)
	}
	return root
}

// viewerWrite writes only inside a test's home fixture.
func viewerWrite(t *testing.T, root, name string, data []byte) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// viewerLink makes a link in a temporary home.
func viewerLink(t *testing.T, root, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
}

// TestFileRootsAreTheProfilesHomes filters service paths, preserving profile identity.
func TestFileRootsAreTheProfilesHomes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	empty, err := h.client.FileRoots(ctx)
	if err != nil || empty.Roots == nil || len(empty.Roots) != 0 {
		t.Fatalf("%+v %v", empty, err)
	}
	a, b, parent, home := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	refused := filepath.Join(parent, "data")
	h.writeProfiles(map[string]string{
		"b.md":       "---\nmodel: faux/faux-1\nhome: " + b + "\nprivate: true\n---\n# Private Agent\n",
		"a.md":       "---\nmodel: faux/faux-1\nhome: " + a + "\n---\n# Agent A\n",
		"c.md":       "---\nmodel: faux/faux-1\nhome: " + parent + "\n---\n",
		"invalid.md": "---\nmodel: invalid\n---\n",
	})
	h.server.SetFiles(FilesConfig{Home: home, Refused: []string{refused}})
	got, err := h.client.FileRoots(ctx)
	want := proto.FileRoots{SessionFolders: true, Home: home, Roots: []proto.FileRoot{{ID: "a", Name: "Agent A", Path: a}, {ID: "b", Name: "Private Agent", Path: b, Private: true}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v; want %+v", got, err, want)
	}
	// A profile without a heading uses its id as its name.
	h.writeProfiles(map[string]string{"plain.md": "---\nmodel: faux/faux-1\nhome: " + a + "\n---\n"})
	got, err = h.client.FileRoots(ctx)
	if err != nil || len(got.Roots) != 1 || got.Roots[0].Name != "plain" {
		t.Fatalf("%+v %v", got, err)
	}
}

// TestTheFileViewerRefusesPathsOutOfTheHome validates names before walking links.
func TestTheFileViewerRefusesPathsOutOfTheHome(t *testing.T) {
	h := newHarness(t)
	root := viewerHome(t, h)
	ctx := context.Background()
	for _, p := range []string{"/etc/passwd", "../x", "a/../../x", ".git/config", "notes/.env", "my-token.txt"} {
		t.Run(p, func(t *testing.T) {
			_, err := h.client.ListFiles(ctx, "a", p)
			asError(t, err, 400, proto.ErrorCodeBadRequest)
			_, err = h.client.ReadFile(ctx, "a", p)
			asError(t, err, 400, proto.ErrorCodeBadRequest)
		})
	}
	for _, read := range []bool{false, true} {
		var err error
		if read {
			_, err = h.client.ReadFile(ctx, "unknown", "")
		} else {
			_, err = h.client.ListFiles(ctx, "unknown", "")
		}
		asError(t, err, 404, proto.ErrorCodeNotFound)
	}
	outside := t.TempDir()
	viewerWrite(t, outside, "public.txt", []byte("outside"))
	viewerWrite(t, root, ".env", []byte("hidden"))
	viewerLink(t, root, "absolute", filepath.Join(outside, "public.txt"))
	relative, err := filepath.Rel(root, filepath.Join(outside, "public.txt"))
	if err != nil {
		t.Fatal(err)
	}
	viewerLink(t, root, "relative", relative)
	viewerLink(t, root, "hidden", ".env")
	for _, p := range []string{"absolute", "relative", "hidden", "missing"} {
		_, err := h.client.ReadFile(ctx, "a", p)
		asError(t, err, 404, proto.ErrorCodeNotFound)
		_, err = h.client.ListFiles(ctx, "a", p)
		asError(t, err, 404, proto.ErrorCodeNotFound)
	}
}

// TestTheFileViewerListsAFolder keeps asked and resolved paths distinct.
func TestTheFileViewerListsAFolder(t *testing.T) {
	h := newHarness(t)
	root := viewerHome(t, h)
	ctx := context.Background()
	for _, name := range []string{"a/b/.env", "a/b/my-token.txt", "a/b/README.md"} {
		viewerWrite(t, root, name, []byte("hello"))
	}
	viewerLink(t, root, "linked", "a/b")
	viewerLink(t, root, "a/b/copy", "README.md")
	for _, tt := range []struct{ asked, clean string }{{"a/./b", "a/b"}, {"linked", "linked"}} {
		folder, err := h.client.ListFiles(ctx, "a", tt.asked)
		if err != nil || folder.Root != "a" || folder.Path != tt.clean || folder.Real != "a/b" || len(folder.Entries) != 3 {
			t.Fatalf("%+v %v", folder, err)
		}
		entries := map[string]proto.FileEntry{}
		for _, e := range folder.Entries {
			entries[e.Name] = e
		}
		if !entries["my-token.txt"].Withheld || !entries["copy"].Link || entries["copy"].Kind != proto.FileEntryFile || entries["README.md"].Size != 5 {
			t.Fatal(entries)
		}
	}
}

// TestTheFileViewerReadsByKind prepares text and decodable images by real extension.
func TestTheFileViewerReadsByKind(t *testing.T) {
	h := newHarness(t)
	root := viewerHome(t, h)
	ctx := context.Background()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"README.md": []byte("# Readme\n"), "notes.txt": []byte("notes"), "nul.txt": {'x', 0}, "utf8.txt": {0xff},
		"large.txt": bytes.Repeat([]byte("a"), proto.MaxFileText+1), "over.txt": bytes.Repeat([]byte("a"), proto.MaxFileImage+1),
		"pic.png": pngData.Bytes(), "pic2.png": []byte("not an image"), "AGENTS.md": []byte("# Agent"),
	} {
		viewerWrite(t, root, name, data)
	}
	viewerLink(t, root, "CLAUDE.md", "AGENTS.md")
	viewerLink(t, root, "alias.txt", "README.md")
	for _, tt := range []struct{ name, kind, text, real, note string }{
		{"README.md", proto.FileMarkdown, "# Readme\n", "README.md", ""},
		{"notes.txt", proto.FileText, "notes", "notes.txt", ""},
		{"nul.txt", proto.FileBinary, "", "nul.txt", "not shown: not UTF-8 text"},
		{"utf8.txt", proto.FileBinary, "", "utf8.txt", "not shown: not UTF-8 text"},
		{"large.txt", proto.FileLarge, "", "large.txt", "over 1,048,576 bytes: not shown"},
		{"over.txt", proto.FileLarge, "", "over.txt", "over 5,242,880 bytes: not shown"},
		{"pic.png", proto.FileImage, "", "pic.png", ""},
		{"pic2.png", proto.FileBinary, "", "pic2.png", "not shown:"},
		{"CLAUDE.md", proto.FileMarkdown, "# Agent", "AGENTS.md", ""},
		{"alias.txt", proto.FileMarkdown, "# Readme\n", "README.md", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, err := h.client.ReadFile(ctx, "a", tt.name)
			if err != nil || f.Root != "a" || f.Path != tt.name || f.Real != tt.real || f.Kind != tt.kind || f.Text != tt.text || !strings.HasPrefix(f.Note, tt.note) {
				t.Fatalf("%+v %v", f, err)
			}
			if tt.kind == proto.FileImage {
				if f.MimeType != "image/png" {
					t.Fatal(f.MimeType)
				}
				if _, err := png.Decode(bytes.NewReader(f.Image)); err != nil {
					t.Fatal(err)
				}
			} else if len(f.Image) != 0 {
				t.Fatal("unexpected image")
			}
		})
	}
}

// TestTheFileViewerNeedsTheDeviceToken checks auth, protocol and read-only routes.
func TestTheFileViewerNeedsTheDeviceToken(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{proto.PathFiles, proto.PathFilesList, proto.PathFilesRead} {
		t.Run(p, func(t *testing.T) {
			if r, e := h.raw(http.MethodGet, p, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version)}); r.StatusCode != 401 || e.Code != proto.ErrorCodeUnauthorized {
				t.Fatalf("%d %+v", r.StatusCode, e)
			}
			if r, e := h.raw(http.MethodGet, p, "", map[string]string{"Authorization": "Bearer " + h.token}); r.StatusCode != 400 || e.Code != proto.ErrorCodeProtocol {
				t.Fatalf("%d %+v", r.StatusCode, e)
			}
			if r, e := h.raw(http.MethodPost, p, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}); r.StatusCode != 405 {
				t.Fatalf("%d %+v", r.StatusCode, e)
			}
		})
	}
}

// filesEngine replaces folder reads while retaining the harness's other methods.
type filesEngine struct {
	*fakeEngine
	list func(context.Context, string, string, int, int) (files.Folder, error)
}

// ListFiles lets a test hold a read at its sandbox boundary.
func (e filesEngine) ListFiles(ctx context.Context, root, rel string, entries, names int) (files.Folder, error) {
	return e.list(ctx, root, rel, entries, names)
}

// TestTheFileViewerRunsBoundedReads holds every slot before admitting one more read.
func TestTheFileViewerRunsBoundedReads(t *testing.T) {
	h := newHarness(t)
	root := viewerHome(t, h)
	entered := make(chan struct{}, proto.MaxFileReads+1)
	release := make(chan struct{})
	done := make(chan error, proto.MaxFileReads+1)
	var mu sync.Mutex
	active, peak := 0, 0
	h.server.SetEngine(filesEngine{fakeEngine: h.engine, list: func(ctx context.Context, got, rel string, entries, names int) (files.Folder, error) {
		if got != root || rel != "" || entries != proto.MaxFolderEntries || names != proto.MaxFolderNames {
			return files.Folder{}, fmt.Errorf("wrong arguments")
		}
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		active--
		mu.Unlock()
		return files.Folder{Entries: []files.Entry{}}, nil
	}})
	if cap(h.server.filesSlots) != proto.MaxFileReads {
		t.Fatal("wrong capacity")
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	for range proto.MaxFileReads + 1 {
		go func() { _, err := h.client.ListFiles(context.Background(), "a", ""); done <- err }()
	}
	for range proto.MaxFileReads {
		<-entered
	}
	if len(h.server.filesSlots) != proto.MaxFileReads {
		t.Fatal("reads ran without taking slots")
	}
	release <- struct{}{}
	<-entered
	unblock()
	for range proto.MaxFileReads + 1 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > proto.MaxFileReads {
		t.Fatalf("%d concurrent reads", peak)
	}
}

func TestSessionFilesStayInsideTheRecordedDirectory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	home := t.TempDir()
	hidden := filepath.Join(home, "private")
	h.server.SetFiles(FilesConfig{Home: home, Refused: []string{hidden}})
	cwd := filepath.Join(home, "repo--agent-review")
	viewerWrite(t, cwd, "src/main.go", []byte("package main"))
	viewerWrite(t, home, "sibling.txt", []byte("sibling"))
	viewerLink(t, cwd, "escape", "../sibling.txt")
	viewerLink(t, home, "alias", cwd)
	for i, dir := range []string{cwd, hidden, home, filepath.Join(home, ".ssh"), filepath.Join(home, "alias"), t.TempDir()} {
		_, err := h.store.InsertSession(ctx, store.SessionRow{ID: int64(i + 1), Title: "test", Cwd: dir, Provider: "faux", Model: "faux-1"})
		if err != nil {
			t.Fatal(err)
		}
	}
	root := proto.SessionFilesRoot(1)
	folder, err := h.client.ListFiles(ctx, root, "src")
	if err != nil || len(folder.Entries) != 1 || folder.Root != root {
		t.Fatalf("%+v %v", folder, err)
	}
	file, err := h.client.ReadFile(ctx, root, "src/main.go")
	if err != nil || file.Text != "package main" {
		t.Fatalf("%+v %v", file, err)
	}
	_, err = h.client.ReadFile(ctx, root, "escape")
	asError(t, err, 404, proto.ErrorCodeNotFound)
	_, err = h.client.ReadFile(ctx, root, "../sibling.txt")
	asError(t, err, 400, proto.ErrorCodeBadRequest)
	for _, id := range []int64{2, 3, 4, 5, 6, 999} {
		_, err = h.client.ListFiles(ctx, proto.SessionFilesRoot(id), "")
		asError(t, err, 404, proto.ErrorCodeNotFound)
	}
}

func TestSessionFileRootForARegisteredWorktree(t *testing.T) {
	h := newRepoHarness(t)
	ctx := context.Background()
	h.server.SetFiles(FilesConfig{Home: t.TempDir()})
	s, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "worktree-files", Profile: "duncan", Repository: "frostyard/chairlift", Worktree: true, Title: "Review files"})
	if err != nil {
		t.Fatal(err)
	}
	viewerWrite(t, s.Cwd, "notes.md", []byte("Worktree notes"))
	f, err := h.client.ReadFile(ctx, proto.SessionFilesRoot(s.ID), "notes.md")
	if err != nil || f.Text != "Worktree notes" {
		t.Fatalf("%+v %v", f, err)
	}
}
