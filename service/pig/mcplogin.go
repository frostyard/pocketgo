package pig

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The owner's steps for an MCP server (ADR-0063) run as pocketd mcp
// commands, with the data folder's lock, so pocketd is stopped: the store
// has one writer at a time, the serving engine or the command. Each writes
// through the store alone, and none prints a secret.

// maxMCPHeaderValue bounds a header's value, in bytes.
const maxMCPHeaderValue = 8 << 10

// maxCallbackLine bounds the address the owner pastes, in bytes.
const maxCallbackLine = 8 << 10

// MCPSetHeader keeps a header's value for server, read from in as a
// secret (not shown on a terminal), in the data folder's store, for the
// server's URL alone, then connects to the server to list its tools, and
// says on out what came of it. mcp.json must name the header for the
// server: pocketd sends only what it names.
func MCPSetHeader(ctx context.Context, dataDir string, server profiles.MCPServer, header string, in io.Reader, out io.Writer) error {
	canonical := http.CanonicalHeaderKey(header)
	if !slices.Contains(server.Headers, canonical) {
		return fmt.Errorf(`server %s names no header %s in %s: write "headers": {"%s": "%s"} there first; nothing was changed`,
			server.Name, canonical, profiles.MCPFile, canonical, profiles.MCPStored)
	}
	store, err := openMCPStore(dataDir)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Paste the value of %s for %s (the whole value, as \"Bearer …\" when it is a bearer token), then Enter: ", canonical, server.Name)
	value, err := readSecretUpTo(in, maxMCPHeaderValue)
	if err != nil {
		return fmt.Errorf("read the value: %w", err)
	}
	if err := headerValue(value); err != nil {
		return fmt.Errorf("%w: nothing was changed", err)
	}
	if err := store.setHeader(server.Name, server.URL, canonical, value); err != nil {
		return fmt.Errorf("keep the value: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Kept %s for %s, for its URL alone.\n", canonical, server.Name)
	reportListing(ctx, out, store, server)
	return nil
}

// minMCPCredential is a header's credential at least, in bytes: pocketd
// takes every text a tool sees through redact, and a shorter secret
// would take ordinary words with it.
const minMCPCredential = 8

// headerValue refuses a header's value that is empty, too long, holds a
// character outside printable ASCII (spaces inside it included), or whose
// credential (headerCredential) is shorter than minMCPCredential.
func headerValue(value string) error {
	switch {
	case strings.TrimSpace(value) == "":
		return errors.New("no value")
	case len(value) > maxMCPHeaderValue:
		return fmt.Errorf("the value is longer than %d bytes", maxMCPHeaderValue)
	case value != strings.TrimSpace(value) || !printableValue(value):
		return errors.New("the value holds a character outside printable ASCII, or a space at an end")
	case len(headerCredential(value)) < minMCPCredential:
		return fmt.Errorf("the value's credential is shorter than %d characters: pocketd keeps a secret out of every text a tool sees, which a shorter one would not allow", minMCPCredential)
	}
	return nil
}

func printableValue(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r > '~' })
}

// MCPLogout drops what pocketd keeps for the server named name: its
// headers and its sign-in. It says whether there was any. It reads the
// credentials alone: a listings file it cannot read keeps no credential
// on disk.
func MCPLogout(dataDir, name string) (bool, error) {
	store, err := openMCPCredentials(dataDir)
	if err != nil {
		return false, err
	}
	return store.forget(name)
}

// MCPState is what pocketd keeps for one server, with no secret in it.
type MCPState struct {
	// Headers are the headers mcp.json names that pocketd keeps a value
	// of for the server's URL, and Missing those it does not.
	Headers, Missing []string
	// SignedIn says an OAuth sign-in is kept for the server's URL.
	SignedIn bool
	// Tools is how many tools the server listed last, at its URL; Listed
	// says it listed any there.
	Tools  int
	Listed bool
}

// MCPStates are what pocketd keeps for each of servers, by name. It reads
// the store and changes nothing, so it runs while pocketd serves.
func MCPStates(dataDir string, servers profiles.MCPServers) (map[string]MCPState, error) {
	store, err := openMCPStore(dataDir)
	if err != nil {
		return nil, err
	}
	out := map[string]MCPState{}
	for _, s := range servers.Servers {
		kept, missing := store.headers(s.Name, s.URL, s.Headers)
		state := MCPState{Missing: missing, SignedIn: store.signedIn(s.Name, s.URL)}
		for _, h := range s.Headers {
			if _, ok := kept[h]; ok {
				state.Headers = append(state.Headers, h)
			}
		}
		tools, listed := store.listing(s.Name, s.URL)
		state.Tools, state.Listed = len(tools), listed
		out[s.Name] = state
	}
	return out, nil
}

// MCPLogin signs pocketd in to server, whose mcp.json entry has oauth:
// PiG's authorization code flow with PKCE, registering pocketd as a
// client when mcp.json names none. It prints the page to approve pocketd
// on out, reads the address the browser lands on, pasted, from in, and
// keeps the sign-in, which the serving pocketd refreshes, in the data
// folder's store; then it lists the server's tools. A sign-in that does
// not end changes nothing.
func MCPLogin(ctx context.Context, dataDir string, server profiles.MCPServer, in io.Reader, out io.Writer) error {
	if server.OAuth == nil {
		return fmt.Errorf("server %s has no oauth in %s: it signs in with its headers (pocketd mcp set-header); nothing was changed", server.Name, profiles.MCPFile)
	}
	store, err := openMCPStore(dataDir)
	if err != nil {
		return err
	}
	resource, err := oauth.ParseURL(server.URL)
	if err != nil {
		return fmt.Errorf("server %s's url is no URL", server.Name)
	}
	// The headers mcp.json names go with the probe of the server alone,
	// never to the authorization server: a gateway may want one before it
	// answers its challenge.
	headers, missing := store.headers(server.Name, server.URL, server.Headers)
	if len(missing) > 0 {
		return fmt.Errorf("server %s needs its header %s first: pocketd mcp set-header %s %s; nothing was changed", server.Name, missing[0], server.Name, missing[0])
	}
	fetch := mcpClient(mcpLoginTimeout)
	challenge := probeChallenge(ctx, fetch, server.URL, headers)
	// The scopes and the kept client are weighed against this discovery,
	// which the flow then runs on: one that failed leaves nothing to
	// weigh them against, and a second one could name other scopes.
	info, err := discoverServer(ctx, fetch, server, challenge)
	if err != nil {
		return fmt.Errorf("the sign-in to %s did not start: %s; nothing was changed", server.Name, oauthWords(err))
	}
	scope := loginScope(server, challenge, info)
	// The sign-in runs on a copy: only a sign-in that ends reaches the
	// store. A client pocketd registered before is kept when it was
	// registered with the authorization server discovery names now, for
	// this redirect URI and for every scope asked for now: another
	// server never got it, and a server may refuse a client a scope it
	// did not register. The flow runs on the discovery checked here, as
	// PiG's flow keeps one: a second discovery could name another
	// authorization server, which would get the client kept for this one.
	seed := oauth.McpOAuthState{ServerURL: resource.Href(), Discovery: &oauth.OAuthDiscoveryState{AuthorizationServerURL: info.AuthorizationServerURL,
		AuthorizationServerMetadata: info.AuthorizationServerMetadata, ResourceMetadata: info.ResourceMetadata}}
	if challenge.ResourceMetadataURL != nil {
		seed.Discovery.ResourceMetadataURL = challenge.ResourceMetadataURL.Href()
	}
	if old := store.oauthState(server.Name, server.URL); old != nil && server.OAuth.ClientID == "" && old.ClientInformation != nil &&
		slices.Contains(old.ClientInformation.RedirectURIs, server.OAuth.CallbackURL) && registeredFor(old.ClientInformation.Scope, scope) &&
		old.Discovery != nil && old.Discovery.AuthorizationServerURL == info.AuthorizationServerURL {
		seed.ClientInformation = old.ClientInformation
	}
	memory := &oauth.MemoryOAuthStateStore{}
	if err := memory.Save(ctx, seed); err != nil {
		return err
	}
	var authorization *oauth.URL
	provider, err := oauth.NewMcpOAuthProvider(oauth.McpOAuthProviderOptions{
		ServerURL: server.URL, RedirectURL: server.OAuth.CallbackURL, ClientID: server.OAuth.ClientID,
		ClientMetadata: oauth.OAuthClientMetadata{ClientName: mcpClientName, Scope: server.OAuth.Scope},
		Store:          memory,
		OnRedirect:     func(_ context.Context, u oauth.URL) error { authorization = &u; return nil },
	})
	if err != nil {
		return err
	}
	flow := oauth.OAuthFlowOptions{ServerURL: server.URL, Fetch: fetch, ResourceMetadataURL: challenge.ResourceMetadataURL,
		Scope: scope, SkipRefresh: true}
	result, err := oauth.AuthorizeMcp(ctx, provider, flow)
	if err != nil {
		return fmt.Errorf("the sign-in to %s did not start: %s; nothing was changed", server.Name, oauthWords(err))
	}
	if result != oauth.OAuthRedirect || authorization == nil {
		return fmt.Errorf("the sign-in to %s did not start: the server named no page to approve pocketd; nothing was changed", server.Name)
	}
	// The page the owner opens is bound as pocketd's own exchanges are:
	// the owner's approval never crosses plain HTTP to another host.
	if page, err := url.Parse(authorization.Href()); err != nil || !profiles.MCPTransportAllowed(page) {
		return fmt.Errorf("the sign-in to %s did not start: its authorization page %s; nothing was changed", server.Name, errMCPInsecure)
	}
	state, err := provider.State(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Open this page in a browser, and approve pocketd for %s:\n\n  %s\n\n", server.Name, authorization.Href())
	_, _ = fmt.Fprintf(out, "Then paste the address the browser lands on, which starts with %s (the page itself need not load), and Enter:\n", server.OAuth.CallbackURL)
	line, err := readLine(in, maxCallbackLine)
	if err != nil {
		return fmt.Errorf("read the address: %w; nothing was changed", err)
	}
	code, iss, err := callbackCode(line, server.OAuth.CallbackURL, state)
	if err != nil {
		return fmt.Errorf("%w; nothing was changed", err)
	}
	flow.AuthorizationCode, flow.Iss, flow.SkipRefresh = code, iss, false
	if _, err := oauth.AuthorizeMcp(ctx, provider, flow); err != nil {
		return fmt.Errorf("the sign-in to %s did not end: %s; nothing was changed", server.Name, oauthWords(err))
	}
	signed, err := memory.Load(ctx)
	if err != nil {
		return err
	}
	if signed == nil || signed.Tokens == nil || signed.Tokens.AccessToken == "" {
		return fmt.Errorf("the sign-in to %s did not end: the server gave no token; nothing was changed", server.Name)
	}
	keep := signed.Clone()
	keep.CodeVerifier, keep.OAuthState = "", ""
	if err := store.saveOAuth(server.Name, server.URL, keep); err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	refresh := "it holds no refresh token, so the owner signs in again once its access token ends"
	if keep.Tokens.RefreshToken != "" {
		refresh = "pocketd refreshes it"
	}
	_, _ = fmt.Fprintf(out, "Signed in to %s; %s.\n", server.Name, refresh)
	reportListing(ctx, out, store, server)
	return nil
}

// probeChallenge is the challenge a server answers an initialize with no
// sign-in with: where its resource metadata is, and the scopes it wants.
// A server that answers otherwise, or not at all, gives none, and
// discovery looks where RFC 9728 says.
func probeChallenge(ctx context.Context, fetch *http.Client, server string, headers map[string]string) oauth.OAuthChallenge {
	body := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"` + mcp.LatestProtocolVersion +
		`","capabilities":{},"clientInfo":{"name":"` + mcpClientName + `","version":"1"}}}`)
	ctx, cancel := context.WithTimeout(ctx, mcpLoginTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(body))
	if err != nil {
		return oauth.OAuthChallenge{}
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := fetch.Do(request)
	if err != nil {
		return oauth.OAuthChallenge{}
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusUnauthorized {
		return oauth.OAuthChallenge{}
	}
	return oauth.ParseWWWAuthenticate(strings.Join(response.Header.Values("Www-Authenticate"), ", "))
}

// loginScope is the scopes a sign-in asks for: the ones the server asks
// for, its challenge's, else its resource metadata's, with the ones
// mcp.json adds. PiG asks for the scopes it is given in place of the
// metadata's, so they are read here: an added scope never drops one the
// server's tools need, and a kept client is kept only for the scopes the
// request names (registeredFor).
func loginScope(server profiles.MCPServer, challenge oauth.OAuthChallenge, info *oauth.OAuthServerInfo) string {
	wanted := challenge.Scope
	if wanted == "" && info != nil && info.ResourceMetadata != nil {
		wanted = strings.Join(info.ResourceMetadata.ScopesSupported, " ")
	}
	return mergeScopes(wanted, server.OAuth.Scope)
}

// discoverServer is what discovery finds of server's sign-in now: its
// resource metadata and its authorization server. PiG's flow runs on it,
// as its own cached discovery; it tells which kept client may serve, and
// which scopes the request names.
func discoverServer(ctx context.Context, fetch *http.Client, server profiles.MCPServer, challenge oauth.OAuthChallenge) (*oauth.OAuthServerInfo, error) {
	options := oauth.DiscoveryOptions{Fetch: fetch}
	if challenge.ResourceMetadataURL != nil {
		options.ResourceMetadataURL = challenge.ResourceMetadataURL.Href()
	}
	info, err := oauth.DiscoverOAuthServerInfo(ctx, server.URL, options)
	if err == nil && info == nil {
		err = errors.New("discovery found no authorization server")
	}
	return info, err
}

// registeredFor says whether a client registered for the scopes
// registered may ask for wanted: it registered none, which bounds
// nothing, or every one of wanted.
func registeredFor(registered, wanted string) bool {
	if strings.TrimSpace(registered) == "" {
		return true
	}
	have := strings.Fields(registered)
	for _, scope := range strings.Fields(wanted) {
		if !slices.Contains(have, scope) {
			return false
		}
	}
	return true
}

// mergeScopes are the scopes of every list, each once, in order.
func mergeScopes(lists ...string) string {
	var out []string
	for _, list := range lists {
		for _, scope := range strings.Fields(list) {
			if !slices.Contains(out, scope) {
				out = append(out, scope)
			}
		}
	}
	return strings.Join(out, " ")
}

// oauthWords says why a sign-in failed in pocketd's words, with the OAuth
// error's code alone: the server's description, and PiG's errors, may
// quote what was sent.
func oauthWords(err error) string {
	var oauthErr *oauth.OAuthError
	var status *mcp.McpHttpError
	switch {
	case errors.As(err, &oauthErr):
		return "the authorization server answered " + safeWord(oauthErr.Code)
	case errors.As(err, &status):
		return fmt.Sprintf("the server answered HTTP %d", status.Status)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "it was stopped"
	case errors.Is(err, errMCPInsecure):
		return "its discovery " + errMCPInsecure.Error()
	}
	return "pocketd could not complete the OAuth discovery, registration or token exchange with the server"
}

// oauthCodes are the OAuth error codes pocketd names (RFC 6749, RFC 6750,
// RFC 7591 and OpenID Connect's): any other value is said to be none, as
// a server or a pasted address may put a token or a code where a code
// belongs.
var oauthCodes = []string{
	"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type", "invalid_scope",
	"access_denied", "unsupported_response_type", "server_error", "temporarily_unavailable", "invalid_token", "insufficient_scope",
	"invalid_redirect_uri", "invalid_client_metadata", "invalid_software_statement", "unapproved_software_statement",
	"interaction_required", "login_required", "consent_required", "account_selection_required",
}

// safeWord is an OAuth error code as it may show: one of oauthCodes, else
// words that say it is none.
func safeWord(code string) string {
	if slices.Contains(oauthCodes, code) {
		return code
	}
	return "an error pocketd does not name"
}

// readLine reads one line of at most limit bytes from in, its line end
// and the spaces around it dropped. A longer line is refused, never cut:
// a part of an address would lose its iss, or cut its code.
func readLine(in io.Reader, limit int) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, int64(limit)+2)).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", err
	}
	if len(strings.TrimRight(line, "\r\n")) > limit {
		return "", fmt.Errorf("the line is longer than %d bytes", limit)
	}
	return strings.TrimSpace(line), nil
}

// callbackCode is the authorization code of the address the browser
// landed on, pasted, and its iss when it names one: an address at the
// redirect URI, with the state pocketd sent. Its refusals never repeat
// the code.
func callbackCode(line, callback, state string) (code string, iss *string, err error) {
	got, err := url.Parse(line)
	if err != nil || !got.IsAbs() {
		return "", nil, fmt.Errorf("that is not the address the browser landed on: it starts with %s", callback)
	}
	want, err := url.Parse(callback)
	if err != nil {
		return "", nil, err
	}
	if got.Scheme != want.Scheme || got.Host != want.Host || strings.TrimSuffix(got.Path, "/") != strings.TrimSuffix(want.Path, "/") {
		return "", nil, fmt.Errorf("that address is not at the redirect URI %s", callback)
	}
	query := got.Query()
	if refused := query.Get("error"); refused != "" {
		return "", nil, fmt.Errorf("the server did not approve pocketd: it answered %s", safeWord(refused))
	}
	if query.Get("state") != state {
		return "", nil, errors.New("that address is of another sign-in (its state differs): start again")
	}
	code = query.Get("code")
	if code == "" {
		return "", nil, errors.New("that address holds no code")
	}
	if query.Has("iss") {
		value := query.Get("iss")
		iss = &value
	}
	return code, iss, nil
}

// reportListing connects to server with what the store keeps for it,
// lists its tools, keeps them, and says on out what came of it.
func reportListing(ctx context.Context, out io.Writer, store *mcpStore, server profiles.MCPServer) {
	n, unkept, err := listOnce(ctx, store, server)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(out, "Its tools could not be listed now: it %s. pocketd connects it again when it starts.\n", strings.TrimPrefix(err.Error(), server.Name+" "))
	case unkept != nil:
		_, _ = fmt.Fprintf(out, "%s lists %d tools, but pocketd could not keep them (%v): it lists them again when it starts.\n", server.Name, n, unkept)
	default:
		_, _ = fmt.Fprintf(out, "%s lists %d tools; pocketd offers them from its next start.\n", server.Name, n)
	}
}

// listOnce connects to server once, lists its tools and keeps them in
// store, then closes the connection. unkept says why the listing is not
// kept; err is mcpConn.failure's.
func listOnce(ctx context.Context, store *mcpStore, server profiles.MCPServer) (n int, unkept, err error) {
	hubCtx, cancel := context.WithCancel(ctx)
	h := &mcpHub{store: store, log: slog.New(slog.DiscardHandler), conns: map[string]*mcpConn{}, servers: map[string][]string{},
		tools: map[string][]mcpTool{}, exts: map[string]*durable.Extension{}, ctx: hubCtx, cancel: cancel}
	// Every exchange it began, a refresh's included, ends before the
	// command lets go of the data folder's lock; the store writes nothing
	// after.
	defer h.close(context.Background())
	c := &mcpConn{hub: h, server: server}
	h.conns[server.Name] = c
	if tools, ok := store.listing(server.Name, server.URL); ok {
		h.tools[server.Name] = tools
	}
	client, err := c.connect(ctx, false)
	if err != nil {
		return 0, nil, errors.New(server.Name + " " + c.failure(err))
	}
	c.drop(client)
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tools[server.Name]), h.unkept[server.Name], nil
}
