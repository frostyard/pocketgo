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

// tailnetIPv4 and tailnetIPv6 are the addresses Tailscale gives the devices
// of a tailnet: its CGNAT range and its unique local IPv6 prefix.
var (
	tailnetIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// NeedsHTTPWarning reports whether signing in to server (as ParseServer
// returns it) sends the device token in clear text over a network nothing
// protects, so the person confirms first: plain http to a host that is
// neither this computer nor on the tailnet. This computer is "localhost" or
// a loopback address; the tailnet is 100.64.0.0/10, fd7a:115c:a1e0::/48 and
// MagicDNS names under .ts.net, which WireGuard encrypts. A host name is
// never resolved, so a DNS answer cannot waive the warning.
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
		ip = ip.Unmap()
		return !ip.IsLoopback() && !tailnetIPv4.Contains(ip) && !tailnetIPv6.Contains(ip)
	}
	return true
}
