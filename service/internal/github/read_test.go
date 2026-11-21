package github

import (
	"context"
	"errors"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// An endpoint is a relative REST path and a query pocketd encodes: no
// scheme, host, escape, placeholder, dot segment, flag or graphql gets
// through (ADR-0036).
func TestEndpoint(t *testing.T) {
	for _, tc := range []struct {
		path  string
		query map[string]string
		want  string
	}{
		{"repos/frostyard/pocketgo/issues", nil, "repos/frostyard/pocketgo/issues"},
		{"/repos/frostyard/pocketgo/pulls/58", nil, "repos/frostyard/pocketgo/pulls/58"},
		{"repos/frostyard/pocketgo/issues", map[string]string{"state": "open", "per_page": "50", "labels": "bug,good first issue"},
			"repos/frostyard/pocketgo/issues?labels=bug%2Cgood+first+issue&per_page=50&state=open"},
		// A search's query keeps its colons, slashes and spaces as text:
		// encoded, none is a placeholder or a second parameter.
		{"search/issues", map[string]string{"q": "repo:frostyard/pocketgo is:open label:bug&state=closed {owner}"},
			"search/issues?q=repo%3Afrostyard%2Fpocketgo+is%3Aopen+label%3Abug%26state%3Dclosed+%7Bowner%7D"},
		{"repos/frostyard/pocketgo/compare/main...bk/x", nil, "repos/frostyard/pocketgo/compare/main...bk/x"},
		{"repos/frostyard/pocketgo/contents/docs/README.md", map[string]string{"ref": "main"}, "repos/frostyard/pocketgo/contents/docs/README.md?ref=main"},
		{"rate_limit", map[string]string{}, "rate_limit"},
	} {
		got, err := Endpoint(tc.path, tc.query)
		if err != nil || got != tc.want {
			t.Errorf("Endpoint(%q, %v) = %q, %v; want %q", tc.path, tc.query, got, err, tc.want)
		}
	}
	long := strings.Repeat("a", MaxQueryValue)
	many := map[string]string{}
	for i := range MaxQueryParams + 1 {
		many["p"+strings.Repeat("x", i)] = "1"
	}
	wide := map[string]string{}
	for i := range 9 {
		wide["p"+strings.Repeat("x", i)] = long
	}
	for name, tc := range map[string]struct {
		path  string
		query map[string]string
		want  string
	}{
		"empty":               {"", nil, "give the path"},
		"a slash alone":       {"/", nil, "give the path"},
		"a scheme":            {"https://evil.example/x", nil, "holds a character"},
		"a host":              {"//evil.example/x", nil, "empty segment"},
		"a backslash":         {`repos\..\user`, nil, "holds a character"},
		"a dot dot":           {"repos/../user/emails", nil, `".." segment`},
		"a dot":               {"repos/./x", nil, `"." segment`},
		"an escaped dot dot":  {"repos/%2e%2e/user", nil, "holds a character"},
		"an empty segment":    {"repos//x", nil, "empty segment"},
		"a trailing slash":    {"repos/x/", nil, "empty segment"},
		"a query in the path": {"repos/a/b/issues?state=open", nil, "in query"},
		"a fragment":          {"repos/a/b#x", nil, "in query"},
		"a placeholder":       {"repos/{owner}/{repo}/issues", nil, "holds a character"},
		"a colon placeholder": {"repos/:owner/:repo/issues", nil, "holds a character"},
		"a space":             {"repos/a b", nil, "holds a character"},
		"a flag":              {"-f", nil, "starts with"},
		"a long flag":         {"--input=x", nil, "holds a character"},
		"a dash first":        {"-x/y", nil, "starts with"},
		"a digit first":       {"1/x", nil, "starts with"},
		"graphql":             {"graphql", nil, "graphql is not offered"},
		"GraphQL under":       {"GraphQL/x", nil, "graphql is not offered"},
		"a newline":           {"repos/a\n/b", nil, "holds a character"},
		"a NUL":               {"repos/a\x00", nil, "holds a character"},
		"too long":            {strings.Repeat("a/", MaxEndpointPath/2) + "a", nil, "more than"},
		"too many segments":   {strings.Repeat("a/", MaxPathSegments) + "a", nil, "segments"},
		"a bad name":          {"repos", map[string]string{"-f": "x"}, "not a name"},
		"a name with =":       {"repos", map[string]string{"a=b": "x"}, "not a name"},
		"an empty name":       {"repos", map[string]string{"": "x"}, "not a name"},
		"a value not UTF-8":   {"repos", map[string]string{"q": "\xff"}, "not UTF-8"},
		"a long value":        {"repos", map[string]string{"q": long + "a"}, "more than"},
		"too many parameters": {"repos", many, "parameters"},
		"too long encoded":    {"repos", wide, "encoded"},
	} {
		if got, err := Endpoint(tc.path, tc.query); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Endpoint(%q) = %q, %v; want an error with %q", name, tc.path, got, err, tc.want)
		}
	}
}

// ghPlaceholder is what gh api fills in from the git checkout of its
// folder, running git: an endpoint pocketd builds never holds one.
var ghPlaceholder = regexp.MustCompile(`(\:(owner|repo|branch)\b|\{[a-z]+\})`)

// FuzzEndpoint: whatever an agent sends, an endpoint Endpoint accepts is a
// relative path on api.github.com that gh reads as an endpoint and not a
// flag, holds no placeholder, dot segment or graphql, and keeps its query
// as pocketd encoded it.
func FuzzEndpoint(f *testing.F) {
	for _, seed := range [][2]string{
		{"repos/frostyard/pocketgo/issues", "open"}, {"/search/issues", "repo:a/b is:open"}, {"graphql", ""},
		{"https://evil.example/", ""}, {"repos/../user", ""}, {"-f", "x"}, {"repos/{owner}", "{repo}"},
		{"repos/:owner/x", ":branch"}, {"a/%2e%2e/b", "&x=y"}, {"a//b", "#f"}, {"répos", "é"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, p, value string) {
		endpoint, err := Endpoint(p, map[string]string{"q": value})
		if err != nil {
			return
		}
		pathPart, rawQuery, _ := strings.Cut(endpoint, "?")
		first, _, _ := strings.Cut(pathPart, "/")
		switch {
		case strings.HasPrefix(endpoint, "-") || strings.HasPrefix(endpoint, "/"):
			t.Fatalf("%q starts as a flag or an absolute path", endpoint)
		case strings.EqualFold(first, "graphql"):
			t.Fatalf("%q is graphql", endpoint)
		case ghPlaceholder.MatchString(endpoint):
			t.Fatalf("%q holds a placeholder gh fills in", endpoint)
		case strings.ContainsAny(pathPart, ":%{}#\\ \t\n"):
			t.Fatalf("%q holds a character a path never does", endpoint)
		case path.Clean("/"+pathPart) != "/"+pathPart:
			t.Fatalf("%q is not a clean path", endpoint)
		}
		u, err := url.Parse("https://api.github.com/" + endpoint)
		if err != nil || u.Host != "api.github.com" || u.Path != "/"+pathPart {
			t.Fatalf("%q reads as %v, %v", endpoint, u, err)
		}
		if query, err := url.ParseQuery(rawQuery); err != nil || len(query) != 1 || !slices.Equal(query["q"], []string{value}) {
			t.Fatalf("%q's query reads as %v, %v", endpoint, query, err)
		}
	})
}

// Get runs gh api with pocketd's arguments alone: the host, the method
// GET, and the endpoint after "--"; a path it refuses runs nothing, and
// gh's failure comes back with the endpoint (ADR-0036).
func TestGetRunsGHWithItsOwnArguments(t *testing.T) {
	var calls [][]string
	gh := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		if strings.HasSuffix(args[len(args)-1], "/missing") {
			return "", errors.New("gh: Not Found (HTTP 404)")
		}
		return `[{"number":1}]`, nil
	}
	ctx := context.Background()
	out, err := Get(ctx, gh, "/repos/frostyard/pocketgo/issues", map[string]string{"state": "open"})
	if err != nil || out != `[{"number":1}]` {
		t.Fatalf("Get: %q, %v", out, err)
	}
	want := []string{"api", "--hostname", "github.com", "--method", "GET", "--", "repos/frostyard/pocketgo/issues?state=open"}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("gh ran with %q, want %q", calls, want)
	}
	if _, err := Get(ctx, gh, "repos/frostyard/pocketgo/missing", nil); err == nil || !strings.Contains(err.Error(), "GET repos/frostyard/pocketgo/missing on GitHub") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("a failure: %v", err)
	}
	calls = nil
	for _, p := range []string{"graphql", "-f", "repos/../user", "https://evil.example/x"} {
		if _, err := Get(ctx, gh, p, nil); err == nil {
			t.Errorf("Get(%q) ran", p)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused path ran gh: %q", calls)
	}
}
