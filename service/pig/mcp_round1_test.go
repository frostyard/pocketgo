package pig

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"

	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The guards of the Codex review of #126, round 1.

// A server that echoes the token alone, without its scheme, shows none of
// it: the credential of a stored header is a secret of its own. A value
// whose credential is too short to keep out of a text is refused, and a
// server's refusal is redacted before it is cut, so a cut cannot leave a
// secret's start.
func TestTheTokenAloneIsRedactedToo(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(toolTurn("mcp__notes__echo_token", map[string]any{}, "c1"), answer("ok")),
		mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	fake := mcpTransport.(*fakeMCP)
	fake.set(func() {
		fake.tools = append(fake.tools, map[string]any{"name": "echo_token", "inputSchema": map[string]any{"type": "object"}})
	})
	e.ConnectMCP(ctx)
	if _, err := e.SetMCPTools(ctx, id, "chani"); err != nil {
		t.Fatal(err)
	}
	if got := mcpResult(t, e, id, "echo"); strings.Contains(got.Text, "notes-secret-value") || got.Text != "your token is [redacted]" {
		t.Fatalf("result %+v", got)
	}
	if err := headerValue("Bearer abc"); err == nil {
		t.Fatal("a credential of 3 characters was taken")
	}
	c := e.mcp.conns["notes"]
	text := c.serverText(strings.Repeat("x", maxMCPServerText-5) + "notes-secret-value")
	if strings.Contains(text, "notes") {
		t.Fatalf("the cut left a secret's start: %q", text[len(text)-20:])
	}
}

// A tool listing that holds a stored secret is neither kept nor offered
// with it: a name that holds one leaves its tool out, a description has it
// redacted, and a schema that holds one is offered as an object of any
// properties.
func TestAListingThatHoldsASecretIsNotKeptWithIt(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() {
		fake.tools = []map[string]any{
			{"name": "leaky", "description": "Uses notes-secret-value to call.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string", "default": "notes-secret-value"}}}},
			{"name": "notes-secret-value", "inputSchema": map[string]any{"type": "object"}},
		}
	})
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef, Profile: chani})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := e.conversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := conversation.Agent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mcpTools []string
	for _, tool := range agent.Tools {
		if strings.HasPrefix(tool.Name, mcpToolPrefix) {
			mcpTools = append(mcpTools, tool.Name)
			data, _ := json.Marshal(tool.ToolSchema)
			if strings.Contains(string(data), "notes-secret-value") {
				t.Fatalf("offered %s", data)
			}
		}
	}
	if !slices.Equal(mcpTools, []string{"mcp__notes__leaky"}) {
		t.Fatalf("offered %v", mcpTools)
	}
	kept, err := os.ReadFile(filepath.Join(f.dataDir, MCPToolsFile))
	if err != nil || strings.Contains(string(kept), "notes-secret-value") {
		t.Fatalf("kept %s, %v", kept, err)
	}
}

// Every request pocketd sends a server, the notification after its
// initialize included, has a deadline: a server that stalls holds nothing
// past its timeout.
func TestEveryExchangeWithAServerHasADeadline(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() { fake.refresh, fake.next = "ref-1", "acc-2" })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes", "gateway"}})
	e.ConnectMCP(context.Background())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.seen) < 6 {
		t.Fatalf("too few requests: %v", fake.seen)
	}
	for i, has := range fake.deadlines {
		if !has {
			t.Errorf("%s had no deadline", fake.seen[i])
		}
	}
	if !slices.Contains(fake.seen, "POST "+authURL+"/token") {
		t.Fatalf("no refresh: %v", fake.seen)
	}
}

// A listing of a tool listed twice, with a long name, offers it once,
// keeps it once, and a kept listing that holds it twice never keeps
// pocketd from starting.
func TestAListingAnExtensionCouldNotTakeKeepsPocketdStarting(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	long := strings.Repeat("y", 80)
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setListing("notes", notesURL, []mcpTool{{Name: long}, {Name: long}}); err != nil {
		t.Fatal(err)
	}
	fake.set(func() {
		fake.tools = []map[string]any{{"name": long, "inputSchema": map[string]any{"type": "object"}}, {"name": long, "inputSchema": map[string]any{"type": "object"}}}
	})
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	if got := len(e.mcp.extension("chani").Tools); got != 1 {
		t.Fatalf("offered %d tools from the kept listing", got)
	}
	e.ConnectMCP(context.Background())
	if got := len(e.mcp.extension("chani").Tools); got != 1 {
		t.Fatalf("offered %d tools", got)
	}
	after, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if tools, _ := after.listing("notes", notesURL); len(tools) != 1 {
		t.Fatalf("kept %v", tools)
	}
}

// A call that is stopped ends alone: the connection, which every session
// of the server shares, stays.
func TestAStoppedCallKeepsTheSharedConnection(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	c := e.mcp.conns["notes"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	if before == nil {
		t.Fatal("not connected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := c.call(ctx, "search", map[string]any{})
	if got.IsError == nil || !*got.IsError {
		t.Fatalf("result %+v", got)
	}
	c.mu.Lock()
	after := c.client
	c.mu.Unlock()
	if after != before || after.ConnectionState() != mcp.ClientStateConnected {
		t.Fatal("a stopped call closed the shared connection")
	}
}

// fakeAfter holds the retries pocketd schedules, to run them by hand.
type fakeAfter struct {
	mu    sync.Mutex
	waits []time.Duration
	runs  []func()
}

func (a *fakeAfter) after(d time.Duration, f func()) func() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.waits, a.runs = append(a.waits, d), append(a.runs, f)
	return func() bool { return true }
}

func (a *fakeAfter) next(t *testing.T) func() {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.runs) == 0 {
		t.Fatal("no retry is scheduled")
	}
	f := a.runs[0]
	a.runs = a.runs[1:]
	return f
}

// A server that was down when pocketd started, and never listed a tool,
// is tried again later, waiting longer each time; once it answers, its
// tools are offered and its profile's sessions are told, with no restart.
func TestAServerDownAtStartIsTriedAgain(t *testing.T) {
	fake := newFakeMCP(t)
	timers := &fakeAfter{}
	mcpAfter = timers.after
	t.Cleanup(func() { mcpAfter = defaultMCPAfter })
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() { fake.down = true })
	var mu sync.Mutex
	var told []string
	e, err := open(context.Background(), Options{
		DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve, NeedsApproval: func(string) bool { return false },
		Unsandboxed: true, MCP: mcpServers(t), MCPProfiles: map[string][]string{"chani": {"notes"}},
		OnMCPTools: func(profile string) { mu.Lock(); told = append(told, profile); mu.Unlock() },
	}, fauxModels())
	if err != nil {
		t.Fatal(err)
	}
	e.Resume()
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	e.ConnectMCP(context.Background())
	if len(e.mcp.extension("chani").Tools) != 0 {
		t.Fatal("tools with none listed")
	}
	timers.next(t)()
	timers.mu.Lock()
	waits := slices.Clone(timers.waits)
	timers.mu.Unlock()
	if !slices.Equal(waits, []time.Duration{mcpRetryFirst, 2 * mcpRetryFirst}) {
		t.Fatalf("waits %v", waits)
	}
	fake.set(func() { fake.down = false })
	timers.next(t)()
	if got := len(e.mcp.extension("chani").Tools); got != 2 {
		t.Fatalf("offered %d tools", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(told, []string{"chani"}) {
		t.Fatalf("told %v", told)
	}
}

// pocketd mcp login sends the headers mcp.json names with the probe of
// the server alone, never to the authorization server, and asks for the
// scopes the server's metadata names as well as the ones mcp.json adds.
func TestMCPLoginSendsTheHeadersToTheProbeAndKeepsTheServersScopes(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.tenant, fake.scopes = "tenant-value-1", []string{"notes:read"} })
	dir := t.TempDir()
	servers, err := profiles.ParseMCP([]byte(`{"mcpServers": {"gateway": {"url": "` + gatewayURL + `", "headers": {"X-Tenant": "stored"}, "oauth": {"scope": "offline_access"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	server, _ := servers.Find("gateway")
	var out strings.Builder
	if err := MCPLogin(context.Background(), dir, server, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "pocketd mcp set-header gateway X-Tenant") {
		t.Fatalf("a login with no header: %v", err)
	}
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("gateway", gatewayURL, "X-Tenant", "tenant-value-1"); err != nil {
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
	var page string
	for line := range strings.SplitSeq(printed.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), authURL+"/authorize") {
			page = strings.TrimSpace(line)
		}
	}
	authorize, err := url.Parse(page)
	if err != nil || page == "" {
		t.Fatalf("no page in %q", printed.String())
	}
	if got := authorize.Query().Get("scope"); got != "notes:read offline_access" {
		t.Fatalf("scope %q", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	probed := false
	for i, seen := range fake.seen {
		tenant := fake.headers[i].Get("X-Tenant")
		switch {
		case seen == "POST "+gatewayURL:
			probed = probed || tenant == "tenant-value-1"
		case strings.Contains(seen, "auth.example.com") && tenant != "":
			t.Errorf("%s carried the header", seen)
		}
	}
	if !probed {
		t.Fatalf("no probe carried the header: %v", fake.seen)
	}
}

// A schema pocketd keeps is an object schema within its bound: one of
// another type, or past the bound, is kept and offered as an object of any
// properties, never as the server sent it.
func TestASchemaIsNormalizedBeforeItIsKept(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	huge := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string", "description": strings.Repeat("z", maxMCPSchema)}}}
	fake.set(func() {
		fake.tools = []map[string]any{
			{"name": "stringy", "inputSchema": map[string]any{"type": "string"}},
			{"name": "huge", "inputSchema": huge},
			{"name": "badprops", "inputSchema": map[string]any{"type": "object", "properties": []string{"x"}}},
		}
	})
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	store, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := store.listing("notes", notesURL)
	if len(tools) != 3 {
		t.Fatalf("kept %v", tools)
	}
	for _, tool := range tools {
		var compact bytes.Buffer
		if err := json.Compact(&compact, tool.InputSchema); err != nil {
			t.Fatal(err)
		}
		if compact.String() != emptySchema {
			t.Errorf("%s kept %.80s", tool.Name, compact.String())
		}
	}
}

// A result's notices count in its bound: many images left out are one
// notice, and a cut text's mark fits within the bound.
func TestAResultsNoticesCountInItsBound(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	c := e.mcp.conns["notes"]
	fake.set(func() { fake.images = 500 })
	got := c.call(context.Background(), "pictures", map[string]any{})
	images, texts, size := 0, 0, 0
	for _, block := range got.Output {
		switch b := block.(type) {
		case ai.ImageContent:
			images++
		case ai.TextContent:
			texts++
			size += len(b.Text)
		}
	}
	if images != maxMCPImages || texts != 2 || size > maxMCPOutput {
		t.Fatalf("%d images, %d texts, %d bytes", images, texts, size)
	}
	fake.set(func() { fake.big = 3 * maxMCPOutput; fake.images = 0 })
	got = c.call(context.Background(), "big", map[string]any{})
	size = 0
	for _, block := range got.Output {
		if b, ok := block.(ai.TextContent); ok {
			size += len(b.Text)
		}
	}
	if size > maxMCPOutput || size < maxMCPOutput/2 {
		t.Fatalf("a big result is %d bytes", size)
	}
}
