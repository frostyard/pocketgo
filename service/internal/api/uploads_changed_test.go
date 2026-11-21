//go:build unix

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// An image whose file changed since it was uploaded, as through a hard
// link an agent made before the uploads folder was read-only, goes to no
// model, and its line says why; one with another name but its own bytes
// goes (ADR-0035; Codex review of #63, rounds 1 and 2).
func TestAnImageWhoseFileChangedIsNotShown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: eyes.ModelRef})
	device := "u:" + mustDevice(t, h).ID + ":"
	kept, err := h.upload(session.ID, "u1", "kept.png", "image/png", pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := h.upload(session.ID, "u2", "changed.png", "image/png", pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Link(kept.Path, filepath.Join(dir, "kept.png")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	other := filepath.Join(dir, "changed.png")
	if err := os.Link(changed.Path, other); err != nil {
		t.Fatal(err)
	}
	// Other bytes of the same size, written through the other name.
	data, err := os.ReadFile(changed.Path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(other, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: []int64{kept.ID, changed.ID}}); err != nil {
		t.Fatal(err)
	}
	_, files := proto.SplitAttachments(h.engine.texts[device+"m1"])
	if len(files) != 2 || files[0].Note != "" || files[1].Note != "[Image: not shown to you: its file changed since it was uploaded.]" {
		t.Fatalf("the files %+v", files)
	}
	if len(h.engine.images[device+"m1"]) != 1 {
		t.Fatalf("the model got %d images, want the unchanged one", len(h.engine.images[device+"m1"]))
	}
}
