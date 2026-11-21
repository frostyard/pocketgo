package team

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// team_github's answer is bounded in code (ADR-0036): JSON compacted, an
// array cut to whole items, other text cut at a whole character, each cut
// said in a note the tool's limits let through, and bytes that are not
// text refused.
func TestGitHubAnswer(t *testing.T) {
	for name, tc := range map[string]struct{ out, want string }{
		"compacted":  {"{\n  \"number\": 3,\n  \"title\": \"Fix  it\"\n}\n", `{"number":3,"title":"Fix  it"}`},
		"text":       {"Keep it logically awesome.\n", "Keep it logically awesome.\n"},
		"empty":      {"", "GitHub answered with no body: the call succeeded."},
		"an array":   {"[\n {\"a\": 1},\n {\"b\": 2}\n]", `[{"a":1},{"b":2}]`},
		"not json":   {"{ not json", "{ not json"},
		"html stays": {`{"body":"<b>x</b> & y"}`, `{"body":"<b>x</b> & y"}`},
		// Text that is not JSON comes as it came, its whitespace included:
		// an indented first line, and a body of whitespace alone, which is
		// a body (Codex review of #61, round 1).
		"indented":        {"    MMM.           .MMM\n    MMMMMMMMMMMMMMMMMMM\n", "    MMM.           .MMM\n    MMMMMMMMMMMMMMMMMMM\n"},
		"whitespace only": {" \n", " \n"},
	} {
		if got, err := GitHubAnswer(tc.out); err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if _, err := GitHubAnswer("PK\x03\x04\xff\xfe"); err == nil || !strings.Contains(err.Error(), "not text") {
		t.Errorf("an archive: %v", err)
	}

	// An array over the bound keeps the whole items that fit, and says how
	// many of how many.
	var items []string
	for i := range 400 {
		items = append(items, fmt.Sprintf(`{"number": %d, "title": %q}`, i+1, strings.Repeat("t", 200)))
	}
	got, err := GitHubAnswer("[" + strings.Join(items, ",\n") + "]")
	if err != nil {
		t.Fatal(err)
	}
	body, note, ok := strings.Cut(got, "\n")
	var kept []json.RawMessage
	if !ok || len(body) > MaxGitHubAnswer || json.Unmarshal([]byte(body), &kept) != nil || len(kept) == 0 ||
		!strings.Contains(note, fmt.Sprintf("the first %d of the 400 items", len(kept))) || !strings.Contains(note, "per_page") || len(note) > MaxGitHubNote {
		t.Fatalf("a long array: %d bytes kept, %d items, note %q", len(body), len(kept), note)
	}
	// One more item would not have fit.
	next := fmt.Sprintf(`{"number":%d,"title":%q}`, len(kept)+1, strings.Repeat("t", 200))
	if len(body)+1+len(next) <= MaxGitHubAnswer {
		t.Fatalf("the cut kept %d items in %d bytes: room for one more", len(kept), len(body))
	}

	// Other text over the bound is cut at a whole character.
	wide := strings.Repeat("\U0001F600", MaxGitHubAnswer/4+10)
	got, err = GitHubAnswer(`{"body":"` + wide + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	body, note, _ = strings.Cut(got, "\n[team_github:")
	if len(body) > MaxGitHubAnswer || len(body) < MaxGitHubAnswer-4 || !utf8.ValidString(body) || !strings.Contains(note, fmt.Sprintf("the first %d of the answer's", len(body))) || len(note) > MaxGitHubNote {
		t.Fatalf("a long object: %d bytes kept, note %q", len(body), note)
	}
	// An array whose first item does not fit is cut so too.
	got, err = GitHubAnswer(`[{"body":"` + wide + `"}]`)
	if err != nil || !strings.Contains(got, "of the answer's") || !utf8.ValidString(got) {
		t.Fatalf("a long first item: %d bytes, %v", len(got), err)
	}
	// An answer at the bound comes whole.
	exact := strings.Repeat("x", MaxGitHubAnswer)
	if got, err := GitHubAnswer(exact); err != nil || got != exact {
		t.Fatalf("an answer at the bound: %d bytes, %v", len(got), err)
	}
	// Text over the bound only with its whitespace is over it: it is cut,
	// and says so (Codex review of #61, round 1).
	padded := "  " + exact + "\n"
	if got, err := GitHubAnswer(padded); err != nil || !strings.HasPrefix(got, "  x") || !strings.Contains(got, "\n[team_github: the first 49152 of the answer's 49155 bytes") {
		t.Fatalf("text over the bound with its whitespace: %q…, %v", got[:min(len(got), 16)], err)
	}
}

func TestWholeRunes(t *testing.T) {
	for _, tc := range []struct {
		text string
		max  int
		want int
	}{
		{"abc", 5, 3},
		{"abc", 2, 2},
		{"aé", 2, 1},
		{"aé", 3, 3},
		{"\U0001F600\U0001F600", 7, 4},
		{"\U0001F600", 0, 0},
	} {
		if got := wholeRunes(tc.text, tc.max); got != tc.want {
			t.Errorf("wholeRunes(%q, %d) = %d, want %d", tc.text, tc.max, got, tc.want)
		}
	}
}
