package pig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/api"
)

// The guards of the Codex review of #126, round 2.

// A schema that spells a stored secret with JSON's escapes, which a model
// reads as the secret, is offered with none.
func TestASchemaThatSpellsASecretWithEscapesIsOfferedWithNone(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() {
		fake.tools = []map[string]any{{"name": "escaped",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"k":{"type":"string","default":"notes-` + "\\u" + `0073ecret-value"}}}`)}}
	})
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	tools := e.mcp.extension("chani").Tools
	if len(tools) != 1 {
		t.Fatalf("offered %d tools", len(tools))
	}
	data, err := json.Marshal(tools[0].ToolSchema)
	if err != nil || strings.Contains(string(data), "notes-secret-value") {
		t.Fatalf("offered %s, %v", data, err)
	}
}

// A structured result is redacted before PiG renders it as JSON: a
// secret with a quote or a backslash, which JSON escapes, shows in no
// spelling.
func TestAStructuredResultShowsNoSecret(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	const value = `Bearer abc"def\ghi1234`
	f.keepHeader(value)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	structured, _ := json.Marshal(map[string]any{"echo": value, "token": `abc"def\ghi1234`})
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{StructuredContent: structured})
	for _, block := range got.Output {
		text, _ := block.(ai.TextContent)
		for _, spelling := range []string{`abc"def\ghi1234`, `abc\"def\\ghi1234`} {
			if strings.Contains(text.Text, spelling) {
				t.Fatalf("the result shows %q: %s", spelling, text.Text)
			}
		}
	}
}

// A call the server refuses with an HTTP status of its own (429 here)
// ends alone: the shared connection stays.
func TestARefusedCallKeepsTheSharedConnection(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	c := e.mcp.conns["notes"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	got := c.call(context.Background(), "busy", map[string]any{})
	if got.IsError == nil || !*got.IsError || !strings.Contains(got.Output[0].(ai.TextContent).Text, "answered HTTP 429") {
		t.Fatalf("result %+v", got)
	}
	c.mu.Lock()
	after := c.client
	c.mu.Unlock()
	if before == nil || after != before {
		t.Fatal("a refused call closed the shared connection")
	}
}

// A call waits for a connection being made only until its own context
// ends: it stops waiting at once, and the connection is made all the same
// for the others.
func TestACallStopsWaitingForAConnectionBeingMade(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.connect(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped wait: %v", err)
	}
	close(hold)
	if err := <-connected; err != nil {
		t.Fatalf("the connection: %v", err)
	}
}

// A server that refuses the stored header at start is not tried again:
// the owner stores another, with pocketd stopped.
func TestARefusedHeaderIsNotTriedAgain(t *testing.T) {
	newFakeMCP(t)
	timers := &fakeAfter{}
	mcpAfter = timers.after
	t.Cleanup(func() { mcpAfter = defaultMCPAfter })
	f := newFixture(t)
	f.keepHeader("Bearer a-wrong-value")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	timers.mu.Lock()
	defer timers.mu.Unlock()
	if len(timers.runs) != 0 {
		t.Fatalf("a refused header is tried again after %v", timers.waits)
	}
}

// An OpenID token kept with a sign-in is a secret too.
func TestAnOpenIDTokenIsASecret(t *testing.T) {
	store, err := openMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: "access-token-1", IDToken: "id-token-value-1"}}); err != nil {
		t.Fatal(err)
	}
	if got := redact("it is id-token-value-1", store.secrets()); got != "it is [redacted]" {
		t.Fatalf("redacted %q", got)
	}
}

// A session is offered at most maxSessionTools tools, its own first: a
// profile's servers list at most maxProfileMCPTools of them.
func TestASessionsToolsFitTheProvidersLimit(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() {
		fake.tools = nil
		for i := range 200 {
			fake.tools = append(fake.tools, map[string]any{"name": fmt.Sprintf("tool_%d", i), "inputSchema": map[string]any{"type": "object"}})
		}
	})
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	if got := len(e.mcp.extension("chani").Tools); got != maxProfileMCPTools {
		t.Fatalf("the extension holds %d tools", got)
	}
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	tools := offered(t, e, id)
	if len(tools) > maxSessionTools || tools[0] != "read" {
		t.Fatalf("offered %d tools, first %s", len(tools), tools[0])
	}
}

// A tool with no description is described within the bound, whatever its
// name's length.
func TestAToolsMadeDescriptionIsBounded(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	reg := e.mcp.conns["notes"].registration("mcp__notes__x", mcpTool{Name: strings.Repeat("n", 5000)})
	if len(reg.Description) > maxMCPDescription {
		t.Fatalf("a description of %d bytes", len(reg.Description))
	}
}

// A listing whose write failed is written by the next listing of the same
// tools, and pocketd mcp says it was not kept.
func TestAListingWhoseWriteFailedIsWrittenAgain(t *testing.T) {
	newFakeMCP(t)
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only folder")
	}
	dir := t.TempDir()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", notesKey); err != nil {
		t.Fatal(err)
	}
	server, _ := mcpServers(t).Find("notes")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	n, unkept, err := listOnce(context.Background(), store, server)
	if err != nil || n != 2 || unkept == nil {
		t.Fatalf("listed %d, unkept %v, %v", n, unkept, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &mcpHub{store: store, log: discardLog(), conns: map[string]*mcpConn{"notes": {server: server}}, servers: map[string][]string{},
		tools: map[string][]mcpTool{}, unkept: map[string]error{}}
	h.listed("notes", []mcpTool{{Name: "search"}})
	h.unkept["notes"] = errors.New("a write that failed")
	h.listed("notes", []mcpTool{{Name: "search"}})
	if h.keptErr("notes") != nil {
		t.Fatalf("not kept: %v", h.keptErr("notes"))
	}
	if tools, ok := store.listing("notes", notesURL); !ok || len(tools) != 1 {
		t.Fatalf("kept %v", tools)
	}
}

// The mark of a cut text fits within the bound with the notice of the
// images left out, whichever text block the cut falls in.
func TestTheCutMarkFitsWithTheImagesNotice(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	c := e.mcp.conns["notes"]
	content := []mcp.ContentBlock{{Type: "text", Text: strings.Repeat("a", maxMCPOutput-200)}, {Type: "text", Text: strings.Repeat("b", 1000)}}
	for range 10 {
		content = append(content, mcp.ContentBlock{Type: "image", Data: "aGk=", MimeType: "image/png"})
	}
	got := c.result(&mcp.CallToolResult{Content: content})
	size := 0
	for _, block := range got.Output {
		if text, ok := block.(ai.TextContent); ok {
			size += len(text.Text)
		}
	}
	if size > maxMCPOutput {
		t.Fatalf("%d bytes of text", size)
	}
}

// discardLog logs nothing.
func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
