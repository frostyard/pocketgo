package pig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/uploads"
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
	// maxMCPTools is the tools pocketd takes of one server at most, and
	// maxProfileMCPTools of all a profile's servers, in the order of its
	// mcp list: the rest of a session's tools fit beside them in
	// maxSessionTools.
	maxMCPTools        = 128
	maxProfileMCPTools = 96
	// maxSessionTools is the tools a session is offered at most: the
	// fewest a model provider pocketd runs takes (OpenAI's Chat
	// Completions, 128 functions). A session's MCP tools give way to its
	// own.
	maxSessionTools = 128
	// maxMCPDescription bounds a tool's description, in bytes.
	maxMCPDescription = 2048
	// maxMCPSchema bounds a tool's input schema, in bytes: a larger one
	// is offered as an object of any properties.
	maxMCPSchema = 32 << 10
	// maxMCPOutput bounds the text of a call's result, in bytes, the
	// notices pocketd adds to it included.
	maxMCPOutput = 64 << 10
	// maxMCPImages bounds the images of a call's result, and
	// maxMCPImageBytes each one's data.
	maxMCPImages     = 4
	maxMCPImageBytes = 4 << 20
	// maxMCPServerText bounds what a server's own error says, in bytes.
	maxMCPServerText = 1000
	// maxMCPMessage bounds one message a server sends, in bytes, and
	// maxMCPListPages the pages of its tool list pocketd reads: so a list,
	// whatever its length, holds at most their product in memory.
	maxMCPMessage   = 8 << 20
	maxMCPListPages = 4
	// mcpLoginTimeout bounds each request of pocketd mcp login.
	mcpLoginTimeout = 30 * time.Second
	// The waits before pocketd tries again to connect a server that failed
	// at start: mcpRetryFirst, doubled each time, up to mcpRetryMost.
	mcpRetryFirst = time.Minute
	mcpRetryMost  = 30 * time.Minute
)

// mcpTransport carries the MCP servers' requests; nil is directTransport.
// Tests set it, so none reaches a network.
var mcpTransport http.RoundTripper

// mcpAfter runs f once after d, unless the stop it returns is called
// first: the retries of a server that failed at start. Tests replace it,
// so none waits on the clock.
var mcpAfter = defaultMCPAfter

// defaultMCPAfter is mcpAfter on the clock.
func defaultMCPAfter(d time.Duration, f func()) (stop func() bool) { return time.AfterFunc(d, f).Stop }

// mcpClient is the HTTP client of the MCP servers' requests, their OAuth
// ones included: on directTransport, no proxy whatever HTTP_PROXY says, a
// redirect followed only to the origin it came from, so a stored header
// never reaches a host mcp.json does not name, and every exchange bounded
// by timeout, its body included: PiG sends a notification and refreshes a
// sign-in with no deadline of their own. Every answer but an MCP
// message's, which PiG bounds by message (maxMCPMessage), is bounded too
// (maxMCPDocument): PiG reads an OAuth document whole.
func mcpClient(timeout time.Duration) *http.Client {
	var transport http.RoundTripper = mcpDirectTransport
	if mcpTransport != nil {
		transport = mcpTransport
	}
	transport = boundedTransport{next: transport}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// The origin, not its spelling: a host's case and a scheme's
		// default port name the same one.
		if !proto.SameOrigin(via[0].URL, req.URL) {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}}
}

// maxMCPDocument bounds an answer pocketd takes from an MCP server's
// OAuth side, in bytes: discovery, registration and tokens.
const maxMCPDocument = 1 << 20

// errMCPTooLarge is an answer past maxMCPDocument.
var errMCPTooLarge = fmt.Errorf("an answer is larger than %d bytes", maxMCPDocument)

// errMCPInsecure is a request pocketd does not send: plain HTTP to a host
// that is neither loopback nor a tailnet name (profiles.MCPTransportAllowed).
var errMCPInsecure = errors.New("names an address on plain HTTP whose host is neither loopback nor a tailnet name, which pocketd sends nothing to")

// boundedTransport sends no request profiles.MCPTransportAllowed refuses,
// and bounds the body of every answer but an MCP message's, known by what
// the request takes: PiG's MCP transport, and pocketd's probe, accept an
// event stream, and no OAuth request of PiG's does, whatever URL either
// names.
type boundedTransport struct {
	next http.RoundTripper
}

func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// No exchange on plain HTTP but with a loopback host or a tailnet
	// name, whatever discovery or a server named: a sign-in never moves
	// to a channel anyone may read.
	if !profiles.MCPTransportAllowed(r.URL) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, errMCPInsecure
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.Body == nil || mcpMessage(r) {
		return resp, err
	}
	resp.Body = &boundedBody{body: resp.Body, left: maxMCPDocument}
	return resp, nil
}

// mcpMessage says whether r is an MCP message, whose answer PiG bounds by
// message (maxMCPMessage): one that accepts an event stream.
func mcpMessage(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(accept, "text/event-stream") {
			return true
		}
	}
	return false
}

// boundedBody fails a read once more than left bytes came: a document cut
// short would parse as another.
type boundedBody struct {
	body io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return 0, errMCPTooLarge
	}
	return n, err
}

func (b *boundedBody) Close() error { return b.body.Close() }

// mcpDirectTransport is directTransport with no wait of its own for an
// answer's headers: an MCP server's timeout, up to MaxMCPTimeout, bounds
// each exchange whole (mcpClient), longer than a model provider's idle
// limit.
var mcpDirectTransport = func() *http.Transport {
	t := directTransport.Clone()
	t.ResponseHeaderTimeout = 0
	return t
}()

// mcpExtensionName is the durable extension of profile's MCP tools.
func mcpExtensionName(profile string) string { return "pocketgo-mcp-" + profile }

// isMCPExtension says whether an extension holds a profile's MCP tools.
func isMCPExtension(name string) bool { return strings.HasPrefix(name, "pocketgo-mcp-") }

// mcpPlainName is a server's tool as PiG names it: mcp__<server>__<tool>,
// every character but a letter, a digit or _ made _.
func mcpPlainName(server, tool string) string {
	return mcpToolPrefix + profiles.MCPNamespace(server) + "__" + profiles.MCPNamespace(tool)
}

// mcpDigestName is a server's tool named by its plain name, cut, then __
// and a digest of the server's and the tool's names, 16 hexadecimal
// digits of SHA-256 (mcpDigest): the same for the same tool, whatever else
// the servers list. It ends with __ and the digest, which no plain name
// does (a plain name's tool part holds no __), so no tool's plain name is
// another's digest name.
func mcpDigestName(server, tool string) string {
	name := mcpPlainName(server, tool)
	digest := mcpDigest(server, tool)
	return name[:min(len(name), maxMCPToolName-len(digest)-2)] + "__" + digest
}

// mcpDigest is the digest of a server's tool in its digest name (tests
// replace it, to make two alike).
var mcpDigest = func(server, tool string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	return hex.EncodeToString(sum[:])[:16]
}

// delimited says whether a part of a plain tool name leaves the __
// between the server and the tool the first __ of the name: no __ in it,
// and no _ at either end.
func delimited(part string) bool {
	return part != "" && !strings.Contains(part, "__") && !strings.HasPrefix(part, "_") && !strings.HasSuffix(part, "_")
}

// mcpToolNames are the names a profile's tools are offered as, in order,
// by server and tool: a tool's plain name when it is at most 64
// characters, the server's part is delimited, and the tool's name reads
// in it as itself (letters, digits and _ alone, delimited), else its
// digest name. Either follows from the server's and the tool's names
// alone, and no two tools share one, in any listing: mcp.json gives no two
// servers parts alike (profiles.ParseMCP), a plain name reads back to
// exactly one server and tool, and a digest name is of the two names as
// they are. So a name never moves to another tool, whatever a listing's
// order or what a later listing holds: find.note and find_note never
// share one. A name two tools would share, which a digest's collision
// alone could make, is "" for both, whatever their order: neither is
// offered, so the name never reaches one tool in one listing and the
// other in the next.
func mcpToolNames(servers []string, tools map[string][]mcpTool) map[string][]string {
	out := map[string][]string{}
	count := map[string]int{}
	for _, server := range servers {
		for _, tool := range tools[server] {
			name := mcpPlainName(server, tool.Name)
			if !delimited(profiles.MCPNamespace(server)) || profiles.MCPNamespace(tool.Name) != tool.Name || !delimited(tool.Name) ||
				len(name) > maxMCPToolName {
				name = mcpDigestName(server, tool.Name)
			}
			count[name]++
			out[server] = append(out[server], name)
		}
	}
	for _, names := range out {
		for i, name := range names {
			if count[name] > 1 {
				names[i] = ""
			}
		}
	}
	return out
}

// mcpHub holds the MCP servers the profiles name, as pocketd read them at
// start: mcp.json and each profile's mcp list. A change to either applies
// when pocketd starts again.
type mcpHub struct {
	store    *mcpStore
	registry harness.Registry
	log      *slog.Logger
	// changed is told each profile whose tools changed (Options.OnMCPTools).
	changed func(profile string)
	// conns are the servers some profile names, by name.
	conns map[string]*mcpConn
	// servers are each profile's servers that mcp.json has, in the
	// profile's order.
	servers map[string][]string
	// ctx ends with the hub: the retries' connections run under it.
	ctx    context.Context
	cancel context.CancelFunc
	// mu guards tools, exts and retries.
	mu sync.Mutex
	// tools are each server's tools, as it listed them last.
	tools map[string][]mcpTool
	// exts are each profile's extension, as last installed.
	exts map[string]*durable.Extension
	// retries stop each retry that waits, by server.
	retries map[string]func() bool
	closed  bool
	// unkept are why each server's last listing is not kept, by server.
	unkept map[string]error
	// work are the exchanges under way, connections being made and calls,
	// which close waits for: begin adds one, under mu, unless closed.
	work sync.WaitGroup
}

// mcpConn is one server's connection: made when needed, and made again
// after a failure.
type mcpConn struct {
	hub    *mcpHub
	server profiles.MCPServer
	// mu guards client, attempt and state; it is taken before hub.mu,
	// never after, and held across no exchange with the server.
	mu      sync.Mutex
	client  *mcp.Client
	attempt *mcpAttempt
	state   string
	// authMu guards flight, the OAuth refresh under way (serveAuth), the
	// server's whatever connection asked for it.
	authMu sync.Mutex
	flight *authFlight
}

// newMCPHub is the hub of servers, as mcp.json has them, for the profiles'
// mcp lists (profile id to server names), with the tools each listed last
// from store, its profiles' extensions installed in registry. changed is
// told each profile whose tools change from then on. A kept listing that
// holds a secret, or that pocketd cannot offer, offers what it can: it
// never keeps pocketd from starting.
func newMCPHub(store *mcpStore, registry harness.Registry, servers profiles.MCPServers, lists map[string][]string, log *slog.Logger, changed func(string)) *mcpHub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &mcpHub{store: store, registry: registry, log: log, changed: changed, conns: map[string]*mcpConn{}, servers: map[string][]string{},
		ctx: ctx, cancel: cancel, tools: map[string][]mcpTool{}, exts: map[string]*durable.Extension{}, retries: map[string]func() bool{}}
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
					h.tools[name] = h.sanitize(name, tools)
				}
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for profile := range h.servers {
		if err := h.installLocked(profile); err != nil {
			h.log.Warn("offer an MCP server's kept tools", "profile", profile, "err", err)
			for _, name := range h.servers[profile] {
				delete(h.tools, name)
			}
			if err := h.installLocked(profile); err != nil {
				h.log.Warn("offer no MCP tool", "profile", profile, "err", err)
			}
		}
	}
	return h
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

// installLocked builds profile's extension from its servers' tools and
// installs it in place of the one installed; mu is held.
func (h *mcpHub) installLocked(profile string) error {
	ext := &durable.Extension{Name: mcpExtensionName(profile), Tools: []*durable.ToolRegistration{}}
	names := mcpToolNames(h.servers[profile], h.tools)
	for _, server := range h.servers[profile] {
		c := h.conns[server]
		for i, tool := range h.tools[server] {
			switch {
			case names[server][i] == "":
				h.log.Warn("an MCP tool is not offered: its name is another's", "server", server)
				continue
			case len(ext.Tools) == maxProfileMCPTools:
				h.log.Warn("an MCP tool is not offered: the profile's servers list more than a session takes", "server", server, "most", maxProfileMCPTools)
				continue
			}
			ext.Tools = append(ext.Tools, c.registration(names[server][i], tool))
		}
	}
	if err := h.registry.Install(ext); err != nil {
		return fmt.Errorf("install the MCP tools of %s: %w", profile, err)
	}
	h.exts[profile] = ext
	return nil
}

// sanitize is tools, as server listed them, with no secret the store
// keeps in them, each schema bounded and an object, and no name listed
// twice: a tool whose name holds a secret is left out, a description has
// each one in its place, and a schema that holds one is offered as an
// object of any properties. A server may echo what it was sent.
func (h *mcpHub) sanitize(server string, tools []mcpTool) []mcpTool {
	secrets := h.store.secrets()
	out := []mcpTool{}
	seen := map[string]bool{}
	for _, t := range tools {
		if t.Name == "" || seen[t.Name] || len(out) == maxMCPTools || holdsSecret(t.Name, secrets) {
			continue
		}
		seen[t.Name] = true
		// A description that is JSON is redacted decoded, as a result's
		// text is: an encoder's escapes may spell a secret otherwise.
		t.Description = cut(redactText(t.Description, secrets), maxMCPDescription)
		t.InputSchema = normalSchema(t.InputSchema)
		if holdsSecret(string(t.InputSchema), secrets) || decodedHoldsSecret(t.InputSchema, secrets) {
			h.log.Warn("an MCP tool's schema holds a secret pocketd keeps: it is offered with none", "server", server)
			t.InputSchema = json.RawMessage(emptySchema)
		}
		out = append(out, t)
	}
	return out
}

// listed takes tools as what server name listed last. When they changed,
// it installs again the extension of every profile that names the server,
// keeps them, and tells changed of each profile; a listing an extension
// cannot take is not kept, and the last one stays.
func (h *mcpHub) listed(name string, tools []mcpTool) {
	tools = h.sanitize(name, tools)
	var profiles []string
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return
		}
		old, had := h.tools[name]
		if had && mcpToolsEqual(old, tools) {
			// The same tools: kept, or kept now, if a write failed before.
			if h.unkept[name] != nil {
				h.keepLocked(name, tools)
			}
			return
		}
		h.tools[name] = tools
		for profile, names := range h.servers {
			if slices.Contains(names, name) {
				profiles = append(profiles, profile)
			}
		}
		for _, profile := range profiles {
			if err := h.installLocked(profile); err != nil {
				h.log.Warn("an MCP server's new tools are not offered: it keeps the ones it listed before", "server", name, "err", err)
				if had {
					h.tools[name] = old
				} else {
					delete(h.tools, name)
				}
				for _, p := range profiles {
					_ = h.installLocked(p)
				}
				profiles = nil
				return
			}
		}
		h.keepLocked(name, tools)
	}()
	if h.changed != nil {
		for _, profile := range profiles {
			h.changed(profile)
		}
	}
}

// keepLocked writes tools as what server name listed last, and notes a
// write that failed (unkept), which the next listing tries again; mu is
// held.
func (h *mcpHub) keepLocked(name string, tools []mcpTool) {
	if h.unkept == nil {
		h.unkept = map[string]error{}
	}
	if err := h.store.setListing(name, h.conns[name].server.URL, tools); err != nil {
		h.unkept[name] = err
		h.log.Warn("keep an MCP server's tools", "server", name, "err", err)
		return
	}
	delete(h.unkept, name)
}

// begin counts an exchange in work, unless the hub closed: then it
// starts none.
func (h *mcpHub) begin() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.work.Add(1)
	return true
}

// isClosed says whether close ran.
func (h *mcpHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// keptErr is why the tools server name listed last are not kept, nil
// when they are.
func (h *mcpHub) keptErr(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unkept[name]
}

func mcpToolsEqual(a, b []mcpTool) bool {
	return slices.EqualFunc(a, b, func(x, y mcpTool) bool {
		return x.Name == y.Name && x.Description == y.Description && string(x.InputSchema) == string(y.InputSchema)
	})
}

// connectAll connects every server, or reads its tools again, at once,
// and returns when each has answered or failed. A server that failed for
// a cause that may pass is tried again later (retryLater).
func (h *mcpHub) connectAll(ctx context.Context) {
	if h == nil {
		return
	}
	var wg sync.WaitGroup
	for _, c := range h.conns {
		wg.Go(func() {
			if _, err := c.connect(ctx, true); err != nil {
				c.note(err)
				if ctx.Err() == nil && mayPass(err) {
					h.retryLater(c, mcpRetryFirst)
				}
			}
		})
	}
	wg.Wait()
}

// mayPass says whether a connection's failure may pass with no step of
// the owner's: not a header or a sign-in pocketd lacks, nor a stored
// header or a sign-in the server refused (a 401 after any refresh).
func mayPass(err error) bool {
	var oauthErr *oauth.OAuthError
	var auth *mcp.McpAuthRequiredError
	if errors.As(err, &oauthErr) {
		return oauthTransient(oauthErr)
	}
	return !errors.Is(err, errMCPNeedsHeader) && !errors.Is(err, errMCPNotSignedIn) && !errors.Is(err, errMCPSignIn) &&
		!errors.As(err, &auth) && !errors.Is(err, errMCPScope) && !errors.Is(err, errMCPInsecure)
}

// oauthTransient says whether an authorization server's refusal says it
// cannot answer now (RFC 6749's temporarily_unavailable and
// server_error), rather than that it refuses pocketd's sign-in.
func oauthTransient(err *oauth.OAuthError) bool {
	return err.Code == "temporarily_unavailable" || err.Code == "server_error"
}

// retryLater connects c again after wait, and again after twice the wait,
// up to mcpRetryMost, until it connects or fails for a cause that does not
// pass. A server that never listed a tool has none to call, so this is how
// it comes to offer its tools with no restart; once it lists them, changed
// is told, and the sessions get them.
func (h *mcpHub) retryLater(c *mcpConn, wait time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if stop, ok := h.retries[c.server.Name]; ok {
		stop()
	}
	h.retries[c.server.Name] = mcpAfter(wait, func() {
		h.mu.Lock()
		delete(h.retries, c.server.Name)
		closed := h.closed
		h.mu.Unlock()
		if closed {
			return
		}
		if _, err := c.connect(h.ctx, true); err != nil {
			c.note(err)
			if h.ctx.Err() == nil && mayPass(err) {
				h.retryLater(c, min(2*wait, mcpRetryMost))
			}
		}
	})
}

// close ends every retry, every dial under way and every connection, and
// returns once each ended or ctx did. From then on the hub keeps no
// listing and takes no connection: pocketd's lock on the data folder may
// be let go.
func (h *mcpHub) close(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closed = true
	for name, stop := range h.retries {
		stop()
		delete(h.retries, name)
	}
	h.mu.Unlock()
	h.cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The exchanges under way end first, the dials with the hub's
		// context: they take no connection and write no listing once it
		// closed.
		h.work.Wait()
		var wg sync.WaitGroup
		for _, c := range h.conns {
			c.mu.Lock()
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
	// Whatever is still under way past the deadline writes nothing.
	h.store.fence()
}

// registration is server tool offered as name in a profile's extension.
// A call is not replayed after a restart: what it did on the server is not
// known, and may have happened.
func (c *mcpConn) registration(name string, tool mcpTool) *durable.ToolRegistration {
	description := tool.Description
	if description == "" {
		description = cut("MCP tool "+tool.Name+" of the server "+c.server.Name+".", maxMCPDescription)
	}
	return &durable.ToolRegistration{
		ToolSchema: mcpSchema(name, description, tool.InputSchema),
		Replay:     durable.ReplayUnsafe,
		// The result is bounded already (result), its notices within the
		// bound: PiG's default limits, 50 KiB and 2,000 lines, would cut
		// its tail, notices and all.
		OutputLimits: &durable.ToolOutputLimits{MaxBytes: ptr(maxMCPOutput), MaxLines: ptr(maxMCPOutput + 1)},
		Execute: func(ctx context.Context, args any, _ durable.ToolExecutionApi) (durable.ToolExecutionResult, error) {
			return c.call(ctx, tool.Name, args), nil
		},
	}
}

// emptySchema is the input schema of a tool whose own pocketd cannot
// take: an object of any properties.
const emptySchema = `{"type":"object","properties":{}}`

// normalSchema is a server's input schema as pocketd keeps and offers
// it: an object schema with a type and properties, as a model provider
// wants one, at most maxMCPSchema bytes; any other is emptySchema. A
// schema of another type, or whose properties are no object, would make
// a provider refuse the whole request.
func normalSchema(input json.RawMessage) json.RawMessage {
	// Measured compact: a kept listing's indentation is no part of it.
	var compact bytes.Buffer
	if len(input) == 0 || json.Compact(&compact, input) != nil || compact.Len() > maxMCPSchema {
		return json.RawMessage(emptySchema)
	}
	input = compact.Bytes()
	var object map[string]json.RawMessage
	if json.Unmarshal(input, &object) != nil || object == nil {
		return json.RawMessage(emptySchema)
	}
	switch t, ok := object["type"]; {
	case !ok || string(t) == "null":
		object["type"] = json.RawMessage(`"object"`)
	case string(t) != `"object"`:
		return json.RawMessage(emptySchema)
	}
	if properties, ok := object["properties"]; !ok || string(properties) == "null" {
		object["properties"] = json.RawMessage(`{}`)
	} else {
		var fields map[string]json.RawMessage
		if json.Unmarshal(properties, &fields) != nil || fields == nil {
			return json.RawMessage(emptySchema)
		}
	}
	data, err := json.Marshal(object)
	if err != nil || len(data) > maxMCPSchema {
		return json.RawMessage(emptySchema)
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil || !validSchema(decoded) {
		return json.RawMessage(emptySchema)
	}
	return data
}

// maxSchemaDepth bounds how deep validSchema reads a schema.
const maxSchemaDepth = 32

// validSchema says whether a decoded JSON Schema holds a schema wherever
// its keywords take one, as a model provider checks it: an object or a
// boolean under properties, items and the like, lists of them under
// allOf and the like, a type that is a name or a list of names, and a
// required list of names. Any other keyword is the server's. Each $ref in
// a schema's place names a place of the schema itself that is read as a
// schema (#, #/$defs/x): a reference to a default, an example, a
// property's name or a type would have a provider refuse the whole
// request. A recursive reference is checked as the place it names, never
// followed.
func validSchema(value any) bool {
	w := &schemaWalk{at: map[string]bool{}}
	if !w.valid(value, "", 0) {
		return false
	}
	for _, ref := range w.refs {
		pointer, ok := refPointer(ref)
		if !ok || !w.at[pointer] {
			return false
		}
	}
	return true
}

// schemaWalk is one read of a schema by validSchema: at holds the JSON
// pointer (RFC 6901) of every place read as a schema, and refs every
// $ref read in one.
type schemaWalk struct {
	at   map[string]bool
	refs []string
}

// childPointer is the JSON pointer of key under the place at path.
func childPointer(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// refPointer is the JSON pointer a reference into the schema itself
// names, written as childPointer writes one: # is the schema, and #/a/b
// its place a, then b, percent-decoded as a URI's fragment is.
func refPointer(ref string) (string, bool) {
	fragment, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return "", false
	}
	fragment, err := url.PathUnescape(fragment)
	if err != nil {
		return "", false
	}
	if fragment == "" {
		return "", true
	}
	parts, ok := strings.CutPrefix(fragment, "/")
	if !ok {
		return "", false
	}
	pointer := ""
	for _, part := range strings.Split(parts, "/") {
		pointer = childPointer(pointer, strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~"))
	}
	return pointer, true
}

// valid reads value, at path, as a schema (validSchema).
func (w *schemaWalk) valid(value any, path string, depth int) bool {
	if depth > maxSchemaDepth {
		return false
	}
	if _, ok := value.(bool); ok {
		w.at[path] = true
		return true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	w.at[path] = true
	for key, v := range object {
		at := childPointer(path, key)
		switch key {
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
			fields, ok := v.(map[string]any)
			if !ok {
				return false
			}
			for name, field := range fields {
				if !w.valid(field, childPointer(at, name), depth+1) {
					return false
				}
			}
		case "items":
			if list, ok := v.([]any); ok {
				for i, item := range list {
					if !w.valid(item, childPointer(at, strconv.Itoa(i)), depth+1) {
						return false
					}
				}
			} else if !w.valid(v, at, depth+1) {
				return false
			}
		case "additionalProperties", "additionalItems", "not", "contains", "propertyNames", "if", "then", "else",
			"unevaluatedProperties", "unevaluatedItems":
			if !w.valid(v, at, depth+1) {
				return false
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return false
			}
			for i, item := range list {
				if !w.valid(item, childPointer(at, strconv.Itoa(i)), depth+1) {
					return false
				}
			}
		case "type":
			if !schemaType(v) {
				return false
			}
		case "required":
			// Names, each once, as JSON Schema wants them.
			list, ok := v.([]any)
			if !ok {
				return false
			}
			names := map[string]bool{}
			for _, item := range list {
				name, ok := item.(string)
				if !ok || names[name] {
					return false
				}
				names[name] = true
			}
		case "enum", "examples":
			if _, ok := v.([]any); !ok {
				return false
			}
		case "$ref":
			// A reference into the schema itself: no provider fetches
			// another.
			ref, ok := v.(string)
			if !ok || !strings.HasPrefix(ref, "#") {
				return false
			}
			w.refs = append(w.refs, ref)
		case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "minContains", "maxContains":
			n, ok := v.(float64)
			if !ok || n < 0 || n != float64(int64(n)) {
				return false
			}
		case "minimum", "maximum":
			if _, ok := v.(float64); !ok {
				return false
			}
		case "exclusiveMinimum", "exclusiveMaximum":
			// A number, or draft 4's true or false.
			_, number := v.(float64)
			_, flag := v.(bool)
			if !number && !flag {
				return false
			}
		case "multipleOf":
			if n, ok := v.(float64); !ok || n <= 0 {
				return false
			}
		case "pattern", "format", "title", "description", "$id", "$schema", "$comment", "$anchor", "contentEncoding", "contentMediaType":
			if _, ok := v.(string); !ok {
				return false
			}
		case "uniqueItems", "readOnly", "writeOnly", "deprecated", "nullable":
			if _, ok := v.(bool); !ok {
				return false
			}
		}
	}
	return true
}

// schemaTypes are JSON Schema's type names.
var schemaTypes = []string{"object", "array", "string", "number", "integer", "boolean", "null"}

// schemaType says whether a schema's type is a type name or a list of
// them.
func schemaType(v any) bool {
	if name, ok := v.(string); ok {
		return slices.Contains(schemaTypes, name)
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	// Each name once, as JSON Schema wants a list of them.
	seen := map[string]bool{}
	for _, item := range list {
		name, ok := item.(string)
		if !ok || !slices.Contains(schemaTypes, name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

// mcpSchema is a tool's schema from its input schema, as normalSchema
// keeps it.
func mcpSchema(name, description string, input json.RawMessage) ai.ToolSchema {
	encoded, err := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}{name, description, normalSchema(input)})
	var out ai.ToolSchema
	if err != nil || json.Unmarshal(encoded, &out) != nil {
		return schema(name, description, emptySchema)
	}
	return out
}

// toolsOf are the tools of a server's list as pocketd takes them, before
// sanitize: a name, a description (its title when it has none) and its
// input schema.
func toolsOf(listed []mcp.Tool) []mcpTool {
	out := []mcpTool{}
	for _, t := range listed {
		description := strings.TrimSpace(t.Description)
		if description == "" {
			description = strings.TrimSpace(t.Title)
		}
		out = append(out, mcpTool{Name: t.Name, Description: description, InputSchema: t.InputSchema})
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
	// errMCPNoRefresh is a sign-in the server refused that holds no
	// refresh token: the owner signs in again (it is an errMCPSignIn).
	errMCPNoRefresh = fmt.Errorf("%w: its sign-in holds no refresh token", errMCPSignIn)
	// errMCPRefresh is a sign-in PiG's flow could not refresh, for no
	// refusal of the authorization server's: it did not answer, or
	// answered server_error. PiG's flow then starts an authorization,
	// which refreshOnly refuses with it.
	errMCPRefresh = errors.New("could not refresh its sign-in")
	// errMCPScope is a call the server refused for a scope pocketd's
	// sign-in lacks (403 insufficient_scope): a step up needs the owner.
	errMCPScope = errors.New("needs a scope its sign-in lacks")
	// errMCPClosed is a connection made as pocketd closes.
	errMCPClosed = errors.New("was connected as pocketd closed")
	// errMCPList is a tool list that is no list.
	errMCPList = errors.New("answered its tool list with no list")
)

// mcpAttempt is one attempt to connect a server, which every caller that
// needs the connection meanwhile waits for, each until its own context
// ends: a caller that stops waiting stops none of the others.
type mcpAttempt struct {
	done   chan struct{}
	client *mcp.Client
	err    error
}

// connect is the server's connection, made when there is none; list reads
// its tools again on one there is. A connection being made is waited for,
// until ctx ends, not made twice; it is made under the hub's context, so a
// caller that stops waiting stops it for nobody. It needs every header
// mcp.json names stored for the server's URL, and its sign-in when it
// signs in, else it tries nothing.
func (c *mcpConn) connect(ctx context.Context, list bool) (*mcp.Client, error) {
	c.mu.Lock()
	if c.client != nil && c.client.ConnectionState() == mcp.ClientStateConnected {
		client := c.client
		c.mu.Unlock()
		if list {
			unpin := c.hub.store.pin()
			defer unpin()
			// A listing that failed for itself alone keeps the connection,
			// which calls of every session may be using.
			if err := c.list(ctx, client); err != nil {
				if !requestLocal(ctx, err) {
					c.drop(client)
				}
				return nil, err
			}
		}
		return client, nil
	}
	attempt := c.attempt
	if attempt == nil {
		if !c.hub.begin() {
			c.mu.Unlock()
			return nil, errMCPClosed
		}
		attempt = &mcpAttempt{done: make(chan struct{})}
		c.attempt = attempt
		if c.client != nil {
			_ = c.client.Close()
			c.client = nil
		}
		go c.dial(attempt)
	}
	c.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.client, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// dial makes attempt: the connection, and the listing of the server's
// tools, each request within the server's timeout, under the hub's
// context (setupContext).
func (c *mcpConn) dial(attempt *mcpAttempt) {
	defer c.hub.work.Done()
	ctx, cancel := c.setupContext()
	defer cancel()
	unpin := c.hub.store.pin()
	defer unpin()
	client, err := c.open(ctx)
	if err == nil {
		if err = c.list(ctx, client); err != nil {
			_ = client.Close()
			client = nil
		}
	}
	// A hub that closed meanwhile takes no connection: it would outlive
	// pocketd's close.
	if err == nil && c.hub.isClosed() {
		_ = client.Close()
		client, err = nil, errMCPClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempt = nil
	attempt.client, attempt.err = client, err
	if err == nil {
		c.client = client
		c.setStateLocked("connected", nil)
	}
	close(attempt.done)
}

// setupContext is the context a connection is made under: the hub's,
// which its close ends, with no deadline of its own. Each request of it
// is bounded by the server's timeout alone, as the client's request
// timeout and mcpClient bound it: one deadline for the initialize, a
// refresh and every page would leave a server that answers each in time
// never connected.
func (c *mcpConn) setupContext() (context.Context, context.CancelFunc) {
	parent := c.hub.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithCancel(parent)
}

// open connects to the server with what the store keeps for it.
func (c *mcpConn) open(ctx context.Context) (*mcp.Client, error) {
	store, server := c.hub.store, c.server
	headers, missing := store.headers(server.Name, server.URL, server.Headers)
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s (pocketd mcp set-header %s %s)", errMCPNeedsHeader, missing[0], server.Name, missing[0])
	}
	options := mcp.StreamableHTTPTransportOptions{URL: server.URL, Headers: headers, Fetch: mcpClient(server.Timeout), OpenGetStream: new(false),
		MaxMessageBytes: maxMCPMessage}
	if server.OAuth != nil {
		if !store.signedIn(server.Name, server.URL) {
			return nil, errMCPNotSignedIn
		}
		provider, err := oauth.NewMcpOAuthProvider(oauth.McpOAuthProviderOptions{
			ServerURL: server.URL, RedirectURL: server.OAuth.CallbackURL, ClientID: server.OAuth.ClientID,
			ClientMetadata: oauth.OAuthClientMetadata{ClientName: mcpClientName, Scope: server.OAuth.Scope},
			Store:          mcpOAuthStore{store: store, name: server.Name, url: server.URL, log: c.hub.log},
		})
		if err != nil {
			return nil, err
		}
		options.AuthProvider = &serveAuth{conn: c, provider: refreshOnly{provider}}
	}
	transport, err := mcp.NewStreamableHTTPTransport(options)
	if err != nil {
		return nil, err
	}
	timeout := int(server.Timeout / time.Millisecond)
	client := mcp.NewClient(mcp.ClientOptions{Implementation: mcp.Implementation{Name: mcpClientName, Version: "1"}, RequestTimeoutMs: timeout})
	if _, err := client.Connect(ctx, transport); err != nil {
		return nil, err
	}
	return client, nil
}

// mcpClientName names pocketd to an MCP server, and in a dynamic client
// registration.
const mcpClientName = "pocketd"

// list reads the server's tools on client, a page at a time, and keeps no
// more than pocketd takes: at most maxMCPTools tools, from at most
// maxMCPListPages pages of at most maxMCPMessage bytes each. PiG's own
// ListTools reads every page, up to a thousand, before it returns.
func (c *mcpConn) list(ctx context.Context, client *mcp.Client) error {
	var listed []mcp.Tool
	var cursor *string
	for page := 0; page < maxMCPListPages && len(listed) < maxMCPTools; page++ {
		var params any
		if cursor != nil {
			params = map[string]string{"cursor": *cursor}
		}
		raw, err := client.Request(ctx, "tools/list", params, mcp.RequestOptions{TimeoutMs: int(c.server.Timeout / time.Millisecond)})
		if err != nil {
			return err
		}
		var result struct {
			Tools      *[]mcp.Tool `json:"tools"`
			NextCursor *string     `json:"nextCursor"`
		}
		// Every page names its tools, an empty list included: one that
		// names none is no list, and the last listing stays.
		if err := json.Unmarshal(raw, &result); err != nil || result.Tools == nil {
			return errMCPList
		}
		got := *result.Tools
		listed = append(listed, got[:min(len(got), maxMCPTools-len(listed))]...)
		if result.NextCursor == nil || *result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
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
// did not run it; any other failure is not, since it may have. The
// connection is every session's: a call stopped, or past its own timeout,
// ends that call alone, and only a failure of the connection drops it.
func (c *mcpConn) call(ctx context.Context, tool string, args any) durable.ToolExecutionResult {
	// The credentials this call goes out with stay redacted until its
	// answer is: a refresh may replace them while it waits.
	unpin := c.hub.store.pin()
	defer unpin()
	if !c.hub.begin() {
		return c.errorResult(errMCPClosed)
	}
	defer c.hub.work.Done()
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
			return toolText("", fmt.Errorf("MCP server %s refused the call: %s", c.server.Name, c.serverText(rpc.Message)))
		case requestLocal(ctx, err):
			return c.errorResult(err)
		}
		c.drop(client)
		c.note(err)
		return c.errorResult(err)
	}
}

// requestLocal says whether err ended one request alone, the connection
// holding: a request stopped or past its own timeout, refused by the
// server with an HTTP status of its own (a 401 included) or a JSON-RPC
// error, answered with no list, refused for a sign-in the next request
// tries again, or answered with what pocketd could not read (past
// maxMCPMessage, or no JSON-RPC message), which PiG says in an error of
// no type. Only a session the server ended, a connection PiG closed, or
// an exchange that failed itself (exchangeFailed) is the connection's,
// which every session of the server shares.
func requestLocal(ctx context.Context, err error) bool {
	var abort *mcp.McpAbortError
	var timeout *mcp.McpTimeoutError
	var status *mcp.McpHttpError
	var oauthErr *oauth.OAuthError
	var expired *mcp.McpSessionExpiredError
	var closed *mcp.McpConnectionClosedError
	if errors.As(err, &expired) || errors.As(err, &closed) {
		return false
	}
	var rpc *mcp.McpError
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		// The request's own deadline (the HTTP client's): the connection
		// may be well.
		return true
	}
	return ctx.Err() != nil || errors.As(err, &abort) || errors.As(err, &timeout) || errors.As(err, &status) ||
		errors.As(err, &oauthErr) || errors.Is(err, errMCPScope) || errors.Is(err, errMCPRefresh) || errors.Is(err, errMCPSignIn) ||
		errors.As(err, &rpc) || errors.Is(err, errMCPList) || !exchangeFailed(err) && !errors.Is(err, errMCPClosed)
}

// exchangeFailed says whether err is an HTTP exchange that failed itself,
// with no answer: the server not reached, or the connection broken, which
// net/http says in a url.Error or a net.OpError.
func exchangeFailed(err error) bool {
	var urlErr *url.Error
	var opErr *net.OpError
	return errors.As(err, &urlErr) || errors.As(err, &opErr)
}

// serverText is what a server said, with no secret in it, then cut to its
// bound: cut first, a secret could be left in part.
func (c *mcpConn) serverText(text string) string {
	return cut(redactText(text, c.hub.store.secrets()), maxMCPServerText)
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
	var abort *mcp.McpAbortError
	var rpc *mcp.McpError
	switch {
	case errors.Is(err, errMCPNeedsHeader):
		return "is not connected: it " + c.redact(err.Error())
	case errors.Is(err, errMCPNotSignedIn):
		return "is not connected: it is not signed in: the owner signs in with pocketd mcp login " + name
	case errors.As(err, &oauthErr) && oauthTransient(oauthErr):
		return "could not refresh pocketd's sign-in: its authorization server cannot answer now: try again later"
	case errors.Is(err, errMCPNoRefresh):
		return "refused pocketd's sign-in, which holds no refresh token: the owner signs in again with pocketd mcp login " + name
	case errors.Is(err, errMCPSignIn), errors.As(err, &oauthErr):
		return "refused pocketd's sign-in, and its refresh: the owner signs in again with pocketd mcp login " + name
	case errors.Is(err, errMCPRefresh):
		return "refused pocketd's sign-in, and pocketd could not refresh it now: try again later; if it goes on, the owner signs in again with pocketd mcp login " + name
	case errors.Is(err, errMCPScope):
		var scoped *mcpScopeError
		asks := ""
		if errors.As(err, &scoped) && scoped.scope != "" {
			asks = ", which asks for " + scoped.scope
		}
		return "refused the call for a scope pocketd's sign-in lacks (HTTP 403)" + asks +
			": the owner adds it to the server's oauth scope in mcp.json and signs in again with pocketd mcp login " + name
	case errors.Is(err, errMCPList):
		return errMCPList.Error() + ": try again later"
	case errors.Is(err, errMCPInsecure):
		return "was not reached: its sign-in " + errMCPInsecure.Error() + ": the owner fixes the server's OAuth addresses"
	case errors.As(err, &auth):
		if c.server.OAuth != nil {
			return "refused pocketd's sign-in (HTTP 401): the owner signs in again with pocketd mcp login " + name
		}
		return "refused pocketd's stored headers (HTTP 401): the owner stores them again with pocketd mcp set-header " + name + " HEADER"
	case errors.As(err, &status):
		return fmt.Sprintf("answered HTTP %d: try again later", status.Status)
	case errors.As(err, &timeout), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("did not answer within %s: it may be down; try again later", c.server.Timeout)
	case errors.Is(err, context.Canceled), errors.As(err, &abort):
		return "was not reached: the call was stopped"
	case errors.As(err, &rpc):
		return "refused the connection: " + c.serverText(rpc.Message)
	}
	var closed *mcp.McpConnectionClosedError
	if !exchangeFailed(err) && !errors.As(err, &closed) && !errors.Is(err, errMCPClosed) {
		return fmt.Sprintf("answered with nothing pocketd could read (an answer past %d bytes, or no JSON-RPC message): try again later", maxMCPMessage)
	}
	return "is unreachable: pocketd could not connect to it; it may be down: try again later"
}

// redact is text with every secret the store holds in its place: a
// server may echo what it was sent.
func (c *mcpConn) redact(text string) string { return redact(text, c.hub.store.secrets()) }

// redact is text with each of secrets, longest first, made [redacted]:
// as it is, and as JSON writes it in a string, which a text that holds
// JSON may.
func redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
		if quoted := jsonInString(secret); quoted != secret {
			text = strings.ReplaceAll(text, quoted, "[redacted]")
		}
	}
	return text
}

// jsonInString is s as JSON writes it inside a string.
func jsonInString(s string) string {
	quoted, _ := json.Marshal(s)
	return string(quoted[1 : len(quoted)-1])
}

// decodedHoldsSecret says whether JSON data holds one of secrets in a key
// or a string value once decoded: an escape (s) spells one in other
// bytes, which a model reads as the secret.
func decodedHoldsSecret(data json.RawMessage, secrets []string) bool {
	value, err := decodeJSON(data)
	if err != nil {
		return false
	}
	found := false
	walkJSON(value, func(s string) string {
		found = found || holdsSecret(s, secrets)
		return s
	})
	return found
}

// decodeJSON decodes data whole, its numbers as written (json.Number): a
// document written again changes its strings alone, never an id past a
// float's precision.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("text follows the JSON")
	}
	return value, nil
}

// walkJSON calls f with each key and string value of a decoded JSON
// value, and puts what f returns in its place.
func walkJSON(value any, f func(string) string) any {
	switch v := value.(type) {
	case string:
		return f(v)
	case []any:
		for i := range v {
			v[i] = walkJSON(v[i], f)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[f(key)] = walkJSON(item, f)
		}
		return out
	}
	return value
}

// redactJSON is JSON data with every secret redacted in each key and
// string value once decoded, encoded again: PiG renders a structured
// result as text with JSON's escapes, which a secret's own spelling
// would not match. Data that does not decode is none.
func redactJSON(data json.RawMessage, secrets []string) json.RawMessage {
	if len(data) == 0 {
		return data
	}
	value, err := decodeJSON(data)
	if err != nil {
		return nil
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(walkJSON(value, func(s string) string { return redact(s, secrets) })) != nil {
		return nil
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// redactText is a text block of a result with no secret in it: one that
// is JSON, as servers often send, an object, a list or a string alone, is
// redacted decoded, then written again compact (any encoder's escapes
// would hide a secret from its spelling), and any text literally, as it
// is and as JSON writes it.
func redactText(text string, secrets []string) string {
	if trimmed := strings.TrimSpace(text); trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') && json.Valid([]byte(trimmed)) {
		if out := redactJSON(json.RawMessage(trimmed), secrets); out != nil {
			text = string(out)
		}
	}
	return redact(text, secrets)
}

// holdsSecret says whether text holds one of secrets, as it is or as JSON
// writes it in a string.
func holdsSecret(text string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && (strings.Contains(text, secret) || strings.Contains(text, jsonInString(secret))) {
			return true
		}
	}
	return false
}

// result is a call's result for the model: its text and images, within
// their bounds, an error result when the server says the call failed, no
// secret in it. Its text blocks are one text, each after the last on a
// line of its own, the newline counted, an empty one left out: a
// provider joins blocks with newlines past any bound pocketd would count
// otherwise. The notices pocketd adds count in the text's bound: the mark
// of a cut text, and one notice of the images left out on a line of its
// own, both kept room for before any text. A structured result is
// redacted before PiG renders it as JSON, whose escapes a secret's
// spelling would not match.
func (c *mcpConn) result(result *mcp.CallToolResult) durable.ToolExecutionResult {
	const imagesNotice = "[pocketd left out %d images: a result takes at most %d, of at most %d bytes each, png, jpeg, gif or webp]"
	cutMark := fmt.Sprintf("\n[pocketd cut the rest: the result is longer than %d bytes]", maxMCPOutput)
	budget := maxMCPOutput - len("\n") - len(fmt.Sprintf(imagesNotice, 9_999_999_999, maxMCPImages, maxMCPImageBytes)) - len(cutMark)
	secrets := c.hub.store.secrets()
	// PiG renders a structured result with no content blocks as indented
	// JSON, whole, before any bound: pocketd writes it compact, redacted,
	// as a text block the bound then cuts.
	blocks := mcp.ToLLMContent(mcp.CallToolResult{Content: result.Content})
	if len(result.Content) == 0 && len(result.StructuredContent) > 0 {
		if structured := redactJSON(result.StructuredContent, secrets); structured != nil {
			blocks = append(blocks, mcp.LLMContent{Type: mcp.LLMContentTypeText, Text: string(structured)})
		}
	}
	var images []ai.ToolResultMessageContent
	var text strings.Builder
	tried, dropped, done := 0, 0, false
	for _, block := range blocks {
		if block.Type == mcp.LLMContentTypeImage {
			// The cheap bounds first: no image past them is decoded, and
			// at most maxMCPImages are, whether each goes or not, since
			// a decode waits on the owner's uploads.
			if tried >= maxMCPImages || len(block.Data) > maxMCPImageBytes {
				dropped++
				continue
			}
			tried++
			image, ok := mcpImage(block.MimeType, block.Data, secrets)
			if !ok {
				dropped++
				continue
			}
			images = append(images, image)
			continue
		}
		s := redactText(block.Text, secrets)
		if done || s == "" {
			continue
		}
		if text.Len() > 0 {
			s = "\n" + s
		}
		if len(s) > budget {
			s, done = cut(s, budget)+cutMark, true
		}
		budget = max(0, budget-len(s))
		text.WriteString(s)
	}
	if dropped > 0 {
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		fmt.Fprintf(&text, imagesNotice, dropped, maxMCPImages, maxMCPImageBytes)
	}
	var out []ai.ToolResultMessageContent
	if text.Len() > 0 {
		out = append(out, ai.TextContent{Text: text.String()})
	}
	out = append(out, images...)
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

// serveAuth is the OAuth sign-in of a serving pocketd's connection, as
// PiG's adapter (oauth.AdaptOAuthProvider) runs one, but with its refresh
// counted among the hub's exchanges: pocketd's close waits for it, so no
// refresh writes the store once it closed. A 403 that asks for more scope
// (PiG hands it here as an insufficient_scope challenge) is errMCPScope at
// once, since the step up needs the owner. A 401 refreshes the tokens with
// PiG's flow (refreshOnly), once for every request that waits on it, and
// a request whose token was replaced meanwhile is made again with the new
// one: a second refresh with a rotated refresh token would fail. The
// refresh under way is the server's (mcpConn.flight), not the
// connection's: one made again after a drop waits for it too.
type serveAuth struct {
	conn     *mcpConn
	provider refreshOnly
}

// mcpScopeError is a call the server refused for scope, which pocketd's
// sign-in lacks: it is an errMCPScope, and scope ("" for none it may say)
// is challengeScope's.
type mcpScopeError struct{ scope string }

func (e *mcpScopeError) Error() string { return errMCPScope.Error() }
func (e *mcpScopeError) Unwrap() error { return errMCPScope }

// The bounds of a scope pocketd says a server asked for (challengeScope).
const (
	maxChallengeScopes    = 8
	maxChallengeScopeText = 64
)

// challengeScope is the scope a 403's challenge asks for, as pocketd may
// say it: at most maxChallengeScopes scope tokens (RFC 6749, section 3.3:
// printable ASCII but a space, a quote and a backslash) of at most
// maxChallengeScopeText bytes each, none holding a secret the store
// keeps; anything else is "", since the header is the server's text.
func challengeScope(scope string, secrets []string) string {
	tokens := strings.Fields(scope)
	if len(tokens) == 0 || len(tokens) > maxChallengeScopes {
		return ""
	}
	for _, token := range tokens {
		if len(token) > maxChallengeScopeText || holdsSecret(token, secrets) ||
			strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' || r == '"' || r == '\\' }) {
			return ""
		}
	}
	return strings.Join(tokens, " ")
}

// authFlight is one refresh, which every request refused meanwhile waits
// for.
type authFlight struct {
	done chan struct{}
	err  error
}

func (a *serveAuth) Token(ctx context.Context) (string, error) {
	tokens, err := a.provider.Tokens(ctx)
	if err != nil || tokens == nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

func (a *serveAuth) OnUnauthorized(ctx context.Context, unauthorized mcp.UnauthorizedContext) error {
	if unauthorized.Response != nil && unauthorized.Response.StatusCode == http.StatusForbidden {
		// The scope it asks for, as pocketd may say it: a call may want
		// one that no sign-in's probe discovers.
		asked := oauth.ParseWWWAuthenticate(strings.Join(unauthorized.Response.Header.Values("Www-Authenticate"), ", "))
		return &mcpScopeError{scope: challengeScope(asked.Scope, a.conn.hub.store.secrets())}
	}
	challenge := oauth.ParseWWWAuthenticate(strings.Join(unauthorized.Response.Header.Values("Www-Authenticate"), ", "))
	a.conn.authMu.Lock()
	flight := a.conn.flight
	if flight == nil {
		tokens, err := a.provider.Tokens(ctx)
		switch {
		case err != nil:
		case unauthorized.Token != "" && tokens != nil && tokens.AccessToken != "" && tokens.AccessToken != unauthorized.Token:
			a.conn.authMu.Unlock()
			return nil
		case tokens == nil || tokens.RefreshToken == "":
			// No refresh token: no wait mends it, and only the owner
			// signs in again.
			a.conn.authMu.Unlock()
			return errMCPNoRefresh
		}
	}
	if flight == nil {
		if !a.conn.hub.begin() {
			a.conn.authMu.Unlock()
			return errMCPClosed
		}
		flight = &authFlight{done: make(chan struct{})}
		a.conn.flight = flight
		server, fetch := unauthorized.ServerURL.String(), unauthorized.Fetch
		go func() {
			defer a.conn.hub.work.Done()
			// A refresh the server answered may have rotated the refresh
			// token, so it runs to its end, within the server's timeout,
			// whoever stopped waiting.
			ctx, cancel := context.WithTimeout(context.Background(), a.conn.server.Timeout)
			defer cancel()
			result, err := oauth.AuthorizeMcp(ctx, a.provider, oauth.OAuthFlowOptions{
				ServerURL: server, ResourceMetadataURL: challenge.ResourceMetadataURL, Fetch: fetch,
			})
			if err == nil && result != oauth.OAuthAuthorized {
				err = errMCPRefresh
			}
			a.conn.authMu.Lock()
			a.conn.flight = nil
			a.conn.authMu.Unlock()
			flight.err = err
			close(flight.done)
		}()
	}
	a.conn.authMu.Unlock()
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// mcpImageTypes are the image types a result's image may have: the ones
// the model providers take.
var mcpImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// prepareImage decodes and fits an image as an owner's attached one is;
// tests count its calls.
var prepareImage = uploads.PrepareImage

// mcpImage is an image of a result as the model gets it, and whether it
// goes: one of mcpImageTypes, its data base64 holding no secret the store
// keeps, encoded or decoded, that decodes whole, pixels and all, within
// the bounds an owner's attached image has (uploads.PrepareImage, which
// fits a large one): a header that reads well before pixels that do not
// would have the provider refuse every request that carries it. A
// server's image is never redacted inside; one that fails is left out.
func mcpImage(mimeType, data string, secrets []string) (ai.ImageContent, bool) {
	if !slices.Contains(mcpImageTypes, mimeType) || holdsSecret(data, secrets) {
		return ai.ImageContent{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return ai.ImageContent{}, false
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(decoded, []byte(secret)) {
			return ai.ImageContent{}, false
		}
	}
	// Of the type it says it is, then whole.
	if _, format, err := image.DecodeConfig(bytes.NewReader(decoded)); err != nil || "image/"+format != mimeType {
		return ai.ImageContent{}, false
	}
	prepared, _, err := prepareImage(decoded)
	if err != nil {
		return ai.ImageContent{}, false
	}
	// Within the result's bound as the model gets it: a re-encoded GIF or
	// a turned JPEG may come out larger than it came.
	encoded := base64.StdEncoding.EncodeToString(prepared.Data)
	if len(encoded) > maxMCPImageBytes || holdsSecret(encoded, secrets) {
		return ai.ImageContent{}, false
	}
	// What the model gets holds no secret either: a re-encoding writes
	// other bytes, and other base64.
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(prepared.Data, []byte(secret)) {
			return ai.ImageContent{}, false
		}
	}
	return ai.ImageContent{Data: encoded, MimeType: prepared.MimeType}, true
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

// mcpWithin are the first of tools that fit beside a session's own tools,
// own of them, within maxSessionTools.
func mcpWithin(tools []*durable.ToolRegistration, own int) []*durable.ToolRegistration {
	return tools[:min(len(tools), max(0, maxSessionTools-own))]
}

// MCPProfiles are the profiles with an MCP server pocketd connects, as it
// read them at start, sorted (ADR-0063).
func (e *Engine) MCPProfiles() []string { return e.mcp.profiles() }

// ConnectMCP connects every MCP server a profile names, or reads its tools
// again, and returns once each answered or failed (ADR-0063): a server
// that is down is logged, keeps the tools it listed last, and is tried
// again later.
func (e *Engine) ConnectMCP(ctx context.Context) { e.mcp.connectAll(ctx) }

// SetMCPTools gives session id the MCP tools of profile's servers as
// pocketd holds them now, after its other tools, in its profile's
// extension, and says whether it changed anything (ADR-0063). A session
// that is offered every tool (no list) gets the extension alone, whose
// tools it is then offered. profile "" takes every MCP tool and extension
// from the session: one that takes none, which an older run may have
// given some. Its caller decides which sessions take them
// (team.MayUseMCP).
func (e *Engine) SetMCPTools(ctx context.Context, id int64, profile string) (bool, error) {
	var ext *durable.Extension
	if profile != "" {
		if ext = e.mcp.extension(profile); ext == nil {
			return false, nil
		}
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
	if ext != nil {
		extensions = append(extensions, ext)
	}
	change, changed := harness.AgentChange{}, false
	if !slices.Equal(extensionNames(agent.Extensions), extensionNames(extensions)) {
		change.Extensions, changed = harness.SetTo(harness.ExtensionChange{List: extensions}), true
	}
	if stored.Tools != nil && stored.Tools.Exact {
		tools := slices.DeleteFunc(slices.Clone(agent.Tools), func(t *durable.ToolRegistration) bool { return strings.HasPrefix(t.Name, mcpToolPrefix) })
		if ext != nil {
			tools = append(tools, mcpWithin(ext.Tools, len(tools))...)
		}
		if !slices.Equal(stored.Tools.Names, toolNames(tools)) {
			change.Tools, changed = harness.SetTo(harness.ToolChange{List: tools}), true
		}
	}
	if !changed {
		return false, nil
	}
	return true, conversation.Configure(ctx, change)
}
