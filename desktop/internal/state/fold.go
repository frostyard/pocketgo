package state

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// FromService says whether a user row is a message the service placed
// itself (ADR-0065): a report, a note, a scheduled message, a brief, an
// answer, a round. pocketd's mark alone says so, never the text.
func (r Row) FromService() bool {
	return r.Kind == RowUser && r.Sender != nil && r.Sender.Kind == proto.SenderService
}

// FoldedRunes is the most characters a folded message's Line shows, its
// "…" included.
const FoldedRunes = 120

// Folded is a message the service placed, folded to one line (ADR-0065).
type Folded struct {
	// Head is the bracketed header the message starts with, as
	// "[T300 · Murbella · done]" or "[scheduled]"; empty when it has none.
	Head string
	// Line is the start of the message's first line of words after Head,
	// as plain text, cut to FoldedRunes with "…".
	Line string
}

// FoldOf folds text to one line.
func FoldOf(text string) Folded {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	var folded Folded
	first, _, _ := strings.Cut(text, "\n")
	if strings.HasPrefix(first, "[") {
		if end := strings.IndexByte(first, ']'); end >= 0 &&
			!strings.Contains(first[1:end], "[") &&
			!strings.HasPrefix(first[end+1:], "(") &&
			utf8.RuneCountInString(first[:end+1]) <= FoldedRunes {
			folded.Head, text = first[:end+1], text[end+1:]
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		if plain := foldedPlainText(line); plain != "" {
			folded.Line = Truncate(plain, FoldedRunes, false)
			break
		}
	}
	return folded
}

var foldedLink = regexp.MustCompile(`!?\[([^\[\]]*)\]\([^)]*\)`)
var foldedMarks = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "")

func foldedPlainText(line string) string {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	switch {
	case strings.HasPrefix(line, "#"):
		end := len(line) - len(strings.TrimLeft(line, "#"))
		if strings.HasPrefix(line[end:], " ") {
			line = line[end+1:]
		}
	case strings.HasPrefix(line, ">"):
		line = strings.TrimPrefix(line[1:], " ")
	case strings.HasPrefix(line, "- "), strings.HasPrefix(line, "* "), strings.HasPrefix(line, "+ "):
		line = line[2:]
	}
	line = foldedLink.ReplaceAllString(line, "$1")
	line = foldedMarks.Replace(line)
	return strings.Join(strings.Fields(line), " ")
}

// Unfolded holds the messages the owner opened, by row key: the window's
// memory alone, for as long as the client runs (ADR-0065).
type Unfolded map[string]bool

// Folds says whether row r shows folded to one line: a message the
// service placed that the owner has not opened.
func (u Unfolded) Folds(r Row) bool { return r.FromService() && !u[r.Key] }

// Toggle opens a folded row and folds an open one; it does nothing to a
// row that does not fold (not FromService).
func (u Unfolded) Toggle(r Row) {
	if !r.FromService() || u == nil {
		return
	}
	if u[r.Key] {
		delete(u, r.Key)
	} else {
		u[r.Key] = true
	}
}
