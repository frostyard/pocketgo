package pig

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/ai"
	"golang.org/x/term"
)

// Login signs in to a provider's subscription with its OAuth flow, or
// keeps an API key for a provider with none, and stores the credential in
// authPath. It asks its questions on out and reads the
// answers, one line each, from in: run it in a terminal.
func Login(ctx context.Context, authPath, provider string, in io.Reader, out io.Writer) error {
	auth, err := ai.NewAuthStorage(authPath)
	if err != nil {
		return err
	}
	models := ai.CreateModels(ai.CreateModelsOptions{Credentials: auth})
	for _, p := range ai.BuiltinProviders() {
		if p != nil {
			models.SetProvider(p)
		}
	}
	// A subscription signs in by OAuth. A provider with no OAuth sign-in,
	// as the DeepSeek API, takes an API key, whose models serve a thread's
	// worker alone (ADR-0021): a subscription's sign-in is never replaced
	// by a key.
	p := models.GetProvider(provider)
	if p == nil {
		return fmt.Errorf("sign in to %s: no such provider", provider)
	}
	kind, what := ai.CredentialOAuth, "the credential"
	if p.Auth.OAuth == nil {
		kind, what = ai.CredentialAPIKey, "the API key, whose models serve threads' workers alone,"
	}
	lines := newLineReader(in)
	interaction := ai.AuthInteraction{
		Prompt: func(ctx context.Context, prompt ai.AuthPrompt) (string, error) {
			readLine := func() (string, error) { return lines.read(ctx) }
			if _, secret := prompt.(ai.AuthSecretPrompt); secret && kind == ai.CredentialAPIKey {
				// The key is the prompt's only answer: typed at a terminal,
				// it is not shown, and it is kept only as typed (literalKey).
				_, _ = fmt.Fprint(out, prompt.(ai.AuthSecretPrompt).Message+": ")
				key, err := readSecret(in, readLine)
				if err != nil {
					return "", err
				}
				return key, literalKey(key)
			}
			switch p := prompt.(type) {
			case ai.AuthSelectPrompt:
				_, _ = fmt.Fprintln(out, p.Message)
				for i, option := range p.Options {
					_, _ = fmt.Fprintf(out, "  %d) %s  %s\n", i+1, option.Label, option.Description)
				}
				_, _ = fmt.Fprint(out, "Choose a number: ")
				answer, err := readLine()
				if err != nil {
					return "", err
				}
				var n int
				if _, err := fmt.Sscanf(answer, "%d", &n); err != nil || n < 1 || n > len(p.Options) {
					return "", fmt.Errorf("choose 1 to %d", len(p.Options))
				}
				return p.Options[n-1].ID, nil
			case ai.AuthTextPrompt:
				_, _ = fmt.Fprint(out, p.Message+": ")
			case ai.AuthSecretPrompt:
				_, _ = fmt.Fprint(out, p.Message+": ")
			case ai.AuthManualCodePrompt:
				_, _ = fmt.Fprint(out, p.Message+": ")
			default:
				_, _ = fmt.Fprint(out, prompt.Type()+": ")
			}
			return readLine()
		},
		Notify: func(event ai.AuthEvent) {
			switch e := event.(type) {
			case ai.AuthURLEvent:
				_, _ = fmt.Fprintf(out, "Open this URL in a browser:\n  %s\n", e.URL)
				if e.Instructions != "" {
					_, _ = fmt.Fprintln(out, e.Instructions)
				}
			case ai.AuthDeviceCodeEvent:
				_, _ = fmt.Fprintf(out, "Open %s and enter the code %s\n", e.VerificationURI, e.UserCode)
			case ai.AuthInfoEvent:
				_, _ = fmt.Fprintln(out, e.Message)
				for _, link := range e.Links {
					_, _ = fmt.Fprintf(out, "  %s %s\n", link.Label, link.URL)
				}
			case ai.AuthProgressEvent:
				_, _ = fmt.Fprintln(out, e.Message)
			}
		},
	}
	if _, err := models.Login(ctx, provider, kind, interaction); err != nil {
		return fmt.Errorf("sign in to %s: %w", provider, err)
	}
	_, _ = fmt.Fprintf(out, "Signed in to %s; %s is in %s\n", provider, what, authPath)
	return nil
}

// lineReader reads lines on a goroutine of its own, so a prompt can stop
// waiting when its context ends: PiG cancels the manual-code prompt once the
// browser callback signs in, while the terminal stays silent.
type lineReader struct {
	once  sync.Once
	in    io.Reader
	lines chan lineResult
}

type lineResult struct {
	line string
	err  error
}

func newLineReader(in io.Reader) *lineReader {
	return &lineReader{in: in, lines: make(chan lineResult)}
}

func (r *lineReader) read(ctx context.Context) (string, error) {
	r.once.Do(func() {
		go func() {
			buffered := bufio.NewReader(r.in)
			for {
				line, err := buffered.ReadString('\n')
				if err != nil && line == "" {
					r.lines <- lineResult{err: err}
					return
				}
				r.lines <- lineResult{line: strings.TrimSpace(line)}
			}
		}()
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-r.lines:
		return result.line, result.err
	}
}

// readSecret reads one line: from a terminal without showing it, else as
// readLine does.
func readSecret(in io.Reader, readLine func() (string, error)) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		key, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(key)), err
	}
	return readLine()
}

// literalKey refuses an API key PiG would not keep as typed: it reads a
// key that starts with ! as a shell command to run, and $NAME in one as an
// environment variable, which would make a sign-in run a command as pocketd
// or read its environment (ADR-0021; protocol rule 14d). An empty key, or
// one with a space or a character outside printable ASCII, is no key
// either: it would replace a working sign-in with nothing.
func literalKey(key string) error {
	switch {
	case key == "":
		return errors.New("no API key: nothing was changed")
	case len(key) > 1024:
		return errors.New("the API key is longer than 1024 characters")
	case strings.HasPrefix(key, "!") || strings.Contains(key, "$"):
		return errors.New("an API key with ! first or $ in it would be read as a command or an environment variable: give the key itself")
	}
	for _, r := range key {
		if r <= ' ' || r > '~' {
			return errors.New("an API key holds no space and no character outside printable ASCII")
		}
	}
	return nil
}
