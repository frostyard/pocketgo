package github

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The merge base is read from GitHub's compare, checked, and never fetched
// with git (ADR-0019).
func TestMergeBase(t *testing.T) {
	head, base := strings.Repeat("b", 40), strings.Repeat("e", 40)
	var got []string
	gh := func(_ context.Context, args ...string) (string, error) {
		got = args
		return `{"merge_base_commit":{"sha":"` + base + `"}}`, nil
	}
	sha, err := MergeBase(context.Background(), gh, "frostyard/example", "release/1.x", head)
	if err != nil || sha != base {
		t.Fatalf("MergeBase %q, %v", sha, err)
	}
	if !slices.Contains(got, "repos/frostyard/example/compare/release%2F1.x..."+head) {
		t.Fatalf("gh %q", got)
	}
	for name, answer := range map[string]string{
		"no merge base":  `{"merge_base_commit":null}`,
		"a short sha":    `{"merge_base_commit":{"sha":"abc"}}`,
		"not json":       `<html>`,
		"an empty field": `{"merge_base_commit":{"sha":""}}`,
	} {
		gh := func(context.Context, ...string) (string, error) { return answer, nil }
		if _, err := MergeBase(context.Background(), gh, "frostyard/example", "main", head); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	failing := func(context.Context, ...string) (string, error) { return "", errors.New("HTTP 404") }
	if _, err := MergeBase(context.Background(), failing, "frostyard/example", "main", head); err == nil {
		t.Error("a failed compare gave a base")
	}
	for _, bad := range []string{"", "-x", "a b", "a..b~1"} {
		if _, err := MergeBase(context.Background(), gh, "frostyard/example", bad, head); err == nil {
			t.Errorf("branch %q was compared", bad)
		}
	}
	if _, err := MergeBase(context.Background(), gh, "frostyard/example", "main", "HEAD"); err == nil {
		t.Error("a head that is no commit was compared")
	}
}
