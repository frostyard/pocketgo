package github

import (
	"errors"
	"net/http"
	"time"
)

// RateLimited says err is GitHub's rate limit on the token, and when it
// ends (zero when GitHub names no end): an HTTP 429, a 403 that names when
// the limit ends, or a GraphQL answer whose errors say RATE_LIMITED, which
// GitHub sends with a 200 (pocketgo ADR-0031, Codex review of #53, round
// 5).
func RateLimited(err error) (until time.Time, ok bool) {
	var api *APIError
	if errors.As(err, &api) && (api.Status == http.StatusTooManyRequests || !api.RetryAfter.IsZero()) {
		return api.RetryAfter, true
	}
	var g *gqlErrors
	if errors.As(err, &g) {
		for _, e := range g.list {
			if e.Type == "RATE_LIMITED" {
				return time.Time{}, true
			}
		}
	}
	return time.Time{}, false
}
