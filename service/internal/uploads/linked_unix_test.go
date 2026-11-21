//go:build unix

package uploads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An upload with another name (a hard link) is not read for the model: it
// could change through that name, outside the uploads folder the agents'
// sandbox keeps read-only, as one an agent made before that folder was
// read-only could. Once the other name goes, it reads (ADR-0035; Codex
// review of #63, round 1).
func TestAnUploadWithAnotherNameIsNotRead(t *testing.T) {
	f, home := testFolder(t)
	file, size, err := f.Store(7, "shot.png", strings.NewReader("pixels"))
	if err != nil {
		t.Fatal(err)
	}
	if data, err := f.Read(7, file, size); err != nil || string(data) != "pixels" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	other := filepath.Join(home, "kept-by-an-agent.png")
	if err := os.Link(f.Path(7, file), other); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if data, err := f.Read(7, file, size); !errors.Is(err, ErrLinked) || data != nil {
		t.Fatalf("an upload with another name read %q, %v", data, err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if data, err := f.Read(7, file, size); err != nil || string(data) != "pixels" {
		t.Fatalf("Read once the other name went = %q, %v", data, err)
	}
}
