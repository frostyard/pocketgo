package pig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/files"
)

// TestAFilesReadRunsInTheSandbox keeps home reads outside pocketd's data.
func TestAFilesReadRunsInTheSandbox(t *testing.T) {
	l := newLayout(t)
	s := usableSandbox(t, l)
	e := &Engine{opts: Options{Sandbox: s}, commands: allowedEnv()}
	root := filepath.Join(l.home, "agents", "a")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(l.data, "auth.json"), filepath.Join(root, "keys")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	file, err := e.ReadFile(ctx, root, "README.md", 100)
	if err != nil || string(file.Data) != "hello" {
		t.Fatalf("%+v %v", file, err)
	}
	if _, err := e.ReadFile(ctx, root, "keys", 100); !errors.Is(err, files.ErrOutside) {
		t.Fatal(err)
	}
	folder, err := e.ListFiles(ctx, l.data, ".", 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range folder.Entries {
		if entry.Name == "auth.json" {
			t.Fatal("sandbox exposed auth.json")
		}
	}
	if len(folder.Entries) != 0 {
		t.Fatalf("hidden data folder: %+v", folder)
	}
}
