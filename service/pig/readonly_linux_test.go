//go:build linux

package pig

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"golang.org/x/sys/unix"
)

// A named pipe the owner put in a shown folder's place holds no look at
// the folders: it is opened without waiting for a writer, found no folder,
// and not kept, so later commands and looks are not stuck behind it
// (Codex review of #63, round 3).
func TestAPipeInAShownFoldersPlaceHoldsNothing(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pocketgo-uploads")
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/sh", Shown: []string{dir}}
	saved := stopRunning
	t.Cleanup(func() { stopRunning = saved })
	stopRunning = func(string) error { return nil }
	if err := s.keepShown(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.keepShown() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no folder") {
			t.Fatalf("a pipe in the folder's place: %v", err)
		}
	case <-time.After(30 * time.Second):
		// Give the open its writer, so it ends, and fail.
		if f, err := os.OpenFile(dir, os.O_WRONLY, 0); err == nil {
			_ = f.Close()
		}
		<-done
		t.Fatal("the look waited on a pipe in the folder's place")
	}
}

// A sandbox stopSandboxes found, but cannot look at again once its pidfd
// is open, is a stop that failed, never one done: keepShown tries again
// at the next look (Codex review of #63, round 3).
func TestASandboxNotLookedAtAgainIsNoStop(t *testing.T) {
	cat, err := systemProgram("cat")
	if err != nil {
		t.Skip(err)
	}
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &exec.Cmd{Path: cat, Args: []string{bwrap}, Stdin: in}
	if err := sandbox.Start(); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	defer func() { _ = out.Close() }()
	stat := "/proc/" + strconv.Itoa(sandbox.Process.Pid) + "/stat"
	reads := 0
	saved := readProc
	t.Cleanup(func() { readProc = saved })
	readProc = func(name string) ([]byte, error) {
		if name == stat {
			if reads++; reads == 2 {
				return nil, syscall.EMFILE
			}
		}
		return saved(name)
	}
	if err := stopSandboxes(bwrap); err == nil {
		t.Fatal("a stop that could not look at the sandbox again succeeded")
	}
	if sandbox.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("the sandbox was signalled unchecked")
	}
	readProc = saved
	if err := stopSandboxes(bwrap); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the sandbox ended with %v, not killed", err)
	}
}

// pocketd stops a sandbox only through a pidfd of the child it checked: a
// child started as bwrap is killed so, a process started as anything else
// is not, and where the kernel opens no pidfd nothing is signalled and the
// stop fails, rather than signal a number another process may have taken
// meanwhile (Codex review of #63, round 2).
func TestASandboxIsStoppedOnlyThroughAPidfd(t *testing.T) {
	cat, err := systemProgram("cat")
	if err != nil {
		t.Skip(err)
	}
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	start := func(argv0 string) (*exec.Cmd, *os.File) {
		t.Helper()
		in, out, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := &exec.Cmd{Path: cat, Args: []string{argv0}, Stdin: in}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = in.Close()
		return cmd, out
	}
	sandbox, sandboxIn := start(bwrap)
	other, otherIn := start(cat)
	defer func() {
		_ = otherIn.Close()
		_ = other.Wait()
	}()
	alive := func(cmd *exec.Cmd) bool { return cmd.Process.Signal(syscall.Signal(0)) == nil }

	saved := pidfdOpen
	t.Cleanup(func() { pidfdOpen = saved })
	pidfdOpen = func(int, int) (int, error) { return -1, unix.ENOSYS }
	if err := stopSandboxes(bwrap); err == nil {
		t.Fatal("a stop with no pidfd succeeded")
	}
	if !alive(sandbox) {
		t.Fatal("a sandbox was signalled with no pidfd")
	}
	pidfdOpen = saved
	if err := stopSandboxes(bwrap); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the sandbox ended with %v, not killed", err)
	}
	_ = sandboxIn.Close()
	if !alive(other) {
		t.Fatal("a process that is no sandbox was stopped")
	}
}

// A command already running when the owner removes and makes again a
// shown folder lost its read-only bind with the old folder, so the new
// one is writable to it: the sandbox's watch stops it before it can write
// there, and the next command finds the new folder read-only (Codex review
// of #63, round 1).
func TestARunningCommandStopsWhenAShownFolderIsReplaced(t *testing.T) {
	l, _, up := newShownLayout(t)
	work := t.TempDir()
	s := usableSandbox(t, l)
	gate := filepath.Join(work, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	planted := filepath.Join(up, "planted")
	var mu sync.Mutex
	var out strings.Builder
	ready := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The command waits at the gate, then writes where the uploads
		// folder is.
		_, _ = g.Exec(context.Background(), `echo READY; cat "`+gate+`" >/dev/null; touch "`+planted+`" && echo WROTE`, &env.ShellExecOptions{
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
	// The owner removes the uploads folder by hand, and it is made again.
	if err := os.RemoveAll(up); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(up, 0o700); err != nil {
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
		t.Fatalf("the command running as the folder was replaced was not stopped:\n%s", out.String())
	}
	mu.Lock()
	output := out.String()
	mu.Unlock()
	if strings.Contains(output, "WROTE") {
		t.Fatalf("the running command wrote in the new folder:\n%s", output)
	}
	if _, err := os.Lstat(planted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was planted in the new folder: %v", err)
	}
	// The next command finds the new folder read-only.
	var next strings.Builder
	if _, err := g.Exec(context.Background(), `touch "`+planted+`" && echo LEAK; echo RAN`, &env.ShellExecOptions{
		OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) { next.WriteString(text) }}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.String(), "RAN") || strings.Contains(next.String(), "LEAK") {
		t.Fatalf("the next command:\n%s", next.String())
	}
}

// An engine with a sandbox that shows folders watches them as it runs:
// a folder removed while it runs is made again at the watch's next tick,
// with no command started, and the watch ends with the engine (Codex
// review of #63, round 1).
func TestAnEngineWatchesItsShownFolders(t *testing.T) {
	l, skills, _ := newShownLayout(t)
	s, err := newSandbox(l.paths("/usr/bin/bwrap")) // never run: no command starts
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	saved := shownTicker
	shownTicker = func() (<-chan time.Time, func()) { return ticks, func() { close(stopped) } }
	t.Cleanup(func() { shownTicker = saved })
	f := newFixture(t)
	e, err := open(context.Background(), Options{DataDir: f.dataDir, Approver: f.approve, NeedsApproval: approvals.NeedsApproval, Sandbox: s}, fauxModels())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(skills); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case ticks <- time.Now():
		case <-time.After(30 * time.Second):
			t.Fatal("the engine does not watch its shown folders")
		}
	}
	if info, err := os.Stat(skills); err != nil || !info.IsDir() {
		t.Fatalf("the removed skills folder was not made again: %v", err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the watch outlived the engine")
	}
}
