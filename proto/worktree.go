package proto

import (
	"crypto/sha256"
	"encoding/base32"
	"path"
	"strings"
)

// A session the owner starts on a repository may work in a new git
// worktree of its checkout (rule 6a, ADR-0027). Its name is a rule both
// sides compute from the request: the client shows the folder before it
// sends, and pocketd makes that one from the request alone, never from a
// path the client sent. Paths are the service host's, so they are cleaned
// as slash-separated paths whatever the client's system.

// OwnerAgent stands for the owner in the worktree of a session with no
// profile, the owner's own: <checkout>--owner-<slug>, on owner/<slug>.
const OwnerAgent = "owner"

// MaxSlugWords is the most characters a slug's words take.
const MaxSlugWords = 40

// SuffixSize is how many characters of the request id's digest end a
// session's slug.
const SuffixSize = 6

// SlugWords is title as a folder and branch name: its lowercase ASCII
// letters and digits, as words joined by dashes, whole words while the
// next one and a dash fit in MaxSlugWords characters; "" when none does.
// A thread's slug starts with it too (team.Slug).
func SlugWords(title string) string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	slug := ""
	for _, w := range words {
		if len(slug)+len(w)+1 > MaxSlugWords {
			break
		}
		if slug != "" {
			slug += "-"
		}
		slug += w
	}
	return slug
}

// suffixEncoding is RFC 4648 base32 in lowercase, without padding.
var suffixEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// WorktreeSuffix ends a session's slug: the first SuffixSize characters of
// the lowercase base32 (RFC 4648, no padding) of the SHA-256 of requestID.
// The same request id gives the same suffix, so a retry of the request
// names the same worktree.
func WorktreeSuffix(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return suffixEncoding.EncodeToString(sum[:])[:SuffixSize]
}

// SessionSlug is the slug of a session's worktree: the words of its title,
// trimmed (SlugWords), else "session", then a dash and the request's
// WorktreeSuffix.
func SessionSlug(title, requestID string) string {
	words := SlugWords(strings.TrimSpace(title))
	if words == "" {
		words = "session"
	}
	return words + "-" + WorktreeSuffix(requestID)
}

// SessionWorktree is the new worktree a session works in, and its branch:
// next to checkout, <checkout>--<agent>-<slug>, on the branch
// <agent>/<slug>, where agent is the session's profile id, or OwnerAgent
// with none, and slug is SessionSlug(title, requestID). checkout is
// cleaned first, as pocketd cleans every folder.
func SessionWorktree(checkout, agent, title, requestID string) (worktree, branch string) {
	slug := SessionSlug(title, requestID)
	return path.Clean(checkout) + "--" + agent + "-" + slug, agent + "/" + slug
}
