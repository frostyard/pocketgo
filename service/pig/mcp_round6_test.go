package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/uploads"
)

// The guards of the Codex review of #126, round 6.

// A server's error that is JSON shows no secret, whatever its escapes, as
// a result's text does not.
func TestAServersJSONErrorShowsNoSecret(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(`Bearer abc"d&ef1234`)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	c := e.mcp.conns["notes"]
	for _, text := range []string{c.serverText(`{"echo": "abc\"d&ef1234"}`), c.failure(mcp.NewMcpError(-32603, `{"echo": "abc\"d&ef1234"}`, nil))} {
		if strings.Contains(text, `abc\"d&ef1234`) || strings.Contains(text, `abc"d&ef1234`) || strings.Contains(text, "ef1234") {
			t.Fatalf("the error shows the secret: %s", text)
		}
	}
}

// Two connections of one server share one refresh: one made again after a
// drop waits for the refresh the first began, and the token endpoint is
// asked once.
func TestReconnectionsShareOneRefresh(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	hold, arrived := make(chan struct{}), make(chan struct{}, 4)
	fake.set(func() { fake.refresh, fake.next, fake.holdToken, fake.arrived = "ref-1", "acc-2", hold, arrived })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"gateway"}})
	c := e.mcp.conns["gateway"]
	auth := func() *serveAuth {
		provider, err := oauth.NewMcpOAuthProvider(oauth.McpOAuthProviderOptions{ServerURL: gatewayURL, RedirectURL: profiles.DefaultMCPCallback,
			Store: mcpOAuthStore{store: e.mcp.store, name: "gateway", url: gatewayURL}})
		if err != nil {
			t.Fatal(err)
		}
		return &serveAuth{conn: c, provider: refreshOnly{provider}}
	}
	server, _ := url.Parse(gatewayURL)
	refused := mcp.UnauthorizedContext{
		Response: &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {
			`Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/gateway/mcp"`}}},
		ServerURL: server, Fetch: mcpClient(c.server.Timeout), Token: "old-token",
	}
	results := make(chan error, 2)
	go func() { results <- auth().OnUnauthorized(context.Background(), refused) }()
	<-arrived
	go func() { results <- auth().OnUnauthorized(context.Background(), refused) }()
	close(hold)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("a refresh: %v", err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	tokens := 0
	for _, seen := range fake.seen {
		if seen == "POST "+authURL+"/token" {
			tokens++
		}
	}
	if tokens != 1 {
		t.Fatalf("the token endpoint was asked %d times", tokens)
	}
}

// pocketd mcp login keeps a client only for the authorization server that
// registered it: one discovery names another server registers again.
func TestMCPLoginKeepsAClientOnlyForItsIssuer(t *testing.T) {
	fake := newFakeMCP(t)
	dir := t.TempDir()
	server, _ := mcpServers(t).Find("gateway")
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resource, _ := oauth.ParseURL(gatewayURL)
	client := oauth.OAuthClientInformation{ClientID: "client-old"}
	client.RedirectURIs = []string{profiles.DefaultMCPCallback}
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{ServerURL: resource.Href(), ClientInformation: &client,
		Discovery: &oauth.OAuthDiscoveryState{AuthorizationServerURL: "https://old-auth.example.com"}}); err != nil {
		t.Fatal(err)
	}
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
	_ = inW.Close()
	_, _ = io.ReadAll(outR)
	<-done
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 1 || strings.Contains(printed.String(), "client-old") {
		t.Fatalf("registered %d: %s", fake.registered, printed.String())
	}
}

// pocketd mcp's listing ends every exchange it began, and its store
// writes nothing after, before the command lets go of the lock.
func TestACommandsListingEndsItsWorkAndItsWrites(t *testing.T) {
	newFakeMCP(t)
	dir := t.TempDir()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", notesKey); err != nil {
		t.Fatal(err)
	}
	server, _ := mcpServers(t).Find("notes")
	if _, _, err := listOnce(context.Background(), store, server); err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", "Bearer after-the-command"); !errors.Is(err, errMCPStoreFenced) {
		t.Fatalf("a write after the listing: %v", err)
	}
}

// A credentials write that failed is made again at the next read, with no
// second token exchange: a rotated refresh token is never lost to it.
func TestAFailedCredentialsWriteIsMadeAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only folder")
	}
	dir := t.TempDir()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: "a-1", RefreshToken: "rotated-1"}}); err == nil {
		t.Fatal("a write to a read-only folder succeeded")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = store.oauthState("gateway", gatewayURL)
	again, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if state := again.oauthState("gateway", gatewayURL); state == nil || state.Tokens.RefreshToken != "rotated-1" {
		t.Fatalf("kept %+v", state)
	}
}

// An exchange waits for its answer's headers as long as its server's
// timeout says, longer than a model provider's idle limit, and its own
// deadline is the request's, never the connection's.
func TestAServersTimeoutBoundsItsWaitAlone(t *testing.T) {
	if mcpDirectTransport.ResponseHeaderTimeout != 0 {
		t.Fatalf("the header wait is %v", mcpDirectTransport.ResponseHeaderTimeout)
	}
	if client := mcpClient(600_000_000_000); client.Timeout != 600_000_000_000 {
		t.Fatalf("the exchange's bound is %v", client.Timeout)
	}
	deadline := &url.Error{Op: "Post", URL: notesURL, Err: context.DeadlineExceeded}
	if !requestLocal(context.Background(), deadline) {
		t.Fatal("a request's deadline drops the connection")
	}
}

// A JSON document is written again with its numbers as they came: an id
// past a float's precision stays the same id.
func TestRedactionKeepsNumbersAsWritten(t *testing.T) {
	got := string(redactJSON(json.RawMessage(`{"id": 9007199254740993, "name": "notes-secret-value"}`), []string{"notes-secret-value"}))
	if got != `{"id":9007199254740993,"name":"[redacted]"}` {
		t.Fatalf("redacted %s", got)
	}
}

// A kept schema near its bound is offered again as it was: the store
// writes it compact, and its bound is measured compact.
func TestAKeptSchemaNearItsBoundSurvivesAReopen(t *testing.T) {
	properties := map[string]any{}
	for i := 0; ; i++ {
		properties[fmt.Sprintf("field_%04d", i)] = map[string]any{"type": "string"}
		data, _ := json.Marshal(map[string]any{"type": "object", "properties": properties})
		if len(data) > maxMCPSchema-200 {
			break
		}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties})
	if string(normalSchema(schema)) == emptySchema {
		t.Fatal("the schema is past its bound to begin with")
	}
	dir := t.TempDir()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setListing("notes", notesURL, []mcpTool{{Name: "wide", InputSchema: schema}}); err != nil {
		t.Fatal(err)
	}
	again, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := again.listing("notes", notesURL)
	if len(tools) != 1 || string(normalSchema(tools[0].InputSchema)) == emptySchema {
		t.Fatal("a kept schema near its bound came back empty")
	}
}

// A schema whose reference names a part it does not have is offered as an
// object of any properties.
func TestASchemasReferencesMustResolve(t *testing.T) {
	missing := `{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}},"$defs":{"y":{"type":"string"}}}`
	if got := string(normalSchema(json.RawMessage(missing))); got != emptySchema {
		t.Fatalf("kept %s", got)
	}
	found := `{"type":"object","properties":{"x":{"$ref":"#/$defs/y"}},"$defs":{"y":{"type":"string"}}}`
	if got := string(normalSchema(json.RawMessage(found))); got == emptySchema {
		t.Fatal("a schema whose reference resolves was dropped")
	}
}

// No image past the result's bounds is decoded: the fifth and later
// images, and one past its size, never reach the decoder.
func TestNoImagePastTheBoundsIsDecoded(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	decoded := 0
	prepareImage = func(data []byte) (uploads.Image, string, error) {
		decoded++
		return uploads.PrepareImage(data)
	}
	t.Cleanup(func() { prepareImage = uploads.PrepareImage })
	var content []mcp.ContentBlock
	content = append(content, mcp.ContentBlock{Type: "image", Data: strings.Repeat("A", maxMCPImageBytes+4), MimeType: "image/png"})
	for range 10 {
		content = append(content, mcp.ContentBlock{Type: "image", Data: pngImage, MimeType: "image/png"})
	}
	e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: content})
	if decoded != maxMCPImages {
		t.Fatalf("%d images decoded", decoded)
	}
}
