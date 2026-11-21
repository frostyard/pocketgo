package profiles

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"unicode/utf8"
)

// SkillFile is a skill's text in its folder of the skills folder, where the
// owner's configuration puts it for the service user:
// ~/.pi/agent/skills/<name>/SKILL.md (ADR-0033).
const SkillFile = "SKILL.md"

// MaxSkill bounds a skill's text: it goes whole into each asked agent's
// brief.
const MaxSkill = 64 << 10

// MaxGatherNote bounds a gather's note to the agents it asks.
const MaxGatherNote = 2000

// skillName is a skill's name: its folder in the skills folder.
var skillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// CheckSkillName refuses a name that is no skill's folder name.
func CheckSkillName(name string) error {
	if !skillName.MatchString(name) {
		return fmt.Errorf("%q is no skill's name: 1 to 64 letters, digits, dashes and underscores", name)
	}
	return nil
}

// Skill is a skill's text, by its name.
type Skill struct {
	Name, Text string
}

// SkillPath is where skill name is in the skills folder dir:
// <dir>/<name>/SKILL.md. A name that is no skill's is an error, and so is
// no folder, with ErrNoSkill.
func SkillPath(dir, name string) (string, error) {
	if err := CheckSkillName(name); err != nil {
		return "", err
	}
	if dir == "" {
		return "", fmt.Errorf("%w: pocketd has no skills folder", ErrNoSkill)
	}
	return filepath.Join(dir, name, SkillFile), nil
}

// ParseSkill is skill name from its text as read, at most MaxSkill+1
// bytes: at most MaxSkill bytes of UTF-8, not empty.
func ParseSkill(name string, data []byte) (Skill, error) {
	switch {
	case len(data) > MaxSkill:
		return Skill{}, fmt.Errorf("the skill %s is larger than %d bytes", name, MaxSkill)
	case !utf8.Valid(data):
		return Skill{}, fmt.Errorf("the skill %s is not UTF-8", name)
	case len(data) == 0:
		return Skill{}, fmt.Errorf("the skill %s is empty", name)
	}
	return Skill{Name: name, Text: string(data)}, nil
}

// LoadSkill reads skill name in the skills folder dir (SkillPath) with the
// caller's own rights, as pocketd profiles reports a schedule's skill: a
// regular file once its links are followed, as the owner's configuration
// may link it (ParseSkill). It opens without waiting for a writer, so a
// named pipe is refused at once (Codex review of #57, round 1). A skill
// that is not there is an error that says so, with ErrNoSkill. The service
// reads a gather's skill as its agents' sandbox shows it instead, never
// with its own rights.
func LoadSkill(dir, name string) (Skill, error) {
	path, err := SkillPath(dir, name)
	if err != nil {
		return Skill{}, err
	}
	f, err := openFollow(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Skill{}, fmt.Errorf("%w: there is no %s", ErrNoSkill, path)
	} else if err != nil {
		return Skill{}, fmt.Errorf("the skill %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Skill{}, fmt.Errorf("the skill %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return Skill{}, fmt.Errorf("the skill %s: %s is not a regular file", name, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSkill+1))
	if err != nil {
		return Skill{}, fmt.Errorf("the skill %s: %w", name, err)
	}
	return ParseSkill(name, data)
}

// ErrNoSkill marks a skill that is not in the skills folder.
var ErrNoSkill = errors.New("no such skill")
