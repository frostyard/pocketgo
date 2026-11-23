package files

import "testing"

// TestContentNames classifies extensions without inspecting contents.
func TestContentNames(t *testing.T) {
	for _, tt := range []struct {
		name            string
		image, markdown bool
	}{
		{"a.png", true, false}, {"a.JPG", true, false}, {"a.jpeg", true, false}, {"a.GIF", true, false}, {"a.WeBp", true, false},
		{"README.md", false, true}, {"a.MARKDOWN", false, true}, {"notes.txt", false, false}, {"a.png.txt", false, false}, {"", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if IsImageName(tt.name) != tt.image || IsMarkdownName(tt.name) != tt.markdown {
				t.Fatal(tt)
			}
		})
	}
}
