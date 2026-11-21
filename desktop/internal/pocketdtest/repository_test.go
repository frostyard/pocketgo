package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The fake finds a repository's checkout as pocketd does (protocol rule
// 6a): with no profile, a name the profiles give two folders needs the
// folder, so a desktop that drops the chosen folder fails its tests (Codex
// review of #46, round 1).
func TestTheFakeNeedsTheFolderOfARepositoryInTwo(t *testing.T) {
	s := New()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	s.AddModel(proto.Model{ModelRef: opus, Name: "Claude Opus 5.5"})
	s.AddProfile(proto.Profile{ID: "duncan", Runs: opus, Repositories: []proto.Repository{{Name: "frostyard/chairlift", Folder: "/srv/chairlift"}}})
	s.AddProfile(proto.Profile{ID: "teg", Runs: opus, Repositories: []proto.Repository{{Name: "frostyard/chairlift", Folder: "/home/teg/chairlift"}}})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var pe *proto.Error
	if _, err := client.CreateSession(ctx, proto.CreateSession{RequestID: "a", Repository: "frostyard/chairlift", Model: opus}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("no folder of two: %v, want 400", err)
	}
	got, err := client.CreateSession(ctx, proto.CreateSession{RequestID: "b", Repository: "frostyard/chairlift", Cwd: "/home/teg/chairlift", Model: opus})
	if err != nil || got.Cwd != "/home/teg/chairlift" {
		t.Fatalf("teg's folder: %+v, %v", got, err)
	}
}
