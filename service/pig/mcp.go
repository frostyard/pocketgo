package pig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A profile's MCP servers (ADR-0063) are connected by pocketd itself, not
// by a tool's command: each is one Streamable HTTP connection with PiG's
// mcp package, made when a tool of it is first called or when pocketd
// starts (ConnectMCP), shared by every session of the profiles that name
// it. Its tools are offered as mcp__<server>__<tool>, as PiG names them,
// in one durable extension a profile (mcpExtensionName): a session selects
// its own profile's alone, so it reaches no other profile's servers. The
// credentials (mcpStore) stay in pocketd: a tool sees what the server
// answers, never a header's value or a token.

// mcpToolPrefix starts every MCP tool's name, and no other tool's.
const mcpToolPrefix = "mcp__"

// The bounds of what pocketd takes from an MCP server.
const (
	// maxMCPToolName is a tool's name at most: a model provider takes 64
	// characters.
	maxMCPToolName = 64
	// maxMCPTools is the tools pocketd offers of one server at most.
	maxMCPTools = 128
	// maxMCPDescription bounds a tool's description, in bytes.
	maxMCPDescription = 2048
	// maxMCPSchema bounds a tool's input schema, in bytes: a larger one
	// is offered as an object of any properties.
	maxMCPSchema = 32 << 10
	// maxMCPOutput bounds the text of a call's result, in bytes.
	maxMCPOutput = 64 << 10
	// maxMCPImages bounds the images of a call's result, and
	// maxMCPImageBytes each one's data.
	maxMCPImages     = 4
	maxMCPImageBytes = 4 << 20
	// maxMCPServerText bounds what a server's own error says, in bytes.
	maxMCPServerText = 1000
)

// mcpTransport carries the MCP servers' requests; nil is directTransport.
// Tests set it, so none reaches a network.
var mcpTransport http.RoundTripper

// mcpClient is the HTTP client of the MCP servers' requests, their OAuth
// ones included: on directTransport, no proxy whatever HTTP_PROXY says,
// and a redirect followed only to the origin it came from, so a stored
// header never reaches a host mcp.json does not name.
func mcpClient() *http.Client {
	var transport http.RoundTripper = directTransport
	if mcpTransport != nil {
		transport = mcpTransport
	}
	return &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}}
}

// mcpExtensionName is the durable extension of profile's MCP tools.
func mcpExtensionName(profile string) string { return "pocketgo-mcp-" + profile }

// isMCPExtension says whether an extension holds a profile's MCP tools.
func isMCPExtension(name string) bool { return strings.HasPrefix(name, "pocketgo-mcp-") }

// mcpToolName is how a server's tool is offered: mcp__<server>__<tool>,
// every character but a letter, a digit or _ made _, as PiG names them,
// and shortened with a digest of both names when it is too long, or when
// taken says another tool has it already.
func mcpToolName(server, tool string, taken func(string) bool) string {
	name := mcpToolPrefix + profiles.MCPNamespace(server) + "__" + profiles.MCPNamespace(tool)
	if len(name) <= maxMCPToolName && (taken == nil || !taken(name)) {
		return name
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	digest := hex.EncodeToString(sum[:])[:8]
	return name[:min(len(name), maxMCPToolName-len(digest)-1)] + "_" + digest
}

// mcpHub holds the MCP servers the profiles name, as pocketd read them at
// start: mcp.json and each profile's mcp list. A change to either applies
// when pocketd starts again.
type mcpHub struct {
	store    *mcpStore
	registry harness.Registry
	log      *slog.Logger
	// conns are the servers some profile names, by name.
	conns map[string]*mcpConn
	// servers are each profile's servers that mcp.json has, in the
	// profile's order.
	servers map[string][]string
	// mu guards tools and exts.
	mu sync.Mutex
	// tools are each server's tools, as it listed them last.
	tools map[string][]mcpTool
	// exts are each profile's extension, as last installed.
	exts map[string]*durable.Extension
}

// mcpConn is one server's connection: made when needed, and made again
// after a failure.
type mcpConn struct {
	hub    *mcpHub
	server profiles.MCPServer
	// mu guards client and state; it is taken before hub.mu, never after.
	mu     sync.Mutex
	client *mcp.Client
	state  string
}

// newMCPHub is the hub of servers, as mcp.json has them, for the profiles'
// mcp lists (profile id to server names), with the tools each listed last
// from store, its profiles' extensions installed in registry.
func newMCPHub(store *mcpStore, registry harness.Registry, servers profiles.MCPServers, lists map[string][]string, log *slog.Logger) (*mcpHub, error) {
	h := &mcpHub{store: store, registry: registry, log: log, conns: map[string]*mcpConn{}, servers: map[string][]string{},
		tools: map[string][]mcpTool{}, exts: map[string]*durable.Extension{}}
	for profile, names := range lists {
		for _, name := range names {
			server, ok := servers.Find(name)
			if !ok {
				continue
			}
			h.servers[profile] = append(h.servers[profile], name)
			if _, ok := h.conns[name]; !ok {
				h.conns[name] = &mcpConn{hub: h, server: server}
				if tools, ok := store.listing(name, server.URL); ok {
					h.tools[name] = tools
				}
			}
		}
	}
	for profile := range h.servers {
		if err := h.install(profile); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// profiles are the profiles with an MCP server, sorted.
func (h *mcpHub) profiles() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.servers))
	for p := range h.servers {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// extension is profile's extension, nil when it has no server.
func (h *mcpHub) extension(profile string) *durable.Extension {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exts[profile]
}

// install builds profile's extension from its servers' tools and installs
// it in place of the one installed.
func (h *mcpHub) install(profile string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.installLocked(profile)
}

func (h *mcpHub) installLocked(profile string) error {
	ext := &durable.Extension{Name: mcpExtensionName(profile), Tools: []*durable.ToolRegistration{}}
	taken := map[string]bool{}
	for _, name := range h.servers[profile] {
		c := h.conns[name]
		for _, tool := range h.tools[name] {
			offered := mcpToolName(name, tool.Name, func(n string) bool { return taken[n] })
			taken[offered] = true
			ext.Tools = append(ext.Tools, c.registration(offered, tool))
		}
	}
	if err := h.registry.Install(ext); err != nil {
		return fmt.Errorf("install the MCP tools of %s: %w", profile, err)
	}
	h.exts[profile] = ext
	return nil
}

// listed takes tools as what server name listed last, keeps them, and
// installs again the extension of every profile that names it, when they
// changed.
func (h *mcpHub) listed(name string, tools []mcpTool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if mcpToolsEqual(h.tools[name], tools) {
		return
	}
	h.tools[name] = tools
	if err := h.store.setListing(name, h.conns[name].server.URL, tools); err != nil {
		h.log.Warn("keep an MCP server's tools", "server", name, "err", err)
	}
	for profile, names := range h.servers {
		if slices.Contains(names, name) {
			if err := h.installLocked(profile); err != nil {
				h.log.Warn("offer an MCP server's tools", "server", name, "profile", profile, "err", err)
			}
		}
	}
}

func mcpToolsEqual(a, b []mcpTool) bool {
	return slices.EqualFunc(a, b, func(x, y mcpTool) bool {
		return x.Name == y.Name && x.Description == y.Description && string(x.InputSchema) == string(y.InputSchema)
	})
}

// connectAll connects every server, or reads its tools again, at once,
// and returns when each has answered or failed.
func (h *mcpHub) connectAll(ctx context.Context) {
	if h == nil {
		return
	}
	var wg sync.WaitGroup
	for _, c := range h.conns {
		wg.Go(func() {
			if _, err := c.connect(ctx, true); err != nil {
				c.note(err)
			}
		})
	}
	wg.Wait()
}

// close ends every connection, at once, and returns once each ended or
// ctx did. A connection being made is left to end with pocketd.
func (h *mcpHub) close(ctx context.Context) {
	if h == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, c := range h.conns {
			if !c.mu.TryLock() {
				continue
			}
			client := c.client
			c.client = nil
			c.mu.Unlock()
			if client != nil {
				wg.Go(func() { _ = client.Close() })
			}
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// registration is server tool offered as name in a profile's extension.
// A call is not replayed after a restart: what it did on the server is not
// known, and may have happened.
func (c *mcpConn) registration(name string, tool mcpTool) *durable.ToolRegistration {
	description := tool.Description
	if description == "" {
		description = "MCP tool " + tool.Name + " of the server " + c.server.Name + "."
	}
	return &durable.ToolRegistration{
		ToolSchema: mcpSchema(name, description, tool.InputSchema),
		Replay:     durable.ReplayUnsafe,
		Execute: func(ctx context.Context, args any, _ durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
			return c.call(ctx, tool.Name, args), nil
		},
	}
}

// emptySchema is the input schema of a tool whose own pocketd cannot
// take: an object of any properties.
const emptySchema = `{"type":"object","properties":{}}`

// mcpSchema is a tool's schema from a server's input schema: an object,
// with a type and properties, as a model provider wants one.
func mcpSchema(name, description string, input json.RawMessage) ai.ToolSchema {
	params := json.RawMessage(emptySchema)
	if len(input) > 0 && len(input) <= maxMCPSchema {
		var object map[string]json.RawMessage
		if json.Unmarshal(input, &object) == nil && object != nil {
			if t, ok := object["type"]; !ok || string(t) == "null" {
				object["type"] = json.RawMessage(`"object"`)
			}
			if _, ok := object["properties"]; !ok {
				object["properties"] = json.RawMessage(`{}`)
			}
			if data, err := json.Marshal(object); err == nil {
				params = data
			}
		}
	}
	encoded, err := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}{name, description, params})
	var out ai.ToolSchema
	if err != nil || json.Unmarshal(encoded, &out) != nil {
		return schema(name, description, emptySchema)
	}
	return out
}

// toolsOf are the tools pocketd takes of a server's list: those with a
// name, at most maxMCPTools, each description cut to its bound.
func toolsOf(listed []mcp.Tool) []mcpTool {
	out := []mcpTool{}
	for _, t := range listed {
		if t.Name == "" || len(out) == maxMCPTools {
			continue
		}
		description := strings.TrimSpace(t.Description)
		if description == "" {
			description = strings.TrimSpace(t.Title)
		}
		out = append(out, mcpTool{Name: t.Name, Description: cut(description, maxMCPDescription), InputSchema: t.InputSchema})
	}
	return out
}

// cut is s cut to at most n bytes, at a character (cutText).
func cut(s string, n int) string {
	out, _ := cutText(s, n)
	return out
}

// The states of a connection, as the log and the tools say them.
var (
	errMCPNeedsHeader = errors.New("needs a header pocketd does not keep")
	errMCPNotSignedIn = errors.New("is not signed in")
	errMCPSignIn      = errors.New("needs the owner to sign in again")
	// errMCPRefresh is a sign-in PiG's flow could not refresh, for no
	// refusal of the authorization server's: it did not answer, answered
	// server_error, or the sign-in holds no refresh token. PiG's flow then
	// starts an authorization, which refreshOnly refuses with it.
	errMCPRefresh = errors.New("could not refresh its sign-in")
)

// connect is the server's connection, made when there is none; list reads
// its tools again on one there is. It needs every header mcp.json names
// stored for the server's URL, and its sign-in when it signs in, else it
// tries nothing.
func (c *mcpConn) connect(ctx context.Context, list bool) (*mcp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil && c.client.ConnectionState() == mcp.ClientStateConnected {
		if list {
			if err := c.listLocked(ctx, c.client); err != nil {
				c.dropLocked(c.client)
				return nil, err
			}
		}
		return c.client, nil
	}
	c.dropLocked(c.client)
	store, server := c.hub.store, c.server
	headers, missing := store.headers(server.Name, server.URL, server.Headers)
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s (pocketd mcp set-header %s %s)", errMCPNeedsHeader, missing[0], server.Name, missing[0])
	}
	options := mcp.StreamableHTTPTransportOptions{URL: server.URL, Headers: headers, Fetch: mcpClient(), OpenGetStream: new(false)}
	if server.OAuth != nil {
		if !store.signedIn(server.Name, server.URL) {
			return nil, errMCPNotSignedIn
		}
		provider, err := oauth.NewMcpOAuthProvider(oauth.McpOAuthProviderOptions{
			ServerURL: server.URL, RedirectURL: server.OAuth.CallbackURL, ClientID: server.OAuth.ClientID,
			ClientMetadata: oauth.OAuthClientMetadata{ClientName: mcpClientName, Scope: server.OAuth.Scope},
			Store:          mcpOAuthStore{store: store, name: server.Name, url: server.URL},
		})
		if err != nil {
			return nil, err
		}
		options.AuthProvider = oauth.AdaptOAuthProvider(refreshOnly{provider})
	}
	transport, err := mcp.NewStreamableHTTPTransport(options)
	if err != nil {
		return nil, err
	}
	timeout := int(server.Timeout / time.Millisecond)
	client := mcp.NewClient(mcp.ClientOptions{Implementation: mcp.Implementation{Name: mcpClientName, Version: "1"}, RequestTimeoutMs: timeout})
	connectCtx, cancel := context.WithTimeout(ctx, server.Timeout)
	defer cancel()
	if _, err := client.Connect(connectCtx, transport); err != nil {
		return nil, err
	}
	if err := c.listLocked(connectCtx, client); err != nil {
		_ = client.Close()
		return nil, err
	}
	c.client = client
	c.setStateLocked("connected", nil)
	return client, nil
}

// mcpClientName names pocketd to an MCP server, and in a dynamic client
// registration.
const mcpClientName = "pocketd"

// listLocked reads the server's tools on client; mu is held.
func (c *mcpConn) listLocked(ctx context.Context, client *mcp.Client) error {
	listed, err := client.ListTools(ctx, mcp.RequestOptions{})
	if err != nil {
		return err
	}
	c.hub.listed(c.server.Name, toolsOf(listed))
	return nil
}

// dropLocked closes client, the connection's or an old one; mu is held.
func (c *mcpConn) dropLocked(client *mcp.Client) {
	if client == nil {
		return
	}
	if c.client == client {
		c.client = nil
	}
	_ = client.Close()
}

// drop closes client, when it is still the connection's.
func (c *mcpConn) drop(client *mcp.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == client {
		c.dropLocked(client)
	}
}

// note logs what err says of the connection, once a state; a call that
// was stopped says nothing of it.
func (c *mcpConn) note(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setStateLocked("", err)
}

// setStateLocked logs the connection's state when it changed: state, or
// what err says; mu is held. No log line holds a secret (failure).
func (c *mcpConn) setStateLocked(state string, err error) {
	if err != nil {
		state = c.failure(err)
	}
	if state == c.state {
		return
	}
	c.state = state
	if err != nil {
		c.hub.log.Warn("an MCP server is not connected", "server", c.server.Name, "why", state)
		return
	}
	c.hub.log.Info("an MCP server is connected", "server", c.server.Name)
}

// call calls tool with args on the server, connecting first when needed.
// A server that is down, or refuses pocketd, is an error result that says
// so, never a failed task: the session goes on. A call whose session the
// server ended is made once more on a new connection, since the server
// did not run it; any other failure is not, since it may have.
func (c *mcpConn) call(ctx context.Context, tool string, args any) durable.ToolExecutionResult {
	if args == nil {
		args = map[string]any{}
	}
	options := mcp.RequestOptions{TimeoutMs: int(c.server.Timeout / time.Millisecond)}
	for attempt := 0; ; attempt++ {
		client, err := c.connect(ctx, false)
		if err != nil {
			c.note(err)
			return c.errorResult(err)
		}
		result, err := client.CallTool(ctx, tool, args, options)
		if err == nil {
			return c.result(result)
		}
		var expired *mcp.McpSessionExpiredError
		var rpc *mcp.McpError
		switch {
		case errors.As(err, &expired) && attempt == 0:
			c.drop(client)
			continue
		case errors.As(err, &rpc):
			// The server's own answer to the call: the connection holds.
			return toolText("", fmt.Errorf("MCP server %s refused the call: %s", c.server.Name, c.redact(cut(rpc.Message, maxMCPServerText))))
		}
		c.drop(client)
		c.note(err)
		return c.errorResult(err)
	}
}

// errorResult is a call's error result for err, which names no secret.
func (c *mcpConn) errorResult(err error) durable.ToolExecutionResult {
	return toolText("", fmt.Errorf("MCP server %s %s", c.server.Name, c.failure(err)))
}

// failure says what err means for the server in pocketd's own words, and
// what the owner does about it: PiG's errors may hold a body the server
// sent, and an OAuth server's description, neither of which pocketd shows.
func (c *mcpConn) failure(err error) string {
	name := c.server.Name
	var auth *mcp.McpAuthRequiredError
	var status *mcp.McpHttpError
	var oauthErr *oauth.OAuthError
	var timeout *mcp.McpTimeoutError
	var rpc *mcp.McpError
	switch {
	case errors.Is(err, errMCPNeedsHeader):
		return "is not connected: it " + c.redact(err.Error())
	case errors.Is(err, errMCPNotSignedIn):
		return "is not connected: it is not signed in: the owner signs in with pocketd mcp login " + name
	case errors.Is(err, errMCPSignIn), errors.As(err, &oauthErr):
		return "refused pocketd's sign-in, and its refresh: the owner signs in again with pocketd mcp login " + name
	case errors.Is(err, errMCPRefresh):
		return "refused pocketd's sign-in, and pocketd could not refresh it now: try again later; if it goes on, the owner signs in again with pocketd mcp login " + name
	case errors.As(err, &auth):
		if c.server.OAuth != nil {
			return "refused pocketd's sign-in (HTTP 401): the owner signs in again with pocketd mcp login " + name
		}
		return "refused pocketd's stored headers (HTTP 401): the owner stores them again with pocketd mcp set-header " + name + " HEADER"
	case errors.As(err, &status):
		return fmt.Sprintf("answered HTTP %d: try again later", status.Status)
	case errors.As(err, &timeout), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("did not answer within %s: it may be down; try again later", c.server.Timeout)
	case errors.Is(err, context.Canceled):
		return "was not reached: the call was stopped"
	case errors.As(err, &rpc):
		return "refused the connection: " + c.redact(cut(rpc.Message, maxMCPServerText))
	}
	return "is unreachable: pocketd could not connect to it; it may be down: try again later"
}

// redact is text with every secret the store holds in its place: a
// server may echo what it was sent.
func (c *mcpConn) redact(text string) string {
	for _, secret := range c.hub.store.secrets() {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// result is a call's result for the model: its text and images, within
// their bounds, an error result when the server says the call failed, no
// secret in it.
func (c *mcpConn) result(result *mcp.CallToolResult) durable.ToolExecutionResult {
	var out []ai.ToolResultMessageContent
	text, images := 0, 0
	for _, block := range mcp.ToLLMContent(*result) {
		switch block.Type {
		case mcp.LLMContentTypeImage:
			if images >= maxMCPImages || len(block.Data) > maxMCPImageBytes {
				out = append(out, ai.TextContent{Text: "[an image pocketd left out: too many, or too large]"})
				continue
			}
			images++
			out = append(out, ai.ImageContent{Data: block.Data, MimeType: block.MimeType})
		default:
			s := c.redact(block.Text)
			if text+len(s) > maxMCPOutput {
				s = cut(s, max(0, maxMCPOutput-text)) + "\n[pocketd cut the rest: the result is longer than " + fmt.Sprint(maxMCPOutput) + " bytes]"
			}
			text += len(s)
			out = append(out, ai.TextContent{Text: s})
		}
		if text >= maxMCPOutput {
			break
		}
	}
	isError := result.IsError != nil && *result.IsError
	if len(out) == 0 {
		if isError {
			out = append(out, ai.TextContent{Text: "MCP server " + c.server.Name + " says the call failed."})
		} else {
			out = append(out, ai.TextContent{Text: "(no content)"})
		}
	}
	r := durable.ToolExecutionResult{Output: out}
	if isError {
		r.IsError = &isError
	}
	return r
}

// refreshOnly is the OAuth sign-in a serving pocketd connects with: it
// refreshes the tokens, under PiG's flow, and does nothing that needs the
// owner. It registers no client (it saves none), starts no authorization
// (it keeps no verifier), and drops no credential. An authorization
// server that refuses the refresh is PiG's OAuthError, and the owner signs
// in again with pocketd mcp login; a refresh that failed otherwise reaches
// the authorization PiG's flow starts next, which is errMCPRefresh.
type refreshOnly struct{ p *oauth.McpOAuthProvider }

func (r refreshOnly) RedirectURL() string                       { return r.p.RedirectURL() }
func (r refreshOnly) ClientMetadata() oauth.OAuthClientMetadata { return r.p.ClientMetadata() }
func (r refreshOnly) ClientInformation(ctx context.Context) (*oauth.OAuthClientInformationMixed, error) {
	info, err := r.p.ClientInformation(ctx)
	if err == nil && info == nil {
		return nil, errMCPSignIn
	}
	return info, err
}
func (r refreshOnly) Tokens(ctx context.Context) (*oauth.OAuthTokens, error) { return r.p.Tokens(ctx) }
func (r refreshOnly) SaveTokens(ctx context.Context, tokens oauth.OAuthTokens) error {
	return r.p.SaveTokens(ctx, tokens)
}
func (r refreshOnly) RedirectToAuthorization(context.Context, oauth.URL) error { return errMCPRefresh }
func (r refreshOnly) SaveCodeVerifier(context.Context, string) error           { return errMCPRefresh }
func (r refreshOnly) CodeVerifier(context.Context) (string, error)             { return "", errMCPRefresh }
func (r refreshOnly) SaveDiscoveryState(ctx context.Context, state oauth.OAuthDiscoveryState) error {
	return r.p.SaveDiscoveryState(ctx, state)
}
func (r refreshOnly) DiscoveryState(ctx context.Context) (*oauth.OAuthDiscoveryState, error) {
	return r.p.DiscoveryState(ctx)
}

// MCPProfiles are the profiles with an MCP server pocketd connects, as it
// read them at start, sorted (ADR-0063).
func (e *Engine) MCPProfiles() []string { return e.mcp.profiles() }

// ConnectMCP connects every MCP server a profile names, or reads its tools
// again, and returns once each answered or failed (ADR-0063): a server
// that is down is logged, and keeps the tools it listed last.
func (e *Engine) ConnectMCP(ctx context.Context) { e.mcp.connectAll(ctx) }

// SetMCPTools gives session id the MCP tools of profile's servers as
// pocketd holds them now, after its other tools, in its profile's
// extension, and says whether it changed anything (ADR-0063). A session
// that is offered every tool (no list) gets the extension alone, whose
// tools it is then offered. Its caller decides which sessions take them
// (team.MayUseMCP).
func (e *Engine) SetMCPTools(ctx context.Context, id int64, profile string) (bool, error) {
	ext := e.mcp.extension(profile)
	if ext == nil {
		return false, nil
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		return false, err
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		return false, err
	}
	stored, err := durable.Snapshot[harness.AgentState](ctx, e.harness, harness.AgentDoc, durable.ConversationId(id))
	if err != nil {
		return false, err
	}
	extensions := slices.DeleteFunc(slices.Clone(agent.Extensions), func(x *durable.Extension) bool { return isMCPExtension(x.Name) })
	extensions = append(extensions, ext)
	change, changed := harness.AgentChange{}, false
	if !slices.Equal(extensionNames(agent.Extensions), extensionNames(extensions)) {
		change.Extensions, changed = harness.SetTo(harness.ExtensionChange{List: extensions}), true
	}
	if stored.Tools != nil && stored.Tools.Exact {
		tools := slices.DeleteFunc(slices.Clone(agent.Tools), func(t *durable.ToolRegistration) bool { return strings.HasPrefix(t.Name, mcpToolPrefix) })
		tools = append(tools, ext.Tools...)
		if !slices.Equal(stored.Tools.Names, toolNames(tools)) {
			change.Tools, changed = harness.SetTo(harness.ToolChange{List: tools}), true
		}
	}
	if !changed {
		return false, nil
	}
	return true, conversation.Configure(ctx, change)
}
