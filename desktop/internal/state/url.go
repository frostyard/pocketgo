package state

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// ParseServer reads a service URL as the sign-in form takes it and returns
// its canonical form, "scheme://host[:port]": the scheme is http or https,
// the host is set, and the URL has no user, path, query or fragment. A URL
// without a scheme is refused rather than guessed, so the person sees which
// scheme the token travels over.
func ParseServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("enter the service URL, such as https://pocketd.example.ts.net:8443")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the service URL does not parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return "", errors.New("the service URL needs its scheme: https:// or http://")
	default:
		return "", fmt.Errorf("the service URL's scheme is %q: use https:// or http://", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("the service URL has no host")
	}
	if u.User != nil {
		return "", errors.New("the service URL must not carry a user name or password")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("the service URL is the service's address only, without a path, query or fragment")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// NeedsHTTPWarning reports whether signing in to server (as ParseServer
// returns it) may send the device token in clear text, so the person
// confirms first: plain http to a host that is neither this computer nor a
// MagicDNS name of the tailnet. This computer is "localhost" or a loopback
// address. A MagicDNS name ends in .ts.net and resolves only inside the
// tailnet, whose WireGuard tunnel encrypts the traffic. An address in
// Tailscale's ranges (100.64.0.0/10, fd7a:115c:a1e0::/48) is not enough:
// 100.64.0.0/10 is shared address space that ISPs and companies route
// without any tunnel. A host name is never resolved, so a DNS answer cannot
// waive the warning.
func NeedsHTTPWarning(server string) bool {
	u, err := url.Parse(server)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return !ip.Unmap().IsLoopback()
	}
	return true
}

// SameOrigin reports whether to has the scheme, host and port of from. It
// is the only redirect the client follows (HTTPClient): a token the person
// confirmed for one service URL never goes to another host, nor over
// another scheme, as Go would send it on a redirect from https to http on
// the same host name.
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
