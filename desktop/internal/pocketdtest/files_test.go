package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// TestTheFakeAnswersFilesAsPocketdDoes checks validation before root lookup and missing entries.
func TestTheFakeAnswersFilesAsPocketdDoes(t *testing.T) {
	s := New()
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if roots, err := client.FileRoots(ctx); err != nil || roots.Roots == nil {
		t.Fatalf("%+v %v", roots, err)
	}
	roots := proto.FileRoots{Roots: []proto.FileRoot{{ID: "a", Name: "Agent"}}}
	s.SetFileRoots(roots)
	roots.Roots[0].ID = "mutated"
	folder := proto.FileFolder{Root: "a", Path: "notes", Entries: []proto.FileEntry{{Name: "file.txt", Kind: proto.FileEntryFile}}}
	s.SetFileFolder(folder)
	folder.Entries[0].Name = "mutated"
	s.SetFileFolder(proto.FileFolder{Root: "a"})
	content := proto.FileContent{Root: "a", Path: "pic.png", Image: []byte{1, 2}, Kind: proto.FileImage}
	s.SetFileContent(content)
	content.Image[0] = 9
	for _, tt := range []struct {
		root, path string
		status     int
		code       proto.ErrorCode
		message    string
	}{
		{"unknown", "../x", 400, proto.ErrorCodeBadRequest, "path leaves the agent's home"},
		{"unknown", "/etc/passwd", 400, proto.ErrorCodeBadRequest, ""},
		{"a", ".git/config", 400, proto.ErrorCodeBadRequest, ""},
		{"a", "my-token.txt", 400, proto.ErrorCodeBadRequest, ""},
		{"unknown", "", 404, proto.ErrorCodeNotFound, "no agent home unknown"},
		{"a", "missing", 404, proto.ErrorCodeNotFound, "a/missing: nothing is there"},
	} {
		for _, verb := range []string{"list", "read"} {
			t.Run(verb+tt.root+tt.path, func(t *testing.T) {
				var err error
				if verb == "list" {
					_, err = client.ListFiles(ctx, tt.root, tt.path)
				} else {
					_, err = client.ReadFile(ctx, tt.root, tt.path)
				}
				var pe *proto.Error
				if !errors.As(err, &pe) || pe.Status != tt.status || pe.Code != tt.code || (tt.message != "" && pe.Message != tt.message) {
					t.Fatalf("%v", err)
				}
			})
		}
	}
	if f, err := client.ListFiles(ctx, "a", "notes/./"); err != nil || len(f.Entries) != 1 || f.Entries[0].Name != "file.txt" || f.Path != "notes" {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := client.ListFiles(ctx, "a", ""); err != nil || f.Entries == nil {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := client.ReadFile(ctx, "a", "pic.png"); err != nil || len(f.Image) != 2 || f.Image[0] != 1 {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := client.ReadFile(ctx, "a", ""); err == nil {
		t.Fatal("missing home file was found")
	}
}
