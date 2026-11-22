package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/pig"
)

// mcpFolder is a profile folder with an mcp.json of two servers, and
// chani, whose mcp list names one of them and one mcp.json lacks.
func mcpFolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{
		profiles.MCPFile: `{"mcpServers": {"notes": {"url": "https://mcp.example.com/notes", "headers": {"Authorization": "stored"}},
			"gateway": {"url": "https://mcp.example.com/gateway/mcp", "oauth": {}}}}`,
		"chani.md": "---\nmodel: a/b\nmcp: [notes, mail]\n---\n# Chani\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pocketd mcp set-header, login and logout write the MCP store only while
// pocketd does not serve: its engine is the store's one writer while it
// runs (ADR-0063).
func TestTheMCPStoreIsNotWrittenWhilePocketdServes(t *testing.T) {
	data, dir := t.TempDir(), mcpFolder(t)
	unlock, err := lockData(filepath.Join(data, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"set-header", "notes", "Authorization"}, {"login", "gateway"}, {"logout", "notes"}} {
		var out strings.Builder
		err := mcpCommand(context.Background(), data, dir, args, strings.NewReader("Bearer secret\n"), &out)
		if err == nil || !strings.Contains(err.Error(), "stop pocketd") {
			t.Errorf("%v while pocketd serves: %v", args, err)
		}
	}
	unlock()
	if _, err := os.Stat(filepath.Join(data, pig.MCPAuthFile)); !os.IsNotExist(err) {
		t.Fatalf("the store was written: %v", err)
	}
	var out strings.Builder
	if err := mcpCommand(context.Background(), data, dir, []string{"logout", "notes"}, strings.NewReader(""), &out); err != nil ||
		!strings.Contains(out.String(), "keeps nothing for notes") {
		t.Fatalf("logout while nothing serves: %v %s", err, out.String())
	}
	for _, args := range [][]string{nil, {"list", "x"}, {"login"}, {"set-header", "notes"}, {"frob"}} {
		if err := mcpCommand(context.Background(), data, dir, args, strings.NewReader(""), &out); err != errMCPUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := mcpCommand(context.Background(), data, dir, []string{"login", "mail"}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), `no server "mail"`) {
		t.Errorf("a server mcp.json lacks: %v", err)
	}
}

// pocketd mcp list, and pocketd profiles, say each server, who names it,
// what pocketd keeps for it and what the owner does next, and each name a
// profile lists that mcp.json lacks.
func TestTheMCPReportSaysWhatToDo(t *testing.T) {
	data, dir := t.TempDir(), mcpFolder(t)
	var out strings.Builder
	if err := mcpCommand(context.Background(), data, dir, []string{"list"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"notes: https://mcp.example.com/notes, for chani; no value of Authorization: pocketd mcp set-header notes Authorization; no tools listed yet",
		"gateway: https://mcp.example.com/gateway/mcp, named by no profile: pocketd connects it not; not signed in: pocketd mcp login gateway",
		"chani names mail, which mcp.json does not have",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, out.String())
		}
	}
	var profilesOut strings.Builder
	if err := listProfiles(context.Background(), &profilesOut, data, dir, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profilesOut.String(), "MCP servers (mcp.json") {
		t.Fatalf("pocketd profiles lacks the MCP servers:\n%s", profilesOut.String())
	}
}

// pocketd serve reads the servers and the profiles' lists at start, and
// logs a name mcp.json lacks; a broken mcp.json connects none.
func TestServeReadsTheMCPServersAtStart(t *testing.T) {
	dir := mcpFolder(t)
	folder, err := profiles.Pin(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	servers, lists := mcpAtStart(folder, log)
	if len(servers.Servers) != 2 || !slices.Equal(lists["chani"], []string{"notes", "mail"}) {
		t.Fatalf("servers %+v, lists %v", servers, lists)
	}
	if !strings.Contains(logged.String(), "server=mail") {
		t.Fatalf("the log lacks the missing server:\n%s", logged.String())
	}
	if err := os.WriteFile(filepath.Join(dir, profiles.MCPFile), []byte(`{"mcpServers": {"notes": {"url": "https://mcp.example.com/", "headers": {"Authorization": "Bearer secret"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	if servers, lists := mcpAtStart(folder, log); len(servers.Servers) != 0 || lists != nil {
		t.Fatalf("a broken file connects %+v %v", servers, lists)
	}
	if strings.Contains(logged.String(), "secret") || !strings.Contains(logged.String(), "set-header") {
		t.Fatalf("the log:\n%s", logged.String())
	}
}
