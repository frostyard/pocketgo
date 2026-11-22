package pig

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The guards of the Codex review of #126, round 4.

// bigBody answers every request with n bytes.
type bigBody struct{ n int }

func (b bigBody) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(make([]byte, b.n))), Request: r}, nil
}

// An answer from an MCP server's OAuth side is read within its bound, and
// one past it fails, never cut: PiG reads an OAuth document whole. The
// endpoint's own answers are PiG's to bound, by message.
func TestAnOAuthAnswerIsReadWithinItsBound(t *testing.T) {
	mcpTransport = bigBody{n: 2 * maxMCPDocument}
	t.Cleanup(func() { mcpTransport = nil })
	client := mcpClient(mcpLoginTimeout, notesURL)
	read := func(target string) error {
		resp, err := client.Get(target)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
		return err
	}
	if err := read(authURL + "/.well-known/oauth-authorization-server"); !errors.Is(err, errMCPTooLarge) {
		t.Fatalf("a document past its bound: %v", err)
	}
	if err := read(notesURL); err != nil {
		t.Fatalf("the endpoint's answer: %v", err)
	}
}

// An image goes only when its decoded bytes are of its type and hold no
// secret: two bytes called a PNG, or a PNG that carries a stored secret,
// are left out.
func TestAnImageIsCheckedByItsDecodedBytes(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d}
	leaky := base64.StdEncoding.EncodeToString(append(png, []byte("notes-secret-value")...))
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("hi")), MimeType: "image/png"},
		{Type: "image", Data: leaky, MimeType: "image/png"},
		{Type: "image", Data: pngImage, MimeType: "image/jpeg"},
		{Type: "image", Data: pngImage, MimeType: "image/png"},
	}})
	images := 0
	for _, block := range got.Output {
		if b, ok := block.(ai.ImageContent); ok {
			images++
			if b.Data != pngImage {
				t.Fatalf("an image went: %s", b.Data)
			}
		}
	}
	if images != 1 {
		t.Fatalf("%d images went", images)
	}
}

// A tool list with no tools array keeps the last listing: the server
// answered no list, not an empty one.
func TestAListWithNoToolsArrayKeepsTheLastListing(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	if len(e.mcp.extension("chani").Tools) != 2 {
		t.Fatal("no tools")
	}
	fake.set(func() { fake.noList = true })
	e.ConnectMCP(ctx)
	if got := len(e.mcp.extension("chani").Tools); got != 2 {
		t.Fatalf("a list with no tools array left %d tools", got)
	}
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if tools, _ := store.listing("notes", notesURL); len(tools) != 2 {
		t.Fatalf("kept %v", tools)
	}
}

// A listing on a live connection that the server refuses alone (a 429)
// keeps the connection, which calls of every session may be using.
func TestARefusedListingKeepsTheSharedConnection(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	c := e.mcp.conns["notes"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	fake.set(func() { fake.listBusy = true })
	if _, err := c.connect(ctx, true); err == nil {
		t.Fatal("a refused listing succeeded")
	}
	c.mu.Lock()
	after := c.client
	c.mu.Unlock()
	if before == nil || after != before {
		t.Fatal("a refused listing closed the shared connection")
	}
}

// An authorization server that cannot answer now (temporarily_unavailable)
// is no refusal of pocketd's sign-in: the server is tried again, and the
// owner is told to wait, not to sign in again.
func TestATransientOAuthRefusalIsTriedAgain(t *testing.T) {
	fake := newFakeMCP(t)
	timers := &fakeAfter{}
	mcpAfter = timers.after
	t.Cleanup(func() { mcpAfter = defaultMCPAfter })
	f := newFixture(t)
	fake.set(func() { fake.tokenError = "temporarily_unavailable" })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"gateway"}})
	e.ConnectMCP(context.Background())
	timers.mu.Lock()
	scheduled := len(timers.runs)
	timers.mu.Unlock()
	if scheduled != 1 {
		t.Fatalf("%d retries scheduled", scheduled)
	}
	text := e.mcp.conns["gateway"].failure(&oauth.OAuthError{Code: "temporarily_unavailable"})
	if !strings.Contains(text, "try again later") || strings.Contains(text, "pocketd mcp login") {
		t.Fatalf("the failure says %q", text)
	}
}

// pocketd mcp login registers a client again when the one it kept was
// registered for fewer scopes than it asks for now.
func TestMCPLoginRegistersAgainForMoreScopes(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.scopes = []string{"notes:read"} })
	dir := t.TempDir()
	servers, err := profiles.ParseMCP([]byte(`{"mcpServers": {"gateway": {"url": "` + gatewayURL + `", "oauth": {"scope": "admin"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	server, _ := servers.Find("gateway")
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resource, _ := oauth.ParseURL(gatewayURL)
	client := oauth.OAuthClientInformation{ClientID: "client-old"}
	client.RedirectURIs, client.Scope = []string{profiles.DefaultMCPCallback}, "notes:read"
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{ServerURL: resource.Href(), ClientInformation: &client}); err != nil {
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
	registered := fake.registered
	fake.mu.Unlock()
	if registered != 1 || strings.Contains(printed.String(), "client_id=client-old") {
		t.Fatalf("registered %d: %s", registered, printed.String())
	}
	for line := range strings.SplitSeq(printed.String(), "\n") {
		if u, err := url.Parse(strings.TrimSpace(line)); err == nil && strings.HasPrefix(strings.TrimSpace(line), authURL) {
			if got := u.Query().Get("scope"); got != "notes:read admin" {
				t.Fatalf("scope %q", got)
			}
		}
	}
}

// The credentials an exchange went out with stay redacted until it
// ends, however many the store replaces meanwhile.
func TestAPinnedSecretOutlivesItsRotations(t *testing.T) {
	store, err := openMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", "Bearer pinned-value-1"); err != nil {
		t.Fatal(err)
	}
	unpin := store.pin()
	for i := range maxFormerSecrets + 10 {
		if err := store.setHeader("notes", notesURL, "Authorization", "Bearer rotated-value-"+strings.Repeat("x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if got := redact("it is pinned-value-1", store.secrets()); got != "it is [redacted]" {
		t.Fatalf("redacted %q", got)
	}
	unpin()
}

// Closing the hub waits for a connection being made, and takes it not:
// no connection is left open, and no listing written, after close.
func TestClosingWaitsForADialAndKeepsNothingOfIt(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	hold, arrived := make(chan struct{}), make(chan struct{}, 1)
	fake.set(func() { fake.hold, fake.arrived = hold, arrived })
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	c := e.mcp.conns["notes"]
	connected := make(chan error, 1)
	go func() {
		_, err := c.connect(context.Background(), false)
		connected <- err
	}()
	<-arrived
	closed := make(chan struct{})
	go func() {
		e.mcp.close(context.Background())
		close(closed)
	}()
	close(hold)
	<-closed
	if err := <-connected; err == nil {
		t.Fatal("a dial a close cut short connected")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		t.Fatal("a connection outlived the close")
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, MCPToolsFile)); !os.IsNotExist(err) {
		t.Fatalf("a listing was written: %v", err)
	}
}
