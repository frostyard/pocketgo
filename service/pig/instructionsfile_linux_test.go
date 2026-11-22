//go:build linux

package pig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/frostyard/pocketgo/service/internal/api"
)

// instructionsSandbox is the layout's sandbox with the owner's global
// instructions file kept read-only, holding text, and the sandbox proved:
// it skips where bwrap cannot make any sandbox here (ADR-0044).
func instructionsSandbox(t *testing.T, l layout, file, text string) *Sandbox {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	p := l.paths(usableBwrap(t))
	p.Files = []*string{&file}
	s, err := newSandbox(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Fatalf("the sandbox fails on a host that can run bwrap: %v", err)
	}
	return s
}

// A command in the sandbox reads the owner's global instructions file, but
// cannot write, truncate or remove it, link it, put a link in its place,
// move it or the folders above it (ADR-0044).
func TestCommandsCannotChangeTheInstructionsFile(t *testing.T) {
	l := newLayout(t)
	file := filepath.Join(l.home, ".pi", "agent", "AGENTS.md")
	s := instructionsSandbox(t, l, file, "RULES")
	work := t.TempDir()
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	run := func(command string) string {
		t.Helper()
		var mu sync.Mutex
		var out strings.Builder
		if _, err := g.Exec(context.Background(), command, &env.ShellExecOptions{OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
			mu.Lock()
			out.WriteString(text)
			mu.Unlock()
		}}); err != nil {
			t.Fatalf("Exec(%q): %v", command, err)
		}
		return out.String()
	}
	q := func(path string) string { return `"` + path + `"` }
	pi := filepath.Join(l.home, ".pi")
	moved := filepath.Join(work, "moved")
	out := run(`echo x >> ` + q(file) + ` && echo LEAK-1; ` +
		`printf y > ` + q(file) + ` && echo LEAK-2; ` +
		`rm -f ` + q(file) + ` && echo LEAK-3; ` +
		`mv ` + q(file) + ` ` + q(moved) + ` && echo LEAK-4; ` +
		`mv ` + q(filepath.Join(pi, "agent")) + ` ` + q(filepath.Join(pi, "moved")) + ` && echo LEAK-5; ` +
		`mv ` + q(pi) + ` ` + q(filepath.Join(l.home, "moved-pi")) + ` && echo LEAK-6; ` +
		`ln ` + q(file) + ` ` + q(filepath.Join(work, "hard")) + ` && echo LEAK-7; ` +
		`ln -sf /etc/hostname ` + q(file) + ` && echo LEAK-8; ` +
		`rm -rf ` + q(filepath.Join(pi, "agent")) + ` && echo LEAK-9; ` +
		`cat ` + q(file))
	if !strings.Contains(out, "RULES") {
		t.Errorf("the command cannot read the instructions file:\n%s", out)
	}
	if strings.Contains(out, "LEAK-") {
		t.Errorf("a command changed the instructions file:\n%s", out)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "RULES" {
		t.Errorf("the instructions file changed on the host: %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(work, "hard")); !os.IsNotExist(err) {
		t.Errorf("a command linked the instructions file elsewhere: %v", err)
	}
}

// A session reads the owner's global instructions file as the owner
// replaces it: a write in place, which stops nothing, and a new file
// renamed over it, which stops the commands running and keeps the new file
// read-only too (ADR-0044).
func TestSessionsReadTheInstructionsFileAsTheOwnerReplacesIt(t *testing.T) {
	l := newLayout(t)
	file := filepath.Join(l.home, ".pi", "agent", "AGENTS.md")
	s := instructionsSandbox(t, l, file, "v1 RULES")
	work := t.TempDir()
	e := &Engine{opts: Options{Sandbox: s, Instructions: file}, commands: allowedEnv()}
	ctx := context.Background()
	global := func(want string) {
		t.Helper()
		loaded := e.loadContext(ctx, api.SessionSpec{Cwd: work})
		if !strings.Contains(loaded.Global, want) {
			t.Fatalf("a session's instructions do not hold %q:\n%s", want, loaded.Global)
		}
	}
	global("v1 RULES")

	// In place: the same file, which every command already has read-only.
	if err := os.WriteFile(file, []byte("v2 RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	global("v2 RULES")

	// A new file renamed over it: another file, which the binds of the
	// commands running did not cover.
	next := filepath.Join(work, "AGENTS.md.new")
	if err := os.WriteFile(next, []byte("v3 RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, file); err != nil {
		t.Fatal(err)
	}
	global("v3 RULES")

	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	var mu sync.Mutex
	var out strings.Builder
	if _, err := g.Exec(ctx, `echo x >> "`+file+`" && echo LEAK; cat "`+file+`"`, &env.ShellExecOptions{OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
		mu.Lock()
		out.WriteString(text)
		mu.Unlock()
	}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(out.String(), "LEAK") {
		t.Errorf("a command changed the new instructions file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "v3 RULES") {
		t.Errorf("the new instructions file is not the one a command reads:\n%s", out.String())
	}
}

// A command already running when the owner renames a new file over the
// instructions file lost its read-only bind with the old file, so the new
// one is writable to it: the sandbox's watch stops it before it can write
// there, and the next command finds the new file read-only (ADR-0044).
func TestARunningCommandStopsWhenTheInstructionsFileIsReplaced(t *testing.T) {
	l := newLayout(t)
	file := filepath.Join(l.home, ".pi", "agent", "AGENTS.md")
	s := instructionsSandbox(t, l, file, "v1 RULES")
	work := t.TempDir()
	gate := filepath.Join(work, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	var mu sync.Mutex
	var out strings.Builder
	ready := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The command waits at the gate, then writes to the file's path.
		_, _ = g.Exec(context.Background(), `echo READY; cat "`+gate+`" >/dev/null; echo EVIL >> "`+file+`" && echo WROTE`, &env.ShellExecOptions{
			OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
				mu.Lock()
				out.WriteString(text)
				started := strings.Contains(out.String(), "READY")
				mu.Unlock()
				if started {
					once.Do(func() { close(ready) })
				}
			}})
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not start")
	}
	// The owner renames a new file over it.
	next := filepath.Join(work, "AGENTS.md.new")
	if err := os.WriteFile(next, []byte("v2 RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, file); err != nil {
		t.Fatal(err)
	}
	// One tick of the watch, then another, which it takes only once the
	// first is done.
	ticks := make(chan time.Time)
	watch, stop := context.WithCancel(context.Background())
	defer stop()
	go s.watchShown(watch, ticks)
	for range 2 {
		select {
		case ticks <- time.Now():
		case <-time.After(30 * time.Second):
			t.Fatal("the watch does not take its ticks")
		}
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		// Open the gate, so the command ends, and fail.
		if f, err := os.OpenFile(gate, os.O_WRONLY, 0); err == nil {
			_ = f.Close()
		}
		<-done
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the command running as the file was replaced was not stopped:\n%s", out.String())
	}
	mu.Lock()
	output := out.String()
	mu.Unlock()
	if strings.Contains(output, "WROTE") {
		t.Fatalf("the running command wrote to the new file:\n%s", output)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "v2 RULES" {
		t.Fatalf("the new file changed: %q, %v", b, err)
	}
	// The next command finds the new file read-only.
	var after strings.Builder
	if _, err := g.Exec(context.Background(), `echo EVIL >> "`+file+`" && echo LEAK; echo RAN`, &env.ShellExecOptions{
		OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) { after.WriteString(text) }}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.String(), "RAN") || strings.Contains(after.String(), "LEAK") {
		t.Fatalf("the next command:\n%s", after.String())
	}
}
