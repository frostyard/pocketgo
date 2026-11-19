package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const merged = `{"id":4321,"number":1,"title":"Fix the build","html_url":"https://github.com/frostyard/hotedge/pull/1","state":"closed","draft":false,
"merged_at":"2026-09-25T10:00:00Z","labels":[{"name":"risk:high"},{"name":"ready-for-human"}],"head":{"ref":"owners/fix","sha":"8869f09a1b2c3d4e5f60718293a4b5c6d7e8f901","repo":{"full_name":"frostyard/hotedge"}},
"base":{"ref":"main","repo":{"full_name":"frostyard/hotedge"}}}`

func ghAnswering(out string, err error) (GH, *[]string) {
	var args []string
	return func(_ context.Context, a ...string) (string, error) {
		args = a
		return out, err
	}, &args
}

func TestReadParsesAPullRequest(t *testing.T) {
	gh, args := ghAnswering(merged, nil)
	p, err := Read(context.Background(), gh, "frostyard/hotedge", 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(*args, " ") != "api --hostname github.com repos/frostyard/hotedge/pulls/1" {
		t.Fatalf("gh %q", *args)
	}
	if p.ID != 4321 || p.State != Merged || p.Head != "owners/fix" || p.HeadRepository != "frostyard/hotedge" || p.Base != "main" ||
		p.Title != "Fix the build" || p.URL != "https://github.com/frostyard/hotedge/pull/1" || p.Repository != "frostyard/hotedge" {
		t.Fatalf("pull %+v", p)
	}
	open := strings.Replace(strings.Replace(merged, `"state":"closed"`, `"state":"open"`, 1), `"merged_at":"2026-09-25T10:00:00Z"`, `"merged_at":null`, 1)
	if p, _ := parse("frostyard/hotedge", 1, open); p.State != Open {
		t.Fatalf("open: %+v", p)
	}
	closed := strings.Replace(merged, `"merged_at":"2026-09-25T10:00:00Z"`, `"merged_at":null`, 1)
	if p, _ := parse("frostyard/hotedge", 1, closed); p.State != Closed {
		t.Fatalf("closed: %+v", p)
	}
	fork := strings.Replace(merged, `"repo":{"full_name":"frostyard/hotedge"}},
"base"`, `"repo":null},
"base"`, 1)
	if p, err := parse("frostyard/hotedge", 1, fork); err != nil || p.HeadRepository != "" {
		t.Fatalf("a gone head repository: %+v %v", p, err)
	}
}

// What GitHub answers is checked: another number, another repository, a
// link elsewhere, a commit that is none, and gh's failure are refused.
func TestReadRefusesWhatIsNotThePullRequestAsked(t *testing.T) {
	for name, out := range map[string]string{
		"not json":         "Not Found",
		"another number":   strings.Replace(merged, `"number":1`, `"number":2`, 1),
		"another repo":     strings.ReplaceAll(merged, `"base":{"ref":"main","repo":{"full_name":"frostyard/hotedge"}}`, `"base":{"ref":"main","repo":{"full_name":"evil/hotedge"}}`),
		"a link elsewhere": strings.Replace(merged, "https://github.com/frostyard/hotedge/pull/1", "https://example.com/x", 1),
		"no commit":        strings.Replace(merged, "8869f09a1b2c3d4e5f60718293a4b5c6d7e8f901", "HEAD", 1),
		"odd state":        strings.Replace(merged, `"state":"closed"`, `"state":"weird"`, 1),
		"no merged_at":     strings.Replace(merged, `"merged_at":"2026-09-25T10:00:00Z",`, ``, 1),
		"empty merged_at":  strings.Replace(merged, `"2026-09-25T10:00:00Z"`, `""`, 1),
		"odd merged_at":    strings.Replace(merged, `"2026-09-25T10:00:00Z"`, `"yesterday"`, 1),
		"open and merged":  strings.Replace(merged, `"state":"closed"`, `"state":"open"`, 1),
		"no id":            strings.Replace(merged, `"id":4321,`, ``, 1),
		"no draft":         strings.Replace(merged, `"draft":false,`, ``, 1),
		"null draft":       strings.Replace(merged, `"draft":false`, `"draft":null`, 1),
		"text draft":       strings.Replace(merged, `"draft":false`, `"draft":"no"`, 1),
		"no title":         strings.Replace(merged, `"title":"Fix the build",`, ``, 1),
		"null title":       strings.Replace(merged, `"title":"Fix the build"`, `"title":null`, 1),
	} {
		if _, err := parse("frostyard/hotedge", 1, out); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	broken := errors.New("HTTP 404: Not Found")
	gh, _ := ghAnswering("", broken)
	if _, err := Read(context.Background(), gh, "frostyard/hotedge", 1); !errors.Is(err, broken) {
		t.Errorf("gh's failure: %v", err)
	}
	for _, repo := range []string{"hotedge", "a/b/c", "../x", "a/..", "a b/c", "-x/y;rm"} {
		if _, err := Read(context.Background(), gh, repo, 1); err == nil || errors.Is(err, broken) {
			t.Errorf("repository %q reached gh: %v", repo, err)
		}
	}
	if _, err := Read(context.Background(), gh, "frostyard/hotedge", 0); err == nil || errors.Is(err, broken) {
		t.Errorf("number 0 reached gh: %v", err)
	}
	long := strings.Replace(merged, "Fix the build", strings.Repeat("x", MaxTitle+10), 1)
	if p, err := parse("frostyard/hotedge", 1, long); err != nil || len([]rune(p.Title)) != MaxTitle {
		t.Errorf("a long title: %d, %v", len([]rune(p.Title)), err)
	}
}
