package app

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/proto"
)

// The composer attaches files as frostyard/pocket's does (ADR-0029): Attach
// files opens the chooser, a pasted image and a dropped file attach too,
// each shows as a chip with its name, "uploading…" while it uploads, and
// Remove; Send waits for every upload, carries the files, and the
// transcript shows them as chips under the message, with its images.
func TestTheComposerAttachesFilesSnapshot(t *testing.T) {
	x := newTestApp(t)
	opus := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, Name: "Claude Opus 5.5", Reasoning: true,
		ThinkingLevels: []string{"off", "high"}, Images: true}
	x.srv.SetModels([]proto.Model{opus})
	x.srv.AddSession(proto.Session{ID: 1, Title: "Fix the flaky test", Cwd: "/home/team/projects/nsl", Model: opus.ModelRef,
		State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	dir := t.TempDir()
	logFile, notes := filepath.Join(dir, "build.log"), filepath.Join(dir, "notes.md")
	for _, p := range []string{logFile, notes} {
		if err := os.WriteFile(p, []byte("FAIL: TestFlaky (0.02s)\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var picks int
	x.a.opts.PickFiles = func(context.Context) ([]string, error) { picks++; return []string{logFile}, nil }
	x.connect(t)
	x.a.openSession(1)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	uploaded := func(n int) func() bool {
		return func() bool {
			files := x.a.st.Attachments(1)
			return len(files) == n && x.a.st.AttachmentsReady(1) && x.a.st.InFlight() == 0
		}
	}

	x.click(t, "Attach files")
	x.until(t, uploaded(1))
	if picks != 1 {
		t.Fatalf("the chooser opened %d times", picks)
	}

	// A pasted image attaches as pocket names one.
	var shot bytes.Buffer
	if err := png.Encode(&shot, image.NewGray(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	x.tt.SetClipboardData(transfer.New(transfer.NewItem(transfer.Bytes(transfer.PNG, shot.Bytes()))))
	field, ok := x.tt.AccessByRole(ui.RoleTextField, "Message")
	if !ok {
		t.Fatalf("no message field in %q", x.tt.Texts())
	}
	cx, cy := field.Bounds.X+field.Bounds.W/2, field.Bounds.Y+field.Bounds.H/2
	x.tt.ClickAt(cx, cy)
	x.tt.Command("paste")
	x.until(t, uploaded(2))

	// A dropped file, whose upload is held: its chip says it uploads, and
	// the message waits for it.
	path := proto.Path(proto.PathUploads, 1)
	release := x.srv.HoldAnswerOnce(http.MethodPost, path)
	x.tt.DropFiles(cx, cy, notes)
	x.until(t, func() bool { return x.srv.Holding(http.MethodPost, path) })
	x.hasAll(t, "build.log", "pasted-image.png", "notes.md · uploading…")
	if x.a.st.MaySend(1) {
		t.Fatal("a message may go while a file uploads")
	}
	*x.a.composer.draft(1) = "Why does this fail?"
	x.settle()
	x.snapshot(t, "composer-attachments")
	release()
	x.until(t, uploaded(3))

	// Remove takes a file off the message.
	x.click(t, "Remove attachment notes.md")
	x.until(t, uploaded(2))
	if slices.Contains(x.tt.Texts(), "notes.md") {
		t.Fatalf("the removed file still shows: %q", x.tt.Texts())
	}

	x.click(t, "Send")
	x.until(t, func() bool {
		entries := x.a.st.Data.Open().Entries
		return !x.a.st.Sending(1).Busy && x.a.st.InFlight() == 0 && len(entries) == 1
	})
	var sent proto.SendMessage
	reqs := x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))
	ups := x.srv.Uploads()
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &sent) != nil || sent.Text != "Why does this fail?" ||
		!slices.Equal(sent.Attachments, []int64{ups[0].ID, ups[1].ID}) {
		t.Fatalf("sent %+v (uploads %+v)", sent, ups)
	}
	if left := x.a.st.Attachments(1); len(left) != 0 || *x.a.composer.draft(1) != "" {
		t.Fatalf("after the message went: chips %+v, draft %q", left, *x.a.composer.draft(1))
	}
	x.hasAll(t, "Why does this fail?", "build.log", "pasted-image.png", "1 image")
	x.snapshot(t, "transcript-attachments")
}

// Files alone make a message: Send takes no words with them.
func TestFilesAloneMakeAMessage(t *testing.T) {
	x := newTestApp(t)
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	x.srv.AddSession(proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/projects/nsl", Model: opus,
		State: proto.SessionIdle, UpdatedAt: at(-time.Minute)})
	x.connect(t)
	x.a.openSession(1)
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded })
	x.a.pasteAttachments(1, transfer.New(transfer.NewItem(transfer.Bytes(transfer.PNG, []byte("\x89PNG\r\n\x1a\n")))))
	x.until(t, func() bool {
		return len(x.a.st.Attachments(1)) == 1 && x.a.st.AttachmentsReady(1) && x.a.st.InFlight() == 0
	})
	x.click(t, "Send")
	x.until(t, func() bool { return len(x.a.st.Data.Open().Entries) == 1 && x.a.st.InFlight() == 0 })
	x.hasAll(t, "pasted-image.png")
	// A paste of text alone is the field's.
	if x.a.pasteAttachments(1, transfer.TextData("words")) {
		t.Fatal("a paste of text was taken as a file")
	}
}
