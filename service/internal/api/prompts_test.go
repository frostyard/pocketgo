package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// GET /v1/prompts lists the owner's saved prompts by name, read from the
// profile folder at each request: a bad file with its problem, the others
// still valid, and a change at the next request, with no restart
// (ADR-0051).
func TestPromptsListsTheOwnersSavedPrompts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Without profiles there are none, and nothing is wrong.
	if list, err := h.client.Prompts(ctx); err != nil || list.Problem != "" || list.Prompts == nil || len(list.Prompts) != 0 {
		t.Fatalf("without profiles: %+v %v", list, err)
	}
	dir, _ := h.writeProfiles(map[string]string{"odrade.md": "---\nmodel: faux/faux-1\n---\n# Odrade\n"})
	// A profile folder with no prompts folder has none.
	if list, err := h.client.Prompts(ctx); err != nil || list.Problem != "" || len(list.Prompts) != 0 {
		t.Fatalf("no prompts folder: %+v %v", list, err)
	}
	if err := os.Mkdir(filepath.Join(dir, profiles.PromptsFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, profiles.PromptsFolder, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("review-pr.md", "---\ndescription: Review a pull request\nargument-hint: <number>\n---\nReview $ARGUMENTS and fix what you find.\n")
	write("broken.md", "---\nmodel: x\n---\nBroken.")
	list, err := h.client.Prompts(ctx)
	if err != nil || list.Problem != "" || len(list.Prompts) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	if p := list.Prompts[0]; p.Name != "broken" || p.Text != "" || !strings.Contains(p.Problem, `the field "model"`) {
		t.Errorf("broken %+v", p)
	}
	if p := list.Prompts[1]; p != (proto.Prompt{Name: "review-pr", Description: "Review a pull request", ArgumentHint: "<number>", Text: "Review $ARGUMENTS and fix what you find."}) {
		t.Errorf("review-pr %+v", p)
	}
	// A new prompt shows at the next request.
	write("ship-it.md", "Ship it.")
	if list, err := h.client.Prompts(ctx); err != nil || len(list.Prompts) != 3 || list.Prompts[2].Name != "ship-it" || list.Prompts[2].Text != "Ship it." {
		t.Fatalf("after a change: %+v %v", list, err)
	}
	// A folder that cannot be read lists none, and says why.
	if err := os.RemoveAll(filepath.Join(dir, profiles.PromptsFolder)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, profiles.PromptsFolder)); err != nil {
		t.Fatal(err)
	}
	if list, err := h.client.Prompts(ctx); err != nil || len(list.Prompts) != 0 || !strings.Contains(list.Problem, "symbolic link") {
		t.Fatalf("a linked folder: %+v %v", list, err)
	}
}

// The prompts are read through the folder pocketd pinned at start: a
// profile folder replaced since lists none, and says so.
func TestPromptsOfAReplacedProfileFolderListNone(t *testing.T) {
	h := newHarness(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "agents")
	if err := os.MkdirAll(filepath.Join(dir, profiles.PromptsFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profiles.PromptsFolder, "ship-it.md"), []byte("Ship it."), 0o600); err != nil {
		t.Fatal(err)
	}
	folder, err := profiles.Pin(dir, parent)
	if err != nil {
		t.Fatal(err)
	}
	h.server.SetProfiles(folder)
	if list, err := h.client.Prompts(context.Background()); err != nil || len(list.Prompts) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, profiles.PromptsFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	if list, err := h.client.Prompts(context.Background()); err != nil || len(list.Prompts) != 0 || !strings.Contains(list.Problem, "restart pocketd") {
		t.Fatalf("a replaced folder: %+v %v", list, err)
	}
}

// GET /v1/prompts is read only: another method is 405, as on every route,
// and it needs the protocol and the token as every request does.
func TestPromptsIsReadOnly(t *testing.T) {
	h := newHarness(t)
	response, e := h.raw(http.MethodPost, proto.PathPrompts, `{"requestId":"r1"}`, map[string]string{
		proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token, "Content-Type": "application/json"})
	if response.StatusCode != http.StatusMethodNotAllowed || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("POST: %d %+v", response.StatusCode, e)
	}
	if response, e := h.raw(http.MethodGet, proto.PathPrompts, "", map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version)}); response.StatusCode != http.StatusUnauthorized || e.Code != proto.ErrorCodeUnauthorized {
		t.Fatalf("without a token: %d %+v", response.StatusCode, e)
	}
	if response, e := h.raw(http.MethodGet, proto.PathPrompts, "", map[string]string{"Authorization": "Bearer " + h.token}); response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeProtocol {
		t.Fatalf("without the protocol: %d %+v", response.StatusCode, e)
	}
}
