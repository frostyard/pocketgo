// Package profiles reads the agents' profiles (ADR-0013): one Markdown
// file per agent, its frontmatter the agent's settings and its body the
// agent's identity. Every field is checked here, in code: a profile that
// breaks a rule is listed with its problem and starts nothing.
package profiles

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/frostyard/pocketgo/proto"
)

// Role is what an agent does on the team.
type Role string

const (
	RoleSupervisor Role = "supervisor"
	RoleClerk      Role = "clerk"
	RoleAgent      Role = "agent"
)

// Profile is a valid profile.
type Profile struct {
	// ID is the file's name without .md; Name the body's first heading,
	// else the id.
	ID, Name string
	Role     Role
	// Model is the profile's model as written: its provider may be one the
	// router resolves to another (router.Choose).
	Model proto.ModelRef
	// Repositories maps owner/name to the repository's folder.
	Repositories map[string]string
	// Home and Workspace are folders, with ~ expanded.
	Home, Workspace string
	MCP             []string
	Private         bool
	// Tools are the tools the agent may call; nil is every tool.
	Tools []string
	// Identity is the body: the agent's identity prompt.
	Identity string
}

// Problem is a profile file that is not a valid profile.
type Problem struct {
	ID, Err string
}

// idPattern is a profile's id, which is its file's name without .md.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// toolPattern is a tool's name.
var toolPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// mcpPattern is an MCP server's name, as mcp.json keys it.
var mcpPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// repositoryPattern is an owner/name.
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Load reads every profile in dir, by id. Files that are not <id>.md, such
// as backups, are not profiles. home expands a leading ~ in folders.
func Load(dir, home string) ([]Profile, []Problem, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var profiles []Profile
	var problems []Problem
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".md")
		if !ok || !idPattern.MatchString(id) || entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			problems = append(problems, Problem{ID: id, Err: err.Error()})
			continue
		}
		p, err := Parse(id, data, home)
		if err != nil {
			problems = append(problems, Problem{ID: id, Err: err.Error()})
			continue
		}
		profiles = append(profiles, p)
	}
	return profiles, problems, nil
}

// Find is the profile id in dir, or an error that says why there is none.
func Find(dir, home, id string) (Profile, error) {
	if !idPattern.MatchString(id) {
		return Profile{}, fmt.Errorf("%q is not a profile id", id)
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".md"))
	if errors.Is(err, os.ErrNotExist) {
		return Profile{}, fmt.Errorf("no profile %q", id)
	}
	if err != nil {
		return Profile{}, err
	}
	return Parse(id, data, home)
}

// Folder is a profile folder: the default is pi-pocket-team's,
// ~/.config/pi-pocket-team/agents (ADR-0013). Home expands ~ in profiles.
type Folder struct {
	Dir, Home string
}

// Load reads the folder's profiles.
func (f Folder) Load() ([]Profile, []Problem, error) { return Load(f.Dir, f.Home) }

// Find reads the folder's profile id.
func (f Folder) Find(id string) (Profile, error) { return Find(f.Dir, f.Home, id) }

// frontmatter is the profile's settings as written. Values are decoded
// into yaml.Nodes so each one's shape is checked here, with a message that
// names the field.
type frontmatter map[string]yaml.Node

// Parse reads one profile file.
func Parse(id string, data []byte, home string) (Profile, error) {
	if !idPattern.MatchString(id) {
		return Profile{}, fmt.Errorf("%q is not a profile id", id)
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return Profile{}, errors.New("the file does not start with frontmatter (---)")
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		if head, ok = bytes.CutSuffix(rest, []byte("\n---")); !ok {
			return Profile{}, errors.New("the frontmatter does not end (---)")
		}
		body = nil
	}
	var fields frontmatter
	if err := yaml.Unmarshal(head, &fields); err != nil {
		return Profile{}, fmt.Errorf("frontmatter: %w", err)
	}
	p := Profile{ID: id, Name: id, Role: RoleAgent, Identity: strings.TrimSpace(string(body))}
	for line := range strings.SplitSeq(p.Identity, "\n") {
		if name, ok := strings.CutPrefix(line, "# "); ok && strings.TrimSpace(name) != "" {
			p.Name = strings.TrimSpace(name)
			break
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		node := fields[key]
		if err := p.set(key, &node, home); err != nil {
			return Profile{}, fmt.Errorf("%s: %w", key, err)
		}
	}
	if p.Model.Provider == "" {
		return Profile{}, errors.New("model: every profile names its model as provider/id")
	}
	if p.Private && p.Home == "" {
		return Profile{}, errors.New("private: a private agent works only in its home, and this profile has none")
	}
	return p, nil
}

func (p *Profile) set(key string, node *yaml.Node, home string) error {
	switch key {
	case "role":
		var role string
		if err := scalar(node, &role); err != nil {
			return err
		}
		switch Role(role) {
		case RoleSupervisor, RoleClerk, RoleAgent:
			p.Role = Role(role)
		default:
			return fmt.Errorf("%q is not supervisor, clerk or agent", role)
		}
	case "model":
		var model string
		if node.Kind == yaml.SequenceNode {
			// A list names fallbacks; the first is the model.
			if len(node.Content) == 0 {
				return errors.New("the list is empty")
			}
			node = node.Content[0]
		}
		if err := scalar(node, &model); err != nil {
			return err
		}
		provider, id, ok := strings.Cut(model, "/")
		if !ok || provider == "" || id == "" || strings.ContainsAny(model, " \t") {
			return fmt.Errorf("%q is not provider/id", model)
		}
		p.Model = proto.ModelRef{Provider: provider, ID: id}
	case "repositories":
		if node.Kind != yaml.MappingNode {
			return errors.New("not a map of owner/name to a folder")
		}
		p.Repositories = map[string]string{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			var name, dir string
			if err := scalar(node.Content[i], &name); err != nil {
				return err
			}
			if !repositoryPattern.MatchString(name) {
				return fmt.Errorf("%q is not owner/name", name)
			}
			if err := folder(node.Content[i+1], &dir); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			path, err := expand(dir, home)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			p.Repositories[name] = path
		}
	case "home", "workspace":
		var dir string
		if err := folder(node, &dir); err != nil {
			return err
		}
		path, err := expand(dir, home)
		if err != nil {
			return err
		}
		if key == "home" {
			p.Home = path
		} else {
			p.Workspace = path
		}
	case "mcp":
		names, err := names(node, mcpPattern, "an MCP server name")
		if err != nil {
			return err
		}
		p.MCP = names
	case "tools":
		names, err := names(node, toolPattern, "a tool name")
		if err != nil {
			return err
		}
		p.Tools = names
	case "private":
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			return errors.New("not true or false")
		}
		p.Private = node.Value == "true"
	default:
		return errors.New("not a profile field: role, model, repositories, workspace, home, mcp, private or tools")
	}
	return nil
}

// scalar decodes a string value.
func scalar(node *yaml.Node, out *string) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("not text")
	}
	*out = node.Value
	return nil
}

// folder decodes a folder value: text, or a bare ~, which YAML reads as
// null.
func folder(node *yaml.Node, out *string) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" && node.Value == "~" {
		*out = "~"
		return nil
	}
	return scalar(node, out)
}

// names decodes a list of names matching pattern.
func names(node *yaml.Node, pattern *regexp.Regexp, what string) ([]string, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, errors.New("not a list")
	}
	out := []string{}
	for _, item := range node.Content {
		var name string
		if err := scalar(item, &name); err != nil {
			return nil, err
		}
		if !pattern.MatchString(name) {
			return nil, fmt.Errorf("%q is not %s", name, what)
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// expand is folder with a leading ~ made home, cleaned; it must be
// absolute.
func expand(dir, home string) (string, error) {
	if dir == "~" {
		dir = home
	} else if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		dir = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q is not a folder: write it absolute or as ~/…", dir)
	}
	return filepath.Clean(dir), nil
}

// Folders are the folders the agent works in: its home, its workspace and
// its repositories.
func (p Profile) Folders() []string {
	var out []string
	for _, f := range append([]string{p.Home, p.Workspace}, mapValues(p.Repositories)...) {
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func mapValues(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}
