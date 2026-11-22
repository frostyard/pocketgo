package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	// former are the secrets the store held and replaced or dropped since
	// it opened, newest last, at most maxFormerSecrets, in memory alone: an
	// exchange that went out with one may answer after a refresh replaced
	// it, and its answer is redacted all the same (secrets).
	former []string
	// pinned are the secrets each exchange under way went out with, by
	// pin, and every one the store replaced while it was under way, kept
	// however many former ones come after them (pin).
	pinned map[int][]string
	pins   int
	// fenced says the engine that owns the store closed: it writes no
	// file from then on, so a writer that takes the data folder's lock
	// after pocketd's stop is the only one.
	fenced bool
	// dirty says the credentials file is behind what the store holds: its
	// last write failed (saveCredsLocked).
	dirty bool
	// listingsRead says the store read the listings file, so it may write
	// it (openMCPStore, not openMCPCredentials).
	listingsRead bool
}

// errMCPStoreFenced is a write the store refuses once its engine closed.
var errMCPStoreFenced = errors.New("pocketd closed its MCP store: nothing is written")

// fence ends the store's writes (fenced), once it wrote the credentials
// a failed write left behind (dirty), a rotated refresh token perhaps,
// while pocketd still holds the data folder's lock: it says when that
// last write failed too.
func (s *mcpStore) fence() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.dirty && !s.fenced {
		err = s.saveCredsLocked()
	}
	s.fenced = true
	return err
}

// maxFormerSecrets bounds mcpStore.former.
const maxFormerSecrets = 256

// pin keeps the secrets the store holds now, and every one it holds and
// replaces until unpin is called, among those it redacts (secrets) until
// then, however many it replaces meanwhile: an exchange pins the
// credentials it goes out with until its answer is sanitized, since the
// server may echo them after a refresh replaced them, and PiG may make
// the request again with a token a refresh gave it meanwhile, which a
// later refresh may replace before the answer comes.
func (s *mcpStore) pin() (unpin func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held []string
	for _, c := range s.creds {
		held = append(held, credentialSecrets(c)...)
	}
	if s.pinned == nil {
		s.pinned = map[int][]string{}
	}
	s.pins++
	id := s.pins
	s.pinned[id] = held
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.pinned, id)
	}
}

// retireLocked keeps each secret of c, about to be replaced or dropped,
// among the former ones; mu is held.
func (s *mcpStore) retireLocked(c *mcpCredential) {
	if c == nil {
		return
	}
	for _, secret := range credentialSecrets(c) {
		if !slices.Contains(s.former, secret) {
			s.former = append(s.former, secret)
		}
		// An exchange under way may have gone out with it: it stays
		// redacted until that exchange's answer is.
		for id, held := range s.pinned {
			if !slices.Contains(held, secret) {
				s.pinned[id] = append(held, secret)
			}
		}
	}
	if extra := len(s.former) - maxFormerSecrets; extra > 0 {
		s.former = slices.Delete(s.former, 0, extra)
	}
}

// credentialSecrets are the secrets c holds: each header's value and its
// credential, the OAuth tokens (an OpenID one too) and the client secret.
func credentialSecrets(c *mcpCredential) []string {
	var out []string
	add := func(secret string) {
		if secret != "" {
			out = append(out, secret)
		}
	}
	for _, v := range c.Headers {
		add(v)
		add(headerCredential(v))
	}
	if c.OAuth != nil {
		if t := c.OAuth.Tokens; t != nil {
			add(t.AccessToken)
			add(t.RefreshToken)
			add(t.IDToken)
		}
		if ci := c.OAuth.ClientInformation; ci != nil {
			add(ci.ClientSecret)
		}
	}
	return out
}

// errMCPStoreUnreadable is a store file that does not parse. Its own
// words: a parser's error may quote the file, a secret included.
var errMCPStoreUnreadable = errors.New("does not parse as pocketd wrote it")

// openMCPStore reads the MCP store in the data folder dir: empty when its
// files are not there.
func openMCPStore(dir string) (*mcpStore, error) {
	s, err := openMCPCredentials(dir)
	if err != nil {
		return nil, err
	}
	var tools struct {
		Servers map[string]mcpListing `json:"servers"`
	}
	if err := readStoreFile(filepath.Join(dir, MCPToolsFile), &tools); err != nil {
		return nil, err
	}
	maps.Copy(s.tools, tools.Servers)
	s.listingsRead = true
	return s, nil
}

// openMCPCredentials is the store of dir's credentials alone, for a
// command that changes them and no listing (pocketd mcp logout): a
// listings file it cannot read stops it not, and it writes none.
func openMCPCredentials(dir string) (*mcpStore, error) {
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
	return s, nil
}

// errMCPListingsUnread is a listing kept in a store that did not read the
// listings file (openMCPCredentials): its write would drop the others.
var errMCPListingsUnread = errors.New("the MCP store did not read the listings: none is written")

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
	// Compact: an indentation would grow each kept schema past its bound.
	data, err := json.Marshal(v)
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
	if s.fenced {
		return errMCPStoreFenced
	}
	err := writeStoreFile(filepath.Join(s.dir, MCPAuthFile), struct {
		Servers map[string]*mcpCredential `json:"servers"`
	}{s.creds})
	// A write that failed leaves the file behind what the store holds, a
	// rotated refresh token perhaps: the next read writes it again
	// (flushLocked), with no second token exchange.
	s.dirty = err != nil
	return err
}

// flushLocked writes the credentials again when a write failed before;
// mu is held.
func (s *mcpStore) flushLocked() {
	if s.dirty && !s.fenced {
		_ = s.saveCredsLocked()
	}
}

// entryLocked is server name's credential for url, made empty when there
// is none or it was stored for another URL: what was stored for another
// one never goes to this one. mu is held.
func (s *mcpStore) entryLocked(name, url string) *mcpCredential {
	c := s.creds[name]
	if c == nil || c.URL != url {
		s.retireLocked(c)
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
	s.flushLocked()
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
	s.retireLocked(&mcpCredential{Headers: map[string]string{header: c.Headers[header]}})
	c.Headers[header] = value
	return s.saveCredsLocked()
}

// forget drops what pocketd keeps for server name: its headers and its
// sign-in. It says whether there was any.
func (s *mcpStore) forget(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[name]
	if !ok {
		return false, nil
	}
	s.retireLocked(c)
	delete(s.creds, name)
	return true, s.saveCredsLocked()
}

// oauthState is a copy of server name's OAuth state for url, nil for none.
func (s *mcpStore) oauthState(name, url string) *oauth.McpOAuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
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
	c := s.entryLocked(name, url)
	s.retireLocked(&mcpCredential{OAuth: c.OAuth})
	c.OAuth = &clone
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
	if s.fenced {
		return errMCPStoreFenced
	}
	if !s.listingsRead {
		return errMCPListingsUnread
	}
	s.tools[name] = mcpListing{URL: url, Tools: slices.Clone(tools)}
	return writeStoreFile(filepath.Join(s.dir, MCPToolsFile), struct {
		Servers map[string]mcpListing `json:"servers"`
	}{s.tools})
}

// secrets are every secret the store holds, longest first: each header's
// value, and its credential (headerCredential) on its own, as a server
// may echo the token alone; the OAuth tokens (an OpenID one too) and
// client secrets; and the ones it held since it opened (former). No text
// pocketd shows, a tool's result included, holds one (redact).
func (s *mcpStore) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	add := func(secret string) {
		if secret != "" && !slices.Contains(out, secret) {
			out = append(out, secret)
		}
	}
	for _, c := range s.creds {
		for _, secret := range credentialSecrets(c) {
			add(secret)
		}
	}
	for _, held := range s.pinned {
		for _, secret := range held {
			add(secret)
		}
	}
	for _, secret := range s.former {
		add(secret)
	}
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// headerCredential is the credential of a header's value: what follows
// its scheme, as "Bearer <token>" writes it, else the whole value.
func headerCredential(value string) string {
	if _, credential, ok := strings.Cut(value, " "); ok && strings.TrimSpace(credential) != "" {
		return strings.TrimSpace(credential)
	}
	return value
}

// mcpOAuthStore is the store of one server's OAuth state, for PiG's
// provider: what pocketd keeps for the server's name and URL.
type mcpOAuthStore struct {
	store     *mcpStore
	name, url string
	// log is told of a write that failed (nil for none).
	log *slog.Logger
}

func (o mcpOAuthStore) Load(context.Context) (*oauth.McpOAuthState, error) {
	return o.store.oauthState(o.name, o.url), nil
}

// Save keeps state. A write to the file that failed is no failure of the
// save: the store holds the state, which the next read writes again
// (mcpStore.flushLocked), and PiG's provider, which keeps a failed save's
// error for every later read and write, would leave the server unusable
// once the disk mends, with a rotated refresh token in memory alone. Only
// a store fenced at pocketd's stop refuses it.
func (o mcpOAuthStore) Save(_ context.Context, state oauth.McpOAuthState) error {
	err := o.store.saveOAuth(o.name, o.url, state)
	if err == nil || errors.Is(err, errMCPStoreFenced) {
		return err
	}
	if o.log != nil {
		o.log.Warn("keep an MCP server's sign-in: the write failed; it is made again at the next read", "server", o.name, "err", err)
	}
	return nil
}
