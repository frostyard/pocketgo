package pig

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
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
	if _, err := e.Send(ctx, id, "u:dev:m1", "What is this?", api.Image{Data: picture, MimeType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	settle(t, e, id)
	mu.Lock()
	blocks, ok := seen.(ai.UserContentBlocks)
	mu.Unlock()
	if !ok || len(blocks) != 2 {
		t.Fatalf("the model saw %#v", seen)
	}
	if text, ok := blocks[0].(ai.TextContent); !ok || text.Text != "What is this?" {
		t.Fatalf("the first block is %#v", blocks[0])
	}
	if image, ok := blocks[1].(ai.ImageContent); !ok || image.MimeType != "image/png" || image.Data != base64.StdEncoding.EncodeToString(picture) {
		t.Fatalf("the second block is %#v", blocks[1])
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
	if user == nil || user.Text != "What is this?" || user.Images != 1 {
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
	folder := uploads.New(uploads.Dir(l.home), nil)
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
