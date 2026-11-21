//go:build unix

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// An image whose file has another name (a hard link) goes to no model,
// and its line says why: it could have changed through that name, outside
// the read-only uploads folder (ADR-0035; Codex review of #63, round 1).
func TestAnImageWithAnotherNameIsNotShown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.more = []proto.Model{eyes}
	session, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "c", Cwd: h.work, Model: eyes.ModelRef})
	device := "u:" + mustDevice(t, h).ID + ":"
	pic, err := h.upload(session.ID, "u1", "shot.png", "image/png", pngOf(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(pic.Path, filepath.Join(t.TempDir(), "kept.png")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if _, err := h.client.Send(ctx, session.ID, proto.SendMessage{RequestID: "m1", Text: "See.", Attachments: []int64{pic.ID}}); err != nil {
		t.Fatal(err)
	}
	_, files := proto.SplitAttachments(h.engine.texts[device+"m1"])
	if len(files) != 1 || files[0].Note != "[Image: not shown to you: its file has another name, through which it could have changed.]" {
		t.Fatalf("the files %+v", files)
	}
	if len(h.engine.images[device+"m1"]) != 0 {
		t.Fatalf("the model got %d images", len(h.engine.images[device+"m1"]))
	}
}
