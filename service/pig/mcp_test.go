package pig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The fake MCP world: two servers on one host, one that takes a header and
// one that signs in with OAuth, and the OAuth server. Every request is
// answered in memory: no socket, no network.
const (
	notesURL   = "https://mcp.example.com/notes"
	gatewayURL = "https://mcp.example.com/gateway/mcp"
	authURL    = "https://auth.example.com"
	notesKey   = "Bearer notes-secret-value"
)

// fakeMCP answers the fake MCP world's requests.
type fakeMCP struct {
	t  *testing.T
	mu sync.Mutex
	// down fails every request, as a host that is not up.
	down bool
	// tools are what both servers list.
	tools []map[string]any
	// access are the gateway's access tokens it takes now.
	access map[string]bool
	// refresh is the refresh token the token endpoint takes, and next the
	// access token it gives for it; revoked refuses every refresh, and
	// tokenDown fails every request to the token endpoint, as a host that
	// is not up.
	refresh, next      string
	revoked, tokenDown bool
	// challenges are the PKCE challenges of the codes it approved.
	challenges map[string]string
	// seen are the requests, by method and URL, auths each one's
	// Authorization header, and calls the tools called, in order; headers
	// are each request's headers, and deadlines whether its context had a
	// deadline.
	seen, auths, calls []string
	headers            []http.Header
	deadlines          []bool
	registered         int
	// scopes are the gateway's resource metadata's scopes_supported, and
	// tenant the X-Tenant header the gateway wants before it answers its
	// challenge ("" for none).
	scopes []string
	tenant string
	// images is how many images the pictures tool answers, and big how
	// many bytes of text the big tool does.
	images, big int
	// hold, when set, keeps every initialize waiting until it is closed,
	// after it says so on arrived.
	hold, arrived chan struct{}
	// perPage, when set, makes tools/list answer pages of that many tools,
	// each with a next cursor, and pages counts them.
	perPage, pages int
	// noList answers tools/list with no tools array, listBusy with a
	// 429, and tokenError answers every refresh with that OAuth error.
	noList, listBusy, listError bool
	tokenError                  string
}

func newFakeMCP(t *testing.T) *fakeMCP {
	f := &fakeMCP{t: t, access: map[string]bool{}, challenges: map[string]string{}, tools: []map[string]any{
		{"name": "search", "description": "Search the notes.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
		{"name": "echo_auth", "description": "Say what came.", "inputSchema": map[string]any{"type": "object"}},
	}}
	mcpTransport = f
	t.Cleanup(func() { mcpTransport = nil })
	return f
}

func (f *fakeMCP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	hold, arrived := f.hold, f.arrived
	f.mu.Unlock()
	if hold != nil && r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"initialize"`)) {
			arrived <- struct{}{}
			<-hold
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.String())
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.headers = append(f.headers, r.Header.Clone())
	_, hasDeadline := r.Context().Deadline()
	f.deadlines = append(f.deadlines, hasDeadline)
	if f.down {
		return nil, errors.New("dial tcp: connection refused")
	}
	w := httptest.NewRecorder()
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	switch r.URL.String() {
	case notesURL:
		if r.Header.Get("Authorization") != notesKey {
			w.WriteHeader(http.StatusUnauthorized)
			break
		}
		f.serveMCP(w, r, body)
	case gatewayURL:
		if f.tenant != "" && r.Header.Get("X-Tenant") != f.tenant {
			w.WriteHeader(http.StatusForbidden)
			break
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !f.access[token] {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/gateway/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			break
		}
		f.serveMCP(w, r, body)
	case "https://mcp.example.com/.well-known/oauth-protected-resource/gateway/mcp":
		metadata := map[string]any{"resource": gatewayURL, "authorization_servers": []string{authURL}}
		if f.scopes != nil {
			metadata["scopes_supported"] = f.scopes
		}
		writeJSON(w, metadata)
	case authURL + "/.well-known/oauth-authorization-server":
		writeJSON(w, map[string]any{"issuer": authURL, "authorization_endpoint": authURL + "/authorize", "token_endpoint": authURL + "/token",
			"registration_endpoint": authURL + "/register", "response_types_supported": []string{"code"},
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"}})
	case authURL + "/register":
		f.registered++
		var metadata map[string]any
		_ = json.Unmarshal(body, &metadata)
		metadata["client_id"] = "client-1"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(metadata)
	case authURL + "/token":
		if f.tokenDown {
			return nil, errors.New("dial tcp: connection refused")
		}
		form, _ := url.ParseQuery(string(body))
		f.token(w, form)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return w.Result(), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// token answers the token endpoint: a code it approved, with its PKCE
// verifier, or the refresh token it takes.
func (f *fakeMCP) token(w http.ResponseWriter, form url.Values) {
	refuse := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code, "error_description": "the refresh token " + form.Get("refresh_token") + " is no good"})
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if challenge, ok := f.challenges[form.Get("code")]; !ok || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			refuse("invalid_grant")
			return
		}
		f.access["acc-1"], f.refresh, f.next = true, "ref-1", "acc-2"
		writeJSON(w, map[string]any{"access_token": "acc-1", "refresh_token": "ref-1", "token_type": "Bearer", "expires_in": 3600})
	case "refresh_token":
		if f.tokenError != "" {
			refuse(f.tokenError)
			return
		}
		if f.revoked || form.Get("refresh_token") != f.refresh {
			refuse("invalid_grant")
			return
		}
		f.access[f.next] = true
		writeJSON(w, map[string]any{"access_token": f.next, "token_type": "Bearer", "expires_in": 3600})
	default:
		refuse("unsupported_grant_type")
	}
}

// serveMCP answers one JSON-RPC message of the Streamable HTTP transport.
func (f *fakeMCP) serveMCP(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method == http.MethodDelete {
		return
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if message.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch message.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "s1")
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
	case "tools/list":
		if f.listBusy {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if f.listError {
			writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32603, "message": "the list is not ready"}})
			return
		}
		if f.noList {
			result = map[string]any{}
			break
		}
		if f.perPage > 0 {
			f.pages++
			var page []map[string]any
			for i := range f.perPage {
				page = append(page, map[string]any{"name": fmt.Sprintf("p%d_%d", f.pages, i), "inputSchema": map[string]any{"type": "object"}})
			}
			result = map[string]any{"tools": page, "nextCursor": "more"}
			break
		}
		result = map[string]any{"tools": f.tools}
	case "tools/call":
		f.calls = append(f.calls, message.Params.Name)
		if message.Params.Name == "admin" {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="admin"`)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if message.Params.Name == "busy" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		text := "found: " + toString(message.Params.Arguments["q"])
		switch message.Params.Name {
		case "echo_auth":
			text = "you sent " + r.Header.Get("Authorization")
		case "echo_token":
			_, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
			text = "your token is " + token
		case "big":
			text = strings.Repeat("x", f.big)
		}
		content := []map[string]any{{"type": "text", "text": text}}
		if message.Params.Name == "pictures" {
			for range f.images {
				content = append(content, map[string]any{"type": "image", "data": pngImage, "mimeType": "image/png"})
			}
		}
		result = map[string]any{"content": content}
	default:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "no method " + message.Method}})
		return
	}
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func (f *fakeMCP) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// mcpServers are the fake world's servers, as mcp.json checks them.
func mcpServers(t *testing.T) profiles.MCPServers {
	t.Helper()
	servers, err := profiles.ParseMCP([]byte(`{"mcpServers": {
		"notes": {"url": "` + notesURL + `", "headers": {"Authorization": "stored"}},
		"gateway": {"url": "` + gatewayURL + `", "oauth": {}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	return servers
}

// openMCP is the fixture's engine with the fake world's servers, for the
// profiles' lists.
func (f *fixture) openMCP(models *ai.Models, servers profiles.MCPServers, lists map[string][]string) *Engine {
	f.t.Helper()
	e, err := open(context.Background(), Options{
		DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve, NeedsApproval: func(string) bool { return false },
		Unsandboxed: true, MCP: servers, MCPProfiles: lists,
	}, models)
	if err != nil {
		f.t.Fatal(err)
	}
	e.Resume()
	f.t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// keepHeader stores the notes server's header, as pocketd mcp set-header
// does.
func (f *fixture) keepHeader(value string) {
	f.t.Helper()
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", value); err != nil {
		f.t.Fatal(err)
	}
}

// mcpResult runs one message in session id and returns the last tool
// result of the transcript.
func mcpResult(t *testing.T, e *Engine, id int64, text string) *proto.ToolResult {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Send(ctx, id, text, text); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ToolResult != nil {
			return entries[i].ToolResult
		}
	}
	t.Fatal("no tool result")
	return nil
}

// offered are the tools a session's agent is offered, by name.
func offered(t *testing.T, e *Engine, id int64) []string {
	t.Helper()
	conversation, err := e.conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return toolNames(agent.Tools)
}

var chani = &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true, MCP: []string{"notes", "gateway"}}

// A profile's header server connects with the header pocketd keeps: its
// session lists the server's tools as mcp__<server>__<tool> and calls
// them, and the header's value reaches neither the tool's result nor the
// transcript (ADR-0063).
func TestAProfilesHeaderServerIsCalledWithTheKeptHeader(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(toolTurn("mcp__notes__search", map[string]any{"q": "tea"}, "c1"), answer("ok"),
		toolTurn("mcp__notes__echo_auth", map[string]any{}, "c2"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	tools := offered(t, e, id)
	if !slices.Contains(tools, "mcp__notes__search") || !slices.Contains(tools, "mcp__notes__echo_auth") {
		t.Fatalf("offered %v", tools)
	}
	if got := mcpResult(t, e, id, "search"); got.IsError || got.Text != "found: tea" {
		t.Fatalf("search: %+v", got)
	}
	got := mcpResult(t, e, id, "echo")
	if strings.Contains(got.Text, "notes-secret-value") || got.Text != "you sent [redacted]" {
		t.Fatalf("the echo shows the header: %+v", got)
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(entries)
	if strings.Contains(string(data), "notes-secret-value") {
		t.Fatal("the transcript holds the header's value")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !slices.Equal(fake.calls, []string{"search", "echo_auth"}) {
		t.Fatalf("calls %v", fake.calls)
	}
}

// A server that is down keeps the tools it listed last: its session
// starts, is offered them, and a call says the server is down and what to
// do, never a failed run.
func TestAServerThatIsDownGivesAClearToolError(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setListing("notes", notesURL, []mcpTool{{Name: "search", Description: "Search.", InputSchema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
		t.Fatal(err)
	}
	fake.set(func() { fake.down = true })
	e := f.openMCP(fauxModels(toolTurn("mcp__notes__search", map[string]any{}, "c1"), answer("ok")), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	if tools := offered(t, e, id); !slices.Contains(tools, "mcp__notes__search") {
		t.Fatalf("offered %v", tools)
	}
	got := mcpResult(t, e, id, "search")
	if !got.IsError || !strings.Contains(got.Text, "MCP server notes is unreachable") || !strings.Contains(got.Text, "try again later") {
		t.Fatalf("result %+v", got)
	}
}

// A profile with no mcp list gets no MCP tool, nor does a worker, a
// review, a consult, a gather's agent or a plan's lead of a profile with
// one: only the profile's own servers, in the sessions that take them.
func TestOnlyTheProfilesOwnSessionsGetItsServers(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	vera := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}
	for name, spec := range map[string]api.SessionSpec{
		"no mcp list":   {Profile: vera},
		"no profile":    {},
		"a worker":      {Worker: true, Thread: 1},
		"a review":      {Profile: chani, Thread: 1, Review: true},
		"a consult":     {Profile: chani, Consult: 1},
		"a gather":      {Profile: chani, Gather: 1},
		"a plan's lead": {Profile: chani, Thread: 1, Plan: 1},
	} {
		spec.Cwd, spec.Model = f.work, fauxRef
		id, err := e.CreateSession(ctx, name, spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range offered(t, e, id) {
			if strings.HasPrefix(tool, mcpToolPrefix) {
				t.Errorf("%s: offered %s", name, tool)
			}
		}
	}
	id, err := e.CreateSession(ctx, "specialist", api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani, Thread: 1})
	if err != nil {
		t.Fatal(err)
	}
	if tools := offered(t, e, id); !slices.Contains(tools, "mcp__notes__search") {
		t.Errorf("a thread's specialist is offered %v", tools)
	}
}

// pocketd refreshes an OAuth sign-in with no owner step: a token the
// server refuses is refreshed, the call made again with the new one, and
// the new one kept.
func TestAnOAuthTokenRefreshesWithNoOwnerStep(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	fake.set(func() { fake.refresh, fake.next = "ref-1", "acc-2" })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	e := f.openMCP(fauxModels(toolTurn("mcp__gateway__search", map[string]any{"q": "x"}, "c1"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"gateway"}})
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	// No tool is listed before the first connection: the start's pass
	// gives the session the tools once the server lists them.
	e.ConnectMCP(ctx)
	if _, err := e.SetMCPTools(ctx, id, "chani"); err != nil {
		t.Fatal(err)
	}
	if got := mcpResult(t, e, id, "search"); got.IsError || got.Text != "found: x" {
		t.Fatalf("result %+v", got)
	}
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	state := store.oauthState("gateway", gatewayURL)
	if state == nil || state.Tokens == nil || state.Tokens.AccessToken != "acc-2" || state.Tokens.RefreshToken != "ref-1" {
		t.Fatalf("kept %+v", state)
	}
}

// A sign-in the server revoked gives a clear error: the owner signs in
// again. pocketd starts no sign-in itself, registers no client and drops
// nothing it keeps.
func TestARevokedSignInSaysToSignInAgain(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	fake.set(func() { fake.revoked = true })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	ctx := context.Background()
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setListing("gateway", gatewayURL, []mcpTool{{Name: "search"}}); err != nil {
		t.Fatal(err)
	}
	e := f.openMCP(fauxModels(toolTurn("mcp__gateway__search", map[string]any{}, "c1"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"gateway"}})
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpResult(t, e, id, "search")
	if !got.IsError || !strings.Contains(got.Text, "refused pocketd's sign-in, and its refresh: the owner signs in again with pocketd mcp login gateway") ||
		strings.Contains(got.Text, "ref-1") {
		t.Fatalf("result %+v", got)
	}
	after, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	state := after.oauthState("gateway", gatewayURL)
	if state == nil || state.Tokens == nil || state.Tokens.RefreshToken != "ref-1" || state.CodeVerifier != "" {
		t.Fatalf("kept %+v", state)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 0 {
		t.Fatalf("pocketd registered %d clients", fake.registered)
	}
}

// seedSignIn keeps a sign-in to the gateway, as pocketd mcp login does.
func seedSignIn(t *testing.T, dir, access, refresh string) {
	t.Helper()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := oauth.ParseURL(gatewayURL)
	if err != nil {
		t.Fatal(err)
	}
	client := oauth.OAuthClientInformation{ClientID: "client-1"}
	client.RedirectURIs = []string{profiles.DefaultMCPCallback}
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{ServerURL: resource.Href(), ClientInformation: &client,
		Tokens: &oauth.OAuthTokens{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer"}}); err != nil {
		t.Fatal(err)
	}
}

// pocketd mcp login signs in with PKCE, from the address the owner pastes,
// and keeps the sign-in and the server's tools; the code and the tokens
// are never printed.
func TestMCPLoginKeepsTheSignInFromThePastedAddress(t *testing.T) {
	fake := newFakeMCP(t)
	dir := t.TempDir()
	server, _ := mcpServers(t).Find("gateway")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- MCPLogin(context.Background(), dir, server, inR, outW)
		_ = outW.Close()
	}()
	var printed strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(printed.String(), "and Enter:") {
		n, err := outR.Read(buf)
		if err != nil {
			t.Fatalf("read the prompt: %v (%s)", err, printed.String())
		}
		printed.Write(buf[:n])
	}
	var page string
	for line := range strings.SplitSeq(printed.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), authURL+"/authorize") {
			page = strings.TrimSpace(line)
		}
	}
	authorize, err := url.Parse(page)
	if err != nil || page == "" {
		t.Fatalf("no page to approve in %q", printed.String())
	}
	query := authorize.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("client_id") != "client-1" || query.Get("redirect_uri") != profiles.DefaultMCPCallback {
		t.Fatalf("the page %s", page)
	}
	fake.set(func() { fake.challenges["code-1"] = query.Get("code_challenge") })
	go func() {
		_, _ = io.WriteString(inW, profiles.DefaultMCPCallback+"?code=code-1&state="+url.QueryEscape(query.Get("state"))+"\n")
	}()
	rest, _ := io.ReadAll(outR)
	printed.Write(rest)
	if err := <-done; err != nil {
		t.Fatalf("login: %v\n%s", err, printed.String())
	}
	if !strings.Contains(printed.String(), "Signed in to gateway; pocketd refreshes it.") || !strings.Contains(printed.String(), "gateway lists 2 tools") {
		t.Fatalf("printed %s", printed.String())
	}
	for _, secret := range []string{"code-1", "acc-1", "ref-1"} {
		if strings.Contains(printed.String(), secret) {
			t.Fatalf("printed %s: %s", secret, printed.String())
		}
	}
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := store.oauthState("gateway", gatewayURL)
	if state == nil || state.Tokens.AccessToken != "acc-1" || state.Tokens.RefreshToken != "ref-1" || state.ClientInformation.ClientID != "client-1" ||
		state.CodeVerifier != "" || state.OAuthState != "" {
		t.Fatalf("kept %+v", state)
	}
	if tools, ok := store.listing("gateway", gatewayURL); !ok || len(tools) != 2 {
		t.Fatalf("listing %v %v", tools, ok)
	}
	info, err := os.Stat(filepath.Join(dir, MCPAuthFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the store's file %v, %v", info, err)
	}
}

// A pasted address of another sign-in, or of another place, is refused,
// and the code never shows in the refusal.
func TestCallbackCodeChecksTheAddress(t *testing.T) {
	cb := profiles.DefaultMCPCallback
	if code, iss, err := callbackCode(cb+"?code=c&state=s&iss=https%3A%2F%2Fauth.example.com", cb, "s"); err != nil || code != "c" || iss == nil || *iss != authURL {
		t.Fatalf("a good address: %q %v %v", code, iss, err)
	}
	for name, line := range map[string]string{
		"another state": cb + "?code=secret-code&state=x",
		"another place": "http://127.0.0.1:9999/other?code=secret-code&state=s",
		"no code":       cb + "?state=s",
		"a refusal":     cb + "?error=access_denied&state=s",
		"not a URL":     "secret-code",
	} {
		_, _, err := callbackCode(line, cb, "s")
		if err == nil || strings.Contains(err.Error(), "secret-code") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A header's value is kept for the URL it was stored for alone: a server
// whose URL changed in mcp.json gets none of it, and connects not.
func TestAHeaderGoesToTheURLItWasStoredForAlone(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	moved, err := profiles.ParseMCP([]byte(`{"mcpServers": {"notes": {"url": "https://other.example.com/notes", "headers": {"Authorization": "stored"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	e := f.openMCP(fauxModels(), moved, map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i, auth := range fake.auths {
		if auth != "" {
			t.Fatalf("request %s carried %q", fake.seen[i], auth)
		}
	}
	if len(fake.seen) != 0 {
		t.Fatalf("pocketd connected with no header: %v", fake.seen)
	}
}

// pocketd mcp set-header keeps the value pasted for the server's URL, and
// lists its tools; a header mcp.json does not name is refused.
func TestMCPSetHeaderKeepsTheValue(t *testing.T) {
	newFakeMCP(t)
	dir := t.TempDir()
	server, _ := mcpServers(t).Find("notes")
	var out strings.Builder
	if err := MCPSetHeader(context.Background(), dir, server, "authorization", strings.NewReader(notesKey+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "notes-secret-value") || !strings.Contains(out.String(), "notes lists 2 tools") {
		t.Fatalf("printed %s", out.String())
	}
	states, err := MCPStates(dir, mcpServers(t))
	if err != nil || !slices.Equal(states["notes"].Headers, []string{"Authorization"}) || states["notes"].Tools != 2 || states["gateway"].SignedIn {
		t.Fatalf("states %+v, %v", states, err)
	}
	if err := MCPSetHeader(context.Background(), dir, server, "X-Other", strings.NewReader("v\n"), &out); err == nil || !strings.Contains(err.Error(), "names no header X-Other") {
		t.Fatalf("an unnamed header: %v", err)
	}
	if err := MCPSetHeader(context.Background(), dir, server, "Authorization", strings.NewReader("\n"), &out); err == nil {
		t.Fatal("an empty value was kept")
	}
	if removed, err := MCPLogout(dir, "notes"); err != nil || !removed {
		t.Fatalf("logout %v %v", removed, err)
	}
	if states, _ := MCPStates(dir, mcpServers(t)); len(states["notes"].Headers) != 0 {
		t.Fatalf("after logout %+v", states["notes"])
	}
}

// A session made before its profile's server listed a new tool gets it
// from the start's pass, after its other tools, and keeps its own.
func TestSetMCPToolsGivesAnOlderSessionTheTools(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() { fake.tools = fake.tools[:1] })
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	limited := &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true, MCP: []string{"notes"}, Tools: []string{"read"}}
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: limited})
	if err != nil {
		t.Fatal(err)
	}
	fake.set(func() {
		fake.tools = append(fake.tools, map[string]any{"name": "write", "inputSchema": map[string]any{"type": "object"}})
	})
	e.ConnectMCP(ctx)
	if changed, err := e.SetMCPTools(ctx, id, "chani"); err != nil || !changed {
		t.Fatalf("changed %v, %v", changed, err)
	}
	tools := offered(t, e, id)
	if tools[0] != "read" || !slices.Contains(tools, "mcp__notes__write") || !slices.Contains(tools, "mcp__notes__search") {
		t.Fatalf("offered %v", tools)
	}
	if changed, err := e.SetMCPTools(ctx, id, "chani"); err != nil || changed {
		t.Fatalf("again: changed %v, %v", changed, err)
	}
}

// mcpNamesOf are the names server's tools are offered as.
func mcpNamesOf(server string, tools ...string) []string {
	var list []mcpTool
	for _, name := range tools {
		list = append(list, mcpTool{Name: name})
	}
	return mcpToolNames([]string{server}, map[string][]mcpTool{server: list})[server]
}

// Tool names are PiG's, mcp__<server>__<tool>, with the server's name
// made _ where it is no letter or digit; a tool whose name needs the
// same, or is too long, takes a digest of both names.
func TestMCPToolNames(t *testing.T) {
	if got := mcpNamesOf("notes-main", "find_note"); !slices.Equal(got, []string{"mcp__notes_main__find_note"}) {
		t.Errorf("got %v", got)
	}
	if got := mcpNamesOf("notes-main", "find.note")[0]; got != mcpDigestName("notes-main", "find.note") {
		t.Errorf("a tool name that is not plain: %s", got)
	}
	long := mcpNamesOf("notes-main", strings.Repeat("x", 80))[0]
	if len(long) != maxMCPToolName || !strings.HasPrefix(long, "mcp__notes_main__xxx") || long != mcpDigestName("notes-main", strings.Repeat("x", 80)) {
		t.Errorf("long %s", long)
	}
}

// A name stays bound to its tool, whatever the order a server lists its
// tools in (Codex review of #126, round 1), and whatever a later listing
// holds: a tool replaced by one whose name reads alike never takes its
// name (round 2).
func TestAToolsNameDoesNotMoveWhenTheListingIsReordered(t *testing.T) {
	first := mcpNamesOf("notes", "a-b", "a_b")
	again := mcpNamesOf("notes", "a_b", "a-b")
	if first[0] != again[1] || first[1] != again[0] || first[0] == first[1] {
		t.Fatalf("names %v, then %v", first, again)
	}
	before := mcpNamesOf("notes", "find.note")
	after := mcpNamesOf("notes", "find_note")
	if before[0] == after[0] {
		t.Fatalf("find.note and then find_note were both %s", before[0])
	}
	// Server names whose parts would run together take digests.
	if a, b := mcpNamesOf("x__", "a")[0], mcpNamesOf("x_", "_a")[0]; a == b {
		t.Fatalf("both %s", a)
	}
}

// The store's file of credentials refuses a link in its place, and one
// that does not parse is an error in pocketd's words, which quote none of
// it.
func TestTheMCPStoreReadsOnlyItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MCPAuthFile), []byte(`{"servers": {"a": {"url": "x", "headers": {"Authorization": "Bearer secret`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openMCPStore(dir); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a broken file: %v", err)
	}
	other := t.TempDir()
	target := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(other, MCPAuthFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := openMCPStore(other); err == nil {
		t.Fatal("a link was read")
	}
}

// A refresh the authorization server never answered is no revocation: the
// call says to try again later, and the owner signs in again only if it
// goes on; pocketd drops nothing it keeps.
func TestARefreshThatDidNotReachTheServerSaysToTryAgain(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	fake.set(func() { fake.refresh, fake.tokenDown = "ref-1", true })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setListing("gateway", gatewayURL, []mcpTool{{Name: "search"}}); err != nil {
		t.Fatal(err)
	}
	e := f.openMCP(fauxModels(toolTurn("mcp__gateway__search", map[string]any{}, "c1"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"gateway"}})
	id, err := e.CreateSession(context.Background(), t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpResult(t, e, id, "search")
	if !got.IsError || !strings.Contains(got.Text, "pocketd could not refresh it now: try again later") {
		t.Fatalf("result %+v", got)
	}
	after, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if state := after.oauthState("gateway", gatewayURL); state == nil || state.Tokens.RefreshToken != "ref-1" {
		t.Fatalf("kept %+v", state)
	}
}

// The MCP store lives in the data folder: the file tools refuse it, as
// they refuse auth.json, and the sandbox hides it from every command
// (ADR-0012, ADR-0063).
func TestTheFileToolsCannotReadTheMCPStore(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	secret := filepath.Join(f.dataDir, MCPAuthFile)
	e := f.openMCP(fauxModels(toolTurn("read", map[string]any{"path": secret}, "r1"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"notes"}})
	id, err := e.CreateSession(context.Background(), t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpResult(t, e, id, "read")
	if !got.IsError || strings.Contains(got.Text, "notes-secret-value") || !strings.Contains(got.Text, "belongs to pocketd") {
		t.Fatalf("result %+v", got)
	}
}

// pngImage is an image a provider takes: a whole PNG of 2x2 pixels, in
// base64.
var pngImage = func() string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}()
