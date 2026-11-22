package state

import (
	"strconv"

	"github.com/frostyard/pocketgo/proto"
)

// Reopened says the work board marks t reopened: a message that reached
// its closed specialist took it back to open (ADR-0061), and it is not
// closed again.
func Reopened(t proto.Thread) bool {
	return t.ReopenedAt != nil && t.State != proto.ThreadClosed
}

// ThreadLinks is the line a thread's card says of the threads it is
// linked to (ADR-0061): the one it carries on, and the newest that carries
// it on, "" for neither.
func ThreadLinks(t proto.Thread) string {
	var out string
	if t.Follows > 0 {
		out = "Carries on T" + strconv.FormatInt(t.Follows, 10) + "."
	}
	if t.Successor > 0 {
		if out != "" {
			out += " "
		}
		out += "Carried on in T" + strconv.FormatInt(t.Successor, 10) + "."
	}
	return out
}
