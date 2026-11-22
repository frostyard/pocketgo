package proto

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxChangedPath is the most bytes of a path a file's diff is asked for
// by (rule 64).
const MaxChangedPath = 4096

// CheckChangedPath refuses a path no listed change of a session can have
// (rule 64, ADR-0053): empty, too long, not UTF-8, holding a NUL,
// absolute, or with an empty, "." or ".." part. pocketd refuses it before
// it looks at the session, and so does the desktop's fake.
func CheckChangedPath(path string) error {
	switch {
	case path == "" || len(path) > MaxChangedPath || !utf8.ValidString(path) || strings.ContainsRune(path, 0):
		return fmt.Errorf("path must be 1 to %d bytes of UTF-8 without a NUL", MaxChangedPath)
	case strings.HasPrefix(path, "/"):
		return errors.New("path names a file in the checkout, from its top, not an absolute path")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("path %q is not a file's path from the checkout's top", clipPath(path))
		}
	}
	return nil
}

// clipPath is a path short enough for an error's message.
func clipPath(path string) string {
	if r := []rune(path); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return path
}
