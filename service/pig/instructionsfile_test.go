package pig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
)

// instructionsLayout is a layout whose sandbox keeps the owner's global
// instructions file read-only, as pocketd serve names it (ADR-0045): the
// file is missing, as on a host's first start.
func instructionsLayout(t *testing.T) (l layout, file string) {
	t.Helper()
	l = newLayout(t)
	return l, filepath.Join(l.home, ".pi", "agent", "AGENTS.md")
}

// instructionsPaths are the layout's sandbox paths, with the owner's global
// instructions file named.
func instructionsPaths(l layout, file *string) sandboxPaths {
	p := l.paths("/usr/bin/bwrap")
	p.Files = []*string{file}
	return p
}

// countStops makes the sandbox's stop count its calls and succeed; the
// test puts the real one back.
func countStops(t *testing.T) *int {
	t.Helper()
	stops := 0
	saved := stopRunning
	t.Cleanup(func() { stopRunning = saved })
	stopRunning = func(string, string) error { stops++; return nil }
	return &stops
}

// The sandbox keeps the owner's global instructions file read-only
// (ADR-0045): it makes it when it is missing, empty and private, binds it
// read-only, pins the folders above it, and names it as the file it keeps;
// one that is there already is never truncated.
func TestNewSandboxKeepsTheInstructionsFileReadOnly(t *testing.T) {
	l, file := instructionsLayout(t)
	p := instructionsPaths(l, &file)
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Fatalf("the instructions file was not made an empty private file: %v, %v", info, err)
	}
	if !slices.Contains(s.Shown, file) {
		t.Errorf("Shown %q does not hold %s", s.Shown, file)
	}
	if want := []string{file}; !slices.Equal(s.files, want) {
		t.Errorf("files %q, want %q", s.files, want)
	}
	pi, agent := filepath.Join(l.home, ".pi"), filepath.Join(l.home, ".pi", "agent")
	for _, dir := range []string{pi, agent} {
		if !slices.Contains(s.Pin, dir) {
			t.Errorf("Pin %q does not hold %s", s.Pin, dir)
		}
	}
	args, err := s.Command("true")
	if err != nil {
		t.Fatal(err)
	}
	bind := slices.Index(args, "--ro-bind")
	found := false
	for i := range args {
		if i+2 < len(args) && args[i] == "--ro-bind" && args[i+1] == file && args[i+2] == file {
			found = true
		}
	}
	if bind < 0 || !found {
		t.Errorf("the command line does not bind the instructions file read-only: %q", args)
	}
	if *p.Files[0] != file {
		t.Errorf("the file pocketd is told to read is %q, want %q", *p.Files[0], file)
	}

	// A file that is there is never truncated.
	if err := os.WriteFile(file, []byte("RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := instructionsPaths(l, &file)
	if _, err := newSandbox(again); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "RULES" {
		t.Fatalf("the instructions file holds %q, %v, want RULES", b, err)
	}
}

// pocketd keeps the owner's global instructions file it is named and no
// other: an empty or absent name keeps none and makes nothing, while a
// file the owner named elsewhere is made and kept where it is (ADR-0045).
func TestTheInstructionsFileFollowsTheFlag(t *testing.T) {
	l, file := instructionsLayout(t)
	// (a) no instructions file: -instructions "".
	empty := ""
	p := l.paths("/usr/bin/bwrap")
	p.Files = []*string{&empty, nil}
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.files) != 0 {
		t.Errorf("files %q, want none", s.files)
	}
	for _, path := range s.Shown {
		if strings.HasSuffix(path, "AGENTS.md") {
			t.Errorf("Shown holds %s, want no instructions file", path)
		}
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was made at %s: %v", file, err)
	}

	// (b) a file elsewhere: -instructions ~/rules/global.md.
	other := filepath.Join(l.home, "rules", "global.md")
	p = l.paths("/usr/bin/bwrap")
	p.Files = []*string{&other}
	if s, err = newSandbox(p); err != nil {
		t.Fatal(err)
	}
	if want := []string{other}; !slices.Equal(s.files, want) {
		t.Errorf("files %q, want %q", s.files, want)
	}
	if !slices.Contains(s.Shown, other) {
		t.Errorf("Shown %q does not hold %s", s.Shown, other)
	}
	if info, err := os.Lstat(other); err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Errorf("the instructions file was not made, empty: %v, %v", info, err)
	}
	if !slices.Contains(s.Pin, filepath.Join(l.home, "rules")) {
		t.Errorf("Pin %q does not hold the file's folder", s.Pin)
	}
}

// The sandbox starts only with a regular file at the instructions file's
// path: a folder, a link the service user could retarget, or a path named
// both as a folder and as a file stops it (ADR-0045).
func TestNewSandboxRefusesAnInstructionsFileThatIsNoFile(t *testing.T) {
	// (a) a folder where the file is.
	l, file := instructionsLayout(t)
	if err := os.MkdirAll(file, 0o700); err != nil {
		t.Fatal(err)
	}
	p := instructionsPaths(l, &file)
	if _, err := newSandbox(p); err == nil {
		t.Error("a folder at the instructions file's path was accepted")
	}

	// (b) a symbolic link the user could retarget.
	l, file = instructionsLayout(t)
	target := filepath.Join(l.home, "rules.md")
	if err := os.WriteFile(target, []byte("RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	p = l.paths("/usr/bin/bwrap")
	p.Files = []*string{&file}
	if _, err := newSandbox(p); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link at the instructions file's path: %v", err)
	}

	// (c) the same path as a folder and as a file.
	l = newLayout(t)
	both := filepath.Join(l.home, "agent")
	l.shown = []string{both}
	p = l.paths("/usr/bin/bwrap")
	p.Files = []*string{&both}
	if _, err := newSandbox(p); err == nil || !strings.Contains(err.Error(), "both as a folder and as a file") {
		t.Errorf("a path named as a folder and as a file: %v", err)
	}
}

// A global instructions file the owner removed by hand is made again,
// empty, before the next command, and no command runs while a folder
// stands where it was: the bind the commands had went with the file
// (ADR-0045).
func TestARemovedInstructionsFileIsMadeAgainEmpty(t *testing.T) {
	l, file := instructionsLayout(t)
	stops := countStops(t)
	p := instructionsPaths(l, &file)
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	*stops = 0
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := s.keepShown(context.Background()); err != nil || *stops != 1 {
		t.Fatalf("a removed instructions file: %v, %d stops", err, *stops)
	}
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("the instructions file was not made again, empty: %v, %v", info, err)
	}

	// A folder where the file was is no file to keep read-only.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.keepShown(context.Background()); err == nil {
		t.Error("a folder where the instructions file was was kept")
	}
	if args, err := s.Command("true"); err == nil {
		t.Errorf("a command ran with a folder where the instructions file was: %q", args)
	}
}

// A command's read-only bind of the instructions file goes when the owner
// writes a new one over it: the sandbox stops the running commands then,
// and holds the new file, though a write in place changes nothing it must
// stop (ADR-0045).
func TestAnInstructionsFileWrittenInPlaceStopsNothing(t *testing.T) {
	l, file := instructionsLayout(t)
	stops := countStops(t)
	p := instructionsPaths(l, &file)
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.keepShown(context.Background()); err != nil || *stops != 0 {
		t.Fatalf("the file written in place: %v, %d stops", err, *stops)
	}
	before, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	*stops = 0
	// In place: the same inode, and the same mount for the commands.
	if err := os.WriteFile(file, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the write in place made another file")
	}
	if err := s.keepShown(context.Background()); err != nil || *stops != 0 {
		t.Fatalf("a write in place: %v, %d stops", err, *stops)
	}

	// A new file renamed over it is another file, which the binds of the
	// commands running did not cover.
	next := filepath.Join(filepath.Dir(file), "AGENTS.md.new")
	if err := os.WriteFile(next, []byte("v3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, file); err != nil {
		t.Fatal(err)
	}
	if err := s.keepShown(context.Background()); err != nil || *stops != 1 {
		t.Fatalf("a file renamed over it: %v, %d stops", err, *stops)
	}
	// The new file is held: the next look stops nothing.
	if err := s.keepShown(context.Background()); err != nil || *stops != 1 {
		t.Fatalf("the new file kept: %v, %d stops", err, *stops)
	}
}

// A symbolic link put in the place of a folder above the instructions file
// leads the read-only bind where the file tools do not guard: no command
// runs until pocketd restarts (ADR-0045).
func TestALinkPutOnTheWayToTheInstructionsFileStopsCommands(t *testing.T) {
	l, file := instructionsLayout(t)
	stops := countStops(t)
	p := instructionsPaths(l, &file)
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	*stops = 0
	pi := filepath.Join(l.home, ".pi")
	agent, elsewhere := filepath.Join(pi, "agent"), filepath.Join(pi, "elsewhere")
	if err := os.Rename(agent, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, agent); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := s.keepShown(context.Background()); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a link on the way to the instructions file: %v", err)
	}
	if *stops < 1 {
		t.Errorf("a link on the way stopped %d commands, want at least one", *stops)
	}
	if args, err := s.Command("true"); err == nil {
		t.Errorf("a command ran with a link on the way to the instructions file: %q", args)
	}
}

// The file tools read the owner's global instructions file and refuse
// every change to it, and to the folders above it, with the error that
// says it holds the owner's rules for every session (ADR-0045). A
// single-file read-only path that is not that file keeps the folders'
// error.
func TestFileToolsReadButNeverChangeTheInstructionsFile(t *testing.T) {
	l, file := instructionsLayout(t)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := instructionsPaths(l, &file)
	s, err := newSandbox(p) // never run: file tools do not start commands
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work := t.TempDir()
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}),
		protected: s.Guarded(), readOnly: s.Shown, files: s.files}
	if got, err := g.ReadTextFile(ctx, file); err != nil || got != "RULES" {
		t.Fatalf("a read of the instructions file: %q, %v", got, err)
	}
	link := filepath.Join(work, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	hard := filepath.Join(work, "hard")
	if err := os.Link(file, hard); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	kept := filepath.Join(work, "taken")
	taken := filepath.Join(work, "given")
	for _, path := range []string{kept, taken} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, change := range map[string]func() error{
		"write":                     func() error { return g.WriteFile(ctx, file, "EVIL") },
		"append":                    func() error { return g.AppendFile(ctx, file, "EVIL") },
		"truncate":                  func() error { return g.TruncateFile(ctx, file, 0) },
		"flush":                     func() error { return g.FlushFile(ctx, file) },
		"rename out":                func() error { return g.RenameFile(ctx, file, kept) },
		"rename in":                 func() error { return g.RenameFile(ctx, taken, file) },
		"remove":                    func() error { return g.Remove(ctx, file, nil) },
		"write through a link":      func() error { return g.WriteFile(ctx, link, "EVIL") },
		"write through a hard link": func() error { return g.WriteFile(ctx, hard, "EVIL") },
	} {
		if err := change(); !errors.Is(err, errReadOnlyFile) {
			t.Errorf("%s: %v, want errReadOnlyFile", name, err)
		}
	}
	// A folder above the file, or the folder that holds it, goes with it.
	for name, change := range map[string]func() error{
		"remove the folder above": func() error {
			return g.Remove(ctx, filepath.Join(l.home, ".pi"), &env.RemoveOptions{Recursive: true})
		},
		"rename the folder that holds it": func() error {
			return g.RenameFile(ctx, filepath.Join(l.home, ".pi", "agent"), filepath.Join(l.home, "moved"))
		},
	} {
		if err := change(); !errors.Is(err, errReadOnlyFile) && !errors.Is(err, errReadOnly) {
			t.Errorf("%s: %v, want errReadOnlyFile or errReadOnly", name, err)
		}
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "RULES" {
		t.Fatalf("the instructions file holds %q, %v, want RULES", b, err)
	}
	// Beside it, the file tools write as before.
	if err := g.WriteFile(ctx, filepath.Join(l.home, ".pi", "agent", "notes.txt"), "kept"); err != nil {
		t.Errorf("a write beside the instructions file: %v", err)
	}

	// Another read-only file that is no instructions file keeps the
	// folder's error, as the skills and the uploads do.
	other := filepath.Join(work, "other.md")
	if err := os.WriteFile(other, []byte("RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	g2 := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), readOnly: []string{other}}
	if err := g2.WriteFile(ctx, other, "EVIL"); !errors.Is(err, errReadOnly) || errors.Is(err, errReadOnlyFile) {
		t.Errorf("a write to a read-only file that is no instructions file: %v", err)
	}
}

// A session's write and edit tools are refused on the owner's global
// instructions file, and its read tool reads it (ADR-0045).
func TestASessionsWriteAndEditToolsAreRefusedOnTheInstructionsFile(t *testing.T) {
	l, file := instructionsLayout(t)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := instructionsPaths(l, &file)
	s, err := newSandbox(p) // never run: file tools do not start commands
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	e, err := open(context.Background(), Options{DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve,
		NeedsApproval: approvals.NeedsApproval, Sandbox: s},
		fauxModels(toolTurn("write", map[string]any{"path": file, "content": "EVIL"}, "w1"), answer("done"),
			toolTurn("edit", map[string]any{"path": file, "edits": []any{
				map[string]any{"oldText": "RULES", "newText": "EVIL"}}}, "e1"), answer("done"),
			toolTurn("read", map[string]any{"path": file}, "r1"), answer("done")))
	if err != nil {
		t.Fatal(err)
	}
	e.Resume()
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	last := func() *proto.ToolResult {
		t.Helper()
		waitIdle(t, e, id)
		entries, _, _ := e.Transcript(ctx, id)
		var result *proto.ToolResult
		for _, entry := range entries {
			if entry.ToolResult != nil {
				result = entry.ToolResult
			}
		}
		return result
	}
	for _, turn := range []struct {
		key, message string
		failed       bool
	}{
		{"u:dev:w", "write the rules", true},
		{"u:dev:e", "edit the rules", true},
		{"u:dev:r", "read the rules", false},
	} {
		if _, err := e.Send(ctx, id, turn.key, turn.message); err != nil {
			t.Fatal(err)
		}
		if result := last(); result == nil || result.IsError != turn.failed {
			t.Errorf("%s: %+v, want a failure %v", turn.message, result, turn.failed)
		}
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "RULES" {
		t.Fatalf("the instructions file holds %q, %v, want RULES", b, err)
	}
}

// The sandbox's gate refuses a command unless a read-only mount covers the
// instructions file: a mount of its own at its path, read-only, is what a
// command finds; a writable one, or none at all, refuses it (ADR-0045).
func TestTheGateChecksTheInstructionsFile(t *testing.T) {
	_, file := instructionsLayout(t)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	savedMounts := gateMounts
	t.Cleanup(func() { gateMounts = savedMounts })
	root := "1 0 0:9 / / rw - btrfs /dev/sda2 rw\n"
	put := func(table string) {
		gateMounts = func() ([]mount, error) { return parseMountinfo(strings.NewReader(table)) }
	}
	put(root + "2 1 0:9 " + file + " " + file + " ro - btrfs /dev/sda2 rw\n")
	if err := gateCheck([]string{file}, nil, nil); err != nil {
		t.Errorf("a read-only mount at the instructions file: %v", err)
	}
	put(root + "2 1 0:9 " + file + " " + file + " rw - btrfs /dev/sda2 rw\n")
	if err := gateCheck([]string{file}, nil, nil); err == nil {
		t.Error("a writable mount at the instructions file was let through")
	}
	put(root)
	if err := gateCheck([]string{file}, nil, nil); err == nil {
		t.Error("an instructions file on the writable root mount alone was let through")
	}
}
