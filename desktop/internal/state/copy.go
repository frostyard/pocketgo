package state

import (
	"strings"
	"time"
)

// Copy is the text a transcript row's Copy button puts on the clipboard,
// and whether the row has one: only an agent's reply (RowAssistant) that is
// finished (not Live) and has words (Text not only white space). The text is
// the reply's Markdown source, Text exactly as the entry holds it; its
// thinking is never copied.
func (r Row) Copy() (text string, ok bool) {
	if r.Kind != RowAssistant || r.Live || strings.TrimSpace(r.Text) == "" {
		return "", false
	}
	return r.Text, true
}

// CopyHold is how long a Copy button shows that it copied.
const CopyHold = 1500 * time.Millisecond

// Copied is the reply whose Copy button last copied: its session, its row's
// Key, and when (the window's frame clock).
type Copied struct {
	Session int64
	Key     string
	At      time.Time
}

// Shows reports whether the Copy button of row key in session shows "Copied"
// at now: it is the row copied last, in that session, and now is at At or
// after it and less than CopyHold after it.
func (m Copied) Shows(session int64, key string, now time.Time) bool {
	return m.Session == session && m.Key == key && m.Left(now) > 0
}

// Left is how long the row copied last still shows "Copied" at now: 0 once
// Shows no longer holds for it (or nothing was copied), and CopyHold-(now-At) before.
func (m Copied) Left(now time.Time) time.Duration {
	elapsed := now.Sub(m.At)
	if m.Key == "" || elapsed < 0 || elapsed >= CopyHold {
		return 0
	}
	return CopyHold - elapsed
}
