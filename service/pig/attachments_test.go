package pig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/chord/delta"
	"github.com/MichaelKinsy/PiG/durable"
	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/uploads"
)

// A message's images reach the model after its text, as PiG's image
// blocks, and its user entry counts them; a model whose catalog entry
// lists image among its inputs is listed as taking images (ADR-0029).
func TestAMessagesImagesReachTheModel(t *testing.T) {
	var mu sync.Mutex
	var seen ai.UserContent
	look := ai.FauxFactoryStep(func(transcript ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		messages := transcript.Messages()
		mu.Lock()
		if user, ok := messages[len(messages)-1].(ai.UserMessage); ok {
			seen = user.Content
		}
		mu.Unlock()
		return ai.FauxResponse{Content: []ai.FauxContentBlock{ai.FauxText("I see it.")}}.AssistantMessage(), nil
	})
	faux := ai.NewFauxProvider(ai.FauxConfig{Models: []ai.FauxModelDefinition{{ID: "faux-1", Input: []string{"text", "image"}}, {ID: "faux-blind", Input: []string{"text"}}}})
	models := ai.CreateModels()
	models.SetProvider(faux.Provider())
	faux.SetResponses([]ai.FauxResponseStep{look})

	listed, err := available(context.Background(), models, nil, nil)
	if err != nil || len(listed) != 2 {
		t.Fatalf("models %+v, %v", listed, err)
	}
	for _, m := range listed {
		if m.Images != (m.ID == "faux-1") {
			t.Errorf("%s lists images %v", m.ID, m.Images)
		}
	}

	f := newFixture(t)
	e := f.open(models)
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err != nil {
		t.Fatal(err)
	}
	picture := []byte("\x89PNG\r\n\x1a\nnot really")
	lines := proto.AttachmentLines([]proto.AttachedFile{{Path: "/home/team/.local/share/pocketgo-uploads/1/a-shot.png", Name: "shot.png", MimeType: "image/png", Size: 16}})
	if _, err := e.Send(ctx, id, "u:dev:m1", "What is this?", api.Attached{Lines: lines, Images: []api.Image{{Data: picture, MimeType: "image/png"}}}); err != nil {
		t.Fatal(err)
	}
	settle(t, e, id)
	mu.Lock()
	blocks, ok := seen.(ai.UserContentBlocks)
	mu.Unlock()
	// The words, the heading alone, the files' lines, the image.
	if !ok || len(blocks) != 4 {
		t.Fatalf("the model saw %#v", seen)
	}
	for i, want := range []string{"What is this?", proto.AttachmentsHeading, lines} {
		if text, ok := blocks[i].(ai.TextContent); !ok || text.Text != want {
			t.Fatalf("block %d is %#v", i, blocks[i])
		}
	}
	if image, ok := blocks[3].(ai.ImageContent); !ok || image.MimeType != "image/png" || image.Data != base64.StdEncoding.EncodeToString(picture) {
		t.Fatalf("the last block is %#v", blocks[3])
	}
	entries, _, err := e.Transcript(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var user *proto.Entry
	for i := range entries {
		if entries[i].Kind == proto.EntryUser {
			user = &entries[i]
		}
	}
	if user == nil || user.Text != proto.WithAttachmentLines("What is this?", lines) || user.Images != 1 || user.Files != 1 {
		t.Fatalf("the user entry is %+v", user)
	}
	// A message without images is the text alone, as before.
	if got := userContent("hi", nil); got != ai.UserText("hi") {
		t.Fatalf("a message without images is %#v", got)
	}
}

// The uploads folder is outside every folder the sandbox hides: an upload
// pocketd stored is read by a command in the sandbox and by the file
// tools, while the data folder stays hidden (ADR-0029).
func TestAnUploadIsReadableInTheSandbox(t *testing.T) {
	l, work := newLayout(t), t.TempDir()
	s := usableSandbox(t, l)
	folder := uploads.New(uploads.Dir(l.home), filepath.Join(l.data, "upload-staging"), nil)
	if s.Hides(folder.Base()) {
		t.Fatalf("the sandbox hides the uploads folder %s (Hide %q)", folder.Base(), s.Hide)
	}
	if !s.Hides(filepath.Join(l.data, "auth.json")) {
		t.Fatal("Hides says the data folder shows")
	}
	file, _, err := folder.Store(7, "build.log", strings.NewReader("FAIL: TestX"))
	if err != nil {
		t.Fatal(err)
	}
	path := folder.Path(7, file)
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s,
		protected: append([]string{l.data}, s.Guarded()...)}
	var mu sync.Mutex
	var out bytes.Buffer
	result, err := g.Exec(context.Background(), []string{"cat", path}, &env.ShellExecOptions{OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
		mu.Lock()
		out.WriteString(text)
		mu.Unlock()
	}})
	if err != nil || result.ExitCode != 0 || !strings.Contains(out.String(), "FAIL: TestX") {
		t.Fatalf("cat in the sandbox: %d %q %v", result.ExitCode, out.String(), err)
	}
	data, err := g.ReadTextFile(context.Background(), path)
	if err != nil || data != "FAIL: TestX" {
		t.Fatalf("the read tool got %q, %v", data, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// The sandbox's hidden folders hold the data folder, never the uploads
// folder in ~/.local/share, whether or not bwrap runs here (ADR-0029).
func TestTheSandboxShowsTheUploadsFolder(t *testing.T) {
	l := newLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap"))
	if err != nil {
		t.Fatal(err)
	}
	if dir := uploads.Dir(l.home); s.Hides(dir) || s.Hides(filepath.Join(dir, "7", "a-b-x.png")) {
		t.Fatalf("the sandbox hides %s (Hide %q)", dir, s.Hide)
	}
	if !s.Hides(l.data) || !s.Hides(filepath.Join(l.data, "auth.json")) {
		t.Fatalf("the sandbox shows the data folder (Hide %q)", s.Hide)
	}
	// A data folder that holds the uploads folder hides it.
	l.data = filepath.Join(l.home, ".local", "share")
	if s, err = newSandbox(l.paths("/usr/bin/bwrap")); err != nil {
		t.Fatal(err)
	}
	if !s.Hides(uploads.Dir(l.home)) {
		t.Fatal("a data folder that holds the uploads folder shows it")
	}
	// So does a home named through a link, before the uploads folder
	// exists (Codex review of #49, round 1).
	link := filepath.Join(l.root, "home-link")
	if err := os.Symlink(l.home, link); err != nil {
		t.Fatal(err)
	}
	if dir := uploads.Dir(link); !s.Hides(dir) {
		t.Fatalf("%s, through a link to the home, shows (Hide %q)", dir, s.Hide)
	}
}

// The model that reads a message gets its images as images only when it
// takes images, else PiG's placeholder, at each request: a message that
// queued under one model and runs under another gets what that one takes
// (Codex review of #49, round 5).
func TestAModelWithoutImagesGetsAPlaceholder(t *testing.T) {
	message := ai.UserMessage{Content: userContent("See.", []api.Attached{{Images: []api.Image{{Data: []byte("png"), MimeType: "image/png"}}}})}
	blind := &ai.Model{ID: "blind", Input: []string{"text"}}
	seeing := &ai.Model{ID: "seeing", Input: []string{"text", "image"}}
	shown := func(model *ai.Model) (images int, text string) {
		out := ai.TransformMessages([]ai.Message{message}, model, nil)
		for _, block := range out[0].(ai.UserMessage).Content.(ai.UserContentBlocks) {
			switch b := block.(type) {
			case ai.ImageContent:
				images++
			case ai.TextContent:
				text += b.Text
			}
		}
		return images, text
	}
	if images, text := shown(blind); images != 0 || !strings.Contains(text, "image omitted") {
		t.Fatalf("a model without images got %d images and %q", images, text)
	}
	if images, _ := shown(seeing); images != 1 {
		t.Fatalf("a model that takes images got %d", images)
	}
}

// A message's files are those pocketd sent apart from its words: a list
// the owner wrote or pasted in their words is words, an entry of no files,
// sent or queued (Codex review of #49, round 8).
func TestAListInTheOwnersWordsIsNoFiles(t *testing.T) {
	lines := proto.AttachmentLines([]proto.AttachedFile{{Path: "/home/team/.local/share/pocketgo-uploads/1/a-shot.png", Name: "shot.png", MimeType: "image/png", Size: 16}})
	quoted := proto.WithAttachmentLines("Like before:", lines)
	for _, c := range []struct {
		content ai.UserContent
		files   int
		text    string
	}{
		{ai.UserText(quoted), 0, quoted},
		{userContent("Like before:", []api.Attached{{Lines: lines}}), 1, quoted},
		{userContent("", []api.Attached{{Lines: lines}}), 1, proto.AttachmentsHeading + "\n" + lines},
		{userContent(quoted, []api.Attached{{Lines: lines}}), 1, quoted + "\n\n" + proto.AttachmentsHeading + "\n" + lines},
	} {
		entry := wireEntry(durable.EntryRecord{Kind: durable.UserEntry.Kind, Model: []ai.Message{ai.UserMessage{Content: c.content}}})
		if entry.Files != c.files || entry.Text != c.text {
			t.Errorf("%#v gives %d files, %q", c.content, entry.Files, entry.Text)
		}
		data, err := json.Marshal(c.content)
		if err != nil {
			t.Fatal(err)
		}
		content, err := delta.DecodeJson(data)
		if err != nil {
			t.Fatal(err)
		}
		if text, files, _ := attachedText(contentBlocks(content)); files != c.files || text != c.text {
			t.Errorf("queued %#v gives %d files, %q", c.content, files, text)
		}
	}
}

// Reading a transcript copies no image: an entry keeps a user message's
// text and counts, read from its Go value, never its image data (Codex
// review of #49, round 8).
func TestAnEntryIsReadWithoutItsImages(t *testing.T) {
	big := strings.Repeat("A", 8<<20)
	record := durable.EntryRecord{Kind: durable.UserEntry.Kind, Model: []ai.Message{ai.UserMessage{Content: ai.UserContentBlocks{
		ai.TextContent{Text: "See."}, ai.ImageContent{Data: big, MimeType: "image/png"},
	}}}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	entry := wireEntry(record)
	runtime.ReadMemStats(&after)
	if entry.Text != "See." || entry.Images != 1 {
		t.Fatalf("the entry is %+v", entry)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("reading the entry took %d bytes", grew)
	}
}
