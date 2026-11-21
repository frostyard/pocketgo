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
	stopRunning = func(string, string) error { return nil }
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

// fakeSandbox starts a child of the test, as pocketd starts a sandbox: sh
// named argv0, with the sandbox's mark in its arguments when mark is not
// "", running a cat on a pipe the test holds, its own child, as a
// sandbox's namespace has a first process. It gives back the child and
// the pipe's end, which the caller closes.
func fakeSandbox(t *testing.T, argv0, mark string) (*exec.Cmd, *os.File) {
	t.Helper()
	sh, err := systemProgram("sh")
	if err != nil {
		t.Skip(err)
	}
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	said, says, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// The cat reads the test's pipe (a background command of sh reads
	// /dev/null unless told otherwise); sh says when it started it.
	args := []string{argv0, "-c", "exec 3<&0; cat <&3 & echo; wait"}
	if mark != "" {
		args = append(args, "--setenv", sandboxMark, mark)
	}
	cmd := &exec.Cmd{Path: sh, Args: args, Stdin: in, Stdout: says}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	_ = says.Close()
	if _, err := said.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	_ = said.Close()
	return cmd, out
}

// alive says whether a child of the test has not ended: a child that
// ended and is not waited for yet is a zombie, which a signal 0 still
// reaches.
func alive(cmd *exec.Cmd) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(stat), ')')
	state := strings.Fields(string(stat[i+1:]))[0]
	return state != "Z" && state != "X"
}

// A sandbox stopSandboxes found, but cannot look at again once its pidfd
// is open, is a stop that failed, never one done: keepShown tries again
// at the next look (Codex review of #63, round 3).
func TestASandboxNotLookedAtAgainIsNoStop(t *testing.T) {
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	sandbox, sandboxIn := fakeSandbox(t, bwrap, "m1")
	defer func() { _ = sandboxIn.Close() }()
	stat := "/proc/" + strconv.Itoa(sandbox.Process.Pid) + "/stat"
	reads := 0
	saved := readProc
	t.Cleanup(func() { readProc = saved })
	readProc = func(name string) ([]byte, error) {
		if name == stat {
			// The scan reads it twice, then the check through the pidfd.
			if reads++; reads == 3 {
				return nil, syscall.EMFILE
			}
		}
		return saved(name)
	}
	if err := stopSandboxes(bwrap, "m1"); err == nil {
		t.Fatal("a stop that could not look at the sandbox again succeeded")
	}
	if !alive(sandbox) {
		t.Fatal("the sandbox was signalled unchecked")
	}
	readProc = saved
	if err := stopSandboxes(bwrap, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the sandbox ended with %v, not killed", err)
	}
}

// A stop is done only once the sandboxes it killed have ended, not when
// its signals went: a wait that ran out is a stop that failed (Codex
// review of #63, round 4), and the next stop waits again for what had not
// ended, though it can no longer find it as a child of a bwrap (round 5).
func TestAStopWaitsForTheSandboxesToEnd(t *testing.T) {
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	sandbox, sandboxIn := fakeSandbox(t, bwrap, "m2")
	defer func() { _ = sandboxIn.Close() }()
	saved := pidfdWait
	t.Cleanup(func() { pidfdWait = saved })
	var waited []int
	pidfdWait = func(fds []int, d time.Duration) ([]int, error) {
		waited = append(waited, len(fds))
		return fds, errors.New("did not end in time")
	}
	if err := stopSandboxes(bwrap, "m2"); err == nil || len(waited) != 1 || waited[0] != 2 {
		t.Fatalf("a stop whose wait ran out: %v, after waiting on %v", err, waited)
	}
	_ = sandbox.Wait() // the bwrap has ended, and is no child to find again
	pidfdWait = func(fds []int, d time.Duration) ([]int, error) {
		waited = append(waited, len(fds))
		return saved(fds, d)
	}
	if err := stopSandboxes(bwrap, "m2"); err != nil || len(waited) != 2 || waited[1] != 2 {
		t.Fatalf("the next stop: %v, after waiting on %v", err, waited)
	}
	if err := stopSandboxes(bwrap, "m2"); err != nil || len(waited) != 2 {
		t.Fatalf("a stop with nothing left: %v, after waiting on %v", err, waited)
	}
}

// The first process of a sandbox's namespace, the bwrap child's own
// child, is held and killed with it, and the stop waits for both: none
// outlives it, though nothing ties it to its parent's end (Codex review of
// #63, round 4).
func TestASandboxsFirstProcessIsStoppedToo(t *testing.T) {
	sh, err := systemProgram("sh")
	if err != nil {
		t.Skip(err)
	}
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	printed, printer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// sh, started as bwrap with the mark, starts a cat that reads the
	// test's pipe, says its number, and waits.
	sandbox := &exec.Cmd{Path: sh, Args: []string{bwrap, "-c", "exec 3<&0; cat <&3 & echo $!; wait", "--setenv", sandboxMark, "m3"}, Stdin: in, Stdout: printer}
	if err := sandbox.Start(); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	_ = printer.Close()
	defer func() { _ = out.Close() }()
	line := make([]byte, 32)
	n, err := printed.Read(line)
	if err != nil {
		t.Fatal(err)
	}
	said.Write(line[:n])
	first, err := strconv.Atoi(strings.TrimSpace(said.String()))
	if err != nil {
		t.Fatalf("the sandbox said %q", said.String())
	}
	if err := stopSandboxes(bwrap, "m3"); err != nil {
		t.Fatal(err)
	}
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(first) + "/stat"); err == nil {
		i := strings.LastIndexByte(string(stat), ')')
		if state := strings.Fields(string(stat[i+1:]))[0]; state != "Z" && state != "X" {
			t.Fatalf("the sandbox's first process is still %s after the stop", state)
		}
	}
	_ = sandbox.Wait()
	_ = printed.Close()
}

// pocketd stops a sandbox only through a pidfd of the child it checked: a
// child started as bwrap with the sandbox's mark is killed so; one started
// as anything else, or as that bwrap without the mark, as the owner could
// from a terminal, is not (round 5); and where the kernel opens no pidfd
// nothing is signalled and the stop fails, rather than signal a number
// another process may have taken meanwhile (Codex review of #63, round
// 2).
func TestASandboxIsStoppedOnlyThroughAPidfd(t *testing.T) {
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	sandbox, sandboxIn := fakeSandbox(t, bwrap, "m4")
	defer func() { _ = sandboxIn.Close() }()
	others := map[string]*exec.Cmd{}
	for name, argv0 := range map[string]string{"another program": "cat", "the owner's bwrap": bwrap} {
		cmd, in := fakeSandbox(t, argv0, "")
		others[name] = cmd
		defer func() {
			_ = in.Close()
			_ = cmd.Wait()
		}()
	}
	saved := pidfdOpen
	t.Cleanup(func() { pidfdOpen = saved })
	pidfdOpen = func(int, int) (int, error) { return -1, unix.ENOSYS }
	if err := stopSandboxes(bwrap, "m4"); err == nil {
		t.Fatal("a stop with no pidfd succeeded")
	}
	if !alive(sandbox) {
		t.Fatal("a sandbox was signalled with no pidfd")
	}
	pidfdOpen = saved
	if err := stopSandboxes(bwrap, "m4"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the sandbox ended with %v, not killed", err)
	}
	for name, cmd := range others {
		if !alive(cmd) {
			t.Errorf("%s was stopped", name)
		}
	}
	// A sandbox with no mark stops nothing at all.
	if err := stopSandboxes(bwrap, ""); err != nil {
		t.Fatal(err)
	}
	for name, cmd := range others {
		if !alive(cmd) {
			t.Errorf("%s was stopped by a sandbox with no mark", name)
		}
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
