package profiles

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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

// LoadSkill reads skill name in the skills folder dir: <dir>/<name>/SKILL.md,
// a regular file once its links are followed, as the owner's configuration
// may link it, of at most MaxSkill bytes of UTF-8. A skill that is not
// there is an error that says so, with ErrNoSkill.
func LoadSkill(dir, name string) (Skill, error) {
	if err := CheckSkillName(name); err != nil {
		return Skill{}, err
	}
	if dir == "" {
		return Skill{}, fmt.Errorf("%w: pocketd has no skills folder", ErrNoSkill)
	}
	path := filepath.Join(dir, name, SkillFile)
	f, err := os.Open(path)
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

// ErrNoSkill marks a skill that is not in the skills folder.
var ErrNoSkill = errors.New("no such skill")
