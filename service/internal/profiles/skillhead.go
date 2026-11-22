package profiles

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// MaxSkillHead bounds what pocketd reads of a SKILL.md to list it in a
// session's instructions (ADR-0043): its frontmatter ends within it.
const MaxSkillHead = 8 << 10

// MaxSkillDescription bounds a listed skill's description, in characters,
// as the Agent Skills format does.
const MaxSkillDescription = 1024

// ParseSkillHead is the description of skill name, the folder its SKILL.md
// is in, from head, the file's first bytes as read: at most MaxSkillHead+1,
// so a longer read says the file goes on past them (ADR-0043). The file
// starts with YAML frontmatter between --- lines, one map whose name is the
// folder's and whose description is text that is not empty; other keys
// are the skill's own. The description comes on one line, its spaces and
// line breaks made single spaces, at most MaxSkillDescription characters.
// An error says what is wrong, for the note that leaves the skill out.
func ParseSkillHead(name string, head []byte) (string, error) {
	if err := CheckSkillName(name); err != nil {
		return "", err
	}
	more := len(head) > MaxSkillHead
	if more {
		head = head[:MaxSkillHead]
	}
	head = bytes.ReplaceAll(head, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(head, []byte("---\n"))
	if !ok {
		return "", errors.New("it does not start with frontmatter (---)")
	}
	front, _, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		front, ok = bytes.CutSuffix(rest, []byte("\n---"))
		if !ok || more {
			if more {
				return "", fmt.Errorf("its frontmatter does not end (---) within its first %d bytes", MaxSkillHead)
			}
			return "", errors.New("its frontmatter does not end (---)")
		}
	}
	if !utf8.Valid(front) {
		return "", errors.New("its frontmatter is not UTF-8")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(front))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("its frontmatter is no YAML: %v", err)
	}
	var again yaml.Node
	if err := decoder.Decode(&again); !errors.Is(err, io.EOF) {
		return "", errors.New("its frontmatter holds more than one YAML document")
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("its frontmatter is not a map of fields")
	}
	fields := root.Content[0].Content
	var named, description string
	seen := map[string]bool{}
	for i := 0; i+1 < len(fields); i += 2 {
		key := fields[i].Value
		if key != "name" && key != "description" {
			continue
		}
		if seen[key] {
			return "", fmt.Errorf("its frontmatter names %s twice", key)
		}
		seen[key] = true
		out := &named
		if key == "description" {
			out = &description
		}
		if err := scalar(fields[i+1], out); err != nil {
			return "", fmt.Errorf("its %s is %w", key, err)
		}
	}
	switch {
	case named == "":
		return "", errors.New("its frontmatter has no name")
	case named != name:
		return "", fmt.Errorf("its name %q is not its folder's, %q", named, name)
	}
	description = strings.Join(strings.Fields(description), " ")
	switch {
	case description == "":
		return "", errors.New("its frontmatter has no description")
	case utf8.RuneCountInString(description) > MaxSkillDescription:
		return "", fmt.Errorf("its description is longer than %d characters", MaxSkillDescription)
	}
	return description, nil
}
