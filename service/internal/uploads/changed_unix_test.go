//go:build unix

package uploads

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An upload is read for the model only as the bytes it was uploaded with,
// checked against the digest kept when it was staged: one changed through
// another name (a hard link an agent made before the uploads folder was
// read-only to it), or by hand, is ErrChanged, however many names it has
// now; one with another name but its own bytes reads (ADR-0035; Codex
// review of #63, rounds 1 and 2).
func TestAnUploadThatChangedIsNotRead(t *testing.T) {
	f, home := testFolder(t)
	file, size, err := f.Store(7, "shot.png", strings.NewReader("pixels"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("pixels"))
	digest := hex.EncodeToString(sum[:])
	if data, err := f.ReadUpload(7, file, size, digest); err != nil || string(data) != "pixels" {
		t.Fatalf("ReadUpload = %q, %v", data, err)
	}
	other := filepath.Join(home, "kept-by-an-agent.png")
	if err := os.Link(f.Path(7, file), other); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if data, err := f.ReadUpload(7, file, size, digest); err != nil || string(data) != "pixels" {
		t.Fatalf("an unchanged upload with another name: %q, %v", data, err)
	}
	// Changed through the other name, at the same size; then cut short.
	if err := os.WriteFile(other, []byte("PIXELS"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := f.ReadUpload(7, file, size, digest); !errors.Is(err, ErrChanged) || data != nil {
		t.Fatalf("a changed upload read %q, %v", data, err)
	}
	if err := os.WriteFile(other, []byte("pix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadUpload(7, file, size, digest); !errors.Is(err, ErrChanged) {
		t.Fatalf("an upload cut short: %v", err)
	}
	// Grown, it changed too, and says so (Codex review of #63, round 6).
	if err := os.WriteFile(other, []byte("pixels and more"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadUpload(7, file, size, digest); !errors.Is(err, ErrChanged) {
		t.Fatalf("an upload grown: %v", err)
	}
	// Removing the other name restores nothing: the bytes are checked.
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadUpload(7, file, size, digest); !errors.Is(err, ErrChanged) {
		t.Fatalf("a changed upload with one name again: %v", err)
	}
	if _, err := f.ReadUpload(7, file, size, ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("an upload with no digest kept: %v", err)
	}
}
