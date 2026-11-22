package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MCPFile is the owner's MCP servers, in the profile folder, which agents'
// commands see read-only (ADR-0063). It has the shape of Pi's
// ~/.pi/agent/mcp.json, servers by name under "mcpServers", but holds no
// secret: agents read the profile folder, so a header's value lives in
// pocketd's data folder (pocketd mcp set-header), and an OAuth sign-in too
// (pocketd mcp login).
const MCPFile = "mcp.json"

// MCPStored is the one value a header takes in MCPFile: pocketd keeps the
// value, and sends it only to the URL it was stored for.
const MCPStored = "stored"

// The bounds of the owner's MCP servers (ADR-0063).
const (
	MaxMCPServers = 16
	// MaxMCPName bounds a server's name: its tools are named
	// mcp__<server>__<tool>, which a model provider takes up to 64
	// characters of.
	MaxMCPName = 32
	// MaxMCPTimeout bounds a server's timeout, in seconds.
	MaxMCPTimeout = 600
	// DefaultMCPTimeout is a request's timeout when the server names none.
	DefaultMCPTimeout = 60 * time.Second
	maxMCPHeaders     = 8
	maxMCPScope       = 512
	maxMCPClientID    = 256
)

// DefaultMCPCallback is the redirect URI of an OAuth sign-in that names
// none: pocketd mcp login reads the address the browser lands on, pasted,
// so nothing has to listen there.
const DefaultMCPCallback = "http://127.0.0.1:8789/callback"

// MCPServer is one of the owner's MCP servers, as checked.
type MCPServer struct {
	// Name is the server's key in MCPFile, which a profile's mcp list
	// names.
	Name string
	// URL is the server's Streamable HTTP endpoint, as written.
	URL string
	// Headers are the names of the headers pocketd sends it, each with
	// the value it keeps for the server (MCPStored), canonical, sorted.
	Headers []string
	// OAuth is the server's OAuth sign-in, nil for none.
	OAuth *MCPOAuth
	// Timeout bounds each request to it.
	Timeout time.Duration
}

// MCPOAuth is an OAuth sign-in's settings: a pre-registered public client
// (none: pocketd registers one dynamically), the scopes it asks for (none:
// the ones the server names) and the redirect URI.
type MCPOAuth struct {
	ClientID, Scope, CallbackURL string
}

// MCPServers are the owner's MCP servers, by name, sorted.
type MCPServers struct {
	Servers []MCPServer
}

// Find is the server named name.
func (s MCPServers) Find(name string) (MCPServer, bool) {
	i := slices.IndexFunc(s.Servers, func(server MCPServer) bool { return server.Name == name })
	if i < 0 {
		return MCPServer{}, false
	}
	return s.Servers[i], true
}

// MCPNamespace is how a server's name reads in its tools' names: every
// character but a letter, a digit or _ made _ (MCPToolName).
func MCPNamespace(server string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, server)
}

// LoadMCP reads the owner's MCP servers in dir: none when there is no
// file. A file that is no regular file of its own, or breaks a rule of
// ParseMCP, is an error: none of its servers connects.
//
// The folder is opened first, as Folder.MCP opens it: a folder that is not
// there is an error, never a missing file.
func LoadMCP(dir string) (MCPServers, error) {
	return Folder{Dir: dir}.MCP()
}

// MCP reads the folder's MCP servers, through the folder pocketd pinned at
// start (Folder.read).
func (f Folder) MCP() (MCPServers, error) {
	data, err := f.read(MCPFile)
	if errors.Is(err, fs.ErrNotExist) {
		return MCPServers{}, nil
	}
	if err != nil {
		return MCPServers{}, fmt.Errorf("%s: %w", MCPFile, err)
	}
	servers, err := ParseMCP(data)
	if err != nil {
		return MCPServers{}, fmt.Errorf("%s: %w", MCPFile, err)
	}
	return servers, nil
}

// ParseMCP checks the owner's MCP servers (ADR-0063): {"mcpServers":
// {name: {url, headers, oauth, timeout, type}}}, 1 to MaxMCPServers of
// them. Every object's keys are read as written, as ParseProviders reads
// them: another case, a key named twice or an unknown key is refused.
//
// A refusal names a value by its place, never by the value, which may be
// a secret pasted in the wrong place: pocketd logs it.
func ParseMCP(data []byte) (MCPServers, error) {
	if len(data) > maxProfileSize {
		return MCPServers{}, fmt.Errorf("the file is larger than %d bytes", maxProfileSize)
	}
	if !utf8.Valid(data) {
		return MCPServers{}, errors.New("the file is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var out MCPServers
	seen := false
	err := readObject(dec, "the file", func(key string) error {
		if key != "mcpServers" {
			return fmt.Errorf("the file has the field %q: it takes mcpServers", key)
		}
		seen = true
		return readObject(dec, "mcpServers", func(name string) error {
			server, err := readMCPServer(dec, name)
			if err != nil {
				return err
			}
			out.Servers = append(out.Servers, server)
			return nil
		})
	})
	if err != nil {
		return MCPServers{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return MCPServers{}, errors.New("text follows the file's object")
	}
	if !seen || len(out.Servers) == 0 || len(out.Servers) > MaxMCPServers {
		return MCPServers{}, fmt.Errorf(`it names %d servers, not 1 to %d: {"mcpServers": {"name": {"url": …}}}`, len(out.Servers), MaxMCPServers)
	}
	// Two names whose tools would read alike (a-b and a_b) are refused, as
	// Pi refuses them.
	byNamespace := map[string]string{}
	for _, s := range out.Servers {
		if other, ok := byNamespace[MCPNamespace(s.Name)]; ok {
			return MCPServers{}, fmt.Errorf("servers %q and %q would name their tools alike", other, s.Name)
		}
		byNamespace[MCPNamespace(s.Name)] = s.Name
	}
	slices.SortFunc(out.Servers, func(a, b MCPServer) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// readMCPServer reads and checks the server named name.
func readMCPServer(dec *json.Decoder, name string) (MCPServer, error) {
	where := fmt.Sprintf("server %q", name)
	if !mcpPattern.MatchString(name) || len(name) > MaxMCPName {
		// The name is a key a profile names; quoted, it stays on its line.
		return MCPServer{}, fmt.Errorf("%s: the name is not letters, digits, _, . and -, starting with a letter or a digit, at most %d", where, MaxMCPName)
	}
	s := MCPServer{Name: name, Timeout: DefaultMCPTimeout}
	var hasURL bool
	type header struct{ name, value string }
	var headers []header
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "url":
			hasURL = true
			return decodeString(dec, where, key, &s.URL)
		case "type":
			var kind string
			if err := decodeString(dec, where, key, &kind); err != nil {
				return err
			}
			if kind != "http" && kind != "streamable-http" {
				return fmt.Errorf("%s: the type is not http or streamable-http: pocketd connects HTTP servers alone", where)
			}
			return nil
		case "headers":
			return readObject(dec, where+"'s headers", func(name string) error {
				var value string
				if err := decodeString(dec, where+"'s headers", name, &value); err != nil {
					return err
				}
				headers = append(headers, header{name, value})
				return nil
			})
		case "oauth":
			o, err := readMCPOAuth(dec, where+"'s oauth")
			if err != nil {
				return err
			}
			s.OAuth = &o
			return nil
		case "timeout":
			var seconds int
			if err := decodePositive(dec, where, key, &seconds); err != nil {
				return err
			}
			if seconds > MaxMCPTimeout {
				return fmt.Errorf("%s: the timeout %d is not 1 to %d seconds", where, seconds, MaxMCPTimeout)
			}
			s.Timeout = time.Duration(seconds) * time.Second
			return nil
		case "command", "args", "env", "cwd":
			return fmt.Errorf("%s has the field %q: pocketd connects HTTP servers alone, since a stdio server's command would run outside the agents' sandbox", where, key)
		case "auth":
			return fmt.Errorf("%s has the field %q: pocketd sends no model provider's sign-in to an MCP server", where, key)
		}
		return fmt.Errorf("%s has the field %q: it takes url, type, headers, oauth and timeout", where, key)
	})
	if err != nil {
		return MCPServer{}, err
	}
	if !hasURL {
		return MCPServer{}, fmt.Errorf("%s names no url", where)
	}
	if err := checkMCPURL(s.URL); err != nil {
		return MCPServer{}, fmt.Errorf("%s: the url %w", where, err)
	}
	if len(headers) > maxMCPHeaders {
		return MCPServer{}, fmt.Errorf("%s names %d headers, at most %d", where, len(headers), maxMCPHeaders)
	}
	for _, h := range headers {
		canonical, err := checkMCPHeader(h.name)
		if err != nil {
			return MCPServer{}, fmt.Errorf("%s: %w", where, err)
		}
		if h.value != MCPStored {
			// Never the value: it is likely the secret itself.
			return MCPServer{}, fmt.Errorf(`%s: the header %s holds a value: agents read the profile folder, so pocketd keeps a header's value: run pocketd mcp set-header %s %s, and write "%s": "%s"`,
				where, canonical, name, canonical, canonical, MCPStored)
		}
		if slices.Contains(s.Headers, canonical) {
			return MCPServer{}, fmt.Errorf("%s names the header %s twice", where, canonical)
		}
		s.Headers = append(s.Headers, canonical)
	}
	slices.Sort(s.Headers)
	if s.OAuth != nil && slices.Contains(s.Headers, "Authorization") {
		return MCPServer{}, fmt.Errorf("%s names both oauth and an Authorization header: an OAuth sign-in sends its own", where)
	}
	return s, nil
}

// readMCPOAuth reads and checks a server's oauth object.
func readMCPOAuth(dec *json.Decoder, where string) (MCPOAuth, error) {
	o := MCPOAuth{CallbackURL: DefaultMCPCallback}
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "clientId":
			return decodeString(dec, where, key, &o.ClientID)
		case "scope":
			return decodeString(dec, where, key, &o.Scope)
		case "callbackUrl":
			return decodeString(dec, where, key, &o.CallbackURL)
		case "clientSecret":
			return fmt.Errorf("%s has the field %q: a secret does not go in the profile folder, which agents read: pocketd signs in as a public client", where, key)
		}
		return fmt.Errorf("%s has the field %q: it takes clientId, scope and callbackUrl", where, key)
	})
	if err != nil {
		return MCPOAuth{}, err
	}
	if len(o.ClientID) > maxMCPClientID || !printable(o.ClientID) {
		return MCPOAuth{}, fmt.Errorf("%s: the clientId is longer than %d characters or not printable ASCII", where, maxMCPClientID)
	}
	if len(o.Scope) > maxMCPScope || !printable(o.Scope) {
		return MCPOAuth{}, fmt.Errorf("%s: the scope is longer than %d characters or not printable ASCII", where, maxMCPScope)
	}
	if err := checkMCPCallback(o.CallbackURL); err != nil {
		return MCPOAuth{}, fmt.Errorf("%s: the callbackUrl %w", where, err)
	}
	return o, nil
}

// printable says whether s is printable ASCII, spaces included.
func printable(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7e })
}

// headerToken is an HTTP header's name (RFC 9110's token).
var headerToken = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$")

// reservedHeaders are the headers pocketd's MCP client sets itself, or the
// connection's own: a stored value never replaces one.
var reservedHeaders = []string{
	"Accept", "Accept-Encoding", "Connection", "Content-Length", "Content-Type", "Cookie", "Host", "Keep-Alive",
	"Last-Event-Id", "Mcp-Protocol-Version", "Mcp-Session-Id", "Proxy-Authorization", "Te", "Trailer",
	"Transfer-Encoding", "Upgrade",
}

// checkMCPHeader is a header's canonical name, or why the name is
// refused.
func checkMCPHeader(name string) (string, error) {
	if !headerToken.MatchString(name) {
		return "", errors.New("a header's name is not an HTTP header name of at most 64 characters")
	}
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	if slices.Contains(reservedHeaders, canonical) {
		return "", fmt.Errorf("the header %s is one pocketd's MCP client sets itself", canonical)
	}
	return canonical, nil
}

// checkMCPURL refuses a server's URL that is not https, or http on a
// loopback host or a tailnet name (*.ts.net, which WireGuard carries), or
// that has a user, a query or a fragment: a secret goes in pocketd's data
// folder, never in the URL. A refusal never repeats the URL, nor
// url.Parse's error, which quotes it.
func checkMCPURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case raw == "":
		return errors.New("is missing")
	case err != nil:
		return errors.New("is no URL")
	case u.Scheme != "https" && u.Scheme != "http":
		return errors.New("is not https")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("names no host")
	case u.User != nil:
		return errors.New("names a user or a password: a secret goes in pocketd mcp set-header")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("has a query or a fragment: a secret goes in pocketd mcp set-header")
	case u.Scheme == "http" && !plainHTTPHost(u.Hostname()):
		return errors.New("is http on a host that is neither loopback nor a tailnet name (*.ts.net): use https")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("has a port that is not 1 to 65535")
		}
	}
	return nil
}

// plainHTTPHost says whether host may take plain HTTP: a loopback host,
// or a tailnet name, whose traffic WireGuard encrypts.
func plainHTTPHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".ts.net") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkMCPCallback refuses a redirect URI that is not http on a loopback
// host (RFC 8252), or that has a user, a query or a fragment.
func checkMCPCallback(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("is no URL")
	case u.Scheme != "http":
		return errors.New("is not http on a loopback host")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("has a user, a query or a fragment")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("is not http on a loopback host")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("has a port that is not 1 to 65535")
		}
	}
	return nil
}
