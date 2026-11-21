package github

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// GitHub's rate limit on the token is a 429, a 403 that names when it
// ends, or a GraphQL answer of 200 whose errors say RATE_LIMITED: each is
// RateLimited, as the client reads it, and nothing else is (Codex review of
// #53, round 5).
func TestRateLimitedKnowsEveryShapeOfGitHubsLimit(t *testing.T) {
	reset := time.Date(2026, 11, 21, 12, 34, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		status  int
		body    string
		headers http.Header
		limited bool
	}{
		{"GraphQL's RATE_LIMITED", 200, `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`, nil, true},
		{"a 429", 429, `{"message":"Too many requests"}`, nil, true},
		{"a 403 with Retry-After", 403, `{"message":"You have exceeded a secondary rate limit"}`, http.Header{"Retry-After": {"60"}}, true},
		{"a 403 with the reset", 403, `{"message":"API rate limit exceeded"}`,
			http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1795350840"}}, true},
		// A secondary limit names no end, with the primary quota left
		// (Codex review of #53, round 6).
		{"a 403 secondary rate limit with no end", 403,
			`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, nil, true},
		{"a 403 of a permission", 403, `{"message":"Resource not accessible by integration"}`, nil, false},
		{"another GraphQL error", 200, `{"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a PullRequest"}]}`, nil, false},
	} {
		f := direct(t)
		f.prFail = func(int) (int, string, http.Header) { return c.status, c.body, c.headers }
		_, err := f.client().Preview(context.Background(), f.ref, CrossReview{})
		if err == nil {
			t.Fatalf("%s: no error", c.name)
		}
		if _, ok := RateLimited(err); ok != c.limited {
			t.Errorf("%s: RateLimited %v, want %v (%v)", c.name, ok, c.limited, err)
		}
	}
	if until, ok := RateLimited(&APIError{Status: 403, RetryAfter: reset}); !ok || !until.Equal(reset) {
		t.Errorf("the end of a 403's limit: %v %v", until, ok)
	}
	if _, ok := RateLimited(errors.New("timeout")); ok {
		t.Error("a timeout is no rate limit")
	}
}

// A rate limit a reconciling read meets goes with the result, whose end is
// still unknown, so the run leaves the rest untried (Codex review of #53,
// round 6).
func TestARateLimitWhileReconcilingGoesWithTheResult(t *testing.T) {
	f := direct(t)
	f.mode[merge] = answerDrop
	c := f.client()
	p, err := c.Preview(context.Background(), f.ref, CrossReview{})
	if err != nil {
		t.Fatal(err)
	}
	f.reset()
	f.prFail = func(read int) (int, string, http.Header) {
		if read >= 3 { // the reads after the merge's lost answer
			return 429, `{"message":"Too many requests"}`, nil
		}
		return 0, "", nil
	}
	res := c.Act(context.Background(), Intent{Preview: p, Mode: "single"}, "op-limit", nil)
	if res.Kind != "unknown" || res.Step != "reconcile merge" {
		t.Fatalf("%+v", res)
	}
	if _, ok := RateLimited(res.Err); !ok || !errors.Is(res.Err, ErrNoAnswer) {
		t.Fatalf("the result's error %v", res.Err)
	}
}
