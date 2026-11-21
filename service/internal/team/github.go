package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// A gather's proposal session reads GitHub with team_github (ADR-0036):
// GET calls of GitHub's REST API, which pocketd builds and runs as gh api
// in the sandbox with the service user's sign-in (github.Get). The agent
// names a path and query parameters alone; these are the bounds of a read
// and the text of its answer.

// The bounds of a team_github read (ADR-0036).
const (
	// GitHubReadTime bounds one read: gh's sign-in and its call.
	GitHubReadTime = 30 * time.Second
	// MaxGitHubAnswer bounds the answer an agent reads, in bytes, before
	// its note.
	MaxGitHubAnswer = 48 << 10
	// MaxGitHubNote bounds the note that ends a cut answer: the tool's
	// output limits let MaxGitHubAnswer and it through.
	MaxGitHubNote = 512
)

// GitHubRead is a team_github call: a GitHub REST path, as
// repos/owner/name/issues, and its query parameters.
type GitHubRead struct {
	Path  string            `json:"path"`
	Query map[string]string `json:"query,omitempty"`
}

// GitHubAnswer is team_github's answer from what gh printed (ADR-0036):
// JSON compacted, other text as it came, and an empty answer said to be
// one. An answer over MaxGitHubAnswer keeps the whole items of a JSON
// array that fit, else its first MaxGitHubAnswer bytes cut at a whole
// character, and ends with a note that says so. Bytes that are not UTF-8
// are no answer.
func GitHubAnswer(out string) (string, error) {
	if !utf8.ValidString(out) {
		return "", fmt.Errorf("GitHub's answer is not text (%d bytes): team_github reads JSON and text alone", len(out))
	}
	text := strings.TrimSpace(out)
	if text == "" {
		return "GitHub answered with no body: the call succeeded.", nil
	}
	var compact bytes.Buffer
	if json.Valid([]byte(text)) && json.Compact(&compact, []byte(text)) == nil {
		text = compact.String()
	}
	if len(text) <= MaxGitHubAnswer {
		return text, nil
	}
	if cut, kept, total, ok := wholeItems(text, MaxGitHubAnswer); ok {
		return cut + fmt.Sprintf("\n[team_github: the first %d of the %d items, as the whole answer has %d bytes, more than %d; ask for fewer with per_page, or a later page.]",
			kept, total, len(text), MaxGitHubAnswer), nil
	}
	cut := text[:wholeRunes(text, MaxGitHubAnswer)]
	return cut + fmt.Sprintf("\n[team_github: the first %d of the answer's %d bytes; ask for less: per_page and page, or an endpoint that names one item.]",
		len(cut), len(text)), nil
}

// wholeItems is the JSON array text, compacted, cut to its first items
// whose array fits in max bytes, with how many it kept of how many; ok is
// false when text is no array, or not even its first item fits.
func wholeItems(text string, max int) (string, int, int, bool) {
	var items []json.RawMessage
	if !strings.HasPrefix(text, "[") || json.Unmarshal([]byte(text), &items) != nil {
		return "", 0, 0, false
	}
	var b strings.Builder
	b.WriteByte('[')
	kept := 0
	for _, item := range items {
		// The item, its comma before it, and the closing bracket.
		need := len(item) + 1
		if kept > 0 {
			need++
		}
		if b.Len()+need > max {
			break
		}
		if kept > 0 {
			b.WriteByte(',')
		}
		b.Write(item)
		kept++
	}
	if kept == 0 {
		return "", 0, 0, false
	}
	b.WriteByte(']')
	return b.String(), kept, len(items), true
}

// wholeRunes is how many of the first max bytes of text end at a whole
// UTF-8 character.
func wholeRunes(text string, max int) int {
	if len(text) <= max {
		return len(text)
	}
	n := max
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return n
}
