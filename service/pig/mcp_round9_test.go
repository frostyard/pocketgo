package pig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/api"
)

// The guards of the Codex review of #126, round 9.

// An exchange keeps redacted every token the store held while it was
// under way: a retry's token, which a refresh gave after the exchange
// began, stays redacted however many refreshes replace it before the
// answer comes.
func TestAnExchangeKeepsEveryTokenOfItsRetriesRedacted(t *testing.T) {
	store, err := openMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	save := func(access string) {
		t.Helper()
		if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: access}}); err != nil {
			t.Fatal(err)
		}
	}
	save("access-before-1")
	unpin := store.pin()
	save("access-retried-1")
	for i := range 2 * maxFormerSecrets {
		save(fmt.Sprintf("access-later-%04d", i))
	}
	if !slices.Contains(store.secrets(), "access-retried-1") {
		t.Fatal("the retry's token is no longer redacted while its exchange is under way")
	}
	unpin()
}

// A connection is made under the hub's context with no deadline of its
// own: each request is bounded by the server's timeout alone, and the
// hub's close ends it.
func TestASetupHasNoSharedDeadline(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx, cancel := e.mcp.conns["notes"].setupContext()
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("a setup has the deadline %v", deadline)
	}
	e.mcp.cancel()
	<-ctx.Done()
}

// A result PiG's default limits would cut reaches the transcript whole.
func TestAResultPastPiGsDefaultReachesTheTranscriptWhole(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	size := 55 * 1024
	fake.set(func() {
		fake.big = size
		fake.tools = append(fake.tools, map[string]any{"name": "big", "inputSchema": map[string]any{"type": "object"}})
	})
	e := f.openMCP(fauxModels(toolTurn("mcp__notes__big", map[string]any{}, "c1"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	if got := mcpResult(t, e, id, "big"); got.IsError || got.Text != strings.Repeat("x", size) {
		t.Fatalf("the result is %d bytes, error %v", len(got.Text), got.IsError)
	}
}

// A result's text blocks are one text whose newlines count in its bound,
// as a provider joins them; an empty one adds nothing.
func TestTextBlockSeparatorsCountInTheBound(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	var content []mcp.ContentBlock
	for range 40_000 {
		content = append(content, mcp.ContentBlock{Type: "text", Text: "x"}, mcp.ContentBlock{Type: "text", Text: ""})
	}
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: content})
	size, texts := 0, 0
	for _, block := range got.Output {
		if b, ok := block.(ai.TextContent); ok {
			size += len(b.Text)
			texts++
		}
	}
	// As a provider joins the blocks: one newline between each two.
	if joined := size + max(0, texts-1); joined > maxMCPOutput {
		t.Fatalf("%d text blocks join to %d bytes", texts, joined)
	}
}

// A schema's type list names each type once.
func TestATypeListNamesEachOnce(t *testing.T) {
	twice := `{"type":"object","properties":{"a":{"type":["string","string"]}}}`
	if got := string(normalSchema(json.RawMessage(twice))); got != emptySchema {
		t.Fatalf("kept %s", got)
	}
	once := `{"type":"object","properties":{"a":{"type":["string","null"]}}}`
	if got := string(normalSchema(json.RawMessage(once))); got == emptySchema {
		t.Fatal("a schema with a list of distinct types was dropped")
	}
}

// A call refused for more scope says the scope the server asked for, as
// pocketd may say it: scope tokens alone, few and short, no secret.
func TestAStepUpSaysTheScopeItAsks(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	fake.set(func() { fake.access["good-token"] = true })
	seedSignIn(t, f.dataDir, "good-token", "ref-1")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"gateway"}})
	e.ConnectMCP(context.Background())
	got := e.mcp.conns["gateway"].call(context.Background(), "admin", map[string]any{})
	if text := got.Output[0].(ai.TextContent).Text; !strings.Contains(text, "which asks for admin") {
		t.Fatalf("the refusal says %s", text)
	}
	for scope, want := range map[string]string{
		"notes:read  admin":     "notes:read admin",
		"good-token":            "",
		`a"b`:                   "",
		strings.Repeat("s", 65): "",
		strings.Repeat("a ", maxChallengeScopes+1): "",
	} {
		if got := challengeScope(scope, []string{"good-token"}); got != want {
			t.Errorf("%q: %q, want %q", scope, got, want)
		}
	}
}

// pocketd mcp logout forgets a server's credentials when the listings
// file cannot be read, and leaves that file as it is.
func TestMCPLogoutNeedsNoListings(t *testing.T) {
	dir := t.TempDir()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", notesKey); err != nil {
		t.Fatal(err)
	}
	listings := filepath.Join(dir, MCPToolsFile)
	if err := os.WriteFile(listings, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := MCPLogout(dir, "notes"); err != nil || !removed {
		t.Fatalf("logout: %v, %v", removed, err)
	}
	again, err := openMCPCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if kept, _ := again.headers("notes", notesURL, []string{"Authorization"}); len(kept) != 0 {
		t.Fatal("the header is still kept")
	}
	if data, _ := os.ReadFile(listings); string(data) != "not json" {
		t.Fatalf("the listings file is %q", data)
	}
	if err := again.setListing("notes", notesURL, nil); err == nil {
		t.Fatal("a store that did not read the listings wrote one")
	}
}
