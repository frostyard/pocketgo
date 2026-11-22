package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/pig"
)

// errMCPUsage says how the mcp commands are called.
var errMCPUsage = errors.New("usage: pocketd mcp list | login NAME | set-header NAME HEADER | logout NAME")

// mcpCommand runs pocketd mcp (ADR-0063): list reads and changes nothing,
// so it runs while pocketd serves; login, set-header and logout write the
// MCP store in the data folder, only with the data folder's lock, so the
// serving engine, its one writer while it runs, is stopped.
func mcpCommand(ctx context.Context, data, profileDir string, args []string, in io.Reader, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "list":
		return mcpReport(out, data, profileDir)
	case len(args) == 2 && args[0] == "login", len(args) == 3 && args[0] == "set-header":
		server, err := mcpServer(profileDir, args[1])
		if err != nil {
			return err
		}
		unlock, err := lockStore(data, "sign pocketd in to an MCP server")
		if err != nil {
			return err
		}
		defer unlock()
		if args[0] == "login" {
			return pig.MCPLogin(ctx, data, server, in, out)
		}
		return pig.MCPSetHeader(ctx, data, server, args[2], in, out)
	case len(args) == 2 && args[0] == "logout":
		unlock, err := lockStore(data, "forget an MCP server's credentials")
		if err != nil {
			return err
		}
		defer unlock()
		removed, err := pig.MCPLogout(data, args[1])
		if err != nil {
			return err
		}
		if !removed {
			_, err = fmt.Fprintf(out, "pocketd keeps nothing for %s.\n", args[1])
			return err
		}
		_, err = fmt.Fprintf(out, "Forgot %s's headers and sign-in. pocketd connects it no more until you store them again.\n", args[1])
		return err
	}
	return errMCPUsage
}

// lockStore takes the data folder's lock for a change to the MCP store,
// which pocketd's engine writes while it serves: the change is refused
// while it does.
func lockStore(data, what string) (func(), error) {
	abs, err := filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	unlock, err := lockData(filepath.Join(abs, lockFile))
	if err != nil {
		return nil, fmt.Errorf("stop pocketd before you %s, then start it again: %w", what, err)
	}
	return unlock, nil
}

// mcpServer is the server named name in the profile folder's mcp.json.
func mcpServer(profileDir, name string) (profiles.MCPServer, error) {
	if profileDir == "" {
		return profiles.MCPServer{}, errors.New("profiles are off (-profiles is empty), and mcp.json lives in the profile folder")
	}
	servers, err := profiles.LoadMCP(profileDir)
	if err != nil {
		return profiles.MCPServer{}, err
	}
	server, ok := servers.Find(name)
	if !ok {
		return profiles.MCPServer{}, fmt.Errorf("%s names no server %q", filepath.Join(profileDir, profiles.MCPFile), name)
	}
	return server, nil
}

// mcpReport says what the owner's MCP servers are: each server of mcp.json
// with the profiles that name it, what pocketd keeps for it and the tools
// it listed last, and each name a profile lists that mcp.json lacks, as
// pocketd serve reads them at start (ADR-0063). No secret shows.
func mcpReport(out io.Writer, data, profileDir string) error {
	w, ok := out.(*firstError)
	if !ok {
		w = &firstError{w: out}
	}
	if profileDir == "" {
		_, _ = fmt.Fprintln(w, "\nMCP servers: none: profiles are off.")
		return w.err
	}
	servers, err := profiles.LoadMCP(profileDir)
	if err != nil {
		_, _ = fmt.Fprintf(w, "\nMCP servers: none connect: %v\n", err)
		return w.err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	valid, _, err := profiles.Load(profileDir, home)
	if err != nil {
		_, _ = fmt.Fprintf(w, "\nMCP servers: the profiles do not read: %v\n", err)
		return w.err
	}
	if len(servers.Servers) == 0 && !slices.ContainsFunc(valid, func(p profiles.Profile) bool { return len(p.MCP) > 0 }) {
		_, _ = fmt.Fprintf(w, "\nMCP servers: none (%s)\n", profiles.MCPFile)
		return w.err
	}
	states, err := pig.MCPStates(data, servers)
	if err != nil {
		_, _ = fmt.Fprintf(w, "\nMCP servers: what pocketd keeps does not read: %v\n", err)
		return w.err
	}
	_, _ = fmt.Fprintf(w, "\nMCP servers (%s; pocketd reads it and the profiles' mcp lists when it starts):\n", profiles.MCPFile)
	for _, s := range servers.Servers {
		var names []string
		for _, p := range valid {
			if slices.Contains(p.MCP, s.Name) {
				names = append(names, p.ID)
			}
		}
		named := "named by no profile: pocketd connects it not"
		if len(names) > 0 {
			named = "for " + strings.Join(names, ", ")
		}
		_, _ = fmt.Fprintf(w, "  %s: %s, %s; %s\n", s.Name, s.URL, named, mcpKept(s, states[s.Name]))
	}
	for _, p := range valid {
		for _, name := range p.MCP {
			if _, ok := servers.Find(name); !ok {
				_, _ = fmt.Fprintf(w, "  %s names %s, which %s does not have: its sessions get none of its tools\n", p.ID, name, profiles.MCPFile)
			}
		}
	}
	return w.err
}

// mcpAtStart are the owner's MCP servers and the profiles' mcp lists, by
// profile id, as pocketd serve reads them from the pinned folder at start
// (ADR-0063): an mcp.json that breaks a rule, or profiles that do not
// read, connect no server, and a profile's name that mcp.json lacks is
// logged. Nothing here stops pocketd.
func mcpAtStart(folder profiles.Folder, log *slog.Logger) (profiles.MCPServers, map[string][]string) {
	servers, err := folder.MCP()
	if err != nil {
		log.Error("the MCP servers are not connected", "err", err)
		return profiles.MCPServers{}, nil
	}
	valid, _, err := folder.LoadPinned()
	if err != nil {
		log.Error("the MCP servers are not connected: the profiles do not read", "err", err)
		return profiles.MCPServers{}, nil
	}
	lists := map[string][]string{}
	for _, p := range valid {
		if len(p.MCP) == 0 {
			continue
		}
		lists[p.ID] = slices.Clone(p.MCP)
		for _, name := range p.MCP {
			if _, ok := servers.Find(name); !ok {
				log.Warn("a profile names an MCP server mcp.json does not have", "profile", p.ID, "server", name)
			}
		}
	}
	return servers, lists
}

// mcpKept says what pocketd keeps for server s, and what the owner does
// next, with no secret in it.
func mcpKept(s profiles.MCPServer, state pig.MCPState) string {
	var parts []string
	if len(state.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("no value of %s: pocketd mcp set-header %s %s", strings.Join(state.Missing, ", "), s.Name, state.Missing[0]))
	} else if len(s.Headers) > 0 {
		parts = append(parts, "headers kept: "+strings.Join(state.Headers, ", "))
	}
	if s.OAuth != nil {
		if state.SignedIn {
			parts = append(parts, "signed in")
		} else {
			parts = append(parts, "not signed in: pocketd mcp login "+s.Name)
		}
	}
	if state.Listed {
		parts = append(parts, fmt.Sprintf("%d tools listed last", state.Tools))
	} else {
		parts = append(parts, "no tools listed yet")
	}
	return strings.Join(parts, "; ")
}
