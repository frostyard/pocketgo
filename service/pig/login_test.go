package pig

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
)

func TestLineReaderStopsWhenTheContextEnds(t *testing.T) {
	in, out := io.Pipe()
	defer func() { _ = out.Close() }()
	r := newLineReader(in)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.read(ctx)
		done <- err
	}()
	cancel() // the browser callback signed in; the terminal stays silent
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel: %v", err)
	}
	// A later prompt still gets the next line.
	go func() { _, _ = io.WriteString(out, "  the-code \n") }()
	line, err := r.read(context.Background())
	if err != nil || line != "the-code" {
		t.Fatalf("next line %q %v", line, err)
	}
}

func TestLineReaderReportsEOF(t *testing.T) {
	in, out := io.Pipe()
	r := newLineReader(in)
	_ = out.Close()
	if _, err := r.read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF: %v", err)
	}
}

// A provider with no OAuth sign-in, as the DeepSeek API, takes an API key
// from standard input, and pocketd keeps it as one: its models then serve
// workers alone (ADR-0021).
func TestLoginTakesAnAPIKeyWhereThereIsNoOAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	var out bytes.Buffer
	if err := Login(context.Background(), path, "deepseek", strings.NewReader("sk-test-not-a-key\n"), &out); err != nil {
		t.Fatal(err)
	}
	auth, err := ai.NewAuthStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	cred, ok, err := auth.GetRaw("deepseek")
	if err != nil || !ok || cred.Type != ai.CredentialAPIKey || cred.Key != "sk-test-not-a-key" {
		t.Fatalf("the credential %+v, %v, %v", cred.Type, ok, err)
	}
	if !strings.Contains(out.String(), "workers alone") {
		t.Fatalf("the answer does not say what the key serves: %q", out.String())
	}
	if err := Login(context.Background(), path, "nosuch", strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("a sign-in to no provider succeeded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// A key PiG would not keep as typed is refused, and nothing is stored: one
// that starts with ! runs a command, one with $ reads the environment, and
// an empty one would replace a working sign-in (Codex review of #29,
// round 1).
func TestLoginKeepsOnlyALiteralKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	for _, key := range []string{"!cat /etc/passwd", "sk-$HOME", "${DEEPSEEK_API_KEY}", "", "sk with space", "sk-é"} {
		if err := Login(context.Background(), path, "deepseek", strings.NewReader(key+"\n"), io.Discard); err == nil {
			t.Errorf("the key %q was kept", key)
		}
	}
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "deepseek") {
		t.Fatalf("a refused key was stored: %s", data)
	}
}

// A provider whose key flow asks for more than a typed key, as Bedrock's
// profile or credential chain and Vertex's service-account file, is
// refused, and the sign-in stays as it was (Codex review of #29, round 2).
func TestLoginRefusesAFlowThatAsksMoreThanAKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	before := `{"amazon-bedrock": {"type": "api_key", "key": "kept-key"}}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	// Bedrock's third choice, the credential chain, and Vertex's second,
	// Application Default Credentials, store a credential with no key.
	for provider, input := range map[string]string{"amazon-bedrock": "3\n\n", "google-vertex": "2\n\n"} {
		if err := Login(context.Background(), path, provider, strings.NewReader(input), io.Discard); err == nil {
			t.Errorf("%s: a flow that asks more than a key signed in", provider)
		}
	}
	auth, err := ai.NewAuthStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	if cred, ok, err := auth.GetRaw("amazon-bedrock"); err != nil || !ok || cred.Key != "kept-key" {
		t.Fatalf("Bedrock's sign-in after the refusal: %+v %v %v", cred.Key, ok, err)
	}
	if _, ok, _ := auth.GetRaw("google-vertex"); ok {
		t.Fatal("Vertex got a sign-in")
	}
}
