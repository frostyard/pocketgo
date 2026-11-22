package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/MichaelKinsy/PiG/mcp/oauth"
)

// MCPAuthFile holds the MCP servers' credentials (ADR-0063): the header
// values the owner stores with pocketd mcp set-header, and each OAuth
// sign-in pocketd mcp login makes, which pocketd refreshes. It lives in
// the data folder, which the sandbox hides from every command as it hides
// auth.json, and which the file tools refuse (Options.Protected).
const MCPAuthFile = "mcp-auth.json"

// MCPToolsFile holds the tools each MCP server listed last, so a server
// that is down when pocketd starts still offers them, and a call of one
// says the server is down. It holds no secret, but lives beside
// MCPAuthFile, with its one writer.
const MCPToolsFile = "mcp-tools.json"

// mcpCredential is what pocketd keeps for one server, by its name: it goes
// to URL alone, the server's URL when it was stored, so a server whose URL
// changed in mcp.json gets none of it until the owner stores it again.
type mcpCredential struct {
	URL     string               `json:"url"`
	Headers map[string]string    `json:"headers,omitempty"`
	OAuth   *oauth.McpOAuthState `json:"oauth,omitempty"`
}

// mcpListing is the tools a server at URL listed last.
type mcpListing struct {
	URL   string    `json:"url"`
	Tools []mcpTool `json:"tools"`
}

// mcpTool is one tool of a server, as pocketd keeps it.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// mcpStore is the one store of the MCP servers' credentials and listings,
// and their one writer: while pocketd serves, its engine's store alone
// writes the files; pocketd mcp login, set-header and logout write them
// only with the data folder's lock, which needs pocketd stopped. Each file
// is read once, when the store opens, and written whole, through a file
// beside it renamed into place, readable by the service user alone.
type mcpStore struct {
	dir   string
	mu    sync.Mutex
	creds map[string]*mcpCredential
	tools map[string]mcpListing
}

// errMCPStoreUnreadable is a store file that does not parse. Its own
// words: a parser's error may quote the file, a secret included.
var errMCPStoreUnreadable = errors.New("does not parse as pocketd wrote it")

// openMCPStore reads the MCP store in the data folder dir: empty when its
// files are not there.
func openMCPStore(dir string) (*mcpStore, error) {
	s := &mcpStore{dir: dir, creds: map[string]*mcpCredential{}, tools: map[string]mcpListing{}}
	var creds struct {
		Servers map[string]*mcpCredential `json:"servers"`
	}
	if err := readStoreFile(filepath.Join(dir, MCPAuthFile), &creds); err != nil {
		return nil, err
	}
	for name, c := range creds.Servers {
		if c != nil {
			s.creds[name] = c
		}
	}
	var tools struct {
		Servers map[string]mcpListing `json:"servers"`
	}
	if err := readStoreFile(filepath.Join(dir, MCPToolsFile), &tools); err != nil {
		return nil, err
	}
	maps.Copy(s.tools, tools.Servers)
	return s, nil
}

// readStoreFile reads the JSON file path into v: nothing when it is not
// there. A file that is no regular file is refused.
func readStoreFile(path string, v any) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, v) != nil {
		return fmt.Errorf("%s %w", filepath.Base(path), errMCPStoreUnreadable)
	}
	return nil
}

// writeStoreFile writes v to path whole, readable by the service user
// alone: a file beside it, synced, then renamed into place, so a reader
// finds the old file or the new one, never a part.
func writeStoreFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveCredsLocked writes the credentials file; mu is held.
func (s *mcpStore) saveCredsLocked() error {
	return writeStoreFile(filepath.Join(s.dir, MCPAuthFile), struct {
		Servers map[string]*mcpCredential `json:"servers"`
	}{s.creds})
}

// entryLocked is server name's credential for url, made empty when there
// is none or it was stored for another URL: what was stored for another
// one never goes to this one. mu is held.
func (s *mcpStore) entryLocked(name, url string) *mcpCredential {
	c := s.creds[name]
	if c == nil || c.URL != url {
		c = &mcpCredential{URL: url}
		s.creds[name] = c
	}
	return c
}

// headers are the values of server name's headers, stored for url, and
// the names of want that have none.
func (s *mcpStore) headers(name, url string, want []string) (map[string]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	var missing []string
	c := s.creds[name]
	for _, h := range want {
		if c != nil && c.URL == url && c.Headers[h] != "" {
			out[h] = c.Headers[h]
		} else {
			missing = append(missing, h)
		}
	}
	return out, missing
}

// setHeader keeps value as server name's header for url, in place of what
// was stored for it, and drops what was stored for another URL.
func (s *mcpStore) setHeader(name, url, header, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.entryLocked(name, url)
	if c.Headers == nil {
		c.Headers = map[string]string{}
	}
	c.Headers[header] = value
	return s.saveCredsLocked()
}

// forget drops what pocketd keeps for server name: its headers and its
// sign-in. It says whether there was any.
func (s *mcpStore) forget(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[name]; !ok {
		return false, nil
	}
	delete(s.creds, name)
	return true, s.saveCredsLocked()
}

// oauthState is a copy of server name's OAuth state for url, nil for none.
func (s *mcpStore) oauthState(name, url string) *oauth.McpOAuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.creds[name]
	if c == nil || c.URL != url || c.OAuth == nil {
		return nil
	}
	state := c.OAuth.Clone()
	return &state
}

// saveOAuth keeps state as server name's OAuth state for url.
func (s *mcpStore) saveOAuth(name, url string, state oauth.McpOAuthState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := state.Clone()
	s.entryLocked(name, url).OAuth = &clone
	return s.saveCredsLocked()
}

// signedIn says whether server name holds an OAuth sign-in for url: an
// access or a refresh token.
func (s *mcpStore) signedIn(name, url string) bool {
	state := s.oauthState(name, url)
	return state != nil && state.Tokens != nil && (state.Tokens.AccessToken != "" || state.Tokens.RefreshToken != "")
}

// listing is the tools server name at url listed last, and whether it
// listed any there.
func (s *mcpStore) listing(name, url string) ([]mcpTool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.tools[name]
	if !ok || l.URL != url {
		return nil, false
	}
	return slices.Clone(l.Tools), true
}

// setListing keeps tools as what server name at url listed last.
func (s *mcpStore) setListing(name, url string, tools []mcpTool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = mcpListing{URL: url, Tools: slices.Clone(tools)}
	return writeStoreFile(filepath.Join(s.dir, MCPToolsFile), struct {
		Servers map[string]mcpListing `json:"servers"`
	}{s.tools})
}

// secrets are every secret the store holds: the header values, and the
// OAuth tokens and client secrets. No text pocketd shows, a tool's result
// included, holds one (redact).
func (s *mcpStore) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.creds {
		for _, v := range c.Headers {
			out = append(out, v)
		}
		if c.OAuth == nil {
			continue
		}
		if t := c.OAuth.Tokens; t != nil {
			out = append(out, t.AccessToken, t.RefreshToken)
		}
		if ci := c.OAuth.ClientInformation; ci != nil {
			out = append(out, ci.ClientSecret)
		}
	}
	return out
}

// mcpOAuthStore is the store of one server's OAuth state, for PiG's
// provider: what pocketd keeps for the server's name and URL.
type mcpOAuthStore struct {
	store     *mcpStore
	name, url string
}

func (o mcpOAuthStore) Load(context.Context) (*oauth.McpOAuthState, error) {
	return o.store.oauthState(o.name, o.url), nil
}

func (o mcpOAuthStore) Save(_ context.Context, state oauth.McpOAuthState) error {
	return o.store.saveOAuth(o.name, o.url, state)
}
