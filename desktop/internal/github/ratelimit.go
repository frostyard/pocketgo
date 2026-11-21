package github

import (
	"errors"
	"net/http"
	"strings"
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
	// A secondary limit may name no end, with the primary quota left: its
	// 403 says so in words (Codex review of #53, round 6).
	if errors.As(err, &api) && api.Status == http.StatusForbidden {
		msg := strings.ToLower(api.Message)
		if strings.Contains(msg, "secondary rate limit") || strings.Contains(msg, "abuse detection") {
			return time.Time{}, true
		}
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
