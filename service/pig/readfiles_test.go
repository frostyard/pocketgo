package pig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/files"
)

// TestTheFilesHelperAnswers checks the hidden command without a sandbox.
func TestTheFilesHelperAnswers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args []string
		exit int
		verb string
		base error
	}{
		{"list", []string{"list", root, ".", "10", "10"}, 0, "list", nil},
		{"read", []string{"read", root, "README.md", "10"}, 0, "read", nil},
		{"missing", []string{"read", root, "missing", "10"}, 0, "read", files.ErrNotFound},
		{"path", []string{"read", root, "../x", "10"}, 0, "read", files.ErrBadPath},
		{"over", []string{"read", root, "README.md", "1"}, 0, "read", nil},
		{"verb", []string{"other", root, ".", "10"}, 2, "", nil},
		{"short", []string{"read", root, "."}, 2, "", nil},
		{"negative", []string{"read", root, ".", "-1"}, 2, "", nil},
		{"relative", []string{"read", "relative", ".", "10"}, 2, "", nil},
		{"empty root", []string{"read", "", ".", "10"}, 2, "", nil},
		{"bad number", []string{"read", root, ".", "x"}, 2, "", nil},
		{"list negative", []string{"list", root, ".", "-1", "10"}, 2, "", nil},
		{"list names", []string{"list", root, ".", "10", "x"}, 2, "", nil},
		{"list short", []string{"list", root, ".", "10"}, 2, "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := RunReadFilesHelper(tt.args, &out); code != tt.exit {
				t.Fatalf("exit %d: %s", code, &out)
			}
			if tt.exit != 0 {
				return
			}
			folder, file, err := parseFilesAnswer(tt.verb, out.String())
			if !errors.Is(err, tt.base) {
				t.Fatalf("%s: %v", &out, err)
			}
			if tt.base != nil {
				return
			}
			if tt.verb == "list" {
				if len(folder.Entries) != 1 || folder.Entries[0].Name != "README.md" {
					t.Fatal(folder)
				}
			} else if tt.name == "over" {
				if !file.Over || file.Data != nil {
					t.Fatal(file)
				}
			} else if string(file.Data) != "hello\n" || file.Path != "README.md" {
				t.Fatal(file)
			}
		})
	}
}

// TestReadFileParsesTheHelpersAnswer rejects incomplete or conflicting answers.
func TestReadFileParsesTheHelpersAnswer(t *testing.T) {
	for _, tt := range []struct {
		name, verb, out string
		bad             bool
		base            error
	}{
		{"list", "list", "{\"folder\":{}}\n", false, nil},
		{"read", "read", "{\"file\":{\"path\":\"a\",\"size\":3}}\nabc", false, nil},
		{"error", "read", "{\"error\":\"outside\",\"message\":\"no\"}\n", false, files.ErrOutside},
		{"unknown", "read", "{\"error\":\"x\"}\n", true, nil},
		{"two", "read", "{\"folder\":{},\"file\":{}}\n", true, nil},
		{"size", "read", "{\"file\":{\"size\":2}}\na", true, nil},
		{"over bytes", "read", "{\"file\":{\"over\":true}}\nx", true, nil},
		{"newline", "read", "{\"file\":{}}", true, nil},
		{"wrong verb", "list", "{\"file\":{}}\n", true, nil},
		{"empty", "read", "{}\n", true, nil},
		{"bad json", "read", "x\n", true, nil},
		{"trailing json", "read", "{\"file\":{}} {}\n", true, nil},
		{"list bytes", "list", "{\"folder\":{}}\nx", true, nil},
		{"error bytes", "read", "{\"error\":\"path\"}\nx", true, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			folder, file, err := parseFilesAnswer(tt.verb, tt.out)
			if tt.bad {
				if err == nil || err.Error() != "read files: no answer" {
					t.Fatalf("%v", err)
				}
				return
			}
			if !errors.Is(err, tt.base) {
				t.Fatal(err)
			}
			if tt.name == "list" && folder.Entries == nil {
				t.Fatal("nil entries")
			}
			if tt.name == "read" && (file.Path != "a" || string(file.Data) != "abc") {
				t.Fatal(file)
			}
			if tt.base != nil && err.Error() != "no" {
				t.Fatal(err)
			}
		})
	}
}

// TestFilesRejectInvalidArgumentsBeforeRunning needs no executable or sandbox.
func TestFilesRejectInvalidArgumentsBeforeRunning(t *testing.T) {
	e := &Engine{}
	for _, tt := range []struct {
		root           string
		entries, names int
	}{{"relative", 1, 1}, {"/", -1, 1}, {"/", 1, -1}} {
		if _, err := e.ListFiles(context.Background(), tt.root, "", tt.entries, tt.names); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		root string
		max  int64
	}{{"relative", 1}, {"/", -1}, {"/", 1<<63 - 1}} {
		if _, err := e.ReadFile(context.Background(), tt.root, "", tt.max); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatal(err)
		}
	}
}
