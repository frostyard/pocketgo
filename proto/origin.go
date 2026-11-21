package proto

import (
	"net/netip"
	"net/url"
	"strings"
)

// SameOrigin reports whether to has the scheme, host and port of from: the
// scheme in any case, the host as the resolver looks it up (sameHost),
// and a port its scheme's default when it names none. An upload's
// redirect follows only such a URL (Client.Upload), and so does every
// request of the desktop (its state.SameOrigin).
func SameOrigin(from, to *url.URL) bool {
	return strings.EqualFold(from.Scheme, to.Scheme) &&
		sameHost(from.Hostname(), to.Hostname()) &&
		portOf(from) == portOf(to)
}

// sameHost says whether hosts a and b are one: an IP address as itself,
// its zone exactly; an ASCII name in any ASCII case, a final dot kept,
// since a name with one bypasses the resolver's search list and may name
// another machine (Codex review of #49, round 8); any other name byte for
// byte, since no fold of a name outside ASCII is known to name the host
// the transport looks up, as its IDNA form may differ (Codex review of
// #49, round 7).
func sameHost(a, b string) bool {
	if a == b {
		return true
	}
	if x, err := netip.ParseAddr(a); err == nil {
		y, err := netip.ParseAddr(b)
		return err == nil && x == y
	}
	if !isASCII(a) || !isASCII(b) {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// isASCII says whether s is ASCII alone.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
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
