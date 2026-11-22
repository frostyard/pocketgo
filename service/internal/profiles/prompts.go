package profiles

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// PromptsFolder is the owner's saved prompts, a folder of the profile
// folder, which agents' commands see read-only (ADR-0051): one <name>.md
// file a prompt, which the desktop's composer inserts with /.
const PromptsFolder = "prompts"

// The bounds of the saved prompts (ADR-0051). A prompt goes whole into the
// composer, so its file is small; the folder lists at most MaxPrompts
// files, so its answer stays small too.
const (
	MaxPrompts            = 100
	MaxPromptFile         = 16 << 10
	MaxPromptDescription  = 300
	MaxPromptArgumentHint = 100
)

// promptName is a prompt's name: its file's name without .md.
var promptName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// CheckPromptName refuses a name that is no prompt's file name.
func CheckPromptName(name string) error {
	if !promptName.MatchString(name) {
		return fmt.Errorf("%q is no prompt's name: 1 to 64 lowercase letters, digits and dashes, starting with a letter or a digit", name)
	}
	return nil
}

// Prompt is one of the owner's saved prompts: a file of the prompts
// folder, valid, or listed by its name with its Problem.
type Prompt struct {
	// Name is the file's name without .md, as the owner types it after /.
	Name string
	// Description says what the prompt is for, and ArgumentHint what may
	// follow its name; either may be "".
	Description, ArgumentHint string
	// Text is the prompt's text: the file after its front matter, without
	// the white space around it.
	Text string
	// Problem says why the file is no prompt, "" for a valid one: it
	// lists only its Name then.
	Problem string
}

// Prompts are the files of the prompts folder, by name.
type Prompts struct {
	List []Prompt
	// Problem says the folder holds more prompt files than it lists, ""
	// when it lists them all.
	Problem string
}

// LoadPrompts reads the owner's saved prompts in the profile folder dir:
// none when it has no prompts folder.
func LoadPrompts(dir string) (Prompts, error) {
	return Folder{Dir: dir}.Prompts()
}

// Prompts reads the folder's saved prompts (ADR-0051), through the folder
// pocketd pinned at start, at each call, so a change to a prompt shows
// without a restart. The prompts folder is a folder of its own, never a
// symbolic link; no prompts folder is no prompts. Its files that end in
// .md, but those whose name starts with a dot (an editor's), are its
// prompts, by name, at most MaxPrompts of them. Each is read as a profile
// is (a regular file of one name, never through a link, never waiting on a
// named pipe) and checked by ParsePrompt; a file that breaks a rule is
// listed with its problem, and the others still are. A folder that cannot
// be read is an error.
func (f Folder) Prompts() (Prompts, error) {
	root, err := f.openRoot()
	if err != nil {
		return Prompts{}, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(PromptsFolder)
	if errors.Is(err, fs.ErrNotExist) {
		return Prompts{}, nil
	} else if err != nil {
		return Prompts{}, fmt.Errorf("the %s folder: %v", PromptsFolder, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return Prompts{}, fmt.Errorf("the %s folder is a symbolic link: make it a folder of its own in the profile folder", PromptsFolder)
	case !info.IsDir():
		return Prompts{}, fmt.Errorf("%s in the profile folder is not a folder", PromptsFolder)
	}
	dir, err := root.OpenRoot(PromptsFolder)
	if err != nil {
		return Prompts{}, fmt.Errorf("the %s folder: %v", PromptsFolder, err)
	}
	defer func() { _ = dir.Close() }()
	// The folder opened is the one looked at: a link put at its name
	// meanwhile is never read.
	if opened, err := dir.Stat("."); err != nil || !os.SameFile(info, opened) {
		return Prompts{}, fmt.Errorf("the %s folder changed while it was opened", PromptsFolder)
	}
	names, err := promptFiles(dir)
	if err != nil {
		return Prompts{}, fmt.Errorf("the %s folder: %v", PromptsFolder, err)
	}
	var out Prompts
	if len(names) > MaxPrompts {
		out.Problem = fmt.Sprintf("the %s folder holds %d prompt files: only the first %d by name are listed", PromptsFolder, len(names), MaxPrompts)
		names = names[:MaxPrompts]
	}
	for _, file := range names {
		out.List = append(out.List, readPrompt(dir, file))
	}
	return out, nil
}

// promptFiles are the names of the prompt files of the prompts folder dir,
// those that end in .md and start with no dot, sorted by the prompts'
// names, without .md: review before review-ci, as the list and its bound
// go (Codex review of #108, round 1).
func promptFiles(dir *os.Root) ([]string, error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".md") && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		return strings.Compare(strings.TrimSuffix(a, ".md"), strings.TrimSuffix(b, ".md"))
	})
	return names, nil
}

// readPrompt is the prompt file of the prompts folder dir, or its name
// with why it is none.
func readPrompt(dir *os.Root, file string) Prompt {
	name := strings.TrimSuffix(file, ".md")
	if err := CheckPromptName(name); err != nil {
		return Prompt{Name: name, Problem: err.Error()}
	}
	f, err := openInRoot(dir, file)
	if err != nil {
		return Prompt{Name: name, Problem: fmt.Sprintf("%s: %v", file, err)}
	}
	data, err := readOpenedUpTo(f, MaxPromptFile)
	if err != nil {
		return Prompt{Name: name, Problem: fmt.Sprintf("%s: %v", file, err)}
	}
	p, err := ParsePrompt(name, data)
	if err != nil {
		return Prompt{Name: name, Problem: err.Error()}
	}
	return p
}

// ParsePrompt checks prompt name's file as read, at most MaxPromptFile+1
// bytes (ADR-0051): at most MaxPromptFile bytes of UTF-8, which may start
// with front matter between --- lines, one YAML map whose only fields are
// description, at most MaxPromptDescription characters, and argument-hint,
// at most MaxPromptArgumentHint, each text, none named twice, each read on
// one line with its white space made single spaces. The text is the rest,
// without the white space around it: not empty, with no control character
// but tabs and line breaks. A file that breaks a rule is an error that
// says which.
func ParsePrompt(name string, data []byte) (Prompt, error) {
	if err := CheckPromptName(name); err != nil {
		return Prompt{}, err
	}
	switch {
	case len(data) > MaxPromptFile:
		return Prompt{}, fmt.Errorf("the file is larger than %d bytes", MaxPromptFile)
	case !utf8.Valid(data):
		return Prompt{}, errors.New("the file is not UTF-8")
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	p := Prompt{Name: name}
	body := text
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		var front string
		switch {
		case rest == "---":
			body = ""
		case strings.HasPrefix(rest, "---\n"):
			body = rest[len("---\n"):]
		default:
			front, body, ok = strings.Cut(rest, "\n---\n")
			if !ok {
				if front, ok = strings.CutSuffix(rest, "\n---"); !ok {
					return Prompt{}, errors.New("its front matter does not end (---)")
				}
				body = ""
			}
		}
		if err := p.setFront(front); err != nil {
			return Prompt{}, err
		}
	}
	p.Text = strings.TrimSpace(body)
	if p.Text == "" {
		return Prompt{}, errors.New("it has no text")
	}
	if r, ok := controlIn(p.Text, "\n\t"); ok {
		return Prompt{}, fmt.Errorf("its text holds the control character %U", r)
	}
	return p, nil
}

// setFront reads a prompt's front matter: description and argument-hint,
// each key as written, before anything is decoded (Parse).
func (p *Prompt) setFront(front string) error {
	decoder := yaml.NewDecoder(strings.NewReader(front))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("its front matter is no YAML: %v", err)
	}
	var more yaml.Node
	if err := decoder.Decode(&more); !errors.Is(err, io.EOF) {
		return errors.New("its front matter holds more than one YAML document")
	}
	if len(root.Content) == 0 {
		return nil
	}
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return errors.New("its front matter is not a map of fields")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		var key string
		if err := scalar(mapping.Content[i], &key); err != nil {
			return fmt.Errorf("a key of its front matter is %w", err)
		}
		if key != "description" && key != "argument-hint" {
			return fmt.Errorf("its front matter has the field %q: a prompt's are description and argument-hint", key)
		}
		if seen[key] {
			return fmt.Errorf("its front matter names %s twice", key)
		}
		seen[key] = true
		var value string
		if err := scalar(mapping.Content[i+1], &value); err != nil {
			return fmt.Errorf("its %s is %w", key, err)
		}
		value = strings.Join(strings.Fields(value), " ")
		limit, out := MaxPromptDescription, &p.Description
		if key == "argument-hint" {
			limit, out = MaxPromptArgumentHint, &p.ArgumentHint
		}
		if utf8.RuneCountInString(value) > limit {
			return fmt.Errorf("its %s is longer than %d characters", key, limit)
		}
		if r, ok := controlIn(value, ""); ok {
			return fmt.Errorf("its %s holds the control character %U", key, r)
		}
		*out = value
	}
	return nil
}

// controlIn is the first control character of s that allowed does not
// hold, and whether there is one.
func controlIn(s, allowed string) (rune, bool) {
	for _, r := range s {
		if unicode.IsControl(r) && !strings.ContainsRune(allowed, r) {
			return r, true
		}
	}
	return 0, false
}
