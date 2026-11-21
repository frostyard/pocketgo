package proto

import (
	"net/url"
	"strings"
)

// SameOrigin reports whether to has the scheme, host and port of from: the
// scheme and the host in any case, the host without a final dot, and a
// port its scheme's default when it names none. An upload's redirect
// follows only such a URL (Client.Upload), and so does every request of
// the desktop (its state.SameOrigin).
func SameOrigin(from, to *url.URL) bool {
	return strings.EqualFold(from.Scheme, to.Scheme) &&
		strings.EqualFold(strings.TrimSuffix(from.Hostname(), "."), strings.TrimSuffix(to.Hostname(), ".")) &&
		portOf(from) == portOf(to)
}

// portOf is a URL's port, its scheme's default when it names none.
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}
