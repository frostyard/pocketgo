package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// A gather's proposal session reads GitHub with team_github (ADR-0036):
// GET calls of GitHub's REST API alone. The agent names a path and query
// parameters, which Endpoint checks; Get runs gh api with every argument
// pocketd's own: the host, the method GET, and the endpoint after "--".
// No field, body, input file, header, jq filter or template is ever
// passed: a field or an input turns gh's call into a write, the graphql
// endpoint posts, and gh's jq and templates run in its process with its
// environment, which holds the service user's token.

// The bounds of an endpoint team_github reads (ADR-0036).
const (
	// MaxEndpointPath bounds the path, in bytes.
	MaxEndpointPath = 512
	// MaxPathSegments bounds the path's segments.
	MaxPathSegments = 32
	// MaxQueryParams bounds the query's parameters.
	MaxQueryParams = 20
	// MaxQueryValue bounds a parameter's value, in characters.
	MaxQueryValue = 1000
	// MaxEndpoint bounds the whole endpoint, its query encoded, in bytes:
	// GitHub refuses a much longer URL, and the kernel an argument past
	// 128 KiB.
	MaxEndpoint = 8 << 10
)

var (
	// pathSegment is one segment of a REST path: letters, digits and
	// . _ ~ @ + - alone, so no scheme, host, query, fragment, escape (%)
	// or gh placeholder ({owner}, :owner) gets through.
	pathSegment = regexp.MustCompile(`^[A-Za-z0-9._~@+-]+$`)
	// pathRoot is the path's first segment: every REST root starts with a
	// letter, so the endpoint never starts as a flag does.
	pathRoot = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._~@+-]*$`)
	// queryName is a query parameter's name, as per_page or state.
	queryName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
)

// Endpoint is the endpoint gh api reads for path and query, or why it
// refuses them (ADR-0036). The path is a relative REST path, its one
// leading "/" dropped: 1 to MaxPathSegments segments of letters, digits
// and . _ ~ @ + -, none "." or "..", the first starting with a letter
// and not graphql. The query's names and values are pocketd's to encode:
// a value holds no character gh or GitHub reads as anything but its text.
func Endpoint(path string, query map[string]string) (string, error) {
	p := strings.TrimPrefix(path, "/")
	switch {
	case strings.TrimSpace(p) == "":
		return "", errors.New("give the path of a GitHub REST endpoint, as repos/owner/name/issues")
	case len(p) > MaxEndpointPath:
		return "", fmt.Errorf("the path has %d bytes, more than %d", len(p), MaxEndpointPath)
	case strings.ContainsAny(p, "?#"):
		return "", errors.New("give query parameters in query, not in the path")
	}
	segments := strings.Split(p, "/")
	if len(segments) > MaxPathSegments {
		return "", fmt.Errorf("the path has %d segments, more than %d", len(segments), MaxPathSegments)
	}
	for _, segment := range segments {
		switch {
		case segment == "":
			return "", fmt.Errorf("the path %q has an empty segment", p)
		case segment == "." || segment == "..":
			return "", fmt.Errorf("the path %q has a %q segment", p, segment)
		case !pathSegment.MatchString(segment):
			return "", fmt.Errorf("the path's segment %q holds a character other than letters, digits and . _ ~ @ + -", segment)
		}
	}
	switch {
	case !pathRoot.MatchString(segments[0]):
		return "", fmt.Errorf("the path starts with %q: a REST path starts with a letter, as repos or search do", segments[0])
	case strings.EqualFold(segments[0], "graphql"):
		return "", errors.New("team_github reads GitHub's REST API with GET alone: graphql is not offered")
	}
	if len(query) > MaxQueryParams {
		return "", fmt.Errorf("the query has %d parameters, more than %d", len(query), MaxQueryParams)
	}
	values := url.Values{}
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value := query[name]
		switch {
		case !queryName.MatchString(name):
			return "", fmt.Errorf("the query parameter %q is not a name: a letter, then letters, digits and _, at most 64", name)
		case !utf8.ValidString(value):
			return "", fmt.Errorf("the query parameter %s is not UTF-8 text", name)
		case utf8.RuneCountInString(value) > MaxQueryValue:
			return "", fmt.Errorf("the query parameter %s has more than %d characters", name, MaxQueryValue)
		}
		values.Set(name, value)
	}
	endpoint := p
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	if len(endpoint) > MaxEndpoint {
		return "", fmt.Errorf("the endpoint, its query encoded, has %d bytes, more than %d", len(endpoint), MaxEndpoint)
	}
	return endpoint, nil
}

// Get reads path with query on GitHub, as Endpoint checks them, with GET
// alone (ADR-0036), and returns what gh printed. The arguments are
// pocketd's own: no field, body, header, jq filter or template, the host
// github.com, and the endpoint after "--", so nothing the agent wrote is a
// flag. pocketd runs gh in the sandbox with the service user's sign-in
// (Engine.GH).
func Get(ctx context.Context, gh GH, path string, query map[string]string) (string, error) {
	endpoint, err := Endpoint(path, query)
	if err != nil {
		return "", err
	}
	out, err := gh(ctx, getArgs(endpoint)...)
	if err != nil {
		return "", fmt.Errorf("GET %s on GitHub: %w", endpoint, err)
	}
	return out, nil
}

// getArgs are gh's arguments for a GET of endpoint, as Endpoint made it.
func getArgs(endpoint string) []string {
	return []string{"api", "--hostname", "github.com", "--method", "GET", "--", endpoint}
}
