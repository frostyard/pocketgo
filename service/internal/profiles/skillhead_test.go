package profiles

import (
	"strings"
	"testing"
)

// A skill is listed by its frontmatter's name and description (ADR-0043):
// the name its folder's, the description on one line, within its bounds;
// anything else says why it is left out.
func TestParseSkillHead(t *testing.T) {
	long := strings.Repeat("d", MaxSkillDescription+1)
	for name, tc := range map[string]struct {
		head, want, err string
	}{
		"a plain description":          {head: "---\nname: cog\ndescription: Keep the notes.\n---\n# Cog\n", want: "Keep the notes."},
		"a block description":          {head: "---\nname: cog\ndescription: |\n  Keep the notes,\n  and tidy them.\nlicense: MIT\n---\n", want: "Keep the notes, and tidy them."},
		"Windows line ends":            {head: "---\r\nname: cog\r\ndescription: Keep the notes.\r\n---\r\n", want: "Keep the notes."},
		"frontmatter alone":            {head: "---\nname: cog\ndescription: Keep the notes.\n---", want: "Keep the notes."},
		"a quoted description":         {head: "---\nname: cog\ndescription: \"Keep: the notes.\"\n---\n", want: "Keep: the notes."},
		"no frontmatter":               {head: "# Cog\n", err: "it does not start with frontmatter (---)"},
		"frontmatter not ended":        {head: "---\nname: cog\ndescription: Keep.\n", err: "its frontmatter does not end (---)"},
		"no name":                      {head: "---\ndescription: Keep.\n---\n", err: "its frontmatter has no name"},
		"another name":                 {head: "---\nname: memory\ndescription: Keep.\n---\n", err: `its name "memory" is not its folder's, "cog"`},
		"no description":               {head: "---\nname: cog\n---\n", err: "its frontmatter has no description"},
		"an empty description":         {head: "---\nname: cog\ndescription: \"  \"\n---\n", err: "its frontmatter has no description"},
		"a description that is a list": {head: "---\nname: cog\ndescription: [a, b]\n---\n", err: "its description is not text"},
		"a name twice":                 {head: "---\nname: cog\nname: cog\ndescription: Keep.\n---\n", err: "frontmatter"},
		"a list, not a map":            {head: "---\n- cog\n---\n", err: "its frontmatter is not a map of fields"},
		"no YAML":                      {head: "---\nname: [cog\n---\n", err: "its frontmatter is no YAML"},
		"a description too long":       {head: "---\nname: cog\ndescription: " + long + "\n---\n", err: "its description is longer than 1024 characters"},
		"not UTF-8":                    {head: "---\nname: cog\ndescription: Keep \xff.\n---\n", err: "its frontmatter is not UTF-8"},
		"frontmatter past the head":    {head: "---\nname: cog\ndescription: " + strings.Repeat("x", MaxSkillHead) + "\n---\n", err: "its frontmatter does not end (---) within its first 8192 bytes"},
	} {
		got, err := ParseSkillHead("cog", []byte(tc.head))
		switch {
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%s: %q, %v; want %q", name, got, err, tc.want)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: %q, %v; want an error with %q", name, got, err, tc.err)
		}
	}
	if _, err := ParseSkillHead("a\nb", []byte("---\nname: x\ndescription: y\n---\n")); err == nil {
		t.Error("a folder whose name is no skill's is listed")
	}
}
